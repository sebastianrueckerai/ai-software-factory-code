package pg

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

var (
	offersOf        = factoryd.PgOffersOf
	offersOfAnswers = factoryd.PgOffersOfAnswers
	isTerminal      = factoryd.PgIsTerminal
)

// caseCard is the card of caseTask as a face reads it.
var caseCard = Task{ID: caseTask, Kind: "agent", IssueNumber: 1410}

// firstRaise is the amount "Add money" opens with on the budget floor of
// card at refusal why, on the facts of the card's next session on model.
func firstRaise(t *testing.T, r *Runner, card Task, why, model string) float64 {
	t.Helper()
	f, err := r.PgTripFacts(context.Background(), card, "", model)
	if err != nil {
		t.Fatal(err)
	}
	facts := Facts{CaseFacts: f, Card: card, Case: Case{Kind: CaseBudgetFloor, Facts: map[string]any{"reason": why}}}
	a, _ := factoryd.AnswerFor(ActFund, facts)
	res, err := a.Press(facts, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eur, _ := res.Args["eur"].(float64)
	return eur
}

func (g rungRig) budget(t *testing.T) float64 {
	t.Helper()
	var eur float64
	if err := g.s.PgDB().QueryRowContext(context.Background(), `SELECT budget_eur FROM tasks WHERE id = 't1'`).Scan(&eur); err != nil {
		t.Fatal(err)
	}
	return eur
}

// execActivity runs activity name of r in a test activity environment,
// under the interceptors the worker runs it with.
func execActivity(t *testing.T, r *Runner, name string, args ...any) (converter.EncodedValue, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{&factoryd.TerminalForgejoErrors{},
		&factoryd.GateWatch{Store: r.PgStore(), Bound: func(context.Context, string, int64, int) time.Duration { return time.Hour }}}})
	r.RegisterActivities(env)
	return env.ExecuteActivity(name, args...)
}
