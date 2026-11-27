package plugin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/withworkspace/plugin"
	"github.com/withworkspace/plugin/plugintest"
	"github.com/withworkspace/plugin/protocol"
	"github.com/withworkspace/plugin/ui"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// The app as built, at its smallest: its entry and its stylesheet.
const (
	appJS  = `console.log("the app")`
	appCSS = `main { color: var(--ws-fg) }`
)

// hashed is a file of the app's as the registration should name it.
func hashed(name, content string) string {
	sum := sha256.Sum256([]byte(content))
	return "/ui/" + name + "?v=" + hex.EncodeToString(sum[:4])
}

// --- registration ---

func TestRegistrationIsWhatWasDeclared(t *testing.T) {
	files := fstest.MapFS{
		"agents.md":       file("Use `ws jira mine`.\n"),
		"ui/dist/app.js":  file(appJS),
		"ui/dist/app.css": file(appCSS),
	}
	ws := plugintest.New(t, files, func(p *plugin.Plugin) {
		p.Describe("Jira issues beside your missions")
		p.Config.Version(2)
		p.Config.URL("site", "Jira site", plugin.Required, plugin.Example("https://acme.atlassian.net"))
		p.Config.Secret("token", "API token", plugin.Required, plugin.Help("From id.atlassian.com"))
		p.Config.List("projects", "Projects to show", plugin.Default([]string{}))
		p.Config.Enum("kind", "Kind", []string{"a", "b"}, plugin.Default("a"))
		p.Config.String("email", "Your email", plugin.Local)
		p.UI("ui/dist")
		p.View("Jira", plugin.Icon("ticket"), plugin.Under("tasks"))
		p.View("Board", plugin.At("/board"))
		p.Command("mine", "Issues assigned to you", func(*plugin.Call) error { return nil }).Args("[PROJECT]")
		p.Sync("issues", 5*time.Minute, func(context.Context) error { return nil })
		p.Ref("issue", "Jira issue").
			Prefix("RAM-", "RAP-").Address("https://acme.atlassian.net/browse/{ref}").Opens("/issue/{ref}").Icon("ticket").
			Fields(plugin.Title("title"), plugin.State("status", "Status"), plugin.Text("assignee", "Assignee"), plugin.Time("updated"), plugin.Markdown("body")).
			Search(plugin.All("title"), plugin.All(ui.Key), plugin.Upto("body", 10000)).
			Card(ui.Title("title"), ui.Subtitle(ui.Key), ui.Badge("status")).
			Pane(ui.Title("title"), ui.Badge("status"), ui.Field("assignee").Label("Who"), ui.Markdown("body").Lines(12))
		p.Ref("sprint", "").Prefix("SPRINT-")
		p.Guide("", "crew", "orchestrator")
	}, plugintest.Name("jira"), plugintest.Config(map[string]any{"site": "https://x", "token": "t"}))

	got, _ := json.Marshal(ws.Registration())
	want := `{"description":"Jira issues beside your missions",` +
		`"views":[{"title":"Jira","icon":"ticket","under":"tasks","path":"/"},{"title":"Board","path":"/board"}],` +
		`"ui":{"script":"` + hashed("app.js", appJS) + `","style":"` + hashed("app.css", appCSS) + `"},` +
		`"commands":{"name":"jira","short":"Jira issues beside your missions","subcommands":[` +
		`{"name":"mine","short":"Issues assigned to you","args":"[PROJECT]"},` +
		`{"name":"sync","short":"Fetch what's new now","args":"[NAME...]"}]},` +
		`"agent":{"roles":["crew","orchestrator"],"guide":"Use ` + "`ws jira mine`" + `.\n"},` +
		`"syncs":[{"name":"issues","every":300}],` +
		`"refs":[{"kind":"issue","title":"Jira issue","icon":"ticket","prefixes":["RAM-","RAP-"],` +
		`"address":"https://acme.atlassian.net/browse/{ref}","path":"/issue/{ref}",` +
		`"fields":[{"name":"title","type":"title"},{"name":"status","type":"state","label":"Status"},` +
		`{"name":"assignee","type":"text","label":"Assignee"},{"name":"updated","type":"time"},{"name":"body","type":"markdown"}],` +
		`"search":[{"field":"title"},{"field":"key"},{"field":"body","max":10000}],` +
		`"views":{"card":[{"block":"title","field":"title"},{"block":"subtitle","field":"key"},{"block":"badge","field":"status"}],` +
		`"pane":[{"block":"title","field":"title"},{"block":"badge","field":"status"},` +
		`{"block":"field","field":"assignee","label":"Who"},{"block":"markdown","field":"body","lines":12}]}},` +
		`{"kind":"sprint","prefixes":["SPRINT-"]}],` +
		`"config_version":2,` +
		`"config":[` +
		`{"key":"site","title":"Jira site","type":"url","required":true,"example":"https://acme.atlassian.net"},` +
		`{"key":"token","title":"API token","type":"secret","required":true,"help":"From id.atlassian.com","scope":"local"},` +
		`{"key":"projects","title":"Projects to show","type":"list","default":[]},` +
		`{"key":"kind","title":"Kind","type":"enum","default":"a","options":["a","b"]},` +
		`{"key":"email","title":"Your email","type":"string","scope":"local"}]}`
	if string(got) != want {
		t.Errorf("registration\n got %s\nwant %s", got, want)
	}
}

func TestAPluginThatDeclaresWronglyDoesNotStart(t *testing.T) {
	built := fstest.MapFS{"ui/dist/app.js": file(appJS)}
	ok := func(http.ResponseWriter, *http.Request) {}
	for name, tc := range map[string]struct {
		files fstest.MapFS
		setup func(p *plugin.Plugin)
		why   string // what the plugin's author is told
	}{
		"a view and no app": {
			setup: func(p *plugin.Plugin) { p.View("X") },
			why:   `the view "X" has no app to show`,
		},
		"an app that isn't built": {
			files: fstest.MapFS{"ui/dist/app.css": file(appCSS)},
			setup: func(p *plugin.Plugin) { p.UI("ui/dist"); p.View("X") },
			why:   "ui/dist/app.js, isn't in the plugin's files",
		},
		"an app and no files": {
			setup: func(p *plugin.Plugin) { p.UI("ui/dist") },
			why:   "ui/dist/app.js, isn't in the plugin's files",
		},
		"two apps": {
			files: built,
			setup: func(p *plugin.Plugin) { p.UI("ui/dist"); p.UI("ui/dist") },
			why:   "a plugin has one app",
		},
		"a view at a path that isn't one": {
			files: built,
			setup: func(p *plugin.Plugin) { p.UI("ui/dist"); p.View("X", plugin.At("board")) },
			why:   `the view "X" opens at "board"`,
		},
		"two handlers on one path": {
			setup: func(p *plugin.Plugin) { p.HandleFunc("GET /api/a", ok); p.HandleFunc("GET /api/a", ok) },
			why:   "the plugin's HTTP",
		},
		"a command declared twice": {
			setup: func(p *plugin.Plugin) {
				p.Command("a", "", func(*plugin.Call) error { return nil })
				p.Command("a", "", func(*plugin.Call) error { return nil })
			},
			why: "declared twice",
		},
		"a ref declared twice": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-")
				p.Ref("issue", "").Prefix("RAP-")
			},
			why: "declared twice",
		},
		"a ref that names nothing": {
			setup: func(p *plugin.Plugin) { p.Ref("issue", "") },
			why:   "it has no prefixes",
		},
		"a ref whose prefix is ws's own kind of name": {
			setup: func(p *plugin.Plugin) { p.Ref("issue", "").Prefix("demo#") },
			why:   `its prefix "demo#"`,
		},
		"a ref whose address has nowhere for the name": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Address("https://acme.atlassian.net/browse/")
			},
			why: "its address",
		},
		"a ref that opens at a path that isn't one": {
			setup: func(p *plugin.Plugin) { p.Ref("issue", "").Prefix("RAM-").Opens("issue/{ref}") },
			why:   "its path",
		},
		"a ref whose things have nothing they're called": {
			setup: func(p *plugin.Plugin) { p.Ref("issue", "").Prefix("RAM-").Fields(plugin.Text("summary")) },
			why:   "0 fields of type title",
		},
		"a ref with two titles": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Fields(plugin.Title("summary"), plugin.Title("name"))
			},
			why: "2 fields of type title",
		},
		"a ref with a field twice": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Fields(plugin.Title("title"), plugin.Text("title"))
			},
			why: `its field "title" is there twice`,
		},
		"a ref that takes the key for a field": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Fields(plugin.Title("title"), plugin.Text(ui.Key))
			},
			why: `its field "key" is every thing's own`,
		},
		"a ref found by a field it hasn't": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Fields(plugin.Title("title")).Search(plugin.All("body"))
			},
			why: `it's found by "body"`,
		},
		"a ref whose view shows a field it hasn't": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Fields(plugin.Title("title")).Card(ui.Title("title"), ui.Badge("status"))
			},
			why: `its view card shows "status"`,
		},
		"an action declared twice": {
			setup: func(p *plugin.Plugin) {
				p.Action("sync", "Sync", func(*plugin.Act) error { return nil })
				p.Action("sync", "Sync again", func(*plugin.Act) error { return nil })
			},
			why: `its action "sync" is there twice`,
		},
		"an action with nothing that does it": {
			setup: func(p *plugin.Plugin) { p.Action("sync", "Sync", nil) },
			why:   "has nothing that does it",
		},
		"an action on a thing with no title": {
			setup: func(p *plugin.Plugin) {
				p.Ref("issue", "").Prefix("RAM-").Action("move", " ", func(*plugin.Act) error { return nil })
			},
			why: `its action "move" has no title`,
		},
		"a config key declared twice": {
			setup: func(p *plugin.Plugin) {
				p.Config.String("a", "A")
				p.Config.Bool("a", "A")
			},
			why: "declared twice",
		},
	} {
		files := tc.files
		if files == nil {
			files = fstest.MapFS{}
		}
		ws, err := plugintest.Start(files, tc.setup)
		if err == nil {
			ws.Close()
			t.Errorf("a plugin with %s started", name)
		} else if !strings.Contains(err.Error(), tc.why) {
			t.Errorf("a plugin with %s didn't start, and didn't say why (%q): %v", name, tc.why, err)
		}
	}
}

// --- config ---

func TestConfigIsReadThroughItsKeys(t *testing.T) {
	var (
		site     *plugin.StringKey
		projects *plugin.ListKey
		closed   *plugin.BoolKey
		limit    *plugin.NumberKey
		changes  atomic.Int32
	)
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		site = p.Config.URL("site", "Site")
		projects = p.Config.List("projects", "Projects", plugin.Default([]string{"ALL"}))
		closed = p.Config.Bool("closed", "Show closed", plugin.Default(true))
		limit = p.Config.Number("limit", "Limit", plugin.Default(50))
		p.OnConfig(func() { changes.Add(1) })
	}, plugintest.Config(map[string]any{"site": "https://one"}))

	if site.Get() != "https://one" || !site.IsSet() {
		t.Errorf("site is %q", site.Get())
	}
	if got := projects.Get(); len(got) != 1 || got[0] != "ALL" || projects.IsSet() {
		t.Errorf("projects, not set, is %v", got)
	}
	if !closed.Get() || limit.Get() != 50 {
		t.Errorf("defaults: closed %v, limit %v", closed.Get(), limit.Get())
	}

	if err := ws.SetConfig(map[string]any{"site": "https://two", "projects": []string{"A", "B"}, "closed": false, "limit": 5}); err != nil {
		t.Fatal(err)
	}
	if site.Get() != "https://two" || len(projects.Get()) != 2 || closed.Get() || limit.Get() != 5 {
		t.Errorf("after a change: %q %v %v %v", site.Get(), projects.Get(), closed.Get(), limit.Get())
	}
	if changes.Load() != 1 {
		t.Errorf("OnConfig ran %d times", changes.Load())
	}
	select {
	case <-ws.Done():
		t.Error("the plugin stopped on a change to config no Setup read")
	default:
	}
}

// A table is declared with its rows' fields, registered with them, and
// read a row at a time through what declared each: in the order ws gives
// the rows, each field with its default, a table within a row in turn.
func TestConfigTablesAreReadARowAtATime(t *testing.T) {
	var (
		queries, spaces, fields *plugin.TableKey
		label, jql, name, field *plugin.StringField
		pinned, card            *plugin.BoolField
		limit                   *plugin.NumberField
		tags                    *plugin.ListField
		columns                 *plugin.MapField
		kind                    *plugin.StringField
		statuses                *plugin.MapKey
	)
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		queries = p.Config.Table("queries", "Queries", plugin.RowName("Query"), plugin.Required)
		label = queries.String("label", "Label")
		jql = queries.String("query", "JQL", plugin.Required, plugin.Help("What finds its issues"))
		pinned = queries.Bool("pinned", "Pinned", plugin.Default(true))
		limit = queries.Number("limit", "Limit", plugin.Default(50))
		tags = queries.List("tags", "Tags")
		kind = queries.Enum("kind", "Kind", []string{"board", "list"}, plugin.Default("board"))
		spaces = p.Config.Table("spaces", "Spaces", plugin.RowName("Space key"))
		name = spaces.String("label", "Label")
		columns = spaces.Map("columns", "Columns")
		fields = spaces.Table("custom_fields", "Custom fields", plugin.RowName("Field"))
		field = fields.String("field", "Its id")
		card = fields.Bool("card", "On cards")
		statuses = p.Config.Map("statuses", "Statuses", plugin.Default(map[string]string{"Done": "Closed"}))
	}, plugintest.Config(map[string]any{
		"queries": []protocol.ConfigRow{
			plugintest.Row("initiatives", map[string]any{"label": "Initiatives", "query": "project = RAP", "pinned": false, "tags": []string{"a", "b"}}),
			plugintest.Row("bugs", map[string]any{"query": "project = RAM, type = Bug", "limit": 5, "kind": "list"}),
		},
		"spaces": []protocol.ConfigRow{
			plugintest.Row("ram", map[string]any{"label": "Risk Modelling", "columns": map[string]string{"Ready for Development": "To Do"},
				"custom_fields": []protocol.ConfigRow{
					plugintest.Row("Team", map[string]any{"card": true}),
					plugintest.Row("Story Points", map[string]any{"field": "customfield_10016"}),
				}}),
			plugintest.Row("rap", map[string]any{"label": "Risk Platform"}),
		},
	}))

	// What ws is told: each table with its rows' fields, and what a row's
	// name is called.
	reg := ws.Registration().Config
	if len(reg) != 3 || reg[0].Type != protocol.TypeTable || reg[0].Row != "Query" || !reg[0].Required || len(reg[0].Fields) != 6 {
		t.Fatalf("registered: %+v", reg)
	}
	if f := reg[0].Fields[1]; f.Key != "query" || f.Type != protocol.TypeString || !f.Required || f.Help != "What finds its issues" {
		t.Errorf("a field: %+v", f)
	}
	if f := reg[0].Fields[5]; f.Type != protocol.TypeEnum || len(f.Options) != 2 || f.Default != "board" {
		t.Errorf("an enum field: %+v", f)
	}
	if f := reg[1].Fields[2]; f.Key != "custom_fields" || f.Type != protocol.TypeTable || f.Row != "Field" || len(f.Fields) != 2 || f.Fields[1].Type != protocol.TypeBool {
		t.Errorf("a table in a table: %+v", f)
	}
	if reg[1].Fields[1].Type != protocol.TypeMap || reg[2].Type != protocol.TypeMap {
		t.Errorf("maps: %+v, %+v", reg[1].Fields[1], reg[2])
	}

	// Rows in the order ws gave them, each field what's set or its
	// default.
	rows := queries.Rows()
	if len(rows) != 2 || rows[0].Name != "initiatives" || rows[1].Name != "bugs" || !queries.IsSet() {
		t.Fatalf("queries: %+v", rows)
	}
	if label.In(rows[0]) != "Initiatives" || jql.In(rows[0]) != "project = RAP" || pinned.In(rows[0]) || limit.In(rows[0]) != 50 || len(tags.In(rows[0])) != 2 || kind.In(rows[0]) != "board" {
		t.Errorf("the first query: %q %q %v %v %v %q", label.In(rows[0]), jql.In(rows[0]), pinned.In(rows[0]), limit.In(rows[0]), tags.In(rows[0]), kind.In(rows[0]))
	}
	if label.In(rows[1]) != "" || label.IsSetIn(rows[1]) || jql.In(rows[1]) != "project = RAM, type = Bug" || !pinned.In(rows[1]) || limit.In(rows[1]) != 5 || tags.In(rows[1]) != nil || kind.In(rows[1]) != "list" {
		t.Errorf("the second query: %q %q %v %v %v %q", label.In(rows[1]), jql.In(rows[1]), pinned.In(rows[1]), limit.In(rows[1]), tags.In(rows[1]), kind.In(rows[1]))
	}

	// A map, and rows within a row.
	ram := spaces.Rows()[0]
	if name.In(ram) != "Risk Modelling" || columns.In(ram)["Ready for Development"] != "To Do" {
		t.Errorf("a space: %q %v", name.In(ram), columns.In(ram))
	}
	inner := fields.RowsIn(ram)
	if len(inner) != 2 || inner[0].Name != "Team" || !card.In(inner[0]) || field.In(inner[0]) != "" || inner[1].Name != "Story Points" || field.In(inner[1]) != "customfield_10016" || card.In(inner[1]) {
		t.Errorf("a space's custom fields: %+v", inner)
	}
	if got := fields.RowsIn(spaces.Rows()[1]); len(got) != 0 || columns.In(spaces.Rows()[1]) != nil {
		t.Errorf("a space with none: %+v", got)
	}
	if fields.Rows() != nil {
		t.Error("a table that's a row's field has rows of its own")
	}
	if got := statuses.Get(); got["Done"] != "Closed" || statuses.IsSet() {
		t.Errorf("a map, not set: %v", got)
	}

	// A field read in another table's row is a mistake: it's as not set,
	// and the plugin's log says so.
	if label.In(ram) != "" || card.In(rows[0]) || len(fields.RowsIn(rows[0])) != 0 {
		t.Error("a field read in another table's row has a value")
	}
	said := false
	for _, l := range ws.Logs() {
		said = said || strings.Contains(l.Message, "label is a field of queries, and was read in a row of spaces")
	}
	if !said {
		t.Errorf("nothing logged of a field read in the wrong row: %+v", ws.Logs())
	}

	// From a ws that passes the file through as it is, a table is an
	// object by row name: its rows are then by name. And a change is
	// read at once.
	if err := ws.SetConfig(map[string]any{
		"queries":  map[string]any{"zed": map[string]any{"query": "z"}, "alpha": map[string]any{"query": "a", "label": "Alpha"}},
		"spaces":   map[string]any{"ram": map[string]any{"custom_fields": map[string]any{"Team": map[string]any{"card": true}}}},
		"statuses": map[string]string{"Open": "To Do"},
	}); err != nil {
		t.Fatal(err)
	}
	rows = queries.Rows()
	if len(rows) != 2 || rows[0].Name != "alpha" || label.In(rows[0]) != "Alpha" || rows[1].Name != "zed" || jql.In(rows[1]) != "z" {
		t.Errorf("queries as the file has them: %+v", rows)
	}
	if inner := fields.RowsIn(spaces.Rows()[0]); len(inner) != 1 || inner[0].Name != "Team" || !card.In(inner[0]) {
		t.Errorf("a table in a table, as the file has it: %+v", inner)
	}
	if got := statuses.Get(); got["Open"] != "To Do" || len(got) != 1 {
		t.Errorf("a map, set: %v", got)
	}
	if err := ws.SetConfig(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if queries.Rows() != nil || queries.IsSet() {
		t.Errorf("queries with none set: %+v", queries.Rows())
	}
}

// A table's fields are declared once each, and a table goes one level
// down: a plugin that says otherwise doesn't start.
func TestConfigTablesAreDeclaredProperly(t *testing.T) {
	if ws, err := plugintest.Start(nil, func(p *plugin.Plugin) {
		q := p.Config.Table("queries", "Queries")
		q.String("label", "Label")
		q.Bool("label", "Label again")
	}); err == nil || !strings.Contains(err.Error(), `config table queries: field "label" is declared twice`) {
		if ws != nil {
			ws.Close()
		}
		t.Errorf("a field declared twice: %v", err)
	}
	if ws, err := plugintest.Start(nil, func(p *plugin.Plugin) {
		p.Config.Table("spaces", "Spaces").Table("custom_fields", "Custom fields").Table("options", "Options")
	}); err == nil || !strings.Contains(err.Error(), `config table spaces.custom_fields: "options" is a table in a table in a table`) {
		if ws != nil {
			ws.Close()
		}
		t.Errorf("a table three deep: %v", err)
	}
}

// A table a Setup read restarts the plugin when its rows change, as a
// key does.
func TestAChangeToATableSetupReadRestarts(t *testing.T) {
	rows := func(names ...string) []protocol.ConfigRow {
		var out []protocol.ConfigRow
		for _, n := range names {
			out = append(out, plugintest.Row(n, map[string]any{"query": "project = " + n}))
		}
		return out
	}
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		queries := p.Config.Table("queries", "Queries")
		queries.String("query", "JQL")
		p.Setup(func() {
			for _, row := range queries.Rows() {
				p.Command(row.Name, "Issues of "+row.Name, func(*plugin.Call) error { return nil })
			}
		})
	}, plugintest.Config(map[string]any{"queries": rows("bugs", "tasks")}))

	if cmds := ws.Registration().Commands; cmds == nil || len(cmds.Subcommands) != 2 || cmds.Subcommands[1].Name != "tasks" {
		t.Fatalf("commands declared from a table's rows: %+v", cmds)
	}
	ws.SetConfig(map[string]any{"queries": rows("bugs", "tasks")}) //nolint:errcheck // values a test wrote
	select {
	case <-ws.Done():
		t.Fatal("the plugin stopped though the table hadn't changed")
	case <-time.After(50 * time.Millisecond):
	}
	ws.SetConfig(map[string]any{"queries": rows("tasks", "bugs")}) //nolint:errcheck // values a test wrote
	select {
	case <-ws.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the plugin went on though the table's rows changed places")
	}
}

// A plugin whose registration follows its config exits when that config
// changes, to be started afresh.
func TestAChangeToWhatSetupReadRestarts(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		projects := p.Config.List("projects", "Projects")
		p.Config.String("other", "Other")
		p.Setup(func() {
			for _, name := range projects.Get() {
				p.Command(strings.ToLower(name), "Issues of "+name, func(*plugin.Call) error { return nil })
			}
		})
	}, plugintest.Config(map[string]any{"projects": []string{"PROJ", "OPS"}}))

	if cmds := ws.Registration().Commands; cmds == nil || len(cmds.Subcommands) != 2 || cmds.Subcommands[1].Name != "ops" {
		t.Fatalf("commands declared in Setup: %+v", cmds)
	}
	ws.SetConfig(map[string]any{"projects": []string{"PROJ", "OPS"}, "other": "x"}) //nolint:errcheck // values a test wrote
	select {
	case <-ws.Done():
		t.Fatal("the plugin stopped though what Setup read hadn't changed")
	case <-time.After(50 * time.Millisecond):
	}
	ws.SetConfig(map[string]any{"projects": []string{"PROJ"}}) //nolint:errcheck // values a test wrote
	select {
	case <-ws.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the plugin carried on though what Setup read had changed")
	}
}

// --- commands ---

type issue struct {
	Key     string `json:"key" ws:"Issue"`
	Summary string `json:"summary"`
}

func (i issue) Markdown() string { return "## " + i.Key + "\n\n" + i.Summary }

func commands(p *plugin.Plugin) {
	issues := []issue{{"PROJ-1", "Token cache"}, {"PROJ-2", "Billing is slow"}}
	p.Describe("Issues")
	p.Command("mine", "Issues assigned to you", func(c *plugin.Call) error {
		if c.Flag("none") {
			return c.List([]issue{})
		}
		if project := c.Option("project"); project != "" {
			return c.Say("only " + project + " " + strings.Join(c.Args(), ","))
		}
		return c.List(issues)
	}).Flag("none", "Show nothing").Option("project", "Only this project").Args("[QUERY]")
	p.Command("show", "One issue", func(c *plugin.Call) error {
		if c.Arg(0) == "" {
			return plugin.Exit(3, errors.New("which issue?"))
		}
		return c.Show(issues[0])
	}).Args("KEY")
	p.Command("fail", "Fails", func(*plugin.Call) error { return errors.New("it broke") })
	p.Command("panic", "Panics", func(*plugin.Call) error { panic("oh no") })
	p.Command("wait", "Waits to be stopped", func(c *plugin.Call) error {
		<-c.Context().Done()
		return c.Context().Err()
	})
}

func TestCommands(t *testing.T) {
	ws := plugintest.New(t, nil, commands, plugintest.Name("jira"))

	for _, tc := range []struct {
		args   []string
		stdout string
		stderr string
		exit   int
	}{
		{args: []string{"mine"}, stdout: "ISSUE   SUMMARY\nPROJ-1  Token cache\nPROJ-2  Billing is slow\n"},
		{args: []string{"mine", "-f", "llm"}, stdout: "| Issue | Summary |\n| --- | --- |\n| PROJ-1 | Token cache |\n| PROJ-2 | Billing is slow |\n"},
		{args: []string{"mine", "--format=json"}, stdout: "[\n  {\n    \"key\": \"PROJ-1\",\n    \"summary\": \"Token cache\"\n  },\n  {\n    \"key\": \"PROJ-2\",\n    \"summary\": \"Billing is slow\"\n  }\n]\n"},
		{args: []string{"mine", "--none"}, stdout: "Nothing to show.\n"},
		{args: []string{"mine", "--none", "-f", "json"}, stdout: "[]\n"},
		{args: []string{"mine", "--project", "OPS", "a", "--", "-b"}, stdout: "only OPS a,-b\n"},
		{args: []string{"mine", "--project=OPS"}, stdout: "only OPS \n"},
		{args: []string{"show", "PROJ-1"}, stdout: "Issue:    PROJ-1\nSummary:  Token cache\n"},
		{args: []string{"show", "PROJ-1", "-f", "llm"}, stdout: "## PROJ-1\n\nToken cache\n"},
		{args: []string{"show"}, stderr: "which issue?\n", exit: 3},
		{args: []string{"fail"}, stderr: "it broke\n", exit: 1},
		{args: []string{"panic"}, stderr: "panic panicked: oh no\n", exit: 1},
	} {
		got := ws.Run(tc.args...)
		if got.Stdout != tc.stdout || got.Stderr != tc.stderr || got.Exit != tc.exit {
			t.Errorf("ws jira %s\n got %+v\nwant stdout %q stderr %q exit %d", strings.Join(tc.args, " "), got, tc.stdout, tc.stderr, tc.exit)
		}
	}
}

func TestCommandsHelpAndMistakes(t *testing.T) {
	ws := plugintest.New(t, nil, commands, plugintest.Name("jira"))

	help := ws.Run()
	if help.Exit != 0 || !strings.Contains(help.Stdout, "ws jira <command> [flags]") || !strings.Contains(help.Stdout, "mine   Issues assigned to you") {
		t.Errorf("no arguments: %+v", help)
	}
	if got := ws.Run("--help"); got.Stdout != help.Stdout {
		t.Errorf("--help differs from no arguments: %+v", got)
	}
	one := ws.Run("mine", "--help")
	for _, want := range []string{"ws jira mine [QUERY] [flags]", "--none", "Show nothing", "--project <value>", "-f, --format <format>"} {
		if !strings.Contains(one.Stdout, want) {
			t.Errorf("mine --help lacks %q:\n%s", want, one.Stdout)
		}
	}
	for _, args := range [][]string{{"nope"}, {"mine", "--nope"}, {"mine", "-f", "yaml"}, {"mine", "--project"}, {"mine", "--none=1"}} {
		if got := ws.Run(args...); got.Exit != 2 || got.Stdout != "" || got.Stderr == "" {
			t.Errorf("ws jira %s: %+v", strings.Join(args, " "), got)
		}
	}
}

func TestATableIsCutToTheTerminal(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("long", "", func(c *plugin.Call) error {
			return c.List([]issue{{"PROJ-1", strings.Repeat("word ", 40)}})
		})
	})
	ws.Terminal = protocol.Terminal{TTY: true, Width: 30}
	for _, line := range strings.Split(strings.TrimSpace(ws.Run("long").Stdout), "\n") {
		if n := len([]rune(line)); n > 30 {
			t.Errorf("a line %d wide in a terminal of 30: %q", n, line)
		}
	}
}

func TestACommandReadsWhatsPipedToIt(t *testing.T) {
	// More than one read's worth, cut through a character, so it comes in
	// pieces and has to go back together byte for byte.
	piped := strings.Repeat("é", protocol.ReadAtOnce) + "\x00\xff end"
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("count", "", func(c *plugin.Call) error {
			b, err := io.ReadAll(c.Stdin())
			if err != nil {
				return err
			}
			return c.Say(fmt.Sprintf("piped %v, %d bytes, same %v", c.Piped(), len(b), string(b) == piped))
		})
	})
	if got := ws.Pipe(piped, "count"); got.Exit != 0 || got.Stdout != fmt.Sprintf("piped true, %d bytes, same true\n", len(piped)) {
		t.Errorf("a command read what was piped: %+v", got)
	}
	// Run pipes nothing, as a ws from before input: nothing to read.
	if got := ws.Run("count"); got.Exit != 0 || got.Stdout != "piped false, 0 bytes, same false\n" {
		t.Errorf("a command with nothing piped: %+v", got)
	}
}

func TestACommandThatDoesntReadItsInputIsntHeldUp(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("hi", "", func(c *plugin.Call) error { return c.Say("hi") })
	})
	if got := ws.Pipe("never read", "hi"); got.Exit != 0 || got.Stdout != "hi\n" {
		t.Errorf("a command that didn't read its input: %+v", got)
	}
}

func TestACommandMarkedForConversationsIsRegisteredSo(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("show", "", func(*plugin.Call) error { return nil }).Conversations()
		p.Command("post", "", func(*plugin.Call) error { return nil })
	})
	cmds := ws.Registration().Commands
	if cmds == nil || len(cmds.Subcommands) != 2 || !cmds.Subcommands[0].Conversations || cmds.Subcommands[1].Conversations {
		t.Errorf("commands: %+v", cmds)
	}
}

func TestACommandsLabelAndOptionsAreRegistered(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("show", "", func(*plugin.Call) error { return nil }).Label("Pulling up {1}").Option("rev", "").Flag("all", "")
		p.Command("post", "", func(*plugin.Call) error { return nil })
	})
	cmds := ws.Registration().Commands
	if cmds == nil || len(cmds.Subcommands) != 2 {
		t.Fatalf("commands: %+v", cmds)
	}
	show, post := cmds.Subcommands[0], cmds.Subcommands[1]
	if show.Label != "Pulling up {1}" || len(show.Options) != 1 || show.Options[0] != "rev" {
		t.Errorf("show: %+v", show)
	}
	if post.Label != "" || post.Options != nil {
		t.Errorf("post: %+v", post)
	}
}

func TestACommandOutOfTimeSaysSo(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.CommandTimeout = 20 * time.Millisecond
		commands(p)
	})
	got := ws.Run("wait")
	if got.Exit != 1 || !strings.Contains(got.Stderr, "ran out of time") {
		t.Errorf("a command past its time: %+v", got)
	}
}

// --- the view ---

func viewFiles() fstest.MapFS {
	return fstest.MapFS{
		"ui/dist/app.js":        file(appJS),
		"ui/dist/app.css":       file(appCSS),
		"ui/dist/chunks/one.js": file(`export const one = 1`),
		"ui/dist/logo.svg":      file(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"ui/src/main.ts":        file(`the app's source: not served`),
		"agents.md":             file("not served either"),
	}
}

func view(saved *[]string) func(p *plugin.Plugin) {
	return func(p *plugin.Plugin) {
		p.UI("ui/dist")
		p.View("Jira", plugin.Icon("ticket"))
		p.View("Board", plugin.At("/board"))
		p.HandleFunc("GET /api/ping", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pong")) })
		p.HandleFunc("GET /api/issues/{key}", func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("key") == "BAD" {
				panic("no such issue")
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"key": r.PathValue("key")}) //nolint:errcheck // a test's recorder
		})
		p.Handle("POST /api/save", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*saved = append(*saved, r.FormValue("name"))
			w.WriteHeader(http.StatusNoContent)
		}))
	}
}

// A plugin's HTTP is its app's files and what its app asks it, and
// nothing else: it serves no page.
func TestTheAppsFilesAndItsAPI(t *testing.T) {
	var saved []string
	ws := plugintest.New(t, viewFiles(), view(&saved), plugintest.Name("jira"))

	ui := ws.Registration().UI
	if ui == nil || ui.Script != hashed("app.js", appJS) || ui.Style != hashed("app.css", appCSS) {
		t.Fatalf("the app as registered: %+v", ui)
	}

	// The app's files, as what they are, where the registration says.
	for _, tc := range []struct{ path, kind, cache, body string }{
		{ui.Script, "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", appJS},
		{ui.Style, "text/css; charset=utf-8", "public, max-age=31536000, immutable", appCSS},
		{"/ui/app.js", "text/javascript; charset=utf-8", "no-cache", appJS},
		{"/ui/chunks/one.js", "text/javascript; charset=utf-8", "no-cache", `export const one = 1`},
		{"/ui/logo.svg", "image/svg+xml", "no-cache", `<svg xmlns="http://www.w3.org/2000/svg"/>`},
	} {
		got := ws.Get(tc.path)
		if got.Status != 200 || got.Header.Get("Content-Type") != tc.kind || got.Header.Get("Cache-Control") != tc.cache || got.Body != tc.body {
			t.Errorf("GET %s: %d, %q, %q\n%s", tc.path, got.Status, got.Header.Get("Content-Type"), got.Header.Get("Cache-Control"), got.Body)
		}
	}

	// Nothing else is there, and nothing is HTML: not a view's path (ws
	// serves the page), not a folder of the app's, not the plugin's other
	// files.
	for _, path := range []string{"/", "/board", "/browse/PROJ-1", "/ui/", "/ui", "/ui/chunks/", "/ui/nope.js", "/ui/../agents.md", "/ui/../src/main.ts", "/agents.md", "/static/app.js"} {
		got := ws.Get(path)
		if got.Status != 404 || strings.Contains(got.Header.Get("Content-Type"), "html") || strings.Contains(got.Body, "<") {
			t.Errorf("GET %s: %d, %q\n%s", path, got.Status, got.Header.Get("Content-Type"), got.Body)
		}
	}

	// What the app asks the plugin.
	if ping := ws.Get("/api/ping"); ping.Status != 200 || ping.Body != "pong" {
		t.Errorf("a handler: %d %q", ping.Status, ping.Body)
	}
	if one := ws.Get("/api/issues/PROJ-1"); one.Status != 200 || strings.TrimSpace(one.Body) != `{"key":"PROJ-1"}` {
		t.Errorf("a handler with a path's value: %d %q", one.Status, one.Body)
	}
	if post := ws.Post("/api/save", map[string][]string{"name": {"x"}}); post.Status != 204 || len(saved) != 1 || saved[0] != "x" {
		t.Errorf("a POST: %d, saved %v", post.Status, saved)
	}
	if wrong := ws.Post("/api/ping", nil); wrong.Status != 405 {
		t.Errorf("a POST to a GET's handler: %d", wrong.Status)
	}

	// A handler that panics answers 500 in plain text, and ws is told.
	bad := ws.Get("/api/issues/BAD")
	if bad.Status != 500 || !strings.Contains(bad.Body, "no such issue") || !strings.HasPrefix(bad.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("a handler that panicked: %d, %q\n%s", bad.Status, bad.Header.Get("Content-Type"), bad.Body)
	}
	if logs := ws.Logs(); len(logs) != 1 || logs[0].Level != "error" || !strings.Contains(logs[0].Message, "GET /api/issues/{key} panicked: no such issue") {
		t.Errorf("ws wasn't told of the panic: %+v", logs)
	}
}

// An app needs no stylesheet, and a plugin needs no app: one with only
// handlers serves them, and one with nothing serves nothing.
func TestAnAppWithNoStylesheetAndAPluginWithNoApp(t *testing.T) {
	bare := plugintest.New(t, fstest.MapFS{"web/app.js": file(appJS)}, func(p *plugin.Plugin) {
		p.UI("web/")
		p.View("X")
	})
	if ui := bare.Registration().UI; ui == nil || ui.Script != hashed("app.js", appJS) || ui.Style != "" {
		t.Errorf("an app with no stylesheet: %+v", ui)
	}
	if got := bare.Get("/ui/app.css"); got.Status != 404 {
		t.Errorf("a stylesheet that isn't there: %d", got.Status)
	}

	api := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.HandleFunc("POST /hooks/push", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	})
	if reg := api.Registration(); reg.UI != nil || len(reg.Views) != 0 {
		t.Errorf("a plugin with no app registered one: %+v", reg)
	}
	if got := api.Post("/hooks/push", nil); got.Status != 202 {
		t.Errorf("a handler of a plugin with no app: %d", got.Status)
	}

	nothing := plugintest.New(t, nil, func(*plugin.Plugin) {})
	if got := nothing.Get("/"); got.Status != 404 {
		t.Errorf("a plugin that serves nothing: %d", got.Status)
	}
}

func TestChangedAndBadgeTellWS(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("poke", "", func(*plugin.Call) error {
			p.Changed()
			p.Badge("/", 3)
			return nil
		})
	})
	ws.Run("poke")
	if n := len(ws.Notifications(protocol.MethodViewChanged)); n != 1 {
		t.Errorf("%d view/changed", n)
	}
	if got := ws.Notifications(protocol.MethodViewBadge); len(got) != 1 || string(got[0]) != `{"path":"/","count":3}` {
		t.Errorf("view/badge: %s", got)
	}
}

// --- the database ---

func visits(p *plugin.Plugin) {
	p.Command("visit", "", func(c *plugin.Call) error {
		if _, err := p.DB().Exec(`insert into visits (at, who) values (1, 'me')`); err != nil {
			return err
		}
		var n int
		if err := p.DB().QueryRow(`select count(*) from visits`).Scan(&n); err != nil {
			return err
		}
		return c.Say(strings.Repeat("x", n))
	})
	p.UI("ui/dist")
	p.View("V")
	p.HandleFunc("GET /api/visits", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if err := p.DB().QueryRowContext(r.Context(), `select count(*) from visits`).Scan(&n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]int{"visits": n}) //nolint:errcheck // a test's recorder
	})
}

func TestMigrationsAreAppliedInOrderEachOnce(t *testing.T) {
	files := fstest.MapFS{
		"ui/dist/app.js":             file(appJS),
		"migrations/0001_visits.sql": file("create table visits (at integer not null);"),
		"migrations/0002_who.sql":    file("alter table visits add column who text;\ncreate index visits_who on visits (who);"),
		"migrations/notes.txt":       file("not a migration"),
	}
	ws := plugintest.New(t, files, visits)
	if got := ws.Run("visit"); got.Stdout != "x\n" {
		t.Fatalf("after both migrations: %+v", got)
	}
	if got := ws.Run("visit"); got.Stdout != "xx\n" {
		t.Fatalf("a second visit: %+v", got)
	}
	if got := ws.Get("/api/visits"); got.Status != 200 || strings.TrimSpace(got.Body) != `{"visits":2}` {
		t.Errorf("the app's API, reading the database: %d %q", got.Status, got.Body)
	}
	if _, err := os.Stat(filepath.Join(ws.Data(), "plugin.db")); err != nil {
		t.Errorf("the database isn't in the data folder: %v", err)
	}
}

func TestAFailedMigrationStopsThePluginRunning(t *testing.T) {
	files := fstest.MapFS{
		"ui/dist/app.js":             file(appJS),
		"migrations/0001_visits.sql": file("create table visits (at integer not null);"),
		"migrations/0002_bad.sql":    file("create table other (x integer);\nalter table nowhere add column y;"),
	}
	ws := plugintest.New(t, files, visits)

	// It still registered: ws can show what it is, and what's wrong.
	if len(ws.Registration().Views) != 1 {
		t.Errorf("a plugin with a bad migration didn't register its view")
	}
	if logs := ws.Logs(); len(logs) != 1 || logs[0].Level != "error" || !strings.Contains(logs[0].Message, "0002_bad.sql") {
		t.Errorf("ws wasn't told which migration failed: %+v", logs)
	}
	if got := ws.Run("visit"); got.Exit != 1 || !strings.Contains(got.Stderr, "0002_bad.sql") {
		t.Errorf("a command ran against a half-migrated database: %+v", got)
	}
	// The app still loads, and what it asks is answered with why not, in
	// plain text.
	if app := ws.Get(ws.Registration().UI.Script); app.Status != 200 {
		t.Errorf("the app of a plugin with a bad migration: %d", app.Status)
	}
	if got := ws.Get("/api/visits"); got.Status != 503 || !strings.Contains(got.Body, "0002_bad.sql") || !strings.HasPrefix(got.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("a handler ran against a half-migrated database: %d, %q\n%s", got.Status, got.Header.Get("Content-Type"), got.Body)
	}
}

// A database with something in it is copied aside before it's changed.
func TestTheDatabaseIsCopiedAsideBeforeAMigration(t *testing.T) {
	dir := t.TempDir()
	first := fstest.MapFS{
		"ui/dist/app.js":             file(appJS),
		"migrations/0001_visits.sql": file("create table visits (at integer not null, who text);"),
	}
	ws := plugintest.New(t, first, visits, plugintest.Dir(dir))
	ws.Run("visit")
	ws.Close()
	if _, err := os.Stat(filepath.Join(dir, "data", "plugin.db.bak")); err == nil {
		t.Fatal("a new database was copied aside: there was nothing to keep")
	}

	second := fstest.MapFS{
		"ui/dist/app.js":             file(appJS),
		"migrations/0001_visits.sql": first["migrations/0001_visits.sql"],
		"migrations/0002_index.sql":  file("create index visits_who on visits (who);"),
	}
	ws = plugintest.New(t, second, visits, plugintest.Dir(dir))
	if got := ws.Run("visit"); got.Stdout != "xx\n" {
		t.Errorf("what was kept didn't survive a migration: %+v", got)
	}
	if info, err := os.Stat(filepath.Join(dir, "data", "plugin.db.bak")); err != nil || info.Size() == 0 {
		t.Errorf("the database wasn't copied aside before the migration: %v", err)
	}
}

// --- sync ---

func TestSyncRunsAtStartAndOnDemandOneAtATime(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Sync("issues", time.Hour, func(context.Context) error {
			if runs.Add(1) > 1 {
				<-release
			}
			return nil
		})
	})

	// At start, unasked.
	states := ws.Wait(protocol.MethodSyncState, 1)
	if len(states) != 1 || runs.Load() != 1 {
		t.Fatalf("at start: %d runs, %s", runs.Load(), states)
	}
	var st protocol.SyncStateParams
	json.Unmarshal(states[0], &st) //nolint:errcheck // the plugin's own JSON
	if st.Name != "issues" || st.Error != "" || st.At == "" {
		t.Errorf("sync/state: %+v", st)
	}
	if n := len(ws.Wait(protocol.MethodViewChanged, 1)); n != 1 {
		t.Errorf("the open app wasn't told after a sync: %d view/changed", n)
	}
	if states := ws.Plugin().SyncStates(); len(states) != 1 || states[0].Name != "issues" || states[0].At.IsZero() || states[0].Error != "" {
		t.Errorf("how the sync stands, for a plugin's app to show: %+v", states)
	}

	// Asked for three times at once: one run, three answers.
	var wg sync.WaitGroup
	results := make([]plugintest.Result, 3)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = ws.Run("sync") }()
	}
	for runs.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the other two have asked by now
	close(release)
	wg.Wait()
	if runs.Load() != 2 {
		t.Errorf("three requests at once ran the sync %d times, not once", runs.Load()-1)
	}
	for _, r := range results {
		if r.Exit != 0 || !strings.Contains(r.Stdout, "issues") {
			t.Errorf("ws x sync: %+v", r)
		}
	}

	// Asked for by the plugin's own code: a handler its app posts to, say.
	if err := ws.Plugin().SyncNow(context.Background()); err != nil || runs.Load() != 3 {
		t.Errorf("SyncNow: %v, %d runs", err, runs.Load())
	}
}

func TestAFailedSyncIsRetriedSoonerAndSaysSo(t *testing.T) {
	defer plugin.SetRetryAfter(10 * time.Millisecond)()
	var runs atomic.Int32
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Sync("issues", time.Hour, func(context.Context) error {
			if runs.Add(1) < 3 {
				return errors.New("401 from Jira")
			}
			return nil
		})
	})
	states := ws.Wait(protocol.MethodSyncState, 3)
	if len(states) != 3 {
		t.Fatalf("a failing sync was run %d times in five seconds, with an hour between syncs", len(states))
	}
	var first, third protocol.SyncStateParams
	json.Unmarshal(states[0], &first) //nolint:errcheck // the plugin's own JSON
	json.Unmarshal(states[2], &third) //nolint:errcheck // the plugin's own JSON
	if first.Error != "401 from Jira" || third.Error != "" {
		t.Errorf("first %+v, third %+v", first, third)
	}
	if logs := ws.Logs(); len(logs) != 2 || logs[0].Level != "warn" || !strings.Contains(logs[0].Message, "401 from Jira") {
		t.Errorf("ws wasn't told of the failures: %+v", logs)
	}
	if got := ws.Run("sync", "nope"); got.Exit != 2 {
		t.Errorf("a sync that doesn't exist: %+v", got)
	}
}

func TestSyncWaitsForRequiredConfig(t *testing.T) {
	var runs atomic.Int32
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Config.Secret("token", "Token", plugin.Required)
		p.Sync("issues", time.Hour, func(context.Context) error { runs.Add(1); return nil })
	})
	time.Sleep(50 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatalf("a sync ran with required config missing")
	}
	if got := ws.Run("sync"); got.Exit != 1 || !strings.Contains(got.Stderr, "config isn't complete") {
		t.Errorf("ws x sync with config missing: %+v", got)
	}
	ws.SetConfig(map[string]any{"token": "t"}) //nolint:errcheck // values a test wrote
	if n := len(ws.Wait(protocol.MethodSyncState, 1)); n != 1 || runs.Load() != 1 {
		t.Errorf("once config was set: %d runs", runs.Load())
	}
}

// settled is how many times a sync has run once it's stopped running:
// the count, after a moment in which it didn't move.
func settled(runs *atomic.Int32) int32 {
	for {
		was := runs.Load()
		time.Sleep(60 * time.Millisecond)
		if runs.Load() == was {
			return was
		}
	}
}

func TestASyncKeepsTheAppsPaceWhileItsOnScreen(t *testing.T) {
	var runs atomic.Int32
	var told []plugin.Looking
	var mu sync.Mutex
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Sync("issues", time.Hour, func(context.Context) error { runs.Add(1); return nil }, plugin.WhileOpen(10*time.Millisecond))
		p.OnLooking(func(l plugin.Looking) { mu.Lock(); told = append(told, l); mu.Unlock() })
	})
	if got := ws.Registration().Syncs; len(got) != 1 || got[0].Every != 3600 || got[0].Open != 0 {
		// (An interval under a second is 0 in the registration, which is
		// for show: nothing real syncs that often.)
		t.Errorf("the sync, as registered: %+v", got)
	}

	// Nobody's looking: the hour is its interval.
	ws.Wait(protocol.MethodSyncState, 1)
	if n := settled(&runs); n != 1 {
		t.Fatalf("with nobody looking, and an hour between syncs, it ran %d times", n)
	}
	if l := ws.Plugin().Looking(); l.App || l.WS {
		t.Errorf("who's looking, before ws said: %+v", l)
	}

	// ws's web UI is showing, on another page: nothing of this sync's.
	ws.Look(plugin.Looking{WS: true}) //nolint:errcheck // a pipe
	if n := settled(&runs); n != 1 {
		t.Errorf("with ws showing and the app not, it ran %d times", n)
	}

	// The app comes on screen: at once, as the last run's further back
	// than the app's interval, and at that interval after.
	ws.Look(plugin.Looking{App: true}) //nolint:errcheck // a pipe
	if n := len(ws.Wait(protocol.MethodSyncState, 4)); n < 4 {
		t.Fatalf("with the app on screen, and 10ms between syncs, it ran %d times in five seconds", n)
	}
	if l := ws.Plugin().Looking(); !l.App || !l.WS {
		t.Errorf("who's looking, with the app on screen: %+v", l)
	}

	// And it's gone: the hour again.
	ws.Look(plugin.Looking{}) //nolint:errcheck // a pipe
	n := settled(&runs)
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != n {
		t.Errorf("with the app gone, it went on running: %d, then %d", n, runs.Load())
	}

	// The plugin's own code heard each change, once.
	ws.Look(plugin.Looking{}) //nolint:errcheck // a pipe
	mu.Lock()
	defer mu.Unlock()
	want := []plugin.Looking{{WS: true}, {App: true, WS: true}, {}}
	if fmt.Sprint(told) != fmt.Sprint(want) {
		t.Errorf("OnLooking was told %+v, not %+v", told, want)
	}
}

func TestASyncWithNoIntervalRunsOnlyWhileTheAppsOnScreen(t *testing.T) {
	var runs atomic.Int32
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Sync("issues", 0, func(context.Context) error { runs.Add(1); return nil }, plugin.WhileOpen(time.Hour))
		p.Sync("board", 0, func(context.Context) error { return nil }, plugin.WhileOpen(10*time.Millisecond))
	})
	if got := ws.Registration().Syncs; len(got) != 2 || got[0].Every != 0 || got[0].Open != 3600 {
		t.Errorf("the syncs, as registered: %+v", got)
	}
	// At start, as every sync does, and no more.
	ws.Wait(protocol.MethodSyncState, 2)
	if n := settled(&runs); n != 1 {
		t.Fatalf("a sync with no interval ran %d times with nobody looking", n)
	}
	// The app comes on screen within its interval of the last run: not
	// yet. The other, whose interval has passed, runs.
	ws.Look(plugin.Looking{App: true}) //nolint:errcheck // a pipe
	if n := len(ws.Wait(protocol.MethodSyncState, 4)); n < 4 {
		t.Fatalf("the sync that was due didn't run when the app came on screen: %d sync/state", n)
	}
	if runs.Load() != 1 {
		t.Errorf("a sync ran when the app came on screen, an hour before it was due: %d runs", runs.Load())
	}
	// Asked for, it runs whoever's looking.
	ws.Look(plugin.Looking{}) //nolint:errcheck // a pipe
	if got := ws.Run("sync", "issues"); got.Exit != 0 || runs.Load() != 2 {
		t.Errorf("ws x sync issues: %+v, %d runs", got, runs.Load())
	}
}

func TestASyncSleptThroughRunsWhenTheClockSaysItsDue(t *testing.T) {
	var runs atomic.Int32
	var ahead atomic.Int64 // how far the wall clock is ahead of the timers
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		plugin.SetClock(p, func() time.Time { return time.Now().Round(0).Add(time.Duration(ahead.Load())) }, 10*time.Millisecond)
		p.Sync("issues", time.Hour, func(context.Context) error { runs.Add(1); return nil })
	})
	ws.Wait(protocol.MethodSyncState, 1)
	if n := settled(&runs); n != 1 {
		t.Fatalf("with an hour between syncs, it ran %d times", n)
	}
	// The machine sleeps for two hours: the clock moves, and no timer does.
	ahead.Store(int64(2 * time.Hour))
	if n := len(ws.Wait(protocol.MethodSyncState, 2)); n != 2 {
		t.Fatalf("a sync two hours old, with an hour between syncs, wasn't run: %d runs", runs.Load())
	}
	// And the clock's put back to before that run: its next is an hour
	// from now, not three.
	ahead.Store(0)
	if n := settled(&runs); n != 2 {
		t.Errorf("after the clock was put back, it ran %d times", n)
	}
	ahead.Store(int64(time.Hour + 10*time.Minute)) // past the hour, and the tenth of it syncs are spread by
	if n := len(ws.Wait(protocol.MethodSyncState, 3)); n != 3 {
		t.Errorf("a clock put back made the wait longer: %d runs", runs.Load())
	}
}

func TestASyncsIntervalCantBeBelowNothing(t *testing.T) {
	_, err := plugintest.Start(nil, func(p *plugin.Plugin) {
		p.Sync("issues", time.Minute, func(context.Context) error { return nil }, plugin.WhileOpen(-time.Second))
	})
	if err == nil || !strings.Contains(err.Error(), "below 0") {
		t.Errorf("a sync with an interval below 0 was taken: %v", err)
	}
}

// --- declaring ---

func TestNothingIsDeclaredOnceRunning(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("late", "", func(c *plugin.Call) error {
			p.Command("another", "", func(*plugin.Call) error { return nil })
			return nil
		})
	})
	if got := ws.Run("late"); got.Exit != 1 || !strings.Contains(got.Stderr, "after Run has started") {
		t.Errorf("a command declared while running: %+v", got)
	}
}

// --- asking ws ---

func TestAskingWS(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Command("missions", "", func(c *plugin.Call) error {
			ms, err := p.Missions(c.Context(), c.Flag("all"))
			if err != nil {
				return err
			}
			for _, m := range ms {
				fmt.Fprintln(c.Stdout(), m.Slug, m.State, m.Headline.Court)
			}
			return nil
		}).Flag("all", "archived too")
		p.Command("mission", "", func(c *plugin.Call) error {
			m, err := p.Mission(c.Context(), c.Arg(0))
			if errors.Is(err, plugin.ErrNotFound) {
				return c.Say("none")
			}
			if err != nil {
				return err
			}
			return c.Say(m.Title + ": " + m.Brief + " in " + m.Capsules[0].PullRequest.State)
		}).Args("SLUG")
		p.Command("said", "", func(c *plugin.Call) error {
			shown, err := p.ShowIn(c.Context(), c.Option("mission"), c.Arg(0))
			if errors.Is(err, plugin.ErrNotFound) {
				return c.Say("none")
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(c.Stdout(), shown.Head, len(shown.Body), len(shown.Links))
			for _, s := range shown.Said {
				fmt.Fprintln(c.Stdout(), s.How, s.Kind, s.Mission, s.Ref, s.Standing, s.Path)
			}
			return nil
		}).Args("REF").Option("mission", "the mission it's one of")
		p.Command("talk", "", func(c *plugin.Call) error {
			started, err := p.StartConversation(c.Context(), c.Arg(0))
			if err != nil {
				return err
			}
			return c.Say(started.ID + " " + started.Path)
		}).Args("MESSAGE")
		p.Command("repos", "", func(c *plugin.Call) error {
			rs, err := p.Repos(c.Context())
			if err != nil {
				return err
			}
			for _, r := range rs {
				fmt.Fprintln(c.Stdout(), r.Name, len(r.Capsules), r.Capsules[0].Mission)
			}
			return nil
		})
	})

	// A ws from before these: each is ErrUnsupported, saying so.
	if got := ws.Run("repos"); got.Exit == 0 || !strings.Contains(got.Stderr, "update ws") {
		t.Errorf("repos of a ws that can't answer: %+v", got)
	}
	if got := ws.Run("said", "RAM-1"); got.Exit == 0 || !strings.Contains(got.Stderr, "update ws") {
		t.Errorf("showing in a ws that can't answer: %+v", got)
	}
	if got := ws.Run("talk", "Let's talk"); got.Exit == 0 || !strings.Contains(got.Stderr, "update ws") {
		t.Errorf("a conversation in a ws that can't start one: %+v", got)
	}

	ws.Missions(
		protocol.MissionDetail{
			Mission: protocol.Mission{Slug: "token-cache", Title: "Token cache", State: protocol.MissionReview,
				Headline: &protocol.Headline{Court: protocol.CourtYou, Text: "PR #12 waits on your review"},
				Capsules: []protocol.MissionCapsule{{Repo: "api", Name: "token-cache", PullRequest: &protocol.PullRequest{Number: 12, State: "open"}}}},
			Brief: "Cache tokens",
		},
		protocol.MissionDetail{Mission: protocol.Mission{Slug: "old", State: protocol.MissionArchived, Headline: &protocol.Headline{Court: protocol.CourtAgent}}},
	)
	ws.Repos(protocol.Repo{Name: "api", Capsules: []protocol.Capsule{{Name: "token-cache", Mission: "token-cache"}}})

	if got := ws.Run("missions"); got.Stdout != "token-cache review you\n" {
		t.Errorf("missions in play: %+v", got)
	}
	if got := ws.Run("missions", "--all"); got.Stdout != "token-cache review you\nold archived agent\n" {
		t.Errorf("missions, archived too: %+v", got)
	}
	if got := ws.Run("mission", "token-cache"); got.Stdout != "Token cache: Cache tokens in open\n" {
		t.Errorf("a mission: %+v", got)
	}
	if got := ws.Run("mission", "nope"); got.Stdout != "none\n" {
		t.Errorf("a mission that isn't there: %+v", got)
	}
	if got := ws.Run("repos"); got.Stdout != "api 1 token-cache\n" {
		t.Errorf("repos: %+v", got)
	}

	// What ws show shows, as data: one of the workspace's, and one of a
	// mission's own.
	ws.Shows(
		protocol.Shown{Ref: "RAM-1", Head: "Jira issue RAM-1", Said: []protocol.Said{
			{Ref: "token-cache", Kind: "mission", Title: "Token cache", State: "active", Standing: true, How: protocol.SaidAbout, Path: "/missions/token-cache"},
			{Ref: "d2", Mission: "token-cache", Kind: "decision", How: protocol.SaidMentions, Path: "/missions/token-cache"},
		}},
		protocol.Shown{Ref: "d2", Mission: "token-cache", Head: "Decision d2", Body: []string{"Cache for an hour"}, Links: []protocol.ShownLink{{Ref: "t1", What: "the thread it settled"}}},
	)
	if got := ws.Run("said", "RAM-1"); got.Stdout != "Jira issue RAM-1 0 0\nabout mission  token-cache true /missions/token-cache\nmentions decision token-cache d2 false /missions/token-cache\n" {
		t.Errorf("one of the workspace's things, shown: %+v", got)
	}
	if got := ws.Run("said", "--mission", "token-cache", "d2"); got.Stdout != "Decision d2 1 1\n" {
		t.Errorf("one of a mission's things, shown: %+v", got)
	}
	if got := ws.Run("said", "d2"); got.Stdout != "none\n" {
		t.Errorf("a mission's thing asked for without its mission: %+v", got)
	}

	// A conversation, started with what the person says first: the
	// stand-in names each in turn, and has what each was started with.
	started := ws.Conversations()
	if got := ws.Run("talk", "Let's work out a mission for RAM-1."); got.Stdout != "w1 /conversations/w1\n" {
		t.Errorf("a conversation started: %+v", got)
	}
	if got := ws.Run("talk", "And RAM-2."); got.Stdout != "w2 /conversations/w2\n" {
		t.Errorf("a second conversation: %+v", got)
	}
	if got := ws.Run("talk", " "); got.Exit == 0 {
		t.Errorf("a conversation with nothing said: %+v", got)
	}
	if got := started(); len(got) != 2 || got[0] != "Let's work out a mission for RAM-1." || got[1] != "And RAM-2." {
		t.Errorf("started with: %q", got)
	}
}

// --- the protocol's version ---

// --- refs ---

// issues is a kind of thing as a plugin declares one: named RAM-n, made
// of a title, a status, an assignee and a body.
func issues(p *plugin.Plugin) *plugin.Ref {
	return p.Ref("issue", "Jira issue").Prefix("RAM-").
		Fields(plugin.Title("title"), plugin.State("status"), plugin.Text("assignee", "Assignee"), plugin.Markdown("body"))
}

func TestWSAsksWhatANameIs(t *testing.T) {
	when := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		issues(p).Show(func(_ context.Context, a plugin.Asked) (plugin.Thing, error) {
			switch a.Name {
			case "RAM-1":
				body := "The clock should be injected."
				if a.Full {
					body += "\n\n## Comments\n\nDana: agreed."
				}
				return plugin.Thing{Name: a.Name, Changed: when, Fields: map[string]string{"title": "Token refresh is flaky", "status": "In Progress", "assignee": "Dana", "body": body}}, nil
			case "RAM-2":
				panic("the issue's gone wrong")
			}
			return plugin.Thing{}, plugin.ErrNotFound
		})
		// A kind that says no more than it's told ws.
		p.Ref("sprint", "Sprint").Prefix("SPRINT-").Fields(plugin.Title("name"))
	})

	got, err := ws.Show("issue", "RAM-1", false)
	want := map[string]string{"title": "Token refresh is flaky", "status": "In Progress", "assignee": "Dana", "body": "The clock should be injected."}
	if err != nil || fmt.Sprint(got.Fields) != fmt.Sprint(want) || !got.Changed.Equal(when) {
		t.Errorf("RAM-1: %+v, %v; want %v", got, err, want)
	}
	// Everything, when everything's asked for.
	if got, err := ws.Show("issue", "RAM-1", true); err != nil || !strings.Contains(got.Fields["body"], "Dana: agreed.") {
		t.Errorf("RAM-1 in full: %+v, %v", got, err)
	}
	// A name the plugin has nothing by is said to be, not failed on.
	if _, err := ws.Show("issue", "RAM-9", false); !errors.Is(err, plugin.ErrNotFound) || !strings.Contains(err.Error(), "there's no issue RAM-9") {
		t.Errorf("RAM-9: %v, want not found", err)
	}
	// A handler that panics fails its own answer, and the plugin runs on.
	if _, err := ws.Show("issue", "RAM-2", false); err == nil || !strings.Contains(err.Error(), "gone wrong") {
		t.Errorf("RAM-2: %v, want what went wrong", err)
	}
	if _, err := ws.Show("epic", "EPIC-1", false); err == nil || !strings.Contains(err.Error(), `no ref called "epic"`) {
		t.Errorf("a kind that isn't registered: %v", err)
	}
	// A kind with nothing to show one answers as if it couldn't be
	// asked: ws shows what it was told.
	if _, err := ws.Show("sprint", "SPRINT-1", false); err == nil || !strings.Contains(err.Error(), protocol.MethodRefShow) {
		t.Errorf("a kind with no Show: %v", err)
	}
	if _, err := ws.Show("issue", "RAM-1", false); err != nil {
		t.Errorf("after all that: %v", err)
	}
}

// What a ref's names follow is usually config: declared in Setup, a
// change to it starts the plugin afresh, and it registers them again.
func TestRefsFollowConfig(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		projects := p.Config.List("projects", "Projects")
		p.Setup(func() {
			r := p.Ref("issue", "Issue")
			for _, key := range projects.Get() {
				r.Prefix(key + "-")
			}
		})
	}, plugintest.Config(map[string]any{"projects": []string{"RAM", "RAP"}}))
	refs := ws.Registration().Refs
	if len(refs) != 1 || strings.Join(refs[0].Prefixes, " ") != "RAM- RAP-" {
		t.Errorf("refs: %+v", refs)
	}
}

// A plugin tells ws of its things, so ws has them without asking: all of
// a kind at once (what's gone is forgotten), or some.
func TestAPluginTellsWSOfItsThings(t *testing.T) {
	var issue *plugin.Ref
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) { issue = issues(p) })
	ctx := context.Background()
	names := func() string {
		var out []string
		for _, th := range ws.Things("issue") {
			out = append(out, th.Ref+"="+th.Fields["title"])
		}
		return strings.Join(out, " ")
	}
	titled := func(name, title string) plugin.Thing {
		return plugin.Thing{Name: name, Fields: map[string]string{"title": title}}
	}
	when := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	one := plugin.Thing{Name: "RAM-1", Changed: when, Standing: protocol.StandingSettled, Since: when.Add(-time.Hour), Fields: map[string]string{"title": "One", "status": "Done", "body": "The first.", "colour": "not one of its fields"}}
	if err := issue.Mirror(ctx, []plugin.Thing{one, titled("RAM-2", "Two")}); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "RAM-1=One RAM-2=Two" {
		t.Errorf("mirrored: %s", got)
	}
	// As ws has it: what its kind has no field for isn't kept.
	if th := ws.Things("issue")[0]; th.Kind != "issue" || !th.Changed.Equal(when) || th.Standing != protocol.StandingSettled || !th.Since.Equal(when.Add(-time.Hour)) || fmt.Sprint(th.Fields) != fmt.Sprint(map[string]string{"title": "One", "status": "Done", "body": "The first."}) {
		t.Errorf("RAM-1 as ws has it: %+v", th)
	}
	// All of them again: what's changed is put in place, and what's gone
	// is forgotten.
	if err := issue.Mirror(ctx, []plugin.Thing{titled("RAM-2", "Two, renamed"), titled("RAM-3", "Three")}); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "RAM-2=Two, renamed RAM-3=Three" {
		t.Errorf("mirrored again: %s", got)
	}
	// Some: the rest stay.
	if err := issue.Put(ctx, titled("RAM-4", "Four")); err != nil {
		t.Fatal(err)
	}
	if err := issue.Remove(ctx, "RAM-2", "RAM-9"); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "RAM-3=Three RAM-4=Four" {
		t.Errorf("after a put and a remove: %s", got)
	}
	// What ws won't keep is said, and the rest is kept.
	err := issue.Mirror(ctx, []plugin.Thing{titled("RAM-3", "Three"), titled("ram-5", "Five"), {Name: "RAM-6", Fields: map[string]string{"status": "Open"}}})
	var refused *plugin.Refused
	if !errors.As(err, &refused) || len(refused.Things) != 2 || !strings.Contains(err.Error(), "ram-5: it isn't one of issue's names") || !strings.Contains(err.Error(), "RAM-6: it has nothing for title, its title") {
		t.Errorf("refused: %v", err)
	}
	if got := names(); got != "RAM-3=Three" {
		t.Errorf("after a mirror with some refused: %s", got)
	}
	// More than ws takes at once goes in turn.
	many := make([]plugin.Thing, protocol.ThingsAtOnce+7)
	for i := range many {
		many[i] = titled(fmt.Sprintf("RAM-%d", i+1), "One of many")
	}
	if err := issue.Mirror(ctx, many); err != nil || len(ws.Things("issue")) != len(many) {
		t.Errorf("many: %d kept, %v", len(ws.Things("issue")), err)
	}

	// A ws from before it could be told says so.
	ws.Answer(protocol.MethodThingList, func(json.RawMessage) (any, error) { return nil, plugintest.NoMethod(protocol.MethodThingList) })
	if err := issue.Mirror(ctx, nil); !errors.Is(err, plugin.ErrUnsupported) {
		t.Errorf("a ws that can't be told: %v", err)
	}
}

func TestTheHandshakeChoosesTheHighestBothSpeak(t *testing.T) {
	declare := func(p *plugin.Plugin) { p.Command("hi", "", func(c *plugin.Call) error { return c.Say("hi") }) }
	if ws := plugintest.New(t, nil, declare); ws.Protocol() != protocol.Version {
		t.Errorf("chose %d, want %d", ws.Protocol(), protocol.Version)
	}
	// A ws newer than the plugin, which still speaks what it does.
	ws := plugintest.New(t, nil, declare, plugintest.Offer(1, 99))
	if ws.Protocol() != 1 {
		t.Errorf("a ws that speaks 1 and 99: chose %d", ws.Protocol())
	}
	if got := ws.Run("hi"); got.Exit != 0 || got.Stdout != "hi\n" {
		t.Errorf("a command after the handshake: %+v", got)
	}
}

// With none in common, the plugin registers nothing and says what it
// speaks, so ws can say which side is behind.
func TestNoVersionInCommonRegistersNothing(t *testing.T) {
	var ran atomic.Bool
	_, err := plugintest.Start(nil, func(p *plugin.Plugin) {
		p.Setup(func() { ran.Store(true) })
		p.Command("hi", "", func(*plugin.Call) error { return nil })
	}, plugintest.Offer(99))
	if err == nil || !strings.Contains(err.Error(), "version 0") || !strings.Contains(err.Error(), "[1]") || !strings.Contains(err.Error(), "[99]") {
		t.Fatalf("started against a ws that speaks only 99: %v", err)
	}
	if ran.Load() {
		t.Error("the plugin set itself up for a ws it can't speak to")
	}
}

// A ws from before versions were chosen sends protocol 1 and no list,
// and takes nothing but 1 in answer.
func TestAWSFromBeforeVersionsWereChosen(t *testing.T) {
	p := plugin.New(nil)
	ws, plug := io.Pipe()
	answers, out := io.Pipe()
	go p.Serve(ws, out) //nolint:errcheck // ended by closing its input
	defer plug.Close()
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocol":1,"ws":{"version":"0.3.0"},"plugin":{"name":"old","version":"0.0.0","dir":"` + t.TempDir() + `","data":"` + t.TempDir() + `"},"workspace":{"root":"/ws"},"config":{},"view":{"listen":"","base":"/plugins/old"}}}` + "\n"
	if _, err := plug.Write([]byte(init)); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result protocol.InitializeResult `json:"result"`
		Error  json.RawMessage           `json:"error"`
	}
	if err := json.NewDecoder(answers).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil || reply.Result.Protocol != 1 {
		t.Errorf("answered %+v, error %s", reply.Result, reply.Error)
	}
}

// A command with a timeout of its own outlasts the plugin's, and one with
// none runs until it's stopped.
func TestCommandTimeout(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.CommandTimeout = 20 * time.Millisecond
		wait := func(c *plugin.Call) error {
			select {
			case <-c.Context().Done():
				return c.Context().Err()
			case <-time.After(120 * time.Millisecond):
				return c.Say("done")
			}
		}
		p.Command("short", "Runs out of time", wait)
		p.Command("long", "Has longer", wait).Timeout(time.Second)
		p.Command("forever", "Has as long as it likes", wait).Timeout(0)
	})
	if got := ws.Run("short"); got.Exit != 1 || !strings.Contains(got.Stderr, "ran out of time") {
		t.Errorf("short: exit %d, %q", got.Exit, got.Stderr)
	}
	for _, name := range []string{"long", "forever"} {
		if got := ws.Run(name); got.Exit != 0 || got.Stdout != "done\n" {
			t.Errorf("%s: exit %d, %q %q", name, got.Exit, got.Stdout, got.Stderr)
		}
	}
}

// p.WS runs ws's own program in the workspace, and says what went wrong.
func TestWS(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "ws")
	body := "#!/bin/sh\nif [ \"$1\" = fail ]; then echo partial; echo 'no such thing' >&2; exit 3; fi\necho \"$PWD|$WS_ORIGIN_PWD|$*\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var p *plugin.Plugin
	plugintest.New(t, nil, func(pl *plugin.Plugin) { p = pl }, plugintest.Root(root), plugintest.Executable(script))

	out, err := p.WS(context.Background(), "show", "m12")
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(root)
	if got := strings.TrimSpace(string(out)); got != real+"|"+root+"|show m12" && got != root+"|"+root+"|show m12" {
		t.Errorf("ws ran as %q", got)
	}
	out, err = p.WS(context.Background(), "fail")
	if err == nil || !strings.Contains(err.Error(), "no such thing") || string(out) != "partial\n" {
		t.Errorf("a failing command: %q, %v", out, err)
	}

	// With no program to run, it says so.
	var bare *plugin.Plugin
	plugintest.New(t, nil, func(pl *plugin.Plugin) { bare = pl })
	if _, err := bare.WS(context.Background(), "show"); !errors.Is(err, plugin.ErrNoWS) {
		t.Errorf("with no ws: %v", err)
	}
}

// --- actions ---

// A plugin's actions are run from Find: its own, and what's done to one
// of its things. One that asks is run again with what's answered, from
// the start, and does nothing until it's asked all it wants.
func TestActionsKeysWhereAndHere(t *testing.T) {
	var asked []*plugin.Here
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Opening("new", "New issue", "/new").Beside().Keys("n").On("mission", "jira:issue")
		p.Action("note", "Note on Jira", func(a *plugin.Act) error {
			asked = append(asked, a.Here())
			if h := a.Here(); h != nil {
				a.Open("/new?about=" + h.Ref)
				return nil
			}
			a.Open("/new", plugin.Beside)
			return nil
		}).Beside()
		p.Action("board", "Board", func(a *plugin.Act) error {
			a.Open("/board")
			return nil
		})
	})

	got, _ := json.Marshal(ws.Registration().Actions)
	if string(got) != `[{"id":"new","title":"New issue","keys":["n"],"opens":"/new","where":"beside","on":["mission","jira:issue"]},{"id":"note","title":"Note on Jira","where":"beside"},{"id":"board","title":"Board"}]` {
		t.Errorf("as declared: %s", got)
	}

	// One that only opens is run as one that opens, where it says.
	if r, err := ws.Act("new"); err != nil || r.Open != "/new" || r.Where != plugin.Beside {
		t.Errorf("opening: %+v, %v", r, err)
	}
	// What was open is told; where it opens is the action's unless the
	// run says.
	if r, err := ws.ActHere(protocol.Here{Ref: "fix-it", Kind: "mission", Title: "Fix it"}, "note"); err != nil || r.Open != "/new?about=fix-it" || r.Where != plugin.Beside {
		t.Errorf("here: %+v, %v", r, err)
	}
	if r, err := ws.Act("note"); err != nil || r.Open != "/new" || r.Where != plugin.Beside {
		t.Errorf("nothing open: %+v, %v", r, err)
	}
	if len(asked) != 2 || asked[0] == nil || asked[0].Title != "Fix it" || asked[1] != nil {
		t.Errorf("told: %+v", asked)
	}
	if r, err := ws.Act("board"); err != nil || r.Open != "/board" || r.Where != "" {
		t.Errorf("the page: %+v, %v", r, err)
	}
}

func TestActionsDeclaredWrong(t *testing.T) {
	for name, bad := range map[string]protocol.Action{
		"opens no path":  {ID: "a", Title: "A", Opens: "new"},
		"nowhere":        {ID: "a", Title: "A", Where: "over"},
		"on a bad kind":  {ID: "a", Title: "A", On: []string{"Jira Issue"}},
		"a key, spaced ": {ID: "a", Title: "A", Keys: []string{" n"}},
	} {
		if protocol.ActionsProblem([]protocol.Action{bad}) == "" {
			t.Errorf("%s: allowed", name)
		}
	}
	if p := protocol.ActionsProblem([]protocol.Action{{ID: "a", Title: "A", Opens: "/new", Where: "beside", On: []string{"pr", "jira:issue"}, Keys: []string{"g i"}}}); p != "" {
		t.Errorf("a good one: %s", p)
	}
}

func TestActionsAskAndThenDo(t *testing.T) {
	var moved, made []string
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		issues(p).Action("move", "Move to…", func(a *plugin.Act) error {
			to, err := a.Pick("Move "+a.Name+" to", plugin.Choices("To Do", "In Progress")...)
			if err != nil {
				return err
			}
			why, err := a.Ask("Why?", "A line")
			if err != nil {
				return err
			}
			moved = append(moved, a.Name+" to "+to+": "+why)
			return a.Say(a.Name + " is " + to)
		}).Action("open", "Open it", func(a *plugin.Act) error {
			a.Open("/issue/" + a.Name)
			return nil
		})
		p.Action("new", "New issue…", func(a *plugin.Act) error {
			title, err := a.Ask("What's the issue?", "A title")
			if err != nil {
				return err
			}
			if title == "boom" {
				panic("it went wrong")
			}
			made = append(made, title)
			a.Open("/issue/RAM-9")
			return a.Say("Created RAM-9")
		}).Icon("plus").Hint("In Jira")
		p.Action("sync", "Sync now", func(a *plugin.Act) error { return errors.New("401 from Jira") })
	})

	reg := ws.Registration()
	got, _ := json.Marshal(reg.Actions)
	if string(got) != `[{"id":"new","title":"New issue…","icon":"plus","hint":"In Jira"},{"id":"sync","title":"Sync now"}]` {
		t.Errorf("the plugin's own: %s", got)
	}
	got, _ = json.Marshal(reg.Refs[0].Actions)
	if string(got) != `[{"id":"move","title":"Move to…"},{"id":"open","title":"Open it"}]` {
		t.Errorf("an issue's: %s", got)
	}

	// On a thing: asked a pick, then a line, then done, once.
	r, err := ws.ActOn("issue", "RAM-1", "move")
	if err != nil || r.Ask == nil || r.Ask.Type != protocol.AskPick || r.Ask.Title != "Move RAM-1 to" || len(r.Ask.Options) != 2 || r.Ask.Options[1].Value != "In Progress" || len(moved) != 0 {
		t.Fatalf("first: %+v, %v, moved %v", r, err, moved)
	}
	r, err = ws.ActOn("issue", "RAM-1", "move", "In Progress")
	if err != nil || r.Ask == nil || r.Ask.Type != protocol.AskText || r.Ask.Placeholder != "A line" || len(moved) != 0 {
		t.Fatalf("second: %+v, %v, moved %v", r, err, moved)
	}
	r, err = ws.ActOn("issue", "RAM-1", "move", "In Progress", "It's started")
	if err != nil || r.Ask != nil || r.Say != "RAM-1 is In Progress" || len(moved) != 1 || moved[0] != "RAM-1 to In Progress: It's started" {
		t.Fatalf("third: %+v, %v, moved %v", r, err, moved)
	}
	if r, err := ws.ActOn("issue", "RAM-2", "open"); err != nil || r.Open != "/issue/RAM-2" {
		t.Errorf("open: %+v, %v", r, err)
	}

	// The plugin's own.
	if r, err := ws.Act("new"); err != nil || r.Ask == nil || r.Ask.Title != "What's the issue?" {
		t.Errorf("new, unanswered: %+v, %v", r, err)
	}
	if r, err := ws.Act("new", "The clock"); err != nil || r.Say != "Created RAM-9" || r.Open != "/issue/RAM-9" || len(made) != 1 {
		t.Errorf("new, answered: %+v, %v, made %v", r, err, made)
	}
	// One that fails says why; one that panics fails its own run; one
	// that isn't there, or isn't that kind's, is said.
	if _, err := ws.Act("sync"); err == nil || !strings.Contains(err.Error(), "401 from Jira") {
		t.Errorf("sync: %v", err)
	}
	if _, err := ws.Act("new", "boom"); err == nil || !strings.Contains(err.Error(), "went wrong") {
		t.Errorf("a panic: %v", err)
	}
	for _, run := range []func() (protocol.ActionRunResult, error){
		func() (protocol.ActionRunResult, error) { return ws.Act("move") },
		func() (protocol.ActionRunResult, error) { return ws.ActOn("issue", "RAM-1", "new") },
		func() (protocol.ActionRunResult, error) { return ws.ActOn("sprint", "SPRINT-1", "move") },
	} {
		if _, err := run(); err == nil || !strings.Contains(err.Error(), "no action called") {
			t.Errorf("one that isn't there: %v", err)
		}
	}
	if r, err := ws.Act("new", "After all that"); err != nil || r.Say == "" {
		t.Errorf("after all that: %+v, %v", r, err)
	}
}

// A field that names other things says how it points at them.
func TestAFieldNamesOtherThings(t *testing.T) {
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		p.Ref("issue", "Issue").Prefix("RAM-").Fields(plugin.Title("title"),
			plugin.Names("parent", "Parent", protocol.LinkIn), plugin.NamesSeveral("links", "Links"))
	})
	got, _ := json.Marshal(ws.Registration().Refs[0].Fields[1:])
	if string(got) != `[{"name":"parent","type":"ref","label":"Parent","link":"in"},{"name":"links","type":"refs","label":"Links"}]` {
		t.Errorf("fields: %s", got)
	}
}

// A kind whose things can live in a repo says so, and how. ws finds
// those, so the plugin answers what one's file is found by, and asks ws
// which there are and for one's file.
func TestAKindsThingsLiveInARepo(t *testing.T) {
	var sketch *plugin.Ref
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		sketch = p.Ref("sketch", "Sketch").
			Prefix("sketch-").
			Fields(plugin.Title("title"), plugin.Markdown("text", "On it")).
			Remote(plugin.Remote{
				Path: "sketches/{slug}/",
				Main: "{slug}.excalidraw",
				Read: func(_ context.Context, f plugin.RemoteFile) (map[string]string, error) {
					if len(f.Content) == 0 {
						return nil, errors.New(f.File + " is empty")
					}
					return map[string]string{"text": f.Title + ": " + string(f.Content)}, nil
				},
			})
		p.Ref("note", "Note").Prefix("note-").Fields(plugin.Title("title")).Remote(plugin.Remote{Path: "notes/{slug}/", Main: "{slug}.txt"})
	})
	ctx := context.Background()

	// What ws is told: where one goes, and that the plugin reads one.
	reg := ws.Registration()
	if r := reg.Refs[0].Remote; r == nil || *r != (protocol.Remote{Path: "sketches/{slug}/", Main: "{slug}.excalidraw", Reads: true}) {
		t.Errorf("the sketch's remote, as registered: %+v", r)
	}
	if r := reg.Refs[1].Remote; r == nil || r.Reads {
		t.Errorf("the note's remote, as registered: %+v", r)
	}

	// ws asks what a file is found by.
	fields, err := ws.Read("sketch", "records/sketches/release-flow/", "Release flow", "release-flow.excalidraw", []byte("boxes"))
	if err != nil || fmt.Sprint(fields) != fmt.Sprint(map[string]string{"text": "Release flow: boxes"}) {
		t.Errorf("read: %v, %v", fields, err)
	}
	if _, err := ws.Read("sketch", "records/sketches/empty/", "Empty", "empty.excalidraw", nil); err == nil || !strings.Contains(err.Error(), "empty.excalidraw is empty") {
		t.Errorf("a file it can't read: %v", err)
	}
	// A kind that doesn't read is found by its title: there's nothing to ask.
	if _, err := ws.Read("note", "records/notes/a/", "A", "a.txt", []byte("x")); err == nil || !strings.Contains(err.Error(), protocol.MethodRemoteRead) {
		t.Errorf("a kind that doesn't read: %v", err)
	}
	if _, err := ws.Read("issue", "records/issues/a/", "A", "a.txt", []byte("x")); err == nil {
		t.Error("a kind it hasn't was read")
	}

	// A ws from before remotes says so.
	if _, err := sketch.Remotes(ctx); !errors.Is(err, plugin.ErrUnsupported) {
		t.Errorf("a ws from before remotes: %v", err)
	}

	// The plugin asks ws which there are, and for one's file.
	flow := protocol.RemoteOpened{
		RemoteThing: protocol.RemoteThing{Kind: "sketch", Name: "records/sketches/release-flow/", Repo: "records", Path: "sketches/release-flow/", Title: "Release flow", Main: "release-flow.excalidraw", Picture: "release-flow.svg", Version: "3f9c2a"},
		Content:     []byte("boxes"),
	}
	arch := protocol.RemoteOpened{RemoteThing: protocol.RemoteThing{Kind: "sketch", Name: "records/sketches/arch/", Repo: "records", Path: "sketches/arch/", Title: "Architecture", Main: "arch.excalidraw", Version: "77aa01"}}
	ws.Remotes(flow, arch, protocol.RemoteOpened{RemoteThing: protocol.RemoteThing{Kind: "note", Name: "records/notes/a/", Title: "A"}})
	things, err := sketch.Remotes(ctx)
	if err != nil || len(things) != 2 || things[0].Name != arch.Name || things[1] != flow.RemoteThing {
		t.Errorf("the sketches in repos: %+v, %v", things, err)
	}
	thing, content, err := sketch.OpenRemote(ctx, flow.Name)
	if err != nil || thing != flow.RemoteThing || string(content) != "boxes" {
		t.Errorf("opened: %+v %q, %v", thing, content, err)
	}
	if _, _, err := sketch.OpenRemote(ctx, "records/sketches/gone/"); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("one that isn't there: %v", err)
	}
}

// ws puts one of the plugin's things in a repo, and saves it there, when
// the plugin asks for the person: ws asks the plugin for the thing, names
// its folder, and tells the plugin where it is. Where it's changed there
// since the plugin last had it, the plugin's given what's there, to
// bring the two together.
func TestAThingIsPromotedAndSaved(t *testing.T) {
	type local struct{ title, scene, remote, base string }
	things := map[string]*local{"sketch-12": {title: "Release  flow!", scene: "boxes"}, "sketch-13": {title: "Architecture", scene: "layers"}, "sketch-14": {scene: "untitled"}}
	var exports []string
	var sketch *plugin.Ref
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		sketch = p.Ref("sketch", "Sketch").Prefix("sketch-").Fields(plugin.Title("title")).
			Remote(plugin.Remote{
				Path: "sketches/{slug}/", Main: "{slug}.excalidraw",
				Export: func(_ context.Context, e plugin.Exporting) (plugin.Exported, error) {
					l, ok := things[e.Name]
					if !ok {
						return plugin.Exported{}, plugin.ErrNotFound
					}
					exports = append(exports, e.Name)
					if e.Current != nil { // changed there since: its own merge
						exports[len(exports)-1] += " with " + string(e.Content)
						l.scene, l.base = l.scene+" + "+string(e.Content), e.Current.Version
					}
					return plugin.Exported{Remote: l.remote, Base: l.base, Package: plugin.Package{Title: l.title, Main: []byte(l.scene),
						Files: []plugin.PackageFile{{Name: "{slug}.svg", Content: []byte("<svg>" + l.scene + "</svg>"), Picture: true}}}}, nil
				},
				Saved: func(_ context.Context, name string, thing plugin.RemoteThing) error {
					things[name].remote, things[name].base = thing.Name, thing.Version
					return nil
				},
			})
		p.Ref("note", "Note").Prefix("note-").Fields(plugin.Title("title")).Remote(plugin.Remote{Path: "notes/{slug}/", Main: "{slug}.txt"})
	})
	ctx := context.Background()
	if r := ws.Registration().Refs; !r[0].Remote.Exports || r[1].Remote.Exports {
		t.Fatalf("which kinds export, as registered: %+v %+v", r[0].Remote, r[1].Remote)
	}

	// A ws from before it says so.
	if _, err := sketch.Promote(ctx, "sketch-12"); !errors.Is(err, plugin.ErrUnsupported) {
		t.Fatalf("a ws from before promoting: %v", err)
	}
	inRepos := ws.Remotes()

	// Promoted: ws names its folder from its title, its files after it,
	// and tells the plugin where it is.
	done, err := sketch.Promote(ctx, "sketch-12")
	if err != nil || !done.Changed || done.Capsule != "" {
		t.Fatalf("promoted: %+v, %v", done, err)
	}
	flow := done.Thing
	if flow.Name != "records/sketches/release-flow/" || flow.Title != "Release flow!" || flow.Main != "release-flow.excalidraw" || flow.Picture != "release-flow.svg" || flow.Version == "" {
		t.Fatalf("the thing, promoted: %+v", flow)
	}
	if got := inRepos(); len(got) != 1 || got[0].From != "sketch-12" || string(got[0].Content) != "boxes" || string(got[0].Package.Files[0].Content) != "<svg>boxes</svg>" {
		t.Fatalf("in the repos: %+v", got)
	}
	if l := things["sketch-12"]; l.remote != flow.Name || l.base != flow.Version {
		t.Fatalf("what the plugin was told: %+v", l)
	}
	// It lives there now: it's saved, not promoted again.
	if _, err := sketch.Promote(ctx, "sketch-12"); err == nil || !strings.Contains(err.Error(), "lives at records/sketches/release-flow/ already") {
		t.Errorf("promoted twice: %v", err)
	}
	// One put where the person said is named after that folder; one the
	// plugin hasn't, one with no title, and one not in a repo are said.
	if there, err := sketch.Promote(ctx, "sketch-13", plugin.PromoteTo("docs:design/arch/")); err != nil || there.Thing.Name != "docs/design/arch/" || there.Thing.Main != "arch.excalidraw" {
		t.Errorf("one put where the person said: %+v, %v", there.Thing, err)
	}
	if _, err := sketch.Promote(ctx, "sketch-99"); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("one the plugin hasn't: %v", err)
	}
	if _, err := sketch.Promote(ctx, "sketch-14"); err == nil || !strings.Contains(err.Error(), "it has no title") {
		t.Errorf("one with no title: %v", err)
	}
	if _, err := sketch.SaveRemote(ctx, "sketch-14"); err == nil || !strings.Contains(err.Error(), "isn't in a repo: promote it first") {
		t.Errorf("one that isn't in a repo, saved: %v", err)
	}

	// Saved as it is: nothing changes. Saved once it's drawn on: there
	// now, at another version, which the plugin's told.
	if same, err := sketch.SaveRemote(ctx, "sketch-12"); err != nil || same.Changed || same.Merged {
		t.Errorf("saved as it was: %+v, %v", same, err)
	}
	things["sketch-12"].scene, things["sketch-12"].title = "boxes and arrows", "Release flow"
	saved, err := sketch.SaveRemote(ctx, "sketch-12")
	if err != nil || !saved.Changed || saved.Merged || saved.Thing.Version == flow.Version || saved.Thing.Title != "Release flow" || saved.Thing.Name != flow.Name {
		t.Fatalf("saved: %+v, %v", saved, err)
	}
	if things["sketch-12"].base != saved.Thing.Version {
		t.Errorf("the plugin wasn't told what it saved at")
	}

	// Someone else saves it meanwhile: the plugin's asked again, with
	// what's there, and what it gives then is what's saved.
	theirs := inRepos()[1].RemoteOpened
	theirs.Content, theirs.Version = []byte("their circles"), "0123456789abcdef"
	inRepos = ws.Remotes(theirs)
	things["sketch-12"].scene = "my triangles"
	exports = nil
	merged, err := sketch.SaveRemote(ctx, "sketch-12")
	if err != nil || !merged.Changed || !merged.Merged {
		t.Fatalf("saved over a change there: %+v, %v", merged, err)
	}
	if want := []string{"sketch-12", "sketch-12 with their circles"}; fmt.Sprint(exports) != fmt.Sprint(want) {
		t.Errorf("the plugin was asked %q, want %q", exports, want)
	}
	if got := inRepos(); len(got) != 1 || string(got[0].Content) != "my triangles + their circles" {
		t.Errorf("what's there after: %+v", got)
	}
}
