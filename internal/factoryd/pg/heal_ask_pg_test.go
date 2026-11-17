package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// healAskForgejo is Forgejo as the heal pass and the re-opened ask's face
// read it: the cards' issues open with no comments, and every comment
// posted on them taken and kept.
type healAskForgejo struct {
	mu     sync.Mutex
	posted []string
}

func (f *healAskForgejo) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case isWriterRead(r.URL.Path):
			serveWriters(t, w, r)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/") && !strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `{"state":"open"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var c struct{ Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &c)
			f.posted = append(f.posted, c.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.posted))
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// healAskRig is the heal pass's rig: a runner beside the parked-card
// store, both on one Forgejo stub.
type healAskRig struct {
	s     *PGStore
	r     *Runner
	forge *healAskForgejo
}

func newHealAskRig(t *testing.T, db string) healAskRig {
	t.Helper()
	s := cardCaseStore(t, db)
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	wf := &fakeWorkflows{}
	cfg.Workflows = wf
	forge := &healAskForgejo{}
	objects := &fakeObjectStore{}
	r := factoryd.PgRigRunner(s, objects, cfg, &Timeline{Objects: objects, Events: s, Store: s},
		&Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"})
	return healAskRig{s: s, r: r, forge: forge}
}

// pass runs one heal pass over the rig's cards.
func (g healAskRig) pass(t *testing.T) {
	t.Helper()
	tree := g.r.PgCfg().Policy().Tree
	HealPass(context.Background(), g.s, g.r.PgCfg().Workflows, &Intake{Store: g.s, Forgejo: g.r.PgForgejo(),
		Workflows: g.r.PgCfg().Workflows, BotUser: "factory-agent", Policy: func() policy.Tree { return tree }}, nil, noneCompleted, g.r)
}

// comments is what the face posted on the cards' issues, oldest first.
func (f *healAskForgejo) comments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posted...)
}

// park consumes the card's session-cap ask as an action-bearing answer
// row does, and ends its case resolvedAgo ago: the card keeps waiting on
// a human with no live ask. It tells the ended case.
func park(t *testing.T, s *PGStore, resolvedAgo time.Duration) Case {
	t.Helper()
	askCap(t, s, 27001)
	c := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaChat, map[string]any{
		"action": ActExtend, "agent_time_s": 1800, "comment": int64(27002)})
	if got := caseByID(t, s, c.ID); got.Status != CaseResolved || got.ResolvedAt == nil {
		t.Fatalf("the cap case = %+v; want it resolved by the answer", got)
	}
	if resolvedAgo > 0 {
		if _, err := s.DB().ExecContext(context.Background(),
			`UPDATE cases SET resolved_at = now() - $1::interval WHERE id = $2`,
			fmt.Sprintf("%d seconds", int(resolvedAgo.Seconds())), c.ID); err != nil {
			t.Fatalf("age the case: %v", err)
		}
	}
	return c
}

// A card of a running kind that waits on a human with no ask — its cap
// ask consumed by an answer, its case ended past the at_human bound —
// gets its ask back on the heal pass: the healed-run path opens the
// ask's own case again, with its face on the card's issue, so the
// console's answer paths find an ask to press. A second pass finds the
// ask live and posts no second face.
func TestHealPassReasksACardWaitingWithNoAsk(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask")
	ended := park(t, g.s, 10*time.Minute)
	if ask, err := g.s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != 0 {
		t.Fatalf("the parked card's live ask = %+v, %v; want none", ask, err)
	}

	g.pass(t)

	ask, err := g.s.NewestAskCase(ctx, caseTask)
	if err != nil || ask.ID == 0 || ask.ID == ended.ID || ask.Status != CaseAtHuman {
		t.Fatalf("the healed card's live ask = %+v, %v; want a fresh case at the human", ask, err)
	}
	reopened := ask
	posted := g.forge.comments()
	if len(posted) != 1 || !strings.Contains(posted[0], HealedReason) {
		t.Fatalf("posted %q; want the ask's face once, asking again after the wait", posted)
	}
	asks, err := g.s.AskAnchors(ctx, caseTask)
	if err != nil || len(asks) == 0 {
		t.Fatalf("the card's ask anchors = %+v, %v; want the seeded ask's", asks, err)
	}
	if !strings.Contains(asks[len(asks)-1].Question, "session cap is reached") {
		t.Fatalf("the ask again = %q; want the words the card asked last", asks[len(asks)-1].Question)
	}

	g.pass(t)

	if ask, err := g.s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != reopened.ID {
		t.Fatalf("the ask after two passes = %+v, %v; want %d again", ask, err, reopened.ID)
	}
	if posted := g.forge.comments(); len(posted) != 1 {
		t.Fatalf("posted %q after two passes; want the one face: the ask is live again", posted)
	}
}

// The cards the pass leaves waiting as they are: one whose consumed ask
// ended inside the at_human bound, and one whose ask is live. Neither
// grows a case, and no face is posted on their issues.
func TestHealPassLeavesAskedAndFreshlyParkedCards(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_fresh")
	park(t, g.s, 0)
	if err := g.s.CreateTask(ctx, Task{ID: "t1420", Kind: "agent", Status: "waiting-human", Objective: "y",
		Repository: "sf/r", IssueNumber: 1420, Track: policy.TrackDirect, BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask t1420: %v", err)
	}
	if _, err := g.s.Record(ctx, Event{Repository: "sf/r", TaskID: "t1420", IssueNumber: 1420, Kind: EventQuestionAsked,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"question": capAsk, "ask": capAsk,
			"comment": int64(27020), "source": "cap", "reason": CapReached}}); err != nil {
		t.Fatalf("Record t1420's ask: %v", err)
	}
	live, err := g.s.OpenCasesOf(ctx, "t1420")
	if err != nil || len(live) != 1 {
		t.Fatalf("t1420's cases = %+v, %v; want its one live ask", live, err)
	}

	g.pass(t)

	cases, err := g.s.CasesOf(ctx, caseTask)
	if err != nil || len(cases) != 1 || cases[0].Status != CaseResolved {
		t.Fatalf("the freshly parked card's cases = %+v, %v; want its one ended case", cases, err)
	}
	if ask, err := g.s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != 0 {
		t.Fatalf("the freshly parked card's live ask = %+v, %v; want none: the wait is inside the bound", ask, err)
	}
	cases, err = g.s.CasesOf(ctx, "t1420")
	if err != nil || len(cases) != 1 || cases[0].ID != live[0].ID {
		t.Fatalf("the asked card's cases = %+v, %v; want its one live ask's case", cases, err)
	}
	if ask, err := g.s.NewestAskCase(ctx, "t1420"); err != nil || ask.ID != live[0].ID || ask.Status != CaseAtHuman {
		t.Fatalf("the asked card's live ask = %+v, %v; want the ask that stood", ask, err)
	}
	if posted := g.forge.comments(); len(posted) != 0 {
		t.Fatalf("posted %q; want no face: both cards wait as they are", posted)
	}
}
