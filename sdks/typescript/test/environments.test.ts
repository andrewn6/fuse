import { afterEach, describe, expect, it } from "vitest";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";
import { FuseApiError, isConflict, isFuseApiError } from "../src/index.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("environments", () => {
  it("create posts JSON to /v1/environments and decodes the response", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let auth: string | undefined;
    let body = "";
    current = await serve(async (req, res) => {
      method = req.method;
      path = pathOf(req);
      auth = req.headers.authorization;
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"running","task_id":"task-1","url":"https://x"}`);
    });

    const env = await current.client.environments.create({ task_id: "task-1" });

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments");
    expect(auth).toBe("Bearer tok");
    expect(JSON.parse(body)).toEqual({ task_id: "task-1" });
    expect(env.id).toBe("vm-1");
    expect(env.state).toBe("running");
  });

  it("create serializes spec.gpu_profile for a fractional MIG request", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"running","task_id":"task-1","url":"https://x"}`);
    });

    await current.client.environments.create({
      task_id: "task-1",
      spec: { gpus: 2, gpu_kind: "a100", gpu_profile: "1g.10gb" },
    });

    expect(JSON.parse(body)).toEqual({
      task_id: "task-1",
      spec: { gpus: 2, gpu_kind: "a100", gpu_profile: "1g.10gb" },
    });
  });

  it("create serializes spec.image and expose to snake_case JSON", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"running","task_id":"task-1","url":"https://x"}`);
    });

    await current.client.environments.create({
      task_id: "task-1",
      spec: { cpus: 2, image: "ghcr.io/acme/app:latest" },
      expose: [{ port: 8080, as: "web" }, { port: 5432 }],
    });

    expect(JSON.parse(body)).toEqual({
      task_id: "task-1",
      spec: { cpus: 2, image: "ghcr.io/acme/app:latest" },
      expose: [{ port: 8080, as: "web" }, { port: 5432 }],
    });
  });

  it("get decodes an endpoints array on EnvironmentInfo", async () => {
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"vm-1","state":"running","task_id":"task-1","url":"u",` +
          `"endpoints":[{"as":"web","url":"https://web.x","port":8080},` +
          `{"url":"https://db.x","port":5432}]}`,
      );
    });

    const env = await current.client.environments.get("vm-1");

    expect(env.endpoints).toEqual([
      { as: "web", url: "https://web.x", port: 8080 },
      { url: "https://db.x", port: 5432 },
    ]);
  });

  it("fork posts action=fork with a ForkOptions body and decodes the result", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    let body = "";
    current = await serve(async (req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-2","state":"running","task_id":"task-1","url":"u"}`);
    });

    const env = await current.client.environments.fork("vm-1", {
      reuse_snapshot_id: "snap-1",
      comment: "forked",
    });

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1");
    expect(query).toBe("action=fork");
    expect(JSON.parse(body)).toEqual({
      reuse_snapshot_id: "snap-1",
      comment: "forked",
    });
    expect(env.id).toBe("vm-2");
  });

  it("exec posts action=exec with an argv body and decodes the result", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    let body = "";
    current = await serve(async (req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"exit_code":0,"stdout":"hi\\n","stderr":""}`);
    });

    const out = await current.client.environments.exec("vm-1", {
      cmd: ["ls", "-l"],
      timeout_ms: 5000,
    });

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1");
    expect(query).toBe("action=exec");
    expect(JSON.parse(body)).toEqual({ cmd: ["ls", "-l"], timeout_ms: 5000 });
    expect(out).toEqual({ exit_code: 0, stdout: "hi\n", stderr: "" });
  });

  it("exec returns a non-zero exit_code instead of throwing", async () => {
    current = await serve((req, res) => {
      // the command ran and failed: still a 200, not an api error.
      res.setHeader("Content-Type", "application/json");
      res.end(`{"exit_code":2,"stdout":"","stderr":"no such file\\n"}`);
    });

    const out = await current.client.environments.exec("vm-1", {
      cmd: ["cat", "/nope"],
    });

    expect(out.exit_code).toBe(2);
    expect(out.stderr).toBe("no such file\n");
    expect(out.stdout).toBe("");
  });

  it("exec posts a shell body for pipelines", async () => {
    let query: string | undefined;
    let body = "";
    current = await serve(async (req, res) => {
      query = queryOf(req);
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"exit_code":0,"stdout":"3\\n","stderr":""}`);
    });

    const out = await current.client.environments.exec("vm-1", {
      shell: "ls | wc -l",
    });

    expect(query).toBe("action=exec");
    expect(JSON.parse(body)).toEqual({ shell: "ls | wc -l" });
    expect(out.stdout).toBe("3\n");
  });

  it("exec throws a FuseApiError when the vm is not running", async () => {
    current = await serve((req, res) => {
      res.statusCode = 409;
      res.setHeader("Content-Type", "application/json");
      res.end(`{"error":{"code":"conflict","message":"vm is draining"}}`);
    });

    let caught: unknown;
    try {
      await current.client.environments.exec("vm-1", { cmd: ["ls"] });
    } catch (err) {
      caught = err;
    }

    expect(isFuseApiError(caught)).toBe(true);
    expect(isConflict(caught)).toBe(true);
    expect((caught as FuseApiError).status).toBe(409);
  });

  it("exec rejects a body that is not exactly one of cmd or shell", async () => {
    let called = false;
    current = await serve((req, res) => {
      called = true;
      res.setHeader("Content-Type", "application/json");
      res.end(`{"exit_code":0,"stdout":"","stderr":""}`);
    });

    await expect(current.client.environments.exec("vm-1", {})).rejects.toThrow(
      "one of cmd or shell is required",
    );
    await expect(
      current.client.environments.exec("vm-1", { cmd: ["ls"], shell: "ls" }),
    ).rejects.toThrow("cmd and shell are mutually exclusive");

    expect(called).toBe(false);
  });

  it("list sends snake_case query and unwraps the envelope", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"environments":[{"id":"vm-1","state":"running","task_id":"task-1","url":"u"},` +
          `{"id":"vm-2","state":"draining","task_id":"task-1","url":"u"}]}`,
      );
    });

    const envs = await current.client.environments.list({ taskId: "task-1" });

    expect(method).toBe("GET");
    expect(path).toBe("/v1/environments");
    // list() auto-paginates (walks every page), so it always sends the max
    // page size alongside the caller's filters.
    expect(query).toBe("task_id=task-1&limit=200");
    expect(envs.map((e) => e.id)).toEqual(["vm-1", "vm-2"]);
  });

  it("listPage round-trips the next_cursor into the cursor query param", async () => {
    const seen: (string | undefined)[] = [];
    current = await serve((req, res) => {
      const q = queryOf(req);
      seen.push(q);
      res.setHeader("Content-Type", "application/json");
      if (!q.includes("cursor=")) {
        res.end(
          `{"environments":[{"id":"vm-1","state":"running","url":"u"}],"next_cursor":"YWJj"}`,
        );
      } else {
        res.end(`{"environments":[{"id":"vm-2","state":"running","url":"u"}]}`);
      }
    });

    const page1 = await current.client.environments.listPage({ limit: 1 });
    expect(page1.nextCursor).toBe("YWJj");
    expect(page1.environments.map((e) => e.id)).toEqual(["vm-1"]);

    const page2 = await current.client.environments.listPage({
      cursor: page1.nextCursor,
    });
    expect(page2.nextCursor).toBeUndefined();
    expect(page2.environments.map((e) => e.id)).toEqual(["vm-2"]);
    expect(seen).toEqual(["limit=1", "cursor=YWJj"]);
  });

  it("list auto-paginates across next_cursor pages", async () => {
    let calls = 0;
    current = await serve((req, res) => {
      calls++;
      const q = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      if (!q.includes("cursor=")) {
        res.end(
          `{"environments":[{"id":"vm-1","state":"running","url":"u"}],"next_cursor":"YWJj"}`,
        );
      } else {
        res.end(`{"environments":[{"id":"vm-2","state":"running","url":"u"}]}`);
      }
    });

    const envs = await current.client.environments.list();
    expect(envs.map((e) => e.id)).toEqual(["vm-1", "vm-2"]);
    expect(calls).toBe(2);
  });

  it("drain posts action=drain and returns the updated environment", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"draining","task_id":"task-1","url":"u"}`);
    });

    const env = await current.client.environments.drain("vm-1");

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1");
    expect(query).toBe("action=drain");
    expect(env.state).toBe("draining");
  });

  it("rotateToken posts action=rotate-token and resolves on 204", async () => {
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

    await expect(
      current.client.environments.rotateToken("vm-1"),
    ).resolves.toBeUndefined();

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1");
    expect(query).toBe("action=rotate-token");
  });

  it("destroy issues DELETE and resolves on 204", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.statusCode = 204;
      res.end();
    });

    await current.client.environments.destroy("vm-1");

    expect(method).toBe("DELETE");
    expect(path).toBe("/v1/environments/vm-1");
  });
});
