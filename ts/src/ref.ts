// Refs: a kind of thing the plugin has that words name (a Jira issue,
// RAM-123). ws finds the names wherever they're written, links them, and
// draws them from what the plugin declares here, as data. What the plugin
// tells ws of its things (put), ws keeps, so they're found and drawn
// without asking it.

import type { Block, Field, Ref as RefSpec, Searched, Standing, ThingRefused } from "./protocol.gen.ts";
import { ThingsAtOnce, ViewCard, ViewPane } from "./protocol.gen.ts";
import type { Act } from "./action.ts";
import type { Host } from "./host.ts";
import { stamp } from "./fields.ts";

/** One of a kind's things: its name (RAM-123), when it last changed, and a value for each of its kind's fields. */
export interface Thing {
  name: string;
  changed?: Date;
  /**
   * Where it stands, in words every kind shares: "settled" for an issue
   * that's done, "gone" for one closed as won't do (StandingSettled,
   * StandingGone). What's found of one that's settled, or no longer so, is
   * found below what's still so, the more so the longer since. None is open.
   */
  standing?: Standing;
  /** When it came to stand so, for one that's settled or gone (an issue's resolution date). None is from when it last changed. */
  since?: Date;
  fields: Record<string, string>;
}

/** What ws asks about one thing: its name as written, and whether everything was asked for (ws show --full). */
export interface Asked {
  name: string;
  full: boolean;
}

/** ws didn't keep some of what it was told: which, and why. */
export class RefusedError extends Error {
  readonly things: ThingRefused[];
  constructor(things: ThingRefused[]) {
    const said = things.slice(0, 3).map((t) => `${t.ref}: ${t.why}`);
    if (things.length > 3) said.push(`and ${things.length - 3} more`);
    super(`ws didn't keep ${said.join("; ")}`);
    this.name = "RefusedError";
    this.things = things;
  }
}

/** A kind of thing, as declared. Its methods say more of it, and return it. */
export class Ref {
  /** @internal */ readonly spec: RefSpec;
  /** @internal */ showFn?: (a: Asked, signal: AbortSignal) => Thing | Promise<Thing>;
  /** @internal */ readonly acts = new Map<string, (a: Act) => unknown>();

  private host: Host;

  constructor(host: Host, kind: string, title: string) {
    this.host = host;
    this.spec = { kind, title, prefixes: [] };
  }

  /** What its names start with: RAM-, RAP-. */
  prefix(...prefixes: string[]): this {
    this.spec.prefixes.push(...prefixes);
    return this;
  }

  /** Where one is outside ws, with {ref} where its name goes. */
  address(template: string): this {
    this.spec.address = template;
    return this;
  }

  /** Where one opens in the plugin's app, with {ref} where its name goes: /issue/{ref}. */
  opens(path: string): this {
    this.spec.path = path;
    return this;
  }

  /** One of ws's icon names. */
  icon(name: string): this {
    this.spec.icon = name;
    return this;
  }

  /** What one of its things is made of. */
  fields(...fields: Field[]): this {
    (this.spec.fields ??= []).push(...fields);
    return this;
  }

  /** What a thing's found by, in order: a field's name for all of it, or {field, max} for its first max bytes. */
  search(...by: (string | Searched)[]): this {
    (this.spec.search ??= []).push(...by.map((b) => (typeof b === "string" ? { field: b } : b)));
    return this;
  }

  /** How one's drawn under its name, on hover. */
  card(...blocks: Block[]): this {
    (this.spec.views ??= {})[ViewCard] = blocks;
    return this;
  }

  /** How one's drawn beside the list in Find, for the row the cursor's on. */
  pane(...blocks: Block[]): this {
    (this.spec.views ??= {})[ViewPane] = blocks;
    return this;
  }

  /** Something that can be done to one of its things, from Find. */
  action(id: string, title: string, fn: (a: Act) => unknown): this {
    (this.spec.actions ??= []).push({ id, title });
    this.acts.set(id, fn);
    return this;
  }

  /** One thing as the plugin has it now, by its name: throw NotFoundError for none. */
  show(fn: (a: Asked, signal: AbortSignal) => Thing | Promise<Thing>): this {
    this.showFn = fn;
    return this;
  }

  /** Tells ws of things, to keep: each added, or put in place of what it had by that name. */
  async put(things: Thing[], signal?: AbortSignal): Promise<void> {
    const refused: ThingRefused[] = [];
    for (let i = 0; i < things.length; i += ThingsAtOnce) {
      const batch = things.slice(i, i + ThingsAtOnce).map((t) => {
        const wire: { kind: string; ref: string; fields: Record<string, string>; changed?: string; standing?: Standing; since?: string } = { kind: this.spec.kind, ref: t.name, fields: t.fields ?? {} };
        if (t.changed) wire.changed = stamp(t.changed);
        if (t.standing) wire.standing = t.standing;
        if (t.since) wire.since = stamp(t.since);
        return wire;
      });
      const out = await this.host.ask("thing/put", { things: batch }, signal);
      refused.push(...(out.refused ?? []));
    }
    if (refused.length) throw new RefusedError(refused);
  }

  /** Tells ws to forget things, by name. */
  async remove(names: string[], signal?: AbortSignal): Promise<void> {
    if (names.length) await this.host.ask("thing/remove", { kind: this.spec.kind, refs: names }, signal);
  }

  /** Tells ws of these things, and to forget the rest of the kind's: what a sync does. */
  async mirror(things: Thing[], signal?: AbortSignal): Promise<void> {
    const had = await this.host.ask("thing/list", { kind: this.spec.kind }, signal);
    const have = new Set(things.map((t) => t.name));
    let refused: RefusedError | undefined;
    try {
      await this.put(things, signal);
    } catch (err) {
      if (!(err instanceof RefusedError)) throw err;
      refused = err;
    }
    await this.remove(had.refs.filter((n) => !have.has(n)), signal);
    if (refused) throw refused;
  }
}
