// Package protocol is the plugin protocol's messages, as Go types: what ws
// and a plugin send each other over the plugin's stdin and stdout. See
// PROTOCOL.md at the root of this repository.
//
// The types, constants and method names are protocol.gen.go, generated from
// the protocol's spec in github.com/withworkspace/protocol (plugin/spec):
// change them there, and bring the file here with just protocol. What's
// here is the rules Go holds them to.
package protocol

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Versions are every version of the protocol this package speaks, oldest
// first. Within a version, changes only add; anything else is a new
// version, with types of its own.
var Versions = []int{1}

// Choose is the version a handshake settles on: the highest that both
// sides speak, or 0 when they have none in common.
func Choose(a, b []int) int {
	chosen := 0
	for _, v := range a {
		if v > chosen && slices.Contains(b, v) {
			chosen = v
		}
	}
	return chosen
}

// Offered are the versions ws speaks, from what it said. A ws that says
// nothing of it speaks 1.
func (in InitializeParams) Offered() []int {
	switch {
	case len(in.Protocols) > 0:
		return in.Protocols
	case in.Protocol > 0:
		return []int{in.Protocol}
	}
	return []int{1}
}

// CommandName is the word after ws that runs the plugin's commands: its
// own, or the plugin's name. "" if it adds none.
func (r Registration) CommandName(plugin string) string {
	if r.Commands == nil {
		return ""
	}
	if r.Commands.Name != "" {
		return r.Commands.Name
	}
	return plugin
}

// Named is the field of that name, if the kind has it.
func (r Ref) Named(name string) (Field, bool) {
	for _, f := range r.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Typed is the name of the kind's first field of a type: "" for none.
func (r Ref) Typed(fieldType string) string {
	for _, f := range r.Fields {
		if f.Type == fieldType {
			return f.Name
		}
	}
	return ""
}

var (
	refKind   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	refPrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,30}-$`)
	refField  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

var tailNumber = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// Names says whether name is one of this kind's: one of its prefixes and
// its tail, whole and as written. A kind whose tail this release doesn't
// know names nothing.
func (r Ref) Names(name string) bool {
	if r.Tail != "" && r.Tail != TailNumber {
		return false
	}
	for _, p := range r.Prefixes {
		if tail, ok := strings.CutPrefix(name, p); ok && tailNumber.MatchString(tail) {
			return true
		}
	}
	return false
}

// Problem says what's wrong with a ref as it's registered, or "": ws
// leaves out one with a problem, and the framework won't declare it.
func (r Ref) Problem() string {
	switch {
	case !refKind.MatchString(r.Kind):
		return fmt.Sprintf("its kind %q must be lower-case letters, digits and hyphens, starting with a letter", r.Kind)
	case len(r.Prefixes) == 0:
		return "it has no prefixes: nothing would be named by it"
	}
	for _, p := range r.Prefixes {
		if !refPrefix.MatchString(p) {
			return fmt.Sprintf("its prefix %q must be letters, digits and underscores, starting with a letter and ending in a hyphen (RAM-)", p)
		}
	}
	if r.Address != "" {
		u, err := url.Parse(strings.Replace(r.Address, RefPlaceholder, "X-1", 1))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Count(r.Address, RefPlaceholder) != 1 {
			return fmt.Sprintf("its address %q must be an http or https address with %s once, where the name goes", r.Address, RefPlaceholder)
		}
		if strings.Contains(u.Host, "X-1") || strings.ContainsAny(r.Address, " \t\n") {
			return fmt.Sprintf("its address %q must have %s after its host", r.Address, RefPlaceholder)
		}
	}
	if r.Path != "" && (!strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") || strings.Count(r.Path, RefPlaceholder) != 1) {
		return fmt.Sprintf("its path %q must be a path in the plugin's app (/issue/%s) with %s once", r.Path, RefPlaceholder, RefPlaceholder)
	}
	// What its things are made of, found by and drawn from.
	has := map[string]bool{FieldKey: true}
	titles, states := 0, 0
	for _, f := range r.Fields {
		switch {
		case f.Name == FieldKey:
			return fmt.Sprintf("its field %q is every thing's own: its name", FieldKey)
		case !refField.MatchString(f.Name):
			return fmt.Sprintf("its field %q must be lower-case letters, digits and underscores, starting with a letter", f.Name)
		case has[f.Name]:
			return fmt.Sprintf("its field %q is there twice", f.Name)
		}
		has[f.Name] = true
		switch f.Type {
		case FieldTitle:
			titles++
		case FieldState:
			states++
		case "":
			return fmt.Sprintf("its field %q has no type", f.Name)
		}
	}
	switch {
	case len(r.Fields) > 0 && titles != 1:
		return fmt.Sprintf("it has %d fields of type %s: a thing has one thing it's called", titles, FieldTitle)
	case states > 1:
		return fmt.Sprintf("it has %d fields of type %s: a thing stands in one place", states, FieldState)
	}
	for _, s := range r.Search {
		if !has[s.Field] || s.Max < 0 {
			return fmt.Sprintf("it's found by %q, which isn't one of its fields", s.Field)
		}
	}
	for where, blocks := range r.Views {
		for _, b := range blocks {
			if !has[b.Field] {
				return fmt.Sprintf("its view %s shows %q, which isn't one of its fields", where, b.Field)
			}
		}
	}
	if r.Remote != nil {
		if problem := r.Remote.Problem(); problem != "" {
			return problem
		}
		if titles != 1 {
			return "it has a remote and no title field: one in a repo is called what its " + ManifestName + " says"
		}
	}
	return ActionsProblem(r.Actions)
}

var (
	remotePath = regexp.MustCompile(`^([A-Za-z0-9._-]+/)*[A-Za-z0-9._-]*\{slug\}[A-Za-z0-9._-]*/$`)
	remoteMain = regexp.MustCompile(`^[A-Za-z0-9._-]*\{slug\}[A-Za-z0-9._-]*\.[A-Za-z0-9]+$`)
)

// Problem says what's wrong with a remote as it's registered, or "".
func (r Remote) Problem() string {
	switch {
	case !remotePath.MatchString(r.Path) || strings.Contains(r.Path, ".."):
		return fmt.Sprintf("its remote's path %q must be a folder in a repo, ending in /, with %s in its last part (sketches/%s/)", r.Path, RemoteSlug, RemoteSlug)
	case !remoteMain.MatchString(r.Main) || strings.Contains(r.Main, ".."):
		return fmt.Sprintf("its remote's main file %q must be a file's name with %s in it and a type after (%s.excalidraw)", r.Main, RemoteSlug, RemoteSlug)
	}
	// A main file that some slug would name as the manifest is.
	before, after, _ := strings.Cut(strings.ToLower(r.Main), RemoteSlug)
	if len(before)+len(after) < len(ManifestName) && strings.HasPrefix(ManifestName, before) && strings.HasSuffix(ManifestName, after) {
		return fmt.Sprintf("its remote's main file %q could be %s, ws's own file in the folder", r.Main, ManifestName)
	}
	return ""
}

var actionID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// ActionsProblem says what's wrong with a list of actions as registered,
// or "".
func ActionsProblem(actions []Action) string {
	seen := map[string]bool{}
	for _, a := range actions {
		switch {
		case !actionID.MatchString(a.ID):
			return fmt.Sprintf("its action %q must be lower-case letters, digits and hyphens, starting with a letter", a.ID)
		case seen[a.ID]:
			return fmt.Sprintf("its action %q is there twice", a.ID)
		case strings.TrimSpace(a.Title) == "":
			return fmt.Sprintf("its action %q has no title: it's what Find calls it", a.ID)
		}
		seen[a.ID] = true
	}
	return ""
}

// Secret says whether the key's value is a secret: kept apart from
// config's files, and always local.
func (k ConfigKey) Secret() bool { return k.Type == TypeSecret }

var remoteFile = regexp.MustCompile(`^[A-Za-z0-9._-]*(\{slug\})?[A-Za-z0-9._-]*$`)

// Problem says what's wrong with a package as it's given, or "".
func (p RemotePackage) Problem() string {
	if strings.TrimSpace(p.Title) == "" {
		return "it has no title: a thing in a repo is called something"
	}
	pictures := 0
	seen := map[string]bool{}
	for _, f := range p.Files {
		switch {
		case f.Name == "" || f.Name == "." || f.Name == ".." || !remoteFile.MatchString(f.Name) || strings.HasPrefix(f.Name, "."):
			return fmt.Sprintf("its file %q must be a name alone, of letters, digits, dots, hyphens and underscores, with %s where the slug goes", f.Name, RemoteSlug)
		case strings.EqualFold(f.Name, ManifestName):
			return fmt.Sprintf("its file %q is ws's own file in the folder", f.Name)
		case seen[f.Name]:
			return fmt.Sprintf("its file %q is there twice", f.Name)
		}
		seen[f.Name] = true
		if f.Picture {
			pictures++
		}
	}
	if pictures > 1 {
		return "it has more than one picture"
	}
	return ""
}

// ProposesProblem says what's wrong with a list of proposes as
// registered, or "".
func ProposesProblem(list []Proposes) string {
	seen := map[string]bool{}
	for _, p := range list {
		switch {
		case !actionID.MatchString(p.ID):
			return fmt.Sprintf("what it proposes, %q, must be lower-case letters, digits and hyphens, starting with a letter", p.ID)
		case seen[p.ID]:
			return fmt.Sprintf("it proposes %q twice", p.ID)
		case strings.TrimSpace(p.Title) == "":
			return fmt.Sprintf("what it proposes, %q, has no title: it's what the card calls it", p.ID)
		}
		seen[p.ID] = true
	}
	return ""
}

// NotifiesProblem says what's wrong with a list of notification kinds as
// registered, or "".
func NotifiesProblem(list []Notifies) string {
	seen := map[string]bool{}
	for _, n := range list {
		switch {
		case !actionID.MatchString(n.ID):
			return fmt.Sprintf("a kind of notification, %q, must be lower-case letters, digits and hyphens, starting with a letter", n.ID)
		case seen[n.ID]:
			return fmt.Sprintf("it notifies %q twice", n.ID)
		case strings.TrimSpace(n.Title) == "":
			return fmt.Sprintf("a kind of notification, %q, has no title: it's what Settings calls it", n.ID)
		case n.Default != "" && n.Default != RouteFeed && n.Default != RouteOff:
			return fmt.Sprintf("a kind of notification, %q, is sent to %q by default: it's feed or off (the desktop is the person's to choose)", n.ID, n.Default)
		}
		seen[n.ID] = true
	}
	return ""
}

// NotifiesKind is the kind registered by id, or ok false.
func NotifiesKind(list []Notifies, id string) (Notifies, bool) {
	for _, n := range list {
		if n.ID == id {
			return n, true
		}
	}
	return Notifies{}, false
}
