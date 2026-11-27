package plugin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/withworkspace/plugin"
	"github.com/withworkspace/plugin/plugintest"
	"github.com/withworkspace/plugin/protocol"
)

type newIssue struct {
	Summary string `json:"summary"`
	Project string `json:"project"`
	Parent  string `json:"parent,omitempty"`
}

// issueProposals is a plugin proposing issues: a list of them, each made
// with the next key, a sub-issue under what its parent (#N) made.
func issueProposals(made *[]string) func(p *plugin.Plugin) {
	return func(p *plugin.Plugin) {
		p.Proposes("issues", "Jira issues").Icon("ticket").
			When("Issues to create.").
			Guide(func(context.Context) (string, error) {
				return `A JSON list: [{"summary": "…", "project": "RAM"}]`, nil
			}).
			Draft(func(d *plugin.Draft) error {
				var in []newIssue
				if err := d.Decode(&in); err != nil {
					return err
				}
				for _, is := range in {
					if is.Project != "RAM" {
						return d.Refuse("%q isn't a project here: RAM is", is.Project)
					}
					d.Item(is.Summary, is).Row("Project", is.Project).Row("Parent", is.Parent)
				}
				if len(in) > 1 {
					d.Title(fmt.Sprintf("%d issues in RAM", len(in)))
				}
				return nil
			}).
			Accept(func(a *plugin.Accepting) error {
				var is newIssue
				if err := a.Decode(&is); err != nil {
					return err
				}
				if is.Summary == "fails" {
					return errors.New("Jira said no")
				}
				parent, err := a.Resolve(is.Parent)
				if err != nil {
					return err
				}
				key := fmt.Sprintf("RAM-%d", 100+len(*made))
				*made = append(*made, key+" "+is.Summary+" under "+parent)
				a.Made(key)
				a.Say("Created " + key)
				return nil
			})
	}
}

func TestProposesDraftsAndAccepts(t *testing.T) {
	var made []string
	ws := plugintest.New(t, nil, issueProposals(&made))
	reg := ws.Registration()
	if len(reg.Proposes) != 1 || reg.Proposes[0].ID != "issues" || reg.Proposes[0].When != "Issues to create." {
		t.Fatalf("registered: %+v", reg.Proposes)
	}
	if reg.Proposes[0].Unattended {
		t.Error("it's said to be safe unattended, and wasn't told so")
	}

	guide, err := ws.Guide("issues")
	if err != nil || !strings.Contains(guide, "JSON list") {
		t.Errorf("the guide: %q, %v", guide, err)
	}
	for input, want := range map[string]string{
		`[{"summary": "A", "project": "XYZ"}]`: `"XYZ" isn't a project`,
		`{"summary": "A"}`:                     "doesn't read as it should",
		`[{"summry": "A", "project": "RAM"}]`:  "doesn't read as it should",
		`[]`:                                   "nothing in it",
	} {
		out, err := ws.Draft("issues", input)
		if err != nil || !strings.Contains(out.Refused, want) || out.Proposal != nil {
			t.Errorf("%s: %+v, %v; want it refused saying %q", input, out, err, want)
		}
	}

	out, err := ws.Draft("issues", []newIssue{{Summary: "Epic", Project: "RAM"}, {Summary: "Story", Project: "RAM", Parent: "#1"}, {Summary: "fails", Project: "RAM"}})
	if err != nil || out.Proposal == nil {
		t.Fatalf("drafted: %+v, %v", out, err)
	}
	if out.Proposal.Title != "3 issues in RAM" || len(out.Proposal.Items) != 3 || out.Proposal.Items[1].Rows[1].Value != "#1" {
		t.Errorf("drafted: %+v", out.Proposal)
	}

	// The story alone: its parent isn't made.
	got, err := ws.Accept("issues", "w1/1", out.Proposal, 2)
	if err != nil || len(got.Items) != 1 || !strings.Contains(got.Items[0].Problem, "item 1") {
		t.Errorf("the story before its epic: %+v, %v", got, err)
	}
	// All of it: the story goes under what the epic made; one fails.
	got, err = ws.Accept("issues", "w1/1", out.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	says := func(r protocol.ProposalAcceptResult) string {
		var out []string
		for _, it := range r.Items {
			out = append(out, fmt.Sprintf("%d %s%s", it.N, it.Made, it.Problem))
		}
		return strings.Join(out, ", ")
	}
	if s := says(got); s != "1 RAM-100, 2 RAM-101, 3 Jira said no" {
		t.Errorf("accepted: %s", s)
	}
	if made[1] != "RAM-101 Story under RAM-100" {
		t.Errorf("the story: %q", made[1])
	}
	// Asked again (ws didn't hear), the ones done aren't done again; and
	// an earlier item's made comes from ws when it isn't asked again.
	got, err = ws.Accept("issues", "w1/1", out.Proposal)
	if s := says(got); err != nil || s != "1 RAM-100, 2 RAM-101, 3 Jira said no" || len(made) != 2 {
		t.Errorf("asked again: %s, %v, made %v", s, err, made)
	}
	got, err = ws.AcceptItems(protocol.ProposalAcceptParams{ID: "issues", Key: "w1/2", Made: map[int]string{1: "RAM-100"},
		Items: []protocol.AcceptItem{{N: 2, Payload: out.Proposal.Items[1].Payload}}})
	if err != nil || says(got) != "2 RAM-102" || made[2] != "RAM-102 Story under RAM-100" {
		t.Errorf("with what was made before: %s, %v, %v", says(got), err, made)
	}
	if _, err := ws.Draft("nothing", `[]`); err == nil {
		t.Error("drafted what the plugin doesn't propose")
	}
}

func TestProposesNeedsAllItsParts(t *testing.T) {
	_, err := plugintest.Start(nil, func(p *plugin.Plugin) {
		p.Proposes("issues", "Jira issues").Draft(func(*plugin.Draft) error { return nil })
	})
	if err == nil || !strings.Contains(err.Error(), "Guide") {
		t.Errorf("one without a guide: %v", err)
	}
}

// A kind said to be safe unattended is registered so: what ws lets an
// automation accept with nobody looking.
func TestProposesUnattended(t *testing.T) {
	var made []string
	ws := plugintest.New(t, nil, func(p *plugin.Plugin) {
		issueProposals(&made)(p)
		p.Proposes("comments", "Comments").Unattended().
			Guide(func(context.Context) (string, error) { return "a JSON list of comments", nil }).
			Draft(func(d *plugin.Draft) error { d.Item("a comment", nil); return nil }).
			Accept(func(a *plugin.Accepting) error { return nil })
	})
	reg := ws.Registration()
	if len(reg.Proposes) != 2 || reg.Proposes[0].Unattended || !reg.Proposes[1].Unattended {
		t.Errorf("registered: %+v", reg.Proposes)
	}
}
