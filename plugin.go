// Package plugin is the framework for writing a ws plugin in Go.
//
// A plugin is one process that ws's daemon starts and speaks to over the
// process's stdin and stdout (see PROTOCOL.md). This package speaks for it,
// so a plugin is what it declares and a call to Run:
//
//	//go:embed agents.md migrations ui/dist
//	var files embed.FS
//
//	// Hello is what the app is sent.
//	type Hello struct {
//		Greeting string `json:"greeting"`
//	}
//
//	func main() {
//		p := plugin.New(files)
//		greeting := p.Config.String("greeting", "Greeting", plugin.Default("Hello"))
//		p.UI("ui/dist")
//		p.View("Hello", plugin.Icon("sparkle"))
//		plugin.Get(p, "/api/greeting", func(r *http.Request) (Hello, error) {
//			return Hello{Greeting: greeting.Get()}, nil
//		})
//		p.Command("say", "Says the greeting", func(c *plugin.Call) error {
//			return c.Say(greeting.Get() + ", workspace")
//		})
//		p.Run()
//	}
//
// Declaring a thing and handling it are one call, and what ws is told the
// plugin adds (its registration) is written from those calls.
//
// A plugin's web UI is one client-side app (UI), which ws shows in a page
// of its own, at each of the plugin's views (View). The plugin serves the
// app's built files and what the app asks it: its endpoints (Get, Post),
// each declared with the types it takes and answers, which `go tool
// apigen` writes for the app in TypeScript. The framework renders no
// pages, and a plugin has no HTML: the app talks to ws with
// @withworkspace/ui-lib, the page's side of the frame protocol.
package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/withworkspace/plugin/internal/rpc"
	"github.com/withworkspace/plugin/protocol"
)

// Plugin is a plugin: what it declares before Run, and what it's running
// with after.
type Plugin struct {
	// Config is the plugin's config: keys are declared on it, and read
	// through what declaring them returns.
	Config *Config
	// Log is the plugin's log. Its lines go to the plugin's log file, and
	// warnings and errors to ws as well.
	Log *slog.Logger
	// CommandTimeout is how long a command may run (default 2 minutes);
	// SyncTimeout a sync (5 minutes).
	CommandTimeout time.Duration
	SyncTimeout    time.Duration

	files fs.FS

	// What's declared.
	description string
	commands    []*Command
	views       []*View
	uiDir       string // the folder of files that holds the app, as built
	routes      []route
	syncs       []*syncer
	refs        []*Ref
	actions     []*Action
	proposals   []*Proposal
	notifies    []*NotifyKind
	// accepting is held while a proposal's items are done: one at a time.
	accepting sync.Mutex
	setups    []func()
	onConfig  []func()
	onLooking []func(Looking)
	guide     string
	guideSet  bool
	roles     []string
	declErrs  []error

	mu       sync.Mutex
	started  bool // Run has answered initialize: nothing more can be declared
	inSetup  bool
	info     protocol.InitializeParams
	conn     *rpc.Conn
	db       *sql.DB
	broken   error // why nothing can run: a migration failed
	listener net.Listener
	server   *http.Server
	handler  http.Handler // the plugin's HTTP: its app's files and its handlers
	ui       *protocol.UI // the app, as ws is told of it
	looking  Looking      // who's looking, as ws last said

	// The wall clock, and how often a resting sync looks at it: a test's
	// to set.
	now     func() time.Time
	recheck time.Duration

	running     sync.WaitGroup // commands being run
	initialized chan struct{}  // closed once initialize has been answered for

	ctx    context.Context // ends when the plugin stops
	cancel context.CancelFunc
	done   chan struct{} // closed to stop Serve
	once   sync.Once
}

// New is a plugin whose files (agents.md, migrations/, its app as built)
// are in files: the plugin's folder, built into its executable (with
// //go:embed). files may be nil for a plugin with none of them.
func New(files fs.FS) *Plugin {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Plugin{
		files:          files,
		CommandTimeout: 2 * time.Minute,
		SyncTimeout:    5 * time.Minute,
		now:            func() time.Time { return time.Now().Round(0) }, // no monotonic reading: it stops while a machine sleeps
		recheck:        time.Minute,
		ctx:            ctx,
		cancel:         cancel,
		done:           make(chan struct{}),
		initialized:    make(chan struct{}),
	}
	p.Config = &Config{p: p, values: map[string]json.RawMessage{}}
	p.Log = slog.New(&logHandler{p: p, text: slog.NewTextHandler(os.Stderr, nil)})
	return p
}

// Describe says what the plugin is, in a line, for ws plugin list.
func (p *Plugin) Describe(text string) {
	p.declaring("Describe")
	p.description = text
}

// Setup is run once initialize has brought the config, before the
// registration is given: it's where to declare what depends on config.
// When a config key read in fn later changes, the plugin exits and ws
// starts it afresh, so what was registered from it is registered again.
func (p *Plugin) Setup(fn func()) {
	p.declaring("Setup")
	p.setups = append(p.setups, fn)
}

// OnConfig is run after the config changes while the plugin runs. Values
// read through their keys are already the new ones.
func (p *Plugin) OnConfig(fn func()) {
	p.declaring("OnConfig")
	p.onConfig = append(p.onConfig, fn)
}

// Looking is who's looking at what the plugin shows in ws's web UI, as ws
// last said. ws says it and asks nothing by it: what a plugin fetches,
// and when, is the plugin's to decide.
type Looking struct {
	// App: the plugin's app is on screen, in a page of ws's that's showing.
	App bool
	// WS: a page of ws's web UI is showing, whichever it is, so the
	// plugin's sidebar entries and their badges are on screen. True
	// whenever App is.
	WS bool
}

// Looking is who's looking now. It's nobody until ws says otherwise, and
// a ws from before it said never does.
func (p *Plugin) Looking() Looking {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.looking
}

// OnLooking is run when who's looking changes, with who is now: for a
// plugin that decides for itself what that's worth (a sync's WhileOpen
// covers the usual case). Like any notification's, fn mustn't wait on
// ws.
func (p *Plugin) OnLooking(fn func(Looking)) {
	p.declaring("OnLooking")
	p.onLooking = append(p.onLooking, fn)
}

// looked takes who's looking, as ws said it: syncs work out again when
// they're next due, and the plugin's own code is told.
func (p *Plugin) looked(l Looking) {
	l.WS = l.WS || l.App
	p.mu.Lock()
	was := p.looking
	p.looking = l
	p.mu.Unlock()
	if was == l {
		return
	}
	p.lookSyncs()
	for _, fn := range p.onLooking {
		p.guard("OnLooking", func() { fn(l) })
	}
}

// Guide is what agents are told of the plugin: markdown, and the roles
// it's for (none means every role). A plugin whose files hold agents.md
// needs no call: that's its guide. Guide("", roles...) keeps the file's
// text and names roles.
func (p *Plugin) Guide(text string, roles ...string) {
	p.declaring("Guide")
	if text != "" {
		p.guide, p.guideSet = text, true
	}
	p.roles = roles
}

// declaring panics if the plugin is running: what a plugin adds is said
// once, as it starts.
func (p *Plugin) declaring(what string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started && !p.inSetup {
		panic("plugin: " + what + " after Run has started: declare everything before Run, or in Setup")
	}
}

// Run speaks the protocol on stdin and stdout until ws says to stop, and
// then exits. With --registration as the only argument it prints what the
// plugin would register, and exits.
func (p *Plugin) Run() {
	if len(os.Args) == 2 && os.Args[1] == "--registration" {
		reg, err := p.Registration()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(reg) //nolint:errcheck // stdout
		os.Exit(0)
	}
	// Stdout is the connection's alone: anything the plugin's own code
	// prints goes to its log instead.
	out := os.Stdout
	os.Stdout = os.Stderr
	if err := p.Serve(os.Stdin, out); err != nil {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Serve speaks the protocol on r and w until the other side says to stop
// or r ends. Run is Serve on stdin and stdout; a test serves on pipes.
func (p *Plugin) Serve(r io.Reader, w io.Writer) error {
	conn := rpc.NewConn(r, w, p.handle)
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
	select {
	case <-conn.Done():
	case <-p.done:
	}
	p.stop()
	return conn.Err()
}

// stop ends what the plugin holds: its handlers, its HTTP, its database.
func (p *Plugin) stop() {
	p.cancel()
	p.mu.Lock()
	server, db := p.server, p.db
	p.server, p.db = nil, nil
	p.mu.Unlock()
	if server != nil {
		server.Close() //nolint:errcheck // stopping
	}
	if db != nil {
		db.Close() //nolint:errcheck // stopping
	}
}

// exit stops Serve, once.
func (p *Plugin) exit() { p.once.Do(func() { close(p.done) }) }

// handle answers what ws asks.
func (p *Plugin) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case protocol.MethodInitialize:
		var in protocol.InitializeParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		return p.initialize(in)
	case protocol.MethodConfigChanged:
		var in protocol.ConfigChangedParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		p.configChanged(in.Config)
		return nil, nil
	case protocol.MethodViewLooking:
		var in protocol.ViewLookingParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		// What's declared is all there once the plugin's ready.
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.looked(Looking{App: in.App, WS: in.WS})
		return nil, nil
	case protocol.MethodCommandRun:
		var in protocol.CommandRunParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		// Nothing runs before the plugin's ready for it.
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		return protocol.CommandRunResult{Exit: p.runCommand(ctx, in)}, nil
	case protocol.MethodRefShow:
		var in protocol.RefShowParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		return p.showRef(ctx, in)
	case protocol.MethodRemoteRead:
		var in protocol.RemoteReadParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		return p.readRemote(ctx, in)
	case protocol.MethodRemoteExport:
		var in protocol.RemoteExportParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		return p.exportRemote(ctx, in)
	case protocol.MethodRemoteSaved:
		var in protocol.RemoteSavedParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		return p.savedRemote(ctx, in)
	case protocol.MethodActionRun:
		var in protocol.ActionRunParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		return p.runAction(ctx, in)
	case protocol.MethodProposalDraft, protocol.MethodProposalAccept:
		select {
		case <-p.initialized:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.running.Add(1)
		defer p.running.Done()
		if method == protocol.MethodProposalDraft {
			var in protocol.ProposalDraftParams
			if err := json.Unmarshal(params, &in); err != nil {
				return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
			}
			return p.draftProposal(ctx, in)
		}
		var in protocol.ProposalAcceptParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
		}
		return p.acceptProposal(ctx, in)
	case protocol.MethodShutdown:
		// Commands being run get a moment to finish and be answered.
		finished := make(chan struct{})
		go func() { p.running.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
		}
		return rpc.Then{Result: nil, Do: p.exit}, nil
	}
	return nil, rpc.MethodNotFound(method)
}

// initialize takes what ws says of where the plugin is running, gets it
// ready (its config-dependent declarations, its database, its HTTP), and
// answers with its registration.
func (p *Plugin) initialize(in protocol.InitializeParams) (any, error) {
	p.mu.Lock()
	if p.info.Protocol != 0 {
		p.mu.Unlock()
		return nil, errors.New("initialize was already sent")
	}
	chosen := protocol.Choose(protocol.Versions, in.Offered())
	if chosen == 0 {
		// ws refuses the plugin, and says which side is behind: it's
		// told which versions the plugin speaks, and nothing more.
		p.info = in
		p.mu.Unlock()
		close(p.initialized)
		p.Log.Error(fmt.Sprintf("ws speaks versions %v of the plugin protocol, and this plugin %v: none in common", in.Offered(), protocol.Versions))
		return protocol.InitializeResult{Protocols: protocol.Versions}, nil
	}
	in.Protocol = chosen // what everything after this is in
	p.info = in
	p.mu.Unlock()
	defer close(p.initialized)
	p.Config.set(in.Config)

	reg, err := p.prepare()
	if err != nil {
		return nil, err
	}
	// The database, if the plugin has migrations. One that can't be
	// brought forward stops everything but the registration: commands and
	// the plugin's handlers say why.
	if p.hasMigrations() {
		if _, err := p.openDB(); err != nil {
			p.broken = err
			p.Log.Error(err.Error())
		}
	}
	if p.handler != nil && in.View.Listen != "" {
		if err := p.listen(in.View.Listen); err != nil {
			return nil, fmt.Errorf("listening on %s: %w", in.View.Listen, err)
		}
	}
	return rpc.Then{
		Result: protocol.InitializeResult{Protocol: chosen, Protocols: protocol.Versions, Registration: reg},
		Do:     p.startSyncs, // once ws has the registration
	}, nil
}

// Registration is what the plugin registers, as declared so far. Run
// answers initialize with it; a test or --registration can ask for it
// without a connection.
func (p *Plugin) Registration() (protocol.Registration, error) { return p.prepare() }

// prepare runs the Setups and builds what the declarations need (the
// plugin's HTTP), then closes declaring. It's run once.
func (p *Plugin) prepare() (protocol.Registration, error) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		if len(p.declErrs) > 0 {
			return protocol.Registration{}, errors.Join(p.declErrs...)
		}
		return p.registration(), nil
	}
	p.started, p.inSetup = true, true
	p.mu.Unlock()
	for _, fn := range p.setups {
		fn()
	}
	p.mu.Lock()
	p.inSetup = false
	p.mu.Unlock()

	if !p.guideSet && p.files != nil {
		if data, err := fs.ReadFile(p.files, "agents.md"); err == nil {
			p.guide = string(data)
		}
	}
	if err := p.buildView(); err != nil {
		p.declErrs = append(p.declErrs, err)
	}
	p.addSyncCommand()
	if _, errs := p.refsDeclared(); len(errs) > 0 {
		p.declErrs = append(p.declErrs, errs...)
	}
	if _, err := p.actionsDeclared(); err != nil {
		p.declErrs = append(p.declErrs, err)
	}
	if _, err := p.proposalsDeclared(); err != nil {
		p.declErrs = append(p.declErrs, err)
	}
	if _, err := p.notifiesDeclared(); err != nil {
		p.declErrs = append(p.declErrs, err)
	}
	if len(p.declErrs) > 0 {
		return protocol.Registration{}, errors.Join(p.declErrs...)
	}
	return p.registration(), nil
}

// registration is what's been declared, as ws is told it.
func (p *Plugin) registration() protocol.Registration {
	reg := protocol.Registration{Description: p.description, ConfigVersion: p.Config.version}
	for _, v := range p.views {
		reg.Views = append(reg.Views, protocol.View{Title: v.title, Icon: v.icon, Under: v.under, Path: v.path})
	}
	reg.UI = p.ui
	if len(p.commands) > 0 {
		cmds := &protocol.Commands{Name: p.info.Plugin.Name, Short: p.description}
		for _, c := range p.commands {
			sub := protocol.Subcommand{Name: c.name, Short: c.short, Args: c.usage(), Conversations: c.conversations, Label: c.label}
			for _, f := range c.flags {
				if f.takesValue {
					sub.Options = append(sub.Options, f.name)
				}
			}
			cmds.Subcommands = append(cmds.Subcommands, sub)
		}
		reg.Commands = cmds
	}
	if p.guide != "" {
		reg.Agent = &protocol.Agent{Roles: p.roles, Guide: p.guide}
	}
	for _, s := range p.syncs {
		reg.Syncs = append(reg.Syncs, protocol.Sync{Name: s.name, Every: int(s.every / time.Second), Open: int(s.open / time.Second)})
	}
	reg.Refs, _ = p.refsDeclared()
	reg.Actions, _ = p.actionsDeclared()
	reg.Proposes, _ = p.proposalsDeclared()
	reg.Notifies, _ = p.notifiesDeclared()
	reg.Config = p.Config.spec()
	return reg
}

// configChanged takes the config after a change. If a key that a Setup
// read has changed, what was registered may no longer hold, so the plugin
// exits to be started afresh; otherwise it carries on with the new values.
func (p *Plugin) configChanged(values map[string]json.RawMessage) {
	restart := p.Config.set(values)
	if restart {
		p.Log.Info("config that the registration depends on changed: exiting to start afresh")
		p.exit()
		return
	}
	for _, fn := range p.onConfig {
		p.guard("OnConfig", fn)
	}
	p.wakeSyncs()
	// What the app shows may well follow config: an open one looks again.
	p.Changed()
}

// guard runs fn, turning a panic into a logged error.
func (p *Plugin) guard(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			p.Log.Error(fmt.Sprintf("%s panicked: %v", what, r))
		}
	}()
	fn()
}

// recovering runs fn, turning a panic into an error.
func (p *Plugin) recovering(what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panicked: %v", what, r)
		}
	}()
	return fn()
}

// notify sends ws a notification, if there's a ws to send it to.
func (p *Plugin) notify(method string, params any) {
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn != nil {
		conn.Notify(method, params) //nolint:errcheck // ws has gone: nothing to tell
	}
}

// Changed tells ws that what the app shows is stale. ws tells the open
// app (ws:changed: onChanged in @withworkspace/ui-lib), which fetches
// what it shows again.
func (p *Plugin) Changed() { p.notify(protocol.MethodViewChanged, struct{}{}) }

// Badge sets the count beside a view's sidebar entry: the view whose
// entry opens at path. 0 removes it.
func (p *Plugin) Badge(path string, n int) {
	p.notify(protocol.MethodViewBadge, protocol.ViewBadgeParams{Path: path, Count: n})
}

// Name is the plugin's name, as ws knows it. Like Dir, Data and Root,
// it's known once the plugin is running.
func (p *Plugin) Name() string { return p.where().Plugin.Name }

// Dir is the plugin's own folder: where its plugin.toml is.
func (p *Plugin) Dir() string { return p.where().Plugin.Dir }

// Data is the folder for whatever the plugin keeps. ws never reads it.
func (p *Plugin) Data() string { return p.where().Plugin.Data }

// Root is the workspace's folder.
func (p *Plugin) Root() string { return p.where().Workspace.Root }

func (p *Plugin) where() protocol.InitializeParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}
