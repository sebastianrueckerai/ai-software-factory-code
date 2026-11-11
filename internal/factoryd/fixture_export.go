package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"golang.org/x/text/unicode/norm"
)

// FixtureExporter reads one card's fixture. It reads and writes nothing.
// Every text goes through Scrub and sanitize.Outbound with no link host
// allowed, then the whole fixture through the mask of fixture_mask.go.
// Hosts are the host names the daemon is configured with.
type FixtureExporter struct {
	Store   Store
	Objects ObjectStore
	Policy  policy.Tree
	Scrub   *redact.Scrubber
	Hosts   []string
	// Repositories are repositories the daemon serves besides the policy's
	// projects (the config repository), masked like theirs.
	Repositories []string
	// Mask are logins, names and mail domains to mask besides those the
	// card's records name; a term with a dot is masked as a host too. Keep
	// are words after an @ that name no person, such as code annotations.
	Mask, Keep []string
	Now        func() time.Time
}

// maxFixtureSummary bounds a session's stop summary.
const maxFixtureSummary = 2 << 10

// fixtureLine is the part of a timeline line the export reads.
type fixtureLine struct {
	Step       string                  `json:"step"`
	Time       time.Time               `json:"time"`
	Session    string                  `json:"session"`
	Model      string                  `json:"model"`
	Attempt    int                     `json:"attempt"`
	Round      int                     `json:"round"`
	Outcome    string                  `json:"outcome"`
	Summary    string                  `json:"summary"`
	Commit     string                  `json:"commit"`
	State      string                  `json:"state"`
	Verdict    string                  `json:"verdict"`
	Note       string                  `json:"note"`
	Moment     string                  `json:"moment"`
	Findings   json.RawMessage         `json:"findings"`
	Decision   string                  `json:"decision"`
	ReasonKind string                  `json:"reason_kind"`
	Reason     string                  `json:"reason"`
	Status     string                  `json:"status"`
	BackoffS   float64                 `json:"backoff_s"`
	StartFail  string                  `json:"start_failed"`
	Verdicts   map[string]CheckVerdict `json:"verdicts"`
	Record     *CheckInRecord          `json:"record"`
}

// fixtureRun is one export: the card, and the logins its records name.
type fixtureRun struct {
	*FixtureExporter
	t      Task
	logins map[string]bool
}

// Export is the fixture of the card of repository's issue; ErrNotFound when
// no card has the issue.
func (e *FixtureExporter) Export(ctx context.Context, repository string, issue int64) (Fixture, error) {
	t, ok, err := e.Store.TaskByIssue(ctx, repository, issue)
	if err != nil {
		return Fixture{}, err
	}
	if !ok {
		return Fixture{}, fmt.Errorf("%s#%d: no card has this issue: %w", repository, issue, ErrNotFound)
	}
	events, err := e.Store.TaskEvents(ctx, t.ID, cardDigestEvents)
	if err != nil {
		return Fixture{}, err
	}
	slices.Reverse(events)
	tl, err := e.Objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
	if err != nil {
		return Fixture{}, fmt.Errorf("read the timeline: %w", err)
	}
	r := &fixtureRun{FixtureExporter: e, t: t, logins: map[string]bool{}}
	r.login(t.Author)
	for _, a := range e.Policy.ApproversOf(t.Repository) {
		r.login(a)
	}
	for _, p := range e.Policy.Projects {
		for _, a := range e.Policy.ApproversOf(p.Repository) {
			r.login(a)
		}
	}
	for _, ev := range events {
		r.login(ev.Actor)
	}
	f := Fixture{Sessions: []FixtureSession{}, Gates: []FixtureGate{}, Checks: []FixtureChecks{}, Asks: []FixtureAsk{}, Answers: []FixtureAnswer{},
		Script: FixtureScript{Rung1: []FixtureRung1Answer{}, Rung2: []FixtureRung2Answer{}}}
	if f.Card, err = r.card(ctx); err != nil {
		return Fixture{}, err
	}
	cases, err := r.cases(ctx)
	if err != nil {
		return Fixture{}, err
	}
	f.Cases = cases.out
	if err := r.fromTimeline(ctx, tl, events, &f); err != nil {
		return Fixture{}, err
	}
	f.Asks = r.asks(events, cases.byID)
	f.Answers = r.answers(events, cases.byID)
	return r.mask(f)
}

// login notes a login the fixture's texts must not carry; factoryd's own
// and the factory's bot accounts are no login of a human.
func (r *fixtureRun) login(name string) {
	if name != "" && !strings.HasPrefix(name, "factory") {
		r.logins[name] = true
	}
}

// text is s as it may leave in a fixture, read in its NFKC form, so a
// full-width credential is the credential the scrubber knows.
func (r *fixtureRun) text(s string) string {
	return sanitize.Outbound(r.Scrub.String(norm.NFKC.String(s)), nil)
}

// written stands for text its author wrote, a person, a model or factoryd:
// the replay stubs every model role and reads none of it.
func written(author, s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("[written by %s, %d bytes]", author, len(s))
}

func typed(s string) string { return written("a person", s) }

// briefAuthor is who wrote a card's objective, by how the card arrived: a
// person filed it, a model cut or filed it, else factoryd did. A successor
// copies the brief of the card it replaces, a person's.
func briefAuthor(source string) string {
	switch source {
	case "", "webhook", "api", "successor":
		return "a person"
	case SourcePlan, SourceChat, "delegate":
		return "a model"
	}
	return "factoryd"
}

// by is who wrote text when role says so: a human's typing is a placeholder.
func (r *fixtureRun) by(role, s string) string {
	if role == RoleHuman {
		return typed(s)
	}
	return r.text(s)
}

// value is v with every string in it passed through text.
func (r *fixtureRun) value(v any) any {
	switch v := v.(type) {
	case string:
		return r.text(v)
	case map[string]any:
		return r.args(v, "")
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = r.value(x)
		}
		return out
	}
	return v
}

// args is m with its strings passed through text; the answer a human typed
// is a placeholder.
func (r *fixtureRun) args(m map[string]any, role string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok && k == "answer" {
			out[r.text(k)] = r.by(role, s)
			continue
		}
		out[r.text(k)] = r.value(v)
	}
	return out
}

// fixtureRole is the role of an actor: by its rung when it has one (0 and 1
// factoryd, 2 the project agent, 3 a human), else by its login and the way
// it came. A negative rung has none.
func fixtureRole(rung int, actor, via string) string {
	switch {
	case rung == 0 || rung == 1:
		return RoleFactoryd
	case rung == 2:
		return RoleProjectAgent
	case rung == 3:
		return RoleHuman
	case actor == FactoryDecider:
		return RoleFactoryd
	case actor == ChatUser || via == ViaChat:
		return RoleProjectAgent
	}
	return RoleHuman
}

func (r *fixtureRun) card(ctx context.Context) (FixtureCard, error) {
	t := r.t
	_, project, _ := r.Policy.ProjectOf(t.Repository)
	c := FixtureCard{Issue: t.IssueNumber, TaskID: t.ID, Repository: t.Repository, Kind: t.Kind, Track: t.Track, Source: t.Source,
		Size: t.Size, Rung: t.Rung, BudgetEUR: t.BudgetEUR, CeilingEUR: t.CeilingEUR, Objective: written(briefAuthor(t.Source), t.Objective),
		DependsOn: []FixtureDep{}, Autonomy: cmp.Or(project.Autonomy, policy.AutonomyManual), MergeRule: cmp.Or(project.MergeRule, policy.MergeHuman)}
	if t.Source == SourcePlan && t.Parent != "" {
		plan, ok, err := r.Store.Task(ctx, t.Parent)
		if err != nil {
			return c, err
		}
		if ok {
			c.Plan = &FixturePlan{Issue: plan.IssueNumber, Name: r.text(plan.PlanName)}
		}
	}
	deps, err := r.Store.Dependencies(ctx, t.ID)
	if err != nil {
		return c, err
	}
	for _, d := range deps {
		dep := FixtureDep{Issue: d.Issue, Status: d.Status}
		if Final(d.Status) {
			events, err := r.Store.TaskEvents(ctx, d.TaskID, cardDigestEvents)
			if err != nil {
				return c, err
			}
			if i := slices.IndexFunc(events, func(ev Event) bool { return ev.Kind == EventTaskEnded }); i >= 0 {
				reason, _ := events[i].Detail["reason"].(string)
				dep.Why = r.text(reason)
			}
		}
		c.DependsOn = append(c.DependsOn, dep)
	}
	return c, nil
}

type exportedCases struct {
	out  []FixtureCase
	byID map[int64]Case
}

func (r *fixtureRun) cases(ctx context.Context) (exportedCases, error) {
	rows, err := r.Store.CasesOf(ctx, r.t.ID)
	if err != nil {
		return exportedCases{}, err
	}
	res := exportedCases{out: []FixtureCase{}, byID: map[int64]Case{}}
	for _, c := range rows {
		res.byID[c.ID] = c
		steps, err := r.Store.CaseSteps(ctx, c.ID)
		if err != nil {
			return exportedCases{}, err
		}
		fc := FixtureCase{Kind: string(c.Kind), StateKey: r.text(c.StateKey), Problem: r.text(c.Problem), Facts: r.args(c.Facts, ""),
			Status: c.Status, Steps: []FixtureCaseStep{}}
		if res := c.Resolution; res != nil {
			role := fixtureRole(res.Rung, res.By, "")
			if res.Rung >= 2 {
				r.login(res.By)
			}
			fc.Resolution = &FixtureResolution{By: role, Rung: res.Rung, Action: res.Action, Args: r.args(res.Args, role), Reason: r.by(role, res.Reason)}
		}
		for _, s := range steps {
			role := fixtureRole(s.Rung, s.Actor, "")
			if s.Rung >= 2 {
				r.login(s.Actor)
			}
			fc.Steps = append(fc.Steps, FixtureCaseStep{Rung: s.Rung, Actor: role, Found: r.text(s.Found),
				Decision: r.text(s.Decision), Grounds: r.by(role, s.Grounds), Outcome: s.Outcome})
		}
		res.out = append(res.out, fc)
	}
	return res, nil
}

func detailText(ev Event, keys ...string) string {
	for _, k := range keys {
		if s, _ := ev.Detail[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// fixtureAskKind is the kind of the case ask opened: its case_id's case,
// else the case_kind it carries (a monitor's ask), else the kind the ladder
// gives it (caseFromAsk, which the writers' own rows open cases by), none
// when it names no ask.
func fixtureAskKind(t Task, ask Event, cases map[int64]Case) string {
	if id, _ := toInt(ask.Detail["case_id"]); id != 0 {
		if c, ok := cases[int64(id)]; ok {
			return string(c.Kind)
		}
	}
	if kind := detailText(ask, "case_kind"); kind != "" {
		return kind
	}
	if c, err := caseFromAsk(t, ask, ""); err == nil {
		return string(c.Kind)
	}
	return ""
}

// asks are the card's question_asked rows in order.
func (r *fixtureRun) asks(events []Event, cases map[int64]Case) []FixtureAsk {
	out := []FixtureAsk{}
	for _, ev := range events {
		if ev.Kind != EventQuestionAsked {
			continue
		}
		out = append(out, FixtureAsk{AtS: int64(ev.At.Sub(r.t.CreatedAt) / time.Second), CaseKind: fixtureAskKind(r.t, ev, cases), Source: r.text(detailText(ev, "source")),
			Reason: r.text(detailText(ev, "reason")), Ask: r.text(detailText(ev, "ask", "question")), Why: r.text(detailText(ev, "why")),
			Recommendation: r.text(detailText(ev, "recommendation"))})
	}
	return out
}

// approvalCaseKind is the kind of the approval request a decision settles
// (settle.go: a merge or plan decision settles an approval, never a
// question): the kind of its family, a merge's approval or denial the merge
// approval, a plan's changes the plan confirmation, whatever request is open
// beside it. A decision of another kind (a resource request's) settles no
// case: none, like a close.
func approvalCaseKind(ev Event) string {
	if kind := detailText(ev, "kind"); kind != "" && kind != ApprovalMerge && kind != ApprovalPlan {
		return ""
	}
	if ev.Kind == EventPlanChangesRequested {
		return string(CasePlanConfirmation)
	}
	return string(CaseMergeApproval)
}

// answers are the card's answers in order: the events of answerKinds. A
// question's answer names the case it settled, else the kind of the newest
// ask before it; a merge or plan decision the kind of its approval request;
// a board close none. The case's resolution args and the money its pool
// draws are added.
func (r *fixtureRun) answers(events []Event, cases map[int64]Case) []FixtureAnswer {
	out := []FixtureAnswer{}
	for i, ev := range events {
		if !slices.Contains(answerKinds, ev.Kind) {
			continue
		}
		role := fixtureRole(-1, ev.Actor, ev.Via)
		id, _ := toInt(ev.Detail["case_id"])
		a := FixtureAnswer{AtS: int64(ev.At.Sub(r.t.CreatedAt) / time.Second), By: role, Action: cmp.Or(detailText(ev, "action"), ev.Kind),
			Note: r.by(role, detailText(ev, "answer", "note", "reason")), Args: map[string]any{}}
		c, named := cases[int64(id)]
		if named && c.Resolution != nil {
			a.Args = r.args(c.Resolution.Args, role)
		}
		switch ev.Kind {
		case EventMergeApproved, EventMergeDenied, EventPlanChangesRequested:
			a.CaseKind = approvalCaseKind(ev)
		case EventCardClosed:
			// a board close answers no case
		default:
			if named {
				a.CaseKind = string(c.Kind)
			}
			for j := i - 1; j >= 0 && a.CaseKind == ""; j-- {
				if events[j].Kind == EventQuestionAsked {
					a.CaseKind = fixtureAskKind(r.t, events[j], cases)
					break
				}
			}
		}
		for _, f := range events {
			if n, _ := toInt(f.Detail["case_id"]); f.Kind == EventCardFunded && n == id && id != 0 {
				eur, _ := f.Detail["amount_eur"].(float64)
				a.FundEUR += eur
			}
		}
		out = append(out, a)
	}
	return out
}

// fromTimeline fills f's sessions, gates and checks from the card's
// timeline tl and its events.
func (r *fixtureRun) fromTimeline(ctx context.Context, tl []byte, events []Event, f *Fixture) error {
	var lines []fixtureLine
	for raw := range bytes.SplitSeq(tl, []byte("\n")) {
		var l fixtureLine
		if json.Unmarshal(raw, &l) == nil && l.Step != "" {
			lines = append(lines, l)
		}
	}
	_, sessions := lineSteps(r.Scrub, tl)
	ended := map[string]fixtureLine{}
	for _, l := range lines {
		if l.Step == "session_end" {
			ended[l.Session] = l
		}
	}
	checkins := map[string]*FixtureCheckIn{}
	for _, l := range lines {
		if l.Step == "checkin" {
			if id := checkinOwner(l, sessions); id != "" {
				checkins[id] = r.checkin(l)
			}
		}
	}
	refusals := map[string][]FixtureRefusal{}
	for _, l := range lines {
		if l.Step == StepProviderRefused {
			if id := sessionAt(l.Time, sessions); id != "" {
				refusals[id] = append(refusals[id], FixtureRefusal{Status: l.Status, Reason: r.text(l.Reason), BackoffS: l.BackoffS})
			}
		}
	}
	for i, s := range sessions {
		end := s.end
		switch {
		case !end.IsZero():
		case i+1 < len(sessions):
			end = sessions[i+1].start
		default:
			end = r.Now()
		}
		cost, calls, err := r.Store.SpendAndCallsBetween(ctx, r.t.ID, s.start, end)
		if err != nil {
			return err
		}
		fs := FixtureSession{Attempt: s.attempt, Model: aliasOf(s.model), Outcome: s.outcome, Commit: s.commit,
			Pushed: s.outcome == string(OutcomePushed), CostEUR: cost, Calls: calls,
			Summary: textutil.Truncate(r.text(ended[s.id].Summary), maxFixtureSummary), CheckIn: checkins[s.id], Refusals: refusals[s.id]}
		if fs.Refusals == nil {
			fs.Refusals = []FixtureRefusal{}
		}
		if !s.end.IsZero() {
			fs.AgentTimeS = int64(s.end.Sub(s.start) / time.Second)
		}
		f.Sessions = append(f.Sessions, fs)
	}
	approval, haveApproval, err := r.Store.NewestApproval(ctx, r.t.ID, ApprovalMerge)
	if err != nil {
		return err
	}
	var spec MergeSpec
	if haveApproval {
		_ = json.Unmarshal(approval.Spec, &spec)
	}
	for _, l := range lines {
		if l.Step != "checks" {
			continue
		}
		var found []string
		_ = json.Unmarshal(l.Findings, &found)
		c := FixtureChecks{Head: l.Commit, State: l.State, Findings: []string{}, FailedTests: []FixtureFailedTest{}}
		for _, s := range found {
			c.Findings = append(c.Findings, r.text(s))
		}
		if spec.HeadSHA == l.Commit {
			for _, ft := range spec.FailedTests {
				c.FailedTests = append(c.FailedTests, FixtureFailedTest{Job: r.text(ft.Job), Test: r.text(ft.Test), Package: r.text(ft.Package), Kind: r.text(ft.Kind)})
			}
		}
		f.Checks = append(f.Checks, c)
	}
	f.Gates = r.gates(events, lines)
	return nil
}

// gates are the review, audit and criteria gate results in order. The fact
// rows of the review and the audit (written also when the gate never ran)
// give the verdict or the reason it did not run; the timeline line of the
// same head gives the findings. A line with no row, from before the rows,
// stands alone.
func (r *fixtureRun) gates(events []Event, lines []fixtureLine) []FixtureGate {
	type gateAt struct {
		at time.Time
		g  FixtureGate
	}
	var rows []gateAt
	used := make([]bool, len(lines))
	take := func(step, commit string, round int) *fixtureLine {
		for i := range lines {
			l := &lines[i]
			if !used[i] && l.Step == step && l.Commit == commit && (step != "model_review" || l.Round == 0 || round == 0 || l.Round == round) {
				used[i] = true
				return l
			}
		}
		return nil
	}
	for _, ev := range events {
		var step, gate, ranKey string
		switch ev.Kind {
		case EventModelReview:
			step, gate, ranKey = "model_review", "review", "reviewed"
		case EventGateAudit:
			step, gate, ranKey = "audit", "audit", "ran"
		default:
			continue
		}
		commit := detailText(ev, "commit")
		round, _ := toInt(ev.Detail["round"])
		line := take(step, commit, round)
		g := FixtureGate{Head: commit, Gate: gate, Findings: []FixtureFinding{}}
		if ran, _ := ev.Detail[ranKey].(bool); ran {
			g.Verdict = detailText(ev, "verdict")
			if line != nil {
				g.Findings = r.findings(gateTexts(*line))
			}
		} else {
			g.NotRun = r.text(detailText(ev, "reason", "note"))
		}
		rows = append(rows, gateAt{ev.At, g})
	}
	for i, l := range lines {
		if used[i] {
			continue
		}
		g := FixtureGate{Head: l.Commit, Findings: []FixtureFinding{}}
		texts := gateTexts(l)
		switch l.Step {
		case "model_review":
			g.Gate, g.Verdict = "review", l.Verdict
			if l.Verdict == "" {
				g.NotRun = r.text(l.Note)
			}
		case "audit":
			g.Gate, g.Verdict = "audit", l.Verdict
			if len(texts) == 1 && texts[0] == auditNoVerdict {
				g.Verdict, g.NotRun, texts = "", r.text(auditNoVerdict), nil
			}
		case "gate_criteria":
			g.Gate, g.Verdict, g.NotRun = "criteria", l.Verdict, r.text(l.StartFail)
		default:
			continue
		}
		g.Findings = r.findings(texts)
		rows = append(rows, gateAt{l.Time, g})
	}
	slices.SortStableFunc(rows, func(a, b gateAt) int { return a.at.Compare(b.at) })
	out := make([]FixtureGate, len(rows))
	for i, x := range rows {
		out[i] = x.g
	}
	return out
}

// gateTexts are the finding texts of a review, audit or criteria line.
func gateTexts(l fixtureLine) []string {
	if l.Step == "audit" {
		var found []AuditFinding
		_ = json.Unmarshal(l.Findings, &found)
		texts := make([]string, len(found))
		for i, a := range found {
			texts[i] = a.Item
		}
		return texts
	}
	var texts []string
	_ = json.Unmarshal(l.Findings, &texts)
	return texts
}

// findings are texts with their ids, the id of the text as recorded.
func (r *fixtureRun) findings(texts []string) []FixtureFinding {
	out := make([]FixtureFinding, 0, len(texts))
	for _, s := range texts {
		out = append(out, FixtureFinding{Text: r.text(s), ID: findingID(s)})
	}
	return out
}

func (r *fixtureRun) checkin(l fixtureLine) *FixtureCheckIn {
	c := &FixtureCheckIn{Moment: l.Moment, Decision: l.Decision, ReasonKind: l.ReasonKind, Reason: r.text(l.Reason), Verdicts: map[string]string{}}
	if rec := l.Record; rec != nil {
		c.Moment = cmp.Or(rec.Moment, c.Moment)
		c.Decision, c.ReasonKind, c.Reason = cmp.Or(rec.Decision, c.Decision), cmp.Or(rec.ReasonKind, c.ReasonKind), cmp.Or(r.text(rec.Reason), c.Reason)
	}
	for text, v := range l.Verdicts {
		c.Verdicts[findingID(text)] = v.State
	}
	return c
}

// checkinOwner is the id of the session a check-in line judged: the parent
// session its record names, the session the line names, else the newest
// session started before it.
func checkinOwner(l fixtureLine, sessions []*digestSession) string {
	if l.Record != nil && l.Record.ParentSession != "" {
		return l.Record.ParentSession
	}
	if l.Session != "" {
		return l.Session
	}
	return sessionAt(l.Time, sessions)
}

// sessionAt is the id of the newest session started at or before at.
func sessionAt(at time.Time, sessions []*digestSession) string {
	owner := ""
	for _, s := range sessions {
		if !s.start.After(at) {
			owner = s.id
		}
	}
	return owner
}
