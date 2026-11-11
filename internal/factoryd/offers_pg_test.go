package factoryd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func storedOffers(t *testing.T, ask map[string]any) []AskOffer {
	t.Helper()
	b, err := json.Marshal(ask["offers"])
	if err != nil {
		t.Fatal(err)
	}
	var out []AskOffer
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("stored offers %s: %v", b, err)
	}
	return slices.DeleteFunc(out, func(o AskOffer) bool { return o.Effect == "" })
}

func offerValues(offers []AskOffer) []string {
	var out []string
	for _, o := range offers {
		out = append(out, o.Value)
	}
	return out
}

// A case reaches the human with the offers the live facts allow stored on its
// ask, and an Act refusal of a press on it stores them anew.
func TestACaseReachesTheHumanWithItsOffersStoredAndARefusalRefreshesThem(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.escalateDouble(ctx, "t1", capReached+"\nlast: no session produced a result", []string{"a.txt", "b.txt"}); err != nil {
		t.Fatal(err)
	}
	asks := g.asks(t)
	if len(asks) != 1 {
		t.Fatalf("asks = %+v, want one", asks)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if len(cases) != 1 {
		t.Fatalf("cases = %+v, want one", cases)
	}
	c := cases[0]
	stored := storedOffers(t, asks[0].Detail)
	f, err := g.r.factsAtCase(ctx, Task{ID: "t1", Repository: "demo-app/demo-app"}, c)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := OffersFor(f)
	if len(want) == 0 || !slices.Equal(offerValues(stored), offerValues(want)) {
		t.Fatalf("stored offers %v, want the live facts' %v", offerValues(stored), offerValues(want))
	}
	for _, o := range stored {
		if o.Effect == "" {
			t.Errorf("offer %q stores no effect line", o.Value)
		}
	}

	choices := askOffers(asks[0].Detail)
	if len(choices) == 0 {
		t.Fatalf("the ask's own choices are gone from its offers: %v", asks[0].Detail["offers"])
	}
	if err := g.s.SetCaseOffers(ctx, c.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := storedOffers(t, g.asks(t)[0].Detail); len(got) != 0 {
		t.Fatalf("offers after clearing = %v, want none", got)
	}
	_, err = g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-nobody"}}})
	if _, refused := err.(ErrActRefused); !refused {
		t.Fatalf("a press the table does not offer = %v, want a refusal", err)
	}
	after := g.asks(t)[0].Detail
	if got := storedOffers(t, after); !slices.Equal(offerValues(got), offerValues(want)) {
		t.Fatalf("offers after the refusal = %v, want %v stored anew", offerValues(got), offerValues(want))
	}
	if got := askOffers(after); !slices.Equal(got, choices) {
		t.Fatalf("the ask's own choices after the refusal = %v, want %v kept", got, choices)
	}
}

// countedConnector counts the statements the store sends.
type countedConnector struct {
	driver.Connector
	n *atomic.Int64
}

func (c countedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countedConn{conn, c.n}, nil
}

type countedConn struct {
	driver.Conn
	n *atomic.Int64
}

func (c *countedConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}

func (c *countedConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

func (c *countedConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	c.n.Add(1)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}

func (c *countedConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	c.n.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
}

// A board read of the desk sends the same statements for 2 cards as for 10:
// the stored offers ride on the asks it already reads, and the desk holds no
// runner to read a card's facts with.
func TestTheCaseDeskReadsAConstantNumberOfStatements(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "desk_constant")
	conn, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(schematest.DSN(t, "desk_constant"))
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	counted := sql.OpenDB(countedConnector{conn, &n})
	t.Cleanup(func() { counted.Close() })
	read := func(cards int) int64 {
		for i := 0; i < cards; i++ {
			id := "t-desk-" + string(rune('a'+i))
			if _, ok, _ := s.Task(ctx, id); !ok {
				if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "sf/r", IssueNumber: int64(2000 + i), Track: "direct", BudgetEUR: 5}); err != nil {
					t.Fatal(err)
				}
				for _, ask := range []string{"first?", "second?"} {
					if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: id, IssueNumber: int64(2000 + i), Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
						Detail: map[string]any{"question": ask, "ask": ask, "source": "agent", "reason": AgentAsks + ask, "offers": []AskOffer{{Value: ActClose, Label: "Close", Effect: "End the card."}}}}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if _, err := s.OpenCasesIn(ctx, "sf/r"); err != nil { // the heal opens each card's cases
			t.Fatal(err)
		}
		live := s.db
		s.db = counted
		defer func() { s.db = live }()
		n.Store(0)
		d, err := s.CaseDesk(ctx, "sf/r")
		if err != nil || len(d.Cases) < cards {
			t.Fatalf("desk of %d cards = %+v, %v", cards, d, err)
		}
		return n.Load()
	}
	two, ten := read(2), read(10)
	if two != ten || two == 0 {
		t.Fatalf("the desk sent %d statements for 2 cards and %d for 10, want the same count", two, ten)
	}
}
