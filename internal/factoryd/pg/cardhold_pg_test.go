package pg

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A human's
// hold lands only on a card that has not started: queued and never
// admitted or resumed, or in waiting-deps on a dependency that has not
// ended. The row keeps it whole; the stamp refuses a held card; the board
// words it with its release card's plan; it follows its release card into
// a successor and a retry, as an edge does; an end that names the release
// card it moved from ends nothing, a human's end any hold that holds its
// card. A card that ends, or moves to the human lane, takes its hold with
// it and records the hold's end.
func TestPGStoreCardHold(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_card_hold"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-pl", Status: "waiting-human", IssueNumber: 1915, Track: policy.TrackPlan, Source: "webhook"},
		{ID: "t-r", Status: "running", IssueNumber: 2062, Source: SourcePlan, Parent: "t-pl"},
		{ID: "t-f", Status: "failed", IssueNumber: 2063},
		{ID: "t-gone", Status: "closed", IssueNumber: 2064},
		{ID: "t-q", Status: "queued", IssueNumber: 1960},
		{ID: "t-q2", Status: "queued", IssueNumber: 1961},
		{ID: "t-adm", Status: "queued", IssueNumber: 1962},
		{ID: "t-res", Status: "queued", IssueNumber: 1963},
		{ID: "t-run", Status: "running", IssueNumber: 1964},
		{ID: "t-roll", Status: "queued", IssueNumber: 1965, Source: SourceRoll},
		{ID: "t-noissue", Status: "queued"},
		{ID: "t-wd", Status: "waiting-deps", IssueNumber: 1966},
		{ID: "t-wd-ended", Status: "waiting-deps", IssueNumber: 1967},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		tk.Track, tk.Source = cmp.Or(tk.Track, policy.TrackDirect), cmp.Or(tk.Source, "webhook")
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE tasks SET admitted_at = now() WHERE id = 't-adm'; UPDATE tasks SET resumed_at = now() WHERE id = 't-res'`); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, "t-wd", []string{"t-gone", "t-r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, "t-wd-ended", []string{"t-gone"}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 17, 10, 15, 0, 0, time.UTC)
	hold := CardHold{Reason: "needs the finished pg split", Release: "t-r", By: "sebastian", At: at}
	var took []string
	for _, id := range []string{"t-q", "t-adm", "t-res", "t-run", "t-roll", "t-noissue", "t-wd", "t-wd-ended"} {
		ok, err := s.SetCardHold(ctx, id, hold)
		if err != nil {
			t.Fatalf("SetCardHold(%s): %v", id, err)
		}
		if ok {
			took = append(took, id)
		}
	}
	if !slices.Equal(took, []string{"t-q", "t-noissue", "t-wd"}) {
		t.Fatalf("held = %v, want t-q, the half-filed t-noissue and t-wd", took)
	}
	read := func(id string) Task {
		t.Helper()
		k, _, err := s.Task(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if h := read("t-q").Hold; h == nil || h.Reason != hold.Reason || h.Release != "t-r" || h.By != "sebastian" || !h.At.Equal(at) {
		t.Fatalf("the stored hold = %+v, want %+v", h, hold)
	}
	if h := read("t-adm").Hold; h != nil {
		t.Fatalf("an admitted card holds %+v, want none", h)
	}
	if err := s.SetAdmitted(ctx, "t-q"); !errors.Is(err, errHeld) {
		t.Fatalf("SetAdmitted on a held card = %v, want errHeld", err)
	}
	if k := read("t-q"); k.AdmittedAt != nil {
		t.Fatalf("a held card was stamped at %v", k.AdmittedAt)
	}
	board, err := s.BoardTasks(ctx, "sf/r")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(board, func(b BoardTask) bool { return b.ID == "t-q" })
	if v := board[i].HoldView; v.Line != "on hold until #2062 (plan #1915) is merged or done: needs the finished pg split" || v.Released {
		t.Fatalf("the board's hold = %+v", v)
	}

	// the release card fails and its successor carries its work: the hold follows
	if _, err := s.DB().ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE id = 't-r'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSuccessor(ctx, "t-r", Task{ID: "t-s", Status: "queued", IssueNumber: 2070, BudgetEUR: 1, Repository: "sf/r", Kind: "agent",
		Track: policy.TrackDirect, Source: SourceSuccessor, Parent: "t-pl"}); err != nil {
		t.Fatal(err)
	}
	if h := read("t-q").Hold; h == nil || h.Release != "t-s" {
		t.Fatalf("after the successor the hold = %+v, want its release card t-s", h)
	}
	// a retry's replacement carries a failed release card's work too
	if ok, err := s.SetCardHold(ctx, "t-q2", CardHold{Reason: "after #2063", Release: "t-f", By: "sebastian", At: at}); err != nil || !ok {
		t.Fatalf("SetCardHold(t-q2) = %v, %v", ok, err)
	}
	if ok, err := s.ReplaceCard(ctx, "t-f", "failed", Task{ID: "t-f2", Status: "queued", BudgetEUR: 1, Repository: "sf/r", Kind: "agent",
		Track: policy.TrackDirect, Source: "webhook"}); err != nil || !ok {
		t.Fatalf("ReplaceFailedCard = %v, %v", ok, err)
	}
	if h := read("t-q2").Hold; h == nil || h.Release != "t-f2" {
		t.Fatalf("after the retry the hold = %+v, want its release card t-f2", h)
	}

	if ended, err := s.EndCardHold(ctx, "t-q", "t-r"); err != nil || ended {
		t.Fatalf("an end by the release card the hold moved from = %v, %v; want nothing ended", ended, err)
	}
	if ended, err := s.EndCardHold(ctx, "t-q", "t-s"); err != nil || !ended {
		t.Fatalf("the end by its release card = %v, %v", ended, err)
	}
	if ended, err := s.EndCardHold(ctx, "t-wd", ""); err != nil || !ended {
		t.Fatalf("the human's end = %v, %v", ended, err)
	}
	if ended, err := s.EndCardHold(ctx, "t-wd", ""); err != nil || ended {
		t.Fatalf("a second end = %v, %v; want nothing ended", ended, err)
	}
	if k := read("t-q"); k.Hold != nil {
		t.Fatalf("an ended hold reads %+v", k.Hold)
	}
	if err := s.SetAdmitted(ctx, "t-q"); err != nil || read("t-q").AdmittedAt == nil {
		t.Fatalf("SetAdmitted once the hold ended = %v, want the stamp", err)
	}

	if err := s.CloseTask(ctx, "t-q2", []string{"queued"}, "closed", "the human closed it"); err != nil {
		t.Fatal(err)
	}
	if ended, err := s.EndCardHold(ctx, "t-q2", ""); err != nil || ended {
		t.Fatalf("the release of a closing card = %v, %v; want nothing ended", ended, err)
	}
	if err := s.EndClosed(ctx, "t-q2", []string{"closing"}, "the human closed it"); err != nil {
		t.Fatal(err)
	}
	// the held t-noissue moves to the human lane, where no hold holds
	if _, moved, err := s.CreateHumanCard(ctx, Task{ID: "t-noissue", Kind: "human", Repository: "sf/r", BudgetEUR: 1, Objective: "x"},
		HumanAsk{Q: HumanQuestion{Ask: "Which?"}, By: "factoryd", Asker: "webhook", Via: ViaFactoryd}); err != nil || !moved {
		t.Fatalf("the move to the human lane = %v, %v", moved, err)
	}
	var left int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id IN ('t-q2', 't-noissue')
		AND (hold_at IS NOT NULL OR hold_by <> '' OR hold_reason <> '' OR hold_release IS NOT NULL)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the hold columns of the closed and the moved card: %d rows set, %v; want none", left, err)
	}
	var ends []string
	rows, err := s.DB().QueryContext(ctx, `SELECT task_id || ' ' || actor || ' ' || (detail->>'reason') || ' ' || (detail->>'card_status')
		FROM events WHERE kind = 'card_hold_ended' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		ends = append(ends, e)
	}
	if want := []string{"t-q2 factoryd after #2063 closed", "t-noissue factoryd needs the finished pg split waiting-agent"}; !slices.Equal(ends, want) {
		t.Fatalf("the hold-end rows = %q, want %q", ends, want)
	}
}
