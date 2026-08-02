import { afterEach, describe, expect, it } from "vitest";
import { pathOf, queryOf, serve, type TestServer } from "./server.js";

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
