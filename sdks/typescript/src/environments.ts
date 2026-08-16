import type { Duplex } from "node:stream";

import type { CallOptions, Transport } from "./transport.js";
import type {
  ComputerAction,
  ComputerDisplay,
  ComputerResult,
  ComputerToolResult,
  CreateRequest,
  EnvironmentInfo,
  Event,
  ExecRequest,
  ExecResult,
  ForkOptions,
  ToolResultBlock,
} from "./types.js";
import { FuseError, isFuseApiError } from "./errors.js";
import { requireArg } from "./validate.js";
import { streamEvents } from "./events.js";

/** The server's max page size; list() requests this internally so it walks
 * every page in as few round trips as possible. */
const MAX_PAGE_LIMIT = 200;

/** The Upgrade token that opens a live desktop stream. */
export const VNC_PROTO = "fuse-vnc/1";

/** Filters (and pagination) for environments.list / environments.listPage. */
export interface ListEnvironmentsOptions {
  taskId?: string;
  state?: string;
  hostId?: string;
  /** Page size (server default 50, max 200). Only consulted by listPage —
   * list() always walks every page, so it ignores this and requests the
   * server's max page size. */
  limit?: number;
  /** Opaque cursor from a previous listPage() call's nextCursor. */
  cursor?: string;
}

interface EnvironmentList {
  environments: EnvironmentInfo[];
  next_cursor?: string;
}

/** One page of environments.listPage, plus the cursor to fetch the next one. */
export interface EnvironmentPage {
  environments: EnvironmentInfo[];
  /** Empty/undefined once there are no more results. */
  nextCursor?: string;
}

/** EnvironmentsService manages microVM lifecycle. */
export class EnvironmentsService {
  constructor(private readonly t: Transport) {}

  /** List tracked environments, optionally filtered. Transparently walks
   * every result page. For explicit single-page control (e.g. a cursor from
   * a previous call), use listPage. */
  async list(
    options: ListEnvironmentsOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentInfo[]> {
    const out: EnvironmentInfo[] = [];
    let cursor = options.cursor;
    for (;;) {
      const page = await this.listPage(
        { ...options, limit: MAX_PAGE_LIMIT, cursor },
        opts,
      );
      out.push(...page.environments);
      if (!page.nextCursor) break;
      cursor = page.nextCursor;
    }
    return out;
  }

  /** List one page of tracked environments, optionally filtered. */
  async listPage(
    options: ListEnvironmentsOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentPage> {
    const out = await this.t.json<EnvironmentList>("GET", "/v1/environments", {
      query: {
        task_id: options.taskId,
        state: options.state,
        host_id: options.hostId,
        limit: options.limit ? String(options.limit) : undefined,
        cursor: options.cursor,
      },
      signal: opts.signal,
    });
    return { environments: out.environments ?? [], nextCursor: out.next_cursor };
  }

  /** Fetch a single environment by VM id. */
  async get(vmId: string, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    return this.t.json<EnvironmentInfo>(
      "GET",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { signal: opts.signal },
    );
  }

  /** Provision a new environment. Blocks until running or creation fails. */
  async create(body: CreateRequest, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    return this.t.json<EnvironmentInfo>("POST", "/v1/environments", {
      body,
      signal: opts.signal,
    });
  }

  /** Fork an environment into a new one; returns the new environment. */
  async fork(
    vmId: string,
    body: ForkOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    return this.t.json<EnvironmentInfo>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "fork" }, body, signal: opts.signal },
    );
  }

  /**
   * Run a command inside a running environment's guest and return its exit code
   * with stdout and stderr kept separate.
   *
   * A non-zero exit_code is resolved, not thrown: the command ran and failed.
   * A thrown error means the command could not be run at all.
   *
   * Exec requires the master token.
   *
   * @example
   * const out = await client.environments.exec(id, { cmd: ["ls", "-l"] });
   * if (out.exit_code !== 0) console.error(out.stderr);
   */
  async exec(
    vmId: string,
    body: ExecRequest,
    opts: CallOptions = {},
  ): Promise<ExecResult> {
    requireArg(vmId, "vm id");
    const hasCmd = (body.cmd?.length ?? 0) > 0;
    const hasShell = (body.shell ?? "") !== "";
    if (!hasCmd && !hasShell) {
      throw new FuseError("one of cmd or shell is required");
    }
    if (hasCmd && hasShell) {
      throw new FuseError("cmd and shell are mutually exclusive");
    }
    return this.t.json<ExecResult>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "exec" }, body, signal: opts.signal },
    );
  }

  /**
   * Relay one computer-use action to the environment's desktop and return the
   * result, usually carrying a base64 PNG screenshot. Requires an environment
   * booted from a desktop image; on any other image the server answers 503
   * with a reason.
   *
   * @example
   * const res = await client.environments.computer(id, {
   *   action: "left_click",
   *   coordinate: [512, 384],
   * });
   */
  async computer(
    vmId: string,
    action: ComputerAction,
    opts: CallOptions = {},
  ): Promise<ComputerResult> {
    requireArg(vmId, "vm id");
    requireArg(action?.action, "action");
    return this.t.json<ComputerResult>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}/computer`,
      { body: action, signal: opts.signal },
    );
  }

  /**
   * Execute one computer tool_use against the environment and shape the
   * answer for the Messages API: the tool_use block's `input` goes in
   * verbatim (it is already the wire shape the computer endpoint takes), and
   * content blocks for the tool_result come out.
   *
   * An action the environment refuses (a malformed coordinate, a display
   * that is not up) resolves to `is_error` content rather than throwing: the
   * agent loop should feed it to the model, which is the party that can
   * correct it. A throw is reserved for failures the model cannot fix —
   * transport, auth, an unknown environment.
   *
   * @example
   * for (const block of msg.content) {
   *   if (block.type === "tool_use" && block.name === "computer") {
   *     const result = await client.environments.computerToolResult(id, block.input);
   *     toolResults.push({ type: "tool_result", tool_use_id: block.id, ...result });
   *   }
   * }
   */
  async computerToolResult(
    vmId: string,
    input: unknown,
    opts: CallOptions = {},
  ): Promise<ComputerToolResult> {
    requireArg(vmId, "vm id");
    const action = (input as { action?: string } | null)?.action ?? "";
    if (action === "") {
      return {
        is_error: true,
        content: [{ type: "text", text: "tool input names no action" }],
      };
    }
    let res: ComputerResult;
    try {
      res = await this.t.json<ComputerResult>(
        "POST",
        `/v1/environments/${encodeURIComponent(vmId)}/computer`,
        { body: input, signal: opts.signal },
      );
    } catch (err) {
      if (
        isFuseApiError(err) &&
        (err.status === 400 || err.status === 409 || err.status === 503)
      ) {
        return {
          is_error: true,
          content: [{ type: "text", text: err.message || "the computer action failed" }],
        };
      }
      throw err;
    }
    const content: ToolResultBlock[] = [];
    if (res.output) content.push({ type: "text", text: res.output });
    if (res.screenshot) {
      content.push({
        type: "image",
        source: {
          type: "base64",
          media_type: "image/png",
          data: res.screenshot,
        },
      });
    }
    if (content.length === 0) content.push({ type: "text", text: "done" });
    return { content };
  }

  /**
   * Open the live view of a running environment's desktop: a raw RFB (VNC)
   * byte stream carrying both the display and input, so it is also how a
   * human takes over a session an agent is driving. The bytes are verbatim
   * from the vnc server inside the guest; hand the stream to any RFB client.
   *
   * Node only — browsers cannot open raw sockets. For a browser, use the
   * `fuse desktop` CLI command, which bridges this stream to a local viewer.
   *
   * Requires an environment booted from a desktop image; on any other image
   * the server answers 503 with a reason. Unlike exec this accepts API keys.
   * The caller owns the stream and must destroy() it when done.
   */
  async computerStream(vmId: string, opts: CallOptions = {}): Promise<Duplex> {
    requireArg(vmId, "vm id");
    return this.t.upgrade(
      `/v1/environments/${encodeURIComponent(vmId)}/computer/stream`,
      VNC_PROTO,
      opts.signal,
    );
  }

  /** Report whether the environment has a live display and at what geometry.
   * up is false with a reason on an image with no desktop. */
  async computerDisplay(vmId: string, opts: CallOptions = {}): Promise<ComputerDisplay> {
    requireArg(vmId, "vm id");
    return this.t.json<ComputerDisplay>(
      "GET",
      `/v1/environments/${encodeURIComponent(vmId)}/computer`,
      { signal: opts.signal },
    );
  }

  /**
   * Phase-1 only: quiesces the guest workload and moves the environment to
   * "draining" so you can inspect its output, but does not destroy it. Call
   * destroy() afterward to remove it - a drained environment left
   * undestroyed keeps running (and billing) forever.
   */
  async drain(vmId: string, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    return this.t.json<EnvironmentInfo>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "drain" }, signal: opts.signal },
    );
  }

  /** Rotate the per-VM credentials and push them to the guest. */
  async rotateToken(vmId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(vmId, "vm id");
    await this.t.noContent("POST", `/v1/environments/${encodeURIComponent(vmId)}`, {
      query: { action: "rotate-token" },
      signal: opts.signal,
    });
  }

  /** Forcefully destroy an environment. Idempotent. */
  async destroy(vmId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(vmId, "vm id");
    await this.t.noContent("DELETE", `/v1/environments/${encodeURIComponent(vmId)}`, {
      signal: opts.signal,
    });
  }

  /**
   * Open the SSE event stream for an environment. The returned promise rejects
   * with a FuseApiError on a connect-time failure (e.g. not_found); on success
   * it resolves to an AsyncIterable that yields each Event and ends after a
   * terminal-state event. Pass `opts.signal` to cancel; there is no built-in
   * timeout on the stream.
   *
   * @example
   * for await (const ev of await client.environments.events(id)) {
   *   console.log(ev.state);
   * }
   */
  async events(vmId: string, opts: CallOptions = {}): Promise<AsyncIterable<Event>> {
    requireArg(vmId, "vm id");
    return streamEvents(this.t, vmId, opts.signal);
  }
}
