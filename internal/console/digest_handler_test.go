package console

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The fold of a card's life answers a writer of the card's project with
// the card's digest and the steps of its open cases, oldest first, and
// refuses anyone else with 403.
func TestCardLifeFoldReadsTheDigestAndTheOpenCases(t *testing.T) {
	r := newRig(t)
	members(r)
	r.openCases = map[string][]factoryd.Case{"t-c": {{ID: 4, TaskID: "t-c"}, {ID: 9, TaskID: "t-c"}}}
	r.caseSteps = map[int64][]factoryd.CaseStep{
		4: {{Rung: 1, Actor: "checkin", Outcome: "escalated", Found: "the review & the tests disagree"}},
		9: {{Rung: 2, Actor: "factory agent", Outcome: "in_progress"}, {Rung: 3, Actor: "sebastian", Outcome: "in_progress"}},
	}
	ak, _ := memberLogin(t, r, "alice")
	rec := getPage(r, ak, "/tasks/t-c/digest")
	want := `<pre class="digest">#5 · budget 0.00 spent 0.00 holds 0.00 EUR · queued
flags: none
</pre>
<h4>Open cases</h4><ul><li>rung 1 by checkin → escalated: the review &amp; the tests disagree</li>` +
		`<li>rung 2 by factory agent → in_progress</li><li>rung 3 by sebastian → in_progress</li></ul>
`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("the fold for alice = %d\n%s\nwant 200\n%s", rec.Code, rec.Body, want)
	}
	bk, _ := memberLogin(t, r, "bob")
	if rec := getPage(r, bk, "/tasks/t-c/digest"); rec.Code != http.StatusForbidden {
		t.Fatalf("the fold for bob, who writes only demo = %d, want 403:\n%s", rec.Code, rec.Body)
	}
}

// The drawer folds the card's life and loads it once it shows: neither the
// board nor the drawer reads the digest.
func TestDrawerFoldsTheCardsLifeLazily(t *testing.T) {
	r := newRig(t)
	members(r)
	reads := 0
	digest := r.c.cfg.CardDigest
	r.c.cfg.CardDigest = func(ctx context.Context, taskID string) (factoryd.CardDigest, error) {
		reads++
		return digest(ctx, taskID)
	}
	ak, _ := memberLogin(t, r, "alice")
	if rec := getPage(r, ak, "/"); rec.Code != http.StatusOK {
		t.Fatalf("the board = %d", rec.Code)
	}
	drawer := getPage(r, ak, "/cards/t-c/drawer?tab=overview").Body.String()
	if want := `<details class="sec" data-sec="life"><summary>The card's life</summary>` +
		`<div class="sec-body" hx-get="/tasks/t-c/digest" hx-trigger="revealed">`; !strings.Contains(drawer, want) {
		t.Fatalf("the drawer lacks the fold\n%s\nin\n%s", want, drawer)
	}
	if reads != 0 {
		t.Fatalf("the board and the drawer read the digest %d times, want none", reads)
	}
}
