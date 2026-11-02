package factoryd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeNameStore records the namer's writes; open are the names the
// project's other open plans hold.
type fakeNameStore struct {
	open  []string
	names map[string]string
	// plans are the project's plan tasks; closed holds the ids of the
	// ended ones
	plans  []Task
	closed map[string]bool
	err    error
	// revs are the plans' newest revisions by task id
	revs map[string]PlanRevision
	// revErr are the ids whose revision read fails
	revErr map[string]error
}

func (f *fakeNameStore) PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error) {
	if err := f.revErr[id]; err != nil {
		return PlanRevision{}, false, err
	}
	rev, ok := f.revs[id]
	return rev, ok, nil
}

func (f *fakeNameStore) UnnamedOpenPlans(ctx context.Context) ([]Task, error) {
	var out []Task
	for _, t := range f.plans {
		if t.PlanName == "" && f.names[t.ID] == "" && !f.closed[t.ID] {
			out = append(out, t)
		}
	}
	return out, f.err
}

func (f *fakeNameStore) OpenPlanNames(ctx context.Context, projectID, excludeID string) ([]string, error) {
	return f.open, nil
}

func (f *fakeNameStore) SetPlanName(ctx context.Context, id, name string) error {
	f.names[id] = name
	return nil
}

// newNamer is a namer on the gateway fake ll, the store's open plans
// holding open.
func newNamer(t *testing.T, ll *fakeLiteLLM, open []string) (*Namer, *fakeNameStore) {
	t.Helper()
	store := &fakeNameStore{open: open, names: map[string]string{}}
	return &Namer{Store: store, Model: testSupervisorModel(ll.srv(t).URL),
		Current: func() policy.Version { return nameVersion() }}, store
}

func nameVersion() policy.Version {
	return policy.Version{Tree: policy.Tree{Document: policy.Document{
		Limits: policy.Limits{SupervisorTimeout: 6 * time.Minute}}}}
}

func nameTask() Task {
	return Task{ID: "p1", Repository: "o/r", RequiredCapabilities: []string{policy.ClassCoder}}
}

// nameAsk is the user message of the naming call i, counted from 1.
func nameAsk(t *testing.T, ll *fakeLiteLLM, i int) string {
	t.Helper()
	if len(ll.chats) < i {
		t.Fatalf("call %d never came; %d did", i, len(ll.chats))
	}
	return userMessage(ll.chats[i-1])
}

func TestNamePlanTakesTheFirstValidAnswer(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	if got := n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun"); got != "fix prod latency" {
		t.Fatalf("name %q, want %q", got, "fix prod latency")
	}
	if store.names["p1"] != "fix prod latency" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix prod latency")
	}
	if len(ll.chats) != 1 {
		t.Fatalf("%d calls, want 1", len(ll.chats))
	}
}

func TestNamePlanShrinksALongAnswer(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix the latency of the production system"}`, `{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	if got := n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun"); got != "fix prod latency" {
		t.Fatalf("name %q, want %q", got, "fix prod latency")
	}
	if !strings.Contains(nameAsk(t, ll, 2), "Shorter") {
		t.Fatalf("the retry asks for a shorter name: %q", nameAsk(t, ll, 2))
	}
	if strings.Contains(nameAsk(t, ll, 1), "Shorter") {
		t.Fatal("the first ask already says Shorter")
	}
	if store.names["p1"] != "fix prod latency" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix prod latency")
	}
}

func TestNamePlanRetriesATakenName(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"quota guard"}`, `{"name":"quota lift"}`}}
	n, store := newNamer(t, ll, []string{"quota guard"})
	if got := n.Name(context.Background(), nameTask(), "Quota the pager", "no more pages", "the quota blocks"); got != "quota lift" {
		t.Fatalf("name %q, want %q", got, "quota lift")
	}
	if want := "Already taken by other open plans: quota guard"; !strings.Contains(nameAsk(t, ll, 2), want) {
		t.Fatalf("the retry lists the taken name: %q", nameAsk(t, ll, 2))
	}
	if store.names["p1"] != "quota lift" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "quota lift")
	}
}

func TestNamePlanSurvivesABadShape(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`"fix prod latency"`, `{"name": "fix prod`, `{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	if got := n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun"); got != "fix prod latency" {
		t.Fatalf("name %q, want %q", got, "fix prod latency")
	}
	if len(ll.chats) != 3 {
		t.Fatalf("%d calls, want 3: a bare JSON string and a broken object are bad answers", len(ll.chats))
	}
	if store.names["p1"] != "fix prod latency" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix prod latency")
	}
}

func TestNamePlanFallsBackToTheSlug(t *testing.T) {
	tooLong := `{"name":"way too long to be a plan name at all"}`
	ll := &fakeLiteLLM{contents: []string{tooLong, tooLong, tooLong}}
	n, store := newNamer(t, ll, nil)
	if got := n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun"); got != "fix-the-check" {
		t.Fatalf("name %q, want the slug %q", got, "fix-the-check")
	}
	if len(ll.chats) != 3 {
		t.Fatalf("%d calls, want 3", len(ll.chats))
	}
	if store.names["p1"] != "fix-the-check" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix-the-check")
	}
}

func TestNamePlanSlugEscapesCollisions(t *testing.T) {
	tooLong := `{"name":"way too long to be a plan name at all"}`
	ll := &fakeLiteLLM{contents: []string{tooLong, tooLong, tooLong}}
	n, store := newNamer(t, ll, []string{"fix-the-check"})
	if got := n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun"); !strings.HasSuffix(got, "-2") {
		t.Fatalf("name %q, want one ending -2", got)
	}
	if store.names["p1"] != "fix-the-check-2" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix-the-check-2")
	}
}

func TestNamePlanSuffixedSlugFitsTheBound(t *testing.T) {
	tooLong := `{"name":"way too long to be a plan name at all"}`
	ll := &fakeLiteLLM{contents: []string{tooLong, tooLong, tooLong}}
	n, store := newNamer(t, ll, []string{"fix-the-check-that-keeps"})
	// the title's slug is 24 runes; the number's room comes out of the
	// base, at a word boundary, not on top of the bound
	if got := n.Name(context.Background(), nameTask(), "Fix the check that keeps", "the check passes", "it fails on rerun"); got != "fix-the-check-that-2" {
		t.Fatalf("name %q (%d runes), want %q", got, len([]rune(got)), "fix-the-check-that-2")
	}
	if store.names["p1"] != "fix-the-check-that-2" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix-the-check-that-2")
	}
}

func TestNamePlanAsksTheSmallModel(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix prod latency"}`, `{"name":"quota lift"}`}}
	n, _ := newNamer(t, ll, nil)
	n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun")
	chat := nameTask()
	chat.RequiredCapabilities = []string{policy.ClassChat}
	n.Name(context.Background(), chat, "Quota the pager", "no more pages", "the quota blocks")
	if len(ll.chats) != 2 {
		t.Fatalf("%d calls, want 2", len(ll.chats))
	}
	// the plan-classed task reaches the tier through the fallback, the
	// chat-classed one through the policy's own title route: small either way
	for i, call := range ll.chats {
		if m, _ := call["model"].(string); m != policy.Small {
			t.Fatalf("call %d asks %q, want the small tier %q", i+1, m, policy.Small)
		}
	}
}

func TestNamePlanSurvivesAGatewayError(t *testing.T) {
	ll := &fakeLiteLLM{status: http.StatusInternalServerError}
	n, store := newNamer(t, ll, nil)
	if got := n.Name(context.Background(), nameTask(), "Fix the check!", "the check passes", "it fails on rerun"); got != "fix-the-check" {
		t.Fatalf("name %q, want the slug %q", got, "fix-the-check")
	}
	if store.names["p1"] != "fix-the-check" {
		t.Fatalf("stored %q, want %q", store.names["p1"], "fix-the-check")
	}
	if len(ll.chats) != 1 {
		t.Fatalf("%d calls, want 1: a failed ask is not retried", len(ll.chats))
	}
}

func TestNormalizePlanName(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		ok   bool
	}{{
		in:   "  fix   prod latency ",
		want: "fix prod latency",
		ok:   true,
	}, {
		in: "abcdefghijklmnopqrstuvwxy", // 25 runes
	}, {
		in: "fix\nprod",
	}, {
		in: "fix‮prod", // a bidi override is a format character
	}, {
		in: "ab",
	}, {
		in:   "abcdefghijklmnopqrstuvwx", // 24 runes
		want: "abcdefghijklmnopqrstuvwx",
		ok:   true,
	}} {
		got, ok := normalizePlanName(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("normalizePlanName(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNameOpenPlansNamesSkipsAndIsIdempotent(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	store.plans = []Task{
		{ID: "open", Repository: "o/r", Objective: "Fix the check!\n\nIt fails.", RequiredCapabilities: []string{policy.ClassCoder}},
		{ID: "drafting", Repository: "o/r", Objective: "Not filed yet"},
		{ID: "named", Repository: "o/r", PlanName: "already here"},
		{ID: "ended", Repository: "o/r", Objective: "Old plan"},
	}
	store.closed = map[string]bool{"ended": true}
	store.revs = map[string]PlanRevision{
		"open":  {Plan: []byte(`{"spec":{"outcome":"the check passes"},"summary":"it fails on rerun"}`)},
		"ended": {Plan: []byte(`{"summary":"old"}`)},
	}
	if got := n.NameOpenPlans(context.Background()); got != 1 {
		t.Fatalf("named %d, want 1: the filed open plan only", got)
	}
	if len(store.names) != 1 || store.names["open"] != "fix prod latency" {
		t.Fatalf("names %v, want only open: %q", store.names, "fix prod latency")
	}
	ask := nameAsk(t, ll, 1)
	for _, want := range []string{"Issue title: Fix the check!", "Spec outcome: the check passes", "Summary: it fails on rerun"} {
		if !strings.Contains(ask, want) {
			t.Fatalf("the ask lacks %q: %q", want, ask)
		}
	}
	if got := n.NameOpenPlans(context.Background()); got != 0 || len(ll.chats) != 1 {
		t.Fatalf("second pass named %d with %d calls in all, want 0 and 1", got, len(ll.chats))
	}
}

func TestNameOpenPlansNamesAPlanWhoseRevisionDoesNotRead(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	store.plans = []Task{{ID: "open", Repository: "o/r", Objective: "Fix the check!"}}
	store.revs = map[string]PlanRevision{"open": {Plan: []byte(`not json`)}}
	if got := n.NameOpenPlans(context.Background()); got != 1 || store.names["open"] != "fix prod latency" {
		t.Fatalf("named %d as %v, want 1 as %q", got, store.names, "fix prod latency")
	}
}

func TestNameOpenPlansStopsOnAStoreError(t *testing.T) {
	ll := &fakeLiteLLM{}
	n, store := newNamer(t, ll, nil)
	store.err = errors.New("db down")
	if got := n.NameOpenPlans(context.Background()); got != 0 || len(ll.chats) != 0 {
		t.Fatalf("named %d with %d calls, want none", got, len(ll.chats))
	}
}

func TestNameOpenPlansStopsOnARevisionReadError(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	store.plans = []Task{{ID: "first", Repository: "o/r", Objective: "Fix the check!"}, {ID: "second", Repository: "o/r", Objective: "Quota the pager"}}
	store.revs = map[string]PlanRevision{"first": {Plan: []byte(`{"summary":"a"}`)}, "second": {Plan: []byte(`{"summary":"b"}`)}}
	store.revErr = map[string]error{"second": errors.New("db down")}
	if got := n.NameOpenPlans(context.Background()); got != 1 {
		t.Fatalf("named %d, want 1: the first plan, then the read of the second fails", got)
	}
	if len(store.names) != 1 || store.names["first"] != "fix prod latency" || len(ll.chats) != 1 {
		t.Fatalf("names %v with %d calls, want only first named with 1 call", store.names, len(ll.chats))
	}
}

// signalNameStore tells when a plan's name is stored, so a test waits for
// naming that runs off the filing's path.
type signalNameStore struct {
	NameStore
	named chan struct{}
}

func newSignalNameStore(s NameStore) *signalNameStore {
	return &signalNameStore{NameStore: s, named: make(chan struct{}, 4)}
}

func (s *signalNameStore) SetPlanName(ctx context.Context, id, name string) error {
	err := s.NameStore.SetPlanName(ctx, id, name)
	s.named <- struct{}{}
	return err
}

func (s *signalNameStore) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.named:
	case <-time.After(10 * time.Second):
		t.Fatal("the plan was not named in 10s")
	}
}

func TestPlanSlugStaysInsideTheBounds(t *testing.T) {
	for _, tt := range []struct {
		title string
		taken []string
		want  string
	}{
		{"Go", nil, "plan"},
		{"Go", []string{"plan"}, "plan-2"},
		{"Go!!", []string{"plan", "plan-2"}, "plan-3"},
		{"Fix it", nil, "fix-it"},
	} {
		if got := planSlug(tt.title, tt.taken); got != tt.want {
			t.Fatalf("planSlug(%q, %v) = %q, want %q", tt.title, tt.taken, got, tt.want)
		}
	}
}

func TestNameOpenPlansStopsOnACancelledContext(t *testing.T) {
	ll := &fakeLiteLLM{contents: []string{`{"name":"fix prod latency"}`}}
	n, store := newNamer(t, ll, nil)
	store.plans = []Task{{ID: "open", Repository: "o/r", Objective: "Fix the check!"}}
	store.revs = map[string]PlanRevision{"open": {Plan: []byte(`{"summary":"a"}`)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := n.NameOpenPlans(ctx); got != 0 || len(ll.chats) != 0 || len(store.names) != 0 {
		t.Fatalf("named %d with %d calls and names %v, want none", got, len(ll.chats), store.names)
	}
}
