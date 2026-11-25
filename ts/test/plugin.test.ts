// The kit against ws's side of the protocol, played here over streams: what
// a plugin registers, and how it answers what ws asks.

import { test } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { mkdtemp, mkdir, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { request } from "node:http";
import { Conn, NotFoundError, Plugin, StandingSettled, blocks, fields, RPCError } from "../src/index.ts";
import type { InitializeParams, NotificationsToPlugin, NotificationsToWS, RequestsToPlugin, RequestsToWS } from "../src/protocol.gen.ts";

type FromWS = { requests: RequestsToPlugin; notifications: NotificationsToPlugin };

/** ws's side of a connection to p: what it's told, and what it's asked (answered by answer). */
async function connect(p: Plugin, answer: (method: string, params: unknown) => unknown = () => null) {
  const toPlugin = new PassThrough();
  const toWS = new PassThrough();
  const told: { method: string; params: unknown }[] = [];
  const ws = new Conn<FromWS>(toWS, toPlugin, async (method, params) => {
    const sent = method as keyof NotificationsToWS | keyof RequestsToWS;
    told.push({ method: sent, params });
    return answer(method, params);
  });
  const served = p.serve(toPlugin, toWS);
  const output = (id: string) =>
    told
      .filter((t) => t.method === "command/output" && (t.params as { id: string }).id === id)
      .map((t) => (t.params as { data: string }).data)
      .join("");
  return { ws, told, served, output, close: () => toPlugin.end() };
}

function init(over: Partial<InitializeParams> = {}): InitializeParams {
  return {
    protocol: 1,
    protocols: [1],
    ws: { version: "test" },
    plugin: { name: "hello", version: "0.1.0", dir: "/p", data: "/p/data" },
    workspace: { root: "/w" },
    config: {},
    view: { listen: "", base: "/plugins/hello" },
    ...over,
  };
}

const run = (args: string[], id = "r1") => ({ id, args, dir: "/w", terminal: { tty: false, width: 0, color: false } });

test("initialize answers with what's declared", async () => {
  const p = new Plugin();
  p.describe("Says hello");
  const greeting = p.config.string("greeting", "Greeting", { default: "Hello" });
  p.command("say", "Says the greeting", (c) => c.say(greeting.get())).args("[NAME]").option("to", "who to").label("Saying {1}");
  p.ref("visit", "Visit").prefix("VISIT-").fields(fields.title("title"), fields.time("at", "When")).card(blocks.title("title"), blocks.subtitle(blocks.Key));
  p.action("greet", "Say hello to…", () => {}).icon("sparkle");
  const c = await connect(p);
  const got = await c.ws.call("initialize", init());
  assert.equal(got.protocol, 1);
  assert.deepEqual(got.protocols, [1]);
  assert.deepEqual(got.registration, {
    description: "Says hello",
    commands: { name: "hello", short: "Says hello", subcommands: [{ name: "say", short: "Says the greeting", args: "[NAME]", label: "Saying {1}", options: ["to"] }] },
    refs: [
      {
        kind: "visit",
        title: "Visit",
        prefixes: ["VISIT-"],
        fields: [{ name: "title", type: "title" }, { name: "at", type: "time", label: "When" }],
        views: { card: [{ block: "title", field: "title" }, { block: "subtitle", field: "key" }] },
      },
    ],
    actions: [{ id: "greet", title: "Say hello to…", icon: "sparkle" }],
    config: [{ key: "greeting", title: "Greeting", type: "string", default: "Hello" }],
  });
  c.close();
  await c.served;
});

test("no version in common: it says which it speaks, and registers nothing", async () => {
  const p = new Plugin();
  p.command("say", "", () => {});
  const c = await connect(p);
  const got = await c.ws.call("initialize", init({ protocol: 7, protocols: [7] }));
  assert.deepEqual(got, { protocol: 0, protocols: [1], registration: {} });
  c.close();
});

test("a command's output streams to ws, and it ends with its exit code", async () => {
  const p = new Plugin();
  const greeting = p.config.string("greeting", "Greeting", { default: "Hello" });
  p.command("say", "Says the greeting", (c) => c.say(`${greeting.get()}, ${c.arg(0) || "workspace"}`));
  p.command("visits", "How often", (c) => c.show({ visits: 3 }));
  p.command("fail", "Fails", () => {
    throw new Error("it went wrong");
  });
  const c = await connect(p);
  await c.ws.call("initialize", init({ config: { greeting: "Hi" } }));

  assert.deepEqual(await c.ws.call("command/run", run(["say", "James"], "a")), { exit: 0 });
  assert.equal(c.output("a"), "Hi, James\n");

  assert.deepEqual(await c.ws.call("command/run", run(["say", "-f", "json"], "b")), { exit: 0 });
  assert.equal(c.output("b"), '{\n  "message": "Hi, workspace"\n}\n');

  await c.ws.call("command/run", run(["visits"], "c"));
  assert.equal(c.output("c"), "Visits:  3\n");
  await c.ws.call("command/run", run(["visits", "-f", "llm"], "d"));
  assert.equal(c.output("d"), "- **Visits**: 3\n");

  assert.deepEqual(await c.ws.call("command/run", run(["fail"], "e")), { exit: 1 });
  assert.equal(c.output("e"), "it went wrong\n");

  assert.deepEqual(await c.ws.call("command/run", run(["nope"], "f")), { exit: 2 });
  assert.match(c.output("f"), /^unknown command "nope"\n\nUsage:\n  ws hello <command> \[flags\]/);

  assert.deepEqual(await c.ws.call("command/run", run(["say", "--loud"], "g")), { exit: 2 });
  assert.match(c.output("g"), /^unknown flag --loud\n/);

  await c.ws.call("command/run", run([], "h"));
  assert.match(c.output("h"), /Commands:\n {2}fail {4}Fails\n {2}say {5}Says the greeting\n {2}visits {2}How often\n/);
  c.close();
});

test("config changes: values read the new ones, and the app's told it's stale", async () => {
  const p = new Plugin();
  const greeting = p.config.string("greeting", "Greeting", { default: "Hello" });
  let seen = "";
  p.onConfig(() => (seen = greeting.get()));
  const c = await connect(p);
  await c.ws.call("initialize", init());
  assert.equal(greeting.get(), "Hello");
  c.ws.notify("config/changed", { config: { greeting: "Hey" } });
  await until(() => seen === "Hey");
  await until(() => c.told.some((t) => t.method === "view/changed"));
  c.close();
});

test("a key read in setup changing restarts the plugin", async () => {
  const p = new Plugin();
  const project = p.config.string("project", "Project");
  p.setup(() => {
    if (project.get()) p.command("board", `The ${project.get()} board`, () => {});
  });
  const c = await connect(p);
  const got = await c.ws.call("initialize", init({ config: { project: "RAM" } }));
  assert.equal(got.registration.commands?.subcommands?.[0]?.short, "The RAM board");
  c.ws.notify("config/changed", { config: { project: "RAP" } });
  await c.served; // it stopped, to be started afresh
});

test("an action asks, and is run again with the answer", async () => {
  const p = new Plugin();
  p.action("greet", "Say hello to…", async (a) => {
    const name = await a.ask("Who to?", "A name");
    a.say(`Hello, ${name}`);
  });
  const c = await connect(p);
  await c.ws.call("initialize", init());
  assert.deepEqual(await c.ws.call("action/run", { id: "greet", answers: [] }), { ask: { title: "Who to?", type: "text", placeholder: "A name" } });
  assert.deepEqual(await c.ws.call("action/run", { id: "greet", answers: ["James"] }), { say: "Hello, James" });
  await assert.rejects(c.ws.call("action/run", { id: "nope", answers: [] }), (e: RPCError) => e.code === -32602);
  c.close();
});

test("ref/show: a thing as it is now, or not found", async () => {
  const p = new Plugin();
  p.ref("visit", "Visit")
    .prefix("VISIT-")
    .fields(fields.title("title"))
    .show(({ name }) => {
      if (name !== "VISIT-1") throw new NotFoundError();
      return { name, changed: new Date("2026-11-21T09:00:00.123Z"), fields: { title: "Visit 1" } };
    });
  const c = await connect(p);
  await c.ws.call("initialize", init());
  assert.deepEqual(await c.ws.call("ref/show", { kind: "visit", ref: "VISIT-1" }), { changed: "2026-11-21T09:00:00Z", fields: { title: "Visit 1" } });
  await assert.rejects(c.ws.call("ref/show", { kind: "visit", ref: "VISIT-2" }), (e: RPCError) => e.code === -32001 && e.message === "there's no visit VISIT-2");
  c.close();
});

test("things are put to ws, and what it refused is said", async () => {
  const p = new Plugin();
  const visit = p.ref("visit", "Visit").prefix("VISIT-").fields(fields.title("title"));
  p.command("tell", "", async () => {
    await visit.put([{ name: "VISIT-1", changed: new Date("2026-11-21T09:00:00Z"), standing: StandingSettled, since: new Date("2026-11-21T08:00:00Z"), fields: { title: "One" } }, { name: "VISIT-0", fields: {} }]);
  });
  const c = await connect(p, (method, params) => {
    if (method === "thing/put") {
      const { things } = params as { things: { ref: string }[] };
      return { refused: things.filter((t) => t.ref === "VISIT-0").map((t) => ({ kind: "visit", ref: t.ref, why: "it isn't one of the kind's names" })) };
    }
    return null;
  });
  await c.ws.call("initialize", init());
  assert.deepEqual(await c.ws.call("command/run", run(["tell"], "t")), { exit: 1 });
  assert.equal(c.output("t"), "ws didn't keep VISIT-0: it isn't one of the kind's names\n");
  const put = c.told.find((t) => t.method === "thing/put")!.params;
  assert.deepEqual(put, { things: [{ kind: "visit", ref: "VISIT-1", fields: { title: "One" }, changed: "2026-11-21T09:00:00Z", standing: "settled", since: "2026-11-21T08:00:00Z" }, { kind: "visit", ref: "VISIT-0", fields: {} }] });
  c.close();
});

test("a command reads its standard input from ws", async () => {
  const p = new Plugin();
  p.command("count", "", async (c) => c.say(String((await c.stdin()).length)));
  const chunks = [Buffer.from("hello "), Buffer.from("world")];
  const c = await connect(p, (method) => {
    if (method !== "command/read") return null;
    const next = chunks.shift();
    return next ? { data: next.toString("base64") } : { eof: true };
  });
  await c.ws.call("initialize", init());
  await c.ws.call("command/run", { ...run(["count"], "s"), input: "piped" });
  assert.equal(c.output("s"), "11\n");
  c.close();
});

test("ws giving up on a command aborts its signal", async () => {
  const p = new Plugin();
  p.command("wait", "", (c) => new Promise((_, reject) => c.signal.addEventListener("abort", () => reject(new Error("given up")))));
  const c = await connect(p);
  await c.ws.call("initialize", init());
  const ac = new AbortController();
  const asked = c.ws.call("command/run", run(["wait"], "w"), ac.signal);
  setTimeout(() => ac.abort(), 50);
  await assert.rejects(asked);
  await until(() => c.output("w") === "given up\n");
  c.close();
});

test("the app's files and endpoints are served on the socket ws gives", async () => {
  const root = await mkdtemp(join(tmpdir(), "ws-plugin-"));
  await mkdir(join(root, "ui"));
  await writeFile(join(root, "ui", "app.js"), "console.log('app')");
  const p = new Plugin({ root });
  p.ui("ui");
  p.view("Hello", { icon: "sparkle" });
  p.get("/api/hello", () => ({ greeting: "Hello" }));
  p.post<{ name: string }, { said: string }>("/api/say", (_, body) => ({ said: `Hello, ${body.name}` }));
  p.get("/api/issue/{key}", ({ params }) => {
    if (params.key !== "RAM-1") throw new NotFoundError(`there's no ${params.key}`);
    return { key: params.key };
  });
  const c = await connect(p);
  const listen = join(root, "view.sock");
  const got = await c.ws.call("initialize", init({ view: { listen, base: "/plugins/hello" } }));
  assert.match(got.registration.ui?.script ?? "", /^\/ui\/app\.js\?v=[0-9a-f]{8}$/);
  assert.deepEqual(got.registration.views, [{ title: "Hello", path: "/", icon: "sparkle" }]);

  assert.deepEqual(await fetchUnix(listen, "GET", "/api/hello"), { status: 200, body: '{"greeting":"Hello"}\n' });
  assert.deepEqual(await fetchUnix(listen, "POST", "/api/say", '{"name":"James"}'), { status: 200, body: '{"said":"Hello, James"}\n' });
  assert.deepEqual(await fetchUnix(listen, "POST", "/api/say", ""), { status: 400, body: "POST /api/say takes JSON: it was sent no body\n" });
  assert.deepEqual(await fetchUnix(listen, "GET", "/api/issue/RAM-2"), { status: 404, body: "there's no RAM-2\n" });
  assert.deepEqual(await fetchUnix(listen, "GET", "/ui/app.js"), { status: 200, body: "console.log('app')" });
  assert.equal((await fetchUnix(listen, "GET", "/ui/../secret")).status, 404);
  assert.equal((await fetchUnix(listen, "DELETE", "/api/hello")).status, 405);
  c.close();
  await c.served;
});

test("shutdown is answered, and then the plugin stops", async () => {
  const p = new Plugin();
  const c = await connect(p);
  await c.ws.call("initialize", init());
  assert.equal(await c.ws.call("shutdown", undefined), null);
  await c.served;
});

test("a declaration that's wrong is said at initialize", async () => {
  const p = new Plugin();
  p.ref("Visit", "Visit");
  p.view("Hello");
  const c = await connect(p);
  await assert.rejects(c.ws.call("initialize", init()), (e: RPCError) => /has no app to show/.test(e.message) && /its kind "Visit" must be lower-case/.test(e.message));
  c.close();
});

async function until(ok: () => boolean, ms = 2000) {
  const end = Date.now() + ms;
  while (!ok()) {
    if (Date.now() > end) throw new Error("timed out waiting");
    await new Promise((r) => setTimeout(r, 5));
  }
}

function fetchUnix(socketPath: string, method: string, path: string, body?: string): Promise<{ status: number; body: string }> {
  return new Promise((resolve, reject) => {
    const req = request({ socketPath, method, path, headers: body !== undefined ? { "Content-Type": "application/json" } : {} }, (res) => {
      const parts: Buffer[] = [];
      res.on("data", (c: Buffer) => parts.push(c));
      res.on("end", () => resolve({ status: res.statusCode ?? 0, body: Buffer.concat(parts).toString() }));
    });
    req.on("error", reject);
    req.end(body);
  });
}
