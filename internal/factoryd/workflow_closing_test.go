package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Runs restarted at closing, recorded on a dev server before the move to
// closing stored its end: the Reconcile answer holds no end, and the replay
// schedules the activities the recording did with the inputs it did. A
// plain card's Finish fails it for the interrupted completion; a report
// card runs its report, whose move to running the closing card refused.
func TestWorkflowReplaysRunsRestartedAtClosing(t *testing.T) {
	for _, tc := range []struct {
		file   string
		calls  []string
		finish string
	}{
		{"devserver-closing-restart.json", []string{ActivityTaskConfig, ActivityEnsureIssue, ActivityReconcile, ActivityFinish},
			`"t1"` + "\n" + `"failed"` + "\n" + `"completion resumed after an interrupted run"`},
		{"devserver-report-closing-restart.json", []string{ActivityTaskConfig, ActivityEnsureIssue, ActivityReconcile, ActivitySetState, ActivityFinish},
			`"t1"` + "\n" + `"failed"` + "\n" + `"set running: task t1 is closing, cannot move to running"`},
	} {
		t.Run(tc.file, func(t *testing.T) {
			hist := loadHistory(t, filepath.Join("testdata", tc.file))
			calls, inputs := scheduledActivities(hist)
			if !slices.Equal(calls, tc.calls) || inputs[len(inputs)-1] != tc.finish {
				t.Fatalf("the recording schedules %q, its Finish %q; want %q and %q", calls, inputs[len(inputs)-1], tc.calls, tc.finish)
			}
			st := reconciled(t, hist)
			if _, stored := st["ClosingFinal"]; st["Status"] != "closing" || stored {
				t.Fatalf("the recorded Reconcile answer %v: want a closing card with no stored end", st)
			}
			var got, gotInputs []string
			replaySplice(t, hist.Events, &activitiesSeen{got: &got, inputs: &gotInputs})
			if !slices.Equal(got, calls) || !slices.Equal(gotInputs, inputs) {
				t.Fatalf("the replay scheduled %q with %q, the recording %q with %q", got, gotInputs, calls, inputs)
			}
		})
	}
}

// A run restarted at closing finishes with the end the cut-off run stored
// with its move there, a report card's too, and runs nothing else.
func TestWorkflowRunRestartedAtClosingFinishesWithItsStoredEnd(t *testing.T) {
	for name, st := range map[string]RunState{
		"done":   {Status: "closing", ClosingFinal: "done", ClosingReason: "pull request merged by factory-merge after sebastian's approval"},
		"closed": {Status: "closing", ClosingFinal: "closed", ClosingReason: "issue closed without merge"},
		"report": {Status: "closing", Report: true, ClosingFinal: "done", ClosingReason: "report kept on the task, no issue comment"},
	} {
		h := newWorkflowHarness(t)
		h.state = st
		h.onSessions(pushed("s1", "c1"))
		if final := h.run(t); final != st.ClosingFinal || len(h.finals) != 1 || h.finals[0][1] != st.ClosingReason || len(h.sessions) != 0 || len(h.states) != 0 {
			t.Fatalf("%s: final = %q, finals %v, sessions %d, states %v; want %s for %q and nothing else", name, final, h.finals, len(h.sessions), h.states, st.ClosingFinal, st.ClosingReason)
		}
	}
}

// A report card restarted at closing in a run from before the change runs
// its report, the stored end unread.
func TestWorkflowReportRestartedAtClosingBeforeTheChangeRunsTheReport(t *testing.T) {
	h, began, _ := reportHarness(t, ReportResult{})
	h.state.Status, h.state.ClosingFinal, h.state.ClosingReason = "closing", "closed", "issue closed without merge"
	h.env.OnGetVersion("closing-intent", workflow.DefaultVersion, versionClosingIntent).Return(workflow.DefaultVersion)
	h.run(t)
	if want := [2]string{"done", "report kept on the task, no issue comment"}; len(h.finals) != 1 || h.finals[0] != want || len(*began) != 1 || !slices.Equal(h.states, []string{"running"}) {
		t.Fatalf("finals %v, reports %d, states %v; want %v after one report", h.finals, len(*began), h.states, want)
	}
}

// The move to closing stores the end the run decided; a completion that
// fails after it leaves the card at closing with that end, which the next
// completion's move keeps.
func TestCompletionStoresTheEndWithTheMoveToClosing(t *testing.T) {
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 500}
	fjURL, llURL := back.srv(t)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "in-review", Track: "direct"})

	if err := newCompletion(store, fjURL, llURL).Complete(context.Background(), "t1", "merged", "done", "pull request merged"); err == nil {
		t.Fatal("Complete succeeded against a refusing LiteLLM")
	}
	task, _, _ := store.Task(context.Background(), "t1")
	if task.Status != "closing" || task.ClosingFinal != "done" || task.ClosingReason != "pull request merged" {
		t.Fatalf("task = %s, end %q %q; want closing, done, pull request merged", task.Status, task.ClosingFinal, task.ClosingReason)
	}
}

// CloseTask moves a live card to closing with its end, and a card at
// closing again; a final card stays as it is.
func TestPGStoreCloseTaskStoresTheEnd(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events, tasks, cases CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []Task{
		{ID: "t-live", Repository: "sf/r", IssueNumber: 7, Kind: "agent", Status: "in-review", BudgetEUR: 1, Track: "direct"},
		{ID: "t-done", Repository: "sf/r", IssueNumber: 8, Kind: "agent", Status: "done", BudgetEUR: 1, Track: "direct"},
	} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	from := []string{"in-review", "closing"}
	for range 2 {
		if err := s.CloseTask(ctx, "t-live", from, "closed", "issue closed without merge"); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.Task(ctx, "t-live")
		if err != nil || got.Status != "closing" || got.ClosingFinal != "closed" || got.ClosingReason != "issue closed without merge" {
			t.Fatalf("t-live = %s, end %q %q (%v); want closing, closed, issue closed without merge", got.Status, got.ClosingFinal, got.ClosingReason, err)
		}
	}
	if err := s.CloseTask(ctx, "t-done", from, "failed", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CloseTask of a done card = %v, want ErrNotFound", err)
	}
	if got, _, _ := s.Task(ctx, "t-done"); got.Status != "done" || got.ClosingFinal != "" {
		t.Fatalf("t-done = %s, end %q; want done with none", got.Status, got.ClosingFinal)
	}
}

// scheduledActivities lists the activities hist scheduled and the
// encoded input of each.
func scheduledActivities(hist *historypb.History) (calls, inputs []string) {
	for _, e := range hist.Events {
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil {
			calls, inputs = append(calls, a.GetActivityType().GetName()), append(inputs, payloadsText(a.GetInput()))
		}
	}
	return calls, inputs
}

// reconciled is the Reconcile answer hist recorded, as JSON fields.
func reconciled(t *testing.T, hist *historypb.History) map[string]any {
	t.Helper()
	var reconcile int64
	for _, e := range hist.Events {
		if a := e.GetActivityTaskScheduledEventAttributes(); a.GetActivityType().GetName() == ActivityReconcile {
			reconcile = e.EventId
		}
		if c := e.GetActivityTaskCompletedEventAttributes(); c != nil && c.ScheduledEventId == reconcile {
			var st map[string]any
			if err := json.Unmarshal(c.GetResult().GetPayloads()[0].GetData(), &st); err != nil {
				t.Fatal(err)
			}
			return st
		}
	}
	t.Fatal("the recording holds no Reconcile answer")
	return nil
}

const closingReasonBackfill = "factoryd/20261127103000_closing_reason_backfill.sql"

// The backfill gives a closed card whose reason is empty or says only that
// its issue closed why from its records, by the end's order: the newest
// close answer (c-answer, c-factory, c-console, the plan's words of c-plan,
// over a record on c-over), else the first sentence of the newest close
// record (c-old and c-alert without the old lead, c-new, c-fenced), else
// the human's close in Forgejo (c-human, c-before). A record of dropped
// dependencies alone and the bot's own close say nothing (c-deps), and so
// does an empty newest record (c-newest). A card with its own reason
// (c-own), one not closed (c-failed) and one with no record (c-none) stay.
// A second run changes nothing.
func TestClosingReasonBackfill(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_closing_reason_backfill"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, track, issue_number, closing_reason, finished_at) VALUES
			('c-answer', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 1, '', now()),
			('c-factory', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 2, 'issue closed without merge', now()),
			('c-console', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 3, '', now()),
			('c-plan', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 4, '', now()),
			('c-old', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 5, 'issue closed without merge', now()),
			('c-alert', 1, 0, 'closed', 'sf/r', 'human', NULL, 6, '', now()),
			('c-new', 1, 0, 'closed', 'sf/r', 'human', NULL, 7, '', now()),
			('c-deps', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 8, 'issue closed without merge', now()),
			('c-human', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 9, 'issue closed without merge', now()),
			('c-before', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 10, 'issue closed before the plan filed its cards', now()),
			('c-own', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 11, 'salvaged into #5', now()),
			('c-failed', 1, 0, 'failed', 'sf/r', 'agent', 'direct', 12, '', now()),
			('c-none', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 13, '', now()),
			('c-over', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 14, '', now()),
			('c-fenced', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 15, 'issue closed without merge', now()),
			('c-newest', 1, 0, 'closed', 'sf/r', 'agent', 'direct', 16, '', now())`,
		`INSERT INTO cases (id, task_id, kind, state_key, status, problem, resolution) VALUES
			(901, 'c-answer', 'session_cap', 'cap', 'resolved', 'p', '{"by": "sebastian", "action": "close", "reason": "Superseded: the tests are on main. Session 3 proved it."}'),
			(902, 'c-factory', 'gate_stop', 'stop', 'resolved', 'p', '{"by": "factoryd", "action": "close"}'),
			(914, 'c-over', 'session_cap', 'cap', 'resolved', 'p', '{"by": "sebastian", "action": "close", "reason": "Superseded."}')`,
		`INSERT INTO events (repository, task_id, kind, actor, via, detail) VALUES
			('sf/r', 'c-answer', 'question_answered', 'sebastian', 'console', '{"action": "close", "case_id": 901, "answer": "Superseded: the tests are on main. Session 3 proved it."}'),
			('sf/r', 'c-factory', 'question_answered', 'factoryd', 'factoryd', '{"action": "close", "case_id": 902, "answer": "close"}'),
			('sf/r', 'c-console', 'card_closed', 'alice', 'console', '{"note": "Done by hand.\n\nTwice."}'),
			('sf/r', 'c-plan', 'card_closed', 'controller', 'console', '{"note": "closed by controller: Closed: the credits are out. The work lands locally."}'),
			('sf/r', 'c-own', 'card_closed', 'alice', 'console', '{"note": "Done."}'),
			('sf/r', 'c-failed', 'card_closed', 'alice', 'console', '{"note": "Done."}'),
			('sf/r', 'c-over', 'question_answered', 'sebastian', 'console', '{"action": "close", "case_id": 914, "answer": "Superseded."}')`,
		`INSERT INTO intake_events (task_id, kind, author, body, comment_id) VALUES
			('c-answer', 'factory_closed', 'factoryd', '', 1),
			('c-old', 'factory_closed', 'factoryd', E'factoryd: task t-23bbfa2371f4 closed: stopped for the replan of plan #1915\n\nIt no longer waits on #2176.', 5),
			('c-alert', 'factory_closed', 'factoryd', 'factoryd: the alert cleared at 2026-11-17T03:54:05Z; nobody touched the card, so it closes.', 6),
			('c-new', 'factory_closed', 'factoryd', 'The alert cleared at 2026-11-19T22:35:56Z. Nobody touched the card, so it closes.', 0),
			('c-deps', 'factory_closed', 'factoryd', 'It no longer waits on #1911.', 8),
			('c-deps', 'issue_closed', 'factory-agent', '', 8),
			('c-human', 'issue_closed', 'alice', '', 9),
			('c-before', 'issue_closed', 'alice', '', 10),
			('c-over', 'factory_closed', 'factoryd', 'Retried as #12.', 14),
			('c-fenced', 'factory_closed', 'factoryd', E'sebastian answered with “Close”.\n\n` + "```" + `\nnot needed\n` + "```" + `', 15),
			('c-newest', 'factory_closed', 'factoryd', 'Old reason.', 0),
			('c-newest', 'factory_closed', 'factoryd', '', 16)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	want := map[string]string{
		"c-answer":  "closed by sebastian: Superseded: the tests are on main.",
		"c-factory": "closed by the factory",
		"c-console": "closed by alice: Done by hand.",
		"c-plan":    "closed by controller: Closed: the credits are out.",
		"c-old":     "stopped for the replan of plan #1915",
		"c-alert":   "the alert cleared at 2026-11-17T03:54:05Z; nobody touched the card, so it closes.",
		"c-new":     "The alert cleared at 2026-11-19T22:35:56Z.",
		"c-deps":    "issue closed without merge",
		"c-human":   "closed in Forgejo by alice",
		"c-before":  "closed in Forgejo by alice",
		"c-own":     "salvaged into #5",
		"c-failed":  "",
		"c-none":    "",
		"c-over":    "closed by sebastian: Superseded.",
		"c-fenced":  "sebastian answered with “Close”.",
		"c-newest":  "",
	}
	for run := 1; run <= 2; run++ {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = $1`, closingReasonBackfill); err != nil {
			t.Fatal(err)
		}
		if err := schema.Migrate(ctx, s.db); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id, closing_reason FROM tasks`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for rows.Next() {
			var id, reason string
			if err := rows.Scan(&id, &reason); err != nil {
				t.Fatal(err)
			}
			got[id] = reason
		}
		rows.Close()
		for id, w := range want {
			if got[id] != w {
				t.Errorf("run %d: %s's closing reason %q, want %q", run, id, got[id], w)
			}
		}
	}
}
