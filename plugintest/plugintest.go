// Package plugintest runs a plugin inside a test, against a stand-in for
// ws: it starts the plugin, and then does what ws does to one. It runs a
// command, asks the plugin's HTTP what its app would, changes config, and
// shows what the plugin told ws.
//
//	func TestSay(t *testing.T) {
//		ws := plugintest.New(t, files, setup)
//		got := ws.Run("say")
//		if got.Stdout != "Hello, workspace\n" {
//			t.Errorf("say printed %q", got.Stdout)
//		}
//	}
//
// setup is whatever declares the plugin: what its main does between
// plugin.New and Run.
package plugintest

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/withworkspace/plugin"
	"github.com/withworkspace/plugin/internal/rpc"
	"github.com/withworkspace/plugin/protocol"
)

// WS is the stand-in for ws that a plugin under test is running against.
type WS struct {
	// Terminal is what commands are told is on the other end. The zero
	// value is no terminal: nothing is cut to fit.
	Terminal protocol.Terminal

	p    *plugin.Plugin
	conn *rpc.Conn
	reg  protocol.Registration
	ver  int // the protocol version the handshake chose
	dir  string
	keep bool          // dir is the test's own: not removed
	done chan struct{} // closed when the plugin stops

	mu      sync.Mutex
	seq     int
	notes   []note
	answers map[string]func(json.RawMessage) (any, error)
	inputs  map[string]io.Reader // what's piped to a running command, by its id
	arrive  chan struct{}        // closed and remade whenever a notification comes
	closed  bool
	started chan struct{}             // closed once the plugin's registered
	things  map[string]protocol.Thing // what it's told ws of its things, by kind and name
}

// note is a notification the plugin sent.
type note struct {
	method string
	params json.RawMessage
}

// Option says how the plugin is started.
type Option func(*options)

type options struct {
	name   string
	config map[string]any
	dir    string
	root   string
	ws     string
	offer  []int
}

// Config is the config the plugin starts with, by key.
func Config(values map[string]any) Option { return func(o *options) { o.config = values } }

// Row is one row of a config table, for Config and SetConfig: a table's
// value is its rows, in order.
//
//	plugintest.Config(map[string]any{"queries": []protocol.ConfigRow{
//		plugintest.Row("bugs", map[string]any{"label": "Bugs"}),
//	}})
//
// A field that's a table is rows in turn. (A table given as a map of
// maps, as the file has it, is read too: its rows are then by name.)
func Row(name string, values map[string]any) protocol.ConfigRow {
	out, err := encode(values)
	if err != nil {
		panic("plugintest: row " + name + ": " + err.Error())
	}
	return protocol.ConfigRow{Name: name, Values: out}
}

// Dir is the folder the plugin runs in: its data folder is dir/data. It's
// kept when the plugin stops, so a second plugin started in it finds what
// the first left. The default is a temporary folder of the plugin's own.
func Dir(dir string) Option { return func(o *options) { o.dir = dir } }

// Root is the workspace's folder, as the plugin's told (the default is the
// folder the plugin runs in).
func Root(dir string) Option { return func(o *options) { o.root = dir } }

// Executable is the program the plugin's told is ws: what it runs for a
// ws command (p.WS). A test gives it a script that answers as ws would.
// The default is none, and p.WS fails with plugin.ErrNoWS.
func Executable(path string) Option { return func(o *options) { o.ws = path } }

// Offer is the protocol versions the stand-in for ws says it speaks, in
// place of every version this release of the framework does: to try a
// plugin against a ws that's older or newer than it.
func Offer(versions ...int) Option { return func(o *options) { o.offer = versions } }

// Name is the plugin's name (the default is "test").
func Name(name string) Option { return func(o *options) { o.name = name } }

// New starts a plugin for a test, failing the test if it doesn't start.
// It's stopped when the test ends.
func New(t testing.TB, files fs.FS, setup func(p *plugin.Plugin), opts ...Option) *WS {
	t.Helper()
	ws, err := Start(files, setup, opts...)
	if err != nil {
		t.Fatalf("starting the plugin: %v", err)
	}
	t.Cleanup(ws.Close)
	return ws
}

// Start starts a plugin and returns why it didn't, if it didn't: for a
// test of a plugin that shouldn't start. Close it when done.
func Start(files fs.FS, setup func(p *plugin.Plugin), opts ...Option) (*WS, error) {
	o := options{name: "test"}
	for _, opt := range opts {
		opt(&o)
	}
	dir, keep := o.dir, o.dir != ""
	if !keep {
		var err error
		if dir, err = os.MkdirTemp("", "plugintest-"); err != nil {
			return nil, err
		}
	}
	p := plugin.New(files)
	if setup != nil {
		setup(p)
	}
	ws := &WS{p: p, dir: dir, keep: keep, done: make(chan struct{}), arrive: make(chan struct{}), started: make(chan struct{}), things: map[string]protocol.Thing{}}

	toPlugin, fromWS := io.Pipe()
	toWS, fromPlugin := io.Pipe()
	go func() {
		p.Serve(toPlugin, fromPlugin) //nolint:errcheck // a pipe's end
		fromPlugin.Close()
		close(ws.done)
	}()
	ws.conn = rpc.NewConn(toWS, fromWS, ws.handle)
	closeWS := func() { fromWS.Close() }
	go func() { <-ws.done; toWS.Close() }()

	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		closeWS()
		return nil, err
	}
	config, err := encode(o.config)
	if err != nil {
		closeWS()
		return nil, err
	}
	root := o.root
	if root == "" {
		root = dir
	}
	offer := o.offer
	if len(offer) == 0 {
		offer = protocol.Versions
	}
	in := protocol.InitializeParams{
		Protocol:  slices.Max(offer),
		Protocols: offer,
		WS:        protocol.WSInfo{Version: "test", Executable: o.ws},
		Plugin:    protocol.PluginInfo{Name: o.name, Version: "0.0.0", Dir: dir, Data: data},
		Workspace: protocol.Workspace{Root: root},
		Config:    config,
		// No socket: requests are answered by the plugin's handler.
		View: protocol.ViewInfo{Base: "/plugins/" + o.name},
	}
	var out protocol.InitializeResult
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ws.conn.Call(ctx, protocol.MethodInitialize, in, &out); err != nil {
		closeWS()
		if !keep {
			os.RemoveAll(dir) //nolint:errcheck // a temporary folder
		}
		return nil, err
	}
	if out.Protocol == 0 || !slices.Contains(offer, out.Protocol) {
		closeWS()
		if !keep {
			os.RemoveAll(dir) //nolint:errcheck // a temporary folder
		}
		return nil, fmt.Errorf("the plugin chose version %d of the plugin protocol: it speaks %v, and ws was said to speak %v", out.Protocol, out.Protocols, offer)
	}
	ws.reg, ws.ver = out.Registration, out.Protocol
	close(ws.started)
	go func() { <-ws.done; closeWS() }()
	return ws, nil
}

func encode(values map[string]any) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for k, v := range values {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("config %s: %w", k, err)
		}
		out[k] = data
	}
	return out, nil
}

// handle takes what the plugin sends ws: notifications, kept for the
// test to read.
func (ws *WS) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	ws.mu.Lock()
	answer := ws.answers[method]
	ws.mu.Unlock()
	if answer != nil {
		return answer(params)
	}
	switch method {
	case protocol.MethodThingPut, protocol.MethodThingRemove, protocol.MethodThingList:
		return ws.keepThings(method, params)
	case protocol.MethodCommandRead:
		return ws.read(params)
	}
	if !notifications[method] {
		// A request it wasn't told how to answer: as a ws from before it.
		return nil, rpc.MethodNotFound(method)
	}
	ws.mu.Lock()
	ws.notes = append(ws.notes, note{method, params})
	close(ws.arrive)
	ws.arrive = make(chan struct{})
	ws.mu.Unlock()
	return nil, nil
}

// read answers a command reading its standard input, as ws does: what's
// next of what was piped to it.
func (ws *WS) read(params json.RawMessage) (any, error) {
	var in protocol.CommandReadParams
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
	}
	ws.mu.Lock()
	r := ws.inputs[in.ID]
	ws.mu.Unlock()
	if r == nil {
		return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: "no command " + in.ID + " with input to read"}
	}
	if in.Max <= 0 || in.Max > protocol.ReadAtOnce {
		in.Max = protocol.ReadAtOnce
	}
	b := make([]byte, in.Max)
	n, err := r.Read(b)
	if n == 0 && err == io.EOF {
		return protocol.CommandReadResult{EOF: true}, nil
	}
	if err != nil && err != io.EOF {
		return nil, err
	}
	return protocol.CommandReadResult{Data: b[:n]}, nil
}

// Plugin is the plugin under test.
func (ws *WS) Plugin() *plugin.Plugin { return ws.p }

// notifications are what a plugin tells ws unasked, which the stand-in
// keeps for Notifications and Wait.
var notifications = map[string]bool{
	protocol.MethodCommandOutput: true,
	protocol.MethodLog:           true,
	protocol.MethodViewChanged:   true,
	protocol.MethodViewBadge:     true,
	protocol.MethodSyncState:     true,
}

// Answer has the stand-in answer a request the plugin sends ws (a
// protocol.Method…) with fn, given the request's params. A request it
// hasn't been told how to answer is answered CodeMethodNotFound, as a ws
// from before that request would: the plugin's ErrUnsupported.
func (ws *WS) Answer(method string, fn func(params json.RawMessage) (any, error)) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.answers == nil {
		ws.answers = map[string]func(json.RawMessage) (any, error){}
	}
	ws.answers[method] = fn
}

// NoMethod is what an Answer returns to answer as a ws from before a
// request would: the plugin's ErrUnsupported.
func NoMethod(method string) error { return rpc.MethodNotFound(method) }

// Missions are the missions the stand-in has: what it answers p.Missions
// (those not archived, unless asked for them too) and p.Mission with.
func (ws *WS) Missions(missions ...protocol.MissionDetail) {
	ws.Answer(protocol.MethodMissionList, func(params json.RawMessage) (any, error) {
		var in protocol.MissionListParams
		json.Unmarshal(params, &in) //nolint:errcheck // none is in play only
		out := protocol.MissionListResult{Missions: []protocol.Mission{}}
		for _, m := range missions {
			if in.Archived || m.State != protocol.MissionArchived {
				out.Missions = append(out.Missions, m.Mission)
			}
		}
		return out, nil
	})
	ws.Answer(protocol.MethodMissionShow, func(params json.RawMessage) (any, error) {
		var in protocol.MissionShowParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		for _, m := range missions {
			if m.Slug == in.Slug {
				return m, nil
			}
		}
		return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: "there's no mission " + in.Slug}
	})
}

// Shows are what the stand-in shows: what it answers p.Show and p.ShowIn
// with, each found by its Ref and Mission. Anything else is ErrNotFound.
func (ws *WS) Shows(shown ...protocol.Shown) {
	ws.Answer(protocol.MethodShow, func(params json.RawMessage) (any, error) {
		var in protocol.ShowParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		for _, s := range shown {
			if s.Ref == in.Ref && s.Mission == in.Mission {
				if s.Body == nil {
					s.Body = []string{}
				}
				if s.Links == nil {
					s.Links = []protocol.ShownLink{}
				}
				if s.Said == nil {
					s.Said = []protocol.Said{}
				}
				return s, nil
			}
		}
		return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: "ws has nothing called " + in.Ref}
	})
}

// Notices has the stand-in keep the notifications the plugin sends
// (p.Notify), as ws does: one of a kind the plugin didn't register is
// refused, and one with a key already sent isn't kept again. muted are
// the kinds the person sends nowhere: kept, and answered Muted. It
// returns what's kept, in the order it came.
func (ws *WS) Notices(muted ...string) func() []protocol.NotificationAddParams {
	var mu sync.Mutex
	var kept []protocol.NotificationAddParams
	ws.Answer(protocol.MethodNotificationAdd, func(params json.RawMessage) (any, error) {
		var in protocol.NotificationAddParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		if _, ok := protocol.NotifiesKind(ws.Registration().Notifies, in.Kind); !ok {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: "the plugin registered no kind of notification " + in.Kind}
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: "it says nothing"}
		}
		mu.Lock()
		defer mu.Unlock()
		if in.Key != "" {
			for i, k := range kept {
				if k.Key == in.Key {
					return protocol.NotificationAdded{N: int64(i + 1)}, nil
				}
			}
		}
		kept = append(kept, in)
		return protocol.NotificationAdded{N: int64(len(kept)), Added: true, Muted: slices.Contains(muted, in.Kind)}, nil
	})
	return func() []protocol.NotificationAddParams {
		mu.Lock()
		defer mu.Unlock()
		return append([]protocol.NotificationAddParams(nil), kept...)
	}
}

// Conversations has the stand-in start the conversations the plugin
// asks for (p.StartConversation), w1 then w2 and so on, and returns what
// each was started with, in turn.
func (ws *WS) Conversations() func() []string {
	var mu sync.Mutex
	var started []string
	ws.Answer(protocol.MethodConversationStart, func(params json.RawMessage) (any, error) {
		var in protocol.ConversationStartParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		if strings.TrimSpace(in.Message) == "" {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: "say something"}
		}
		mu.Lock()
		defer mu.Unlock()
		started = append(started, in.Message)
		id := "w" + strconv.Itoa(len(started))
		return protocol.ConversationStarted{ID: id, Path: "/conversations/" + id}, nil
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), started...)
	}
}

// Repos are the repos the stand-in has: what it answers p.Repos with.
func (ws *WS) Repos(repos ...protocol.Repo) {
	ws.Answer(protocol.MethodRepoList, func(json.RawMessage) (any, error) {
		if repos == nil {
			repos = []protocol.Repo{}
		}
		return protocol.RepoListResult{Repos: repos}, nil
	})
}

// InRepo is a thing the stand-in has in its repos: the thing and its main
// file, and the package it was last promoted or saved as, where it was.
type InRepo struct {
	protocol.RemoteOpened
	Package protocol.RemotePackage
	// From is the plugin's own name for the thing it was promoted from.
	From string
}

// Remotes are the things the stand-in has in its repos: what it answers
// Ref.Remotes and Ref.OpenRemote with, and what Ref.Promote and
// Ref.SaveRemote change, as ws does: it asks the plugin for the thing
// (the kind's Export, again with what's there where that's changed
// since), and tells it once it's there (the kind's Saved). A promoted
// thing goes in records, in the folder its kind's Remote names for its
// title's slug (with -2 and so on where that's taken), and a thing's
// version is its folder's: another when its title, its main file or
// anything made from it changes. What's returned is what's there now, in
// order of name.
func (ws *WS) Remotes(remotes ...protocol.RemoteOpened) func() []InRepo {
	var mu sync.Mutex
	has := map[string]*InRepo{}
	for _, r := range remotes {
		has[r.Name] = &InRepo{RemoteOpened: r}
	}
	all := func() []InRepo {
		out := make([]InRepo, 0, len(has))
		for _, r := range has {
			out = append(out, *r)
		}
		slices.SortFunc(out, func(a, b InRepo) int { return strings.Compare(a.Name, b.Name) })
		return out
	}
	version := func(pkg protocol.RemotePackage) string {
		h := sha1.New()
		part := func(name string, content []byte) { fmt.Fprintf(h, "%s\x00%d\x00%s", name, len(content), content) }
		part("title", []byte(strings.Join(strings.Fields(pkg.Title), " ")))
		part("main", pkg.Main)
		for _, f := range pkg.Files {
			part("file "+f.Name, f.Content)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	// put writes a package into a thing, as its folder would hold it.
	put := func(r *InRepo, slug string, kind protocol.Ref, pkg protocol.RemotePackage) {
		r.Title, r.Content, r.Version, r.Package = strings.Join(strings.Fields(pkg.Title), " "), pkg.Main, version(pkg), pkg
		r.Main, r.Picture = strings.ReplaceAll(kind.Remote.Main, protocol.RemoteSlug, slug), ""
		for _, f := range pkg.Files {
			if f.Picture {
				r.Picture = strings.ReplaceAll(f.Name, protocol.RemoteSlug, slug)
			}
		}
		r.Changed = time.Now().UTC().Truncate(time.Second)
	}
	kindOf := func(name string) (protocol.Ref, error) {
		<-ws.started
		i := slices.IndexFunc(ws.reg.Refs, func(r protocol.Ref) bool { return r.Kind == name })
		if i < 0 || ws.reg.Refs[i].Remote == nil {
			return protocol.Ref{}, &rpc.Error{Code: protocol.CodeInvalidParams, Message: "the plugin has no kind " + name + " whose things live in a repo"}
		}
		return ws.reg.Refs[i], nil
	}
	ws.Answer(protocol.MethodRemoteList, func(params json.RawMessage) (any, error) {
		var in protocol.RemoteListParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		mu.Lock()
		defer mu.Unlock()
		out := protocol.RemoteListResult{Things: []protocol.RemoteThing{}}
		for _, r := range all() {
			if r.Kind == in.Kind {
				out.Things = append(out.Things, r.RemoteThing)
			}
		}
		return out, nil
	})
	ws.Answer(protocol.MethodRemoteOpen, func(params json.RawMessage) (any, error) {
		var in protocol.RemoteOpenParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		mu.Lock()
		defer mu.Unlock()
		if r, ok := has[in.Name]; ok && r.Kind == in.Kind {
			return r.RemoteOpened, nil
		}
		return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: "ws has no " + in.Kind + " at " + in.Name}
	})
	// As ws does: the plugin's asked for the thing, and told it's there.
	export := func(kind, ref string, current *protocol.RemoteOpened) (protocol.RemoteExported, error) {
		var out protocol.RemoteExported
		err := ws.conn.Call(context.Background(), protocol.MethodRemoteExport, protocol.RemoteExportParams{Kind: kind, Ref: ref, Current: current}, &out)
		return out, err
	}
	// taken refuses a package ws wouldn't write.
	taken := func(given protocol.RemoteExported) error {
		if problem := given.RemotePackage.Problem(); problem != "" {
			return &rpc.Error{Code: protocol.CodeInvalidParams, Message: problem}
		}
		return nil
	}
	told := func(kind, ref string, thing protocol.RemoteThing) error {
		return ws.conn.Call(context.Background(), protocol.MethodRemoteSaved, protocol.RemoteSavedParams{Kind: kind, Ref: ref, Thing: thing}, nil)
	}
	ws.Answer(protocol.MethodRemotePromote, func(params json.RawMessage) (any, error) {
		var in protocol.RemotePromoteParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		kind, err := kindOf(in.Kind)
		if err != nil {
			return nil, err
		}
		given, err := export(in.Kind, in.Ref, nil)
		if err != nil {
			return nil, err
		}
		if given.Name != "" {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: in.Ref + " lives at " + given.Name + " already: save it there"}
		}
		if err := taken(given); err != nil {
			return nil, err
		}
		mu.Lock()
		base := slugOf(given.Title)
		slug, repo, folder := base, "records", ""
		for n := 2; ; n++ {
			folder = strings.ReplaceAll(kind.Remote.Path, protocol.RemoteSlug, slug)
			if to := strings.TrimSpace(in.To); to != "" {
				// Where the person said: its files are named after the folder.
				repo, folder, _ = strings.Cut(to, ":")
				folder = strings.TrimSuffix(folder, "/") + "/"
				slug = filepath.Base(strings.TrimSuffix(folder, "/"))
			}
			if _, taken := has[repo+"/"+folder]; !taken {
				break
			}
			if in.To != "" {
				mu.Unlock()
				return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: "there's something at " + in.To + " already"}
			}
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		r := &InRepo{From: in.Ref}
		r.Kind, r.Name, r.Repo, r.Path = in.Kind, repo+"/"+folder, repo, folder
		put(r, slug, kind, given.RemotePackage)
		has[r.Name] = r
		thing := r.RemoteThing
		mu.Unlock()
		if err := told(in.Kind, in.Ref, thing); err != nil {
			return nil, err
		}
		return protocol.RemoteSaved{Thing: thing, Changed: true, Said: "Promoted to " + repo + ":" + folder}, nil
	})
	ws.Answer(protocol.MethodRemoteSave, func(params json.RawMessage) (any, error) {
		var in protocol.RemoteSaveParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		kind, err := kindOf(in.Kind)
		if err != nil {
			return nil, err
		}
		given, err := export(in.Kind, in.Ref, nil)
		if err != nil {
			return nil, err
		}
		if given.Name == "" {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: in.Ref + " isn't in a repo: promote it first"}
		}
		there := func() (protocol.RemoteOpened, bool) {
			mu.Lock()
			defer mu.Unlock()
			r, ok := has[given.Name]
			if !ok || r.Kind != in.Kind {
				return protocol.RemoteOpened{}, false
			}
			return r.RemoteOpened, true
		}
		now, ok := there()
		if !ok {
			return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: "ws has no " + in.Kind + " at " + given.Name}
		}
		merged := false
		if given.Base != now.Version {
			// It's changed there since: the plugin brings the two together.
			if given, err = export(in.Kind, in.Ref, &now); err != nil {
				return nil, err
			}
			if given.Base != now.Version {
				return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: in.Ref + " was given what's at " + now.Name + " now, and what came back wasn't made from it"}
			}
			merged = true
		}
		if err := taken(given); err != nil {
			return nil, err
		}
		mu.Lock()
		r := has[now.Name]
		was, _ := json.Marshal(r.Package)
		next, _ := json.Marshal(given.RemotePackage)
		changed := string(was) != string(next)
		if changed {
			put(r, filepath.Base(strings.TrimSuffix(r.Path, "/")), kind, given.RemotePackage) // its folder's name is its slug
		}
		thing := r.RemoteThing
		mu.Unlock()
		if err := told(in.Kind, in.Ref, thing); err != nil {
			return nil, err
		}
		return protocol.RemoteSaved{Thing: thing, Changed: changed, Merged: merged, Said: "Saved to " + thing.Repo + ":" + thing.Path}, nil
	})
	return func() []InRepo {
		mu.Lock()
		defer mu.Unlock()
		return all()
	}
}

// slugOf is a title as the stand-in names a folder for it: its letters
// and digits, lower-case, a hyphen between runs of them.
func slugOf(title string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if gap && b.Len() > 0 {
				b.WriteByte('-')
			}
			gap = false
			b.WriteRune(r)
		default:
			gap = true
		}
	}
	if b.Len() == 0 {
		return "untitled"
	}
	return b.String()
}

// Read asks the plugin what a main file is found by, as ws does of one it
// finds in a repo: kind is the Ref's, name the thing's (its folder), and
// title what its thing.toml calls it.
func (ws *WS) Read(kind, name, title, file string, content []byte) (map[string]string, error) {
	var out protocol.RemoteReadResult
	err := ws.conn.Call(context.Background(), protocol.MethodRemoteRead, protocol.RemoteReadParams{Kind: kind, Name: name, Title: title, File: file, Content: content}, &out)
	return out.Fields, err
}

// keepThings answers what the plugin tells ws of its things, as ws does:
// a thing is kept when its kind is one the plugin registered, its name
// one of that kind's, and it has something for its kind's title.
func (ws *WS) keepThings(method string, params json.RawMessage) (any, error) {
	<-ws.started // what it registered is what its things are named by
	kind := func(k string) (protocol.Ref, bool) {
		i := slices.IndexFunc(ws.reg.Refs, func(r protocol.Ref) bool { return r.Kind == k })
		if i < 0 {
			return protocol.Ref{}, false
		}
		return ws.reg.Refs[i], true
	}
	invalid := func(err error) error { return &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()} }
	ws.mu.Lock()
	defer ws.mu.Unlock()
	switch method {
	case protocol.MethodThingPut:
		var in protocol.ThingPutParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, invalid(err)
		}
		if len(in.Things) > protocol.ThingsAtOnce {
			return nil, invalid(fmt.Errorf("%d things at once: %d is the most", len(in.Things), protocol.ThingsAtOnce))
		}
		out := protocol.ThingPutResult{}
		for _, t := range in.Things {
			why := ""
			r, ok := kind(t.Kind)
			switch {
			case !ok:
				why = "the plugin registered no ref called " + t.Kind
			case !r.Names(t.Ref):
				why = "it isn't one of " + t.Kind + "'s names"
			case r.Typed(protocol.FieldTitle) == "":
				why = t.Kind + " has no title field: there's nothing to call one by"
			case strings.TrimSpace(t.Fields[r.Typed(protocol.FieldTitle)]) == "":
				why = "it has nothing for " + r.Typed(protocol.FieldTitle) + ", its title"
			}
			if why != "" {
				out.Refused = append(out.Refused, protocol.ThingRefused{Kind: t.Kind, Ref: t.Ref, Why: why})
				continue
			}
			// What its kind has no field for isn't kept.
			kept := map[string]string{}
			for name, value := range t.Fields {
				if _, ok := r.Named(name); ok {
					kept[name] = value
				}
			}
			t.Fields = kept
			ws.things[t.Kind+"\x00"+t.Ref] = t
		}
		return out, nil
	case protocol.MethodThingRemove:
		var in protocol.ThingRemoveParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, invalid(err)
		}
		for _, ref := range in.Refs {
			delete(ws.things, in.Kind+"\x00"+ref)
		}
		return struct{}{}, nil
	}
	var in protocol.ThingListParams
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, invalid(err)
	}
	out := protocol.ThingListResult{Refs: []string{}}
	for _, t := range ws.things {
		if t.Kind == in.Kind {
			out.Refs = append(out.Refs, t.Ref)
		}
	}
	slices.Sort(out.Refs)
	return out, nil
}

// Things are what the plugin has told ws of its things of a kind (a
// Ref's Mirror, Put and Remove), by name: what ws would find and draw
// without asking it.
func (ws *WS) Things(kind string) []protocol.Thing {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := []protocol.Thing{}
	for _, t := range ws.things {
		if t.Kind == kind {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b protocol.Thing) int { return strings.Compare(a.Ref, b.Ref) })
	return out
}

// Show asks the plugin what one of its things is, as ws does when a name
// it registered is shown (ws show RAM-123, the card under a name): kind
// is the Ref's, name the thing's as written. A name the plugin has
// nothing by is plugin.ErrNotFound.
func (ws *WS) Show(kind, name string, full bool) (protocol.RefShowResult, error) {
	var out protocol.RefShowResult
	err := ws.conn.Call(context.Background(), protocol.MethodRefShow, protocol.RefShowParams{Kind: kind, Ref: name, Full: full}, &out)
	var e *rpc.Error
	if errors.As(err, &e) && e.Code == protocol.CodeNotFound {
		return out, fmt.Errorf("%s: %w", e.Message, plugin.ErrNotFound)
	}
	return out, err
}

// Act runs one of the plugin's own actions, as ws does when it's chosen
// in Find, with what's been answered so far to what it asked: an answer
// with an Ask is the action wanting one more.
func (ws *WS) Act(id string, answers ...string) (protocol.ActionRunResult, error) {
	return ws.act(protocol.ActionRunParams{ID: id, Answers: answers})
}

// ActOn runs one of a kind's actions on one of its things, by name.
func (ws *WS) ActOn(kind, name, id string, answers ...string) (protocol.ActionRunResult, error) {
	return ws.act(protocol.ActionRunParams{ID: id, Kind: kind, Ref: name, Answers: answers})
}

func (ws *WS) act(in protocol.ActionRunParams) (protocol.ActionRunResult, error) {
	if in.Answers == nil {
		in.Answers = []string{}
	}
	var out protocol.ActionRunResult
	return out, ws.conn.Call(context.Background(), protocol.MethodActionRun, in, &out)
}

// Guide asks how one of what the plugin proposes is written, as a
// conversation's agent does first.
func (ws *WS) Guide(id string) (string, error) {
	out, err := ws.DraftInput(id, nil)
	return out.Guide, err
}

// Draft proposes one of what the plugin proposes, written as input (JSON:
// a json.RawMessage or string as it is, anything else encoded), as a
// conversation's agent does in w1: what's answered is what it drafted,
// or why it's refused.
func (ws *WS) Draft(id string, input any) (protocol.ProposalDraftResult, error) {
	var raw json.RawMessage
	switch v := input.(type) {
	case json.RawMessage:
		raw = v
	case string:
		raw = json.RawMessage(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return protocol.ProposalDraftResult{}, err
		}
		raw = data
	}
	return ws.DraftInput(id, raw)
}

// DraftInput is Draft with the input as it's sent: nil asks for the guide.
func (ws *WS) DraftInput(id string, input json.RawMessage) (protocol.ProposalDraftResult, error) {
	var out protocol.ProposalDraftResult
	return out, ws.conn.Call(context.Background(), protocol.MethodProposalDraft, protocol.ProposalDraftParams{ID: id, Input: input, Conversation: "w1"}, &out)
}

// Accept accepts items of what was drafted (by number, from 1; none: all
// of them), as the user does, under key: asked again under the same key,
// an item done already isn't done again.
func (ws *WS) Accept(id, key string, drafted *protocol.Proposal, items ...int) (protocol.ProposalAcceptResult, error) {
	if drafted == nil {
		return protocol.ProposalAcceptResult{}, errors.New("plugintest: Accept: nothing was drafted (the draft was refused?)")
	}
	if len(items) == 0 {
		for i := range drafted.Items {
			items = append(items, i+1)
		}
	}
	in := protocol.ProposalAcceptParams{ID: id, Key: key, Conversation: "w1"}
	for _, n := range items {
		in.Items = append(in.Items, protocol.AcceptItem{N: n, Payload: drafted.Items[n-1].Payload})
	}
	return ws.AcceptItems(in)
}

// AcceptItems is Accept as it's sent: with what items accepted before
// made, for one that names them.
func (ws *WS) AcceptItems(in protocol.ProposalAcceptParams) (protocol.ProposalAcceptResult, error) {
	var out protocol.ProposalAcceptResult
	return out, ws.conn.Call(context.Background(), protocol.MethodProposalAccept, in, &out)
}

// Protocol is the version of the plugin protocol the handshake chose.
func (ws *WS) Protocol() int { return ws.ver }

// Registration is what the plugin registered.
func (ws *WS) Registration() protocol.Registration { return ws.reg }

// Data is the plugin's data folder: a temporary one.
func (ws *WS) Data() string { return filepath.Join(ws.dir, "data") }

// Result is how a command went.
type Result struct {
	Stdout string
	Stderr string
	Exit   int
}

// Run runs the plugin's command as "ws <plugin> args..." would, and
// returns what it printed and its exit code.
func (ws *WS) Run(args ...string) Result {
	return ws.RunContext(context.Background(), args...)
}

// RunContext is Run for a command that's stopped: ctx ending is Ctrl-C.
// What the command printed before it was stopped is returned, with an
// exit code of -1.
func (ws *WS) RunContext(ctx context.Context, args ...string) Result {
	return ws.run(ctx, nil, args)
}

// Pipe runs the plugin's command with input piped to it, as
// "ws <plugin> args... < file" would: Call.Stdin reads input, and
// Call.Piped is true. Run gives a command nothing to read.
func (ws *WS) Pipe(input string, args ...string) Result {
	return ws.run(context.Background(), strings.NewReader(input), args)
}

func (ws *WS) run(ctx context.Context, input io.Reader, args []string) Result {
	ws.mu.Lock()
	ws.seq++
	id := fmt.Sprintf("c%d", ws.seq)
	if input != nil {
		if ws.inputs == nil {
			ws.inputs = map[string]io.Reader{}
		}
		ws.inputs[id] = input
	}
	ws.mu.Unlock()
	defer func() {
		ws.mu.Lock()
		delete(ws.inputs, id)
		ws.mu.Unlock()
	}()
	if args == nil {
		args = []string{}
	}
	run := protocol.CommandRunParams{ID: id, Args: args, Dir: ws.dir, Terminal: ws.Terminal}
	if input != nil {
		run.Input = protocol.InputPiped
	}
	var out protocol.CommandRunResult
	err := ws.conn.Call(ctx, protocol.MethodCommandRun, run, &out)
	res := Result{Exit: out.Exit}
	if err != nil {
		res.Exit, res.Stderr = -1, err.Error()+"\n"
	}
	// Output sent before the answer was handled before the answer.
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, n := range ws.notes {
		if n.method != protocol.MethodCommandOutput {
			continue
		}
		var o protocol.CommandOutputParams
		if json.Unmarshal(n.params, &o) != nil || o.ID != id {
			continue
		}
		if o.Stream == protocol.Stderr {
			res.Stderr += o.Data
		} else {
			res.Stdout += o.Data
		}
	}
	return res
}

// Response is an answer from the plugin's HTTP: its app's files, and
// what its app asks it.
type Response struct {
	Status int
	Header http.Header
	Body   string
}

// Get asks the plugin's HTTP for path, as its app would with
// fetch(ws.api(path)): the path under the plugin's base
// ("/api/issues?open=1"), as ws sends it on.
func (ws *WS) Get(path string) Response {
	return ws.serve(httptest.NewRequest(http.MethodGet, path, nil))
}

// Post posts a form to the plugin's HTTP. For any other request (a JSON
// body, another method), serve one with Plugin().Handler().
func (ws *WS) Post(path string, form url.Values) Response {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return ws.serve(r)
}

// Send asks the plugin's HTTP as its app does an endpoint (plugin.Get,
// plugin.Post…): method and path, with body sent as JSON, or nothing sent
// for a nil body.
func (ws *WS) Send(method, path string, body any) Response {
	if body == nil {
		return ws.serve(httptest.NewRequest(method, path, nil))
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Response{Status: http.StatusBadRequest, Header: http.Header{}, Body: "plugintest: the body isn't JSON: " + err.Error()}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	return ws.serve(r)
}

func (ws *WS) serve(r *http.Request) Response {
	r.Header.Set("X-Forwarded-Prefix", "/plugins/"+ws.p.Name())
	rec := httptest.NewRecorder()
	ws.p.Handler().ServeHTTP(rec, r)
	return Response{Status: rec.Code, Header: rec.Header(), Body: rec.Body.String()}
}

// SetConfig changes the plugin's config to values, the whole of it, and
// returns once the plugin has taken it.
func (ws *WS) SetConfig(values map[string]any) error {
	config, err := encode(values)
	if err != nil {
		return err
	}
	if err := ws.conn.Notify(protocol.MethodConfigChanged, protocol.ConfigChangedParams{Config: config}); err != nil {
		return err
	}
	// A request is answered only after what came before it was handled.
	ws.conn.Call(context.Background(), "$/ping", nil, nil) //nolint:errcheck // nobody answers it: that's the point
	return nil
}

// Look says who's looking at what the plugin shows, as ws does when it
// changes, and returns once the plugin has taken it.
func (ws *WS) Look(who plugin.Looking) error {
	if err := ws.conn.Notify(protocol.MethodViewLooking, protocol.ViewLookingParams{App: who.App, WS: who.WS || who.App}); err != nil {
		return err
	}
	// A request is answered only after what came before it was handled.
	ws.conn.Call(context.Background(), "$/ping", nil, nil) //nolint:errcheck // nobody answers it: that's the point
	return nil
}

// Notifications are the params of each notification of a method the
// plugin has sent so far ("view/changed", "sync/state", "log", …).
func (ws *WS) Notifications(method string) []json.RawMessage {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var out []json.RawMessage
	for _, n := range ws.notes {
		if n.method == method {
			out = append(out, n.params)
		}
	}
	return out
}

// Wait waits until the plugin has sent n notifications of a method, and
// returns them, or fewer after five seconds.
func (ws *WS) Wait(method string, n int) []json.RawMessage {
	deadline := time.After(5 * time.Second)
	for {
		got := ws.Notifications(method)
		if len(got) >= n {
			return got
		}
		ws.mu.Lock()
		arrive := ws.arrive
		ws.mu.Unlock()
		select {
		case <-arrive:
		case <-ws.done:
			return ws.Notifications(method)
		case <-deadline:
			return ws.Notifications(method)
		}
	}
}

// Logs are the warnings and errors the plugin has told ws of.
func (ws *WS) Logs() []protocol.LogParams {
	var out []protocol.LogParams
	for _, raw := range ws.Notifications(protocol.MethodLog) {
		var l protocol.LogParams
		if json.Unmarshal(raw, &l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// Done is closed when the plugin stops: told to, or of its own accord
// (config its registration depends on changed).
func (ws *WS) Done() <-chan struct{} { return ws.done }

// Close stops the plugin, as ws does: shutdown, then its stdin closed.
func (ws *WS) Close() {
	ws.mu.Lock()
	if ws.closed {
		ws.mu.Unlock()
		return
	}
	ws.closed = true
	ws.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws.conn.Call(ctx, protocol.MethodShutdown, nil, nil) //nolint:errcheck // it may have stopped already
	select {
	case <-ws.done:
	case <-time.After(5 * time.Second):
	}
	if !ws.keep {
		os.RemoveAll(ws.dir) //nolint:errcheck // a temporary folder
	}
}
