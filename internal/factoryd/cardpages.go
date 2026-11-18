package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// PromiseRow is one promise of a plan as its rows stand now: the plan's
// criterion with its standing verdict and evidence.
type PromiseRow struct {
	PlanCriterion
	Verdict, Evidence string
}

// Settled says the promise stands met or waived.
func (r PromiseRow) Settled() bool { return r.Verdict == "met" || r.Verdict == "waived" }

// GateLine says the promise asks nothing of a human: a test's or an
// invariant's that the gate met, its evidence read as blind or open, which
// a request shows as one line.
func (r PromiseRow) GateLine() bool {
	state, _ := GateEvidence(r.ID, r.Evidence)
	return r.Verdict == "met" && (r.Kind == CriterionTest || r.Kind == CriterionInvariant) && state != ""
}

// HoldoutRow is an extra promise of a plan's round: its proof, its index
// among the round's from 1, and whether the plan's confirmation waits for
// its answer.
type HoldoutRow struct {
	PlanProof
	Index   int
	Decides bool
}

// Request is a pending approval request with the rows its faces read, as
// they stand now.
//
// A merge's: its spec; Healed when its red head's checks are green now,
// HeadErr the read of them that failed; Promises the plan's on its
// integration card Integration, Unsettled the human promises with no
// verdict, which hold the merge.
//
// A plan's: its spec, the revision's plan, the cards it takes off main
// (TakeOffRefs, each its issue or its key) with the planner's reasons, the
// revision's questions and its extra promises; Promises the plan's.
//
// A criteria request's: its spec, Promises the plan's with their rows,
// Delivered whether every card of the plan is, ClosesItself whether the
// close check closes the plan by itself once every promise is settled
// (closesItself), and Run the newest run of its tests a human asked for.
type Request struct {
	ApprovalRequest
	Merge                    MergeSpec
	PlanSpec                 PlanSpec
	CriteriaSpec             CriteriaSpec
	Healed                   bool
	HeadErr                  error
	Promises                 []PromiseRow
	Integration              string
	Unsettled                []string
	Delivered, ClosesItself  bool
	Run                      *RunFact
	Plan                     Plan
	TakeOffRefs, TakeOffWhys []string
	PlanQuestions            []PlanQuestion
	HoldoutRows              []HoldoutRow
}

// ErrRequestSpec is a request whose spec does not read, which names no
// rows to read.
var ErrRequestSpec = errors.New("the request's spec does not read")

// RequestReads are the store's reads a request's rows come from.
type RequestReads interface {
	PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error)
	PlanCards(ctx context.Context, plan string) ([]Task, error)
	PlanQuestions(ctx context.Context, id string, revision int) ([]PlanQuestion, error)
	PlanProofs(ctx context.Context, id string) ([]PlanProof, error)
	PlanDelivered(ctx context.Context, plan string) (bool, Task, error)
	CriteriaVerdicts(ctx context.Context, taskID string) ([]CriterionVerdict, error)
	TaskEventsOfKind(ctx context.Context, id, kind string, limit int) ([]Event, error)
}

// ChecksRead says whether the checks of commit sha of project are green
// now.
type ChecksRead func(ctx context.Context, project, sha string) (bool, error)

// ReadRequest is pending request a of card t with its rows, read through
// s and checks. A read that fails leaves the rows read before it.
func ReadRequest(ctx context.Context, s RequestReads, a ApprovalRequest, t Task, checks ChecksRead) (Request, error) {
	r := Request{ApprovalRequest: a}
	switch a.Kind {
	case ApprovalMerge:
		if err := json.Unmarshal(a.Spec, &r.Merge); err != nil {
			return r, fmt.Errorf("%w: %v", ErrRequestSpec, err)
		}
		// a red request whose recorded head's checks are green now (#1013)
		// merges again; a read that fails leaves the red face
		if r.Merge.RedChecks && r.Merge.HeadSHA != "" {
			green, err := checks(ctx, a.Project, r.Merge.HeadSHA)
			r.Healed, r.HeadErr = green && err == nil, err
		}
		var err error
		r.Promises, r.Unsettled, r.Integration, err = MergeRows(ctx, s, t)
		return r, err
	case ApprovalPlan:
		return r, r.planRows(ctx, s)
	case ApprovalCriteria, ApprovalCriteriaRerun:
		return r, r.criteriaRows(ctx, s)
	}
	return r, nil
}

// MergeRows are plan card t's plan's promises with their standing rows on
// its integration card, integration, and the human promises with no
// verdict, which hold its merge; none when t is no integration card or
// delta card of a plan with criteria.
func MergeRows(ctx context.Context, s interface {
	PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error)
	PlanCards(ctx context.Context, plan string) ([]Task, error)
	CriteriaVerdicts(ctx context.Context, taskID string) ([]CriterionVerdict, error)
}, t Task) (rows []PromiseRow, unsettled []string, integration string, err error) {
	if t.Source != SourcePlan || t.Parent == "" {
		return nil, nil, "", nil
	}
	rev, ok, err := s.PlanRevision(ctx, t.Parent, 0)
	if err != nil || !ok {
		return nil, nil, "", err
	}
	p, err := revisionPlan(rev)
	if err != nil || len(p.Criteria) == 0 || !CriteriaCard(p, t.PlanKey) {
		return nil, nil, "", err
	}
	card, err := IntegrationCard(ctx, s, t.Parent, p)
	if err != nil {
		return nil, nil, "", err
	}
	verdicts, err := s.CriteriaVerdicts(ctx, card.ID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("verdicts of %s: %w", card.ID, err)
	}
	for _, pc := range p.Criteria {
		v, _ := StandingRow(verdicts, pc)
		rows = append(rows, PromiseRow{PlanCriterion: pc, Verdict: v.Verdict, Evidence: v.Evidence})
		if v.Verdict == "" && pc.Kind == CriterionHuman {
			unsettled = append(unsettled, pc.ID)
		}
	}
	return rows, unsettled, card.ID, nil
}

// planRows reads a plan request's revision, the cards it takes off main,
// its questions and its round's extra promises.
func (r *Request) planRows(ctx context.Context, s RequestReads) error {
	if err := json.Unmarshal(r.Spec, &r.PlanSpec); err != nil {
		return fmt.Errorf("%w: %v", ErrRequestSpec, err)
	}
	rev, ok, err := s.PlanRevision(ctx, r.TaskID, r.PlanSpec.Revision)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("revision %d of %s is not recorded", r.PlanSpec.Revision, r.TaskID)
	}
	if r.Plan, err = revisionPlan(rev); err != nil {
		return err
	}
	for _, c := range r.Plan.Criteria {
		r.Promises = append(r.Promises, PromiseRow{PlanCriterion: c})
	}
	if len(r.Plan.TakeOffMain) > 0 {
		cards, err := s.PlanCards(ctx, r.TaskID)
		if err != nil {
			return fmt.Errorf("the cards of plan %s: %w", r.TaskID, err)
		}
		issue := map[string]int64{}
		for _, t := range cards {
			issue[t.PlanKey] = t.IssueNumber
		}
		for _, key := range r.Plan.TakeOffMain {
			ref := key
			if n := issue[key]; n > 0 {
				ref = fmt.Sprintf("#%d", n)
			}
			r.TakeOffRefs = append(r.TakeOffRefs, ref)
			if why := r.Plan.TakeOffReason(key); why != "" {
				r.TakeOffWhys = append(r.TakeOffWhys, ref+": "+why)
			}
		}
	}
	if r.PlanQuestions, err = s.PlanQuestions(ctx, r.TaskID, r.PlanSpec.Revision); err != nil {
		return fmt.Errorf("questions of revision %d of %s: %w", r.PlanSpec.Revision, r.TaskID, err)
	}
	confirming := slices.ContainsFunc(r.PlanQuestions, func(q PlanQuestion) bool { return q.Kind == QuestionConfirmation && !q.Answered() })
	proofs, err := s.PlanProofs(ctx, r.TaskID)
	if err != nil {
		return fmt.Errorf("hold-outs of %s: %w", r.TaskID, err)
	}
	for _, ho := range proofs {
		if ho.Origin == ProofOriginHoldout && ho.Round == r.PlanSpec.Revision {
			r.HoldoutRows = append(r.HoldoutRows, HoldoutRow{PlanProof: ho, Index: len(r.HoldoutRows) + 1, Decides: confirming && ho.State == ProofProposed})
		}
	}
	return nil
}

// criteriaRows reads a criteria request's plan, its promises with their
// standing rows, whether the plan's cards are delivered, and for a rerun
// request the newest run of its tests a human asked for.
func (r *Request) criteriaRows(ctx context.Context, s RequestReads) error {
	if err := json.Unmarshal(r.Spec, &r.CriteriaSpec); err != nil {
		return fmt.Errorf("%w: %v", ErrRequestSpec, err)
	}
	rev, ok, err := s.PlanRevision(ctx, r.TaskID, r.CriteriaSpec.Revision)
	if err != nil {
		return fmt.Errorf("revision %d of %s: %w", r.CriteriaSpec.Revision, r.TaskID, err)
	}
	if !ok {
		return fmt.Errorf("revision %d of %s is not recorded", r.CriteriaSpec.Revision, r.TaskID)
	}
	if r.Plan, err = revisionPlan(rev); err != nil {
		return err
	}
	rows, err := s.CriteriaVerdicts(ctx, r.CriteriaSpec.Task)
	if err != nil {
		return fmt.Errorf("verdicts of %s: %w", r.CriteriaSpec.Task, err)
	}
	if r.Delivered, _, err = s.PlanDelivered(ctx, r.TaskID); err != nil {
		return fmt.Errorf("the delivered cards of %s: %w", r.TaskID, err)
	}
	for _, c := range r.Plan.Criteria {
		v, _ := StandingRow(rows, c)
		r.Promises = append(r.Promises, PromiseRow{PlanCriterion: c, Verdict: v.Verdict, Evidence: v.Evidence})
	}
	cards, err := s.PlanCards(ctx, r.TaskID)
	if err != nil {
		return fmt.Errorf("the cards of plan %s: %w", r.TaskID, err)
	}
	if integration, ok := integrationCardOf(cards, r.Plan); ok {
		r.ClosesItself = closesItself(r.Plan, integration, rows)
	}
	if r.Kind != ApprovalCriteriaRerun {
		return nil
	}
	runs, err := s.TaskEventsOfKind(ctx, r.TaskID, EventCriteriaGateRun, 50)
	if err != nil {
		return fmt.Errorf("the runs of %s's tests: %w", r.TaskID, err)
	}
	for _, e := range runs {
		if run, asked := RunOf(e); asked && e.Detail["task"] == r.CriteriaSpec.Task {
			r.Run = &run
			break
		}
	}
	return nil
}

// RequestAnswers says request a is the body of case c: a merge request of
// the merge case that names it, a resource request of its case of the
// resource kind (the export's shape, as the store opens no case of its own
// for one), a criteria request of the criteria case that names it, a
// plan's of its plan question or confirmation. The case names the request
// by its id however its facts decode the number.
func RequestAnswers(a ApprovalRequest, c Case) bool {
	id, named := c.Facts["approval"]
	raw, _ := json.Marshal(id)
	mine := string(raw) == fmt.Sprint(a.ID)
	switch c.Kind {
	case CaseMergeApproval:
		return a.Kind == ApprovalMerge && mine
	case CaseKind(ApprovalResource):
		return a.Kind == ApprovalResource && (!named || mine)
	case CasePlanCriteria:
		return (a.Kind == ApprovalCriteria || a.Kind == ApprovalCriteriaRerun) && mine
	case CasePlanQuestion, CasePlanConfirmation:
		return a.Kind == ApprovalPlan
	}
	return false
}

// planFacts are what plan request r names of its plan.
func (r Request) planFacts() PlanFacts {
	p := PlanFacts{OffMain: r.TakeOffRefs, OffMainWhys: r.TakeOffWhys}
	if r.Plan.Spec != nil {
		p.Outcome = r.Plan.Spec.Outcome
	}
	for _, row := range PlanRows(r.Plan) {
		p.Cards = append(p.Cards, row.Title)
	}
	for _, c := range r.Plan.Criteria {
		p.Promises = append(p.Promises, PromiseOf(c))
	}
	for _, h := range r.HoldoutRows {
		if h.Decides {
			p.HoldoutsOpen++
		}
	}
	for _, q := range r.PlanQuestions {
		if q.Kind != QuestionConfirmation {
			p.Asks++
		}
	}
	return p
}

// questionCase is the case plan request r's first open question opens, as
// its ask opens it; false when no question but the confirmation waits.
func (r Request) questionCase() (Case, bool) {
	for i, q := range r.PlanQuestions {
		if q.Answered() || q.Kind == QuestionConfirmation {
			continue
		}
		choices := make([]any, len(q.Choices))
		for j, ch := range q.Choices {
			choices[j] = ch
		}
		return Case{TaskID: r.TaskID, Repository: r.Project, Kind: CasePlanQuestion, Status: CaseAtHuman, Problem: q.Text,
			Facts: map[string]any{"revision": float64(r.PlanSpec.Revision), "index": float64(i + 1), "question": q.Text, "choices": choices,
				"recommended": q.Recommended, "grounds": q.Grounds}}, true
	}
	return Case{}, false
}

// onPromises is criteria case c as request r's rows read now: the promises
// still open, those of them nothing judged yet and the plan's count, so its
// ask counts them and Drop this promise picks among them, whether every
// one is settled, whether the plan's cards are delivered and whether the
// plan then closes by itself. Rows it could not read leave the request's
// own count.
func (r Request) onPromises(c Case) Case {
	if len(r.Promises) == 0 {
		return c
	}
	var open, unchecked []string
	for _, p := range r.Promises {
		if !p.Settled() {
			open = append(open, p.ID)
			if p.Verdict == "" {
				unchecked = append(unchecked, p.ID)
			}
		}
	}
	c = OnOpen(c, open, unchecked, len(r.Promises))
	c.Facts = withFacts(c.Facts, map[string]any{"settled": len(open) == 0, "undelivered": !r.Delivered, "closes_itself": r.ClosesItself})
	return c
}

// parts are the parts of case c's ask that request r's rows hold, each a
// page of its own: the human promises of a merge with no verdict yet, the
// open promises of a criteria request, the extra promises a plan's
// confirmation waits on. Every part is case c with its part's facts.
func (r *Request) parts(c Case) []Case {
	if r == nil {
		return nil
	}
	var out []Case
	switch c.Kind {
	case CaseMergeApproval:
		c = OnOpen(c, r.Unsettled, nil, 0)
		for _, p := range r.Promises {
			if p.Kind == CriterionHuman && slices.Contains(r.Unsettled, p.ID) {
				out = append(out, OnPromise(c, PromiseFact{ID: p.ID, Promise: PromiseOf(p.PlanCriterion), Kind: p.Kind, Verdict: p.Verdict, Evidence: p.PlanCriterion.Evidence}))
			}
		}
	case CasePlanCriteria:
		c = r.onPromises(c)
		if r.Run != nil {
			c = OnRun(c, *r.Run)
		}
		for _, p := range r.Promises {
			if !p.GateLine() && !p.Settled() {
				_, line := GateEvidence(p.ID, p.Evidence)
				out = append(out, OnPromise(c, PromiseFact{ID: p.ID, Promise: PromiseOf(p.PlanCriterion), Kind: p.Kind, Verdict: p.Verdict, Evidence: line}))
			}
		}
	case CasePlanConfirmation:
		for _, h := range r.HoldoutRows {
			if h.Decides {
				out = append(out, OnHoldout(c, HoldoutFact{Idx: h.Idx, Claim: h.Claim}))
			}
		}
	}
	return out
}

// healed is merge case c on the green checks its head reads now.
func healed(c Case) Case {
	c.Facts = withFacts(c.Facts, map[string]any{"checks": "green", "red_checks": false})
	return c
}

// healedLeft is the story's last line of a merge whose checks healed.
const healedLeft = "The checks healed after a re-run."

// withDeps is case c of card t with the cards a human's work card waits
// for and that are not done, as the board read them in the shape Act's
// facts read them, among its facts: its face says what it waits for, and
// offers "I did it" once none is left.
func withDeps(c Case, t BoardTask) Case {
	if t.Kind != "human" || len(t.Dependencies) == 0 {
		return c
	}
	c.Facts = withFacts(c.Facts, map[string]any{"dependencies": t.Dependencies})
	return c
}

// ShowsCases says card t shows its cases at the human, cases: an open
// card shows them all, a finished one only for a plan's wait, its
// criteria, a held plan, its issue left open after its end or a helper's
// case.
func ShowsCases(t Task, cases []Case) bool {
	return slices.ContainsFunc(cases, func(o Case) bool {
		return !Final(t.Status) || PlanWaitCase(o, t) || o.Kind == CasePlanCriteria || o.Kind == CasePlanHeld || EndedCardCase(o, t) || o.TaskID != t.ID
	})
}

// CardCases are the cases at the human among live that card shows, its
// own and those of its live helpers, in reading order: by id, a plan's in
// the plan's order (orderPlanCases); none when it shows none (ShowsCases).
func CardCases(card BoardTask, live []Case) []Case {
	var out []Case
	for _, c := range live {
		if c.Status == CaseAtHuman && (c.TaskID == card.ID || slices.ContainsFunc(card.Helpers, func(h BoardTask) bool { return h.ID == c.TaskID })) {
			out = append(out, c)
		}
	}
	if !ShowsCases(card.Task, out) {
		return nil
	}
	slices.SortFunc(out, func(a, b Case) int { return cmp.Compare(a.ID, b.ID) })
	orderPlanCases(out)
	return out
}

// AskingCards are the cards that show cases at the human among live which
// cards, a board's read, does not hold: a finished card past Done's pages,
// such as a done plan whose prover asks. Each is read once, by the first
// of its cases, a helper's parent in its place. A card that would not show
// its cases even as a plan needs no read. A read that fails leaves its
// card out, and the failed reads come back joined.
func AskingCards(ctx context.Context, s interface {
	TaskStatuses(ctx context.Context, ids []string) (map[string]string, error)
	BoardTask(ctx context.Context, id string) (BoardTask, bool, error)
}, live []Case, cards []BoardTask) ([]BoardTask, error) {
	held := map[string]bool{}
	hold := func(b BoardTask) {
		held[b.ID] = true
		for _, h := range b.Helpers {
			held[h.ID] = true
		}
	}
	for _, b := range cards {
		hold(b)
	}
	atHuman := map[string][]Case{}
	for _, c := range live {
		if c.Status == CaseAtHuman && !held[c.TaskID] {
			atHuman[c.TaskID] = append(atHuman[c.TaskID], c)
		}
	}
	if len(atHuman) == 0 {
		return nil, nil
	}
	ids := slices.Sorted(maps.Keys(atHuman))
	statuses, err := s.TaskStatuses(ctx, ids)
	if err != nil {
		return nil, err
	}
	var errs []error
	read := func(id string) (BoardTask, bool) {
		b, ok, err := s.BoardTask(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("the card %s: %w", id, err))
		}
		return b, ok && err == nil
	}
	var out []BoardTask
	for _, id := range ids {
		if held[id] || !ShowsCases(Task{ID: id, Status: statuses[id], Track: policy.TrackPlan}, atHuman[id]) {
			continue
		}
		b, ok := read(id)
		if ok && b.Report() {
			if held[b.Parent] {
				continue
			}
			b, ok = read(b.Parent)
		}
		if !ok {
			continue
		}
		hold(b)
		if len(CardCases(b, live)) > 0 {
			out = append(out, b)
		}
	}
	return out, errors.Join(errs...)
}

// PlanCase says case c is a plan's question or its confirmation.
func PlanCase(c Case) bool { return c.Kind == CasePlanQuestion || c.Kind == CasePlanConfirmation }

// FactInt is case c's fact key as a number, 0 for none.
func FactInt(c Case, key string) int {
	n, _ := toInt(c.Facts[key])
	return n
}

// orderPlanCases puts a plan's cases, which sit in the list where its
// newest case does, in reading order: the newest revision first, its
// questions by index, then its confirmation.
func orderPlanCases(list []Case) {
	var at []int
	var plan []Case
	for i, c := range list {
		if PlanCase(c) {
			at, plan = append(at, i), append(plan, c)
		}
	}
	confirmation := func(c Case) int {
		if c.Kind == CasePlanConfirmation {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(plan, func(a, b Case) int {
		return cmp.Or(cmp.Compare(FactInt(b, "revision"), FactInt(a, "revision")), cmp.Compare(confirmation(a), confirmation(b)),
			cmp.Compare(FactInt(a, "index"), FactInt(b, "index")))
	})
	for j, i := range at {
		list[i] = plan[j]
	}
}

// CardPage is one page of a card's asks as the board shows it: a case
// with the facts its face reads, the part of the case's ask it shows (from
// 1, 0 for the case's own page), the request whose body answers it, the
// task whose case it is (a helper's own for a helper's case), and its
// face.
type CardPage struct {
	Case    Case
	Part    int
	Request *Request
	Owner   BoardTask
	Face    Face
}

// Key names page p among its card's: the case's id, and the part after a
// dot.
func (p CardPage) Key() string {
	if p.Part > 0 {
		return fmt.Sprintf("%d.%d", p.Case.ID, p.Part)
	}
	return fmt.Sprint(p.Case.ID)
}

// CardPages are the pages of card's asks: for a card with cases at the
// human (CardCases), each pending resource request of the card first, as a
// page numbered by its request's id negated, which opens no case of its
// own; then each case, a page per part of its ask that its request's rows
// hold, then a page of its own unless its parts are all a criteria
// request asks. A card with none shows a page per pending request of its
// own: a plan's first open question, else the request's own face. asks
// are the cases' newest asks, pending every pending request, read reads
// one with its rows.
func CardPages(card BoardTask, cases []Case, asks map[int64]CaseAsk, pending []ApprovalRequest, read func(ApprovalRequest) Request) []CardPage {
	requests := map[int64]*Request{}
	request := func(a ApprovalRequest) *Request {
		if r, ok := requests[a.ID]; ok {
			return r
		}
		r := read(a)
		requests[a.ID] = &r
		return &r
	}
	var out []CardPage
	if len(cases) == 0 {
		for _, a := range pending {
			if a.TaskID == card.ID {
				out = append(out, requestPage(card, request(a)))
			}
		}
		return out
	}
	var shown []Case
	for _, a := range pending {
		if a.TaskID != card.ID || a.Kind != ApprovalResource {
			continue
		}
		c, err := RequestCase(a)
		if err != nil {
			continue
		}
		c.ID, c.Kind, c.OpenedAt = -a.ID, CaseKind(ApprovalResource), a.CreatedAt
		shown = append(shown, c)
	}
	for _, c := range append(shown, cases...) {
		owner := card
		if i := slices.IndexFunc(card.Helpers, func(h BoardTask) bool { return h.ID == c.TaskID }); i >= 0 {
			owner = card.Helpers[i]
		}
		var r *Request
		for _, a := range pending {
			if a.TaskID == c.TaskID && RequestAnswers(a, c) {
				r = request(a)
				break
			}
		}
		parts := r.parts(c)
		for i, p := range parts {
			out = append(out, casePage(owner, p, i+1, r, asks))
		}
		if len(parts) == 0 || c.Kind != CasePlanCriteria {
			out = append(out, casePage(owner, c, 0, r, asks))
		}
	}
	return out
}

// casePage is part (0 for the whole) c of a case of owner, which request r
// answers, with the facts and the ask its face reads.
func casePage(owner BoardTask, c Case, part int, r *Request, asks map[int64]CaseAsk) CardPage {
	k := withDeps(c, owner)
	if r != nil && r.Kind == ApprovalPlan {
		k = OnPlan(k, r.planFacts())
	}
	ask := asks[c.ID]
	switch {
	case part > 0:
		ask = CaseAsk{}
	case k.Kind == CasePlanCriteria && r != nil:
		k, ask = r.onPromises(k), CaseAsk{}
	case r != nil && r.Healed:
		k = healed(k)
	}
	face := CaseFace(k, owner.Task, ask)
	if r != nil && r.Healed && part == 0 {
		face.Story.Left = healedLeft
	}
	return CardPage{Case: k, Part: part, Request: r, Owner: owner, Face: face}
}

// requestPage is the page of request r of card, which no case of the card
// holds: a plan's first open question, else the request's own face.
func requestPage(card BoardTask, r *Request) CardPage {
	if q, ok := r.questionCase(); ok {
		return CardPage{Case: q, Request: r, Owner: card, Face: CaseFace(q, card.Task, CaseAsk{})}
	}
	k, err := RequestCase(r.ApprovalRequest)
	switch {
	case r.Kind == ApprovalPlan:
		k = OnPlan(Case{Kind: CasePlanConfirmation, TaskID: r.TaskID, Repository: r.Project}, r.planFacts())
	case err != nil:
		k = Case{TaskID: r.TaskID, Repository: r.Project, Facts: map[string]any{"approval": r.ID, "approval_kind": r.Kind}}
	}
	switch {
	case r.Healed:
		k = healed(k)
	case r.Kind == ApprovalCriteria || r.Kind == ApprovalCriteriaRerun:
		k = r.onPromises(k)
	}
	face := CaseFace(k, card.Task, CaseAsk{})
	if r.Healed {
		face.Story.Left = healedLeft
	}
	for i, o := range face.Offers {
		if o.Value == ActNote {
			// no case keeps a note here: Not now writes nothing
			face.Offers[i].Value = KeyPark
		}
	}
	return CardPage{Case: k, Request: r, Owner: card, Face: face}
}
