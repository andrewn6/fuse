import type { CallOptions, Transport } from "./transport.js";
import type { Host, RegisterHostRequest } from "./types.js";
import { requireArg } from "./validate.js";

/** The server's max page size; list() requests this internally so it walks
 * every page in as few round trips as possible. */
const MAX_PAGE_LIMIT = 200;

/** Pagination for hosts.listPage. */
export interface ListHostsOptions {
  /** Page size (server default 50, max 200). */
  limit?: number;
  /** Opaque cursor from a previous listPage() call's nextCursor. */
  cursor?: string;
}

interface HostList {
  hosts: Host[];
  next_cursor?: string;
}

/** One page of hosts.listPage, plus the cursor to fetch the next one. */
export interface HostPage {
  hosts: Host[];
  /** Empty/undefined once there are no more results. */
  nextCursor?: string;
}

/** HostsService manages registered Firecracker hosts. */
export class HostsService {
  constructor(private readonly t: Transport) {}

  /** Register (or re-register) a host. Idempotent on id. */
  async register(body: RegisterHostRequest, opts: CallOptions = {}): Promise<Host> {
    return this.t.json<Host>("POST", "/v1/hosts", { body, signal: opts.signal });
  }

  /** List registered hosts. Transparently walks every result page. For
   * explicit single-page control (e.g. a cursor from a previous call), use
   * listPage. */
  async list(opts: CallOptions = {}): Promise<Host[]> {
    const out: Host[] = [];
    let cursor: string | undefined;
    for (;;) {
      const page = await this.listPage({ limit: MAX_PAGE_LIMIT, cursor }, opts);
      out.push(...page.hosts);
      if (!page.nextCursor) break;
      cursor = page.nextCursor;
    }
    return out;
  }

  /** List one page of registered hosts. */
  async listPage(
    options: ListHostsOptions = {},
    opts: CallOptions = {},
  ): Promise<HostPage> {
    const out = await this.t.json<HostList>("GET", "/v1/hosts", {
      query: {
        limit: options.limit ? String(options.limit) : undefined,
        cursor: options.cursor,
      },
      signal: opts.signal,
    });
    return { hosts: out.hosts ?? [], nextCursor: out.next_cursor };
  }

  /** Fetch a single host by id. */
  async get(hostId: string, opts: CallOptions = {}): Promise<Host> {
    requireArg(hostId, "host id");
    return this.t.json<Host>("GET", `/v1/hosts/${encodeURIComponent(hostId)}`, {
      signal: opts.signal,
    });
  }

  /** Mark a host unschedulable; existing VMs keep running. */
  async cordon(hostId: string, opts: CallOptions = {}): Promise<void> {
    await this.action(hostId, "cordon", opts);
  }

  /** Return a cordoned host to active scheduling. */
  async uncordon(hostId: string, opts: CallOptions = {}): Promise<void> {
    await this.action(hostId, "uncordon", opts);
  }

  /** Remove a host. The host must have no running VMs. */
  async deregister(hostId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(hostId, "host id");
    await this.t.noContent("DELETE", `/v1/hosts/${encodeURIComponent(hostId)}`, {
      signal: opts.signal,
    });
  }

  private async action(hostId: string, action: string, opts: CallOptions): Promise<void> {
    requireArg(hostId, "host id");
    await this.t.noContent("POST", `/v1/hosts/${encodeURIComponent(hostId)}`, {
      query: { action },
      signal: opts.signal,
    });
  }
}
