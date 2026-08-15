import { afterEach, describe, expect, it } from "vitest";
import { serve, readBody, pathOf, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("environments.computer", () => {
  it("posts the action to the computer sub-path and returns the result", async () => {
    let seenPath = "";
    let seenMethod = "";
    let seenBody: unknown;
    current = await serve(async (req, res) => {
      seenPath = pathOf(req);
      seenMethod = req.method ?? "";
      seenBody = JSON.parse(await readBody(req));
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ screenshot: "cGl4ZWxz" }));
    });

    const res = await current.client.environments.computer("vm-1", {
      action: "left_click",
      coordinate: [100, 200],
    });

    expect(seenMethod).toBe("POST");
    expect(seenPath).toBe("/v1/environments/vm-1/computer");
    expect(seenBody).toEqual({ action: "left_click", coordinate: [100, 200] });
    expect(res.screenshot).toBe("cGl4ZWxz");
  });

  it("reads the display report from the same sub-path", async () => {
    current = await serve(async (req, res) => {
      expect(req.method).toBe("GET");
      expect(pathOf(req)).toBe("/v1/environments/vm-1/computer");
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ display: ":1", up: true, width: 1280, height: 800 }));
    });

    const res = await current.client.environments.computerDisplay("vm-1");
    expect(res.up).toBe(true);
    expect(res.width).toBe(1280);
  });

  it("rejects an empty vm id or action before any request is made", async () => {
    current = await serve(async () => {
      throw new Error("no request should be made");
    });
    await expect(
      current.client.environments.computer("", { action: "screenshot" }),
    ).rejects.toThrow("vm id is required");
    await expect(
      current.client.environments.computer("vm-1", { action: "" }),
    ).rejects.toThrow("action is required");
  });
});
