package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/withworkspace/plugin/protocol"
)

// What a plugin tells the person of: it registers the kinds of
// notification it sends (Notifies), each the person's to send to their
// feed, to the desktop as well, or nowhere, under the plugin's name in
// Settings › Notifications; then sends one of a kind when it happens
// (Notify). ws keeps each in the person's feed, the same feed its own
// notifications are in, and opens what it's about when it's clicked.

// NotifyKind is a kind of notification the plugin sends.
type NotifyKind struct {
	decl protocol.Notifies
}

// Notifies declares a kind of notification the plugin sends: its id (what
// Notify names it by) and what Settings calls it. About says when it's
// sent; Off has it sent nowhere until the person says otherwise.
//
//	p.Notifies("assigned", "Given to you").
//		About("Someone else gives you an issue on the board.")
//	p.Notifies("new", "New on the board").
//		About("An issue a query finds that the board hadn't.").Off()
func (p *Plugin) Notifies(id, title string) *NotifyKind {
	p.declaring("Notifies")
	k := &NotifyKind{decl: protocol.Notifies{ID: id, Title: title}}
	p.notifies = append(p.notifies, k)
	return k
}

// About is a line on when it's sent, under its title in Settings.
func (k *NotifyKind) About(text string) *NotifyKind {
	k.decl.About = strings.TrimSpace(text)
	return k
}

// Off has it sent nowhere until the person says otherwise: for a kind
// that's useful to some and noise to most.
func (k *NotifyKind) Off() *NotifyKind {
	k.decl.Default = protocol.RouteOff
	return k
}

// notifiesDeclared are the kinds as ws is told them, and what's wrong
// with them.
func (p *Plugin) notifiesDeclared() ([]protocol.Notifies, error) {
	var out []protocol.Notifies
	for _, k := range p.notifies {
		out = append(out, k.decl)
	}
	if problem := protocol.NotifiesProblem(out); problem != "" {
		return nil, errors.New(problem)
	}
	return out, nil
}

// Notice is a notification: which of the plugin's kinds it is, and what
// happened, as a sentence ("Ana gave you RAM-12: Fix the login page").
type Notice struct {
	// Kind is one of the kinds declared with Notifies.
	Kind string
	// Text says what happened, as a sentence.
	Text string
	// Refs is what it's about, as ws names things (RAM-12): the first is
	// what opening it goes to.
	Refs []string
	// Key is the plugin's own name for what happened: one sent again with
	// a key already sent isn't added twice, so a sync that sees the same
	// change twice needn't remember it told of it. "" for none.
	Key string
	// Attention marks one that went wrong.
	Attention bool
	// At is when it happened: now, if it's zero.
	At time.Time
}

// Notify tells the person of something, in their feed: what's returned
// says its number there, whether it's new (Added false: one with the same
// Key was sent before), and whether its kind is sent nowhere (Muted: it's
// kept, but not shown). Whether it reaches the desktop too is the
// person's to say. A kind the plugin didn't declare is an error, and a ws
// from before notifications is ErrUnsupported.
//
//	_, err := p.Notify(ctx, plugin.Notice{
//		Kind: "assigned",
//		Text: "Ana gave you RAM-12: Fix the login page",
//		Refs: []string{"RAM-12"},
//		Key:  "assigned:RAM-12:10234",
//	})
func (p *Plugin) Notify(ctx context.Context, n Notice) (protocol.NotificationAdded, error) {
	if !p.notifiesKind(n.Kind) {
		return protocol.NotificationAdded{}, fmt.Errorf("notify: the plugin didn't declare the kind %q: declare it with Notifies", n.Kind)
	}
	if strings.TrimSpace(n.Text) == "" {
		return protocol.NotificationAdded{}, errors.New("notify: it says nothing")
	}
	var out protocol.NotificationAdded
	err := p.ask(ctx, protocol.MethodNotificationAdd, protocol.NotificationAddParams{
		Kind: n.Kind, Text: n.Text, Refs: n.Refs, Key: n.Key, Attention: n.Attention, At: n.At,
	}, &out)
	return out, err
}

func (p *Plugin) notifiesKind(id string) bool {
	for _, k := range p.notifies {
		if k.decl.ID == id {
			return true
		}
	}
	return false
}
