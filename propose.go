package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/withworkspace/plugin/internal/rpc"
	"github.com/withworkspace/plugin/protocol"
)

// What a plugin proposes: a conversation's agent in ws writes something
// of the plugin's (issues to create), the plugin checks it and says what
// it'll do, ws shows the user that, under the agent's reply, and the
// plugin does it once they accept it, all of it or some of its items.
//
// The shape of what's written is the plugin's own: ws doesn't read it. It
// asks the plugin how one's written (Guide), sends it what the agent
// wrote (Draft), keeps what the plugin drafted, and sends each item's
// payload back as it was when the user accepts it (Accept). The plugin is
// trusted to do what the draft said, and only that.

// Proposal is something a conversation's agent may propose of the
// plugin's.
type Proposal struct {
	p      *Plugin
	decl   protocol.Proposes
	guide  func(context.Context) (string, error)
	draft  func(*Draft) error
	accept func(*Accepting) error
}

// Proposes declares something a conversation's agent may propose: its id
// (the agent names it as the plugin's name, a colon and the id:
// jira:issues) and what the card that asks the user calls it. Guide,
// Draft and Accept are what it's made of; When tells the agent when.
//
//	p.Proposes("issues", "Jira issues").
//		When("Issues to create, when the conversation settles on work for the board.").
//		Guide(func(ctx context.Context) (string, error) { return howToWrite(ctx) }).
//		Draft(func(d *plugin.Draft) error {
//			var in []newIssue
//			if err := d.Decode(&in); err != nil {
//				return err
//			}
//			for _, is := range in {
//				if is.Summary == "" {
//					return d.Refuse("every issue has a summary")
//				}
//				d.Item(is.Summary, is).Row("Project", is.Project).Markdown(is.Description)
//			}
//			return nil
//		}).
//		Accept(func(a *plugin.Accepting) error {
//			var is newIssue
//			if err := a.Decode(&is); err != nil {
//				return err
//			}
//			parent, err := a.Resolve(is.Parent) // "#1": what the first item made
//			if err != nil {
//				return err
//			}
//			key, err := create(a.Context(), is, parent)
//			if err != nil {
//				return err
//			}
//			a.Made(key)
//			return nil
//		})
func (p *Plugin) Proposes(id, title string) *Proposal {
	p.declaring("Proposes")
	r := &Proposal{p: p, decl: protocol.Proposes{ID: id, Title: title}}
	p.proposals = append(p.proposals, r)
	return r
}

// Icon is the card's icon: one of ws's icon names.
func (r *Proposal) Icon(name string) *Proposal {
	r.decl.Icon = name
	return r
}

// When is a line or two for the agent's first message: when to propose
// it. How it's written is Guide's, which the agent asks for first.
func (r *Proposal) When(text string) *Proposal {
	r.decl.When = strings.TrimSpace(text)
	return r
}

// Guide is how one's written, as it is now (the projects there are, the
// fields each asks for): what the agent's shown when it asks, before it
// writes one. Say the JSON it's written in, with an example.
func (r *Proposal) Guide(fn func(ctx context.Context) (string, error)) *Proposal {
	r.guide = fn
	return r
}

// Draft checks what the agent wrote and says what accepting it will do:
// an Item for each thing done. What's wrong with it is Refuse's, saying
// what to write instead: the agent reads it, and writes it again.
func (r *Proposal) Draft(fn func(d *Draft) error) *Proposal {
	r.draft = fn
	return r
}

// Accept does one item, as the user accepted it: run for each item
// accepted, in order. An error is the item's problem, which the user's
// shown; the item's left to accept again.
func (r *Proposal) Accept(fn func(a *Accepting) error) *Proposal {
	r.accept = fn
	return r
}

// Draft is the agent's proposal, being checked.
type Draft struct {
	// Conversation is the conversation it's proposed in (w3, or t3 in a
	// mission), and Mission the mission's slug, if it's a mission's.
	Conversation, Mission string

	ctx   context.Context
	input json.RawMessage
	title string
	items []protocol.ProposalItem
}

// refusal is what Refuse returns, and Decode for input that doesn't
// decode: the agent's told it.
type refusal struct{ said string }

func (r *refusal) Error() string { return r.said }

// Context ends when ws gives up on it.
func (d *Draft) Context() context.Context { return d.ctx }

// Input is what the agent wrote, as it wrote it.
func (d *Draft) Input() json.RawMessage { return d.input }

// Decode reads what the agent wrote into v, refusing anything v hasn't a
// field for, and anything that isn't JSON: what it returns is returned
// as it is.
func (d *Draft) Decode(v any) error {
	dec := json.NewDecoder(bytes.NewReader(d.input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return d.Refuse("what you wrote doesn't read as it should (%v): ask for the guide (propose it with no input) and write it as that says", err)
	}
	return nil
}

// Refuse says what's wrong with what the agent wrote, and what to write
// instead: return what it returns.
func (d *Draft) Refuse(format string, args ...any) error {
	return &refusal{said: fmt.Sprintf(format, args...)}
}

// Title is a line for the whole proposal, on its card: "3 issues in RAM".
// None: the item's title, for one, or how many there are.
func (d *Draft) Title(text string) { d.title = strings.TrimSpace(text) }

// Item is one thing accepting it does: what it's called, and payload,
// what Accept's given for it (Accepting.Decode), kept by ws as JSON
// meanwhile. Items are numbered from 1, in the order they're added, and
// an item can name an earlier one as #1, #2…
func (d *Draft) Item(title string, payload any) *Item {
	data, err := json.Marshal(payload)
	if err != nil {
		panic("plugin: Draft.Item: the payload isn't JSON: " + err.Error())
	}
	d.items = append(d.items, protocol.ProposalItem{Title: strings.TrimSpace(title), Payload: data})
	return &Item{d: d, n: len(d.items) - 1}
}

// Item is one thing a proposal does, as the user's shown it.
type Item struct {
	d *Draft
	n int
}

func (i *Item) it() *protocol.ProposalItem { return &i.d.items[i.n] }

// On is the thing it changes, by its name (RAM-12): none for one it
// makes.
func (i *Item) On(name string) *Item {
	i.it().On = name
	return i
}

// Row is a label and what it'll be: Project RAM. One with nothing to say
// isn't shown.
func (i *Item) Row(label, value string) *Item {
	if strings.TrimSpace(value) != "" {
		i.it().Rows = append(i.it().Rows, protocol.ProposalRow{Label: label, Value: value})
	}
	return i
}

// Changed is a row that changes something that's there: what it was, and
// what it'll be.
func (i *Item) Changed(label, was, now string) *Item {
	i.it().Rows = append(i.it().Rows, protocol.ProposalRow{Label: label, Value: now, Was: &was})
	return i
}

// Markdown is what it'll say at length: an issue's description.
func (i *Item) Markdown(text string) *Item {
	i.it().Markdown = text
	return i
}

// Accepting is one item being done, as the user accepted it.
type Accepting struct {
	// N is its number in the proposal, from 1; Key is ws's for it, the
	// same each time it's asked (the kit makes nothing twice by it).
	N   int
	Key string
	// Conversation is the conversation it was proposed in, and Mission
	// its mission's slug, if it's a mission's.
	Conversation, Mission string

	ctx     context.Context
	payload json.RawMessage
	made    map[int]string
	out     protocol.AcceptedItem
}

// Context ends when ws gives up on it.
func (a *Accepting) Context() context.Context { return a.ctx }

// Decode reads the item's payload, as Draft gave it, into v.
func (a *Accepting) Decode(v any) error { return json.Unmarshal(a.payload, v) }

// Resolve is a name as the item wrote it, made what it names: #1 is what
// item 1 made, and anything else is itself. An item that hasn't made
// anything (not accepted, or not done) is an error, which the item
// returns as its problem.
func (a *Accepting) Resolve(name string) (string, error) {
	n, ok := strings.CutPrefix(strings.TrimSpace(name), "#")
	if !ok {
		return name, nil
	}
	i, err := strconv.Atoi(n)
	if err != nil {
		return name, nil
	}
	if made := a.made[i]; made != "" {
		return made, nil
	}
	return "", fmt.Errorf("it goes with item %d, which hasn't been done: accept that one first", i)
}

// Made is the name of what it made, or changed (RAM-130): ws links the
// conversation to it, and a later item's #N is it.
func (a *Accepting) Made(name string) { a.out.Made = name }

// Say is a line on what was done, for the user.
func (a *Accepting) Say(text string) { a.out.Say = text }

// Open is where to see it: a path in the plugin's app.
func (a *Accepting) Open(path string) { a.out.Open = path }

// proposalsDeclared are what the plugin proposes as ws is told it, and
// what's wrong with them.
func (p *Plugin) proposalsDeclared() ([]protocol.Proposes, error) {
	var out []protocol.Proposes
	for _, r := range p.proposals {
		switch {
		case r.guide == nil:
			return nil, fmt.Errorf("what it proposes, %q, has no Guide: the agent asks how one's written first", r.decl.ID)
		case r.draft == nil:
			return nil, fmt.Errorf("what it proposes, %q, has no Draft", r.decl.ID)
		case r.accept == nil:
			return nil, fmt.Errorf("what it proposes, %q, has nothing that does it (Accept)", r.decl.ID)
		}
		out = append(out, r.decl)
	}
	if problem := protocol.ProposesProblem(out); problem != "" {
		return nil, errors.New(problem)
	}
	return out, nil
}

func (p *Plugin) proposal(id string) (*Proposal, error) {
	for _, r := range p.proposals {
		if r.decl.ID == id {
			return r, nil
		}
	}
	return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: fmt.Sprintf("this plugin proposes nothing called %q", id)}
}

// draftProposal answers ws's proposal/draft: the guide, with no input;
// otherwise what the draft made of it, or why it's refused.
func (p *Plugin) draftProposal(ctx context.Context, in protocol.ProposalDraftParams) (any, error) {
	r, err := p.proposal(in.ID)
	if err != nil {
		return nil, err
	}
	if p.broken != nil {
		return nil, p.broken
	}
	ctx, cancel := context.WithTimeout(ctx, p.CommandTimeout)
	defer cancel()
	if len(bytes.TrimSpace(in.Input)) == 0 || string(bytes.TrimSpace(in.Input)) == "null" {
		var guide string
		err := p.recovering("the guide to "+in.ID, func() (err error) {
			guide, err = r.guide(ctx)
			return err
		})
		if err != nil {
			return nil, err
		}
		return protocol.ProposalDraftResult{Guide: guide}, nil
	}
	d := &Draft{Conversation: in.Conversation, Mission: in.Mission, ctx: ctx, input: in.Input}
	err = p.recovering("the draft of "+in.ID, func() error { return r.draft(d) })
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return protocol.ProposalDraftResult{Refused: refused.said}, nil
	case err != nil:
		return nil, err
	case len(d.items) == 0:
		return protocol.ProposalDraftResult{Refused: "there's nothing in it to do: write at least one, as the guide says"}, nil
	}
	title := d.title
	switch {
	case title != "":
	case len(d.items) == 1:
		title = d.items[0].Title
	default:
		title = fmt.Sprintf("%d of them", len(d.items))
	}
	return protocol.ProposalDraftResult{Proposal: &protocol.Proposal{Title: title, Items: d.items}}, nil
}

// acceptProposal answers ws's proposal/accept: each item asked, in order,
// done by Accept, but one done already under its key, which is answered
// as it was. What each made is a later one's #N.
func (p *Plugin) acceptProposal(ctx context.Context, in protocol.ProposalAcceptParams) (any, error) {
	r, err := p.proposal(in.ID)
	if err != nil {
		return nil, err
	}
	if p.broken != nil {
		return nil, p.broken
	}
	if in.Key == "" {
		return nil, &rpc.Error{Code: protocol.CodeInvalidParams, Message: "an accept has a key, to make nothing twice by"}
	}
	ctx, cancel := context.WithTimeout(ctx, p.CommandTimeout)
	defer cancel()
	p.accepting.Lock()
	defer p.accepting.Unlock()
	done := p.doneProposals()
	made := map[int]string{}
	for n, m := range in.Made {
		made[n] = m
	}
	out := protocol.ProposalAcceptResult{Items: []protocol.AcceptedItem{}}
	for _, it := range in.Items {
		key := in.Key + "/" + strconv.Itoa(it.N)
		if was, ok := done[key]; ok {
			out.Items = append(out.Items, was)
			made[it.N] = was.Made
			continue
		}
		a := &Accepting{N: it.N, Key: key, Conversation: in.Conversation, Mission: in.Mission, ctx: ctx, payload: it.Payload, made: made}
		err := p.recovering(fmt.Sprintf("accepting item %d of %s", it.N, in.ID), func() error { return r.accept(a) })
		a.out.N = it.N
		if err != nil {
			out.Items = append(out.Items, protocol.AcceptedItem{N: it.N, Problem: err.Error()})
			continue
		}
		if err := p.keepDone(key, a.out); err != nil {
			p.Log.Error("an item was done, but keeping that it was failed: asked again, it'd be done again", "key", key, "err", err)
		}
		made[it.N] = a.out.Made
		out.Items = append(out.Items, a.out)
	}
	return out, nil
}

// proposalsFile is where the items done are kept, by their key, so that
// one asked again (ws didn't hear the answer) isn't done again: a line of
// JSON each, in the plugin's data folder.
const proposalsFile = "proposals-done.jsonl"

type doneLine struct {
	Key  string                `json:"key"`
	Item protocol.AcceptedItem `json:"item"`
}

// doneProposals are the items done, by key.
func (p *Plugin) doneProposals() map[string]protocol.AcceptedItem {
	out := map[string]protocol.AcceptedItem{}
	f, err := os.Open(filepath.Join(p.Data(), proposalsFile))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var l doneLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Key != "" {
			out[l.Key] = l.Item
		}
	}
	return out
}

func (p *Plugin) keepDone(key string, item protocol.AcceptedItem) error {
	if err := os.MkdirAll(p.Data(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(p.Data(), proposalsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	data, err := json.Marshal(doneLine{Key: key, Item: item})
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
