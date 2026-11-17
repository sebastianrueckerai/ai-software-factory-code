package console

import (
	"cmp"
	"context"
	"encoding/json"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// caseRow is the case of a card that Needs you shows: its five parts, its
// page among the card's pages (At of Of), the pages the arrows go to and,
// for a merge or a plan's case, the approval whose body answers it. A
// page that is a part of a case's ask holds the part in its case's facts. Answers: the case's own forms answer it, which only the
// card's live ask takes.
type caseRow struct {
	Case   factoryd.Case
	Face   factoryd.Face
	Since  time.Time
	At, Of int
	// Key names the page in view, PrevKey and NextKey the pages beside it
	// (page.key)
	Key, PrevKey, NextKey string
	Project               string
	Approval              *approvalCard
	Answers               bool
	// Query is the board's filter, which the arrows' reads carry
	Query string
	// Plan is the slot's part 5 when the case is a plan question or its confirmation
	Plan *planSlot
	// Owner is the helper task whose case this is, nil for the card's own:
	// its answers act on the helper (card)
	Owner *card
}

// card is the card case row r of card k acts on: its owner's, else k.
func (r caseRow) card(k card) card {
	if r.Owner != nil {
		return *r.Owner
	}
	return k
}

// planSlot is a plan question's or confirmation's form: the answer of
// revision Revision's question Index.
type planSlot struct {
	Revision, Index int
	Confirm         bool
	Choices         []planChoice
	TakeOffRefs     []string
	TakeOffWhys     []string
}

// planChoice is one answer, which a send names by Index (from 1) among the
// question's choices; Label is what the slot shows, with its secrets masked.
type planChoice struct {
	Index   int
	Label   string
	Checked bool
}

// Waiting is how long the case has waited at the human.
func (r caseRow) Waiting() string {
	if r.Since.IsZero() {
		return ""
	}
	return minutes(time.Since(r.Since))
}

// caseSet is what one board read holds of its repository's cases: the
// desk, each card's cases at the human oldest first, a helper task's
// among its parent's, each task's live ask, the newest live case an answer
// on the task ends, and the helper tasks whose cases a card holds. The
// minute pass withdraws an older ask the live one replaced.
type caseSet struct {
	desk    factoryd.CaseDesk
	byCard  map[string][]factoryd.Case
	liveAsk map[string]factoryd.Case
	helpers map[string]factoryd.BoardTask
}

// caseSetOf reads repository's case set, the cases of the helper tasks of
// cards among their parent's.
func (c *Console) caseSetOf(ctx context.Context, repository string, cards ...factoryd.BoardTask) (caseSet, error) {
	desk, err := c.cfg.CaseDesk(ctx, repository)
	cs := caseSet{desk: desk, byCard: map[string][]factoryd.Case{}, liveAsk: map[string]factoryd.Case{}, helpers: map[string]factoryd.BoardTask{}}
	if err != nil {
		return cs, err
	}
	for _, k := range desk.Cases {
		if factoryd.AnsweredOnCard(k.Kind) && k.ID > cs.liveAsk[k.TaskID].ID {
			cs.liveAsk[k.TaskID] = k
		}
	}
	for _, k := range desk.Cases {
		if k.Status == factoryd.CaseAtHuman {
			cs.byCard[k.TaskID] = append(cs.byCard[k.TaskID], k)
		}
	}
	for _, list := range cs.byCard {
		sortCases(list)
	}
	cs.adopt(cards...)
	return cs, nil
}

// adopt files the cases of the helper tasks of cards among their
// parent's.
func (cs caseSet) adopt(cards ...factoryd.BoardTask) {
	for _, bt := range cards {
		for _, h := range bt.Helpers {
			cs.helpers[h.ID] = h
			if list, ok := cs.byCard[h.ID]; ok {
				cs.byCard[bt.ID] = append(cs.byCard[bt.ID], list...)
				delete(cs.byCard, h.ID)
				sortCases(cs.byCard[bt.ID])
			}
		}
	}
}

// sortCases puts a card's cases in reading order: by id, a plan's in its
// own order (orderPlanCases).
func sortCases(list []factoryd.Case) {
	slices.SortFunc(list, func(a, b factoryd.Case) int { return cmp.Compare(a.ID, b.ID) })
	orderPlanCases(list)
}

// askingCards are the cards that hold a case at the human in cs but that
// cards, the board's read, does not hold: a finished card past Done's
// pages, such as a done plan whose prover asks. Each is read once, by the
// first of its cases, a helper's parent in its place, and cs files the
// read card's helpers' cases among its own, so Needs you holds the row
// whatever page the card is on and whichever id comes first. A card that
// would not hold its cases even as a plan needs no read.
func (c *Console) askingCards(ctx context.Context, cs caseSet, cards []factoryd.BoardTask) []factoryd.BoardTask {
	held := map[string]bool{}
	for _, bt := range cards {
		held[bt.ID] = true
		for _, h := range bt.Helpers {
			held[h.ID] = true
		}
	}
	var ids []string
	for id := range cs.byCard {
		if !held[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	statuses, err := c.cfg.Tasks.TaskStatuses(ctx, ids)
	if err != nil {
		log.Printf("console: the cards of the cases at the human: %v", err)
		return nil
	}
	read := func(id string) (factoryd.BoardTask, bool) {
		bt, ok, err := c.cfg.Tasks.BoardTask(ctx, id)
		if err != nil {
			log.Printf("console: the card of a case at the human, %s: %v", id, err)
		}
		return bt, ok && err == nil
	}
	var out []factoryd.BoardTask
	for _, id := range ids {
		if _, open := cs.byCard[id]; !open || held[id] || !cs.holds(factoryd.Task{ID: id, Status: statuses[id], Track: policy.TrackPlan}) {
			continue
		}
		bt, ok := read(id)
		if ok && bt.Report() {
			if held[bt.Parent] {
				continue
			}
			bt, ok = read(bt.Parent)
		}
		if !ok {
			continue
		}
		held[bt.ID] = true
		cs.adopt(bt)
		for _, h := range bt.Helpers {
			held[h.ID] = true
		}
		if cs.holds(bt.Task) {
			out = append(out, bt)
		}
	}
	return out
}

func planCase(k factoryd.Case) bool {
	return k.Kind == factoryd.CasePlanQuestion || k.Kind == factoryd.CasePlanConfirmation
}

func factInt(k factoryd.Case, key string) int {
	n, _ := k.Facts[key].(float64)
	return int(n)
}

// orderPlanCases puts a plan's cases, which sit in the list where its
// newest case does, in reading order: the newest revision first, its
// questions by index, then its confirmation.
func orderPlanCases(list []factoryd.Case) {
	var at []int
	var plan []factoryd.Case
	for i, k := range list {
		if planCase(k) {
			at, plan = append(at, i), append(plan, k)
		}
	}
	slices.SortStableFunc(plan, func(a, b factoryd.Case) int {
		return cmp.Or(cmp.Compare(factInt(b, "revision"), factInt(a, "revision")),
			cmp.Compare(btoi(a.Kind == factoryd.CasePlanConfirmation), btoi(b.Kind == factoryd.CasePlanConfirmation)),
			cmp.Compare(factInt(a, "index"), factInt(b, "index")))
	})
	for j, i := range at {
		list[i] = plan[j]
	}
}

// pagesOf are the pages of card id's carousel when it holds cases: first
// each pending resource request of the card, which opens no case of its
// own and leads its asks, as a page numbered by its request's id negated;
// then its cases at the human, oldest first.
func pagesOf(v *boardView, cs caseSet, id string) []factoryd.Case {
	cases := cs.byCard[id]
	if len(cases) == 0 {
		return nil
	}
	var pages []factoryd.Case
	for _, a := range v.pending {
		if a.TaskID != id || a.Kind != factoryd.ApprovalResource {
			continue
		}
		k, err := factoryd.RequestCase(a)
		if err != nil {
			log.Printf("console: approval request %d: %v", a.ID, err)
			continue
		}
		k.ID, k.Kind, k.OpenedAt = -a.ID, factoryd.CaseKind(factoryd.ApprovalResource), a.CreatedAt
		pages = append(pages, k)
	}
	return append(pages, cases...)
}

// requestPage says case k is a page pagesOf made of a request.
func requestPage(k factoryd.Case) bool { return k.ID < 0 }

// page is one page of a card's carousel: a case of pagesOf, or one part of
// its ask (Part from 1): a promise a human judges before a merge, an open
// promise of a plan's criteria request, an extra promise of a plan's
// confirmation.
type page struct {
	Case factoryd.Case
	Part int
}

// key names page p in a board read and an arrow's link: the case's id, and
// the part after a dot.
func (p page) key() string {
	k := strconv.FormatInt(p.Case.ID, 10)
	if p.Part > 0 {
		k += "." + strconv.Itoa(p.Part)
	}
	return k
}

// parts are the parts of case k's ask that request ap's rows hold, each a
// page of its own: the human promises of a merge with no verdict yet, the
// open promises of a criteria request, the extra promises a plan's
// confirmation waits on. Every part is case k with its part's facts.
func parts(k factoryd.Case, ap *approvalCard) []factoryd.Case {
	if ap == nil {
		return nil
	}
	var out []factoryd.Case
	promise := func(r criterionRow, evidence string) factoryd.PromiseFact {
		return factoryd.PromiseFact{ID: r.ID, Promise: factoryd.PromiseOf(r.PlanCriterion), Kind: r.Kind, Verdict: r.Verdict, Evidence: evidence}
	}
	switch k.Kind {
	case factoryd.CaseMergeApproval:
		for _, r := range ap.Criteria {
			if r.Human && slices.Contains(ap.Unsettled, r.ID) {
				out = append(out, factoryd.OnPromise(k, promise(r, r.PlanCriterion.Evidence), len(ap.Unsettled), len(ap.Criteria)))
			}
		}
	case factoryd.CasePlanCriteria:
		k = openPromises(k, *ap)
		var open []criterionRow
		for _, r := range ap.Criteria {
			if r.Verdict != "met" && r.Verdict != "waived" {
				open = append(open, r)
			}
		}
		for _, r := range open {
			out = append(out, factoryd.OnPromise(k, promise(r, r.Evidence), len(open), len(ap.Criteria)+len(ap.GateLines)))
		}
	case factoryd.CasePlanConfirmation:
		for _, h := range ap.Holdouts {
			if h.Decides {
				out = append(out, factoryd.OnHoldout(k, factoryd.HoldoutFact{Idx: h.Idx, Claim: h.Claim}))
			}
		}
	}
	return out
}

// planFacts are what plan request ap names of its plan.
func planFacts(ap *approvalCard) factoryd.PlanFacts {
	p := factoryd.PlanFacts{Outcome: ap.PlanOutcome, OffMain: ap.TakeOffRefs, OffMainWhys: ap.TakeOffWhys}
	for _, r := range ap.PlanRows {
		p.Cards = append(p.Cards, r.Title)
	}
	for _, r := range ap.Criteria {
		p.Promises = append(p.Promises, factoryd.PromiseOf(r.PlanCriterion))
	}
	for _, h := range ap.Holdouts {
		if h.Decides {
			p.HoldoutsOpen++
		}
	}
	return p
}

// faceAt is when case k was last put at the human: its newest ask's time,
// else its opening.
func (cs caseSet) faceAt(k factoryd.Case) time.Time {
	if at := cs.desk.Asks[k.ID].At; !at.IsZero() {
		return at
	}
	return k.OpenedAt
}

// caseNeed is card bt's row in Needs you with the page inView in view,
// the first when that page is no longer there; the row waits since its
// oldest case was faced. A case whose ask has parts (parts) is a page per
// part, then a page of its own unless its parts are all it asks.
func (c *Console) caseNeed(ctx context.Context, v *boardView, act actor, bt factoryd.BoardTask, k card, cs caseSet, inView string) need {
	cases := pagesOf(v, cs, bt.ID)
	since := time.Time{}
	for _, o := range cases {
		if f := cs.faceAt(o); since.IsZero() || f.Before(since) {
			since = f
		}
	}
	if since.IsZero() {
		since = waitingSince(bt)
	}
	// each request whose body answers a case, read once
	read := map[int64]*approvalCard{}
	approvalOfCase := func(o factoryd.Case) *approvalCard {
		for _, a := range v.pending {
			if a.TaskID != o.TaskID || !approvalOf(a, o) {
				continue
			}
			if ap, ok := read[a.ID]; ok {
				return ap
			}
			ap := c.approvalCard(ctx, a, act, v.byID, v.csrf, v.approves(k.Repository), true)
			ap.Filter, ap.Writer = v.filter, v.writes
			read[a.ID] = &ap
			return &ap
		}
		return nil
	}
	var pages []page
	var shows []factoryd.Case
	for _, o := range cases {
		ps := parts(o, approvalOfCase(o))
		for i, p := range ps {
			pages, shows = append(pages, page{o, i + 1}), append(shows, p)
		}
		if len(ps) == 0 || o.Kind != factoryd.CasePlanCriteria {
			pages, shows = append(pages, page{o, 0}), append(shows, o)
		}
	}
	at := max(slices.IndexFunc(pages, func(p page) bool { return p.key() == inView }), 0)
	shown, ap := pages[at], approvalOfCase(pages[at].Case)
	// a helper's case asks on its parent and acts on the helper
	owner, helper := cs.helpers[shown.Case.TaskID]
	if !helper {
		owner = bt
	}
	row := caseRow{Case: withDeps(shows[at], owner), Since: cmp.Or(cs.faceAt(shown.Case), since), At: at + 1, Of: len(pages), Key: shown.key(),
		Project: v.filter.Project, Query: v.filter.query(), Approval: ap,
		Answers: cs.liveAsk[owner.ID].ID == shown.Case.ID && shown.Case.CommentID == owner.QuestionComment}
	if helper {
		o := c.card(owner.Task)
		o.Filter, o.MayWriteSummary, o.CSRF = k.Filter, k.MayWriteSummary, k.CSRF
		row.Owner = &o
	}
	if at > 0 {
		row.PrevKey = pages[at-1].key()
	}
	if at+1 < len(pages) {
		row.NextKey = pages[at+1].key()
	}
	if ap != nil && ap.Kind == factoryd.ApprovalPlan {
		row.Case = factoryd.OnPlan(row.Case, planFacts(ap))
	}
	ask := cs.desk.Asks[shown.Case.ID]
	switch {
	case shown.Part > 0:
		ask = factoryd.CaseAsk{}
	case shown.Case.Kind == factoryd.CasePlanCriteria && ap != nil:
		row.Case, ask = openPromises(row.Case, *ap), factoryd.CaseAsk{}
	case ap != nil && ap.Healed:
		row.Case, _ = healedFace(row.Case, bt.Task)
	}
	face := factoryd.CaseFace(row.Case, owner.Task, ask)
	if ap != nil && ap.Healed && shown.Part == 0 {
		face.Story.Left = "The checks healed after a re-run."
	}
	if planCase(shown.Case) && shown.Part == 0 {
		if slot := c.planSlotOf(ctx, face, shown.Case); slot != nil {
			hosts := v.policy.Document.LinkHosts
			for i := range slot.Choices {
				slot.Choices[i].Label = sanitize.Outbound(scrubText(slot.Choices[i].Label), hosts)
			}
			for i := range slot.TakeOffWhys {
				slot.TakeOffWhys[i] = sanitize.Outbound(scrubText(slot.TakeOffWhys[i]), hosts)
			}
			row.Plan = slot
		}
	}
	row.Face = v.scrubbed(face)
	return need{Since: since, Card: k, CSRF: v.csrf, Case: &row}
}

// withDeps is case k of card bt with the cards a human's work card waits
// for and that are not done, as the board read them in the shape Act's
// facts read them, among its facts: its face says what it waits for, and
// offers "I did it" once none is left.
func withDeps(k factoryd.Case, bt factoryd.BoardTask) factoryd.Case {
	if bt.Kind != "human" || len(bt.Dependencies) == 0 {
		return k
	}
	k.Facts = maps.Clone(k.Facts)
	if k.Facts == nil {
		k.Facts = map[string]any{}
	}
	k.Facts["dependencies"] = bt.Dependencies
	return k
}

// planSlotOf is the form of plan case k, whose face is face: the
// question's choices, the one the face recommends checked, or the
// confirmation with the cards it takes off main. A case that names no
// revision and no index has none.
func (c *Console) planSlotOf(ctx context.Context, face factoryd.Face, k factoryd.Case) *planSlot {
	revision, index := factInt(k, "revision"), factInt(k, "index")
	if revision == 0 || index == 0 {
		return nil
	}
	confirm := k.Kind == factoryd.CasePlanConfirmation
	slot := &planSlot{Revision: revision, Index: index, Confirm: confirm}
	_, p, ok, err := c.planTable(ctx, k.TaskID, revision)
	if err != nil {
		log.Printf("console: plan %s revision %d: %v", k.TaskID, revision, err)
	}
	if ok && confirm {
		if slot.TakeOffRefs, slot.TakeOffWhys, err = c.takeOffOf(ctx, k.TaskID, p); err != nil {
			log.Printf("console: %v", err)
		}
	}
	if confirm {
		return slot
	}
	choices, _ := k.Facts["choices"].([]any)
	recommended, _ := k.Facts["recommended"].(string)
	lead := ""
	for _, ch := range choices {
		if s, _ := ch.(string); s != "" && strings.HasPrefix(face.Recommendation, s+":") {
			lead = s
		}
	}
	if lead == "" && face.Recommendation != "" {
		lead = recommended
	}
	for i, ch := range choices {
		if s, _ := ch.(string); s != "" {
			slot.Choices = append(slot.Choices, planChoice{Index: i + 1, Label: s, Checked: s == lead})
		}
	}
	return slot
}

// casesInView reads the page each card showed, which a board read names
// by case=<card>:<page>.
func casesInView(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, v := range r.URL.Query()["case"] {
		if card, key, ok := strings.Cut(v, ":"); ok {
			out[card] = key
		}
	}
	return out
}

// approvalOf says whether approval request a is the body of case k: a
// merge request of the merge case that names it, a resource request of
// its case of the resource kind — the export's shape, as the store opens
// no case of its own for one — a plan's of its plan question or
// confirmation, which the case's own form answers.
func approvalOf(a factoryd.ApprovalRequest, k factoryd.Case) bool {
	switch k.Kind {
	case factoryd.CaseMergeApproval:
		id, _ := json.Marshal(k.Facts["approval"])
		return a.Kind == factoryd.ApprovalMerge && string(id) == strconv.FormatInt(a.ID, 10)
	case factoryd.CaseKind(factoryd.ApprovalResource):
		id, named := k.Facts["approval"]
		raw, _ := json.Marshal(id)
		return a.Kind == factoryd.ApprovalResource && (!named || string(raw) == strconv.FormatInt(a.ID, 10))
	case factoryd.CasePlanCriteria:
		id, _ := json.Marshal(k.Facts["approval"])
		return slices.Contains([]string{factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun}, a.Kind) && string(id) == strconv.FormatInt(a.ID, 10)
	case factoryd.CasePlanQuestion, factoryd.CasePlanConfirmation:
		return a.Kind == factoryd.ApprovalPlan
	}
	return false
}

// caseFragment answers GET /projects/{name}/cards/{id}/cases/{case}: the
// card's page with that key as Needs you shows it, which htmx swaps in for
// the card's row.
func (c *Console) caseFragment(w http.ResponseWriter, r *http.Request) {
	ctx, id, key := r.Context(), r.PathValue("id"), r.PathValue("case")
	f := urlFilter(r)
	f.Project = r.PathValue("name")
	v, all, verr := c.viewOf(r, f)
	if verr != nil {
		log.Printf("console: case of %s: %v", id, verr)
		http.Error(w, "cannot read the card", http.StatusInternalServerError)
		return
	}
	shown, _, writable, serr := c.shownProject(ctx, r, v, all, f)
	if serr != nil {
		http.NotFound(w, r)
		return
	}
	v.writes = writable[shown.Name]
	bt, ok, terr := c.cfg.Tasks.BoardTask(ctx, id)
	switch {
	case terr != nil:
		log.Printf("console: card %s: %v", id, terr)
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return
	case !ok || bt.Repository != shown.Repository:
		http.NotFound(w, r)
		return
	}
	cs, cerr := c.caseSetOf(ctx, shown.Repository, bt)
	if cerr != nil {
		log.Printf("console: the cases of %s: %v", shown.Name, cerr)
		http.Error(w, "cannot read the cases", http.StatusInternalServerError)
		return
	}
	if len(pagesOf(v, cs, id)) == 0 {
		http.NotFound(w, r)
		return
	}
	v.byID[bt.ID], v.cases = bt.Task, cs
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	n := c.caseNeed(ctx, v, button(r), bt, k, cs, key)
	if n.Case.Key != key {
		http.NotFound(w, r)
		return
	}
	c.render(w, "face", c.faceOf(ctx, v, n, true))
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
