package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

var (
	testShape  = policy.PlanShape{MaxCards: 12, Foundation: true, Integration: true}
	testLimits = policy.Limits{AgentTime: 20 * time.Minute, Steps: 200}
)

// testPlan is a valid plan of four cards: the foundation, two features
// (redirects on links-api) and the integration.
func testPlan() Plan {
	return Plan{Summary: "The foundation first, then links and redirects, then the acceptance tests.", Cards: []PlanCard{
		{Key: "foundation", Role: RoleFoundation, Title: "Foundation: layout, contract, schema", Brief: "Lay out cmd/server, the store and one end-to-end test.",
			Directories: []string{"cmd/server", "internal/"}, Acceptance: []string{"go test ./... passes with TestEndToEndHealth"}, Size: "medium"},
		{Key: "links-api", Role: RoleFeature, Title: "Links API", Brief: "Create, list and delete links.",
			Directories: []string{"internal/links"}, Acceptance: []string{"POST /api/links answers 201"}, Size: "small"},
		{Key: "redirects", Role: RoleFeature, Title: "Redirects and clicks", Brief: "GET /<slug> answers 302 and counts a click.",
			DependsOn: []string{"links-api"}, Acceptance: []string{"GET /abc answers 302"}, Size: "small"},
		{Key: "integration", Role: RoleIntegration, Title: "Integration: acceptance tests", Brief: "Turn A1 to A9 into acceptance/ tests.",
			Directories: []string{"acceptance/"}, Acceptance: []string{"go test ./acceptance/ passes"}, Size: "medium"},
	}}
}

func fencePlan(p Plan) string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return "Here is the plan.\n```json\n" + string(b) + "\n```\n"
}

// padded is testPlan's block padded with leading blanks to size bytes.
func padded(size int) string {
	b, _ := json.Marshal(testPlan())
	return "```json\n" + strings.Repeat(" ", size-len(b)) + string(b) + "\n```"
}

// The size table derives from the risk bound (roadmap 159, 184): large is
// large_lines, medium 60 percent of it, small 30 percent, rounded to whole
// lines; a bound of 1000 reproduces the old figures 300/600/1000, and no
// bound keeps them.
func TestSizeLinesDerivesFromTheBound(t *testing.T) {
	for _, tc := range []struct{ bound, small, medium, large int }{
		{1000, 300, 600, 1000},
		{400, 120, 240, 400},
		{333, 100, 200, 333},
		{0, 300, 600, 1000},
	} {
		got := sizeLines(tc.bound)
		for size, want := range map[string]int{"small": tc.small, "medium": tc.medium, "large": tc.large} {
			if got[size] != want {
				t.Fatalf("sizeLines(%d)[%s] = %d, want %d (all: %v)", tc.bound, size, got[size], want, got)
			}
		}
	}
}

// A size whose derived lines reach the bound does not fit under it: the
// filing check refuses it, naming the card, the size's lines and the
// bound. Under a 400 bound large is refused and small and medium file;
// without a bound every known size files as before.
func TestValidatePlanRefusesASizeAtTheBound(t *testing.T) {
	p := testPlan()
	p.Cards[1].Size = "large"
	fenced := fencePlan(p)
	for name, tc := range map[string]struct {
		bound int
		want  []string
	}{
		"large at a 1000 bound":   {1000, []string{"plan: card links-api: size large plans up to 1000 changed lines and the bound is 1000: split the card"}},
		"large under a 400 bound": {400, []string{"plan: card links-api: size large plans up to 400 changed lines and the bound is 400: split the card"}},
		"no bound":                {0, nil},
	} {
		if got := ValidatePlan(fenced, testShape, testLimits, tc.bound); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: ValidatePlan = %q, want %q", name, got, tc.want)
		}
	}
	if got := ValidatePlan(fencePlan(testPlan()), testShape, testLimits, 400); len(got) != 0 {
		t.Fatalf("small and medium under a 400 bound = %q, want no finding", got)
	}
}

func TestValidatePlan(t *testing.T) {
	change := func(edit func(p *Plan)) string {
		p := testPlan()
		edit(&p)
		return fencePlan(p)
	}
	fourteen := change(func(p *Plan) {
		for i := range 10 {
			p.Cards = slices.Insert(p.Cards, 3, PlanCard{Key: "extra-" + strconv.Itoa(i), Role: RoleFeature, Title: "x", Brief: "x", Acceptance: []string{"x"}, Size: "small"})
		}
	})
	dirs := make([]string, 11)
	for i := range dirs {
		dirs[i] = "d" + strconv.Itoa(i)
	}
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"valid":             {fencePlan(testPlan()), nil},
		"brief at the cap":  {change(func(p *Plan) { p.Cards[2].Brief = strings.Repeat("b", 1500) }), nil},
		"block at the cap":  {padded(12 << 10), nil},
		"a byte over":       {padded(12<<10 + 1), []string{"plan: the plan is 12289 bytes; at most 12 KiB: shorten the briefs"}},
		"no block":          {"I could not plan this.", []string{"plan: no fenced json block in the last message"}},
		"over 12 KiB":       {"```json\n" + strings.Repeat("x", 14541) + "\n```", []string{"plan: the plan is 14.2 KiB; at most 12 KiB: shorten the briefs"}},
		"unknown field":     {"```json\n{\"summary\": \"s\", \"cards\": [], \"extra\": 1}\n```", []string{`plan: json: unknown field "extra"`}},
		"cut off":           {"```json\n{\"summary\": \"s\",\n```", []string{"plan: json: unexpected EOF"}},
		"trailing data":     {"```json\n{\"summary\": \"s\", \"cards\": []} {}\n```", []string{"plan: json: data after the plan's object"}},
		"too many cards":    {fourteen, []string{"plan: 14 cards; this process allows 2 to 12"}},
		"key":               {change(func(p *Plan) { p.Cards[2].Key = "Links API" }), []string{`plan: card 3: key "Links API": lowercase letters, digits and dashes, 2 to 32`}},
		"key twice":         {change(func(p *Plan) { p.Cards[2].Key, p.Cards[2].DependsOn = "links-api", nil }), []string{"plan: card key links-api is used twice"}},
		"role":              {change(func(p *Plan) { p.Cards[2].Role = "extra" }), []string{`plan: card redirects: role "extra", want one of [foundation feature integration]`}},
		"no foundation":     {change(func(p *Plan) { p.Cards[0].Role = RoleFeature }), []string{"plan: 0 cards with role foundation; the process wants exactly one"}},
		"foundation waits":  {change(func(p *Plan) { p.Cards[0].DependsOn = []string{"links-api"} }), []string{"plan: card foundation: a foundation card depends on nothing"}},
		"no integration":    {change(func(p *Plan) { p.Cards[3].Role = RoleFeature }), []string{"plan: 0 cards with role integration; the process wants exactly one"}},
		"on integration":    {change(func(p *Plan) { p.Cards[2].DependsOn = []string{"links-api", "integration"} }), []string{"plan: card redirects: depends on the integration card, which comes last"}},
		"unknown card":      {change(func(p *Plan) { p.Cards[2].DependsOn = []string{"qr-codes"} }), []string{`plan: card redirects: depends on "qr-codes", which is no card of the plan`}},
		"itself":            {change(func(p *Plan) { p.Cards[2].DependsOn = []string{"redirects"} }), []string{"plan: card redirects: depends on itself"}},
		"cycle":             {change(func(p *Plan) { p.Cards[1].DependsOn = []string{"redirects"} }), []string{"plan: the dependencies form a cycle: links-api -> redirects -> links-api"}},
		"large":             {change(func(p *Plan) { p.Cards[2].Size = "large" }), nil},
		"size":              {change(func(p *Plan) { p.Cards[2].Size = "huge" }), []string{"plan: card redirects: size huge: split it; a card is one package, one page or one set of endpoints"}},
		"no size":           {change(func(p *Plan) { p.Cards[2].Size = "" }), []string{`plan: card redirects: size "": want small, medium or large`}},
		"no title":          {change(func(p *Plan) { p.Cards[2].Title = "" }), []string{"plan: card redirects: title empty"}},
		"long title":        {change(func(p *Plan) { p.Cards[2].Title = strings.Repeat("t", 121) }), []string{"plan: card redirects: title over 120 characters"}},
		"no brief":          {change(func(p *Plan) { p.Cards[2].Brief = "" }), []string{"plan: card redirects: brief empty"}},
		"long brief":        {change(func(p *Plan) { p.Cards[1].Brief = strings.Repeat("b", 1501) }), []string{"plan: card links-api: brief over 1500 characters"}},
		"many directories":  {change(func(p *Plan) { p.Cards[2].Directories = dirs }), []string{"plan: card redirects: 11 directories; want 0 to 10"}},
		"outside":           {change(func(p *Plan) { p.Cards[2].Directories = []string{"../x", "/etc", "web/../../y"} }), []string{`plan: card redirects: directory "../x": a relative path inside the repository`, `plan: card redirects: directory "/etc": a relative path inside the repository`, `plan: card redirects: directory "web/../../y": a relative path inside the repository`}},
		"workflow dir":      {change(func(p *Plan) { p.Cards[2].Directories = []string{".forgejo/workflows", "./.github", ".Gitea"} }), []string{`plan: card redirects: directory ".forgejo/workflows": closed to agents, the template owns it`, `plan: card redirects: directory "./.github": closed to agents, the template owns it`, `plan: card redirects: directory ".Gitea": closed to agents, the template owns it`}},
		"no acceptance":     {change(func(p *Plan) { p.Cards[1].Acceptance = nil }), []string{"plan: card links-api: 0 acceptance tests; want 1 to 10"}},
		"long acceptance":   {change(func(p *Plan) { p.Cards[1].Acceptance = []string{"ok", strings.Repeat("a", 301)} }), []string{"plan: card links-api: acceptance test 2 is over 300 characters"}},
		"empty acceptance":  {change(func(p *Plan) { p.Cards[1].Acceptance = []string{""} }), []string{"plan: card links-api: acceptance test 1 is empty"}},
		"eleven acceptance": {change(func(p *Plan) { p.Cards[1].Acceptance = slices.Repeat([]string{"a"}, 11) }), []string{"plan: card links-api: 11 acceptance tests; want 1 to 10"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ValidatePlan(tc.text, testShape, testLimits, 0); !slices.Equal(got, tc.want) {
				t.Fatalf("ValidatePlan = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// stage12-design 5.2: a card is bounded by shape, not a session's time or
// step budget (ruling 221: the guide gives no time estimate any more).
func TestPlanGuideCardSizeSentence(t *testing.T) {
	want := "A card is one pull request by one agent: one package, one page, or one set of endpoints, with its tests, about ten files at most; a larger piece of work is two cards."
	if got := planGuide(testShape, testLimits); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// Stage 16 item 3.7: the guide names the three sizes in the json shape
// and in the field rules. The line figures are ValidatePlan's size
// findings (roadmap 186).
func TestPlanGuideSizes(t *testing.T) {
	got := planGuide(testShape, testLimits)
	if !strings.Contains(got, `"size": "small|medium|large"`) ||
		!strings.Contains(got, "size: small, medium or large; anything bigger is two cards.") {
		t.Fatalf("planGuide = %q, want the three sizes", got)
	}
}

// stage12-design 5.2: acceptance items are named tests, not free text
// ValidatePlan cannot check for. The count bound is ValidatePlan's
// finding, not the prose (roadmap 186).
func TestPlanGuideAcceptanceRule(t *testing.T) {
	want := "acceptance: each item a test by name: TestXxx in <package>, its inputs and the numbers it expects."
	if got := planGuide(testShape, testLimits); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// stage12-design 5.2 (plan.go:502): the foundation's brief names the
// contracts every other card builds on.
func TestPlanGuideFoundationContractsSentence(t *testing.T) {
	want := "The foundation's brief names the contracts every card builds on: how a handler gets the logged-in user (the function and its package), " +
		"the store (its interface, its implementation's package, the methods stubbed), and the server constructor tests import (internal/server.New unless the template says otherwise)."
	if got := planGuide(testShape, testLimits); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
	if strings.Contains(planGuide(policy.PlanShape{MaxCards: 2, Foundation: false, Integration: true}, testLimits), want) {
		t.Fatal("a shape with no foundation card still gets the foundation's contracts sentence")
	}
}

// stage12-design 5.2: the planner is told every card's agent and CI run
// bash check, so it never asks a card to verify some other way.
func TestPlanGuideBashCheckLine(t *testing.T) {
	want := "Every card's agent and CI run bash check; do not ask a card to verify with a tool the check does not run."
	if got := planGuide(testShape, testLimits); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// Roadmap 201: the harness reads AGENTS.md into every session itself, so
// the planner's guide does not ask for it.
func TestPlanGuideLeavesAGENTSmdToTheHarness(t *testing.T) {
	for _, shape := range []policy.PlanShape{testShape, {MaxCards: 12}} {
		if got := planGuide(shape, testLimits); strings.Contains(got, "AGENTS.md") {
			t.Fatalf("planGuide = %q, want no AGENTS.md", got)
		}
	}
}

// The route-file rule stays in the prose: round2-design section 8 planned
// the template's AGENTS.md line, but no template's AGENTS.md carries it,
// so this sentence is the rule's only carrier.
func TestPlanGuideKeepsOneRouteFilePerCard(t *testing.T) {
	want := "Every card adds its routes in its own internal/server/routes_<key>.go, so no two cards edit one route list."
	if got := planGuide(testShape, testLimits); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// The plan arrives by the file_plan tool, never as the last message
// (defect 178): RunPlan refuses a session that ends without filing one.
// The split guide keeps its last-message sentence: RunSplit still reads it.
func TestPlanGuideFilesThroughTheTool(t *testing.T) {
	want := "You file the whole result with the file_plan tool: its plan argument is the complete plan json, one object holding "
	if got := planGuide(testShape, testLimits); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// factoryd, not the model, orders the foundation first and the integration
// last: every card depends on the foundation, the integration on every
// card, besides the edges the planner listed among the features.
func TestFactoryAddsFoundationAndIntegrationEdges(t *testing.T) {
	p := testPlan()
	p.Cards[1].DependsOn = []string{"foundation"} // listed as well: once
	got := planEdges(p)
	want := map[string][]string{
		"links-api":   {"foundation"},
		"redirects":   {"foundation", "links-api"},
		"integration": {"foundation", "links-api", "redirects"},
	}
	if len(got) != len(want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Fatalf("edges of %s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	// the integration listed first still files last
	p.Cards = append([]PlanCard{p.Cards[3]}, p.Cards[:3]...)
	var keys []string
	for _, c := range planOrder(p, planEdges(p)) {
		keys = append(keys, c.Key)
	}
	if !slices.Equal(keys, []string{"foundation", "links-api", "redirects", "integration"}) {
		t.Fatalf("order = %v", keys)
	}
}

// planForgejo is Forgejo's issue API for one repository as CreateChildren
// and the plan's comment use it.
type planForgejo struct {
	mu        sync.Mutex
	issues    map[int64]*planIssue
	next      int64
	comments  []*stubComment
	calls     []string
	failLabel int // the nth labels PUT answers 500
	labelPuts int
	branches  map[string]string // the bot's fork: branch → head
	failCall  string            // the call answered 500 once
}

type planIssue struct {
	title, body, poster, state string
	labels                     []string
	deps                       []int64
}

type stubComment struct {
	id     int64
	issue  int64
	body   string
	author string
}

func (f *planForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/linkshort/linkshort/")
		parts := strings.Split(rest, "/")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		call := r.Method + " " + rest
		if call == f.failCall {
			f.failCall = ""
			f.calls = append(f.calls, call)
			http.Error(w, "forgejo down", http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == "POST" && rest == "issues":
			f.next++
			f.issues[f.next] = &planIssue{title: body["title"].(string), body: body["body"].(string), poster: "factory-agent"}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/linkshort/linkshort/issues/%d"}`, f.next, f.next)
		case r.Method == "GET" && rest == "issues":
			var list []map[string]any
			for n := int64(1); n <= f.next; n++ {
				if is := f.issues[n]; is != nil {
					list = append(list, map[string]any{"number": n, "title": is.title, "body": is.body, "user": map[string]string{"login": is.poster}})
				}
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "GET" && len(parts) == 3 && parts[2] == "labels":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			var list []map[string]any
			for _, l := range f.issues[n].labels {
				list = append(list, map[string]any{"name": l})
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "PUT" && len(parts) == 3 && parts[2] == "labels":
			f.labelPuts++
			if f.labelPuts == f.failLabel {
				w.WriteHeader(http.StatusInternalServerError)
				break
			}
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			f.issues[n].labels = nil
			for _, l := range body["labels"].([]any) {
				f.issues[n].labels = append(f.issues[n].labels, l.(string))
			}
			w.Write([]byte(`[]`))
		case len(parts) == 3 && parts[2] == "dependencies":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			if r.Method == "GET" {
				var list []map[string]any
				for _, d := range f.issues[n].deps {
					list = append(list, map[string]any{"number": d})
				}
				json.NewEncoder(w).Encode(list)
				break
			}
			if body["owner"] != "linkshort" || body["repo"] != "linkshort" {
				t.Errorf("dependency body %v", body)
			}
			f.issues[n].deps = append(f.issues[n].deps, int64(body["index"].(float64)))
			call = fmt.Sprintf("%s %v", call, body["index"])
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && len(parts) == 3 && parts[2] == "comments":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			var list []map[string]any
			for _, c := range f.comments {
				if c.issue == n {
					list = append(list, map[string]any{"id": c.id, "body": c.body, "user": map[string]string{"login": c.author}})
				}
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "POST" && len(parts) == 3 && parts[2] == "comments":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			c := &stubComment{id: int64(700 + len(f.comments)), issue: n, body: body["body"].(string), author: "factory-agent"}
			f.comments = append(f.comments, c)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, c.id)
		case r.Method == "PATCH" && len(parts) == 2 && parts[0] == "issues":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			f.issues[n].state = body["state"].(string)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/linkshort/branches/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/factory-agent/linkshort/branches/")
			call = "GET fork branch " + name
			head, ok := f.branches[name]
			if !ok {
				http.NotFound(w, r)
				break
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
		case r.Method == "POST" && r.URL.Path == "/api/v1/repos/factory-agent/linkshort/branches":
			call = fmt.Sprintf("POST fork branches %s", body["new_branch_name"])
			head, ok := f.branches[body["old_branch_name"].(string)]
			if !ok {
				http.Error(w, `{"message":"The old branch does not exist"}`, http.StatusNotFound)
				break
			}
			if _, exists := f.branches[body["new_branch_name"].(string)]; exists {
				http.Error(w, `{"message":"The branch already exists."}`, http.StatusConflict)
				break
			}
			f.branches[body["new_branch_name"].(string)] = head
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/linkshort/linkshort":
			w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == "GET" && rest == "branches/main":
			w.Write([]byte(`{"commit":{"id":"m1"}}`))
		case r.Method == "PATCH" && len(parts) == 3 && parts[1] == "comments":
			id, _ := strconv.ParseInt(parts[2], 10, 64)
			for _, c := range f.comments {
				if c.id == id {
					c.body = body["body"].(string)
				}
			}
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls = append(f.calls, call)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newPlanRunner is a runner on the fixture tree (version 7: linkshort
// follows spec, 20m of agent time, 200 steps) with plan task t-plan on
// issue #1, the planner's revision 1 of p stored.
func newPlanRunner(t *testing.T, p Plan) (*Runner, *fakeStore, *fakeObjectStore, *planForgejo) {
	t.Helper()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		IssueNumber: 1, Author: "alice", AuthorWriter: true, Priority: 2, Objective: "Link shortener\n\nBuild a link shortener."}
	fj := &planForgejo{issues: map[int64]*planIssue{1: {title: "Link shortener", poster: "alice"}}, next: 1}
	runner, _, store := newTestRunner(t, plan, newFakeKube(), &fakeObjectStore{})
	runner.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	runner.completion.Forgejo = runner.forgejo
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	raw, sha, err := planSHA(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(context.Background(), "t-plan", raw, sha); err != nil {
		t.Fatal(err)
	}
	return runner, store, runner.objects.(*fakeObjectStore), fj
}

// createChildren runs CreateChildren of revision 1, accepted by sebastian.
func createChildren(t *testing.T, r *Runner) ChildrenResult {
	t.Helper()
	val, err := execActivity(t, r, ActivityCreateChildren, "t-plan", 1, "sebastian", int64(7))
	return getInto[ChildrenResult](t, val, err)
}

// cardsByKey are the plan's cards filed, by key.
func cardsByKey(t *testing.T, store *fakeStore) map[string]Task {
	t.Helper()
	cards, err := store.PlanCards(context.Background(), "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Task{}
	for _, c := range cards {
		out[c.PlanKey] = c
	}
	return out
}

// The cards are filed so that every card's dependencies exist first: the
// foundation, the features, the integration; each a backlog row of the
// plan's process, authored by the approver, with its Forgejo dependencies
// and task_deps.
func TestChildrenFiledInTopologicalOrder(t *testing.T) {
	p := testPlan()
	p.Cards = []PlanCard{p.Cards[3], p.Cards[2], p.Cards[1], p.Cards[0]} // the model's order is no filing order
	r, store, objects, fj := newPlanRunner(t, p)
	res := createChildren(t, r)
	if !slices.Equal(res.Issues, []int64{2, 3, 4, 5}) || len(res.Refused) != 0 {
		t.Fatalf("result = %+v", res)
	}
	var titles []string
	for n := int64(2); n <= 5; n++ {
		titles = append(titles, fj.issues[n].title)
	}
	if !slices.Equal(titles, []string{"Foundation: layout, contract, schema", "Links API", "Redirects and clicks", "Integration: acceptance tests"}) {
		t.Fatalf("issues filed in order %q", titles)
	}
	cards := cardsByKey(t, store)
	for key, issue := range map[string]int64{"foundation": 2, "links-api": 3, "redirects": 4, "integration": 5} {
		c := cards[key]
		if c.IssueNumber != issue || c.Status != "backlog" || c.Lane != policy.LaneBacklog || c.Process != "spec" || c.Source != SourcePlan ||
			c.Parent != "t-plan" || c.Author != "sebastian" || !c.AuthorWriter || c.Kind != "agent" || c.Priority != 2 {
			t.Fatalf("card %s = %+v", key, c)
		}
		if !slices.Equal(fj.issues[issue].labels, []string{"backlog", "state/backlog"}) {
			t.Fatalf("issue #%d labels %v", issue, fj.issues[issue].labels)
		}
		if got := events(t, objects, c.ID); !slices.Equal(got, []string{"card_created sebastian"}) {
			t.Fatalf("card %s events %v", key, got)
		}
	}
	for key, want := range map[string][]int64{"foundation": nil, "links-api": {2}, "redirects": {2, 3}, "integration": {2, 3, 4}} {
		c := cards[key]
		if !slices.Equal(fj.issues[c.IssueNumber].deps, want) {
			t.Fatalf("issue #%d depends on %v in Forgejo, want %v", c.IssueNumber, fj.issues[c.IssueNumber].deps, want)
		}
		var ids []string
		for _, n := range want {
			for _, d := range cards {
				if d.IssueNumber == n {
					ids = append(ids, d.ID)
				}
			}
		}
		if !slices.Equal(store.deps[c.ID], ids) {
			t.Fatalf("task_deps of %s = %v, want %v", key, store.deps[c.ID], ids)
		}
	}
	redirects := fj.issues[4].body
	want := "Card redirects of plan #1 (3 of 4), accepted by sebastian. Written by the planner.\n\nGET /<slug> answers 302 and counts a click.\n\n" +
		"Directories (guidance, not a boundary): none\nDepends on: #2 (foundation), #3 (links-api)\nAcceptance:\n- GET /abc answers 302\nSize: small\n\n" +
		"<!-- factoryd plan t-plan card redirects -->"
	if redirects != want {
		t.Fatalf("body of #4 =\n%s\nwant\n%s", redirects, want)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"children_created [2 3 4 5]"}) {
		t.Fatalf("plan steps %v", steps)
	}
}

// A card's budget follows its size (policy budget 5 by default, 15 at
// most): small 2, medium 5, large 10, a large one at most the project's
// card maximum.
func TestChildrenBudgetBySize(t *testing.T) {
	p := testPlan()
	p.Cards[1].Size = "large"
	for _, tc := range []struct {
		budget *policy.ProjectBudget
		large  float64
	}{{nil, 10}, {&policy.ProjectBudget{MonthEUR: 50, TaskMaxEUR: 3}, 3}} {
		r, store, _, _ := newPlanRunner(t, p)
		pinned := fixtureVersion(t)
		pinned.Document.Budget.TaskDefaultEUR, pinned.Document.Budget.TaskMaxEUR = 5, 15
		project := pinned.Projects["linkshort"]
		project.Budget = tc.budget
		pinned.Projects["linkshort"] = project
		r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
		if res := createChildren(t, r); len(res.Refused) != 0 {
			t.Fatalf("refused %q", res.Refused)
		}
		cards := cardsByKey(t, store)
		for key, want := range map[string]float64{"foundation": min(5, tc.large), "links-api": tc.large, "redirects": 2} {
			if got := cards[key].BudgetEUR; got != want {
				t.Errorf("project budget %+v: card %s budget %g, want %g", tc.budget, key, got, want)
			}
		}
	}
}

// planSteps are the plan task's plan steps on its timeline.
func planSteps(t *testing.T, objects *fakeObjectStore, id string) []string {
	t.Helper()
	b, _ := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e["step"] {
		case "children_created":
			out = append(out, fmt.Sprintf("children_created %v", e["issues"]))
		case "plan_revision":
			out = append(out, fmt.Sprintf("plan_revision %v %v", e["revision"], e["cards"]))
		case "plan_invalid", "plan_refused":
			out = append(out, fmt.Sprintf("%s %v", e["step"], e["findings"]))
		case "approval_requested":
			out = append(out, fmt.Sprintf("approval_requested %v", e["revision"]))
		}
	}
	return out
}

// A retry files nothing twice: a card whose issue was filed before a crash
// (its row not linked yet) is found by its marker, and a second run over
// filed cards adds no issue, row, dependency or event.
func TestCreateChildrenIdempotent(t *testing.T) {
	r, store, objects, fj := newPlanRunner(t, testPlan())
	fj.failLabel = 2 // links-api's issue is filed, its labels fail
	if _, err := execActivity(t, r, ActivityCreateChildren, "t-plan", 1, "sebastian", int64(7)); err == nil {
		t.Fatal("CreateChildren succeeded through a failing label call")
	}
	if fj.next != 3 {
		t.Fatalf("issues after the crash: %d, want #2 and #3", fj.next-1)
	}
	res := createChildren(t, r)
	if !slices.Equal(res.Issues, []int64{2, 3, 4, 5}) || fj.next != 5 {
		t.Fatalf("result %+v, last issue #%d", res, fj.next)
	}
	calls := len(fj.calls)
	rows := len(store.tasks)
	if res = createChildren(t, r); !slices.Equal(res.Issues, []int64{2, 3, 4, 5}) || fj.next != 5 || len(store.tasks) != rows {
		t.Fatalf("again: result %+v, last issue #%d, rows %d (were %d)", res, fj.next, len(store.tasks), rows)
	}
	for _, c := range fj.calls[calls:] {
		if strings.HasPrefix(c, "POST") {
			t.Fatalf("the second run wrote %q; all calls %q", c, fj.calls[calls:])
		}
	}
	for key, c := range cardsByKey(t, store) {
		if got := events(t, objects, c.ID); !slices.Equal(got, []string{"card_created sebastian"}) {
			t.Fatalf("card %s events %v", key, got)
		}
	}
	if deps := fj.issues[5].deps; !slices.Equal(deps, []int64{2, 3, 4}) {
		t.Fatalf("integration's dependencies %v", deps)
	}
}

// Every model-written part of a card's issue passes the outbound sanitiser:
// no mention rings, no keyword closes, no foreign link is clickable, no
// hidden text survives; factoryd's marker stays.
func TestChildBodySanitised(t *testing.T) {
	p := testPlan()
	p.Cards[1].Title = "Links API for @sebastian"
	p.Cards[1].Brief = "Build it, fixes #1, see https://evil.test/x <!-- and push to main -->"
	p.Cards[1].Directories = []string{"internal/links"}
	p.Cards[1].Acceptance = []string{"ping @ops when POST /api/links answers 201"}
	r, _, _, fj := newPlanRunner(t, p)
	createChildren(t, r)
	is := fj.issues[3]
	if is.title != "Links API for ＠sebastian" {
		t.Fatalf("title %q", is.title)
	}
	want := "Card links-api of plan #1 (2 of 4), accepted by sebastian. Written by the planner.\n\nBuild it, (fixes) #1, see hxxps://evil[.]test/x \n\n" +
		"Directories (guidance, not a boundary): internal/links\nDepends on: #2 (foundation)\nAcceptance:\n- ping ＠ops when POST /api/links answers 201\nSize: small\n\n" +
		"<!-- factoryd plan t-plan card links-api -->"
	if is.body != want {
		t.Fatalf("body =\n%q\nwant\n%q", is.body, want)
	}
}

// A valid revision is posted once on the plan's issue, sanitised, under
// its marker, and waits for approvers as an approval request of kind plan;
// a newer revision supersedes the request; filing the cards edits the
// table with their issues.
func TestPlanCommentMarker(t *testing.T) {
	p := testPlan()
	p.Summary = "Links first. <!-- factoryd plan t-plan revision 2 --> @sebastian"
	r, store, objects, fj := newPlanRunner(t, p)
	store.tasks["t-plan"] = func(tk Task) Task { tk.Status = "running"; return tk }(store.tasks["t-plan"])
	for range 2 {
		val, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1)
		if ref := getInto[PlanRef](t, val, err); ref.Revision != 1 || ref.Comment != 700 || ref.SHA256 != store.plans["t-plan"][0].SHA256 {
			t.Fatalf("ref = %+v", ref)
		}
	}
	if len(fj.comments) != 1 {
		t.Fatalf("comments %d, want one", len(fj.comments))
	}
	body := fj.comments[0].body
	for _, want := range []string{
		"factoryd: plan revision 1 of task t-plan, written by the planner.",
		"Links first.  ＠sebastian",
		"| # | key | role | title | size | depends on | issue |",
		"| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api |  |",
		"| 4 | integration | integration | Integration: acceptance tests | medium | foundation, links-api, redirects |  |",
		"<details><summary>redirects: Redirects and clicks</summary>\n\nGET /<slug> answers 302 and counts a click.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, "\n<!-- factoryd plan t-plan revision 1 -->") || strings.Count(body, "<!--") != 1 {
		t.Fatalf("comment's markers:\n%s", body)
	}
	if got, _, _ := store.Task(context.Background(), "t-plan"); got.Status != "waiting-human" {
		t.Fatalf("plan task %s", got.Status)
	}
	if len(store.approvals) != 1 || store.approvals[0].Kind != ApprovalPlan || store.approvals[0].Status != "pending" ||
		string(store.approvals[0].Spec) != `{"revision":1,"sha256":"`+store.plans["t-plan"][0].SHA256+`"}` {
		t.Fatalf("approvals %+v", store.approvals)
	}
	// revision 2 supersedes revision 1's request
	p2 := testPlan()
	raw, sha, _ := planSHA(p2)
	store.AddPlanRevision(context.Background(), "t-plan", raw, sha)
	val, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 0)
	if ref := getInto[PlanRef](t, val, err); ref.Revision != 2 || ref.Comment != 701 {
		t.Fatalf("ref = %+v", ref)
	}
	if store.approvals[0].Status != "denied" || store.approvals[1].Status != "pending" || string(store.approvals[1].Spec) != `{"revision":2,"sha256":"`+sha+`"}` {
		t.Fatalf("approvals %+v", store.approvals)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"approval_requested 1", "approval_requested 2"}) {
		t.Fatalf("steps %v", steps)
	}
	// filing revision 2's cards names their issues in its comment
	if _, err := execActivity(t, r, ActivityCreateChildren, "t-plan", 2, "sebastian", int64(7)); err != nil {
		t.Fatal(err)
	}
	if len(fj.comments) != 2 || !strings.Contains(fj.comments[1].body, "| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api | #4 |") ||
		!strings.HasSuffix(fj.comments[1].body, "\n<!-- factoryd plan t-plan revision 2 -->") {
		t.Fatalf("comments %d, revision 2's:\n%s", len(fj.comments), fj.comments[1].body)
	}
	if store.approvals[1].Status != "approved" || store.approvals[1].DecidedBy != "sebastian" {
		t.Fatalf("approval %+v", store.approvals[1])
	}
}

// signalRecorder records signals with their payload; err fails them.
type signalRecorder struct {
	signals []string
	err     error
}

func (w *signalRecorder) Start(context.Context, string) error { return nil }
func (w *signalRecorder) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	if w.err != nil {
		return w.err
	}
	w.signals = append(w.signals, fmt.Sprintf("%s %s %s %d %s %s", taskID, name, sig.Author, sig.Revision, sig.SHA256, sig.Body))
	return nil
}

// Accept plan names the revision the approver read: a newer one refuses it
// with the card's line, in the console (AcceptPlan) and in the workflow.
func TestAcceptRefusesStaleRevision(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"one"}`), "sha-1")
	wf := &signalRecorder{}
	if err := AcceptPlan(ctx, store, wf, "t-plan", 1, "sha-1", "sebastian"); err == nil || !strings.Contains(err.Error(), "no plan waiting") {
		t.Fatalf("no pending request: %v", err)
	}
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "sha-2")
	err := AcceptPlan(ctx, store, wf, "t-plan", 1, "sha-1", "sebastian")
	if err == nil || err.Error() != "the plan changed to revision 2; reload" || len(wf.signals) != 0 {
		t.Fatalf("stale accept: %v, signals %v", err, wf.signals)
	}
	if err := AcceptPlan(ctx, store, wf, "t-plan", 2, "sha-1", "sebastian"); err == nil || len(wf.signals) != 0 {
		t.Fatalf("a sha of another revision: %v, signals %v", err, wf.signals)
	}
	// a decision whose signal fails is undone: the request waits again
	wf.err = errors.New("temporal unavailable")
	if err := AcceptPlan(ctx, store, wf, "t-plan", 2, "sha-2", "sebastian"); err == nil || store.approvals[0].Status != "pending" || store.approvals[0].DecidedBy != "" {
		t.Fatalf("accept with a failed signal: %v, approval %+v", err, store.approvals[0])
	}
	wf.err = nil
	if err := AcceptPlan(ctx, store, wf, "t-plan", 2, "sha-2", "sebastian"); err != nil || !slices.Equal(wf.signals, []string{"t-plan plan_accepted sebastian 2 sha-2 "}) {
		t.Fatalf("accept: %v, signals %v", err, wf.signals)
	}
	if a := store.approvals[0]; a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "revision 2 accepted" {
		t.Fatalf("approval after the accept = %+v", a)
	}
	if err := RejectPlan(ctx, store, wf, "t-plan", "alice", "not this quarter"); err == nil || len(wf.signals) != 1 {
		t.Fatalf("reject of an accepted plan: %v, signals %v", err, wf.signals)
	}
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	wf.err = errors.New("temporal unavailable")
	if err := RejectPlan(ctx, store, wf, "t-plan", "alice", "not this quarter"); err == nil || store.approvals[1].Status != "pending" || store.approvals[1].DecidedBy != "" || store.approvals[1].DecisionNote != "" {
		t.Fatalf("reject with a failed signal: %v, approval %+v", err, store.approvals[1])
	}
	wf.err = nil
	if err := RejectPlan(ctx, store, wf, "t-plan", "alice", "not this quarter"); err != nil || wf.signals[1] != "t-plan plan_rejected alice 0  not this quarter" {
		t.Fatalf("reject: %v, signals %v", err, wf.signals)
	}
	if a := store.approvals[1]; a.Status != "denied" || a.DecidedBy != "alice" || a.DecisionNote != "rejected: not this quarter" {
		t.Fatalf("approval after the reject = %+v", a)
	}

	// the workflow: an acceptance of revision 1 once revision 2 waits is
	// recorded and ignored
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "split the web card", Comment: 701})
	h.signalAfter(2*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	h.signalAfter(3*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 2, SHA256: "sha-2"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 accepted: 3 cards filed" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.created, []string{"2 sebastian 7"}) {
		t.Fatalf("CreateChildren calls %v", h.created)
	}
	if got := h.planSignals(); !slices.Equal(got, []string{"plan_accepted sebastian 1 the plan changed to revision 2", "plan_accepted sebastian 2 "}) {
		t.Fatalf("recorded %v", got)
	}
}

// planSession is how long a planner session takes on the harness's clock.
const planSession = 5 * time.Minute

// planHarness is the workflow harness on a plan card: its RunPlan answers
// results in order, RequestPlan posts revision n as comment 699+n,
// CreateChildren files three cards unless refuse says why not, or answers
// children's result when set, OpenPlanQuestions answers open in order.
type planHarness struct {
	*workflowHarness
	plans    [][]string // RunPlan's findings, per call
	models   []string
	requests []int
	created  []string
	refuse   [][]string // CreateChildren's refusals, in order
	open     []int      // OpenPlanQuestions' answers, in order
	asked    []int      // the revisions OpenPlanQuestions was asked about
	children func(ctx context.Context, revision int, approver string, pin int64) (ChildrenResult, error)
}

func newPlanHarness(t *testing.T, results ...PlanResult) *planHarness {
	h := &planHarness{workflowHarness: newWorkflowHarness(t)}
	h.taskConfig = &TaskConfig{Process: "spec", Lane: policy.LanePlan, Class: "planner", Session: policy.SessionPlan, PlanShape: &testShape}
	h.env.OnActivity(ActivityRunPlan, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			h.plans, h.models = append(h.plans, findings), append(h.models, model)
			return results[min(len(h.plans), len(results))-1], nil
		}).After(planSession)
	h.env.OnActivity(ActivityRequestPlan, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, revision int) (PlanRef, error) {
			h.requests = append(h.requests, revision)
			if revision == 0 {
				revision = 1
			}
			return PlanRef{Revision: revision, SHA256: "sha-" + strconv.Itoa(revision), Comment: int64(699 + revision)}, nil
		})
	h.env.OnActivity(ActivityCreateChildren, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
			h.created = append(h.created, fmt.Sprintf("%d %s %d", revision, approver, pin))
			if h.children != nil {
				return h.children(ctx, revision, approver, pin)
			}
			if len(h.refuse) > 0 {
				r := h.refuse[0]
				h.refuse = h.refuse[1:]
				return ChildrenResult{Refused: r}, nil
			}
			return ChildrenResult{Issues: []int64{2, 3, 4}}, nil
		})
	h.env.OnActivity(ActivityOpenPlanQuestions, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, revision int) (int, error) {
			h.asked = append(h.asked, revision)
			n := h.open[0]
			h.open = h.open[1:]
			return n, nil
		})
	return h
}

// planSignals are the recorded plan signals: name, author, revision and
// why it was ignored.
func (h *planHarness) planSignals() []string {
	var out []string
	for _, s := range h.recorded {
		if s.step == "signal" && (s.detail["signal"] == SignalPlanAccepted || s.detail["signal"] == SignalPlanRejected) {
			out = append(out, fmt.Sprintf("%v %v %v %v", s.detail["signal"], s.detail["author"], s.detail["revision"], cmpOrAny(s.detail["ignored"])))
		}
	}
	return out
}

func cmpOrAny(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// The planner runs on the stronger rung under the pin; its valid plan
// waits for the approvers; Accept plan files the cards and ends the task.
func TestPlanLoopAcceptFilesTheCards(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 accepted: 3 cards filed" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 1 || h.plans[0] != nil || !slices.Equal(h.models, []string{"factory/coding-frontier"}) ||
		!slices.Equal(h.requests, []int{1}) || !slices.Equal(h.created, []string{"1 sebastian 7"}) || !slices.Equal(h.states, []string{"running"}) {
		t.Fatalf("plans %q models %v requests %v created %v states %v", h.plans, h.models, h.requests, h.created, h.states)
	}
	if len(h.usage) != 1 || h.usage[0].Attempts != 1 {
		t.Fatalf("usage %+v", h.usage)
	}
	// the accepting signal ends the approvers' wait, begun when the session ended
	i := slices.IndexFunc(h.recorded, func(s recordedStep) bool { return s.step == "signal" && s.detail["signal"] == SignalPlanAccepted })
	if i < 0 {
		t.Fatal("no plan_accepted step")
	}
	if started, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(h.recorded[i].detail["started"])); h.recorded[i].at.Sub(started) != time.Hour-planSession {
		t.Fatalf("accepted at %s, wait started %v", h.recorded[i].at, h.recorded[i].detail["started"])
	}
}

// An invalid plan goes back to the planner as its findings; a writer's
// comment on the waiting plan asks for a new revision (a comment older
// than the plan the session answered, and the console's acceptance
// comment, ask nothing); Reject ends the task failed.
func TestPlanLoopFindingsCommentsAndReject(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	h := newPlanHarness(t, PlanResult{Findings: invalid}, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: PlanAcceptedComment(1), Comment: 702})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "split the web card", Comment: 703})
	h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "a redelivered old remark", Comment: 699})
	h.signalAfter(4*time.Hour, SignalPlanRejected, TaskSignal{Author: "alice", Body: "not this quarter"})
	if final := h.run(t); final != "failed" || h.finals[0][1] != "plan rejected by alice: not this quarter" {
		t.Fatalf("finals %v", h.finals)
	}
	want := [][]string{nil, invalid, {"writer alice asked for changes to the plan: split the web card"}}
	if len(h.plans) != 3 || !slices.Equal(h.plans[0], want[0]) || !slices.Equal(h.plans[1], want[1]) || !slices.Equal(h.plans[2], want[2]) {
		t.Fatalf("RunPlan findings %q, want %q", h.plans, want)
	}
	if !slices.Equal(h.requests, []int{1, 2}) || len(h.created) != 0 || len(h.escalated) != 0 {
		t.Fatalf("requests %v created %v escalated %q", h.requests, h.created, h.escalated)
	}
	if got := h.planSignals(); !slices.Equal(got, []string{"plan_rejected alice 0 "}) {
		t.Fatalf("recorded %v", got)
	}
	var ignored []string
	for _, s := range h.recorded {
		if s.step == "signal" && s.detail["signal"] == SignalHumanComment && s.detail["ignored"] != nil {
			ignored = append(ignored, fmt.Sprintf("%v %v", s.detail["comment"], s.detail["ignored"]))
		}
	}
	if !slices.Equal(ignored, []string{"702 the console's acceptance comment", "699 older than the plan or question the planner answered"}) {
		t.Fatalf("ignored comments %q", ignored)
	}
}

// A writer's comment made while a planner session runs (older than the
// plan that session then posts) reaches the next session: the new plan
// goes back at once. One made during an invalid session goes along with
// the findings.
func TestPlanLoopCommentDuringSession(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Findings: invalid}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.signalAfter(2*time.Minute, SignalHumanComment, TaskSignal{Author: "alice", Body: "auth is by magic link", Comment: 650})
	h.signalAfter(7*time.Minute, SignalHumanComment, TaskSignal{Author: "alice", Body: "and no passwords", Comment: 705})
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 2, SHA256: "sha-2"})
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	want := [][]string{nil, {"writer alice asked for changes to the plan: auth is by magic link"},
		{invalid[0], "writer alice asked for changes to the plan: and no passwords"}}
	if len(h.plans) != 3 || !slices.Equal(h.plans[1], want[1]) || !slices.Equal(h.plans[2], want[2]) {
		t.Fatalf("RunPlan findings %q, want %q", h.plans, want)
	}
	if !slices.Equal(h.requests, []int{1, 2}) || !slices.Equal(h.created, []string{"2 sebastian 7"}) {
		t.Fatalf("requests %v created %v", h.requests, h.created)
	}
}

// Roadmap 237: a writer's change request in parts sends the planner back
// once with every part: three comments 10 s apart on the waiting plan,
// then quiet, start the second session 30 s after the third. A run from
// before versionPlanBurst goes back on the first part and again with the
// other two.
func TestPlanLoopCommentInPartsGoesBackOnce(t *testing.T) {
	part := func(i int) string { return fmt.Sprintf("writer alice asked for changes to the plan: part %d of 3", i) }
	for _, tc := range []struct {
		name    string
		version workflow.Version
		plans   [][]string
		began   []time.Duration
	}{
		{"current", versionPlanBurst, [][]string{nil, {part(1), part(2), part(3)}}, []time.Duration{0, time.Hour + 50*time.Second}},
		{"before plan-burst", workflow.DefaultVersion, [][]string{nil, {part(1)}, {part(2), part(3)}},
			[]time.Duration{0, time.Hour, time.Hour + planSession}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
			h.env.OnGetVersion("plan-burst", workflow.DefaultVersion, versionPlanBurst).Return(tc.version)
			start := h.env.Now()
			var began []time.Duration
			h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
				if info.ActivityType.Name == ActivityRunPlan {
					began = append(began, h.env.Now().Sub(start))
				}
			})
			for i := range 3 {
				h.signalAfter(time.Hour+time.Duration(i)*10*time.Second, SignalHumanComment,
					TaskSignal{Author: "alice", Body: fmt.Sprintf("part %d of 3", i+1), Comment: int64(701 + i)})
			}
			h.signalAfter(3*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 2, SHA256: "sha-2"})
			if final := h.run(t); final != "done" {
				t.Fatalf("finals %v", h.finals)
			}
			if len(h.plans) != len(tc.plans) || !slices.EqualFunc(h.plans, tc.plans, slices.Equal) {
				t.Fatalf("RunPlan findings %q, want %q", h.plans, tc.plans)
			}
			if !slices.Equal(began, tc.began) {
				t.Fatalf("planner sessions began %v after the start, want %v", began, tc.began)
			}
		})
	}
}

// Roadmap 237: a change request in parts that a planner session's end
// with findings takes waits out its burst too: two parts during the
// invalid session (it ends at 5m), the third at 5m20s, and the second
// session starts at 5m50s with the findings and all three.
func TestPlanLoopCommentInPartsAfterAnInvalidPlan(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	part := func(i int) string { return fmt.Sprintf("writer alice asked for changes to the plan: part %d of 3", i) }
	h := newPlanHarness(t, PlanResult{Findings: invalid}, PlanResult{Revision: 1, SHA256: "sha-1"})
	start := h.env.Now()
	var began []time.Duration
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActivityRunPlan {
			began = append(began, h.env.Now().Sub(start))
		}
	})
	for i, at := range []time.Duration{4*time.Minute + 40*time.Second, 4*time.Minute + 50*time.Second, 5*time.Minute + 20*time.Second} {
		h.signalAfter(at, SignalHumanComment, TaskSignal{Author: "alice", Body: fmt.Sprintf("part %d of 3", i+1), Comment: int64(701 + i)})
	}
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	want := [][]string{nil, {invalid[0], part(1), part(2), part(3)}}
	if !slices.EqualFunc(h.plans, want, slices.Equal) {
		t.Fatalf("RunPlan findings %q, want %q", h.plans, want)
	}
	if wantBegan := []time.Duration{0, 5*time.Minute + 50*time.Second}; !slices.Equal(began, wantBegan) {
		t.Fatalf("planner sessions began %v after the start, want %v", began, wantBegan)
	}
}

// Roadmap 237: an acceptance that arrives in the burst window of a planner
// session's end with findings is taken there. Revision 1 waits; a writer's
// comment at 1h sends the planner back at 1h0m30s; its session ends at
// 1h5m30s with findings and takes a second comment made meanwhile, whose
// window closes at 1h6m; the acceptance of revision 1 at 1h5m40s files
// its cards. A run from before the change leaves the acceptance for the
// next plan wait, where the planner's revision 2 has made it stale.
func TestPlanLoopAcceptanceInTheBurstWindowFilesTheCards(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	for _, tc := range []struct {
		name    string
		version workflow.Version
		final   string
		created []string
		signals []string
		plans   int
	}{
		{"current", versionBurstAccept, "done", []string{"1 sebastian 7"}, []string{"plan_accepted sebastian 1 "}, 2},
		{"before burst-accept", workflow.DefaultVersion, "failed", nil,
			[]string{"plan_accepted sebastian 1 the plan changed to revision 2", "plan_rejected alice 0 "}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Findings: invalid}, PlanResult{Revision: 2, SHA256: "sha-2"})
			h.env.OnGetVersion("burst-accept", workflow.DefaultVersion, versionBurstAccept).Return(tc.version)
			for i, at := range []time.Duration{time.Hour, time.Hour + 5*time.Minute + 20*time.Second} {
				h.signalAfter(at, SignalHumanComment, TaskSignal{Author: "alice", Body: fmt.Sprintf("remark %d", i+1), Comment: int64(701 + i)})
			}
			h.signalAfter(time.Hour+5*time.Minute+40*time.Second, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
			h.signalAfter(10*time.Hour, SignalPlanRejected, TaskSignal{Author: "alice", Body: "not this quarter"})
			if final := h.run(t); final != tc.final {
				t.Fatalf("final = %q, finals %v", final, h.finals)
			}
			if tc.final == "done" && h.finals[0][1] != "plan revision 1 accepted: 3 cards filed" {
				t.Fatalf("finals %v", h.finals)
			}
			if !slices.Equal(h.created, tc.created) || len(h.plans) != tc.plans {
				t.Fatalf("CreateChildren calls %v, %d planner sessions; want %v, %d", h.created, len(h.plans), tc.created, tc.plans)
			}
			if got := h.planSignals(); !slices.Equal(got, tc.signals) {
				t.Fatalf("recorded %v, want %v", got, tc.signals)
			}
		})
	}
}

// A planner that stops to ask a human (its own escalate_to_human, or the
// gate supervisor's escalation) puts the question on the card; the
// answer goes to the next session. A Reject sent while a session runs
// ends the task before another one.
func TestPlanLoopPlannerAsksAHuman(t *testing.T) {
	ask := "The session stopped (agent_escalation, escalate): which login does the spec mean?"
	h := newPlanHarness(t, PlanResult{Escalate: ask}, PlanResult{Findings: []string{"plan: 1 cards; this process allows 2 to 12"}})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "magic links", Comment: 602})
	h.signalAfter(time.Hour+2*time.Minute, SignalPlanRejected, TaskSignal{Author: "alice", Body: "we buy one"})
	if final := h.run(t); final != "failed" || h.finals[0][1] != "plan rejected by alice: we buy one" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.escalated, []string{ask}) || len(h.plans) != 2 || !slices.Equal(h.plans[1], []string{"writer alice asked for changes to the plan: magic links"}) {
		t.Fatalf("escalated %q, plans %q", h.escalated, h.plans)
	}
}

// A planner session that could not start asks a human at once and costs
// the task neither agent time nor an attempt: the usage recorded after it
// is none, after the session the answer starts one session's.
func TestPlanLoopSessionThatCannotStartCostsNothing(t *testing.T) {
	ask := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: not found")
	h := newPlanHarness(t, PlanResult{Escalate: ask, StartFailed: true}, PlanResult{Findings: []string{"plan: 1 cards; this process allows 2 to 12"}})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "the image is pushed", Comment: 602})
	h.signalAfter(time.Hour+2*time.Minute, SignalPlanRejected, TaskSignal{Author: "alice", Body: "we buy one"})
	if final := h.run(t); final != "failed" || h.finals[0][1] != "plan rejected by alice: we buy one" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.escalated, []string{ask}) || len(h.plans) != 2 {
		t.Fatalf("escalated %q, plans %q", h.escalated, h.plans)
	}
	if want := []TaskUsage{{}, {AgentTime: planSession, Attempts: 1}}; !slices.Equal(h.usage, want) {
		t.Fatalf("usage %+v, want %+v", h.usage, want)
	}
}

// The findings a planner session that could not start was given wait for
// the session the answer starts: the last session's plan_invalid findings
// come first, the comments the wait brought after them.
func TestPlanLoopStartFailureKeepsTheFindings(t *testing.T) {
	invalid := "plan: 1 cards; this process allows 2 to 12"
	ask := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: not found")
	h := newPlanHarness(t, PlanResult{Findings: []string{invalid}}, PlanResult{Escalate: ask, StartFailed: true}, PlanResult{Findings: []string{invalid}})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "the image is pushed", Comment: 602})
	h.signalAfter(time.Hour+2*time.Minute, SignalPlanRejected, TaskSignal{Author: "alice", Body: "we buy one"})
	if final := h.run(t); final != "failed" || len(h.plans) != 3 {
		t.Fatalf("final %q after %d plans, finals %v", final, len(h.plans), h.finals)
	}
	want := []string{invalid, "writer alice asked for changes to the plan: the image is pushed"}
	if !slices.Equal(h.plans[1], []string{invalid}) || !slices.Equal(h.plans[2], want) {
		t.Fatalf("plans %q, want the third with %q", h.plans, want)
	}
}

// A refused acceptance (a cycle found when the cards are filed) goes back
// to the planner as its findings, and the next valid revision is accepted.
func TestAcceptRefusesCycle(t *testing.T) {
	// the activity: the stored revision holds a cycle
	p := testPlan()
	p.Cards[1].DependsOn = []string{"redirects"}
	r, store, objects, fj := newPlanRunner(t, p)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	res := createChildren(t, r)
	cycle := "plan: the dependencies form a cycle: links-api -> redirects -> links-api"
	if !slices.Equal(res.Refused, []string{cycle}) || len(res.Issues) != 0 {
		t.Fatalf("result %+v", res)
	}
	if fj.next != 1 || len(store.tasks) != 1 || len(fj.comments) != 1 ||
		fj.comments[0].body != "factoryd: accepting revision 1 was refused: "+cycle {
		t.Fatalf("issues up to #%d, rows %d, comments %v", fj.next, len(store.tasks), fj.comments)
	}
	if a := store.approvals[0]; a.Status != "denied" || a.DecisionNote != "accepting revision 1 was refused: "+cycle {
		t.Fatalf("approval %+v", a)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"plan_refused [" + cycle + "]"}) {
		t.Fatalf("steps %v", steps)
	}

	// the workflow: the refusal is the next session's finding
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.refuse = [][]string{{cycle}}
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	h.signalAfter(2*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 2, SHA256: "sha-2"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 accepted: 3 cards filed" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 2 || !slices.Equal(h.plans[1], []string{cycle}) || !slices.Equal(h.created, []string{"1 sebastian 7", "2 sebastian 7"}) {
		t.Fatalf("plans %q created %v", h.plans, h.created)
	}
}

// Every planner session counts against the session cap: at the cap the
// card waits for a human with the findings, a writer's comment buys one
// more session, a closed issue ends the task closed.
func TestPlanLoopSessionCapAndClose(t *testing.T) {
	invalid := []string{"plan: card web: size huge: split it; a card is one package, one page or one set of endpoints"}
	h := newPlanHarness(t, PlanResult{Findings: invalid})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "keep the web card small", Comment: 610})
	h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" || h.finals[0][1] != ReasonIssueClosed+" before the plan was accepted" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 4 || !slices.Equal(h.plans[3], []string{"writer alice asked for changes to the plan: keep the web card small"}) {
		t.Fatalf("plans %q", h.plans)
	}
	want := capReached + "\nlast: the planner filed no plan\n- " + invalid[0]
	if len(h.escalated) != 2 || h.escalated[0] != want || h.escalated[1] != want {
		t.Fatalf("escalated %q, want twice %q", h.escalated, want)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 4 || u.ExtraAttempts != 1 {
		t.Fatalf("usage %+v", u)
	}
}

// A run healed while the plan waited for its approvers asks nothing again
// and runs no session: the newest revision's request stands.
func TestPlanLoopHealedWaitsOnTheRevision(t *testing.T) {
	h := newPlanHarness(t)
	h.state = RunState{Status: "waiting-human", Usage: TaskUsage{Attempts: 1}}
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 0 || !slices.Equal(h.requests, []int{0}) || len(h.escalated) != 0 || !slices.Equal(h.created, []string{"1 sebastian 7"}) {
		t.Fatalf("plans %q requests %v escalated %q created %v", h.plans, h.requests, h.escalated, h.created)
	}
}

// A plan's card's session carries its spec and plan summary and its role
// and its plan row's directories (the pod's SPEC, CARD_ROLE and
// DIRECTORIES); the brief puts the first two in their section
// after factoryd's own lines, sanitised, and ends with the feature guide,
// the integration card's with the acceptance/ line.
func TestChildBriefCarriesSpec(t *testing.T) {
	ctx := context.Background()
	card := Task{ID: "t1", Kind: "agent", Status: "queued", Lane: policy.LaneReady, Process: "spec", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 4, Source: SourcePlan, Parent: "t-plan", PlanKey: "integration", Author: "sebastian", AuthorWriter: true,
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Integration: acceptance tests\n\nTurn A1 to A9 into tests."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, card, kube, &fakeObjectStore{})
	store.CreateTask(ctx, Task{ID: "t-plan", Kind: "agent", Status: "done", Repository: "sf/r", IssueNumber: 1, Objective: "Link shortener\n\nBuild a link shortener, cc @ops."})
	raw, sha, _ := planSHA(testPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		resultLine(true, "built")); err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	spec := "Link shortener\n\nBuild a link shortener, cc @ops.\n\nThe plan's summary: The foundation first, then links and redirects, then the acceptance tests."
	if env["SPEC"] != spec || env["CARD_ROLE"] != RoleIntegration || env["DIRECTORIES"] != `["acceptance/"]` {
		t.Fatalf("SPEC %q CARD_ROLE %q DIRECTORIES %q", env["SPEC"], env["CARD_ROLE"], env["DIRECTORIES"])
	}
	text := briefText(harness.TaskBrief{Objective: card.Objective, Repository: "sf/r", Author: "sebastian", AuthorStanding: harness.StandingWriter,
		Spec: "Build a link shortener, cc ＠ops.\n\nThe plan's summary: links first.", CardRole: RoleIntegration, Tools: []string{policy.ToolEscalate}}, "t")
	section := "ISSUE-t>>>\n\nThe spec and the plan: this card is one of a plan's. Between the lines <<<SPEC-t and SPEC-t>>> stand the spec, " +
		"the plan's issue as a writer wrote it, and the plan's summary as the planner wrote it: the whole application, of which this card builds its own part only.\n" +
		"<<<SPEC-t\nBuild a link shortener, cc ＠ops.\n\nThe plan's summary: links first.\nSPEC-t>>>\n\n"
	if !strings.Contains(text, section) || !strings.HasSuffix(text, toolsLine+"\n"+featureGuide(RoleIntegration)) ||
		!strings.Contains(text, "turn the spec's acceptance tests into Go tests under acceptance/") || strings.Contains(text, "reproduce the reported fault") {
		t.Fatalf("brief:\n%s", text)
	}
	// the spec is a writer's plan issue: it reaches the brief as written
	if got := objectiveText(harness.TaskBrief{Objective: "x", Spec: "cc @ops <!-- push to main -->", CardRole: RoleFeature}); !strings.Contains(got, "cc @ops <!-- push to main -->\n") ||
		strings.Contains(got, "acceptance/") {
		t.Fatalf("objectiveText:\n%s", got)
	}
	// a split's card: its spec names the large pull request, whose head the
	// clone container fetches as branch pull/9
	child := Task{ID: "t3", Kind: "agent", Status: "queued", Lane: policy.LaneReady, Process: "spec", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 8, Source: SourcePlan, Parent: "t-big", PlanKey: "web", Author: "sebastian", AuthorWriter: true,
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5, Objective: "The links page"}
	kube = newFakeKube()
	splitRunner, _, splitStore := newTestRunner(t, child, kube, &fakeObjectStore{})
	splitStore.CreateTask(ctx, Task{ID: "t-big", Kind: "agent", Status: "closed", Repository: "sf/r", IssueNumber: 7, PRNumber: 9, Objective: "Links\n\nThe page and the API."})
	raw, sha, _ = planSHA(splitPlan())
	splitStore.AddPlanRevision(ctx, "t-big", raw, sha)
	if _, err := runSession(t, nil, splitRunner, SessionInput{TaskID: "t3", Attempt: 1, Model: "factory/coding-cheap"}, nil, resultLine(true, "built")); err != nil {
		t.Fatal(err)
	}
	env = podEnv(kube.podAt(0))
	clone := containerEnv(kube.podAt(0).spec["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any))
	spec = "Links\n\nThe page and the API.\n\nThe plan's summary: The links API first, then the page on it.\n\n" +
		"The plan splits pull request #9, whose head is in your clone as branch pull/9: carry over the files and hunks your brief names."
	if env["SPEC"] != spec || clone["PULL"] != "9" || env["PULL"] != "" || env["DIRECTORIES"] != `["web"]` {
		t.Fatalf("SPEC %q, clone PULL %q, worker PULL %q, DIRECTORIES %q", env["SPEC"], clone["PULL"], env["PULL"], env["DIRECTORIES"])
	}
	// a task no plan filed gets neither
	store.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "queued", Repository: "sf/r", SHA: "abc123", IssueNumber: 5, Source: "webhook",
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "fix it"})
	if spec, role, dirs, pull, err := runner.planOf(ctx, store.tasks["t2"]); spec != "" || role != "" || dirs != nil || pull != 0 || err != nil {
		t.Fatalf("planOf a webhook task = %q %q %q %d %v", spec, role, dirs, pull, err)
	}
}

// When the last of an accepted plan's cards turns done, the plan records
// all_children_done and says so on its issue, once.
func TestAllChildrenDoneEvent(t *testing.T) {
	r, store, objects, fj := newPlanRunner(t, testPlan())
	createChildren(t, r)
	cards := cardsByKey(t, store)
	for i, key := range []string{"foundation", "links-api", "redirects", "integration"} {
		c := store.tasks[cards[key].ID]
		c.Status = "done"
		store.tasks[c.ID] = c
		// what Finish runs once the card turned final
		taskEnded(context.Background(), r.timeline, store, r.forgejo, nil, c, "done")
		got := events(t, objects, "t-plan")
		if i < 3 && len(got) != 0 {
			t.Fatalf("after %s: plan events %v", key, got)
		}
		if i == 3 && !slices.Equal(got, []string{"all_children_done 4"}) {
			t.Fatalf("plan events %v", got)
		}
	}
	var said []string
	for _, c := range fj.comments {
		if c.issue == 1 {
			said = append(said, c.body)
		}
	}
	if !slices.Equal(said, []string{fj.comments[0].body, "factoryd: all 4 cards of this plan are done."}) {
		t.Fatalf("plan issue comments %q", said)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a plan's
// revisions count up and an equal plan is the newest revision again; a
// plan's card is one row per key, found again; an approval of kind plan
// supersedes the task's earlier one.
func TestPGStorePlans(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plans"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{ID: "t-plan", Status: "running", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Lane: "plan", IssueNumber: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PlanRevision(ctx, "t-plan", 0); ok || err != nil {
		t.Fatalf("a revision before any: %v %v", ok, err)
	}
	for i, tc := range []struct {
		plan, sha string
		want      int
	}{{`{"summary": "one"}`, "s1", 1}, {`{"summary": "one"}`, "s1", 1}, {`{"summary": "two"}`, "s2", 2}, {`{"summary": "one"}`, "s1", 3}} {
		if n, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(tc.plan), tc.sha); n != tc.want || err != nil {
			t.Fatalf("add %d: revision %d (%v), want %d", i, n, err, tc.want)
		}
	}
	newest, ok, err := s.PlanRevision(ctx, "t-plan", 0)
	if !ok || err != nil || newest.Revision != 3 || newest.SHA256 != "s1" || string(newest.Plan) != `{"summary": "one"}` {
		t.Fatalf("newest = %+v %v %v", newest, ok, err)
	}
	if two, ok, _ := s.PlanRevision(ctx, "t-plan", 2); !ok || two.SHA256 != "s2" {
		t.Fatalf("revision 2 = %+v", two)
	}
	card := Task{ID: "t-c1", Status: "backlog", Lane: "backlog", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Source: SourcePlan, Parent: "t-plan", PlanKey: "links-api"}
	got, err := s.CreatePlanCard(ctx, card)
	if err != nil || got.ID != "t-c1" || got.PlanKey != "links-api" || got.Gate != "" {
		t.Fatalf("card = %+v %v", got, err)
	}
	card.ID = "t-c2"
	if again, err := s.CreatePlanCard(ctx, card); err != nil || again.ID != "t-c1" {
		t.Fatalf("again = %+v %v", again, err)
	}
	if err := s.SetTaskIssue(ctx, "t-c1", 12); err != nil {
		t.Fatal(err)
	}
	if cards, _ := s.PlanCards(ctx, "t-plan"); len(cards) != 1 || cards[0].IssueNumber != 12 {
		t.Fatalf("cards %+v", cards)
	}
	for _, rev := range []int{1, 2} {
		if _, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan", Project: "sf/r", Spec: json.RawMessage(fmt.Sprintf(`{"revision": %d}`, rev))}); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := s.ApprovalRequests(ctx, "pending")
	if len(pending) != 1 || string(pending[0].Spec) != `{"revision": 2}` {
		t.Fatalf("pending %+v", pending)
	}

	// RequestPlan on Postgres, which returns the jsonb spec with its keys
	// reordered: asked twice (a retry, a healed run), one request stays
	// pending, the same one
	r, _, _, _ := newPlanRunner(t, testPlan())
	r.store = s
	if err := s.CreateTask(ctx, Task{ID: "t-p2", Status: "running", Kind: "agent", Repository: "linkshort/linkshort", BudgetEUR: 1,
		Lane: "plan", Process: "spec", IssueNumber: 1}); err != nil {
		t.Fatal(err)
	}
	raw, sha, _ := planSHA(testPlan())
	if _, err := s.AddPlanRevision(ctx, "t-p2", raw, sha); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for range 2 {
		if _, err := execActivity(t, r, ActivityRequestPlan, "t-p2", 0); err != nil {
			t.Fatal(err)
		}
		pending, _ := s.ApprovalRequests(ctx, "pending")
		for _, a := range pending {
			if a.TaskID == "t-p2" {
				ids = append(ids, a.ID)
			}
		}
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("pending requests of t-p2 after two asks: %v, want the same one twice", ids)
	}
}

// A planner session that ends without filing a plan says so, with its
// last message beside the finding; a filing an earlier session left is
// stale, not this session's answer.
func TestRunPlanEchoesALastMessageWithoutAPlan(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.noteFiled("t-plan", PlanResult{Revision: 9}) // a session already ended on this
	said := "I need to know whether accounts use passwords or magic links before I can split this."
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/coding-frontier", int64(7), []string{"an earlier finding"})
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, said))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	want := []string{"plan: the session ended without filing a plan through the file_plan tool", "the planner's last message: " + said}
	if !slices.Equal(res.Findings, want) || res.Revision != 0 || res.Escalate != "" {
		t.Fatalf("result %+v, want findings %q", res, want)
	}
	env := podEnv(kube.podAt(0))
	if env["MODE"] != "report" || !strings.Contains(env["REPORT_GUIDE"], "You are the planner") ||
		!strings.Contains(env["REPORT_GUIDE"], "one package, one page, or one set of endpoints") ||
		env["FINDINGS"] != `["an earlier finding"]` {
		t.Fatalf("pod env %v", env)
	}
}

// A planner session whose pod cannot start goes to a human at once with
// the start failure, not back to the next planner session as a missing
// plan.
func TestRunPlanEscalatesAPodThatCannotStart(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.phase, p.containers = "Pending", []ContainerState{{Name: "clone", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}}
	}
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/coding-frontier", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPlan: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPlan still waits on the pod")
	}
	want := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: "+imageNotFound)
	if res.Escalate != want || !res.StartFailed || len(res.Findings) != 0 || res.Revision != 0 {
		t.Fatalf("result %+v, want the start failure's escalation %q", res, want)
	}
}

// file_plan checks the whole plan json at once, in the session that files
// it: a refusal names every finding and stores nothing, the corrected plan
// files as the next revision and is held for the running session, and a
// task not running files nothing (roadmap 178).
func TestFilePlanChecksAndStores(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, plan, kube, &fakeObjectStore{})
	bounded := fixtureVersion(t)
	bounded.Document.Risk.LargeLines = 400
	runner.cfg.Policy = func() policy.Version { return bounded }
	ctx := context.Background()

	over := testPlan()
	over.Cards[1].Size = "large" // links-api: large plans 400 lines, the bound itself
	b, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plan: card links-api: size large plans up to 400 changed lines and the bound is 400: split the card"}
	if !slices.Equal(res.Findings, want) || res.Revision != 0 {
		t.Fatalf("refused = %+v, want findings %q", res, want)
	}
	if rev, ok, _ := store.PlanRevision(ctx, "t-plan", 0); ok {
		t.Fatalf("a refused plan stored as revision %d", rev.Revision)
	}

	b, err = json.Marshal(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	res, err = runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 1 || res.SHA256 == "" || len(res.Findings) != 0 {
		t.Fatalf("filed = %+v, want revision 1", res)
	}
	rev, ok, err := store.PlanRevision(ctx, "t-plan", 0)
	if err != nil || !ok || rev.Revision != 1 || rev.SHA256 != res.SHA256 {
		t.Fatalf("stored = %+v, %v, %v", rev, ok, err)
	}
	if held, ok := runner.takeFiled("t-plan"); !ok || held.Revision != 1 {
		t.Fatalf("held for the session = %+v, %v", held, ok)
	}

	if err := store.TransitionTask(ctx, "t-plan", []string{"running"}, "in-review"); err != nil {
		t.Fatal(err)
	}
	res, err = runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Findings, []string{"plan: the task is in-review, not running"}) || res.Revision != 0 {
		t.Fatalf("not running = %+v", res)
	}
	if _, ok := runner.takeFiled("t-plan"); ok {
		t.Fatal("a refused filing held for the session")
	}
}

// PlanFileHandler answers the gateway's file_plan with the shape the
// gateway reads: a refused plan 200 with its findings, a valid one its
// revision, a body that is not the pair 400.
func TestPlanFileHandler(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	bounded := fixtureVersion(t)
	bounded.Document.Risk.LargeLines = 400
	runner.cfg.Policy = func() policy.Version { return bounded }
	h := PlanFileHandler(runner)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/plans", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	marshal := func(taskID string, p Plan) string {
		b, err := json.Marshal(map[string]string{"task_id": taskID, "plan": string(mustJSON(p))})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	over := testPlan()
	over.Cards[1].Size = "large"
	if rec := post(marshal("t-plan", over)); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"revision":0,"sha256":"","findings":["plan: card links-api: size large plans up to 400 changed lines and the bound is 400: split the card"]}` {
		t.Fatalf("refused plan: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(marshal("t-plan", testPlan()))
	if rec.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), `{"revision":1,"sha256":"`) {
		t.Fatalf("filed plan: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(marshal("no-such-task", testPlan())); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"plan":"{}"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no task_id: %d", rec.Code)
	}
	if rec := post(`not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("not json: %d", rec.Code)
	}
}

// The plan a session files through the tool is the session's answer: the
// tool's refusal reaches the planner inside the session, the corrected
// filing is what RunPlan returns when the session ends, and both filings
// cost one session, not two (roadmap 178).
func TestRunPlanTakesAToolFiling(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned := fixtureVersion(t)
	pinned.Document.Risk.LargeLines = 400
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.cfg.Policy = func() policy.Version { return pinned }

	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/coding-frontier", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	ctx := context.Background()
	over := testPlan()
	over.Cards[1].Size = "large"
	b, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || len(f.Findings) != 1 ||
		f.Findings[0] != "plan: card links-api: size large plans up to 400 changed lines and the bound is 400: split the card" {
		t.Fatalf("over-bound filing = %+v, %v", f, err)
	}
	b, err = json.Marshal(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	if f, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || f.Revision != 1 {
		t.Fatalf("corrected filing = %+v, %v", f, err)
	}
	kube.scriptPods(0, resultLine(true, "done"))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if res.Revision != 1 || res.SHA256 == "" || len(res.Findings) != 0 {
		t.Fatalf("result = %+v, want the filed revision 1", res)
	}
	if n := len(kube.podOrder); n != 1 {
		t.Fatalf("%d pods, want one session", n)
	}
}

// RunPlan takes its time from the task's class, not the policy's raw
// limits (review-task110.md I-1): reportSession resolves through
// TaskConfig, the planner's own agent_time and check_in, not
// limits.agent_time and limits.check_in. A class override much smaller
// than the limits, as the planner's real shape is (2h/45m against
// 30m/0), proves the wiring: the test activity environment fixes
// activity.GetInfo(ctx).Deadline about 10 minutes out (testRunnerConfig),
// so a session that anchors on the raw limits' 20 minutes starts its
// wall clock already deep in the past and trips at once, while one that
// anchors on the planner's 5 minutes starts on time. Reverting
// reportSession to s.policy.Limits.AgentTime/CheckIn turns this red: the
// runner's supervisor is the srv that fails the test on any call.
func TestRunPlanTakesItsTimeFromThePlannerClassNotTheLimits(t *testing.T) {
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	edit := func(file, old, new string) {
		t.Helper()
		s := string(files[file])
		if !strings.Contains(s, old) {
			t.Fatalf("%s lacks %q", file, old)
		}
		files[file] = []byte(strings.Replace(s, old, new, 1))
	}
	edit("policy.yaml", "  agent_time: 20m\n", "  agent_time: 20m\n  check_in: 15m\n")
	edit("policy.yaml", "  planner:\n    session: plan\n    models: [coding-cheap, coding-frontier]\n    supervisor_model: supervisor\n",
		"  planner:\n    session: plan\n    models: [coding-cheap, coding-frontier]\n    supervisor_model: supervisor\n    time: {agent_time: 5m, check_in: 0s}\n")
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	pinned := policy.Version{ID: 7, Commit: "c7", Tree: tree}

	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }

	if tc, err := runner.TaskConfig(context.Background(), "t-plan", 7); err != nil || tc.AgentTime != 5*time.Minute || tc.CheckIn != 0 {
		t.Fatalf("TaskConfig = %+v, %v, want agent_time 5m, check_in 0", tc, err)
	}

	logs := envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n" + resultLine(true, "no plan yet")
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/coding-frontier", int64(7), []string{})
		if err == nil {
			var res PlanResult
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, logs)
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
}

// deploySpec is a spec with a Deploy line; noDeployOwner the refusal of a
// plan without a card owning deploy/ against it.
const (
	deploySpec    = "Uptime\n\nDeploy: one Deployment with one replica, a Service on 8080. ADMIN_TOKEN comes from a Secret."
	noDeployOwner = "the spec has a Deploy line and no card owns deploy/: add a card whose directories include deploy/ and whose brief names " +
		"the manifests' changes (the Secret, the environment, the volume, the service)"
)

// deployPlan is testPlan whose third card, redirects, owns deploy/.
func deployPlan() Plan {
	p := testPlan()
	p.Cards[2].Directories = []string{"deploy/staging", "internal/x"}
	return p
}

// Stage 16 item 3.6: a spec's Deploy line ("Deploy:" or a Deploy
// heading, any case, leading blanks) needs a card owning deploy/: the
// first card listing deploy or a path under it, cleaned.
func TestDeployOwner(t *testing.T) {
	dot := testPlan()
	dot.Cards[3].Directories = []string{"./deploy"}
	both := deployPlan()
	both.Cards[3].Directories = []string{"deploy"}
	near := testPlan()
	near.Cards[1].Directories = []string{"deployment", "internal/deploy"}
	for _, c := range []struct {
		name, spec string
		p          Plan
		needed     bool
		owner      string
	}{
		{"third card", deploySpec, deployPlan(), true, "redirects"},
		{"no card", deploySpec, testPlan(), true, ""},
		{"no deploy line", "Link shortener\n\nBuild a link shortener.", deployPlan(), false, ""},
		{"heading", "Uptime\n\n## Deploy\n\nOne Deployment.", dot, true, "integration"},
		{"lower case, indented", "Uptime\n\n  deploy: staging only.", both, true, "redirects"},
		{"other paths", deploySpec, near, true, ""},
		{"a sentence", "Uptime\n\nSee the deployment notes. We deploy: later.\nDeployment notes: none.", deployPlan(), false, ""},
		{"Deployment heading", "Uptime\n\n## Deployment\n\nOne replica.", deployPlan(), true, "redirects"},
		{"Deployment line", "Uptime\n\nDeployment: one replica", testPlan(), true, ""},
		{"blank before the colon", "Uptime\n\nDeploy : x", deployPlan(), true, "redirects"},
	} {
		if needed, owner := deployOwner(c.spec, c.p); needed != c.needed || owner != c.owner {
			t.Errorf("%s: deployOwner = %v %q, want %v %q", c.name, needed, owner, c.needed, c.owner)
		}
	}
}

// A plan without a deploy owner against a Deploy spec is refused when its
// cards are filed, as a cycle is; with one the cards are filed and the
// plan comment marks the owner's row.
func TestAcceptRefusesPlanWithoutDeployOwner(t *testing.T) {
	r, store, objects, fj := newPlanRunner(t, testPlan())
	store.tasks["t-plan"] = func(tk Task) Task { tk.Objective = deploySpec; return tk }(store.tasks["t-plan"])
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	res := createChildren(t, r)
	if !slices.Equal(res.Refused, []string{noDeployOwner}) || len(res.Issues) != 0 {
		t.Fatalf("result %+v", res)
	}
	if fj.next != 1 || len(store.tasks) != 1 || len(fj.comments) != 1 ||
		fj.comments[0].body != "factoryd: accepting revision 1 was refused: "+noDeployOwner {
		t.Fatalf("issues up to #%d, rows %d, comments %v", fj.next, len(store.tasks), fj.comments)
	}
	if a := store.approvals[0]; a.Status != "denied" || a.DecisionNote != "accepting revision 1 was refused: "+noDeployOwner {
		t.Fatalf("approval %+v", a)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"plan_refused [" + noDeployOwner + "]"}) {
		t.Fatalf("steps %v", steps)
	}

	r, store, _, fj = newPlanRunner(t, deployPlan())
	store.tasks["t-plan"] = func(tk Task) Task { tk.Objective = deploySpec; return tk }(store.tasks["t-plan"])
	if res := createChildren(t, r); !slices.Equal(res.Issues, []int64{2, 3, 4, 5}) || len(res.Refused) != 0 {
		t.Fatalf("result %+v", res)
	}
	if len(fj.comments) != 1 {
		t.Fatalf("comments %v", fj.comments)
	}
	for _, want := range []string{
		"| # | key | role | title | size | depends on | issue | deploy |\n|---|---|---|---|---|---|---|---|\n",
		"| 1 | foundation | foundation | Foundation: layout, contract, schema | medium |  | #2 |  |\n",
		"| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api | #4 | deploy |\n",
		"| 4 | integration | integration | Integration: acceptance tests | medium | foundation, links-api, redirects | #5 |  |\n",
	} {
		if !strings.Contains(fj.comments[0].body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, fj.comments[0].body)
		}
	}
	if body := planComment(Task{ID: "t-plan", Objective: "Link shortener"}, 1, deployPlan(), nil); !strings.Contains(body,
		"| # | key | role | title | size | depends on | issue |\n|---|---|---|---|---|---|---|\n") ||
		!strings.Contains(body, "| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api |  |\n") {
		t.Fatalf("a spec without a Deploy line has a deploy column:\n%s", body)
	}
}

// The console's Accept plan on a plan without the deploy owner its Deploy
// spec needs answers the finding at once and still signals: the
// workflow's CreateChildren refuses it (the approval denied, no card
// filed) and the planner revises in exactly one new session with the
// finding; with an owner Accept plan only signals.
func TestAcceptPlanWithoutDeployOwnerThePlannerRevises(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj := newPlanRunner(t, testPlan())
	store.tasks["t-plan"] = func(tk Task) Task { tk.Objective = deploySpec; return tk }(store.tasks["t-plan"])
	store.plans["t-plan"][0].SHA256 = "sha-1"
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	wf := &signalRecorder{}
	err := AcceptPlan(ctx, store, wf, "t-plan", 1, "sha-1", "sebastian")
	if !errors.Is(err, ErrNoDeployOwner) || err.Error() != noDeployOwner || !slices.Equal(wf.signals, []string{"t-plan plan_accepted sebastian 1 sha-1 "}) {
		t.Fatalf("accept without an owner: %v, signals %v", err, wf.signals)
	}

	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.children = func(ctx context.Context, revision int, approver string, pin int64) (ChildrenResult, error) {
		return r.CreateChildren(ctx, "t-plan", revision, approver, pin)
	}
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 2 || h.plans[0] != nil || !slices.Equal(h.plans[1], []string{noDeployOwner}) || !slices.Equal(h.created, []string{"1 sebastian 7"}) {
		t.Fatalf("plans %q created %v", h.plans, h.created)
	}
	if store.approvals[0].Status != "denied" || fj.next != 1 || len(store.tasks) != 1 || len(fj.comments) != 1 {
		t.Fatalf("approval %+v, issues up to #%d, rows %d, comments %v", store.approvals[0], fj.next, len(store.tasks), fj.comments)
	}

	raw, sha, _ := planSHA(deployPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	if err := AcceptPlan(ctx, store, wf, "t-plan", 2, sha, "sebastian"); err != nil || len(wf.signals) != 2 || wf.signals[1] != "t-plan plan_accepted sebastian 2 "+sha+" " {
		t.Fatalf("accept with an owner: %v, signals %v", err, wf.signals)
	}
}

// RunPlan answers a provider's refusal, not an invalid plan (roadmap 155).
func TestRunPlanNamesAProviderRefusal(t *testing.T) {
	const said = "DashscopeException: budget exhausted (status 429)"
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, plan, kube, &fakeObjectStore{})
	store.providerRefusals = map[string]string{"t-plan": "upstream 503"}
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/coding-frontier", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(false, said))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if res.Refused != (ProviderWait{Status: 503, Reason: said}) || len(res.Findings) != 0 || res.Revision != 0 || res.Escalate != "" {
		t.Fatalf("result %+v, want the provider's refusal alone", res)
	}
}

var planRefused = PlanResult{Refused: ProviderWait{Status: 429, Reason: refusedSession.Reason}}

// A planner session the provider refused is no attempt (roadmap 155): the
// plan waits on the provider and the planner runs again, same findings.
func TestPlanLoopWaitsOnTheProvider(t *testing.T) {
	h := newPlanHarness(t, planRefused, planRefused, PlanResult{Revision: 1, SHA256: "sha-1"})
	backoffs := providerBackoffs(h.workflowHarness)
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 accepted: 3 cards filed" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 3 || slices.ContainsFunc(h.plans, func(f []string) bool { return f != nil }) || !slices.Equal(*backoffs, []int{120, 240}) ||
		!slices.Equal(h.states, []string{"running", "running", "running"}) || len(h.usage) != 1 || h.usage[0].Attempts != 1 || h.usage[0].AgentTime != planSession {
		t.Fatalf("plans %q, backoffs %v, states %v, usage %+v; want three sessions with no findings after waits of 120 and 240 s, one attempt", h.plans, *backoffs, h.states, h.usage)
	}
}

// Six refused planner sessions in a row ask a human with source provider;
// no attempt is counted.
func TestPlanLoopSixProviderRefusalsAskAHuman(t *testing.T) {
	h := newPlanHarness(t, planRefused)
	backoffs := providerBackoffs(h.workflowHarness)
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	want := "the model provider refused the first call of 6 sessions in a row (status 429): " + refusedSession.Reason
	if final := h.run(t); final != "closed" || len(h.plans) != 6 || !slices.Equal(*backoffs, []int{120, 240, 480, 960, 1800}) ||
		!slices.Equal(h.escalated, []string{want}) || escalationSource(want) != "provider" || len(h.usage) != 0 {
		t.Fatalf("final %q after %d sessions, backoffs %v, escalations %q, usage %+v; want %q, no attempt", final, len(h.plans), *backoffs, h.escalated, h.usage, want)
	}
}

// A plan run from before versionSessionRefusals takes a refusal as a plan
// without a last message: an attempt, its finding to the next session.
func TestPlanLoopRefusalBeforeTheChange(t *testing.T) {
	h := newPlanHarness(t, planRefused, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.env.OnGetVersion("provider-wait-sessions", workflow.DefaultVersion, versionSessionRefusals).Return(workflow.DefaultVersion)
	backoffs := providerBackoffs(h.workflowHarness)
	h.signalAfter(time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	want := []string{"plan: the session ended without a last message: " + refusedSession.Reason}
	if final := h.run(t); final != "done" || len(*backoffs) != 0 || len(h.usage) != 2 || h.usage[1].Attempts != 2 || len(h.plans) != 2 || !slices.Equal(h.plans[1], want) {
		t.Fatalf("final %q, backoffs %v, usage %+v, plans %q; want the second with %q", final, *backoffs, h.usage, h.plans, want)
	}
}

// A card's rung and reason reach its issue as a Rung line intake reads
// (roadmap 222, item 5); a card without one carries none.
func TestCardFactsRungLine(t *testing.T) {
	facts := cardFacts(PlanCard{Size: "small", Rung: RungFrontier, RungReason: "touches the queue"})
	if !strings.HasSuffix(facts, "Size: small\nRung: frontier, touches the queue\n") {
		t.Fatalf("facts = %q", facts)
	}
	if facts := cardFacts(PlanCard{Size: "small"}); strings.Contains(facts, "Rung") {
		t.Fatalf("facts = %q, want no rung line", facts)
	}
	if got := cardFindings(PlanCard{Key: "a", Title: "t", Brief: "b", Size: "small", Rung: "huge"}, testLimits, 0); !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, "rung huge") }) {
		t.Fatalf("findings = %q, want the rung refused", got)
	}
}

// Roadmap 201: a plan card's brief carries its directories once, in the
// repository map, and its size and rung not at all: its objective keeps
// the card's text, its dependencies and its acceptance items. Without a
// map the directories line stays.
func TestPlanCardObjectiveLeavesItsFieldsToTheCard(t *testing.T) {
	c := PlanCard{Key: "redirects", Title: "Redirects", Brief: "GET /<slug> answers 302.", Directories: []string{"internal/links"},
		Acceptance: []string{"GET /abc answers 302"}, Size: "small", Rung: RungCheap, RungReason: "routine"}
	objective := "Redirects\n\n" + cardBody(c, "plan #1", 3, 4, "sebastian", []Dependency{{Issue: 2, Key: "foundation"}})
	want := "Redirects\n\nCard redirects of plan #1 (3 of 4), accepted by sebastian. Written by the planner.\n\nGET /<slug> answers 302.\n\n" +
		"Depends on: #2 (foundation)\nAcceptance:\n- GET /abc answers 302"
	if got := briefObjective(objective, true); got != want {
		t.Fatalf("brief objective =\n%s\nwant\n%s", got, want)
	}
	if got := briefObjective(objective, false); !strings.Contains(got, "Directories (guidance, not a boundary): internal/links\nDepends on") || strings.Contains(got, "Size") || strings.Contains(got, "Rung") {
		t.Fatalf("brief objective without a map =\n%s", got)
	}
}
