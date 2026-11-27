# plugin

The framework for writing ws plugins in Go: `github.com/withworkspace/plugin`. And in TypeScript, on Node or Bun: [`@withworkspace/plugin`](ts), in `ts/`. The two share their versions (one `v*` tag releases both) and their protocol's types, which are generated for each from one spec in [withworkspace/protocol](https://github.com/withworkspace/protocol).

This repo is the framework, not a plugin. Plugins are the repos beside it named `plugin-<name>`.

A plugin is one executable. ws's daemon starts it, and speaks to it over its stdin and stdout ([PROTOCOL.md](PROTOCOL.md)). This package speaks for it, so a plugin is what it declares:

```go
package main

import (
	"embed"
	"net/http"

	"github.com/withworkspace/plugin"
)

//go:embed agents.md migrations ui/dist
var files embed.FS

// Hello is what the app is sent.
type Hello struct {
	Greeting string `json:"greeting"`
}

func main() {
	p := plugin.New(files)

	greeting := p.Config.String("greeting", "Greeting", plugin.Default("Hello"))

	p.UI("ui/dist")
	p.View("Hello", plugin.Icon("sparkle"))
	plugin.Get(p, "/api/hello", func(r *http.Request) (Hello, error) {
		return Hello{Greeting: greeting.Get()}, nil
	})

	p.Command("say", "Says the greeting", func(c *plugin.Call) error {
		return c.Say(greeting.Get() + ", workspace")
	})

	p.Run()
}
```

That's a plugin with an app in ws's web UI, a `ws hello say` command, a setting, and a guide that tells agents it's there. [examples/hello](examples/hello) is the whole of it, with its app's source, a table of its own and a test.

## A plugin's web UI is an app

A plugin's web UI is one client-side app, and ws serves the page it runs in. The framework renders no pages, and a plugin has no HTML: no templates, no layout, nothing at a view's path.

- **The app** is a JS bundle, and a stylesheet if it wants one, built from the plugin's `ui/` into `ui/dist/` as `app.js` and `app.css`. `p.UI("ui/dist")` serves that folder and tells ws to load it.
- **The page** is ws's. It has ws's theme in it before the app loads, and an empty `<div id="root">` for the app to render into. ws serves it at every path under `/x/<name>/`, so a link to anywhere in the app loads the app there.
- **A view** is an entry in ws's sidebar that opens the app at a path: `p.View("Board", plugin.At("/board"))`.
- **ws keeps the history.** The app shows the screen at the path ws says it's at (`ws.path`, then `ws.onNavigate`), and moves by asking: a link to `ws.href("/board")`, or `ws.go("/board")`. Back, forward and ⌘-click all work as they do on ws's own pages.
- **The app talks to ws** with [`@withworkspace/ui-lib`](https://www.npmjs.com/package/@withworkspace/ui-lib): where it is, its title, the theme, and when what it shows is stale. How they talk is the frame protocol, in [`ui-lib`'s `PROTOCOL.md`](https://github.com/withworkspace/ui-lib/blob/main/PROTOCOL.md).
- **The app talks to the plugin** through the plugin's endpoints: `plugin.Get(p, "/api/issues", …)` here is `plugin.get("/api/issues")` there, with the types the Go says ([What the app asks the plugin](#what-the-app-asks-the-plugin)). `p.Changed()` tells the open app to ask again.

```ts
import { connect, endpoints } from "@withworkspace/ui-lib";
import type { Api } from "./api.gen";

const ws = await connect();
const plugin = endpoints<Api>(ws);
ws.setTitle("Hello");

async function show() {
  const { greeting } = await plugin.get("/api/hello");
  ws.root.textContent = `${greeting}, workspace`;
}
ws.onChanged(show);
show();
```

The app can be written with anything that bundles to one file. We suggest [Preact](https://preactjs.com): it's small (about 10 KB in the bundle), it's the JSX and hooks ws's own web UI is written in, and a new plugin from plugin-dev starts on it, with hooks onto ws (`usePath`, `useTitle`, `useAsk`) in its `ui/src/ws.ts`.

The app's built files are committed. `go:embed` needs them when the plugin is built, so with them Go alone builds, tests and installs a plugin, and only changing its app needs Node.

## Starting one

The [plugin-dev](https://github.com/withworkspace/plugin-plugin-dev) plugin writes a new plugin that works as it's written, and builds and runs it in a workspace on every save. It's a plugin itself, written with this:

```bash
ws plugin-dev new notes
```

```bash
cd plugin-notes
```

```bash
ws plugin-dev watch
```

Install it with `ws plugin add withworkspace/plugin-plugin-dev`. A new plugin comes with the workflow that publishes its releases: push a tag `v<version>`, and `ws plugin add <owner>/<repo>` installs it anywhere ([how it's published](PROTOCOL.md#a-plugin-as-its-published)).

## What's built in

Declaring a thing and handling it are one call, and what ws is told the plugin adds is written from those calls.

| | |
|---|---|
| `p.Config.String`, `URL`, `Secret`, `List`, `Bool`, `Number`, `Enum`, `Map` | A config key: in ws's Settings, asked for on install, read with `.Get()`. A change reaches `.Get()` at once. |
| `p.Config.Table` | A config key holding named rows (`[plugin.jira.queries.bugs]`), each with the fields declared with the table's own `String`, `Bool`, `Map`… and read with what each returned: `for _, row := range queries.Rows() { label.In(row) }`. Rows come in the order they were written, the team's with yours laid over them, each checked by ws against the fields. A field may be a table itself (`Table`, read with `RowsIn(row)`), one level down. |
| `p.UI` | The plugin's app: a folder of its built files, with `app.js` and, if it has one, `app.css`. It's served at `/ui/`, and registered with a hash of each file, so a browser asks again when it's built again. |
| `p.View`, with `plugin.Icon`, `Under`, `At` | An entry in ws's sidebar, which opens the app at a path. |
| `plugin.Get`, `Post`, `Put`, `Patch`, `Delete` | An endpoint: what the app asks the plugin, declared with the Go types it takes and answers, sent as JSON ([What the app asks the plugin](#what-the-app-asks-the-plugin)). They're functions, not methods, because Go gives a method no types of its own. |
| `plugin.Query` | Given to `Get` or `Delete` round the endpoint's function, it says the query the endpoint is asked with (what's after the `?`), as a struct the function is given: `plugin.Query(func(r *http.Request, q Since) (History, error) { … })`. |
| `plugin.Status` | An error as an endpoint answers it, with that HTTP status and not 500. An error type of the plugin's says its own with a method `HTTPStatus() int`. |
| `go tool apigen` | Writes `ui/src/api.gen.ts` from the endpoints: their types in TypeScript, and `Api`, which the app's client is given. `-check` fails if the file isn't what the Go says. |
| `p.Handle`, `p.HandleFunc` | Anything else the plugin's HTTP answers, as `net/http` takes it: `"GET /api/export"`. A path nothing handles is a plain 404. |
| `p.Command` | `ws <plugin> <name>`. Return data and it's printed as a table for a person, JSON for `-f json`, markdown for `-f llm`. |
| `.Conversations()` | On a command: a conversation's agent may run it itself, not only propose. For what's fine changed as the user watches (a drawing they can undo) and for reads; what acts for the user outside ws is a proposal. |
| `.Label` on a command | What it's doing, as a conversation shows an agent's step that ran it: `.Label("Pulling up {1}")` shows `ws sketch show sketch-5 -f llm` as "Pulling up sketch-5". `{1}` is its first argument, `{2}` its second. |
| `c.Stdin`, `c.Piped` | What's piped to a command (`ws <plugin> <name> < ops.json`), or the person typing. Read from ws as the command reads it; not `os.Stdin`, which is the plugin's connection. |
| `p.DB()` | A SQLite database in the plugin's data folder, with `migrations/*.sql` applied in order before anything runs. Pure Go: no C compiler. |
| `p.Sync` | The one place a plugin talks to its service: at start, on an interval, and on demand, one at a time, retried when it fails. Everything else reads the database. |
| `plugin.WhileOpen`, `p.Looking`, `p.OnLooking` | Fetching to suit who's looking. ws says when the plugin's app is on screen, and when any page of ws's is; it never asks for a fetch. `p.Sync("issues", 15*time.Minute, fetch, plugin.WhileOpen(time.Minute))` is every minute while the app's on screen (at once when it comes on screen with older than that) and every fifteen while it isn't; an interval of `0` is no fetching unless it is. `p.Looking()` and `p.OnLooking` are for a plugin that decides by hand. |
| `agents.md` | What agents are told of the plugin. |
| `p.Setup`, `p.OnConfig` | Declaring what depends on config, and hearing of a change. |
| `p.Log`, `p.Changed`, `p.Badge` | A log, telling the open app that what it shows is stale, the count in the sidebar. |
| `p.Missions`, `p.Mission`, `p.Repos` | Asking ws what it knows, in the protocol's types: the workspace's missions (where each stands, whose move it is, its capsules and pull requests), one with its brief and plan, and its repos with their capsules. `ErrNotFound` for a mission that isn't there, `ErrUnsupported` from a ws too old to answer. |
| `p.Show`, `p.ShowIn` | One of the workspace's things as `ws show` shows it, as data: what it is, what it links to, and what points at it from elsewhere (`Said`: the missions about one of the plugin's things, the conversations that name it), each with its page in ws. Each link and each thing said is asked for in turn. `ShowIn` is one of a mission's own (`t3`, `d1`). |
| `p.StartConversation` | Starts one of the workspace's conversations for the person, with what they say first as the plugin words it, and has its agent answer: how the app hands something on to ws to be talked over, and made a mission of there. It returns the conversation's page, for the app to open. |
| `p.Notifies`, with `.About`, `.Off`; `p.Notify` | Telling the person of something, in the same feed as ws's own notifications: declare each kind the plugin sends, which Settings › Notifications lists under the plugin's name for the person to send to their feed, to the desktop as well, or nowhere; then `p.Notify(ctx, plugin.Notice{Kind, Text, Refs, Key})` when it happens. A `Key` already sent isn't kept twice, so a sync can tell of what it saw without remembering what it told. `.Off()` is a kind sent nowhere until the person says. |
| `p.Ref`, with `.Prefix`, `.Address`, `.Opens`, `.Icon` | A kind of thing the plugin has that words name (`RAM-123`). ws finds the names wherever they're written and links them. Declared in `p.Setup` when the prefixes follow config. |
| `.Fields`, with `plugin.Title`, `State`, `Text`, `Time`, `Markdown` | What one of the kind's things is made of: a thing is a value for each. |
| `.Search`, with `plugin.All`, `Upto` | What one is found by, and how much of each field. ws makes the words it looks through, and makes them again when this changes. |
| `.Card`, `.Pane`, with `ui.Title`, `Subtitle`, `Badge`, `Field`, `Markdown` | How ws draws one, said in Go and sent as data: under its name, and beside the list in Find. ws draws it in its own web UI, and nothing of the plugin's runs there. The plugin's app (`p.UI`) is for what's more than looking. |
| `ref.Mirror`, `ref.Put`, `ref.Remove` | Telling ws of the things the plugin has, so ws finds and draws them without asking, running or not. `Mirror` is all of a kind, from a sync: what's gone is forgotten. ws keeps them under the plugin, and forgets them when it's removed. What ws wouldn't keep is a `*Refused`. |
| `plugin.Names`, `NamesSeveral` | A field that names other things (a parent, what it links to). ws links the two in its graph, by how the field says it points, and draws the name as what it names. |
| `p.Action`, `.Action` on a ref, with `a.Pick`, `a.Ask`, `a.Say`, `a.Open` | Something the plugin can do, chosen in Find: its own, or to one of its things. One that needs something asks for it first (a pick, a line), and is run again from the start with the answer: ask, then do. |
| `p.Opening`, `.Keys`, `.On`, `.Beside`, `a.Here`, `a.Open(path, plugin.Beside)` | One of the plugin's own that only goes somewhere in its app (ws goes at once); keys it suggests, for the person to take in Settings (never bound by ws); the kinds of what's open it's for (`mission`, `jira:issue`); opening beside the page; and what was open when it was asked for. |
| `.Show` | One thing, as it is now: for one ws wasn't told of, and for `ws show RAM-123`. `ErrNotFound` for a name the plugin has nothing by. |
| `.Remote`, with `plugin.Remote`, `ref.Remotes`, `ref.OpenRemote` | A kind whose things can live in a repo, for everyone with it: a folder each, holding the thing's own file, a `thing.toml` that says what it is, and what the plugin makes from the file. ws finds them, names each by its folder (`records/sketches/release-flow/`) and calls it what its `thing.toml` says. `Remote.Read` says what a file is found by, asked once for each version of it. The plugin keeps no list of them: `Remotes` asks ws which there are, and `OpenRemote` for one's file. |
| `Remote.Export`, `Remote.Saved`, `ref.Promote`, `ref.SaveRemote` | Putting one of the kind's things in a repo, and saving it there. ws does it, for the person: it asks `Export` for the thing as its folder is to hold it (`plugin.Package`: the title, the main file, what the plugin made from it), names the folder, writes its `thing.toml`, commits as the person, lands it, and tells `Saved` where it is and at what version. `Export` says the version what it gives was made from; where the thing has changed in its repo since, ws asks again with what's there (`Exporting.Current`), for the plugin to bring the two together. `ref.Promote(ctx, "sketch-12")` and `ref.SaveRemote` ask ws to, from a button of the plugin's own or a command. |
| `p.WS`, `p.WSCommand` | Running one of ws's own commands in the workspace: `p.WS(ctx, "show", "m12", "-f", "llm")`. For what ws doesn't answer by asking yet. |
| `.Timeout` on a command | How long it may run, where two minutes isn't it. `Timeout(0)` is until it's stopped. |
| `plugintest` | Runs the plugin inside a test, against a stand-in for ws: its commands, its config, and its HTTP as its app would ask it (`ws.Get(path)`, and `ws.Send(method, path, body)` for an endpoint that takes a body). `plugintest.Row(…)` is a row of a config table, `ws.Missions(…)`, `ws.Repos(…)` and `ws.Shows(…)` give the stand-in what it answers, `ws.Conversations()` has it start the conversations asked for and says what each was started with, `ws.Show(kind, name, full)` asks the plugin what one of its things is, `ws.Remotes(…)` gives the stand-in the things it has in its repos (it promotes and saves as ws does, asking the plugin for the thing, and returns what's there now) and `ws.Read(…)` asks the plugin what one's file is found by, `ws.Things(kind)` is what it's told ws of, `ws.Act` and `ws.ActOn` run its actions with what's been answered, and `ws.Look(…)` says who's looking. |

## What the app asks the plugin

An endpoint is declared with what it takes and what it answers, as Go types:

```go
// Issue is one issue, as the app shows it.
type Issue struct {
	Key      string    `json:"key"`
	Summary  string    `json:"summary"`
	Comments []Comment `json:"comments"`
}

// NewComment is what's sent to say something on an issue.
type NewComment struct {
	Text string `json:"text"`
}

plugin.Get(p, "/api/issues/{key}", func(r *http.Request) (Issue, error) {
	return find(r.Context(), r.PathValue("key"))
})
plugin.Post(p, "/api/issues/{key}/comment", func(r *http.Request, in NewComment) (Issue, error) {
	return comment(r.Context(), r.PathValue("key"), in.Text)
})
```

The framework reads the body as what's taken and sends what's answered, as JSON. An error is answered as its text: with 500, with 404 for `plugin.ErrNotFound`, or with the status it's given (`plugin.Status(http.StatusConflict, err)`). The request is the function's for the rest: `r.PathValue`, `r.Context()`.

An endpoint that's read with a query (what's after the `?`) says the query's type too, by being given `plugin.Query`:

```go
// Since is which of the history is asked for.
type Since struct {
	Key   string `json:"key,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

plugin.Get(p, "/api/history", plugin.Query(func(r *http.Request, q Since) (History, error) {
	return history(r.Context(), q.Key, q.Limit)
}))
```

`go tool apigen` reads those declarations and writes `ui/src/api.gen.ts` for the app: the types, in TypeScript, and `Api`, which says which endpoint takes and answers which.

```ts
export interface Api {
    "GET /api/history": { query: Since; out: History };
    "GET /api/issues/{key}": { params: { key: string }; out: Issue };
    "POST /api/issues/{key}/comment": { params: { key: string }; in: NewComment; out: Issue };
}
```

The app asks through [`@withworkspace/ui-lib`'s `endpoints`](https://github.com/withworkspace/ui-lib#asking-the-plugin), given `Api`:

```ts
const plugin = endpoints<Api>(ws);
const issue = await plugin.get("/api/issues/{key}", { params: { key } });  // an Issue
await plugin.post("/api/issues/{key}/comment", { params: { key }, body: { text } });
const recent = await plugin.get("/api/history", { query: { limit: 300 } });
```

So neither side keeps the other's types by hand. Change the Go, run `go tool apigen`, and `tsc` says what in the app has to follow: a field that's gone, a path that moved, a body that takes something new, a query's name that changed.

It's a tool of the plugin's module, so it's the framework's own version of itself:

```bash
go get -tool github.com/withworkspace/plugin/cmd/apigen   # once: a new plugin from plugin-dev has it
go tool apigen                                            # write ui/src/api.gen.ts
go tool apigen -check                                     # fail if it isn't what the Go says: for CI
```

`ui/src/api.gen.ts` is committed. `ws plugin-dev watch` writes it again on each save of the Go.

What holds for an endpoint:

- **Its types are exported, and have names.** What an endpoint takes or answers is a type with a name (`Issue`), a slice, map or pointer of one, or a plain value. Every type it's made of is exported too, in whichever of the plugin's packages it lives: apigen says which isn't. Only those types are written, not the rest of what the plugin exports.
- **`struct{}` is nothing.** An endpoint that takes it is sent no body, and one that answers it answers 204: `func(r *http.Request, _ struct{}) (struct{}, error)`.
- **It's declared where it's called**, with its path as a constant. apigen reads the source, and can't see through a function of the plugin's own that declares for it.
- **Names are the Go's.** `board.Person` is `Person` in the app. Where two packages have a type of one name, the types of each but the plugin's main get their package's name in front: `BoardPerson`.
- **What a type says is what's sent.** A named string type with constants is the union of them (`"open" | "done"`). `time.Time` is a string. A type with a `MarshalJSON` of its own is `unknown`, and one with a `MarshalText` a string. A field marked `omitempty` is optional. A pointer is `| null`.
- **Fill a list inside a struct.** A nil slice or map an endpoint answers with is sent as `[]` or `{}`, as its type says. One in a field is sent as `null` (Go's JSON), though its type says a list: make it empty, or mark it `omitempty`.
- **A query is a struct of plain values.** Each field of a query's type is a string, a number, a bool, a type that reads itself from text (`time.Time`), a pointer to one, or a slice of them, which is the name given more than once (`?label=a&label=b`). A field goes by its json name, and one marked `omitempty` may be left out by the app. A name that isn't sent leaves its field empty, and a value that can't be read as its field (`?limit=lots`) is a 400 that says which. An endpoint declared without `plugin.Query` is asked with no query. `Get` and `Delete` take one: what's sent with a body goes in the body.

Anything that isn't JSON both ways (a file, a stream, a webhook) is a handler, as `net/http` takes it: `p.Handle("GET /api/export", …)`, fetched at `ws.api("/api/export")`.

## A plugin's folder

```
plugin-hello/
  plugin.toml          name, version, command, and how it's built
  main.go
  agents.md            the guide for agents
  migrations/
    0001_visits.sql    the database, one file per change
  ui/
    package.json       the app: @withworkspace/ui-lib, and what builds it
    src/main.ts
    src/app.css
    src/api.gen.ts     what the endpoints take and answer, written by go tool apigen: committed
    dist/              the app as built (app.js, app.css): committed, and served at /ui/
```

After a change to an endpoint or a type it sends, write the app's types again, and commit `ui/src/api.gen.ts` with the change:

```bash
go tool apigen
```

After a change to the app, build it and commit `ui/dist/` with the change:

```bash
cd ui && npm install && npm run build
```

## Trying the example

```bash
just build
```

```bash
cd examples/hello && ./ws-hello --registration
```

prints what the plugin would tell ws it adds. In a workspace:

```bash
ws plugin add --link /path/to/plugin/examples/hello
```

`just ui` builds the example's app again, and `just ui-check` fails if what's committed in `examples/hello/ui/dist/` isn't what its source builds. `just api` writes the example's `ui/src/api.gen.ts` again from its Go, and `just check` fails if it isn't what the Go says.

## Moving a plugin from before v0.2.0

Before v0.2.0 the framework rendered a plugin's pages: Go templates in `views/`, wrapped in a layout with ws's `/plugin-kit.js` and `/plugin-kit.css`. All of that is gone, from here and from ws. A plugin built on it still starts, and ws says in its view that it has no app to show. `ws plugin check` says what's wrong with one.

| Gone | In its place |
|---|---|
| `v.Page`, the templates in `views/`, and the template functions `url`, `reltime` and `pill` | The app renders. What a page's function returned is an endpoint: `plugin.Get`. |
| `v.Post`, and the redirect back | An endpoint the app posts to: `plugin.Post`. |
| `v.Handle` | `p.Handle`: the same, on the plugin. |
| `p.View(title, func(v *plugin.View) { … })` | `p.View(title, options…)`, and `plugin.At(path)` where the entry doesn't open at `/`. |
| `plugin.Request` | `*http.Request`. |
| `static/`, served at `static/` | The app's folder, served at `/ui/`. |
| The error pages (a page that failed, a plugin that couldn't start) | A handler answers in plain text, and the app shows it: 503 with why while the database couldn't be brought up, 500 for a panic. |
| The sync footer on every page, and its Refresh | `p.SyncStates()` and `p.SyncNow()`, for a plugin whose app shows them. |
| `p.PageTimeout` | Nothing: a handler is given no time limit. |
| `plugintest`'s `Page` | `plugintest.Response`: `Get` and `Post` are as they were. |

## Releasing

A version of the framework is a tag `v<version>` on `main`: Go's module proxy serves it from there, and nothing else has to be built or published. A plugin depends on a tagged version (`go get github.com/withworkspace/plugin@v0.2.0`), not on a commit of `main`.

The framework is before 1.0, so a change that breaks plugins is a minor version. v0.2.0 is the one where pages went.

## Not here yet

Events (a mission's pull request merging, told to a plugin), starting a mission from a plugin, and an HTTP client.
