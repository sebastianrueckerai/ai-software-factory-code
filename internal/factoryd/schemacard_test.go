package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// cardForgejo plays the configuration repository of a schema pass that
// files cards: main's tree, the bot's fork with its branches, the pull
// requests, and the issues with their labels a card needs. It answers an
// unexpected call with a teapot, so the caller fails on it.
type cardForgejo struct {
	mu       sync.Mutex
	main     map[string]string // upstream main's tree
	fork     map[string]string // the fork's main
	branches map[string]map[string]string
	prs      []int64
	issues   []issueBody
	labels   map[int64][]string
	writes   []string
}

const (
	cardUpstream = "sf/ai-software-factory-infra"
	cardUpPath   = "/api/v1/repos/" + cardUpstream
	cardForkPath = "/api/v1/repos/factory-agent/ai-software-factory-infra"
)

func (s *cardForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		p, q := r.URL.Path, r.URL.Query()
		if r.Method != http.MethodGet {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body) // #nosec G104 -- a test stub reads it anyway
			s.write(t, w, r, p, body)
			return
		}
		switch {
		case p == cardUpPath+"/branches/main":
			fmt.Fprint(w, `{"commit":{"id":"c1"}}`)
		case strings.HasPrefix(p, cardUpPath+"/raw/") && q.Get("ref") == "c1":
			c, ok := s.main[strings.TrimPrefix(p, cardUpPath+"/raw/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, c)
		case p == cardUpPath+"/pulls" && q.Get("state") == "open":
			// GetPages reads on until a page comes back empty
			if q.Get("page") != "1" {
				fmt.Fprint(w, `[]`)
				return
			}
			out := []map[string]any{}
			for _, n := range s.prs {
				out = append(out, map[string]any{"number": n, "state": "open",
					"head": map[string]any{"ref": "factory/schema-0123456789ab",
						"repo": map[string]string{"full_name": "factory-agent/ai-software-factory-infra"}}})
			}
			json.NewEncoder(w).Encode(out)
		case p == cardForkPath:
			if s.fork == nil {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"fork":true,"private":true,"parent":{"full_name":"`+cardUpstream+`"}}`)
		case p == cardForkPath+"/sync_fork":
			fmt.Fprintf(w, `{"allowed":true,"commits_behind":%d}`, len(changed(s.main, s.fork)))
		case strings.HasPrefix(p, cardForkPath+"/branches/"):
			name := strings.TrimPrefix(p, cardForkPath+"/branches/")
			if s.branches[name] == nil {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"commit":{"id":"b"}}`)
		case strings.HasPrefix(p, cardForkPath+"/contents/") && s.ref(q.Get("ref")) != nil:
			c, ok := s.ref(q.Get("ref"))[strings.TrimPrefix(p, cardForkPath+"/contents/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(c)), "encoding": "base64", "sha": blobOf(c)})
		default:
			t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ref is the fork's branch ref, its main when ref names no branch.
func (s *cardForgejo) ref(name string) map[string]string {
	if b, ok := s.branches[name]; ok {
		return b
	}
	if name == "" || name == "main" {
		return s.fork
	}
	return nil
}

// write plays the writes: the fork, its commits, the pull request, the
// card's issue and its labels.
// issueBody is an issue the stub was asked to file.
type issueBody struct{ title, body string }

func (s *cardForgejo) write(t *testing.T, w http.ResponseWriter, r *http.Request, p string, body map[string]any) {
	s.writes = append(s.writes, r.Method+" "+p)
	switch {
	case r.Method == http.MethodPost && p == cardUpPath+"/forks":
		s.fork = maps.Clone(s.main)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"fork":true,"private":true,"parent":{"full_name":"`+cardUpstream+`"}}`)
	case r.Method == http.MethodPost && p == cardForkPath+"/sync_fork":
		s.fork = maps.Clone(s.main)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, cardForkPath+"/branches/"):
		delete(s.branches, strings.TrimPrefix(p, cardForkPath+"/branches/"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && p == cardForkPath+"/contents":
		b := maps.Clone(s.fork)
		for _, op := range body["files"].([]any) {
			op := op.(map[string]any)
			content, _ := base64.StdEncoding.DecodeString(op["content"].(string))
			b[op["path"].(string)] = string(content)
		}
		s.branches[body["new_branch"].(string)] = b
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"commit":{"sha":"c2"}}`)
	case r.Method == http.MethodPost && p == cardUpPath+"/pulls":
		s.prs = append(s.prs, int64(len(s.prs)+1))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/%s/pulls/%d","head":{"ref":"factory/schema-0123456789ab","sha":"h1"}}`,
			len(s.prs), cardUpstream, len(s.prs))
	case r.Method == http.MethodPost && p == cardUpPath+"/issues":
		n := int64(len(s.issues) + 1)
		s.issues = append(s.issues, issueBody{title: body["title"].(string), body: body["body"].(string)})
		if s.labels == nil {
			s.labels = map[int64][]string{}
		}
		s.labels[n] = []string{}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/%s/issues/%d"}`, n, cardUpstream, n)
	case r.Method == http.MethodPut && strings.HasSuffix(p, "/labels"):
		var number int64
		fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(p, cardUpPath+"/issues/"), "/labels"), "%d", &number)
		for _, name := range body["labels"].([]any) {
			s.labels[number] = append(s.labels[number], name.(string))
		}
		io.WriteString(w, `[]`)
	default:
		t.Errorf("unexpected %s %s", r.Method, p)
		w.WriteHeader(http.StatusTeapot)
	}
}

func (s *cardForgejo) snapshot() ([]issueBody, map[int64][]string, []string, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issues := slices.Clone(s.issues)
	labels := map[int64][]string{}
	for n, ls := range s.labels {
		labels[n] = slices.Clone(ls)
	}
	return issues, labels, slices.Clone(s.writes), slices.Clone(s.prs)
}

// cardRig is the schema copier of build 0123456789ab as factoryd wires it,
// on one stale copy of policy.schema.json, with the store, workflows and
// timeline a card needs. The configuration repository's project moves
// ready by writers, so a card enters ready and starts.
func cardRig(t *testing.T) (*cardForgejo, *SchemaCopier, *fakeStore, *fakeWorkflows) {
	t.Helper()
	schemas, err := policy.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	main := map[string]string{"factory/policy.yaml": "version: 1\n"}
	for name, b := range schemas {
		main["factory/schema/"+name] = string(b)
	}
	main["factory/schema/policy.schema.json"] = "{}\n"
	fj := &cardForgejo{main: main, branches: map[string]map[string]string{}}
	srv := fj.server(t)
	f := &Forgejo{BaseURL: srv.URL, PublicURL: "https://git.example", BotUser: "factory-agent", BotPassword: "bot-pw", HTTP: srv.Client()}
	config := &ForgejoConfig{Forgejo: f, Token: "config-tok", Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Branch: "main", Dir: "factory"}
	store, wf := newFakeStore(), &fakeWorkflows{}
	store.projects = []Project{seedProject(cardUpstream)}
	c := &SchemaCopier{Forgejo: f, Config: config, Build: "0123456789ab",
		Store: store, Workflows: wf, Timeline: &Timeline{Objects: &fakeObjectStore{}},
		Budget: func() policy.Budget { return testCardBudget }}
	return fj, c, store, wf
}

// countOf is how many of got equal want.
func countOf(got []string, want string) int {
	n := 0
	for _, g := range got {
		if g == want {
			n++
		}
	}
	return n
}

// testCardBudget is the policy block the rig sizes a card with.
var testCardBudget = policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50}

// schemaCards are the rows of source schema-copy, by id.
func schemaCards(store *fakeStore) []Task {
	var out []Task
	for _, t := range store.tasks {
		if t.Source == SourceSchemaCopy {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// TestSchemaCopierFilesCard: the pass that opens the schema pull request
// files a card of source schema-copy for it (roadmap 187): the pull
// request set on the row, its issue on the configuration repository,
// small's budget, ready's class and its workflow started.
func TestSchemaCopierFilesCard(t *testing.T) {
	ctx := context.Background()
	fj, c, store, wf := cardRig(t)
	if err := c.SyncOnce(ctx, 1, 0); err != nil {
		t.Fatal(err)
	}
	cards := schemaCards(store)
	if len(cards) != 1 {
		t.Fatalf("cards %v, want the one of source %s", cards, SourceSchemaCopy)
	}
	card := cards[0]
	if card.PRNumber != 1 || card.IssueNumber != 1 {
		t.Fatalf("card = %+v, want pull request #1 and its issue linked", card)
	}
	if card.Status != "queued" || card.Kind != "agent" || card.Lane != "" || card.Process != "" ||
		!slices.Equal(card.RequiredCapabilities, []string{policy.ClassCoder}) {
		t.Fatalf("card = %+v, want a queued no-lane card of ready's class, like a roll", card)
	}
	if card.BudgetEUR != policy.CardBudget(testCardBudget, nil, "small", 0) || card.RiskClass != "standard" {
		t.Fatalf("card = %+v, want small's budget of the policy in force", card)
	}
	if card.Repository != cardUpstream || card.SHA != "0123456789ab" || card.Objective != "Schema copies for 0123456789ab" {
		t.Fatalf("card = %+v, want it on %s named for this build", card, cardUpstream)
	}
	if !slices.Equal(wf.started, []string{card.ID}) {
		t.Fatalf("started %v, want the card's workflow once", wf.started)
	}
	issues, labels, _, prs := fj.snapshot()
	if len(prs) != 1 || len(issues) != 1 {
		t.Fatalf("pull requests %v, issues %v: want the one and its card", prs, issues)
	}
	wantBody := "factoryd opened pull request [#1](https://git.example/sf/ai-software-factory-infra/pulls/1) on sf/ai-software-factory-infra: " +
		"main's copies of the generated schemas differ from what build 0123456789ab generates.\n\n" +
		"Files: factory/schema/policy.schema.json\n\n"
	if !strings.HasPrefix(issues[0].body, wantBody) {
		t.Fatalf("issue body = %q, want it to name pull request #1 and the file it writes", issues[0].body)
	}
	if got := labels[1]; !slices.Contains(got, "state/queued") {
		t.Fatalf("issue labels %v, want the state mirrored", got)
	}
}

// TestSchemaCopierIdempotent: calling open twice for the same build files
// one card: the second pass finds its own pull request open, opens no
// second one and leaves the card alone.
func TestSchemaCopierIdempotent(t *testing.T) {
	ctx := context.Background()
	fj, c, store, wf := cardRig(t)
	for range 2 {
		if err := c.SyncOnce(ctx, 1, 0); err != nil {
			t.Fatal(err)
		}
	}
	cards := schemaCards(store)
	if len(cards) != 1 {
		t.Fatalf("%d cards of a build, want one: %v", len(cards), cards)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("%d task rows in the store, want the one card: %v", len(store.tasks), store.tasks)
	}
	if store.createCalls != 1 {
		t.Fatalf("CreateTask called %d times, want once: the second pass must file no card", store.createCalls)
	}
	if cards[0].PRNumber != 1 {
		t.Fatalf("card = %+v, want it on pull request #1", cards[0])
	}
	_, _, writes, prs := fj.snapshot()
	if len(prs) != 1 || countOf(writes, "POST "+cardUpPath+"/pulls") != 1 {
		t.Fatalf("pull requests %v, writes %v: want the one pull request of the first pass", prs, writes)
	}
	if len(wf.started) != 1 {
		t.Fatalf("started %v, want the card's workflow once", wf.started)
	}
}
