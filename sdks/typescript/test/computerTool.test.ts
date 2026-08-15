import { afterEach, describe, expect, it } from "vitest";
import { serve, readBody, pathOf, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("environments.computerToolResult", () => {
  it("passes the tool input through verbatim and shapes the result", async () => {
    let seenBody = "";
    current = await serve(async (req, res) => {
      expect(pathOf(req)).toBe("/v1/environments/vm-1/computer");
      seenBody = await readBody(req);
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ output: "x:5 y:7", screenshot: "cGl4ZWxz" }));
    });

    const result = await current.client.environments.computerToolResult("vm-1", {
      action: "left_click",
      coordinate: [1, 2],
      future_field: true,
    });

    // unknown fields survive: the guest owns the action schema, not this sdk.
    expect(JSON.parse(seenBody)).toEqual({
      action: "left_click",
      coordinate: [1, 2],
      future_field: true,
    });
    expect(result.is_error).toBeUndefined();
    expect(result.content).toEqual([
      { type: "text", text: "x:5 y:7" },
      {
        type: "image",
        source: { type: "base64", media_type: "image/png", data: "cGl4ZWxz" },
      },
    ]);
  });

  it("turns a refusal into error content instead of throwing", async () => {
    current = await serve(async (_req, res) => {
      res.statusCode = 503;
      res.setHeader("content-type", "application/json");
      res.end(
        JSON.stringify({
          error: { code: "unavailable", message: "display :1 is not up" },
        }),
      );
    });

    const result = await current.client.environments.computerToolResult("vm-1", {
      action: "screenshot",
    });
    expect(result.is_error).toBe(true);
    expect(result.content[0]?.text).toContain("display :1 is not up");
  });

  it("still throws on failures the model cannot fix", async () => {
    current = await serve(async (_req, res) => {
      res.statusCode = 401;
      res.end();
    });
    await expect(
      current.client.environments.computerToolResult("vm-1", {
        action: "screenshot",
      }),
    ).rejects.toThrow();
  });

  it("answers error content for input naming no action", async () => {
    current = await serve(async () => {
      throw new Error("no request should be made");
    });
    const result = await current.client.environments.computerToolResult("vm-1", {});
    expect(result.is_error).toBe(true);
  });
});
