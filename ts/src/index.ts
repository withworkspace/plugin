// @withworkspace/plugin: the framework for writing a ws plugin in
// TypeScript, on Node (22.18 or later) or Bun.
//
//   import { Plugin, blocks, fields } from "@withworkspace/plugin";
//
//   const p = new Plugin();
//   const greeting = p.config.string("greeting", "Greeting", { default: "Hello" });
//   p.ui("ui/dist");
//   p.view("Hello", { icon: "sparkle" });
//   p.get("/api/greeting", () => ({ greeting: greeting.get() }));
//   p.command("say", "Says the greeting", (c) => c.say(`${greeting.get()}, workspace`));
//   p.run();
//
// Declaring a thing and handling it are one call, and what ws is told the
// plugin adds (its registration) is written from those calls. It's the
// Go kit's (github.com/withworkspace/plugin) way, and its protocol: both
// are generated from one spec, github.com/withworkspace/protocol.

export { Plugin, type Log, type Looking, type PluginOptions, type ViewOptions } from "./plugin.ts";
export { Config, Key, type KeyOptions } from "./config.ts";
export { Call, Command, ExitError } from "./command.ts";
export { FormatJSON, FormatLLM, FormatTable, type Format } from "./output.ts";
export { Act, Action, choices, type Choice } from "./action.ts";
export { Ref, RefusedError, type Asked, type Thing } from "./ref.ts";
export { StandingGone, StandingLasting, StandingOpen, StandingPassing, StandingSettled, type Standing } from "./protocol.gen.ts";
export { StatusError, type RawHandler, type Request } from "./view.ts";
export { NotFoundError, UnsupportedError } from "./host.ts";
export { stamp } from "./fields.ts";
export { Versions, choose } from "./rules.ts";
export { Conn, RPCError, Then, type Handler, type Methods } from "./rpc.ts";
export * as fields from "./fields.ts";
export * as blocks from "./blocks.ts";
