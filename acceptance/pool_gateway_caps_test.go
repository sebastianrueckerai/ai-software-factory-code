package acceptance

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The acceptance proof that a project's escalation pool lifts no spend
// ceiling (the pool-gateway caps card of plan #1399), on the recovered
// pool-gateway rig: the real gateway handler, production policy.Fit and
// the rig's TakeHold, against each ceiling bound on its own with the
// other two loose — the day's EUR 100 and the month's EUR 600 of
// policy.yaml, which no project file moves, and the code project's own
// lower month cap of EUR 100 in its file. Every scenario leaves EUR
// 0.50 of the bound's free headroom against a prompt whose reservation
// is irreducibly one euro, so fitting cannot shrink the call under the
// cap: the refusal names its bound, quotes all four free figures —
// which are also the proof that the caller's card-funded allocation
// books no spend — and keeps the key a human raises by a commit. Spend
// alone and spend beside a sibling's live hold both bind, the hold
// counted once. After a human-confirmed Top up of EUR 1 through the
// console's own surface, the landed file's fresh policy version —
// installed explicitly into a fresh gateway, never read off a merge's
// or reload's acknowledgement — holds every ceiling exactly as before.

// TestPoolDoesNotLiftSpendCaps runs the ceiling matrix under the file a
// policy PR wrote (the pool at EUR 10), then again under the file a
// human-confirmed Top up landed (the pool at EUR 11): three ceilings,
// each bound on its own, before and after. The top-up is the real chat
// flow (the agent's set_pool proposal, the human's Do it, the guarded
// commit, pull request and merge on the configuration repository's
// fake, the reload's activation), and only escalation.pool.eur moves: the
// fixed day and month caps of policy.yaml and the project's own lower
// cap stand, and the gateway under the landed version refuses every
// ceiling with the same decisions, the same literal wording and the
// same figures, no key minted and nothing proxied in any scenario.
func TestPoolDoesNotLiftSpendCaps(t *testing.T) {
	head, err := accGatewayPolicy(1, gwPoolHead)
	if err != nil {
		t.Fatal(err)
	}
	gwCapsRefusals(t, "the pool at EUR 10", head)

	// the confirmed Top up of EUR 1 through the console's own surface,
	// on the same project file the gateway's policy version reads
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	r.PutInfraFile(accInfraFile, gwPoolHead)
	topUp := `{"action":"top_up","eur":1,"reason":"the findings cost money"}`
	token := proposePool(t, r, path, ck, csrf, topUp)
	equal(t, "the writes the proposal alone made", poolWrites(r), 0)
	r.OnReload(func() { r.SetLoadedPool(11) })
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("the top-up's Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the top-up's Do it", body, topUp,
		"The pool of "+accProjectName+" is €11.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it.")

	// the one write the top-up made: only escalation.pool.eur moved, from
	// 10 to 11 — the comment, the caps and every other key stand
	landedFile := r.InfraFiles()[accInfraFile]
	equal(t, "main's file after the top-up", landedFile, gwPoolLanded)
	commits := r.InfraCommits()
	if len(commits) != 1 {
		t.Fatalf("the top-up made %d commits, want the one: %+v", len(commits), commits)
	}
	if msg := commits[0].Message; !strings.HasPrefix(msg, "Project "+accProjectName+": escalation pool +1.00 EUR\n\n") {
		t.Errorf("the top-up's commit message = %q", msg)
	}
	// the pool the policy in force holds is the reload's activation
	// alone: the merge's acknowledgement changed no policy
	equal(t, "the reload requests", r.Reloads(), 1)
	loaded, ok := r.LoadedPool()
	equal(t, "the loaded pool", []any{ok, loaded}, []any{true, 11.0})
	rows := r.Events()
	if len(rows) != 1 {
		t.Fatalf("%d event rows, want the one pool_changed: %+v", len(rows), rows)
	}
	equal(t, "the top-up's row", []any{rows[0].Kind, rows[0].Actor, rows[0].Via, rows[0].Repository},
		[]any{factoryd.EventPoolChanged, "sebastian", factoryd.ViaChat, "sf/code"})
	equal(t, "the top-up's row detail", rows[0].Detail, map[string]any{"action": "top_up", "from_eur": 10.0,
		"confirmed_eur": 1.0, "to_eur": 11.0, "reason": "the findings cost money", "pr": "sf/infra#101"})

	// the landed file's own policy version, installed explicitly into
	// the gateway below — a fresh version and fresh instances, not a
	// merge's acknowledgement read as activation: the pool moved to 11
	// and no ceiling moved
	after, err := accGatewayPolicy(2, landedFile)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the fixed global caps under the landed policy", after.Document.Budget, head.Document.Budget)
	landed := after.Tree.Projects[accProjectName]
	if b := landed.Budget; b == nil || b.MonthEUR != gwProjectCapEUR || b.TaskMaxEUR != 5 ||
		landed.Escalation == nil || landed.Escalation.Pool != (policy.Draw{Sessions: 30, EUR: 11}) {
		t.Fatalf("the landed project = %+v, want only the pool moved to EUR 11 under the unchanged cap", landed)
	}
	gwCapsRefusals(t, "the pool at EUR 11", after)
}

// gwCapsRefusals runs the three ceilings' scenarios against the policy
// version the gateway enforces: the day cap and the month cap seeded on
// a sibling of another project (outside the code project's own cap),
// the project's cap on a sibling of the code project itself, each bound
// alone with the other limits loose. Every scenario seeds EUR 99.50 of
// the bound — the day's booked today, the month's and the project's
// earlier in the UTC month, which leaves the day loose — either as
// spend alone or as EUR 98.50 of spend beside a sibling's live hold of
// EUR 1, so the hold's single count is the difference between the
// refusal's EUR 0.50 of free headroom and the minus EUR 0.50 a double
// count would quote. The caller's own figure is its whole EUR 100
// card-funded allocation — the pool's fund raised it, an allocation
// books no ledger row — so the day's, the month's and the project's
// figures, their ceilings less the seeded spend alone, prove the funded
// money contributed no model spend. No scenario reaches the backend:
// nothing minted, nothing proxied, nothing booked.
func gwCapsRefusals(t *testing.T, what string, v policy.Version) {
	t.Helper()
	body, bytes := gwChatBody(t), gwBodyBytes(t)
	refusal := "free headroom 0.500000 EUR (task 100.000000, "
	tail := ") fits 0 output tokens after a request of " + strconv.Itoa(bytes) + " bytes (at most " +
		strconv.Itoa(bytes) + " prompt tokens), below the minimum call of " + strconv.Itoa(policy.MinCallTokens)
	for _, tc := range []struct {
		bound    string
		seed     func(*accGatewayStore)
		decision string
		reason   string
	}{
		{
			bound: "the day cap, spend alone",
			seed: func(s *accGatewayStore) {
				s.seedDay(gwOtherRepo.ID, 99.5) // today, so this month too: day 0.50, month 500.50
			},
			decision: policy.RefusedDayCap,
			reason:   refusal + "day 0.500000, month 500.500000, project 100.000000" + tail,
		},
		{
			bound: "the day cap, spend beside a hold counted once",
			seed: func(s *accGatewayStore) {
				s.seedDay(gwOtherRepo.ID, 98.5)
				s.seedHold(gwOtherRepo.ID, 1) // counted once: day 0.50, not -0.50
			},
			decision: policy.RefusedDayCap,
			reason:   refusal + "day 0.500000, month 500.500000, project 100.000000" + tail,
		},
		{
			bound: "the month cap, spend alone",
			seed: func(s *accGatewayStore) {
				s.seedMonth(gwOtherRepo.ID, 599.5) // earlier this UTC month: the day stays loose
			},
			decision: policy.RefusedMonthCap,
			reason: refusal + "day 100.000000, month 0.500000, project 100.000000" + tail +
				"; the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit",
		},
		{
			bound: "the month cap, spend beside a hold counted once",
			seed: func(s *accGatewayStore) {
				s.seedMonth(gwOtherRepo.ID, 598.5)
				s.seedHold(gwOtherRepo.ID, 1) // counts against the day too: day 99, month 0.50
			},
			decision: policy.RefusedMonthCap,
			reason: refusal + "day 99.000000, month 0.500000, project 100.000000" + tail +
				"; the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit",
		},
		{
			bound: "the project's own cap, spend alone",
			seed: func(s *accGatewayStore) {
				s.seedMonth(gwOwnRepo.ID, 99.5) // the code project's sibling, under its cap of EUR 100
			},
			decision: policy.RefusedProjectCap,
			reason: refusal + "day 100.000000, month 500.500000, project 0.500000" + tail +
				"; the project cap budget.month_eur in projects/" + accProjectName + ".yaml is spent, and only a human raises it, by a commit",
		},
		{
			bound: "the project's own cap, spend beside a hold counted once",
			seed: func(s *accGatewayStore) {
				s.seedMonth(gwOwnRepo.ID, 98.5)
				s.seedHold(gwOwnRepo.ID, 1) // a same-repo hold binds the project too: project 0.50, not -0.50
			},
			decision: policy.RefusedProjectCap,
			reason: refusal + "day 99.000000, month 500.500000, project 0.500000" + tail +
				"; the project cap budget.month_eur in projects/" + accProjectName + ".yaml is spent, and only a human raises it, by a commit",
		},
	} {
		store := newAccGatewayStore(gwCaller, gwOtherRepo, gwOwnRepo)
		tc.seed(store)
		store.setPolicy(v)
		store.assertTaskLedger(t)
		minter, stub := &accGatewayMinter{}, newAccStubLiteLLM(t)
		h := gwGateway(store, minter, accGatewayPrices{price: gwPrice(bytes)}, stub.URL)
		req := gwRefused(t, what+", "+tc.bound, h, body, store, minter, stub, v.ID, tc.decision, tc.reason)
		gwHoldRequest(t, what+", "+tc.bound, req, bytes)
	}
}
