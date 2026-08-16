import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import type { Duplex } from "node:stream";
import { afterEach, describe, expect, it } from "vitest";
import { FuseClient, FuseApiError, VNC_PROTO } from "../src/index.js";

// The upgrade path cannot use test/server.ts: an upgrade bypasses the normal
// request handler, so the stub must listen for the `upgrade` event itself.

let server: Server | undefined;
const sockets: Duplex[] = [];
afterEach(async () => {
  // a socket handed to the `upgrade` event is detached from the server's
  // connection tracking, so close() would wait on it forever
  for (const s of sockets.splice(0)) s.destroy();
  if (server) {
    server.closeAllConnections();
    await new Promise<void>((resolve) => server!.close(() => resolve()));
    server = undefined;
  }
});

async function listen(s: Server): Promise<FuseClient> {
  await new Promise<void>((resolve) => s.listen(0, "127.0.0.1", () => resolve()));
  const address = s.address() as AddressInfo;
  return new FuseClient({ baseUrl: `http://127.0.0.1:${address.port}`, token: "tok" });
}

describe("environments.computerStream", () => {
  it("upgrades and relays raw bytes both directions", async () => {
    const greeting = "RFB 003.008\n";
    let seenUpgrade = "";
    let seenAuth = "";
    server = createServer();
    server.on("upgrade", (req, socket) => {
      sockets.push(socket);
      seenUpgrade = req.headers.upgrade ?? "";
      seenAuth = req.headers.authorization ?? "";
      socket.write(
        "HTTP/1.1 101 Switching Protocols\r\n" +
          `Upgrade: ${VNC_PROTO}\r\n` +
          "Connection: Upgrade\r\n\r\n" +
          greeting,
      );
      socket.on("data", (chunk: Buffer) => {
        socket.write(Buffer.concat([Buffer.from("echo:"), chunk]));
      });
    });
    const client = await listen(server);

    const stream = await client.environments.computerStream("vm-1");
    try {
      expect(seenUpgrade).toBe(VNC_PROTO);
      expect(seenAuth).toBe("Bearer tok");

      const read = (n: number): Promise<string> =>
        new Promise((resolve, reject) => {
          let got = Buffer.alloc(0);
          const onData = (chunk: Buffer) => {
            got = Buffer.concat([got, chunk]);
            if (got.length >= n) {
              stream.off("data", onData);
              resolve(got.toString("utf-8"));
            }
          };
          stream.on("data", onData);
          stream.once("error", reject);
        });

      // guest -> client: the greeting rides right behind the response head
      expect(await read(greeting.length)).toBe(greeting);

      // client -> guest and back
      stream.write("hello");
      expect(await read("echo:hello".length)).toBe("echo:hello");
    } finally {
      stream.destroy();
    }
  });

  it("surfaces a plain HTTP refusal as a FuseApiError", async () => {
    server = createServer((_req, res) => {
      res.statusCode = 503;
      res.setHeader("content-type", "application/json");
      res.end(
        JSON.stringify({
          error: { code: "unavailable", message: "guest surface unavailable" },
        }),
      );
    });
    const client = await listen(server);

    const err = await client.environments
      .computerStream("vm-1")
      .then(() => undefined)
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(FuseApiError);
    expect((err as FuseApiError).status).toBe(503);
    expect((err as FuseApiError).code).toBe("unavailable");
  });

  it("rejects an empty vm id before any request is made", async () => {
    server = createServer(() => {
      throw new Error("no request should be made");
    });
    const client = await listen(server);
    await expect(client.environments.computerStream("")).rejects.toThrow(
      "vm id is required",
    );
  });
});
