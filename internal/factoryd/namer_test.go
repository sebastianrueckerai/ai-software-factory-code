package factoryd

import (
	"context"
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
