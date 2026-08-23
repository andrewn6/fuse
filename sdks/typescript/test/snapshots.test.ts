import { afterEach, describe, expect, it } from "vitest";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("snapshots", () => {
  it("create posts to the environment's snapshots path", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"snap-1","vm_id":"vm-1","created_at":"2024-01-01T00:00:00Z"}`);
    });

    const snap = await current.client.snapshots.create("vm-1", { comment: "c" });

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1/snapshots");
    expect(snap.id).toBe("snap-1");
    expect(snap.vm_id).toBe("vm-1");
  });

  it("create sends live and decodes the returned kind", async () => {
    let body: string | undefined;
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"snap-1","vm_id":"vm-1","kind":"live","created_at":"2024-01-01T00:00:00Z"}`,
      );
    });

    const snap = await current.client.snapshots.create("vm-1", { live: true });

    expect(JSON.parse(body!).live).toBe(true);
    expect(snap.kind).toBe("live");
  });

  it("create reports the kind the server wrote, not the one requested", async () => {
    // an agent too old to honour live answers with a disk snapshot; the sdk
    // must surface that rather than let a caller read its own request back.
    current = await serve((_req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"snap-1","vm_id":"vm-1","kind":"disk","created_at":"2024-01-01T00:00:00Z"}`,
      );
    });

    const snap = await current.client.snapshots.create("vm-1", { live: true });

    expect(snap.kind).toBe("disk");
  });

  it("create omits live when unset", async () => {
    let body: string | undefined;
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"snap-1","vm_id":"vm-1","created_at":"2024-01-01T00:00:00Z"}`);
    });

    await current.client.snapshots.create("vm-1", { comment: "c" });

    expect(Object.keys(JSON.parse(body!))).not.toContain("live");
  });

  it("list sends vm_id and unwraps the envelope", async () => {
    let method: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"snapshots":[{"id":"snap-1","vm_id":"vm-1","created_at":"2024-01-01T00:00:00Z"}]}`,
      );
    });

    const snaps = await current.client.snapshots.list({ vmId: "vm-1" });

    expect(method).toBe("GET");
    // list() auto-paginates (walks every page), so it always sends the max
    // page size alongside the caller's filters.
    expect(query).toBe("vm_id=vm-1&limit=200");
    expect(snaps.map((s) => s.id)).toEqual(["snap-1"]);
  });

  it("listPage sends layer_key and arch as separate filters", async () => {
    let query: string | undefined;
    current = await serve((req, res) => {
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"snapshots":[]}`);
    });

    // arch is its own filter rather than being folded into the layer key, so
    // a lookup that omits it can match an artifact it cannot boot.
    await current.client.snapshots.listPage({ layerKey: "abc123", arch: "arm64" });

    const params = new URLSearchParams(query);
    expect(params.get("layer_key")).toBe("abc123");
    expect(params.get("arch")).toBe("arm64");
  });

  it("listPage omits layer_key and arch when unset", async () => {
    let query: string | undefined;
    current = await serve((req, res) => {
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"snapshots":[]}`);
    });

    // an empty layer_key would ask the server to match every snapshot that is
    // not a layer, so an unset filter must not be sent at all.
    await current.client.snapshots.listPage({ vmId: "vm-1" });

    const params = new URLSearchParams(query);
    expect(params.has("layer_key")).toBe(false);
    expect(params.has("arch")).toBe(false);
  });

  it("get decodes the build artifact fields", async () => {
    current = await serve((_req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"snap-1","vm_id":"vm-1","mode":"build","layer_key":"abc123","arch":"amd64","digest":"deadbeef","kind":"live","created_at":"2024-01-01T00:00:00Z"}`,
      );
    });

    const snap = await current.client.snapshots.get("snap-1");

    expect(snap.layer_key).toBe("abc123");
    expect(snap.arch).toBe("amd64");
    expect(snap.digest).toBe("deadbeef");
    // kind has to decode on get too, not only on create: the docs tell callers
    // to check it, which they can only do wherever they read the snapshot.
    expect(snap.kind).toBe("live");
  });

  it("restore posts action=restore and resolves on 204", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      res.statusCode = 204;
      res.end();
    });

    await current.client.snapshots.restore("snap-1");

    expect(method).toBe("POST");
    expect(path).toBe("/v1/snapshots/snap-1");
    expect(query).toBe("action=restore");
  });
});
