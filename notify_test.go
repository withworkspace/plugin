package plugin_test

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/withworkspace/plugin"
	"github.com/withworkspace/plugin/plugintest"
	"github.com/withworkspace/plugin/protocol"
)

// notifier is a plugin that registers two kinds of notification and has
// a command that sends one: KIND KEY TEXT.
func notifier(p *plugin.Plugin) {
	p.Notifies("assigned", "Given to you").About("  Someone gives you an issue.  ")
	p.Notifies("new", "New on the board").Off()
	p.Command("tell", "", func(c *plugin.Call) error {
		added, err := p.Notify(c.Context(), plugin.Notice{Kind: c.Arg(0), Key: c.Arg(1), Text: c.Arg(2), Refs: []string{"RAM-1"}})
		if err != nil {
			return err
		}
		return c.Say(fmt.Sprintf("n%d added=%v muted=%v", added.N, added.Added, added.Muted))
	}).Args("KIND KEY TEXT")
}

func TestTheKindsOfNotificationAreRegistered(t *testing.T) {
	ws, err := plugintest.Start(fstest.MapFS{}, notifier)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	got := ws.Registration().Notifies
	want := []protocol.Notifies{
		{ID: "assigned", Title: "Given to you", About: "Someone gives you an issue."},
		{ID: "new", Title: "New on the board", Default: protocol.RouteOff},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("registered %+v, want %+v", got, want)
	}
}

func TestAPluginNotifiesOfItsKindsOnceAKey(t *testing.T) {
	ws, err := plugintest.Start(fstest.MapFS{}, notifier)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	// A ws from before notifications: it says to update ws.
	if got := ws.Run("tell", "assigned", "k1", "Ana gave you RAM-1"); got.Exit == 0 || !strings.Contains(got.Stderr, "update ws") {
		t.Errorf("notifying a ws that can't be told: %+v", got)
	}

	kept := ws.Notices("new")
	for _, tc := range []struct {
		args []string
		said string
	}{
		{[]string{"assigned", "k1", "Ana gave you RAM-1"}, "n1 added=true muted=false"},
		{[]string{"assigned", "k1", "Ana gave you RAM-1"}, "n1 added=false muted=false"}, // seen again: not kept twice
		{[]string{"new", "", "RAM-1 is new"}, "n2 added=true muted=true"},
	} {
		got := ws.Run(append([]string{"tell"}, tc.args...)...)
		if got.Exit != 0 || strings.TrimSpace(got.Stdout) != tc.said {
			t.Errorf("tell %v: %+v, want %q", tc.args, got, tc.said)
		}
	}
	if got := kept(); len(got) != 2 || got[0].Text != "Ana gave you RAM-1" || got[0].Refs[0] != "RAM-1" || got[1].Kind != "new" {
		t.Errorf("kept %+v", got)
	}

	// A kind it didn't declare is the plugin's mistake: said before ws is
	// asked.
	if got := ws.Run("tell", "moved", "", "RAM-1 moved"); got.Exit == 0 || !strings.Contains(got.Stderr, `didn't declare the kind "moved"`) {
		t.Errorf("an undeclared kind: %+v", got)
	}
	if got := ws.Run("tell", "assigned", "", " "); got.Exit == 0 || !strings.Contains(got.Stderr, "says nothing") {
		t.Errorf("an empty notification: %+v", got)
	}
}

func TestKindsOfNotificationDeclaredWronglyDontStart(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(p *plugin.Plugin)
		why   string
	}{
		"a bad id": {func(p *plugin.Plugin) { p.Notifies("Assigned", "A") }, "lower-case letters"},
		"twice":    {func(p *plugin.Plugin) { p.Notifies("a", "A"); p.Notifies("a", "B") }, `notifies "a" twice`},
		"no title": {func(p *plugin.Plugin) { p.Notifies("a", " ") }, "has no title"},
	} {
		ws, err := plugintest.Start(fstest.MapFS{}, tc.setup)
		if err == nil {
			ws.Close()
			t.Errorf("a plugin with a kind of notification declared %s started", name)
		} else if !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: didn't say %q: %v", name, tc.why, err)
		}
	}
	// The rule ws holds a registration to as well: a default of desktop
	// isn't the plugin's to choose.
	if problem := protocol.NotifiesProblem([]protocol.Notifies{{ID: "a", Title: "A", Default: "desktop"}}); !strings.Contains(problem, "feed or off") {
		t.Errorf("a desktop default: %q", problem)
	}
}
