package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	gwhdr "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestDivideConquerLedgerIntegrity drives the producers that write the
// panels' rows — the Runner's session start and terminal end, gate
// reading and model review, the ReleaseKeeper's hold end and Deploy now —
// over a real PostgreSQL store, through exported factoryd.NewRunner on
// fake Kube, Forgejo, session and store collaborators, no port nil. A
// replayed or retried write leaves the one row its identity keeps, with
// the instants and linkage of the first; a refused write is the caller's
// error and its retry lands; a separate attempt is its own row. The
// PostgreSQL supplement the panels read is verified through those
// producer-event retries, not through settlement rows. (The applied cut
// and the replan and round filings sit behind a Temporal workflow and a
// plan model; their producers' retries are proved in internal/factoryd.)
func TestDivideConquerLedgerIntegrity(t *testing.T) {
	ctx := context.Background()

	// SessionAnswered writes the session_started row the sessions panel
	// counts: a reattached run replays it, a refused row is the caller's
	// error, the retry lands, and the started session's terminal end
	// keeps the one row its identity holds through a refused replay too.
	t.Run("sessions start and end once", func(t *testing.T) {
		s, db := divideStore(t)
		accCard(t, s, "t-sess-1", "sf/code", "small", "h", 71)
		ledger := &accLedger{PGStore: s}
		r := newLedgerRunner(t, ledger, "", "")
		in := factoryd.SessionAnswerInput{TaskID: "t-sess-1", SessionID: "s-1", Model: "factory/small", Attempt: 1}
		before := time.Now()
		if ok, err := r.SessionAnswered(ctx, in); err != nil || !ok {
			t.Fatalf("SessionAnswered = (%v, %v), want (true, nil)", ok, err)
		}
		first := accRows(t, db, factoryd.EventSessionStarted)
		time.Sleep(20 * time.Millisecond)
		in.Attempt = 2 // the reattached run counts on, the session is the same
		if ok, err := r.SessionAnswered(ctx, in); err != nil || !ok {
			t.Fatalf("replayed SessionAnswered = (%v, %v), want (true, nil)", ok, err)
		}
		again := accRows(t, db, factoryd.EventSessionStarted)
		if len(first) != 1 || len(again) != 1 || !again[0].At.Equal(first[0].At) || first[0].At.Before(before.Add(-time.Millisecond)) {
			t.Fatalf("rows %+v then %+v, want the first start alone, at its own instant", first, again)
		}
		if got := again[0]; got.Task != "t-sess-1" || got.Repository != "sf/code" || got.Issue != 71 || got.Identity != "session_started t-sess-1 s-1" ||
			got.Detail["session"] != "s-1" || got.Detail["attempt"] != float64(1) {
			t.Errorf("start row %+v, want session s-1 of t-sess-1 as first written", got)
		}
		ledger.refuse(factoryd.EventSessionStarted, errors.New("the store is gone"))
		next := factoryd.SessionAnswerInput{TaskID: "t-sess-1", SessionID: "s-2", Model: "factory/small", Attempt: 3}
		if _, err := r.SessionAnswered(ctx, next); err == nil || !strings.Contains(err.Error(), "the store is gone") {
			t.Errorf("a refused row answered %v, want it as the caller's error", err)
		}
		if n := len(accRows(t, db, factoryd.EventSessionStarted)); n != 1 {
			t.Errorf("%d start rows after the refused write, want the first alone", n)
		}
		if _, err := r.SessionAnswered(ctx, next); err != nil {
			t.Fatal(err)
		}
		if rows := accRows(t, db, factoryd.EventSessionStarted); len(rows) != 2 || rows[1].Detail["session"] != "s-2" {
			t.Errorf("start rows %+v, want s-1 and the retried s-2", rows)
		}

		// the started session s-1's terminal end, the row the ends panel
		// counts, written the way its producer writes it: one row its
		// identity holds, a refused replay the caller's error, the
		// retried replay the same row at the same instant — never a
		// second session or a second end
		ended := time.Now().Add(time.Minute)
		end := factoryd.SessionEndFact{Session: "s-1", Attempt: 1, Outcome: string(factoryd.OutcomePushed), Started: before}.
			Event("t-sess-1", "sf/code", 71, ended)
		if id, err := ledger.Record(ctx, end); err != nil || id == 0 {
			t.Fatalf("the terminal end recorded (%d, %v), want its own row", id, err)
		}
		ledger.refuse(factoryd.EventSessionEnd, errors.New("the store is gone"))
		if _, err := ledger.Record(ctx, end); err == nil || !strings.Contains(err.Error(), "the store is gone") {
			t.Errorf("the refused end answered %v, want it as the caller's error", err)
		}
		if id, err := ledger.Record(ctx, end); err != nil || id != 0 {
			t.Errorf("the retried end answered (%d, %v), want 0: one session, one end", id, err)
		}
		rows := accRows(t, db, factoryd.EventSessionEnd)
		if len(rows) != 1 || rows[0].At.IsZero() || ended.Sub(rows[0].At) >= time.Millisecond || rows[0].Identity != "session_end s-1 1" ||
			rows[0].Task != "t-sess-1" || rows[0].Detail["outcome"] != string(factoryd.OutcomePushed) {
			t.Errorf("end rows %+v, want s-1's pushed end alone, at its own instant", rows)
		}
	})

	// AutoMergeNote writes the gate_head reading the size-bound panel
	// counts: one row per head. A replay under a changed policy and a
	// changed size leaves the first snapshot, its bound, policy version
	// and size standing; a new head reads the new ones.
	t.Run("gate readings keep their snapshot", func(t *testing.T) {
		s, db := divideStore(t)
		accCard(t, s, "t-gate-1", "sf/code", "small", "head-a", 72)
		if _, err := db.Exec(`UPDATE tasks SET extra_lines = 10 WHERE id = 't-gate-1'`); err != nil {
			t.Fatal(err)
		}
		r := newLedgerRunner(t, &accLedger{PGStore: s}, "", "")
		note := func(head string, largeLines int, version int64, insertions int) {
			t.Helper()
			g := factoryd.GateResults{Policy: factoryd.PolicyGate{Class: "normal", Outcome: policy.OutcomeAuto, MergeRule: "auto",
				LargeLines: largeLines, PolicyVersion: version, Files: 3, Insertions: insertions, Deletions: 20},
				Reproduction: factoryd.ReproResult{Command: "make test"}}
			if err := r.AutoMergeNote(ctx, "t-gate-1", 7, head, g, factoryd.Review{Verdict: "approve", Summary: "green"}); err != nil {
				t.Fatal(err)
			}
		}
		note("head-a", 1000, 7, 200)
		first := accRows(t, db, factoryd.EventGateHead)
		// the policy moves to version 8 (large 2000) and the card to
		// medium; the same head, read again
		if _, err := db.Exec(`UPDATE tasks SET size = 'medium' WHERE id = 't-gate-1'`); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		note("head-a", 2000, 8, 999)
		note("head-b", 2000, 8, 350)
		rows := accRows(t, db, factoryd.EventGateHead)
		if len(rows) != 2 {
			t.Fatalf("%d gate_head rows, want one per head", len(rows))
		}
		// small is 30 percent of large 1000, plus the 10 the card was raised
		want := map[string]any{"gate": factoryd.GateMerge, "head": "head-a", "decision": policy.OutcomeAuto, "size": "small",
			"bound": float64(310), "policy_version": float64(7), "files": float64(3), "insertions": float64(200), "deletions": float64(20)}
		same(t, "head-a detail", rows[0].Detail, want)
		if rows[0].Identity != "gate_head t-gate-1 head-a" || !rows[0].At.Equal(first[0].At) {
			t.Errorf("head-a row %+v, want its identity and its first instant", rows[0])
		}
		// medium is 60 percent of large 2000, plus 10
		if d := rows[1].Detail; d["head"] != "head-b" || d["size"] != "medium" || d["bound"] != float64(1210) || d["policy_version"] != float64(8) || d["insertions"] != float64(350) {
			t.Errorf("head-b detail %v, want medium, bound 1210, version 8", d)
		}
	})

	// ModelReview writes one model_review row per attempt: see accDriveReviews.
	t.Run("model reviews", func(t *testing.T) {
		s, db := divideStore(t)
		accDriveReviews(t, s, db)
	})

	// The ReleaseKeeper's public Pass and DeployNow write the hold facts
	// the hold panel unions. A refused write is the call's error and
	// changes nothing; the retry, minutes later, lands one row with the
	// hold's original start, its own end and the press's linkage; a
	// Deploy now restart is a separate interval.
	t.Run("hold facts survive a refused write", func(t *testing.T) {
		s, db := divideStore(t)
		day := accDay(7)
		at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
		now := at(10, 30)
		runs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/sf/code/actions/runs" {
				t.Errorf("keeper called %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{
				{"id": 21, "commit_sha": "c0ffee1", "event": "push", "status": "success", "prettyref": "main", "created": "2026-10-05T09:00:00Z"}}})
		}))
		t.Cleanup(runs.Close)
		own, _ := factoryd.SplitRepo("sf/code")
		store := &accKeeperStore{ReleaseStore: s}
		k := &factoryd.ReleaseKeeper{Store: store, Forgejo: &factoryd.Forgejo{BaseURL: runs.URL}, Own: own,
			Timeline: &factoryd.Timeline{Store: s, Events: s}, Now: func() time.Time { return now }}
		pass := func(h, m int) error { now = at(h, m); return k.Pass(ctx) }
		openPlan := func(plan string, issue int64, merged time.Time) {
			accPlan(t, s, plan, "sf/code", issue)
			accPlanChild(t, s, plan+"-1", plan, "sf/code", issue+100)
			accMerged(t, s, plan+"-1", "sf/code", merged)
		}
		held := func() time.Time {
			var since sql.NullTime
			if err := db.QueryRow(`SELECT held_since FROM releases WHERE repository = 'sf/code'`).Scan(&since); err != nil {
				t.Fatal(err)
			}
			return since.Time
		}
		released := func() []accRow { return accRows(t, db, factoryd.EventProductionReleased) }

		// a plan merged its first card at 10:00; the pass at 10:30 starts the clock there
		openPlan("t-hold-1", 91, at(10, 0))
		if err := pass(10, 30); err != nil || !held().Equal(at(10, 0)) {
			t.Fatalf("pass = %v, held since %v; want the clock at the first merge 10:00", err, held())
		}
		// the plan closes; the hold's end is refused by the store once
		accClosed(t, s, "t-hold-1", "sf/code", at(11, 0))
		store.refuse("EndReleaseHold", errors.New("the store is down"))
		if err := pass(12, 0); err == nil || !strings.Contains(err.Error(), "the store is down") {
			t.Fatalf("pass = %v, want the refused write as the pass's error", err)
		}
		if n := len(released()); n != 0 || !held().Equal(at(10, 0)) {
			t.Fatalf("%d facts, held since %v after the refused write; want none and the hold standing", n, held())
		}
		// the next pass, five minutes on, ends the hold then, once
		if err := pass(12, 5); err != nil {
			t.Fatal(err)
		}
		if err := pass(12, 6); err != nil {
			t.Fatal(err)
		}
		rows := released()
		if len(rows) != 1 || !rows[0].At.Equal(at(12, 5)) || rows[0].Identity != "production_released sf/code 2026-10-05T10:00:00Z" ||
			rows[0].Actor != "factoryd" || rows[0].Detail["how"] != "closed" || rows[0].Detail["held_since"] != "2026-10-05T10:00:00Z" ||
			rows[0].Detail["hours"] != (2*time.Hour+5*time.Minute).Hours() || !held().IsZero() {
			t.Fatalf("rows %+v, held since %v; want the retried end once, from 10:00 to 12:05", rows, held())
		}

		// a second plan holds from 13:00; Deploy now's write is refused: no commit, the clock unchanged
		openPlan("t-hold-2", 92, at(13, 0))
		if err := pass(13, 10); err != nil || !held().Equal(at(13, 0)) {
			t.Fatalf("pass = %v, held since %v; want the second hold at 13:00", err, held())
		}
		store.refuse("SetDeployNow", errors.New("the store is down"))
		now = at(14, 0)
		if commit, err := k.DeployNow(ctx, "sf/code", "sebastian", factoryd.ViaButton, "member"); err == nil || commit != "" {
			t.Fatalf("DeployNow = %q, %v; want the refused write and no commit", commit, err)
		}
		if n := len(released()); n != 1 || !held().Equal(at(13, 0)) {
			t.Fatalf("%d facts, held since %v after the refused press; want the first alone and the clock at 13:00", n, held())
		}
		now = at(14, 10)
		if commit, err := k.DeployNow(ctx, "sf/code", "sebastian", factoryd.ViaButton, "member"); err != nil || commit != "c0ffee1" {
			t.Fatalf("DeployNow = %q, %v; want c0ffee1", commit, err)
		}
		// a second press restarts the interval again: a separate fact
		now = at(14, 20)
		if _, err := k.DeployNow(ctx, "sf/code", "sebastian", factoryd.ViaButton, "member"); err != nil {
			t.Fatal(err)
		}
		// the plan closes at 14:50; the pass at 15:00 ends the last interval
		accClosed(t, s, "t-hold-2", "sf/code", at(14, 50))
		if err := pass(15, 0); err != nil {
			t.Fatal(err)
		}
		rows = released()
		if len(rows) != 4 {
			t.Fatalf("%d facts, want one per hold and per restart: %+v", len(rows), rows)
		}
		press := rows[1]
		if !press.At.Equal(at(14, 10)) || press.Actor != "sebastian" || press.Via != factoryd.ViaButton ||
			press.Identity != "production_released sf/code 2026-10-05T13:00:00Z" || press.Detail["how"] != "deploy_now" ||
			press.Detail["held_since"] != "2026-10-05T13:00:00Z" || press.Detail["commit"] != "c0ffee1" || press.Detail["decider"] != "member" ||
			press.Detail["hours"] != (70*time.Minute).Hours() {
			t.Errorf("the press's row %+v, want the 13:00 hold ended at 14:10 by sebastian at c0ffee1", press)
		}
		if again := rows[2]; !again.At.Equal(at(14, 20)) || again.Detail["held_since"] != "2026-10-05T14:10:00Z" || again.Identity == press.Identity {
			t.Errorf("the second press's row %+v, want the 14:10 restart as its own fact", again)
		}
		if last := rows[3]; !last.At.Equal(at(15, 0)) || last.Actor != "factoryd" || last.Detail["held_since"] != "2026-10-05T14:20:00Z" {
			t.Errorf("the last row %+v, want the 14:20 interval closed at 15:00", last)
		}
		// the panel unions what the producers wrote: 10:00-12:05, 13:00-14:10,
		// 14:10-14:20 and 14:20-15:00 are 125 + 70 + 10 + 40 minutes
		dc := newDivideRig(t, s).report()
		same(t, "hold_wait_milliseconds", dc.HoldWaitMillis.Value, float64((245 * time.Minute).Milliseconds()))
		same(t, "hold_wait_coverage", dc.HoldWaitCoverage, factoryd.HoldWaitCoverage{Ended: 4})
	})
}

// accDriveReviews drives the Runner's ModelReview over s and checks the rows it leaves. It writes one model_review row per attempt at a head and
// round: a replay of an attempt is one row, a second genuine round of
// the head and a round of another head each their own; a refused row
// is the review's error and its retry lands. A refused or unreadable
// answer is a row that never judged the head, a concerns verdict one
// that did.
func accDriveReviews(t *testing.T, s *factoryd.PGStore, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	accCard(t, s, "t-rev-1", "sf/code", "small", "c1", 81)
	accCard(t, s, "t-rev-2", "sf/other", "small", "o1", 82)
	fj, gw := &accForgejo{}, &accGateway{}
	ledger := &accLedger{PGStore: s}
	r := newLedgerRunner(t, ledger, serve(t, fj), serve(t, gw))
	review := func(task, head string, round int, answer accReply) error {
		fj.setHead(head)
		gw.answer(answer)
		_, err := r.ModelReview(ctx, task, factoryd.PullRequest{Number: 9}, head, factoryd.GateResults{}, nil, round)
		return err
	}
	concerns := accReply{content: `{"verdict":"concerns","summary":"the target skips vet","findings":["Makefile: check does not run go vet"]}`}
	approve := accReply{content: `{"verdict":"approve","summary":"fine"}`}
	refused := accReply{status: http.StatusPaymentRequired, decision: "refused_budget"}
	failed := accReply{status: http.StatusInternalServerError}
	invalid := accReply{content: "LGTM"}
	for round, answer := range []accReply{concerns, refused, invalid, failed} {
		if err := review("t-rev-1", "c1", round+1, answer); err != nil {
			t.Fatal(err)
		}
	}
	four := accRows(t, db, factoryd.EventModelReview)
	time.Sleep(20 * time.Millisecond)
	// the activity retried round 1: the model is asked again, the row stays
	if err := review("t-rev-1", "c1", 1, approve); err != nil {
		t.Fatal(err)
	}
	if again := accRows(t, db, factoryd.EventModelReview); len(again) != 4 || !again[0].At.Equal(four[0].At) || again[0].Detail["verdict"] != "concerns" {
		t.Fatalf("rows %+v after the replay of round 1, want the four attempts as first written", again)
	}
	for i, want := range []struct {
		reviewed bool
		verdict  string
		reason   string
	}{{true, "concerns", ""}, {false, "", "refused_budget"}, {false, "", "answer invalid"}, {false, "", "model review failed: status 500"}} {
		row := four[i]
		if row.Identity != "model_review t-rev-1 c1 "+string('1'+rune(i)) || row.Detail["round"] != float64(i+1) || row.Detail["pr"] != float64(9) ||
			row.Detail["reviewed"] != want.reviewed || row.Detail["verdict"] != want.verdict || (row.Detail["reason"] != nil) == want.reviewed {
			t.Errorf("round %d row %+v, want reviewed %v with verdict %q", i+1, row, want.reviewed, want.verdict)
		}
		if want.reason != "" && !strings.Contains(fmt.Sprint(row.Detail["reason"]), want.reason) {
			t.Errorf("round %d reason %v, want it to say %q", i+1, row.Detail["reason"], want.reason)
		}
	}
	// another head: its first round, refused by the store once
	ledger.refuse(factoryd.EventModelReview, errors.New("the store is gone"))
	if err := review("t-rev-1", "c2", 1, approve); err == nil || !strings.Contains(err.Error(), "the store is gone") {
		t.Fatalf("review over a refused row = %v, want it as the review's error", err)
	}
	if n := len(accRows(t, db, factoryd.EventModelReview)); n != 4 {
		t.Fatalf("%d rows after the refused write, want four", n)
	}
	if err := review("t-rev-1", "c2", 1, approve); err != nil {
		t.Fatal(err)
	}
	// another repository's review answers its own panel
	if err := review("t-rev-2", "o1", 1, invalid); err != nil {
		t.Fatal(err)
	}
	// a keyed row with no classification: unknown evidence
	accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "t-rev-1", IssueNumber: 81, Kind: factoryd.EventModelReview,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, IdentityKey: "model_review t-rev-1 c3 1"}, time.Now().UTC())

}

// accObjects is the object store the timeline appends its steps to.
type accObjects struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (o *accObjects) Put(_ context.Context, key string, body []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[key] = body
	return nil
}

func (o *accObjects) Get(_ context.Context, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.files[key], nil
}

func (o *accObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.files, key)
	return nil
}

// accLeases and accWorkflows stand for collaborators NewRunner insists
// on; no fact this test drives touches them.
type accLeases struct{}

func (accLeases) LiveAddresses(context.Context) (map[string][]string, error) { return nil, nil }
func (accLeases) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	return nil, nil
}

type accWorkflows struct{}

func (accWorkflows) Start(context.Context, string) error { return nil }
func (accWorkflows) Signal(context.Context, string, string, factoryd.TaskSignal) error {
	return nil
}
func (accWorkflows) Reopen(context.Context, string, string, factoryd.TaskSignal) error {
	return nil
}
func (accWorkflows) StartRoutineRun(context.Context, factoryd.RoutineRunInput) error { return nil }

// accKube is the runner's Kubernetes seam as a fake: nothing this test
// drives touches a pod, so every write succeeds and every read answers
// that nothing exists — the fake collaborator NewRunner insists on
// having, not a nil port.
type accKube struct{}

func (accKube) CreateNamespace(context.Context, string, map[string]string) error { return nil }
func (accKube) DeleteNamespace(context.Context, string) error                    { return nil }
func (accKube) NamespacesByLabel(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (accKube) ListNamespacesByLabel(context.Context, string, string) ([]factoryd.NamespaceInfo, error) {
	return nil, nil
}
func (accKube) CreateServiceAccount(context.Context, string, string) error { return nil }
func (accKube) CreateSecret(context.Context, string, string, string, map[string]string) error {
	return nil
}
func (accKube) GetSecretData(context.Context, string, string) (map[string]string, error) {
	return nil, factoryd.ErrNotFound
}
func (accKube) CreateCiliumNetworkPolicy(context.Context, string, string, map[string]any) error {
	return nil
}
func (accKube) ApplyCiliumNetworkPolicy(context.Context, string, string, map[string]any) error {
	return nil
}
func (accKube) CiliumNetworkPolicySpec(context.Context, string, string) (map[string]any, error) {
	return nil, factoryd.ErrNotFound
}
func (accKube) Create(context.Context, string, map[string]any) error { return nil }
func (accKube) CreatePod(context.Context, string, string, map[string]any) error {
	return nil
}
func (accKube) DeletePod(context.Context, string, string) error { return nil }
func (accKube) PodStatus(context.Context, string, string) (string, int32, error) {
	return "", -1, factoryd.ErrNotFound
}
func (accKube) PodIP(context.Context, string, string) (string, error) {
	return "", factoryd.ErrNotFound
}
func (accKube) PodInitFinished(context.Context, string, string) (time.Time, bool, error) {
	return time.Time{}, false, factoryd.ErrNotFound
}
func (accKube) PodLog(context.Context, string, string) (io.ReadCloser, error) {
	return nil, factoryd.ErrNotFound
}
func (accKube) PodDiagnosis(context.Context, string, string) (factoryd.PodDiagnosis, error) {
	return factoryd.PodDiagnosis{}, factoryd.ErrNotFound
}
func (accKube) PodLogTail(context.Context, string, string, int) (string, error) {
	return "", factoryd.ErrNotFound
}
func (accKube) PersistentVolumes(context.Context) ([]factoryd.PersistentVolume, error) {
	return nil, nil
}
func (accKube) DeletePersistentVolume(context.Context, string) error { return nil }
func (accKube) CronJobs(context.Context, string) ([]factoryd.CronJobState, error) {
	return nil, nil
}
func (accKube) ScheduledBackups(context.Context, string) ([]factoryd.CronJobState, error) {
	return nil, nil
}
func (accKube) Deployment(context.Context, string, string) (factoryd.Deployment, error) {
	return factoryd.Deployment{}, factoryd.ErrNotFound
}

// accPolicyTree is the tree the runner validates, its limits the agent
// time the supervisor's wall clock needs.
func accPolicyTree(t *testing.T) policy.Tree {
	t.Helper()
	files := map[string][]byte{"policy.yaml": []byte(policy.Header("policy.yaml") + `
version: 1
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20, month_eur: 50}
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder: {session: code, models: [small, big], tools: {}, review_model: big, supervisor_model: supervisor}
prices:
  max_output_tokens: {small: 32768, big: 32768, supervisor: 32768}
risk:
  classes: [{name: normal, outcome: auto}]
  rules: []
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`)}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// newLedgerRunner wires the real Runner over the ledger with a fake Kube
// and a fake completion beside its fake Forgejo and model gateway — no
// port nil, the collaborators production wires. The timeline records
// into the ledger itself; the gateway serves at gatewayURL.
func newLedgerRunner(t *testing.T, ledger *accLedger, forgejoURL, gatewayURL string) *factoryd.Runner {
	t.Helper()
	objects := &accObjects{files: map[string][]byte{}}
	version := policy.Version{ID: 7, Commit: "c7", Tree: accPolicyTree(t)}
	fj := forgejoURL
	if fj == "" {
		fj = serve(t, &accForgejo{})
	}
	litellm := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key/list" {
			_, _ = w.Write([]byte(`{"keys":[],"total_pages":0}`))
		}
	}))
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc",
		SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test", FactoryNamespace: "factory",
		GatewayURL: "http://gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor:    factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:        func() policy.Version { return version },
		PolicyVersion: func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil },
		Leases:        accLeases{}, Gates: &factoryd.GateWatch{}, Workflows: accWorkflows{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Timeline:    &factoryd.Timeline{Objects: objects, Events: ledger, Store: ledger},
	}
	forgejo := &factoryd.Forgejo{BaseURL: fj, BotUser: "factory-agent", BotPassword: "bot-pw"}
	completion := &factoryd.Completion{Store: ledger, Forgejo: forgejo,
		LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm, MasterKey: "mk"}}
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:1"
	}
	r, err := factoryd.NewRunner(cfg, ledger, accKube{}, forgejo, completion, objects, &factoryd.SupervisorModel{
		GatewayURL: gatewayURL, Token: "gw-token",
		Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// serve starts h as a test server and answers its URL.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// accRow is one events row as PostgreSQL holds it.
type accRow struct {
	At                                     time.Time
	Task, Repository, Actor, Via, Identity string
	Issue                                  int64
	Detail                                 map[string]any
}

// accRows reads the events of kind in the order they landed.
func accRows(t *testing.T, db *sql.DB, kind string) []accRow {
	t.Helper()
	rows, err := db.Query(`SELECT at, COALESCE(task_id, ''), repository, COALESCE(issue_number, 0), actor, via,
		COALESCE(identity_key, ''), detail FROM events WHERE kind = $1 ORDER BY id`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []accRow
	for rows.Next() {
		var r accRow
		var detail []byte
		if err := rows.Scan(&r.At, &r.Task, &r.Repository, &r.Issue, &r.Actor, &r.Via, &r.Identity, &detail); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(detail, &r.Detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// accLedger is the real store the producers write to, a write of one
// kind refusable once, and the audit's answer to a session fixed.
type accLedger struct {
	*factoryd.PGStore
	mu      sync.Mutex
	refused map[string]error
}

func (l *accLedger) refuse(kind string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refused == nil {
		l.refused = map[string]error{}
	}
	l.refused[kind] = err
}

func (l *accLedger) Record(ctx context.Context, e factoryd.Event) (int64, error) {
	l.mu.Lock()
	err := l.refused[e.Kind]
	delete(l.refused, e.Kind)
	l.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return l.PGStore.Record(ctx, e)
}

func (l *accLedger) ModelAnswered(context.Context, string, time.Time) (bool, error) { return true, nil }

// accKeeperStore is the keeper's store with the hold's two writes
// refusable once, and the one project its repository names.
type accKeeperStore struct {
	factoryd.ReleaseStore
	mu      sync.Mutex
	refused map[string]error
}

func (s *accKeeperStore) refuse(write string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused == nil {
		s.refused = map[string]error{}
	}
	s.refused[write] = err
}

func (s *accKeeperStore) take(write string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.refused[write]
	delete(s.refused, write)
	return err
}

func (s *accKeeperStore) EndReleaseHold(ctx context.Context, repository string, ended factoryd.Event) error {
	if err := s.take("EndReleaseHold"); err != nil {
		return err
	}
	return s.ReleaseStore.EndReleaseHold(ctx, repository, ended)
}

func (s *accKeeperStore) SetDeployNow(ctx context.Context, repository, commit string, since time.Time, ended factoryd.Event) error {
	if err := s.take("SetDeployNow"); err != nil {
		return err
	}
	return s.ReleaseStore.SetDeployNow(ctx, repository, commit, since, ended)
}

func (s *accKeeperStore) ProjectByRepository(_ context.Context, repository string) (factoryd.Project, bool, error) {
	return factoryd.Project{Name: "code", Repository: repository}, true, nil
}

// accForgejo serves pull request 9 of any repository at the head the test
// sets, with a one-file diff, and takes its comments.
type accForgejo struct {
	mu   sync.Mutex
	head string
}

func (f *accForgejo) setHead(head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = head
}

func (f *accForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/9.diff"):
		_, _ = io.WriteString(w, "diff --git a/Makefile b/Makefile\n+check:\n")
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/9"):
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "state": "open", "merged": false, "mergeable": true,
			"head": map[string]any{"sha": f.head}, "base": map[string]any{"ref": "main"}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	default:
		http.NotFound(w, r)
	}
}

// accReply is the model gateway's answer: a refusal when status is set.
type accReply struct {
	status   int
	decision string
	content  string
}

// accGateway answers every chat call with the reply the test set last.
type accGateway struct {
	mu    sync.Mutex
	reply accReply
}

func (g *accGateway) answer(r accReply) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reply = r
}

func (g *accGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	reply := g.reply
	g.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if reply.status != 0 {
		w.Header().Set(gwhdr.HeaderDecision, reply.decision)
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, `{"error":{"message":"task headroom 0.000000 EUR","type":"budget_exceeded"}}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply.content}}}})
}
