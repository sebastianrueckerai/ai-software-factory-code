package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

// TestRetrySwitchEndsFreeze: the park rig's console and runner, the
// compiled gateway and the freeze card watcher over one database, two
// coder cards of the zai class running the real TaskWorkflow in Temporal's
// test environment. Each card's session calls the gateway as its sandbox:
// the first meets the provider's dated refusal and arms the class's hold,
// the other is answered by the hold, and both wait to the reset beside
// the one freeze card. From each entry point — the freeze card's Retry
// now, an approver's answer on the card's ask, and the chat's retry on a frozen card, which the runner finds held
// by the freeze card under the policy in force, or on the freeze card
// itself — the press clears the hold, closes the card at once and wakes
// both cards, whose next sessions start at the press. The first press's
// probe meets a dated refusal naming a new reset: the hold re-arms, the
// same card reopens with a new ask and both cards wait to the new reset,
// with no case and no question of their own. The second press's probe
// succeeds: both cards reach the provider and the card stays closed on
// class_resumed. Each press closes the card with its own comment naming
// who pressed, and leaves its freeze_retried row: who, how, the class, the
// card closed and both cards woken. A freeze three days after the success,
// past the reset the card held, files a new card.
// [retry_switch_ends_freeze]
func TestRetrySwitchEndsFreeze(t *testing.T) {
	for _, entry := range []struct {
		name       string
		chat, work bool // the chat's retry, on a frozen work card
	}{{"Retry now", false, false}, {"the chat's retry on a frozen card", true, true}, {"the chat's retry on the freeze card", true, false}} {
		t.Run(entry.name, func(t *testing.T) {
			r := newParkRig(t)
			ctx := context.Background()
			tree := plFrozenVersion(r)
			tree.Document.Approvers = []string{parkMember}
			v := r.loadPolicy(t, "c-retry", tree)
			gwStore, err := factorygateway.OpenStore(ctx, r.dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = gwStore.DB().Close() })
			stub := newAccStubLiteLLM(t)
			gw := httptest.NewServer(factorygateway.NewServer(factorygateway.ServerOptions{
				Auth: accGatewayAuth{}, Store: gwStore, Minter: &accGatewayMinter{},
				Prices:     accGatewayPrices{price: policy.Price{In: 1e-9, Out: 1e-9, MaxOutput: 32768}},
				LiteLLMURL: stub.URL, FactorydToken: "factoryd-token",
			}).Routes("retry-proof"))
			t.Cleanup(gw.Close)
			forge, fj := newAccFreezeForgejo(t)
			watch := &factoryd.FreezeCard{Store: r.store, Forgejo: forge, Current: func() policy.Version { return v }, Timeline: r.runner.Timeline()}

			start := time.Now().UTC().Truncate(time.Second)
			env, _ := plWaitEnv(t, r.store, r.objects, start)
			var mu sync.Mutex
			var woke []string
			watch.Signal = func(_ context.Context, id string) error {
				mu.Lock()
				woke = append(woke, id)
				mu.Unlock()
				return env.SignalWorkflowByID(factoryd.WorkflowID(id), factoryd.SignalProviderRetry, factoryd.TaskSignal{Author: "factoryd"})
			}
			r.runner.RetryFreeze = (&factoryd.RetrySwitch{Freeze: watch, GatewayURL: gw.URL, Token: "factoryd-token"}).RetryFreeze

			// the provider's dated refusal, its reset in Singapore wall
			// time; the gateway holds to it plus its minute's margin
			limit := func(in time.Duration) string {
				return `{"error":{"message":"Usage limit reached for 5 hour. Your limit will reset at ` +
					start.Add(in+8*time.Hour).Format("2006-01-02 15:04:05") + `"}}`
			}
			first, renewed := start.Add(5*time.Hour+time.Minute), start.Add(9*time.Hour+time.Minute)
			stub.serve(http.StatusTooManyRequests, limit(5*time.Hour), nil)

			cards := []string{"t-retry-1", "t-retry-2"}
			for i, id := range cards {
				if err := r.store.CreateTask(ctx, factoryd.Task{ID: id, Status: "queued", Objective: "work through the freeze", Repository: parkRepo,
					Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: int64(61 + i), SHA: wipBase,
					RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
					t.Fatal(err)
				}
			}
			// a session calls the gateway once as its card's sandbox: a
			// named refusal is the card's wait to the reset it names; an
			// answer is the card at work again, whose run the proof ends
			// there, its issue closed
			call := gwChatBody(t)
			began := map[string][]time.Time{}
			env.RegisterActivityWithOptions(func(ctx context.Context, in factoryd.SessionInput) (factoryd.SessionResult, error) {
				mu.Lock()
				defer mu.Unlock()
				began[in.TaskID] = append(began[in.TaskID], env.Now().UTC())
				res := factoryd.SessionResult{SessionID: fmt.Sprintf("s-%d", len(began[in.TaskID]))}
				if err := r.store.SetTaskModelAlias(ctx, in.TaskID, "small"); err != nil {
					return res, err
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(call))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+in.TaskID)
				rec := httptest.NewRecorder()
				gw.Config.Handler.ServeHTTP(rec, req)
				if rec.Code == http.StatusOK {
					// the close lands on the environment's loop before the
					// session ends, so the run ends inside this session
					closed := make(chan error, 1)
					env.RegisterDelayedCallback(func() {
						closed <- env.SignalWorkflowByID(factoryd.WorkflowID(in.TaskID), factoryd.SignalIssueClosed, factoryd.TaskSignal{Author: parkMember})
					}, 0)
					res.Outcome = factoryd.OutcomeNoPush
					return res, <-closed
				}
				var refused struct{ Reset, Words string }
				if rec.Header().Get(factorygateway.HeaderDecision) != factorygateway.RefusedProviderLimit || json.Unmarshal(rec.Body.Bytes(), &refused) != nil {
					return res, fmt.Errorf("the gateway answered %s %d: %s", in.TaskID, rec.Code, rec.Body)
				}
				until, err := time.Parse(time.RFC3339, refused.Reset)
				res.Outcome, res.Status, res.Until, res.Words = factoryd.OutcomeProviderRefused, http.StatusTooManyRequests, until, refused.Words
				res.Reason = "provider limit for class " + policy.ClassCoder + ": reset " + refused.Reset + ": " + refused.Words
				return res, err
			}, activity.RegisterOptions{Name: factoryd.ActivityRunSession})
			env.RegisterWorkflowWithOptions(func(ctx workflow.Context) ([]string, error) {
				var runs []workflow.ChildWorkflowFuture
				for _, id := range cards {
					runs = append(runs, workflow.ExecuteChildWorkflow(workflow.WithChildOptions(ctx,
						workflow.ChildWorkflowOptions{WorkflowID: factoryd.WorkflowID(id)}), "TaskWorkflow", id))
				}
				finals := make([]string, len(runs))
				for i, run := range runs {
					if err := run.Get(ctx, &finals[i]); err != nil {
						return nil, err
					}
				}
				return finals, nil
			}, workflow.RegisterOptions{Name: "FrozenCards"})

			// the member logs in once the freeze card stands, Retry now an
			// answer of its ask
			var ck *http.Cookie
			var csrf string
			press := func(card string) {
				t.Helper()
				mu.Lock()
				woke = nil
				mu.Unlock()
				if entry.work {
					card = cards[0]
				}
				if !entry.chat {
					asks, err := r.store.OpenCasesOf(ctx, card)
					if err != nil || len(asks) != 1 {
						t.Fatalf("the freeze card's open asks = %+v, %v; want its one ask", asks, err)
					}
					req := httptest.NewRequest(http.MethodPost, "/tasks/"+card+"/answer",
						strings.NewReader(url.Values{"csrf": {csrf}, "case": {fmt.Sprint(asks[0].ID)}, "value": {factoryd.ActRetry}}.Encode()))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					req.Header.Set("HX-Request", "true")
					req.AddCookie(ck)
					rec := httptest.NewRecorder()
					r.console.ServeHTTP(rec, req)
					if rec.Code != http.StatusNoContent {
						t.Fatalf("Retry now = %d: %s", rec.Code, rec.Body)
					}
				} else {
					r.Reply([]string{Call(1, "answer", `{"task_id":"`+card+`","action":"retry"}`)})
					code, body := r.postChat(t, ck, url.Values{"message": {"Retry the provider now."}, "csrf": {csrf}})
					var offer struct{ ID, Tool string }
					stream := parkEvents(t, body)
					if code != http.StatusOK || len(stream) == 0 || json.Unmarshal([]byte(strings.TrimPrefix(stream[0], "proposal ")), &offer) != nil || offer.Tool != "answer" {
						t.Fatalf("the chat's retry = %d %v, want the answer's proposal", code, stream)
					}
					if code, body := r.postChat(t, ck, url.Values{"csrf": {csrf}}, "/proposals/"+offer.ID); code != http.StatusOK || !slices.ContainsFunc(parkEvents(t, body),
						func(e string) bool { return strings.HasPrefix(e, "applied ") }) {
						t.Fatalf("Do it = %d:\n%s", code, body)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if slices.Sort(woke); !slices.Equal(woke, cards) {
					t.Errorf("the press woke %q, want both frozen cards", woke)
				}
			}
			// the card's face names the reset it holds and both cards; a
			// card the press closed names who pressed instead
			freezeCard := func(what, status string, reset time.Time) factoryd.Task {
				t.Helper()
				if err := watch.Check(ctx); err != nil {
					t.Fatal(err)
				}
				fc, ok := plFreezeCard(ctx, t, r.store)
				names := "2 cards of the class wait"
				if status == "done" {
					names = parkMember + " cleared the class's hold."
				}
				if face := "The class resets at " + reset.Format(time.RFC3339); !ok || fc.Status != status || !strings.Contains(fc.Objective, face) ||
					!strings.Contains(fc.Objective, names) || plFreezeCards(t, r.store.DB()) != 1 || len(fj.issues) != 1 {
					t.Fatalf("%s: the freeze card = %+v (%d issues), want the one card %s naming %s for both cards", what, fc, len(fj.issues), status, reset)
				}
				return fc
			}
			pressed := func(what string, card factoryd.Task) {
				t.Helper()
				if got, _, err := r.store.Task(ctx, card.ID); err != nil || got.Status != "done" || fj.patches[card.IssueNumber] != "closed" {
					t.Errorf("%s: the freeze card = %s (issue %q), %v; want it closed at once", what, got.Status, fj.patches[card.IssueNumber], err)
				}
			}

			var fc factoryd.Task
			env.RegisterDelayedCallback(func() {
				fc = freezeCard("the arm", "waiting-human", first)
				ck, csrf = r.login(t)
			}, 10*time.Minute)
			env.RegisterDelayedCallback(func() {
				stub.serve(http.StatusTooManyRequests, limit(9*time.Hour), nil)
				press(fc.ID)
				pressed("the first press", fc)
			}, 20*time.Minute)
			env.RegisterDelayedCallback(func() {
				if again := freezeCard("the re-arm", "waiting-human", renewed); again.ID != fc.ID {
					t.Errorf("the re-arm filed %s, want the same card %s", again.ID, fc.ID)
				}
				stub.serve(http.StatusOK, "{}", nil)
				press(fc.ID)
				pressed("the second press", fc)
			}, 30*time.Minute)
			env.ExecuteWorkflow("FrozenCards")
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("the cards' workflows: %v", err)
			}
			var finals []string
			if err := env.GetWorkflowResult(&finals); err != nil || !slices.Equal(finals, []string{"closed", "closed"}) {
				t.Fatalf("the cards ended %q, %v; want both closed", finals, err)
			}
			pressed("past the success", freezeCard("past the success", "done", renewed))

			// both cards woke at each press, not at a reset: the first
			// press's probe met the new reset and held to it, the second's ran
			for _, id := range cards {
				if want := []time.Time{start, start.Add(20 * time.Minute), start.Add(30 * time.Minute)}; !slices.EqualFunc(began[id], want, time.Time.Equal) {
					t.Errorf("%s's sessions began at %v, want the start and each press", id, began[id])
				}
				holds := plWaitHolds(t, r.objects, id)
				if len(holds) != 2 || holds[0]["backoff_s"] != first.Sub(start).Seconds() || holds[1]["backoff_s"] != renewed.Sub(start.Add(20*time.Minute)).Seconds() {
					t.Errorf("%s's holds = %v, want one to the first reset, then one from the first press to the new reset", id, holds)
				}
				if cases, err := r.store.CasesOf(ctx, id); err != nil || len(cases) != 0 {
					t.Errorf("%s's cases = %+v, %v; want none: the freeze asks no one", id, cases, err)
				}
			}
			// one call armed the hold and one re-armed it, the other card
			// answered by the hold each time; the success reached the
			// provider for both
			if calls := stub.calls(); len(calls) != 4 {
				t.Errorf("provider calls %v, want the arm, the re-arm and both probes of the success", calls)
			}
			rows, err := r.store.DB().QueryContext(ctx, `SELECT kind, CASE WHEN kind = $1 THEN COALESCE(task_id, '') ELSE COALESCE(detail->>'reset', '') END FROM events
				WHERE kind IN ('class_frozen', 'class_resumed', $1) ORDER BY id`, factoryd.EventQuestionAsked)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var spine []string
			for rows.Next() {
				var kind, of string
				if err := rows.Scan(&kind, &of); err != nil {
					t.Fatal(err)
				}
				spine = append(spine, kind+" "+of)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			asked := factoryd.EventQuestionAsked + " " + fc.ID
			if want := []string{"class_frozen " + first.Format(time.RFC3339), asked, "class_resumed " + first.Format(time.RFC3339),
				"class_frozen " + renewed.Format(time.RFC3339), asked, "class_resumed " + renewed.Format(time.RFC3339)}; !slices.Equal(spine, want) {
				t.Errorf("the rows = %q, want %q: each press resumed the hold it cleared, and the freeze card alone asked, once for each hold", spine, want)
			}

			// each press closed the card with its own comment naming who
			// pressed, and left its row: who, how, the class, the card it
			// closed and the two cards it woke
			pressSaid := fmt.Sprintf("#issues/%d %s's retry press cleared the %s class's hold: the freeze card closes at once.", fc.IssueNumber, parkMember, policy.ClassCoder)
			if n := len(slices.DeleteFunc(slices.Clone(fj.comments), func(c string) bool { return c != pressSaid })); n != 2 {
				t.Errorf("the card's comments %q hold %d press closes, want one per press: %q", fj.comments, n, pressSaid)
			}
			via := factoryd.ViaButton
			if entry.chat {
				via = factoryd.ViaChat
			}
			presses, err := r.store.DB().QueryContext(ctx, `SELECT concat_ws(' ', task_id, actor, via, detail->>'class', detail->>'closed', detail->>'woken', detail->>'frozen')
				FROM events WHERE kind = $1 ORDER BY id`, factoryd.EventFreezeRetried)
			if err != nil {
				t.Fatal(err)
			}
			defer presses.Close()
			var pressRows []string
			for presses.Next() {
				var row string
				if err := presses.Scan(&row); err != nil {
					t.Fatal(err)
				}
				pressRows = append(pressRows, row)
			}
			if err := presses.Err(); err != nil {
				t.Fatal(err)
			}
			row := strings.Join([]string{fc.ID, parkMember, via, policy.ClassCoder, "1", "2", "2"}, " ")
			if want := []string{row, row}; !slices.Equal(pressRows, want) {
				t.Errorf("the presses' rows = %q, want %q", pressRows, want)
			}

			// a freeze three days after the success lies past the reset the
			// card held: a new episode, a new card, the pressed one left done
			comments := len(fj.comments)
			threeDays := time.Now().UTC().Add(72 * time.Hour)
			plFreezeEventAt(t, r.store.DB(), parkRepo, threeDays, "class_frozen", policy.ClassCoder, threeDays.Add(5*time.Hour).Format(time.RFC3339), "Usage limit reached for 5 hour.")
			if err := watch.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if old, _, err := r.store.Task(ctx, fc.ID); err != nil || old.Status != "done" || plFreezeCards(t, r.store.DB()) != 2 || len(fj.issues) != 2 || len(fj.comments) != comments {
				t.Errorf("the freeze three days on left the pressed card %s (%v), %d cards on %d issues, comments %q; want a new card",
					old.Status, err, plFreezeCards(t, r.store.DB()), len(fj.issues), fj.comments[comments:])
			}
		})
	}
}
