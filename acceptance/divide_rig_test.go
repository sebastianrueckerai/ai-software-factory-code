package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"

	_ "github.com/jackc/pgx/v5/stdlib" // the store's driver, for the seeds no writer exports
)

// The divide-and-conquer proofs (roadmap 336), the integration card's:
// each panel test drives the authenticated console twice — once over a
// scripted Figures port, once over a real PostgreSQL store wired as the
// same port and seeded through the production fact payloads.

// accFigures is the scripted Figures port: it answers the report it was
// handed and records the windows asked of it.
type accFigures struct {
	report factoryd.MetricsReport
	asked  []string
}

func (f *accFigures) ProjectMetrics(_ context.Context, repository string, from, to time.Time) (factoryd.MetricsReport, error) {
	f.asked = append(f.asked, repository+" "+from.Format(time.RFC3339)+" "+to.Format(time.RFC3339))
	return f.report, nil
}

// divideWindow is [2026-09-28, 2026-10-12): the page's dates are inclusive,
// so accQuery's to=2026-10-11 reads the day whole and the report ends at
// the next midnight. The equal window before it is [09-14, 09-28); the
// seven weekly buckets run from 08-24, the window being the last two.
func divideWindow() (from, end time.Time) {
	return time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
}

// accDay is the fixture window's n-th midnight: day 0 opens it, day 14 is
// its exclusive end, day -14 opens the window before.
func accDay(n int) time.Time {
	from, _ := divideWindow()
	return from.AddDate(0, 0, n)
}

// accQuery is the window's own query on the metrics path.
const accQuery = "?from=2026-09-28&to=2026-10-11"

// divideRig is an authenticated member on a console whose Figures port is
// figures: scripted or the real store, the page answers the same.
type divideRig struct {
	t       *testing.T
	a       *AcceptanceRig
	figures console.Metrics
	ck      *http.Cookie
}

// newDivideRig logs a member into the console over figures.
func newDivideRig(t *testing.T, figures console.Metrics) *divideRig {
	t.Helper()
	a, _ := NewMetricsRig(t, figures)
	ck, _ := a.Login(t)
	return &divideRig{t: t, a: a, figures: figures, ck: ck}
}

// page answers the metrics page's body over the fixture window.
func (r *divideRig) page() string {
	r.t.Helper()
	return r.pageQuery(accQuery)
}

// pageQuery answers the metrics page's body over query's own window.
func (r *divideRig) pageQuery(query string) string {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/projects/"+accProjectName+"/metrics"+query, nil)
	req.AddCookie(r.ck)
	rec := httptest.NewRecorder()
	r.a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		r.t.Fatalf("GET metrics = %d: %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// get answers the metrics path with query as the rig's member, the
// response unchecked.
func (r *divideRig) get(query string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/projects/"+accProjectName+"/metrics"+query, nil)
	req.AddCookie(r.ck)
	rec := httptest.NewRecorder()
	r.a.Handler().ServeHTTP(rec, req)
	return rec
}

// reportAt answers the metrics JSON over query's window as the report
// the seam serves.
func (r *divideRig) reportAt(query string) factoryd.MetricsReport {
	r.t.Helper()
	sep := "?"
	if query != "" {
		sep = "&"
	}
	rec := r.get(query + sep + "format=json")
	if rec.Code != http.StatusOK {
		r.t.Fatalf("GET metrics json = %d: %s", rec.Code, rec.Body)
	}
	var report factoryd.MetricsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		r.t.Fatalf("decode: %v", err)
	}
	return report
}

// report answers the fixture window's divide figures.
func (r *divideRig) report() factoryd.DivideConquer {
	r.t.Helper()
	return r.reportAt(accQuery).DivideConquer
}

// accSpan is the metrics query of the inclusive dates first to last.
func accSpan(first, last time.Time) string {
	return "?from=" + first.Format("2006-01-02") + "&to=" + last.Format("2006-01-02")
}

// accFig is a measured figure with its previous window and seven weeks.
func accFig(value, prev float64, weeks [7]float64) factoryd.Figure {
	return factoryd.Figure{Value: value, Prev: prev, Has: true, PrevHas: true, Weeks: weeks}
}

// same fails unless got equals want, field for field.
func same(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %+v, want %+v", what, got, want)
	}
}

// panel is the page's row of the named panel, its name through its
// closing tag, so a literal two panels print stays the one panel's.
func panel(t *testing.T, body, name string) string {
	t.Helper()
	i := strings.Index(body, `">`+name+`</span>`)
	if i < 0 {
		t.Fatalf("page lacks the panel %q", name)
	}
	row := body[i:]
	return row[:strings.Index(row, "</li>")]
}

// holds fails unless body carries every literal and none of the masked.
func holds(t *testing.T, body string, want, masked []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
	for _, m := range masked {
		if strings.Contains(body, m) {
			t.Errorf("page renders %q", m)
		}
	}
}

// divideStore answers a real PostgreSQL, its read tables emptied so
// every test's fixtures stand alone.
func divideStore(t *testing.T) (*factoryd.PGStore, *sql.DB) {
	t.Helper()
	dsn := schematest.DSN(t, "acceptance_divide")
	s, err := factoryd.OpenStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range []string{"events", "alerts", "ledger", "audit", "project_rolls", "ci_runs",
		"rolls", "chat_messages", "tasks", "releases", "plan_revisions"} {
		if _, err := db.ExecContext(context.Background(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	return s, db
}

// accRecord records e at at, answering its id (0 when an identity holds
// the row).
func accRecord(t *testing.T, s *factoryd.PGStore, e factoryd.Event, at time.Time) int64 {
	t.Helper()
	e.At = at
	id, err := s.Record(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// accCard inserts an open card of repository on its own issue, sized
// size ("" unsized) at head.
func accCard(t *testing.T, s *factoryd.PGStore, id, repository, size, head string, issue int64) {
	t.Helper()
	task := factoryd.Task{ID: id, Status: "in-review", Objective: "the divide panels' fixture", Repository: repository,
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: issue, SHA: head, Size: size,
		RequiredCapabilities: []string{"coder"}}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

// accPlan inserts a plan's own task, done; accPlanChild one of its
// filed cards.
func accPlan(t *testing.T, s *factoryd.PGStore, id, repository string, issue int64) {
	t.Helper()
	task := factoryd.Task{ID: id, Status: "done", Objective: "the divide panels' fixture", Repository: repository,
		Kind: "agent", Source: "api", Track: "plan", BudgetEUR: 5, IssueNumber: issue}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

func accPlanChild(t *testing.T, s *factoryd.PGStore, id, plan, repository string, issue int64) {
	t.Helper()
	task := factoryd.Task{ID: id, Status: "done", Objective: "the divide panels' fixture", Repository: repository,
		Kind: "agent", Source: factoryd.SourcePlan, Parent: plan, PlanKey: id + "-key", Track: "direct", BudgetEUR: 5, IssueNumber: issue}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

// accReading records one merge-gate reading of card's head at at.
func accReading(t *testing.T, s *factoryd.PGStore, card, repository, head, size string, bound, ins, del int, issue int64, at time.Time) int64 {
	t.Helper()
	return accRecord(t, s, factoryd.GateHeadFact{Task: card, Head: head, Gate: factoryd.GateMerge, Decision: "auto",
		Size: size, Bound: bound, PolicyVersion: 7, Files: 9, Insertions: ins, Deletions: del}.
		Event(repository, issue), at)
}

// accMerged records the card's pull_request_merged row at at.
func accMerged(t *testing.T, s *factoryd.PGStore, task, repository string, at time.Time) {
	t.Helper()
	accRecord(t, s, factoryd.Event{Repository: repository, TaskID: task, Kind: factoryd.EventPullRequestMerged,
		Actor: factoryd.MergeUser, Via: factoryd.ViaFactoryd, Detail: map[string]any{"pr": 9, "commit": "m"}}, at)
}

// accStarted records one named session's start on task at at.
func accStarted(t *testing.T, s *factoryd.PGStore, session, task, repository string, attempt int, issue int64, at time.Time) int64 {
	t.Helper()
	return accRecord(t, s, factoryd.SessionStartFact{Task: task, Session: session, Model: "big", Attempt: attempt}.
		Event(task, repository, issue, at), at)
}

// accEnded records one session's terminal end on task at at.
func accEnded(t *testing.T, s *factoryd.PGStore, session, outcome, task, repository string, attempt int, issue int64, at time.Time) int64 {
	t.Helper()
	return accRecord(t, s, factoryd.SessionEndFact{Session: session, Attempt: attempt, Outcome: outcome,
		Started: at.Add(-time.Hour)}.Event(task, repository, issue, at), at)
}

// accClosed records the plan's plan_closed row at at, as the verdict
// pass does.
func accClosed(t *testing.T, s *factoryd.PGStore, plan, repository string, at time.Time) {
	t.Helper()
	accRecord(t, s, factoryd.Event{Repository: repository, TaskID: plan, Kind: factoryd.EventPlanClosed,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"criteria": 3}}, at)
}

// accPanelsReport is the scripted report every panel's fake part reads:
// all twelve panels measured, so each proof asserts its own literals off
// one shared shape.
func accPanelsReport() factoryd.MetricsReport {
	move := func(value, prev float64) factoryd.Figure {
		return factoryd.Figure{Value: value, Prev: prev, Has: true, PrevHas: true}
	}
	count := func(value float64) factoryd.Figure { return factoryd.Figure{Value: value, Has: true} }
	return factoryd.MetricsReport{
		Repository: "sf/code",
		From:       time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
		To:         time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
		DivideConquer: factoryd.DivideConquer{
			CoveredFrom:       time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
			GateAboveBound:    move(1, 0),
			GateBoundCoverage: factoryd.GateBoundCoverage{Readings: 3, Measured: 2, UnknownSize: 1},
			SessionsPerCard: []factoryd.SizeFigure{
				{Size: "small", Figure: move(2.0, 1.0)},
				{Size: "medium", Figure: move(2.0, 2.5)},
			},
			SessionsCoverage: factoryd.SessionsCoverage{Cards: 5, UnknownSize: 1, MissingStarts: 1},
			SessionEnds:      []factoryd.NamedCount{{Name: "pushed", Count: 2}, {Name: "wall_clock", Count: 1}},
			EurPerMergedLine: move(0.02, 0.01),
			FixedEurPerCard: []factoryd.SizeIntercept{
				{Size: "small", EUR: 2, Available: true, Cards: 2},
				{Size: "medium", EUR: 3, Available: true, Cards: 2},
			},
			EconomicsCoverage:  factoryd.EconomicsCoverage{Cards: 4},
			Cuts:               move(2, 1),
			CutGroupsClosed:    count(1),
			GroupOpenMillis:    move(21.6e6, 7.2e6),
			GroupOpenHours:     move(6, 2),
			GroupHoursCoverage: factoryd.GroupHoursCoverage{Groups: 2},
			HoldWaitMillis:     move(13.5e6, 0),
			HoldWaitHours:      move(3.75, 0),
			HoldWaitCoverage:   factoryd.HoldWaitCoverage{Ended: 2},
			ReviewsNotReviewed: count(2),
			ReviewEvidence: factoryd.ReviewEvidence{
				Answers: count(3),
				Unknown: count(1),
			},
			PlansByRoundsSpans: []factoryd.NamedFigure{
				{Name: "1", Figure: move(1, 2)},
				{Name: "2", Figure: move(1, 0)},
			},
			CardsFirstRound:  count(4),
			CardsLaterRounds: count(3),
			CardsByRound: []factoryd.NamedFigure{
				{Name: "1", Figure: move(4, 3)},
				{Name: "2", Figure: move(2, 1)},
				{Name: "3", Figure: move(1, 0)},
			},
			ReplansByOriginSpans: []factoryd.NamedFigure{
				{Name: "person", Figure: move(1, 0)},
				{Name: "cut", Figure: move(1, 0)},
				{Name: "round", Figure: move(1, 1)},
				{Name: "unknown", Figure: move(2, 1)},
			},
		},
	}
}
