package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"go.temporal.io/sdk/testsuite"
)

// Roadmap 336: every session that ends ends as a fact row beside its
// timeline line — pushed, wall clock, and a detached session's
// reattached end among them — with no summary, which the timeline line
// alone keeps, while attempts that never ran a session record none. The
// row's identity is the session and the attempt, so exactly the logical
// ends land.
func TestDivideConquerSessionFacts(t *testing.T) {
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, Task{ID: "t5", Status: "running", Objective: "push work",
		Repository: "sf/repo", Track: "direct"}, kube, &fakeObjectStore{})

	// a first attempt that pushes
	first, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 1, Model: "m"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || first.Outcome != OutcomePushed {
		t.Fatalf("first = (%+v, %v), want pushed", first, err)
	}

	// a second attempt factoryd detaches from, whose reattached end is
	// the one fact of that session
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	logs := loopFailure(1) + cutLogs + pushedLine("c-wip") + "\n" + resultLine(true, "pushed") + "\n"
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t5", Attempt: 2, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("detach err = %v, want factoryd stopping", err)
	}
	if ends := eventsOfKind(store, EventSessionEnd); len(ends) != 1 {
		t.Fatalf("session_end rows after the detach = %d, want the first attempt's alone", len(ends))
	}
	live := store.liveSessions["t5"]
	kube.onCreatePod = nil
	second, err := runSessionIn(t, runner, SessionInput{TaskID: "t5", Attempt: 2, Model: "m"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: live.SessionID, Running: true, Events: 1})
	})
	if err != nil || second.Outcome != OutcomePushed || second.SessionID != live.SessionID {
		t.Fatalf("reattach = (%+v, %v), want the live session's push", second, err)
	}

	// a third attempt the wall clock takes
	runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, RepetitionEvents: 24}
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	third, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 3, Model: "m"}, nil, hangLogs)
	if err != nil || third.Outcome != OutcomeWallClock {
		t.Fatalf("third = (%+v, %v), want wall_clock", third, err)
	}

	// a fourth attempt no node takes: no session ever ran, no fact
	runner.startPoll = 20 * time.Millisecond
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.phase, p.conditions = "Pending", []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			Message: "0/3 nodes are available: 3 Insufficient ephemeral-storage."}}
	}
	if res, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 4, Model: "m"}, nil); err != nil || res.Outcome != OutcomeNotStarted {
		t.Fatalf("fourth = (%+v, %v), want not_started", res, err)
	}

	ends := eventsOfKind(store, EventSessionEnd)
	want := []struct {
		session string
		attempt int
		outcome SessionOutcome
	}{{first.SessionID, 1, OutcomePushed}, {live.SessionID, 2, OutcomePushed}, {third.SessionID, 3, OutcomeWallClock}}
	if len(ends) != len(want) {
		t.Fatalf("session_end rows = %d (%v), want %d", len(ends), ends, len(want))
	}
	for i, w := range want {
		row := ends[i]
		if row.TaskID != "t5" || row.Repository != "sf/repo" || row.Actor != FactoryDecider || row.Via != ViaFactoryd {
			t.Fatalf("row %d = %+v, want task t5's, by the decider through factoryd", i, row)
		}
		if key := fmt.Sprintf("%s %s %d", EventSessionEnd, w.session, w.attempt); row.IdentityKey != key {
			t.Fatalf("row %d identity = %q, want %q", i, row.IdentityKey, key)
		}
		if row.Detail["session"] != w.session || row.Detail["attempt"] != w.attempt || row.Detail["outcome"] != string(w.outcome) {
			t.Fatalf("row %d detail = %v, want session %s attempt %d %s", i, row.Detail, w.session, w.attempt, w.outcome)
		}
		if row.Detail["summary"] != nil || row.Detail["parent"] != nil || row.Detail["model"] != nil {
			t.Fatalf("row %d detail = %v: no summary, parent or model; the timeline line keeps the words", i, row.Detail)
		}
		if _, err := time.Parse(time.RFC3339, fmt.Sprint(row.Detail["started"])); err != nil {
			t.Fatalf("row %d started %v: %v", i, row.Detail["started"], err)
		}
		if _, err := time.Parse(time.RFC3339, fmt.Sprint(row.Detail["ended"])); err != nil {
			t.Fatalf("row %d ended %v: %v", i, row.Detail["ended"], err)
		}
	}
}

// Roadmap 336: every review call's answer is a fact row beside its
// timeline line — the concerns a model returned reviewed the head, a
// gateway refusal and an invalid answer never judged it — so two of
// three answers on a head read not reviewed without the concerns
// misclassified. A roll card's note is no review call and records none.
func TestDivideConquerReviewFacts(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n+check:\n"}
	r, store, objects := newGateRunner(t, gateTask, fj)

	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"the target skips vet","findings":["Makefile: check does not run go vet"]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 1)
	if rv := getInto[Review](t, val, err); rv.Verdict != "concerns" {
		t.Fatalf("first review = %+v, want concerns", rv)
	}

	refused := &fakeLiteLLM{status: http.StatusPaymentRequired, decision: "refused_budget"}
	r.supervisor = testSupervisorModel(refused.srv(t).URL)
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 2); err != nil {
		t.Fatal(err)
	}

	invalid := &fakeLiteLLM{content: "LGTM"}
	r.supervisor = testSupervisorModel(invalid.srv(t).URL)
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 3); err != nil {
		t.Fatal(err)
	}

	rows := eventsOfKind(store, EventModelReview)
	if len(rows) != 3 {
		t.Fatalf("model_review rows = %d (%v), want three", len(rows), rows)
	}
	notReviewed := 0
	for i, row := range rows {
		if row.TaskID != "t1" || row.Repository != "sf/repo" || row.IssueNumber != 4 {
			t.Fatalf("row %d = %+v, want task t1's card", i, row)
		}
		if key := fmt.Sprintf("%s c1 %d", EventModelReview, i+1); row.IdentityKey != key {
			t.Fatalf("row %d identity = %q, want %q", i, row.IdentityKey, key)
		}
		if row.Detail["pr"] != int64(9) || row.Detail["commit"] != "c1" || row.Detail["round"] != i+1 || row.Detail["model"] != "big" {
			t.Fatalf("row %d detail = %v, want round %d of c1 by big", i, row.Detail, i+1)
		}
		if row.Detail["summary"] != nil || row.Detail["findings"] != nil {
			t.Fatalf("row %d detail = %v: no summary or findings; the timeline line keeps the words", i, row.Detail)
		}
		if row.Detail["verdict"] != "concerns" {
			notReviewed++
			reviewed, _ := row.Detail["reviewed"].(bool)
			reason, _ := row.Detail["reason"].(string)
			if row.Detail["verdict"] != "" || reviewed || reason == "" || len(reason) > maxFactReason+1 {
				t.Fatalf("row %d detail = %v, want the bounded reason of an answer that never judged", i, row.Detail)
			}
		} else if row.Detail["reviewed"] != true || row.Detail["reason"] != nil {
			t.Fatalf("row %d detail = %v, want the concerns reviewed, with no reason", i, row.Detail)
		}
	}
	if notReviewed != 2 {
		t.Fatalf("not reviewed = %d of 3, want 2", notReviewed)
	}
	if strings.Count(timeline(t, objects), `"step":"model_review"`) != 3 {
		t.Fatal("the rows' timeline lines are gone")
	}

	// a roll card bumps digests and makes no model call: its note is no
	// review fact
	roll := gateTask
	roll.ID, roll.Source = "t9", SourceRoll
	if err := store.CreateTask(context.Background(), roll); err != nil {
		t.Fatal(err)
	}
	if _, err := execActivity(t, r, ActivityModelReview, "t9", PullRequest{Number: 9}, "c1", GateResults{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if rows := eventsOfKind(store, EventModelReview); len(rows) != 3 {
		t.Fatalf("model_review rows = %d, want the three calls': a roll card records none", len(rows))
	}
}

// A fact the store refused is the owning activity's error, and its
// retry writes the row the identity keeps single: one round of one head
// replayed stays one review fact, a second genuine round adds its own,
// and a session whose end row the store refused ends as one fact — the
// retry's session, which is the one that took.
func TestDivideConquerSessionReviewRetries(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n+check:\n"}
	r, store, _ := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"s","findings":["f"]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	store.recordFails = map[string]int{EventModelReview: 1}
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 1); err == nil || !strings.Contains(err.Error(), "the store is down") {
		t.Fatalf("review err = %v, want the refused row", err)
	}
	if rows := eventsOfKind(store, EventModelReview); len(rows) != 0 {
		t.Fatalf("model_review rows = %d, want none taken", len(rows))
	}
	for round := 1; round <= 2; round++ {
		if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, round); err != nil {
			t.Fatal(err)
		}
	}
	rows := eventsOfKind(store, EventModelReview)
	if len(rows) != 2 {
		t.Fatalf("model_review rows = %d (%v), want the replayed round once and its successor once", len(rows), rows)
	}
	for i, round := range []string{"1", "2"} {
		if rows[i].IdentityKey != EventModelReview+" c1 "+round {
			t.Fatalf("row %d identity = %q, want round %s of c1", i, rows[i].IdentityKey, round)
		}
	}

	// the same end rebuilt is the same fact: the identity decides, so a
	// replayed row is one row
	end := SessionEndFact{Session: "s-x", Attempt: 3, Outcome: string(OutcomePushed), Started: time.Unix(0, 0)}
	if first, again := end.Event("t1", "sf/repo", 4, time.Now()), end.Event("t1", "sf/repo", 4, time.Now()); first.IdentityKey != again.IdentityKey {
		t.Fatalf("identities %q and %q differ", first.IdentityKey, again.IdentityKey)
	}

	// the session: the refused end row fails the completed session, and
	// the retry's session ends as the one fact of the attempt
	kube := newFakeKube()
	runner, _, sessions := newTestRunner(t, Task{ID: "t7", Status: "running", Objective: "push work",
		Repository: "sf/repo", Track: "direct"}, kube, &fakeObjectStore{})
	sessions.recordFails = map[string]int{EventSessionEnd: 1}
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t7", Attempt: 1, Model: "m"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed")); err == nil || !strings.Contains(err.Error(), "session end fact") {
		t.Fatalf("session err = %v, want the refused end row", err)
	}
	if ends := eventsOfKind(sessions, EventSessionEnd); len(ends) != 0 {
		t.Fatalf("session_end rows = %d, want none taken", len(ends))
	}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t7", Attempt: 1, Model: "m"}, nil,
		pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	ends := eventsOfKind(sessions, EventSessionEnd)
	if len(ends) != 1 || ends[0].Detail["session"] != res.SessionID ||
		ends[0].IdentityKey != fmt.Sprintf("%s %s 1", EventSessionEnd, res.SessionID) {
		t.Fatalf("session_end rows = %+v, want the retry's session once", ends)
	}
}

// Roadmap 336: one attempt, one review fact — the attempt's evidence.
// The identity is the head and the round together, so a persistence
// replay of one attempt leaves the row it already wrote, a second
// genuine round of the same head adds its own row and a round of
// another head its own beside it; a write the store refuses is the
// activity's error, observable, and its retry writes the row once. A
// legacy row without its identity or classification the store cannot
// collapse — a replay writes it again — and no identity of its own ever
// lands: it stays what it is, unknown evidence the panels name, never
// an answer the producer fabricated. [ledger_retry_integrity]
func TestDivideConquerReviewAttemptEvidence(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n+check:\n"}
	r, store, _ := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"s","findings":["f"]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	// round 1 of head c1: a genuine negative answer that judged the head
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	// the persistence replay of the same attempt: the identity keeps one
	// row, so the replay adds no answer
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	// a second genuine round of the same head: its own answer
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, nil, 2); err != nil {
		t.Fatal(err)
	}
	// a round of another head: the gateway refuses it, its answer never
	// judged — and the store's refusal of the row is the activity's own
	// error, so the retry writes it
	fj.head = "c2"
	refused := &fakeLiteLLM{status: http.StatusPaymentRequired, decision: "refused_budget"}
	r.supervisor = testSupervisorModel(refused.srv(t).URL)
	store.recordFails = map[string]int{EventModelReview: 1}
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c2", GateResults{}, nil, 1); err == nil || !strings.Contains(err.Error(), "the store is down") {
		t.Fatalf("review err = %v, want the refused row", err)
	}
	if rows := eventsOfKind(store, EventModelReview); len(rows) != 2 {
		t.Fatalf("model_review rows = %d (%v), want none of the refused write", len(rows), rows)
	}
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c2", GateResults{}, nil, 1); err != nil {
		t.Fatal(err)
	}

	rows := eventsOfKind(store, EventModelReview)
	if len(rows) != 3 {
		t.Fatalf("model_review rows = %d (%v), want the three attempts' once each", len(rows), rows)
	}
	want := []struct {
		identity string
		reviewed bool
		verdict  string
	}{
		{EventModelReview + " c1 1", true, "concerns"},
		{EventModelReview + " c1 2", true, "concerns"},
		{EventModelReview + " c2 1", false, ""},
	}
	for i, w := range want {
		row := rows[i]
		if row.TaskID != "t1" || row.Repository != "sf/repo" || row.IssueNumber != 4 {
			t.Fatalf("row %d = %+v, want task t1's card", i, row)
		}
		if row.IdentityKey != w.identity {
			t.Fatalf("row %d identity = %q, want %q", i, row.IdentityKey, w.identity)
		}
		if row.Detail["reviewed"] != w.reviewed || row.Detail["verdict"] != w.verdict {
			t.Fatalf("row %d detail = %v, want the classification reviewed %v of verdict %q", i, row.Detail, w.reviewed, w.verdict)
		}
		if !w.reviewed {
			reason, _ := row.Detail["reason"].(string)
			if reason == "" || len(reason) > maxFactReason+1 {
				t.Fatalf("row %d reason = %q, want the bounded reason of an answer that never judged", i, reason)
			}
		}
	}

	// the legacy row no identity holds: recorded twice, it lands twice —
	// the store cannot know it one attempt — and no classification of
	// its own ever lands, so it stays unknown evidence, never an answer
	legacy := Event{Repository: "sf/repo", TaskID: "t1", IssueNumber: 4, Kind: EventModelReview,
		Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"pr": 9, "commit": "c9", "round": 1, "model": "big", "verdict": "approve"}}
	for write := 1; write <= 2; write++ {
		if _, err := store.Record(context.Background(), legacy); err != nil {
			t.Fatal(err)
		}
	}
	rows = eventsOfKind(store, EventModelReview)
	if len(rows) != 5 {
		t.Fatalf("model_review rows = %d, want the three attempts' and the legacy row twice: no identity holds it one", len(rows))
	}
	for i, row := range rows[3:] {
		if row.IdentityKey != "" || row.Detail["reviewed"] != nil {
			t.Fatalf("legacy row %d = %+v: no identity, no classification — unknown evidence, never an answer", i, row)
		}
	}
}

// Roadmap 336: a named start is a fact row beside its timeline line,
// as stable as the end — the row an activity retry replays is one row
// (the identity is the card and the session), and a refused write is
// the activity's error, so its retry records the start instead of
// losing it while the timeline line stands. A session whose activity
// failed has no id — it counts against the cap, and no start row names
// a session "" — and a session without a model answer started none:
// the ModelAnswered boundary is unchanged.
func TestDivideConquerSessionStartRetries(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, store := newTestRunner(t, supervisedTask("t8"), newFakeKube(), objects)
	store.answered = map[string]bool{"t8": true}
	in := SessionAnswerInput{TaskID: "t8", SessionID: "s1", Model: "factory/small", Attempt: 2,
		Since: time.Now().Add(-time.Minute)}

	// the refused row is observable: the activity fails and no row is
	// taken, while the timeline line keeps the start's words
	store.recordFails = map[string]int{EventSessionStarted: 1}
	if ok, err := runner.SessionAnswered(t.Context(), in); err == nil || !strings.Contains(err.Error(), "the store is down") || ok {
		t.Fatalf("session answered = (%v, %v), want the refused start row", ok, err)
	}
	if rows := eventsOfKind(store, EventSessionStarted); len(rows) != 0 {
		t.Fatalf("session_started rows = %d, want none taken", len(rows))
	}
	tl, _ := objects.Get(t.Context(), "sessions/t8/timeline.jsonl")
	if !strings.Contains(string(tl), `"event":"session_started"`) || !strings.Contains(string(tl), `"session":"s1"`) {
		t.Fatalf("timeline = %s, want the line whatever the row does", tl)
	}

	// the retry records the start, and the replay is the same fact: the
	// identity is the card and the session, so both calls leave one row
	for call := 1; call <= 2; call++ {
		if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || !ok {
			t.Fatalf("session answered, call %d = (%v, %v), want true", call, ok, err)
		}
	}
	rows := eventsOfKind(store, EventSessionStarted)
	if len(rows) != 1 {
		t.Fatalf("session_started rows = %d (%v), want the start once", len(rows), rows)
	}
	if key := fmt.Sprintf("%s %s %s", EventSessionStarted, "t8", "s1"); rows[0].IdentityKey != key {
		t.Fatalf("row identity = %q, want %q", rows[0].IdentityKey, key)
	}
	if rows[0].TaskID != "t8" || rows[0].Repository != "sf/repo" || rows[0].Actor != FactoryDecider || rows[0].Via != ViaFactoryd {
		t.Fatalf("row = %+v, want task t8's, by the decider through factoryd", rows[0])
	}
	if rows[0].Detail["session"] != "s1" || rows[0].Detail["model"] != "small" || rows[0].Detail["attempt"] != 2 {
		t.Fatalf("row detail = %v, want session s1, the model's alias and attempt 2", rows[0].Detail)
	}

	// another session of the card is its own fact
	in.SessionID = "s2"
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || !ok {
		t.Fatalf("second session answered = (%v, %v), want true", ok, err)
	}
	if rows := eventsOfKind(store, EventSessionStarted); len(rows) != 2 {
		t.Fatalf("session_started rows = %d, want one per session", len(rows))
	}

	// a session whose activity failed has no id: it counts, and no row
	// names a session ""
	in.SessionID = ""
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || !ok {
		t.Fatalf("answered session without an id = (%v, %v), want true", ok, err)
	}
	if rows := eventsOfKind(store, EventSessionStarted); len(rows) != 2 {
		t.Fatalf("session_started rows = %d, want the named sessions alone", len(rows))
	}

	// a session without a model answer started no session
	store.answered = map[string]bool{"t8": false}
	in.SessionID = "s3"
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || ok {
		t.Fatalf("starved session = (%v, %v), want false", ok, err)
	}
	if rows := eventsOfKind(store, EventSessionStarted); len(rows) != 2 {
		t.Fatalf("session_started rows = %d, want none for the starved session", len(rows))
	}
}

// eventsOfKind picks the fake store's event rows of one kind, oldest
// first.
func eventsOfKind(store *fakeStore, kind string) []Event {
	var out []Event
	for _, e := range store.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}
