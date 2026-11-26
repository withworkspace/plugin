package plugin_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/withworkspace/plugin"
	"github.com/withworkspace/plugin/plugintest"
)

// Issue is what the test's endpoints answer, and NewComment what one
// takes.
type Issue struct {
	Key      string   `json:"key"`
	Comments []string `json:"comments"`
}

type NewComment struct {
	Text string `json:"text"`
}

// Since is a query: which of the history is asked for.
type Since struct {
	Key    string    `json:"key,omitempty"`
	Limit  int       `json:"limit,omitempty"`
	Open   *bool     `json:"open"`
	Labels []string  `json:"label,omitempty"`
	After  time.Time `json:"after,omitzero"`
	Score  float64   `json:"score,omitempty"`
	Paging
	Hidden string `json:"-"`
	note   string //nolint:unused // there to be left out of what the endpoint says
}

// Paging is part of a query, as its own fields.
type Paging struct {
	Page uint `json:"page,omitempty"`
}

type gone struct{ key string }

func (g gone) Error() string   { return g.key + " is gone" }
func (g gone) HTTPStatus() int { return http.StatusGone }

func endpoints(said *[]string) func(p *plugin.Plugin) {
	return func(p *plugin.Plugin) {
		plugin.Get(p, "/api/issues/{key}", func(r *http.Request) (Issue, error) {
			switch key := r.PathValue("key"); key {
			case "NONE-1":
				return Issue{}, plugin.ErrNotFound
			case "OLD-1":
				return Issue{}, gone{key}
			case "BUSY-1":
				return Issue{}, plugin.Status(http.StatusConflict, errors.New("it's being moved"))
			case "BAD-1":
				return Issue{}, errors.New("the store is shut")
			default:
				return Issue{Key: key, Comments: *said}, nil
			}
		})
		plugin.Get(p, "/api/issues", func(r *http.Request) ([]Issue, error) {
			if r.URL.Query().Get("open") == "" {
				return nil, nil
			}
			return []Issue{{Key: "PROJ-1"}}, nil
		})
		plugin.Get(p, "/api/labels", func(*http.Request) (map[string]string, error) { return nil, nil })
		plugin.Post(p, "/api/issues/{key}/comment", func(r *http.Request, in NewComment) (Issue, error) {
			*said = append(*said, in.Text)
			return Issue{Key: r.PathValue("key"), Comments: *said}, nil
		})
		plugin.Post(p, "/api/sync", func(*http.Request, struct{}) (struct{}, error) {
			*said = append(*said, "synced")
			return struct{}{}, nil
		})
		plugin.Put(p, "/api/issues/{key}", func(r *http.Request, in Issue) (Issue, error) { return in, nil })
		plugin.Patch(p, "/api/issues/{key}", func(r *http.Request, in NewComment) (NewComment, error) { return in, nil })
		plugin.Delete(p, "/api/issues/{key}", func(*http.Request) (struct{}, error) { return struct{}{}, nil })
		// What they were asked with, as they read it.
		plugin.Get(p, "/api/history", plugin.Query(func(_ *http.Request, q Since) (Since, error) { return q, nil }))
		plugin.Delete(p, "/api/history", plugin.Query(func(_ *http.Request, q Paging) (Paging, error) { return q, nil }))
	}
}

// An endpoint reads what it takes from the body and answers with JSON,
// and its errors are answered with the status they say.
func TestEndpointsTakeAndAnswerJSON(t *testing.T) {
	var said []string
	ws := plugintest.New(t, nil, endpoints(&said))

	for _, tc := range []struct {
		name, method, path string
		body               any
		status             int
		answer             string
	}{
		{"a read", "GET", "/api/issues/PROJ-1", nil, 200, `{"key":"PROJ-1","comments":null}`},
		{"a list that's nil is an empty one", "GET", "/api/issues", nil, 200, `[]`},
		{"a list", "GET", "/api/issues?open=1", nil, 200, `[{"key":"PROJ-1","comments":null}]`},
		{"a map that's nil is an empty one", "GET", "/api/labels", nil, 200, `{}`},
		{"a body", "POST", "/api/issues/PROJ-1/comment", NewComment{Text: "On it"}, 200, `{"key":"PROJ-1","comments":["On it"]}`},
		{"nothing taken, nothing answered", "POST", "/api/sync", nil, 204, ``},
		{"a put", "PUT", "/api/issues/PROJ-1", Issue{Key: "PROJ-2"}, 200, `{"key":"PROJ-2","comments":null}`},
		{"a patch", "PATCH", "/api/issues/PROJ-1", NewComment{Text: "x"}, 200, `{"text":"x"}`},
		{"a delete", "DELETE", "/api/issues/PROJ-1", nil, 204, ``},

		{"what isn't there", "GET", "/api/issues/NONE-1", nil, 404, `not found`},
		{"an error that says its status", "GET", "/api/issues/OLD-1", nil, 410, `OLD-1 is gone`},
		{"an error given a status", "GET", "/api/issues/BUSY-1", nil, 409, `it's being moved`},
		{"any other error", "GET", "/api/issues/BAD-1", nil, 500, `the store is shut`},
		{"no body where one's taken", "POST", "/api/issues/PROJ-1/comment", nil, 400, `POST /api/issues/{key}/comment takes JSON: it was sent no body`},
		{"a body of the wrong shape", "POST", "/api/issues/PROJ-1/comment", map[string]int{"text": 1}, 400, `POST /api/issues/{key}/comment takes JSON: json: cannot unmarshal number into Go struct field NewComment.text of type string`},
		{"another method", "POST", "/api/issues", nil, 405, `Method Not Allowed`},

		{"a query", "GET", "/api/history?key=PROJ-1&limit=20&open=true&label=a&label=b+c&after=2026-11-20T08:00:00Z&score=1.5&page=2", nil, 200,
			`{"key":"PROJ-1","limit":20,"open":true,"label":["a","b c"],"after":"2026-11-20T08:00:00Z","score":1.5,"page":2}`},
		{"no query: each field as it was", "GET", "/api/history", nil, 200, `{"open":null}`},
		{"names the query hasn't are ignored", "GET", "/api/history?other=x&Hidden=h&note=n&limit=1", nil, 200, `{"limit":1,"open":null}`},
		{"a delete's query", "DELETE", "/api/history?page=3", nil, 200, `{"page":3}`},
		{"a number that isn't one", "GET", "/api/history?limit=lots", nil, 400, `GET /api/history's query: limit is "lots", which isn't a whole number`},
		{"a bool that isn't one", "GET", "/api/history?open=maybe", nil, 400, `GET /api/history's query: open is "maybe", which isn't true or false`},
		{"a number below 0 for one that can't be", "GET", "/api/history?page=-1", nil, 400, `GET /api/history's query: page is "-1", which isn't a whole number of 0 or more`},
		{"a time that isn't one", "GET", "/api/history?after=yesterday", nil, 400, `GET /api/history's query: after is "yesterday", which can't be read: parsing time "yesterday" as "2006-01-02T15:04:05Z07:00": cannot parse "yesterday" as "2006"`},
		{"a float that isn't one", "GET", "/api/history?score=high", nil, 400, `GET /api/history's query: score is "high", which isn't a number`},
	} {
		got := ws.Send(tc.method, tc.path, tc.body)
		if got.Status != tc.status || strings.TrimSpace(got.Body) != tc.answer {
			t.Errorf("%s: %s %s answered %d %q, not %d %q", tc.name, tc.method, tc.path, got.Status, got.Body, tc.status, tc.answer)
		}
		kind := got.Header.Get("Content-Type")
		if want := "application/json"; tc.status == 200 && kind != want {
			t.Errorf("%s: answered as %q, not %q", tc.name, kind, want)
		}
		if tc.status >= 400 && !strings.HasPrefix(kind, "text/plain") {
			t.Errorf("%s: an error answered as %q, not plain text", tc.name, kind)
		}
	}
	if len(said) != 2 || said[0] != "On it" || said[1] != "synced" {
		t.Errorf("what the endpoints were sent: %q", said)
	}
}

// A path with a method in it, or with no /, is the author's mistake, said
// as the plugin starts.
func TestAnEndpointsPathHasNoMethod(t *testing.T) {
	_, err := plugintest.Start(nil, func(p *plugin.Plugin) {
		plugin.Get(p, "GET /api/issues", func(*http.Request) ([]Issue, error) { return nil, nil })
	})
	if err == nil || !strings.Contains(err.Error(), `plugin.Get(p, "GET /api/issues", …): an endpoint's path starts with /, and has no method in it`) {
		t.Errorf("a path with a method in it: %v", err)
	}
}

func TestStatusOfNoErrorIsNone(t *testing.T) {
	if err := plugin.Status(http.StatusConflict, nil); err != nil {
		t.Errorf("Status of no error: %v", err)
	}
	inner := errors.New("inner")
	if err := plugin.Status(http.StatusConflict, inner); !errors.Is(err, inner) || err.Error() != "inner" {
		t.Errorf("Status keeps the error: %v", err)
	}
}

// A query's type that a query can't fill is the author's mistake, said
// as the plugin starts.
func TestAQueryIsAStructOfPlainValues(t *testing.T) {
	said := func(declare func()) (text string) {
		defer func() { text, _ = recover().(string) }()
		declare()
		return ""
	}
	type Filter struct {
		By map[string]string `json:"by"`
	}
	if got := said(func() {
		plugin.Query(func(*http.Request, Filter) (string, error) { return "", nil })
	}); !strings.Contains(got, "Filter.By is a map[string]string: a query's field is a string, a number, a bool") {
		t.Errorf("a query with a map in it: %q", got)
	}
	if got := said(func() {
		plugin.Query(func(*http.Request, string) (string, error) { return "", nil })
	}); !strings.Contains(got, "plugin.Query: string isn't a struct") {
		t.Errorf("a query that isn't a struct: %q", got)
	}
}
