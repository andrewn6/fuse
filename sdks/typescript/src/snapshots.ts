import type { CallOptions, Transport } from "./transport.js";
import type { ResolveSnapshotResponse, Snapshot, SnapshotRequest } from "./types.js";
import { requireArg } from "./validate.js";

/** The server's max page size; list() requests this internally so it walks
 * every page in as few round trips as possible. */
const MAX_PAGE_LIMIT = 200;

/** Filters (and pagination) for snapshots.list / snapshots.listPage. */
export interface ListSnapshotsOptions {
  vmId?: string;
  taskId?: string;
  tenantId?: string;
  state?: string;
  /** Narrows to the build layers taken after one setup step. Unset means "do
   * not filter", never "match artifacts with no layer key". */
  layerKey?: string;
  /** Narrows to artifacts built on one architecture, in GOARCH vocabulary
   * ("amd64", "arm64"). It is a separate filter from layerKey rather than
   * part of it because a rootfs is not portable across architectures, so a
   * layer lookup that does not constrain arch can be served bytes it cannot
   * boot. */
  arch?: string;
  /** Page size (server default 50, max 200). Only consulted by listPage —
   * list() always walks every page, so it ignores this and requests the
   * server's max page size. */
  limit?: number;
  /** Opaque cursor from a previous listPage() call's nextCursor. */
  cursor?: string;
}

interface SnapshotList {
  snapshots: Snapshot[];
  next_cursor?: string;
}

/** One page of snapshots.listPage, plus the cursor to fetch the next one. */
export interface SnapshotPage {
  snapshots: Snapshot[];
  /** Empty/undefined once there are no more results. */
  nextCursor?: string;
}

/** SnapshotsService manages microVM snapshots. */
export class SnapshotsService {
  constructor(private readonly t: Transport) {}

  /** Create a snapshot of a running environment.
   *
   * An empty body takes a disk snapshot, the rootfs and nothing else; pass
   * `{ live: true }` to capture the guest's memory and vCPU state as well.
   *
   * The returned `snapshot.kind` reports what was actually written, which can
   * be `"disk"` even for a `live: true` request that reached a host too old to
   * honour it. A caller that depends on the memory being there should check
   * it. `live: true` against a backend with no live-snapshot support throws a
   * 501 `unimplemented`, not a conflict. */
  async create(
    vmId: string,
    body: SnapshotRequest = {},
    opts: CallOptions = {},
  ): Promise<Snapshot> {
    requireArg(vmId, "vm id");
    return this.t.json<Snapshot>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}/snapshots`,
      { body, signal: opts.signal },
    );
  }

  /** List snapshots, optionally filtered. Transparently walks every result
   * page. For explicit single-page control (e.g. a cursor from a previous
   * call), use listPage. */
  async list(
    options: ListSnapshotsOptions = {},
    opts: CallOptions = {},
  ): Promise<Snapshot[]> {
    const out: Snapshot[] = [];
    let cursor = options.cursor;
    for (;;) {
      const page = await this.listPage(
        { ...options, limit: MAX_PAGE_LIMIT, cursor },
        opts,
      );
      out.push(...page.snapshots);
      if (!page.nextCursor) break;
      cursor = page.nextCursor;
    }
    return out;
  }

  /** List one page of snapshots, optionally filtered. */
  async listPage(
    options: ListSnapshotsOptions = {},
    opts: CallOptions = {},
  ): Promise<SnapshotPage> {
    const out = await this.t.json<SnapshotList>("GET", "/v1/snapshots", {
      query: {
        vm_id: options.vmId,
        task_id: options.taskId,
        tenant_id: options.tenantId,
        state: options.state,
        layer_key: options.layerKey,
        arch: options.arch,
        limit: options.limit ? String(options.limit) : undefined,
        cursor: options.cursor,
      },
      signal: opts.signal,
    });
    return { snapshots: out.snapshots ?? [], nextCursor: out.next_cursor };
  }

  /**
   * Resolve one layer cache key and architecture to the newest ready build
   * artifact, or null when there is none.
   *
   * A miss is not an error and never rejects: a cold cache is the normal state
   * of a first build, and modelling it as a failure would make every caller
   * wrap this in a try/catch to discover nothing was wrong.
   *
   * arch is required rather than defaulted. An ext4 rootfs is not portable
   * across architectures, so resolving without one could hand back an artifact
   * the caller cannot boot, at the exact moment it believes it got a hit.
   *
   * The scope searched comes from how the client authenticated; there is
   * deliberately no tenant parameter.
   */
  async resolve(
    layerKey: string,
    arch: string,
    opts: CallOptions = {},
  ): Promise<Snapshot | null> {
    requireArg(layerKey, "layer key");
    requireArg(arch, "arch");
    const out = await this.t.json<ResolveSnapshotResponse>(
      "GET",
      "/v1/snapshots/resolve",
      { query: { layer_key: layerKey, arch }, signal: opts.signal },
    );
    return out.found && out.snapshot ? out.snapshot : null;
  }

  /** Fetch a single snapshot by id. */
  async get(snapshotId: string, opts: CallOptions = {}): Promise<Snapshot> {
    requireArg(snapshotId, "snapshot id");
    return this.t.json<Snapshot>(
      "GET",
      `/v1/snapshots/${encodeURIComponent(snapshotId)}`,
      { signal: opts.signal },
    );
  }

  /** Delete a snapshot. Idempotent; the snapshot must be a leaf. */
  async delete(snapshotId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(snapshotId, "snapshot id");
    await this.t.noContent("DELETE", `/v1/snapshots/${encodeURIComponent(snapshotId)}`, {
      signal: opts.signal,
    });
  }

  /** Restore an environment from a snapshot. */
  async restore(snapshotId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(snapshotId, "snapshot id");
    await this.t.noContent("POST", `/v1/snapshots/${encodeURIComponent(snapshotId)}`, {
      query: { action: "restore" },
      signal: opts.signal,
    });
  }
}
