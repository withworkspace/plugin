# The plugin protocol

How ws and a plugin talk. Version 1. The types for everything here are written in TypeSpec in [`withworkspace/protocol`](https://github.com/withworkspace/protocol/tree/main/plugin/spec), from which each language's are generated; Go's are [`protocol`](protocol/protocol.gen.go) (`github.com/withworkspace/plugin/protocol`). The framework in this repo (`plugin`) speaks it for you; this is for ws's side, for a plugin in another language, and for settling an argument.

This is what's built: where ws and this differ, one of them has a bug.

It's the protocol of the plugin's process: the JSON-RPC between ws and the plugin, the plugin on disk and as it's published, and what it registers. A plugin's web UI is an app that ws shows in a page of its own, and how that app and ws talk is another protocol, the frame protocol, with versions of its own. It isn't here: it's [`ui-lib`'s `PROTOCOL.md`](https://github.com/withworkspace/ui-lib/blob/main/PROTOCOL.md). [The view](#the-view) says where the two meet.

## A plugin on disk

```
.ws/plugins/<name>/
  app/               the plugin's folder, or a link to one
    plugin.toml      name, version, command, build
    <the executable>
  registration.json  what it last registered (ws writes it)
  data/              the plugin's own files: plugin.db, plugin.log, view.sock
```

ws keeps its own files apart from the plugin's, so a folder linked in for development isn't written into.

```toml
name    = "jira"
version = "0.3.0"
command = ["./ws-jira"]
build   = ["go", "build", "-o", "ws-jira", "."]
```

`command` is run with the plugin's folder (`app/`) as its working directory.

`build` is optional: how the plugin's built from its source, run in its folder. ws runs it for a plugin added from a folder where what `command` starts isn't there, and when it's asked to (`ws plugin add --build`).

A plugin may exit on its own, with status 0, to be started afresh: one whose registration depends on config does when that config changes. ws starts it again at once.

## A plugin as it's published

A plugin is published as a repo on GitHub. `ws plugin add owner/repo` installs it.

A release of it holds, for each platform it's built for:

```
<name>_<tag>_<os>_<arch>.tar.gz     jira_v0.3.0_darwin_arm64.tar.gz
checksums.txt                       a line for each: "<sha256>  <file>", as sha256sum writes it
```

- `<os>` and `<arch>` are Go's: `darwin`, `linux`; `arm64`, `amd64`.
- The archive holds `plugin.toml` and what its `command` starts, at its top. It may hold other files and folders the plugin needs. It holds no links.
- ws installs the file whose name ends `_<os>_<arch>.tar.gz` for the machine it's on, from the latest release that isn't a prerelease, or the one tagged `owner/repo@<tag>`.
- ws checks the file against its line in `checksums.txt` before unpacking it. Read as anyone (with no `gh` logged in), a release's files are those `checksums.txt` lists, so a release needs one.
- The tag is `v` and `plugin.toml`'s `version`.

A repo with no release, or none for the machine, is installed from its source: ws fetches it and runs `plugin.toml`'s `build`.

The plugin-dev plugin gives a new plugin the workflow that publishes this on a tag.

## The connection

- JSON-RPC 2.0, one message per line, over the plugin's stdin (ws to the plugin) and stdout (the plugin to ws).
- The plugin's stderr is its log. ws appends it to `data/plugin.log`.
- Either side may send requests and notifications.
- A line that isn't JSON is skipped. A plugin must write nothing but messages to stdout.
- Giving up on a request is the notification `$/cancel_request` with `{"requestId": <id>}`. The side answering stops and answers with error `-32800`.
- A method either side doesn't know is answered with `-32601` (requests) or dropped (notifications).
- ws closing the plugin's stdin means: exit.

## Versions

- **The plugin protocol has a version of its own**, a whole number starting at 1. It isn't ws's version, the framework's, or the frame protocol's.
- **Within a version, changes only add:** a new optional field, or a new method that the other side may not know. Removing something, changing what it means, or requiring what was optional makes a new version.
- **Each side says every version it speaks,** in `initialize` and its answer, and the plugin picks the highest that both speak.
- **ws keeps speaking old versions.** A plugin built for version 1 goes on working in a ws that has gone on to 2.
- **`initialize`'s version fields hold across versions:** they're said before there's a version to say them in. A version may add fields to `initialize` and its answer, and nothing else.

The framework's list is `protocol.Versions`, fixed in each release: `[1]` today.

## ws to the plugin

### `initialize` (request)

Always first, once.

```json
{
  "protocol": 1,
  "protocols": [1],
  "ws": { "version": "0.42.0", "executable": "/usr/local/bin/workspace" },
  "plugin": {
    "name": "jira",
    "version": "0.3.0",
    "dir": "/ws/.ws/plugins/jira/app",
    "data": "/ws/.ws/plugins/jira/data"
  },
  "workspace": { "root": "/ws" },
  "config": { "site": "https://acme.atlassian.net", "projects": ["PROJ"] },
  "view": { "listen": "/ws/.ws/plugins/jira/data/view.sock", "base": "/plugins/jira" }
}
```

- `protocols` are the versions ws speaks, and `protocol` the newest of them. A ws from before versions were chosen sends only `protocol`, and speaks only that one. One that sends neither speaks 1.
- `ws.executable` is ws's own program. A plugin asks ws what it knows with requests ([Asking ws](#asking-ws)). For what ws doesn't answer that way yet, a plugin can run its commands, in the workspace's root: `<executable> show m12 -f llm`.
- `config` holds what's set, secrets included, by key. A key that isn't set is absent. Values are JSON: a string, a bool, a number, or an array of strings.
- `view.listen` is a unix socket's path. A plugin that serves anything (its app's files, what its app asks it) listens there for HTTP before it answers. The file may be left from a run before: remove it first. It's usually `data/view.sock`, and somewhere shorter under the system's temporary folder when that path would be too long for a socket: listen wherever it says.
- `view.base` is the path the plugin's HTTP is served under ([The view](#the-view)).

The answer:

```json
{ "protocol": 1, "protocols": [1], "registration": { … } }
```

- `protocol` is the version the plugin chose: the highest in both `protocols`. Everything after the answer is in it.
- `protocols` are the versions the plugin speaks.
- With none in common, the plugin answers `protocol` `0`, with its `protocols`, and registers nothing. ws doesn't use it, and says which side is behind: "Update ws" when the plugin's highest version is above ws's, and "Update the plugin" when it isn't. A plugin from before versions were chosen sends no `protocols`, and speaks only its `protocol`.

A plugin must answer even when its config is incomplete. ws asks nothing more of it until what's required is set.

### The registration

Every field is optional.

```json
{
  "description": "Jira issues beside your missions",
  "views": [
    { "title": "Jira", "icon": "ticket", "under": "tasks", "path": "/" }
  ],
  "ui": { "script": "/ui/app.js?v=3f9c2a", "style": "/ui/app.css?v=3f9c2a" },
  "commands": {
    "name": "jira",
    "short": "Jira issues",
    "subcommands": [
      { "name": "mine", "short": "Issues assigned to you", "args": "", "conversations": true }
    ]
  },
  "agent": {
    "roles": ["crew", "orchestrator", "conversation"],
    "guide": "markdown"
  },
  "syncs": [ { "name": "issues", "every": 900, "open": 60 } ],
  "refs": [
    { "kind": "issue", "title": "Jira issue", "icon": "ticket",
      "prefixes": ["RAM-", "RAP-"], "tail": "number",
      "address": "https://acme.atlassian.net/browse/{ref}", "path": "/issue/{ref}",
      "fields": [
        { "name": "title", "type": "title" },
        { "name": "status", "type": "state" },
        { "name": "assignee", "type": "text", "label": "Assignee" },
        { "name": "body", "type": "markdown" }
      ],
      "search": [ { "field": "title" }, { "field": "body", "max": 10000 } ],
      "views": {
        "card": [ { "block": "title", "field": "title" }, { "block": "subtitle", "field": "key" },
                  { "block": "badge", "field": "status" } ],
        "pane": [ { "block": "title", "field": "title" }, { "block": "subtitle", "field": "key" },
                  { "block": "badge", "field": "status" }, { "block": "field", "field": "assignee" },
                  { "block": "markdown", "field": "body" } ]
      },
      "actions": [ { "id": "move", "title": "Move to…" } ] }
  ],
  "actions": [ { "id": "new", "title": "New issue…", "icon": "plus", "hint": "In Jira" } ],
  "config_version": 1,
  "config": [
    { "key": "site", "title": "Jira site", "type": "url", "required": true,
      "help": "", "example": "https://acme.atlassian.net", "scope": "shared" },
    { "key": "token", "title": "API token", "type": "secret", "required": true },
    { "key": "projects", "title": "Projects to show", "type": "list", "default": [] },
    { "key": "kind", "title": "Kind", "type": "enum", "options": ["a", "b"], "default": "a" }
  ]
}
```

- `views[].path` is where in the plugin's app the entry opens. `icon` is one of ws's icon names; one it doesn't have is drawn as a default.
- `ui` is the plugin's app: what ws's page for the plugin loads ([The view](#the-view)). A plugin has one app, whatever views it has: each view is a place in it.
  - `ui.script` is the app's entry, loaded as a module. A plugin with `views` has one. The plugin serves it as JavaScript (`text/javascript`).
  - `ui.style` is optional: a stylesheet, loaded before the app. The plugin serves it as `text/css`.
  - Both are paths in the plugin's own HTTP. Each starts with `/` (not `//`) and has no `..`. A query is kept as it's given, so a plugin can name what it built (`?v=<a hash of the file>`) and a browser asks again when it's built again.
- `commands.name` is the word after `ws`. It defaults to the plugin's name.
- `commands.subcommands[].conversations` is a command a conversation's agent may run itself. A conversation's agent otherwise changes nothing and proposes ([Proposals](#proposals)): ws adds a command marked so to what the agent may run, and tells it so. Added within version 1: a ws from before it runs none of a plugin's commands in a conversation.
- `agent.roles` empty means every role.
- `config[].type` is one of `string`, `url`, `bool`, `number`, `enum`, `list`, `secret`, `map` and `table`. `scope` is `shared` (the default) or `local`; a secret is always local.
- A `map` is text by text (`columns = { "In Review" = "Review" }`). A `table` is named rows, each holding the key's `fields`: [Config in tables](#config-in-tables).
- `syncs` is for show (`ws plugin list`); ws doesn't run them. `every` is seconds between runs, `0` for a sync with no interval of its own. `open`, where it's there, is seconds between runs while the plugin's app is on screen ([`view/looking`](#viewlooking-notification)).
- `refs` are how the plugin's things are named: [Refs](#refs).
- `actions` are what the plugin can do when it's asked from Find, among what there is to do there: each an `id` (lower-case letters, digits and hyphens), a `title`, and an `icon` and a `hint` if it has them. Added within version 1, each may also say: `keys`, keys it suggests (named as ws names a press: `"n"`, `"g i"`), which ws binds only under the plugin key the person chose (`, n`), never by themselves; `opens`, a path in its app it goes to with nothing to run (ws goes there without asking the plugin); `where` it opens, `page` (unless said) or `beside` the page that's open; and `on`, the kinds of what's open it's for (`mission`, `pr`, `conversation`, `document`, `artifact`, `file`, `repo`, or a plugin's `jira:issue`), offered and done by its keys only there. ws runs one with [`action/run`](#actionrun-request). What can be done to one of its things is the thing's kind's (`refs[].actions`).
- `proposes` are what a conversation's agent may propose of the plugin's, for the user to accept: [Proposals](#proposals).
- `notifies` are the kinds of notification the plugin sends the person (`notification/add`), each `{ "id", "title", "about", "default" }`: `id` as an action's is written, `title` what Settings calls it, `about` a line on when it's sent, `default` where it's sent until the person says, `feed` (the default) or `off`. Each kind is listed in Settings › Notifications under the plugin's name, for the person to send to their feed, to the desktop as well, or nowhere. Added within version 1: a ws from before it lists none, and hasn't `notification/add`.

Not built yet, and ignored if sent: `events`, `actions`.

### Refs

A plugin's things have names people and agents write: `RAM-123`. A ref says how one kind of them is named, what one is made of, what it's found by and how it's drawn. All of it is data, not code: ws finds names, and draws things, in its own program and in its web UI, from the registration alone. A plugin may register several.

How one is named:

- `kind` is the plugin's word for what it is (`issue`): lower-case letters, digits and hyphens. ws asks about one by it ([`ref/show`](#refshow-request)).
- `title` is what one is called, for a person. `icon` is one of ws's icon names.
- `prefixes` are what a name starts with. Each is letters, digits and underscores, starting with a letter and ending in a hyphen: `RAM-`. A prefix is written as it's read: `ram-12` isn't `RAM-12`.
- `tail` is what follows the prefix. `number`, the default and the only one yet, is a whole number that doesn't start with 0, of up to nine digits. A tail ws doesn't know is from a newer protocol: ws reads none of that kind's names.
- `address` is where one is outside ws, an `http` or `https` address with `{ref}` once, where the name goes. ws links a name to it when the name goes into a document, and reads such an address as the thing it names, whatever follows a `?` or `#` (unless the address has one of its own).
- `path` is where one opens in the plugin's app, with `{ref}` once: a path as a view's is. Without it, a name goes to its `address`.

What one is made of:

- `fields` are a thing's fields: each a `name` (lower-case letters, digits and underscores), a `type`, and a `label` for where it's drawn with one. A thing, as the plugin gives one ([`thing/put`](#thingput-request), [`ref/show`](#refshow-request)), is a value for each, every value a string. What a kind has no field for is dropped.
  - `title` is what the thing's called: a line. A kind with fields has exactly one, and a thing with nothing for it isn't kept.
  - `state` is where it stands, in the plugin's words. At most one.
  - `text` is words: a name, a priority, a number.
  - `time` is a moment, RFC 3339.
  - `markdown` is what it says at length.
  - `ref` names another thing, and `refs` several, separated by spaces, commas or lines: another of the plugin's things, any plugin's, a pull request (`demo#12`). ws makes each a link in its graph, from this thing to that, and draws it as what it names. The field's `link` says how it points: `in` (it's part of it: a child, of its parent), `about`, `from`, `of`, or `mentions`, which is what none, or one ws doesn't know, is. What's linked is found from the other end: `ws show RAM-100` lists what's in it.
  - A type ws doesn't know is drawn as `text`.
- Every thing has the field `key` besides: its name. A kind can't have a field called that.
- A kind with no `fields` has names, and nothing ws keeps or draws.

What one is found by:

- `search` is the fields a thing is found by, in order, each as far as its `max` bytes (all ws keeps of it, with none). ws makes the words it looks through from these, and makes them again when they change. With no `search`, it's every field but the title. A thing is always found by its title and its name.

How one is drawn:

- `views` are how ws's web UI draws one, by where: `card`, under its name where it's written, and `pane`, beside the list in Find for the row the cursor's on. Each is blocks, in order, each showing one `field` (`key` among them). A block whose field a thing has nothing for isn't drawn.
  - `title` is the line it's known by, and `subtitle` a quiet line under it.
  - `badge` is a word or two set apart, on the line of the title or subtitle before it.
  - `field` is a label and a value: the block's `label`, or else the field's own.
  - `markdown` is drawn as ws draws markdown, to `lines` lines where that's set.
  - A block ws doesn't know isn't drawn, and a view for somewhere ws doesn't know is ignored.
- With no view for somewhere, ws draws one from the fields' types. `ws show`, in the terminal, always does.

What can be done to one:

- `actions` are a kind's actions, as the plugin's own are: Find offers them for the thing the cursor's on, and ws runs one with the kind and the thing's name ([`action/run`](#actionrun-request)). ws adds its own beside them: *Start a mission about it*.

Where one lives beyond this machine:

- `remote` says a kind's things can live in a repo, for everyone with the repo: [Remotes](#remotes). A kind with none has only what the plugin keeps.

A plugin's own app, for what's more than looking and a pick or two, is another thing: [The view](#the-view).

What ws does with them:

- **It finds a name where it's whole:** not inside a longer word, a path or an address (`RAM-12-fix`, `docs/RAM-12.md` and `XRAM-12` name nothing). It doesn't ask whether there's a thing by that name until one is shown.
- **It keeps them, by the plugin that registered them,** in the workspace's records, in place of what that plugin registered before. Names are found from there, whether the plugin's running or not, and removing the plugin forgets them.
- **A prefix is the first plugin's to register it.** A second plugin's ref names by its other prefixes, and `ws plugin list` says what was left out. So is a ref that breaks the rules above: the rest of the registration stands.
- **It keeps the things the plugin tells it of** ([Telling ws of things](#telling-ws-of-things)), so it finds them by what they say and draws them without asking.
- **It asks the plugin** for a thing it wasn't told of, and for `ws show RAM-123`, which wants it as it is now.

### Remotes

A kind's `remote` is where one of its things lives beyond this machine: a folder of a repo, its package. Added within version 1: a ws from before it leaves it out, and finds nothing.

```
sketches/release-flow/
  thing.toml
  release-flow.excalidraw
  release-flow.svg
```

```toml
# thing.toml
kind    = "sketch"
title   = "Release flow"
main    = "release-flow.excalidraw"
picture = "release-flow.svg"
```

- **The main file is the thing.** It's the plugin's own, in whatever form the plugin keeps one.
- **`thing.toml` says what the package is,** and is ws's own: its `kind` as the plugin registered it, its `title`, its `main` file's name and its `picture`'s, if it has one. A folder with no `thing.toml` isn't a thing, whatever's in it.
- **Anything else is made from the main file** by the plugin: a picture of it.

```json
{ "kind": "sketch", "prefixes": ["sketch-"],
  "fields": [ { "name": "title", "type": "title" }, { "name": "text", "type": "markdown" } ],
  "remote": { "path": "sketches/{slug}/", "main": "{slug}.excalidraw", "reads": true, "exports": true } }
```

- `path` is the folder a new one goes in, in its repo, ending in `/`, with `{slug}` in its last part, where the thing's slug goes. `main` is the main file's name there, with `{slug}` in it and a type after.
- `reads` is whether the plugin answers [`remote/read`](#remoteread-request).
- `exports` is whether the plugin answers [`remote/export`](#remoteexport-request): what ws promotes one of its things, and saves one, from. Without it, ws puts none of the kind's things in a repo.
- A kind with a remote has a title field: one in a repo is called what its `thing.toml` says.

What ws does with it:

- **It finds them.** A `thing.toml` in one of the workspace's spaces is a thing, wherever its folder is: `path` says only where a new one goes. It's read from the repo's default branch.
- **It names one by its folder,** as it names any folder: `records/sketches/release-flow/`. The plugin mints no name for it, and the kind's prefixes aren't used for it.
- **It keeps one as a thing of the kind,** called what its `thing.toml` says, with what the plugin says its main file is found by. So it's found, and drawn by the kind's views, as a thing the plugin told ws of is, and opens at the kind's `path` with its name where `{ref}` is, escaped as one part of a path.
- **It's ws's, not the plugin's.** [`thing/list`](#thinglist-request) doesn't list one, and [`thing/put`](#thingput-request) and [`thing/remove`](#thingremove-request) don't reach one. The plugin asks which there are with [`remote/list`](#remotelist-request), and for one's file with [`remote/open`](#remoteopen-request).
- **It puts one there, and saves it,** for the person: asked by the plugin ([`remote/promote`](#remotepromote-request), [`remote/save`](#remotesave-request)), or by ws's own UI. Either way ws asks the plugin for the thing as its folder is to hold it ([`remote/export`](#remoteexport-request)), does the rest itself (the name, the `thing.toml`, the commit, the landing), and tells the plugin where it is ([`remote/saved`](#remotesaved-request)).

### `config/changed` (notification)

```json
{ "config": { … } }
```

The whole config, as in `initialize`.

### Config in tables

A key of type `table` holds named rows, each with the same fields: the queries a board shows, the projects it knows. It's declared with its fields, which are keys themselves (a type, `required`, a `default`, `help`, `example`), and with `row`, what a row's name is called for whoever's asked for one:

```json
{ "key": "spaces", "title": "Spaces", "type": "table", "row": "Space key",
  "fields": [
    { "key": "label", "title": "Label", "type": "string" },
    { "key": "columns", "title": "Columns", "type": "map" },
    { "key": "custom_fields", "title": "Custom fields", "type": "table", "row": "Field",
      "fields": [ { "key": "card", "title": "On cards", "type": "bool" } ] }
  ] }
```

A field is of any type but `secret`. One may be a `table` itself, and its fields may not be: a table goes one level down. Both were added within version 1.

In ws.toml a row is a table under the key, named by the row:

```toml
[plugin.jira.spaces.ram]
label = "Risk Modelling"
columns = { "Ready for Development" = "To Do" }

[plugin.jira.spaces.ram.custom_fields.Team]
card = true
```

ws checks each row against the fields: a field that isn't declared, a value of the wrong type and a required field left out are each said, with the row and the field. A field that's wrong is left out, and a row without a field it needs isn't given to the plugin. It lays yours (ws.local.toml) over the team's row by row, then field by field, and leaves out a row that says `enabled = false`, which is how a team's row is taken away in your own file.

The table's value, in `config`, is its rows in the order they were written, the team's first:

```json
{ "spaces": [
    { "name": "ram", "values": {
        "label": "Risk Modelling",
        "columns": { "Ready for Development": "To Do" },
        "custom_fields": [ { "name": "Team", "values": { "card": true } } ] } }
] }
```

A ws from before tables doesn't know the type: it gives the value as the file has it, an object of objects by row name, with no order and nothing checked, and its commands can't set it. The framework reads both, the second with its rows by name.

### `command/run` (request)

Someone ran `ws jira mine --all -f llm`.

```json
{
  "id": "c7",
  "args": ["mine", "--all", "-f", "llm"],
  "dir": "/ws/repos/api/token-cache",
  "terminal": { "tty": true, "width": 120, "color": true },
  "input": "terminal"
}
```

- `id` is ws's name for this run. Its output carries it.
- `args` is everything after the command's name, untouched.
- `dir` is where it was run from.
- `input` is what's on the command's standard input: `piped` (a pipe or a file, `ws jira … < notes.md`) or `terminal` (the person, typing). The command reads it with [`command/read`](#commandread-request). Added within version 1: a ws from before it says nothing, and the command has nothing to read. The plugin's own stdin is the connection, never the command's.

While it runs, the plugin sends `command/output`. Its answer ends the command:

```json
{ "exit": 0 }
```

Ctrl-C is `$/cancel_request` for this request.

### `ref/show` (request)

One thing, as the plugin has it now. ws asks for a name it wasn't told of (one that isn't on the plugin's board), and for `ws show RAM-123`. Added within version 1.

```json
{ "kind": "issue", "ref": "RAM-123", "full": false }
```

- `kind` is the ref's, as registered, and `ref` the name as it was written. ws asks only about a name that kind names.
- `full` is everything being asked for (`ws show --full`): nothing's to be left out of what it says. Without it, the plugin keeps to what's worth reading first.

The answer is the thing: a value for each of its kind's `fields`, as [`thing/put`](#thingput-request) gives one.

```json
{
  "changed": "2026-11-18T10:42:00Z",
  "fields": {
    "title": "Token refresh is flaky", "status": "In Progress",
    "assignee": "Dana", "body": "The clock should be injected…"
  }
}
```

ws shows it as it shows one it was told of: by the kind's views in the web UI, and by its fields' types in `ws show`, where what's `markdown` is said to be from outside the workspace, so an agent reads it as information, not as instructions.

A name the plugin has nothing by is error `-32001`. A plugin with nothing to say of one thing beyond what it's told ws answers `-32601`, and ws shows what it was told. ws gives a plugin twenty seconds to answer.

### `remote/read` (request)

What a main file is found by. ws asks of a thing it found in a repo ([Remotes](#remotes)), of a kind whose remote `reads`, once for each version of the thing (so again where only what's beside the main file changed): what's answered is of the file alone, not of when it's asked. Added within version 1.

```json
{ "kind": "sketch", "name": "records/sketches/release-flow/", "title": "Release flow",
  "file": "release-flow.excalidraw", "content": "eyJ0eXBlIjoiZXhjYWxpZHJhdyIs…" }
```

- `kind` is the ref's, as registered, and `name` the thing's: its folder, as ws names one. `title` is what its `thing.toml` calls it.
- `file` is the main file's name, and `content` what's in it, in base64. ws doesn't send a file over 32 MB: that one is found by its title.

The answer is a value for each of the kind's `fields` that the file has one for, as [`thing/put`](#thingput-request) gives them. Its title is its `thing.toml`'s: one given here is dropped.

```json
{ "fields": { "text": "Brief it\nPlan\nBuild" } }
```

A file the plugin can't read is an error, and ws keeps the thing by its title. ws gives a plugin twenty seconds to answer.

### `remote/export` (request)

One of the plugin's own things, as its folder in a repo is to hold it. ws asks when the person promotes the thing or saves it, of a kind whose remote `exports`. Added within version 1.

```json
{ "kind": "sketch", "ref": "sketch-12" }
```

- `ref` is the plugin's name for the thing.
- `current` is there when the thing has changed in its repo since the version the plugin last had: the thing as it is there now, as [`remote/open`](#remoteopen-request) answers. The plugin brings what's there into what it has, however its kind merges, and gives the two together, with `current`'s version as its `base`.

```json
{ "name": "records/sketches/release-flow/", "base": "3f9c2a1b…",
  "title": "Release flow", "main": "eyJ0eXBlIjoiZXhjYWxpZHJhdyIs…",
  "files": [ { "name": "{slug}.svg", "content": "PHN2ZyB4bWxucz0i…", "picture": true } ] }
```

- `name` is the thing's name in its repo, for one that's in one already, and `base` the version of it there that this was made from: what the plugin was last told ([`remote/saved`](#remotesaved-request)), opened, or given as `current`. Neither is there for a thing that isn't in a repo yet.
- `title` is what it's called, and `main` its main file's content, in base64.
- `files` are what the plugin made from the main file, each a `name` in the folder (a name alone, with `{slug}` where the thing's slug goes) and its `content`. `picture` marks the one that's its picture.

ws writes the folder as it's given, with its `thing.toml`, in place of what was there. A name the plugin has nothing by is `-32001`. ws gives a plugin two minutes.

### `remote/saved` (request)

```json
{ "kind": "sketch", "ref": "sketch-12",
  "thing": { "kind": "sketch", "name": "records/sketches/release-flow/", "repo": "records", "path": "sketches/release-flow/",
             "title": "Release flow", "main": "release-flow.excalidraw", "picture": "release-flow.svg", "version": "9e8d7c6b…" } }
```

The thing the plugin last exported is in its repo: `thing` is what's there, as [`remote/list`](#remotelist-request) says one. Its `version` is what the plugin's next export of it is made from, and its `name` where the thing lives from now on. The answer is `{}`. Where its repo takes pull requests, the thing is in a capsule, not on the default branch, and the plugin is told all the same.

### `action/run` (request)

An action chosen in Find: one of the plugin's own, or one of a kind's on one of its things. Added within version 1: a plugin that registers `actions` answers it.

```json
{ "id": "move", "kind": "issue", "ref": "RAM-123", "answers": ["In Progress"] }
```

- `id` is the action's. `kind` and `ref` are there for a kind's action, the thing it's done to; neither for one of the plugin's own. ws runs only what's registered, and a kind's only by a name that's the kind's.
- `answers` are what's been answered so far to what the action asked, in order.
- `here`, added within version 1, is what was open when the action was asked for, if anything was: its `ref` (a mission's slug, a Jira issue's `RAM-12`), its `kind` (as `on` names kinds), its `title`, and the words picked out in it (`selection`), if any.

An action that needs something first asks for it, and does nothing yet:

```json
{ "ask": { "title": "Move RAM-123 to", "type": "pick",
           "options": [ { "value": "In Progress", "label": "In Progress", "hint": "2 others there" } ] } }
```

- `type` is `pick`, one of `options` (each a `value`, what's answered, a `label` and a `hint`), or `text`, a line typed, with a `placeholder`.
- Find asks it, and ws runs the action again with the answer added to `answers`. So an action is run from the start each time, and asks until it has all it wants: nothing's kept between runs.

Done, it says what came of it. Both are optional:

```json
{ "say": "RAM-123 is In Progress", "open": "/issue/RAM-123" }
```

- `say` is a line for whoever chose it.
- `open` is a path in the plugin's app, which ws goes to: as the page, or beside the page that's open with `"where": "beside"` (added within version 1). Where the app's open already, it's told to go there rather than opened again.

A plugin that changes one of its things tells ws of it as it now is ([`thing/put`](#thingput-request)). An action that fails is an error, whose message is said. ws gives a plugin two minutes.

### Proposals

A conversation's agent in ws changes nothing, but proposes, and the user accepts what it proposed, or not, with a card under its reply. A plugin adds to what it may propose (issues to create, a field to change) with `proposes` in its registration, each:

```json
{ "id": "issues", "title": "Jira issues", "icon": "ticket", "when": "Issues to create, once the conversation settles on work for the board." }
```

- `id` is lower-case letters, digits and hyphens; the agent names it as the plugin's name, a colon, and the id (`jira:issues`). `title` is what the card calls it. `when` is a line or two for the agent's first message: when to propose it.

What's proposed is in the plugin's own shape, which ws doesn't read: the plugin says how it's written, checks it, says what it'll do, and does it. ws keeps it, draws it and asks the user. The plugin is trusted to do what it said it would, and only that.

#### `proposal/draft` (request)

The agent proposing (`ws propose jira:issues`). Added within version 1: a plugin that registers `proposes` answers it.

```json
{ "id": "issues", "input": [ { "summary": "Cache tokens", "project": "RAM" } ], "conversation": "w3", "mission": "" }
```

- No `input`: the agent's asking how one's written, before it writes one. Answer `{ "guide": "…" }`: as it is now (the projects there are, the fields each asks for), with the JSON to write and an example.
- With `input`, answer `{ "refused": "…" }` for what's wrong with it, saying what to write instead (the agent reads it, and writes it again), or what accepting it will do:

```json
{ "proposal": { "title": "2 issues in RAM", "items": [
  { "title": "Cache tokens", "rows": [ { "label": "Project", "value": "RAM" } ], "markdown": "…", "payload": { … } },
  { "title": "Expire them", "on": "", "rows": [ { "label": "Parent", "value": "#1" } ], "payload": { … } } ] } }
```

- Each item is one thing done, accepted on its own or with the rest. `title` and `rows` (a `label` and a `value`, and `was` for a change to something that's there) and `markdown` are what the user's shown. `on` is the thing it changes, by name, for an item that changes one. `payload` is the plugin's own: ws keeps it and sends it back as it was.
- An item names an earlier one as `#1`, `#2`…: what that made is given when it's accepted.
- ws refuses a proposal of more than 50 items.

#### `proposal/accept` (request)

The user accepting items of a proposal, in order:

```json
{ "id": "issues", "key": "w3/4", "items": [ { "n": 2, "payload": { … } } ], "made": { "1": "RAM-130" }, "conversation": "w3" }
```

- `n` is an item's number in the proposal, from 1. `made` is what items accepted before made, by number.
- `key` is the proposal's, the same however often it's asked; an item's is `key/n`. ws asks again when it didn't hear an answer, and an item done already under its key isn't done again: answer as it was.

```json
{ "items": [ { "n": 2, "made": "RAM-131", "say": "Created RAM-131", "open": "/issue/RAM-131" } ] }
```

- `made` is the name of what it made or changed: ws links the conversation to it, and tells the agent. `say` is a line on it, `open` a path in the plugin's app.
- An item that wasn't done says why in `problem`: it stays on the card, to be accepted again.

A plugin built on the kit has all of this from `p.Proposes(id, title).When(…).Guide(…).Draft(…).Accept(…)`, which keeps what's done under its key.

### `view/looking` (notification)

```json
{ "app": true, "ws": true }
```

Who's looking at what the plugin shows in ws's web UI. ws says it once the plugin has answered `initialize`, if anyone is, and whenever it changes after. Until it's said, nobody is.

- `app`: the plugin's app is on screen, in a page of ws's that's showing (a tab in front, not one behind another).
- `ws`: a page of ws's web UI is showing, whichever it is, so the plugin's sidebar entries and their badges are on screen. It's true whenever `app` is.

ws asks nothing by it, and never asks a plugin to fetch: what a plugin fetches, and when, is the plugin's to decide. This is what it has to decide by. Someone who looks away for a moment (to another tab and back, from one page to the next) hasn't gone: ws says nobody's looking only once that's been so for a few seconds.

Added within version 1: a ws from before it never says, and a plugin from before it drops it.

### `shutdown` (request)

Finish and exit. The answer is `null`. ws closes stdin after it, and kills a plugin still running five seconds later.

## The plugin to ws

### `command/output` (notification)

```json
{ "id": "c7", "stream": "stdout", "data": "PROJ-1  Token cache\n" }
```

`id` is the `command/run`'s. `stream` is `stdout` or `stderr`.

Sent before the command's answer, it's delivered before the command ends.

### `log` (notification)

```json
{ "level": "error", "message": "sync issues: 401 from Jira" }
```

`level` is `debug`, `info`, `warn` or `error`. The last error shows in `ws plugin list`.

### `view/changed` (notification)

```json
{}
```

What the app shows is stale. ws tells the open app (`ws:changed`, in the frame protocol), which asks the plugin again.

### `view/badge` (notification)

```json
{ "path": "/", "count": 3 }
```

The count beside the view's sidebar entry (the view registered with that `path`). `0` removes it.

### `sync/state` (notification)

```json
{ "name": "issues", "at": "2026-11-17T10:42:00Z", "took_ms": 840, "error": "" }
```

A sync finished. For `ws plugin list`.

## Asking ws

Requests from the plugin, each answered by ws in these types: what ws knows, and, last, one thing ws does for the person. Each was added within version 1, so a ws from before it answers `-32601`: the framework's `ErrUnsupported`, which says to update ws. Something asked for by name that ws hasn't is `-32001` (`ErrNotFound`).

A plugin may ask once it has answered `initialize`.

### `command/read` (request)

A running command reading its standard input, which ws reads only when it's asked: a command that never reads it never waits on it. Added within version 1, and answered for a `command/run` that said its `input`.

```json
{ "id": "c7", "max": 65536 }
```

- `id` is the `command/run`'s.
- `max` is the most to give. ws gives at most 65536 (`protocol.ReadAtOnce`) at once, whatever's asked.

The answer is what was read, as base64, so it's whatever was piped, cut wherever:

```json
{ "data": "eyJvcHMiOiBbXX0K" }
```

At least one byte, or, once there's no more, `{ "eof": true }`. A command that's ended, or that has no input, is `-32001`. Output the command sent before it asks is shown before ws reads, so it can prompt.

### `mission/list` (request)

```json
{ "archived": false }
```

The workspace's missions: those in play, and with `archived`, the archived ones after them.

```json
{
  "missions": [
    {
      "slug": "token-cache",
      "title": "Token cache",
      "state": "review",
      "created": "2026-11-17T09:12:00Z",
      "updated": "2026-11-18T10:02:00Z",
      "headline": { "court": "you", "text": "PR #12 waits on your review" },
      "at": "Review",
      "capsules": [
        {
          "repo": "api", "name": "token-cache", "branch": "token-cache",
          "path": "/ws/repos/api/token-cache",
          "pull_request": { "number": 12, "title": "Token cache", "url": "https://github.com/acme/api/pull/12", "state": "open" }
        }
      ],
      "path": "/missions/token-cache",
      "about": ["RAM-123"]
    }
  ]
}
```

- `state` is `briefing`, `draft`, `active`, `review`, `landed`, `parked`, `answered`, `handed-on`, `dropped` or `archived`.
- `headline` is where it stands, and whose move it is: `court` is `agent` (crew or the orchestrator are on it), `you` (it waits on the person) or `outside` (on GitHub). An archived mission has none.
- `at` is where it is on its route, in words.
- A capsule's `path` is there while its folder is. `pull_request` is as ws last heard from GitHub, and `state` is `open`, `merged` or `closed`. `landed` is a capsule of a repo with no pull requests whose commits are on its default branch.
- `path` is the mission's page in ws's web UI: what a plugin's app opens with `ws:open`.
- `about` is what it's about, by name, as ws names things: one of a plugin's things, a pull request. A plugin finds the missions about one of its things by it. The mission's record says it (`ws mission new --about RAM-123`, `ws mission about`, or *Start a mission about it* on the thing in Find), not the plugin.

### `mission/show` (request)

```json
{ "slug": "token-cache" }
```

The mission, as in `mission/list`, with `brief` (markdown) and `plan` (markdown, absent when it has none). No such mission is `-32001`.

### `repo/list` (request)

No params. The workspace's repos, in ws's order:

```json
{
  "repos": [
    {
      "name": "api", "title": "API", "group": "", "aliases": ["a"],
      "cloned": true, "path": "/ws/repos/api",
      "capsules": [
        { "name": ".ground", "branch": "main", "path": "/ws/repos/api/.ground", "ground": true },
        { "name": "token-cache", "branch": "token-cache", "path": "/ws/repos/api/token-cache", "mission": "token-cache" }
      ]
    }
  ]
}
```

- `title` is what ws calls it: its name, when ws.toml gives it no other.
- A capsule's `mission` is the slug of the mission in play that works in it. `ground` is the repo's default branch, which work starts from.

### `show` (request)

```json
{ "ref": "RAM-123" }
```

One of the workspace's things as `ws show` shows it: what it is, what it links to, and what points at it from elsewhere in the workspace. It's how a plugin finds what's said of one of its things (the missions about it, the conversations that name it), and goes from there to any of them.

`ref` names it as `ws show` does: one of a plugin's things (`RAM-123`), a pull request (`api#12`), a document (`records:docs/plan.md`), one of the workspace's conversations, pages or artifacts (`w4`, `p2`, `a3`). One of a mission's own (`t3`, `d1`, `s2`, `c1`, `r1.c2`) is asked for with its mission. A mission itself is its slug, as `ref` or as `mission` with no `ref`. `full` is everything a view would otherwise cut short.

```json
{
  "ref": "RAM-123",
  "head": "Jira issue RAM-123: Token refresh is flaky (In Progress)",
  "body": ["Assignee: Sam", "At: https://example.atlassian.net/browse/RAM-123"],
  "links": [{ "ref": "RAM-100", "what": "Parent" }],
  "said": [
    {
      "ref": "token-cache", "kind": "mission", "title": "Token cache",
      "state": "active", "standing": true, "how": "about",
      "at": "2026-11-17T09:12:00Z", "path": "/missions/token-cache"
    },
    {
      "ref": "t3", "mission": "token-cache", "kind": "thread-ask", "title": "Which clock?",
      "state": "resolved", "standing": false, "how": "mentions",
      "at": "2026-11-18T10:02:00Z", "path": "/missions/token-cache/conversations/t3"
    }
  ],
  "path": "/x/jira/issue/RAM-123"
}
```

- `head` is what it is, in a line, and `body` the essentials of it, as `ws show` prints them: plain text, a line or a paragraph each. One of the plugin's own things is as the plugin last told ws.
- `links` are what it links to. Each is asked for in turn with its `ref`, and the `mission` this was asked with.
- `said` is what points at it, what still stands first, then latest first. `ref` is its ref where it's read: in `mission`, or under `pull_request` (`api#12`), or, with neither, the workspace's. A mission's own `ref` is its slug. Each is asked for in turn with its `ref` and `mission` as they are, and one under a pull request by the pull request.
- `kind` is what it is: `mission`, `brief`, `handoff`, `file`, `decision`, `comment`, `review`, `talk`, `thread-ask`, `thread-change`, `thread-note`, `thing`, `pull-request`, `page`, `artifact`, `document`, and whatever ws adds. `state` is where it stands in its kind's words, and `standing` whether that's still in play.
- `how` is how it points: `about`, `on`, `by`, `for`, `of`, `in`, `from`, `mentions`, `lands`, and whatever ws adds. `lines` is where in a file, as they were when it was said, and `at` when it was said, absent where ws hasn't when.
- `path` is its page in ws's web UI, where it has one: what a plugin's app opens with `ws:open`.
- ws says what it has linked: a mission that's about it, and whatever names it where words are kept (a conversation's turn, a mission's brief, a handoff, a decision, a document).

Nothing by that name is `-32001`.

### `conversation/start` (request)

```json
{ "message": "Let's work out a mission for RAM-123." }
```

Starts one of the workspace's conversations for the person using the plugin, with `message` as what they say first, and has its agent answer. It's how a plugin's app hands something on to ws to be talked over: the agent reads what the message names (`RAM-123`) with `ws show`, and proposes what the conversation comes to (a mission, a decision), for the person to accept there.

```json
{ "id": "w4", "path": "/conversations/w4" }
```

- `id` is the conversation, as `show` names it, and `path` its page in ws's web UI: what the plugin's app opens with `ws:open`.
- What the message names as ws names things is linked: `show` of the thing says the conversation mentions it.
- The message is sent as the person, and an agent answers it, so this is for something they asked for (a button they pressed), not for the plugin's own account. ws records which plugin started the conversation, and says so on it.

An empty `message` is `-32602`.

### `notification/add` (request)

```json
{ "kind": "assigned", "text": "Ana gave you RAM-123: Fix the login page", "refs": ["RAM-123"], "key": "h:10234:assignee:RAM-123" }
```

Tells the person of something that happened, in their feed of notifications: the one ws's own are in (a mission ended, a pull request merged). `kind` is one of the `notifies` the plugin registered; `text` says what happened, as a sentence; `refs` is what it's about as ws names things, the first what opening it goes to. `key` is the plugin's own name for what happened: one sent again with a key the plugin sent before isn't kept twice, so a plugin that sees the same change on two syncs needn't remember it told of it. `attention` marks one that went wrong; `at` is when it happened, if not now.

```json
{ "n": 12, "added": true }
```

- `n` is its number in the feed (`n12`): with `added` false, it was there already, and `n` is that one's.
- `muted` is a kind the person sends nowhere: it's kept all the same, so it's there if they change their mind, but not shown.
- Whether it reaches the desktop too is the person's to say, kind by kind. What the person did themselves isn't news to them: a plugin tells of what others did, and of what it did on its own account.

A kind the plugin didn't register, or an empty `text`, is `-32602`. Added within version 1: a ws from before it hasn't the method (`-32601`).

### `remote/list` (request)

```json
{ "kind": "sketch" }
```

The things of that kind that live in the workspace's repos ([Remotes](#remotes)), as ws found them, in order of name. It's how a plugin lists them beside its own: ws has them, so the plugin needn't keep them.

```json
{ "things": [
  { "kind": "sketch", "name": "records/sketches/release-flow/",
    "repo": "records", "path": "sketches/release-flow/", "title": "Release flow",
    "main": "release-flow.excalidraw", "picture": "release-flow.svg",
    "version": "3f9c2a1b…", "changed": "2026-11-20T10:42:00Z", "changed_by": "Dana" },
  { "kind": "sketch", "name": "records/sketches/cross-stack/architecture/",
    "repo": "records", "repo_label": "Records", "path": "sketches/cross-stack/architecture/", "group": "cross-stack",
    "title": "Architecture", "main": "architecture.excalidraw", "picture": "architecture.svg",
    "version": "7a1e04c2…", "changed": "2026-11-20T11:05:00Z", "changed_by": "Sam" }
] }
```

- `name` is its name in ws, and what it opens by in the plugin's app. `repo` and `path` are where it is: `path` is its folder, ending in `/`.
- `repo_label` is what ws calls the repo, and `group` where the thing is among its kind's things there: the folders between the place the workspace puts the kind's things (`records:sketches/`) and the thing's own folder, with `/` between them (`cross-stack`, `plans/q4`). There's none for one straight in its place. They're how a plugin lists its things as they're organised in their repos, by repo and then by group: a plugin doesn't know where the workspace puts its kind's things, so it can't work a group out from `path`. For a thing in none of those places, `group` is the folders it's in from the repo's top.
- `main` and `picture` are files in its folder, by name.
- `version` is what its folder holds, as ws names it: the same while every file in it is, another when any of them changes (its main file, what's made from it, its `thing.toml`). It's ws's to make, and nothing to read: a plugin keeps it, to give back as an export's `base`, and can keep what it makes from a thing by it.
- `changed` and `changed_by` are when it last changed in its repo and who by, where ws knows.

### `remote/open` (request)

```json
{ "kind": "sketch", "name": "records/sketches/release-flow/" }
```

One of them, and its main file as it is on its repo's default branch: what `remote/list` says of it, with the file's `content` in base64. Nothing of that kind by that name is `-32001`.

### `remote/promote` (request)

```json
{ "kind": "sketch", "ref": "sketch-12" }
```

Puts one of the plugin's own things in a repo, for the person who asked the plugin to (a button they pressed): it's theirs to do, not the plugin's own account. ws asks the plugin for the thing ([`remote/export`](#remoteexport-request)), names its folder, writes it, commits it as the person, lands it as its repo lands, and tells the plugin where it is ([`remote/saved`](#remotesaved-request)).

- `ref` is the plugin's name for the thing. ws keeps that the name is this thing's now.
- `to` is where the person said it's to live, as `repo:folder/`, where they did: its files are named after that folder. With none, it's in the workspace's records repo, in the folder the kind's `remote.path` names for the title's slug (with `-2` and so on where that's taken).

```json
{ "thing": { "kind": "sketch", "name": "records/sketches/release-flow/", "repo": "records", "path": "sketches/release-flow/",
             "title": "Release flow", "main": "release-flow.excalidraw", "picture": "release-flow.svg", "version": "3f9c2a1b…" },
  "changed": true, "said": "Promoted to records:sketches/release-flow/: landed on main (ce64e15)" }
```

- `thing` is the thing as it is in its repo now.
- `capsule` is there where the repo takes pull requests: the thing was committed in a capsule (`records/promote-release-flow`) and left for the person to open a pull request from. It's on no default branch yet, so ws finds it once that's merged.
- `said` is what was done, in a sentence, for the person.

A kind that isn't the plugin's, or whose remote doesn't export, a thing that's in a repo already, a package ws won't take (no title, a file's name that isn't a name alone), and somewhere there's something already are `-32602`. A thing the plugin has nothing by is `-32001`.

### `remote/save` (request)

```json
{ "kind": "sketch", "ref": "sketch-12" }
```

Saves one of the plugin's own things that's in a repo as the person has it now, in one commit, landed as its repo lands. Saving is the person's to ask for: a plugin keeps what's changed since as its own, on this machine, until they do.

ws asks the plugin for the thing, which says where it lives and the version what it gives was made from. Where the thing has changed in its repo since that version, ws asks again with what's there, for the plugin to bring the two together. The folder is then written as it's given, and it and its files keep their names, whatever the title is now.

The answer is as [`remote/promote`](#remotepromote-request)'s:

- `changed` is whether saving changed anything: it's false when the folder was as it was given already.
- `merged` is whether the thing had changed in its repo since the plugin last had it, and the plugin brought the two together.

A thing the plugin says isn't in a repo is `-32602`, and nothing of that kind where it says is `-32001`.

## Telling ws of things

Requests from the plugin, each answered, each added within version 1. A plugin tells ws of the things it has, so ws has them without asking: they're found by what their kind says they're found by (`ws find`, Find in the web UI) and drawn by their kind's views, with the plugin's other things and ws's own, whether the plugin's running or not.

A thing is named by one of the plugin's [refs](#refs): its `kind` is the ref's, and its `ref` one of that kind's names, as ws took them. ws keeps no thing it couldn't name. It keeps each under the plugin that told it, and forgets them when the plugin's removed, or when the plugin registers refs that name them no more.

### `thing/put` (request)

```json
{
  "things": [
    { "kind": "issue", "ref": "RAM-123", "changed": "2026-11-18T10:42:00Z",
      "fields": {
        "title": "Token refresh is flaky", "status": "In Progress",
        "assignee": "Dana", "body": "The clock should be injected…"
      } }
  ]
}
```

Each is kept, in place of what ws had by that name. One that's as ws has it already isn't written again, so telling ws of everything after each sync costs little.

- `fields` are its values, by its kind's [fields](#refs)' names. A thing with nothing for its kind's `title` isn't kept.
- `changed` is when it last changed, RFC 3339, where the plugin knows.
- ws keeps the start of a long value: 300 bytes of a `title`, 60 of a `state`, 2,000 of `text`, 50,000 of `markdown`. It keeps what it's told as it's told, and makes what it's found by from that.
- Up to 500 things in one request. More is refused whole (`-32602`): send them in turn.

The answer says which weren't kept, and why. The rest were.

```json
{ "refused": [ { "kind": "issue", "ref": "ram-12", "why": "it isn't one of issue's names, as ws keeps them" } ] }
```

A thing is refused when its kind isn't a ref ws took from this plugin, its name isn't one of that kind's, it has nothing for its kind's title, or ws has something of its own by that name.

### `thing/remove` (request)

```json
{ "kind": "issue", "refs": ["RAM-123", "RAM-124"] }
```

ws forgets them. One it doesn't have is nothing to forget. The answer is `{}`.

### `thing/list` (request)

```json
{ "kind": "issue" }
```

The names of the things of that kind ws has from this plugin, in order:

```json
{ "refs": ["RAM-123", "RAM-124"] }
```

It's how a plugin tells ws of all it has: put them, and remove what's listed and not among them. The framework's `Mirror` does.

## The view

A plugin's web UI is one client-side app. ws shows it at `/x/<name>/…` in a page of its own, the shell, which loads the app the registration's `ui` names, from the plugin's HTTP. A plugin serves no HTML: no page, and nothing at a view's `path`, where a 404 is what's expected. The shell, what the app is given, and the messages between the app and ws are the frame protocol, which has its own versions: [`ui-lib`'s `PROTOCOL.md`](https://github.com/withworkspace/ui-lib/blob/main/PROTOCOL.md). [`@withworkspace/ui-lib`](https://www.npmjs.com/package/@withworkspace/ui-lib) speaks it for a page.

What's left here is the plugin's own HTTP: its app's files, and whatever its app asks it. ws proxies `GET|POST|… /plugins/<name>/<rest>` to the plugin's socket as `/<rest>`, with:

- `X-Forwarded-Prefix: /plugins/<name>`
- ws's own cookies removed

`ui.script` and `ui.style` are loaded through it: `/ui/app.js` is asked for as `/plugins/<name>/ui/app.js`.

Before the frame protocol, a plugin served its own pages here, with ws's `/plugin-kit.js` and `/plugin-kit.css` in them. Both files are gone, and so is the table of the frame's messages that was in this section. A plugin with `views` and no `ui.script` is one from then: ws says it has no app to show.
