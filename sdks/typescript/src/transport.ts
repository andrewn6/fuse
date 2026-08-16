// The transport layer: builds requests (URL, headers, auth, X-Request-ID,
// JSON body), applies an optional client-default timeout, executes fetch, and
// turns non-2xx responses into FuseApiError. Mirrors the transport in
// sdks/go/environments.go. SSE streams use stream() which deliberately applies
// no timeout (the Go SDK uses a separate Timeout:0 client for events).

import type { Duplex } from "node:stream";

import { FuseError, errorFromResponse, parseApiError } from "./errors.js";

const REQUEST_ID_HEADER = "X-Request-ID";

/** A minimal fetch signature, satisfied by the global fetch and most polyfills. */
export type FetchLike = (input: string | URL, init?: RequestInit) => Promise<Response>;

/** Per-call options accepted by every service method. */
export interface CallOptions {
  /** Abort the request (and, for events, the stream). */
  signal?: AbortSignal;
}

export interface TransportConfig {
  baseUrl: string;
  token?: string;
  fetch?: FetchLike;
  userAgent?: string;
  requestId?: () => string;
  timeoutMs?: number;
  headers?: Record<string, string>;
}

type Query = Record<string, string | undefined>;

interface RequestSpec {
  query?: Query;
  body?: unknown;
  signal?: AbortSignal;
}

/** Combine zero or more AbortSignals into one, without relying on AbortSignal.any (Node 20+). */
function combineSignals(signals: Array<AbortSignal | undefined>): {
  signal?: AbortSignal;
  cleanup: () => void;
} {
  const active = signals.filter((s): s is AbortSignal => s != null);
  if (active.length === 0) return { cleanup: () => {} };
  if (active.length === 1) return { signal: active[0], cleanup: () => {} };

  const controller = new AbortController();
  const onAbort = (event: Event) => {
    const target = event.target as AbortSignal;
    controller.abort(target.reason);
    cleanup();
  };
  const cleanup = () => {
    for (const s of active) s.removeEventListener("abort", onAbort);
  };
  for (const s of active) {
    if (s.aborted) {
      controller.abort(s.reason);
      cleanup();
      return { signal: controller.signal, cleanup: () => {} };
    }
    s.addEventListener("abort", onAbort);
  }
  return { signal: controller.signal, cleanup };
}

export class Transport {
  private readonly baseUrl: URL;
  private readonly token?: string;
  private readonly fetchImpl: FetchLike;
  private readonly userAgent?: string;
  private readonly requestId?: () => string;
  private readonly timeoutMs?: number;
  private readonly defaultHeaders?: Record<string, string>;

  constructor(cfg: TransportConfig) {
    if (!cfg.baseUrl) {
      throw new FuseError("base url is required");
    }
    let parsed: URL;
    try {
      parsed = new URL(cfg.baseUrl);
    } catch {
      throw new FuseError("base url is invalid");
    }
    this.baseUrl = parsed;
    this.token = cfg.token;
    this.userAgent = cfg.userAgent;
    this.requestId = cfg.requestId;
    this.timeoutMs = cfg.timeoutMs;
    this.defaultHeaders = cfg.headers;

    const provided = cfg.fetch;
    if (provided) {
      this.fetchImpl = provided;
    } else if (typeof globalThis.fetch === "function") {
      this.fetchImpl = globalThis.fetch.bind(globalThis) as FetchLike;
    } else {
      throw new FuseError(
        "global fetch is not available; pass a fetch implementation via the `fetch` option (Node 18+ required)",
      );
    }
  }

  /** Perform a request and decode a JSON response body. */
  async json<T>(method: string, path: string, spec: RequestSpec = {}): Promise<T> {
    const { signal: combined, cleanup } = this.withTimeout(spec.signal);
    try {
      const res = await this.execute(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      return await this.decodeJson<T>(res);
    } finally {
      cleanup();
    }
  }

  /** Perform a request that returns no body (e.g. 204 actions). */
  async noContent(method: string, path: string, spec: RequestSpec = {}): Promise<void> {
    const { signal: combined, cleanup } = this.withTimeout(spec.signal);
    try {
      const res = await this.execute(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      // Release the connection even if the server sent a body.
      await res.body?.cancel().catch(() => {});
    } finally {
      cleanup();
    }
  }

  /**
   * Open a long-lived stream (SSE). Applies NO timeout — only the caller's
   * signal cancels it — and checks the status eagerly so connect-time errors
   * (404/401/...) surface as a FuseApiError before any iteration.
   */
  async stream(
    path: string,
    query: Query | undefined,
    signal?: AbortSignal,
  ): Promise<Response> {
    const res = await this.execute(
      "GET",
      path,
      { query, signal },
      signal,
      "text/event-stream",
    );
    if (!res.ok) throw await errorFromResponse(res);
    return res;
  }

  /**
   * Open a raw HTTP/1.1 upgrade and hand back the socket, positioned at the
   * first byte of the stream proper. Node only: browsers get no raw sockets,
   * which is why this imports node:http lazily and fails with a clear error
   * elsewhere. Mirrors dialUpgrade in sdks/go/exec.go — fetch cannot carry an
   * upgrade, and node:https pins HTTP/1.1 (node's h2 lives in a separate
   * module), which is where an upgrade is well defined.
   */
  async upgrade(path: string, proto: string, signal?: AbortSignal): Promise<Duplex> {
    const isHttps = this.baseUrl.protocol === "https:";
    if (!isHttps && this.baseUrl.protocol !== "http:") {
      throw new FuseError(`cannot upgrade a ${this.baseUrl.protocol} url`);
    }

    let request: typeof import("node:http").request;
    try {
      ({ request } = isHttps ? await import("node:https") : await import("node:http"));
    } catch (err) {
      throw new FuseError(
        "this stream needs a Node runtime (browsers cannot open raw sockets); " +
          "use the hosted viewer or the fuse CLI instead",
        { cause: err },
      );
    }

    const url = this.buildUrl(path, undefined);
    const headers = this.buildHeaders(false);
    headers["Connection"] = "Upgrade";
    headers["Upgrade"] = proto;

    return await new Promise<Duplex>((resolve, reject) => {
      const req = request(url, { headers });

      const onAbort = () => req.destroy(signal?.reason ?? new FuseError("aborted"));
      signal?.addEventListener("abort", onAbort, { once: true });
      const cleanup = () => signal?.removeEventListener("abort", onAbort);

      req.on("upgrade", (_res, socket, head) => {
        cleanup();
        // bytes that arrived with the response head belong to the stream
        if (head.length > 0) socket.unshift(head);
        socket.setTimeout(0);
        resolve(socket);
      });
      req.on("response", (res) => {
        // the server answered plain HTTP: an error, surfaced in the same
        // shape as every other call in this SDK
        const chunks: Buffer[] = [];
        res.on("data", (c: Buffer) => chunks.push(c));
        res.on("end", () => {
          cleanup();
          const rid = res.headers[REQUEST_ID_HEADER.toLowerCase()];
          reject(
            parseApiError(
              res.statusCode ?? 0,
              typeof rid === "string" ? rid : undefined,
              Buffer.concat(chunks).toString("utf-8"),
            ),
          );
        });
      });
      req.on("error", (err) => {
        cleanup();
        if (signal?.aborted) {
          reject(err);
          return;
        }
        reject(new FuseError(`upgrade failed: GET ${url}`, { cause: err }));
      });
      req.end();
    });
  }

  private async execute(
    method: string,
    path: string,
    spec: RequestSpec,
    signal: AbortSignal | undefined,
    accept?: string,
  ): Promise<Response> {
    const url = this.buildUrl(path, spec.query);
    const hasBody = spec.body !== undefined && spec.body !== null;
    const headers = this.buildHeaders(hasBody, accept);
    const init: RequestInit = { method, headers };
    if (hasBody) init.body = JSON.stringify(spec.body);
    if (signal) init.signal = signal;

    try {
      return await this.fetchImpl(url, init);
    } catch (err) {
      // A caller- or timeout-initiated abort propagates as-is; everything else
      // is wrapped so consumers get a consistent FuseError with a cause.
      if (signal?.aborted) throw err;
      throw new FuseError(`request failed: ${method} ${url}`, { cause: err });
    }
  }

  private buildUrl(path: string, query?: Query): string {
    const p = path.startsWith("/") ? path : "/" + path;
    const u = new URL(p, this.baseUrl);
    if (query) {
      for (const [key, value] of Object.entries(query)) {
        if (value !== undefined && value !== "") u.searchParams.set(key, value);
      }
    }
    return u.toString();
  }

  private buildHeaders(hasBody: boolean, accept?: string): Record<string, string> {
    const headers: Record<string, string> = { ...(this.defaultHeaders ?? {}) };
    if (hasBody) headers["Content-Type"] = "application/json";
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    if (this.userAgent) headers["User-Agent"] = this.userAgent;
    if (this.requestId) {
      const id = this.requestId();
      if (id) headers[REQUEST_ID_HEADER] = id;
    }
    if (accept) headers["Accept"] = accept;
    return headers;
  }

  private async decodeJson<T>(res: Response): Promise<T> {
    const text = await res.text();
    if (!text) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch (err) {
      throw new FuseError("decode response body", { cause: err });
    }
  }

  private withTimeout(callerSignal?: AbortSignal): {
    signal?: AbortSignal;
    cleanup: () => void;
  } {
    const signals: Array<AbortSignal | undefined> = [callerSignal];
    if (this.timeoutMs && this.timeoutMs > 0) {
      signals.push(AbortSignal.timeout(this.timeoutMs));
    }
    return combineSignals(signals);
  }
}
