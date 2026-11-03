package acceptance

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestPoolDoesNotLiftSpendCaps proves through the real gateway that a
// project's month pool — standing, and after a confirmed Top up of it
// through the console — lifts none of the spend ceilings every call is
// held to: the day cap and the month cap of policy.yaml and the
// project's own cap in its file. Each ceiling binds on its own with the
// other two loose, EUR 0.50 of free headroom against a prompt whose
// irreducible reservation is one euro: the refusal names its bound,
// quotes the free headroom of all four, and keeps the reason's key —
// the month cap's or the project's in its file — a human raises by a
// commit. The confirmed Top up of EUR 1 lands through the console's own
// surface on the recovered policy fixture, moves discretionary_eur and
// no other key, and the gateway under the landed policy version, a
// fresh instance with the version the reload loaded, refuses every
// ceiling exactly as before: the hold requests carry the same ceilings,
// no key is minted, no call is proxied, nothing is booked.
func TestPoolDoesNotLiftSpendCaps(t *testing.T) {
	// the gateway's policy, from the file a policy PR wrote: the pool at
	// EUR 10 under the caps it cannot lift
	head, err := accGatewayPolicy(1, gwPoolHead)
	if err != nil {
		t.Fatal(err)
	}
	gwCapsRefuse(t, "with the pool at EUR 10", head)

	// the confirmed Top up: EUR 1 onto the pool, proposed by the agent
	// through the project's chat and confirmed by the member's press, on
	// the recovered policy fixture with the same file on infra's main
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	r.PutInfraFile(accInfraFile, gwPoolHead)
	topUp := `{"action":"top_up","eur":1,"reason":"the findings cost money"}`
	token := proposePool(t, r, path, ck, csrf, topUp)
	r.OnReload(func() { r.SetLoadedPool(11) })
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("the top-up's Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the top-up's Do it", body, topUp,
		"The pool of "+accProjectName+" is €11.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it.")

	// the one write the top-up made: only discretionary_eur moved
	equal(t, "main's file", r.InfraFiles()[accInfraFile], gwPoolLanded)
	commits := r.InfraCommits()
	if len(commits) != 1 {
		t.Fatalf("the top-up made %d commits, want the one: %+v", len(commits), commits)
	}
	if msg := commits[0].Message; !strings.HasPrefix(msg, "Project "+accProjectName+": month pool +1.00 EUR\n\n") {
		t.Errorf("the commit's message = %q", msg)
	}

	// the landed file's own policy version: the pool moved to 11, every
	// ceiling stands
	after, err := accGatewayPolicy(2, gwPoolLanded)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the global ceilings under the landed policy", after.Document.Budget, head.Document.Budget)
	landedBudget := after.Tree.Projects[accProjectName].Budget
	if landedBudget == nil || landedBudget.MonthEUR != gwProjectCapEUR || landedBudget.TaskMaxEUR != 5 ||
		landedBudget.RaiseMaxEUR != 1 || landedBudget.DiscretionaryEUR != 11 {
		t.Fatalf("the landed project budget = %+v, want only the pool moved to 11", landedBudget)
	}

	// and the gateway under it, a fresh instance reading the version the
	// reload loaded, holds every ceiling exactly as before
	gwCapsRefuse(t, "with the pool at EUR 11", after)
}

// gwCapsRefuse runs the three ceilings' scenarios against the policy
// version the gateway enforces: each bound alone binds — the day cap
// with a sibling's spend and its live hold both counted, the month cap,
// the project's own under its lower cap — and the refusal names its
// bound with the reason literal a member reads, the figures of all four
// bounds quoted from the version's own ceilings. No scenario reaches
// the backend.
func gwCapsRefuse(t *testing.T, what string, v policy.Version) {
	t.Helper()
	body, bytes := gwChatBody(t), gwBodyBytes(t)
	for _, tc := range []struct {
		bound    string
		seed     func(*accGatewayStore)
		decision string
		reason   string
	}{
		{
			bound: "the day cap",
			seed: func(s *accGatewayStore) {
				// another project's task spent EUR 98.50 today and holds
				// one euro live: the day's free headroom is EUR 0.50 with
				// the hold counted once, the month's EUR 500.50
				s.seedDay(gwOtherRepo.ID, 98.5)
				s.seedHold(gwOtherRepo.ID, 1)
			},
			decision: policy.RefusedDayCap,
			reason: "free headroom 0.500000 EUR (task 100.000000, day 0.500000, month 500.500000, project 100.000000) " +
				"fits 0 output tokens after a request of " + strconv.Itoa(bytes) + " bytes (at most " + strconv.Itoa(bytes) + " prompt tokens), " +
				"below the minimum call of " + strconv.Itoa(policy.MinCallTokens),
		},
		{
			bound: "the month cap",
			seed: func(s *accGatewayStore) {
				// another project's tasks spent EUR 599.50 earlier this
				// UTC month: the month's free headroom is EUR 0.50, the
				// day's and the project's loose
				s.seedMonth(gwOtherRepo.ID, 599.5)
			},
			decision: policy.RefusedMonthCap,
			reason: "free headroom 0.500000 EUR (task 100.000000, day 100.000000, month 0.500000, project 100.000000) " +
				"fits 0 output tokens after a request of " + strconv.Itoa(bytes) + " bytes (at most " + strconv.Itoa(bytes) + " prompt tokens), " +
				"below the minimum call of " + strconv.Itoa(policy.MinCallTokens) +
				"; the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit",
		},
		{
			bound: "the project's own cap",
			seed: func(s *accGatewayStore) {
				// the code project's own sibling spent EUR 99.50 earlier
				// this UTC month: under the project cap of EUR 100 its
				// free headroom is EUR 0.50, the month's EUR 500.50
				s.seedMonth(gwOwnRepo.ID, 99.5)
			},
			decision: policy.RefusedProjectCap,
			reason: "free headroom 0.500000 EUR (task 100.000000, day 100.000000, month 500.500000, project 0.500000) " +
				"fits 0 output tokens after a request of " + strconv.Itoa(bytes) + " bytes (at most " + strconv.Itoa(bytes) + " prompt tokens), " +
				"below the minimum call of " + strconv.Itoa(policy.MinCallTokens) +
				"; the project cap budget.month_eur in projects/" + accProjectName + ".yaml is spent, and only a human raises it, by a commit",
		},
	} {
		store := newAccGatewayStore(gwCaller, gwOtherRepo, gwOwnRepo)
		tc.seed(store)
		store.setPolicy(v)
		minter, stub := &accGatewayMinter{}, newAccStubLiteLLM(t)
		h := gwGateway(store, minter, accGatewayPrices{price: gwPrice(bytes)}, stub.URL)
		req := gwRefused(t, what+", "+tc.bound, h, body, store, minter, stub, v.ID, tc.decision, tc.reason)
		gwHoldRequest(t, what+", "+tc.bound, req, bytes)
	}
}
