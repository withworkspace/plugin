package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/withworkspace/plugin/internal/rpc"
	"github.com/withworkspace/plugin/protocol"
	"github.com/withworkspace/plugin/ui"
)

// A plugin's things have names people and agents write: RAM-123. A plugin
// says how each kind is named, what one is made of, what it's found by and
// how it's drawn (Ref), all of it data, and ws does the rest: it finds the
// names in what's written (a brief, a conversation, a document), links
// them, keeps the things the plugin tells it of, finds those by what they
// say, and draws one under its name and beside the list in Find.

// Ref is a kind of thing the plugin has: how one is named, what it's
// made of (Fields), what it's found by (Search), how it's drawn (Card,
// Pane), and what the plugin tells ws of the ones it has (Mirror, Put,
// Remove).
type Ref struct {
	p    *Plugin
	ref  protocol.Ref
	show func(ctx context.Context, a Asked) (Thing, error)
	acts map[string]func(*Act) error // what does each of its Actions
	read func(ctx context.Context, f RemoteFile) (map[string]string, error)
	// Its things in a repo: giving one to ws, and hearing it's there.
	export func(ctx context.Context, e Exporting) (Exported, error)
	saved  func(ctx context.Context, name string, thing RemoteThing) error
}

// Asked is one thing ws asks about, by the name it was written as.
type Asked struct {
	// Name is the thing's name: RAM-123.
	Name string
	// Full: everything was asked for (ws show --full). Leave nothing out
	// of what it says; without it, keep to what's worth reading first.
	Full bool
}

// Thing is one of the plugin's things: its name, and a value for each of
// its kind's fields.
type Thing struct {
	// Name is its name, one of the Ref's: RAM-123.
	Name string
	// Changed is when it last changed, if that's known.
	Changed time.Time
	// Standing is where it stands, in words every kind shares: one of
	// protocol's Standings (protocol.StandingSettled for an issue that's
	// done, protocol.StandingGone for one closed as won't do). What's found
	// of a thing that's settled, or no longer so, is found below what's
	// still so, the more so the longer since. None is open.
	Standing string
	// Since is when it came to stand so, for one that's settled or gone (an
	// issue's resolution date). None is from when it last changed.
	Since time.Time
	// Fields are its values, by its kind's fields' names (Fields). Each
	// is a string: a time is RFC 3339 (Stamp). One with nothing for its
	// kind's title isn't a thing ws keeps.
	Fields map[string]string
}

// Stamp is a time as a Time field has it.
func Stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Ref declares a kind of thing the plugin has that words name. kind is
// the plugin's word for it (issue), title what one is called (Jira
// issue). The rest is said after, each a call on what this returns.
//
// A name is a prefix and a number: RAM-123. What a plugin registers
// usually follows its config (which projects, which site), so it's
// declared in Setup:
//
//	p.Setup(func() {
//		issue = p.Ref("issue", "Jira issue").
//			Prefix("RAM-", "RAP-").
//			Address(site.Get() + "/browse/{ref}").
//			Opens("/issue/{ref}").
//			Fields(plugin.Title("title"), plugin.State("status"),
//				plugin.Text("assignee", "Assignee"), plugin.Markdown("body")).
//			Search(plugin.All("title"), plugin.Upto("body", 10000)).
//			Card(ui.Title("title"), ui.Subtitle(ui.Key), ui.Badge("status")).
//			Pane(ui.Title("title"), ui.Subtitle(ui.Key), ui.Badge("status"),
//				ui.Field("assignee"), ui.Markdown("body")).
//			Show(fetchIssue)
//	})
func (p *Plugin) Ref(kind, title string) *Ref {
	p.declaring("Ref")
	for _, was := range p.refs {
		if was.ref.Kind == kind {
			p.declErrs = append(p.declErrs, fmt.Errorf("the ref %q is declared twice", kind))
		}
	}
	r := &Ref{p: p, ref: protocol.Ref{Kind: kind, Title: title}}
	p.refs = append(p.refs, r)
	return r
}

// Prefix is what a name of this kind starts with: letters, digits and
// underscores, starting with a letter and ending in a hyphen (RAM-). A
// name is one of them and a number, as written: ram-12 isn't RAM-12.
func (r *Ref) Prefix(prefixes ...string) *Ref {
	r.ref.Prefixes = append(r.ref.Prefixes, prefixes...)
	return r
}

// Address is where one is outside ws, with {ref} where its name goes:
// https://acme.atlassian.net/browse/{ref}. ws links a name to it in a
// document, and reads one pasted as the thing it names.
func (r *Ref) Address(template string) *Ref {
	r.ref.Address = template
	return r
}

// Opens is where one opens in the plugin's app, with {ref} where its name
// goes: /issue/{ref}. Without it, a name goes to its address.
func (r *Ref) Opens(path string) *Ref {
	r.ref.Path = path
	return r
}

// Icon is the kind's icon: one of ws's icon names.
func (r *Ref) Icon(name string) *Ref {
	r.ref.Icon = name
	return r
}

// A field of a kind's things, as Fields takes them.
type FieldSpec = protocol.Field

func field(name, kind string, label []string) FieldSpec {
	return FieldSpec{Name: name, Type: kind, Label: strings.Join(label, " ")}
}

// Title is the field a thing's called by: a line. A kind has one, and a
// thing with nothing for it isn't one ws keeps.
func Title(name string) FieldSpec { return field(name, protocol.FieldTitle, nil) }

// State is the field that says where a thing stands, in the plugin's own
// words (In Progress): a kind has one at most. label is what it's called
// where it's drawn with one.
func State(name string, label ...string) FieldSpec { return field(name, protocol.FieldState, label) }

// Text is a field of words: a person's name, a priority, a number.
func Text(name string, label ...string) FieldSpec { return field(name, protocol.FieldText, label) }

// Time is a field that's a moment: its value is Stamp of it.
func Time(name string, label ...string) FieldSpec { return field(name, protocol.FieldTime, label) }

// Markdown is a field that says something at length: a description, what's
// been said on the thing.
func Markdown(name string, label ...string) FieldSpec {
	return field(name, protocol.FieldMarkdown, label)
}

// Names is a field that names another thing (its parent, the issue it
// blocks): its value is that thing's name. ws links the two in its graph
// and draws the name as what it names. how is how it points, one of
// protocol's Links (protocol.LinkIn for a parent): it names it, with none.
func Names(name, label string, how ...string) FieldSpec {
	return FieldSpec{Name: name, Type: protocol.FieldRef, Label: label, Link: strings.Join(how, "")}
}

// NamesSeveral is a field that names other things (its children, what it
// links to): its value is their names, separated by spaces, commas or
// lines.
func NamesSeveral(name, label string, how ...string) FieldSpec {
	return FieldSpec{Name: name, Type: protocol.FieldRefs, Label: label, Link: strings.Join(how, "")}
}

// Fields are what one of this kind's things is made of: what Mirror, Put
// and Show give a value for. Every thing has ui.Key besides, its name.
func (r *Ref) Fields(fields ...FieldSpec) *Ref {
	r.ref.Fields = append(r.ref.Fields, fields...)
	return r
}

// Searched is a field a thing is found by, as Search takes them.
type Searched = protocol.Searched

// All is the whole of a field, as far as ws keeps it.
func All(field string) Searched { return Searched{Field: field} }

// Upto is the start of a field: its first max bytes.
func Upto(field string, max int) Searched { return Searched{Field: field, Max: max} }

// Search is what one of this kind's things is found by, in order: ws
// makes the words it looks through from these, and makes them again when
// they change, without the plugin telling it anything twice. Without it,
// a thing's found by every field it has.
func (r *Ref) Search(by ...Searched) *Ref {
	r.ref.Search = append(r.ref.Search, by...)
	return r
}

// Card is how one is drawn under its name, where it's written (on hover):
// a few lines.
func (r *Ref) Card(blocks ...ui.Block) *Ref { return r.view(protocol.ViewCard, blocks) }

// Pane is how one is drawn beside the list in Find, for the row the
// cursor's on: there's room for what it says.
func (r *Ref) Pane(blocks ...ui.Block) *Ref { return r.view(protocol.ViewPane, blocks) }

func (r *Ref) view(where string, blocks []ui.Block) *Ref {
	if r.ref.Views == nil {
		r.ref.Views = map[string][]protocol.Block{}
	}
	wire := make([]protocol.Block, 0, len(blocks))
	for _, b := range blocks {
		wire = append(wire, b.Wire())
	}
	r.ref.Views[where] = wire
	return r
}

// Action declares something that can be done to one of this kind's
// things, and is what does it: Find offers it for the thing the cursor's
// on, and fn is run with the thing's name (Act.Name). See Plugin.Action.
//
//	issue.Action("move", "Move to…", func(a *plugin.Act) error {
//		to, err := a.Pick("Move "+a.Name+" to", plugin.Choices("To Do", "In Progress", "Done")...)
//		if err != nil {
//			return err
//		}
//		…
//		return a.Say(a.Name + " is " + to)
//	})
func (r *Ref) Action(id, title string, fn func(a *Act) error) *Ref {
	r.ref.Actions = append(r.ref.Actions, protocol.Action{ID: id, Title: title})
	if r.acts == nil {
		r.acts = map[string]func(*Act) error{}
	}
	r.acts[id] = fn
	return r
}

// Show is what answers for one thing, now: fn is given a name ws found,
// and says what the plugin has by it, or ErrNotFound for nothing. ws asks
// for a thing it hasn't been told of (one off the board), and when what
// it's been told isn't enough: ws show RAM-123, which wants it as it is
// now, and in full. A kind with no Show is shown as ws was last told.
func (r *Ref) Show(fn func(ctx context.Context, a Asked) (Thing, error)) *Ref {
	r.show = fn
	return r
}

// Remote is where a kind's things live beyond this machine, as Ref.Remote
// takes it: a folder of a repo each (its package), for everyone with the
// repo. The folder holds the thing's own file (its main file), a
// thing.toml that says what it is, and what the plugin makes from the
// main file. ws finds the packages in the workspace's repos, names each
// thing by its folder (records/sketches/release-flow/) and calls it what
// its thing.toml says. The plugin mints no name for one and needn't keep
// one: Remotes and OpenRemote ask ws.
type Remote struct {
	// Path is the folder a new one goes in, in its repo, with {slug}
	// where the thing's slug goes: sketches/{slug}/. One that's there is
	// found wherever its folder is.
	Path string
	// Main is the main file's name in that folder: {slug}.excalidraw.
	Main string
	// Read says what a main file is found by: a value for each of the
	// kind's fields that the file has one for (the words on a sketch). Its
	// title is its thing.toml's. ws asks once for each version of a file,
	// so what's said is of the file alone. Without Read, one is found by
	// its title.
	Read func(ctx context.Context, f RemoteFile) (map[string]string, error)
	// Export gives one of the plugin's own things as its folder in a repo
	// is to hold it: ws asks when the person promotes it, or saves it. It
	// says where the thing lives, if it's in a repo already, and the
	// version there that what it gives was made from. Where the thing has
	// changed in its repo since that version, ws asks again with what's
	// there now (Exporting.Current): the plugin brings that into what it
	// has, however its kind merges, and gives the two together. Without
	// Export, ws puts none of the kind's things in a repo.
	Export func(ctx context.Context, e Exporting) (Exported, error)
	// Saved is told that one of the plugin's things is in its repo, as it
	// was last exported: name is the plugin's own for it, and thing what's
	// there, whose Version is what the next export is made from.
	Saved func(ctx context.Context, name string, thing RemoteThing) error
}

// Exporting is one of the plugin's own things ws asks for, to put it in a
// repo.
type Exporting struct {
	// Name is the plugin's name for it: sketch-12.
	Name string
	// Current is the thing as it is in its repo now, and Content its main
	// file there, where that's changed since the plugin last had it: nil
	// where it hasn't, and for one that isn't in a repo.
	Current *RemoteThing
	Content []byte
}

// Exported is a thing as Export gives it.
type Exported struct {
	// Remote is its name in its repo (a RemoteThing's Name), where it's in
	// one already: "" for one that isn't.
	Remote string
	// Base is the version there that the package was made from: the
	// RemoteThing's Version, as the plugin last had it. "" for one that
	// isn't in a repo.
	Base string
	Package
}

// RemoteFile is a thing's main file, as ws read it from its repo.
type RemoteFile struct {
	// Name is the thing's name in ws: its folder.
	Name string
	// Title is what its thing.toml calls it.
	Title string
	// File is the main file's name, and Content what's in it.
	File    string
	Content []byte
}

// RemoteThing is one of a kind's things that lives in a repo, as ws
// found it: its name, where it is, what it's called, its files' names,
// and its version, which is its folder's.
type RemoteThing = protocol.RemoteThing

// Remote says this kind's things can live in a repo, and how: see Remote.
// The kind needs a title field, which one in a repo has from its
// thing.toml.
//
//	sketch = p.Ref("sketch", "Sketch").
//		Prefix("sketch-").
//		Fields(plugin.Title("title"), plugin.Markdown("text", "On it")).
//		Remote(plugin.Remote{
//			Path: "sketches/{slug}/",
//			Main: "{slug}.excalidraw",
//			Read: func(ctx context.Context, f plugin.RemoteFile) (map[string]string, error) {
//				return map[string]string{"text": wordsOn(f.Content)}, nil
//			},
//		})
func (r *Ref) Remote(remote Remote) *Ref {
	r.ref.Remote = &protocol.Remote{Path: remote.Path, Main: remote.Main, Reads: remote.Read != nil, Exports: remote.Export != nil}
	r.read, r.export, r.saved = remote.Read, remote.Export, remote.Saved
	return r
}

// Remotes are the kind's things that live in the workspace's repos, as ws
// found them, in order of name. A ws from before remotes is
// ErrUnsupported.
func (r *Ref) Remotes(ctx context.Context) ([]RemoteThing, error) {
	var out protocol.RemoteListResult
	if err := r.p.ask(ctx, protocol.MethodRemoteList, protocol.RemoteListParams{Kind: r.ref.Kind}, &out); err != nil {
		return nil, err
	}
	return out.Things, nil
}

// OpenRemote is one of the kind's things that lives in a repo, by its
// name, and its main file as it is on its repo's default branch. One ws
// has nothing by that name for is ErrNotFound.
func (r *Ref) OpenRemote(ctx context.Context, name string) (RemoteThing, []byte, error) {
	var out protocol.RemoteOpened
	if err := r.p.ask(ctx, protocol.MethodRemoteOpen, protocol.RemoteOpenParams{Kind: r.ref.Kind, Name: name}, &out); err != nil {
		return RemoteThing{}, nil, err
	}
	return out.RemoteThing, out.Content, nil
}

// Package is a thing as its folder in a repo is to hold it: what it's
// called, its main file, and what the plugin makes from the main file.
type Package struct {
	Title string
	// Main is the main file's content.
	Main []byte
	// Files are what's made from it: a picture of it.
	Files []PackageFile
}

// PackageFile is a file made from a thing's main file, by its name in
// the folder, with {slug} where the thing's slug goes: {slug}.svg.
// Picture: it's the thing's picture.
type PackageFile = protocol.RemoteFile

// RemoteSaved is what promoting or saving a thing did: the thing as it
// is in its repo now, whether anything changed there, and the capsule it
// was left in where its repo takes pull requests. See
// protocol.RemoteSaved.
type RemoteSaved = protocol.RemoteSaved

func (pkg Package) wire() protocol.RemotePackage {
	return protocol.RemotePackage{Title: pkg.Title, Main: pkg.Main, Files: pkg.Files}
}

// PromoteTo is the folder the person chose for a thing, as Promote takes
// it (repo:folder/): none is where the kind's things go.
type PromoteTo string

// Promote has ws put one of the plugin's own things in a repo, for the
// person who asked the plugin to: name is the plugin's name for it. ws
// asks for the thing (the kind's Export), names its folder from the
// kind's Remote and the thing's title, writes it with a thing.toml,
// commits it as the person and lands it as its repo lands, and tells the
// plugin (the kind's Saved). What comes back says where it is now. A ws
// from before it is ErrUnsupported.
func (r *Ref) Promote(ctx context.Context, name string, to ...PromoteTo) (RemoteSaved, error) {
	in := protocol.RemotePromoteParams{Kind: r.ref.Kind, Ref: name}
	for _, t := range to {
		in.To = string(t)
	}
	var out RemoteSaved
	err := r.p.ask(ctx, protocol.MethodRemotePromote, in, &out)
	return out, err
}

// SaveRemote has ws save one of the plugin's own things that's in a repo,
// as the person has it now: name is the plugin's name for it. ws asks
// for the thing, and where it's changed in its repo since, asks again
// with what's there, for the plugin to bring the two together. One the
// plugin says isn't in a repo is refused.
func (r *Ref) SaveRemote(ctx context.Context, name string) (RemoteSaved, error) {
	var out RemoteSaved
	err := r.p.ask(ctx, protocol.MethodRemoteSave, protocol.RemoteSaveParams{Kind: r.ref.Kind, Ref: name}, &out)
	return out, err
}

// remoteRef is the ref of that kind, for one of ws's requests about its
// things in a repo.
func (p *Plugin) remoteRef(kind string) (*Ref, error) {
	for _, r := range p.refs {
		if r.ref.Kind == kind {
			return r, nil
		}
	}
	return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: fmt.Sprintf("this plugin has no ref called %q", kind)}
}

// exportRemote answers ws asking for one of the plugin's things as its
// folder in a repo is to hold it.
func (p *Plugin) exportRemote(ctx context.Context, in protocol.RemoteExportParams) (any, error) {
	ref, err := p.remoteRef(in.Kind)
	if err != nil {
		return nil, err
	}
	if ref.export == nil {
		return nil, rpc.MethodNotFound(protocol.MethodRemoteExport)
	}
	if p.broken != nil {
		return nil, p.broken
	}
	ctx, cancel := context.WithTimeout(ctx, p.CommandTimeout)
	defer cancel()
	asked := Exporting{Name: in.Ref}
	if in.Current != nil {
		asked.Current, asked.Content = &in.Current.RemoteThing, in.Current.Content
	}
	var out Exported
	err = p.recovering("exporting "+in.Ref, func() (err error) {
		out, err = ref.export(ctx, asked)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: fmt.Sprintf("there's no %s %s", in.Kind, in.Ref)}
	case err != nil:
		return nil, err
	}
	return protocol.RemoteExported{Name: out.Remote, Base: out.Base, RemotePackage: out.Package.wire()}, nil
}

// savedRemote hears from ws that one of the plugin's things is in a repo.
func (p *Plugin) savedRemote(ctx context.Context, in protocol.RemoteSavedParams) (any, error) {
	ref, err := p.remoteRef(in.Kind)
	if err != nil {
		return nil, err
	}
	if ref.saved == nil {
		return struct{}{}, nil // nothing it keeps of it
	}
	if p.broken != nil {
		return nil, p.broken
	}
	ctx, cancel := context.WithTimeout(ctx, p.CommandTimeout)
	defer cancel()
	err = p.recovering("hearing "+in.Ref+" is saved", func() error { return ref.saved(ctx, in.Ref, in.Thing) })
	if err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// readRemote answers ws asking what a main file is found by.
func (p *Plugin) readRemote(ctx context.Context, in protocol.RemoteReadParams) (any, error) {
	var ref *Ref
	for _, r := range p.refs {
		if r.ref.Kind == in.Kind {
			ref = r
		}
	}
	if ref == nil {
		return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: fmt.Sprintf("this plugin has no ref called %q", in.Kind)}
	}
	if ref.read == nil {
		return nil, rpc.MethodNotFound(protocol.MethodRemoteRead)
	}
	if p.broken != nil {
		return nil, p.broken
	}
	ctx, cancel := context.WithTimeout(ctx, p.CommandTimeout)
	defer cancel()
	var fields map[string]string
	err := p.recovering("reading "+in.Name, func() (err error) {
		fields, err = ref.read(ctx, RemoteFile{Name: in.Name, Title: in.Title, File: in.File, Content: in.Content})
		return err
	})
	if err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]string{}
	}
	return protocol.RemoteReadResult{Fields: fields}, nil
}

// refsDeclared are the refs as ws is told them, and what's wrong with
// any of them.
func (p *Plugin) refsDeclared() ([]protocol.Ref, []error) {
	var out []protocol.Ref
	var errs []error
	for _, r := range p.refs {
		if problem := r.ref.Problem(); problem != "" {
			errs = append(errs, fmt.Errorf("the ref %q: %s", r.ref.Kind, problem))
			continue
		}
		out = append(out, r.ref)
	}
	return out, errs
}

// showRef answers ws asking what one of the plugin's things is.
func (p *Plugin) showRef(ctx context.Context, in protocol.RefShowParams) (any, error) {
	var ref *Ref
	for _, r := range p.refs {
		if r.ref.Kind == in.Kind {
			ref = r
		}
	}
	if ref == nil {
		return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: fmt.Sprintf("this plugin has no ref called %q", in.Kind)}
	}
	if ref.show == nil {
		// It says no more than it's told ws already.
		return nil, rpc.MethodNotFound(protocol.MethodRefShow)
	}
	if p.broken != nil {
		return nil, p.broken
	}
	ctx, cancel := context.WithTimeout(ctx, p.CommandTimeout)
	defer cancel()
	var thing Thing
	err := p.recovering("showing "+in.Ref, func() (err error) {
		thing, err = ref.show(ctx, Asked{Name: in.Ref, Full: in.Full})
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, &rpc.Error{Code: protocol.CodeNotFound, Message: fmt.Sprintf("there's no %s %s", in.Kind, in.Ref)}
	case err != nil:
		return nil, err
	}
	fields := thing.Fields
	if fields == nil {
		fields = map[string]string{}
	}
	return protocol.RefShowResult{Changed: thing.Changed, Fields: fields}, nil
}

// Refused is ws not keeping some of what it was told: each thing, and
// why. The others were kept.
type Refused struct {
	Things []protocol.ThingRefused
}

func (e *Refused) Error() string {
	said := make([]string, 0, len(e.Things))
	for _, t := range e.Things {
		said = append(said, t.Ref+": "+t.Why)
	}
	if len(said) > 3 {
		said = append(said[:3], fmt.Sprintf("and %d more", len(e.Things)-3))
	}
	return "ws didn't keep " + strings.Join(said, "; ")
}

// Mirror tells ws every thing of this kind the plugin has: each is kept
// as it's given, and what ws had of the kind that isn't among them is
// forgotten. It's what a sync calls when it's done: ws finds the plugin's
// things by what they say, and draws them, without asking the plugin.
// What ws wouldn't keep is a *Refused, after the rest is done.
//
//	p.Sync("issues", 5*time.Minute, func(ctx context.Context) error {
//		…
//		return issue.Mirror(ctx, things)
//	})
func (r *Ref) Mirror(ctx context.Context, things []Thing) error {
	var had protocol.ThingListResult
	if err := r.p.ask(ctx, protocol.MethodThingList, protocol.ThingListParams{Kind: r.ref.Kind}, &had); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, t := range things {
		have[t.Name] = true
	}
	refused := r.Put(ctx, things...)
	var no *Refused
	if refused != nil && !errors.As(refused, &no) {
		return refused
	}
	gone := slices.DeleteFunc(had.Refs, func(name string) bool { return have[name] })
	if err := r.Remove(ctx, gone...); err != nil {
		return err
	}
	return refused
}

// Put tells ws of things of this kind: each is kept, in place of what ws
// had by that name. What ws wouldn't keep is a *Refused.
func (r *Ref) Put(ctx context.Context, things ...Thing) error {
	var refused []protocol.ThingRefused
	for batch := range slices.Chunk(things, protocol.ThingsAtOnce) {
		in := protocol.ThingPutParams{Things: make([]protocol.Thing, 0, len(batch))}
		for _, t := range batch {
			fields := t.Fields
			if fields == nil {
				fields = map[string]string{}
			}
			in.Things = append(in.Things, protocol.Thing{Kind: r.ref.Kind, Ref: t.Name, Changed: t.Changed, Standing: t.Standing, Since: t.Since, Fields: fields})
		}
		var out protocol.ThingPutResult
		if err := r.p.ask(ctx, protocol.MethodThingPut, in, &out); err != nil {
			return err
		}
		refused = append(refused, out.Refused...)
	}
	if len(refused) > 0 {
		return &Refused{Things: refused}
	}
	return nil
}

// Remove has ws forget things of this kind, by name. One it doesn't have
// is nothing to forget.
func (r *Ref) Remove(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	return r.p.ask(ctx, protocol.MethodThingRemove, protocol.ThingRemoveParams{Kind: r.ref.Kind, Refs: names}, nil)
}
