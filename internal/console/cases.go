package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// caseRow is the case of a card that Needs you shows: its five parts, its
// place among the card's cases (At of Of, newest first), the cases the
// arrows go to and, for a merge or a plan's case, the approval whose body
// answers it. Answers: the case's own forms answer it, which only the
// card's live ask takes.
type caseRow struct {
	Case           factoryd.Case
	Face           factoryd.Face
	Since          time.Time
	At, Of         int
	PrevID, NextID int64
	Project        string
	Approval       *approvalCard
	// Offers are the human's buttons: the actions Offered lists on the
	// case's facts, the recommended one first and preselected; Refusals
	// say why the nearest ones do not apply when none is offered.
	Offers   []offer
	Refusals []factoryd.ActionRefusal
	// Premade are the words the card's question offers as answers: each
	// sends as a continue with the words as its note
	Premade []string
	// Extras are the card's pending approvals no case of its own answers, under the carousel
	Extras  []approvalCard
	Answers bool
	// Decides says the case's face is the table's: its stored offers, the ask's own answers and a fund and a note in one form
	Decides bool
	// Query is the board's filter, which the arrows' reads carry
	Query string
	// Placed: the row is the card's place on the board, which a card swap replaces; the wide view
	// keeps the card's place in its state column
	Placed bool
	// Plan is the slot's part 5 when the case is a plan question or its confirmation
	Plan *planSlot
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
// desk, each card's cases at the human newest first, and each card's live
// ask, the newest live case an answer on the card ends.
type caseSet struct {
	desk    factoryd.CaseDesk
	byCard  map[string][]factoryd.Case
	liveAsk map[string]factoryd.Case
}

func (c *Console) caseSetOf(ctx context.Context, repository string) (caseSet, error) {
	desk, err := c.cfg.CaseDesk(ctx, repository)
	cs := caseSet{desk: desk, byCard: map[string][]factoryd.Case{}, liveAsk: map[string]factoryd.Case{}}
	if err != nil {
		return cs, err
	}
	for _, k := range desk.Cases {
		if k.Status == factoryd.CaseAtHuman {
			cs.byCard[k.TaskID] = append(cs.byCard[k.TaskID], k)
		}
		if factoryd.AnsweredOnCard(k.Kind) && k.ID > cs.liveAsk[k.TaskID].ID {
			cs.liveAsk[k.TaskID] = k
		}
	}
	for _, list := range cs.byCard {
		slices.SortFunc(list, func(a, b factoryd.Case) int { return cmp.Compare(b.ID, a.ID) })
		orderPlanCases(list)
	}
	return cs, nil
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

// faceAt is when case k was last put at the human: its newest ask's time,
// else its opening.
func (cs caseSet) faceAt(k factoryd.Case) time.Time {
	if at := cs.desk.Asks[k.ID].At; !at.IsZero() {
		return at
	}
	return k.OpenedAt
}

// caseNeed is card bt's row in Needs you with the case inView in view,
// the newest when that case is no longer at the human; the row waits
// since its oldest case was faced.
func (c *Console) caseNeed(ctx context.Context, v *boardView, act actor, bt factoryd.BoardTask, k card, cs caseSet, inView int64) need {
	cases := cs.byCard[bt.ID]
	at := max(slices.IndexFunc(cases, func(o factoryd.Case) bool { return o.ID == inView }), 0)
	since := time.Time{}
	for _, o := range cases {
		if f := cs.faceAt(o); since.IsZero() || f.Before(since) {
			since = f
		}
	}
	if since.IsZero() {
		since = waitingSince(bt)
	}
	shown := cases[at]
	var answered *time.Time
	if t, ok := cs.desk.Answered[bt.ID]; ok {
		answered = &t
	}
	face := factoryd.CaseFace(shown, cs.desk.Steps[shown.ID], factoryd.FactsOf(shown), cs.desk.Asks[shown.ID], answered)
	var slot *planSlot
	if planCase(shown) {
		slot = c.planSlotOf(ctx, &face, bt, shown, cases)
	}
	hosts := v.policy.Document.LinkHosts
	for _, s := range []*string{&face.Ask, &face.Money, &face.Why, &face.Info, &face.Recommendation, &face.Source} {
		*s = sanitize.Outbound(scrubText(*s), hosts)
	}
	row := caseRow{Case: shown, Face: face, Since: cmp.Or(cs.faceAt(shown), since), At: at + 1, Of: len(cases), Project: v.filter.Project, Placed: !v.filter.wide(), Query: v.filter.query(),
		Answers: cs.liveAsk[bt.ID].ID == shown.ID && shown.CommentID == bt.QuestionComment}
	if at > 0 {
		row.PrevID = cases[at-1].ID
	}
	if at+1 < len(cases) {
		row.NextID = cases[at+1].ID
	}
	if slot != nil {
		for i := range slot.Choices {
			slot.Choices[i].Label = sanitize.Outbound(scrubText(slot.Choices[i].Label), hosts)
		}
		for i := range slot.TakeOffWhys {
			slot.TakeOffWhys[i] = sanitize.Outbound(scrubText(slot.TakeOffWhys[i]), hosts)
		}
		row.Plan = slot
	}
	if row.Decides = row.Answers && factoryd.AnswersByOffers(shown, bt.Kind); row.Decides {
		row.Offers, row.Refusals = offersOf(face, shown.Kind == factoryd.CaseAgentTimeCap || factoryd.FactsOf(shown).Planner)
		row.Premade = bt.Choices
	}
	for _, a := range v.pending {
		if a.TaskID != shown.TaskID {
			continue
		}
		// a plan's approval is the body of a plan case that has no slot, whose
		// questions it then asks itself
		slotless := a.Kind == factoryd.ApprovalPlan && planCase(shown) && row.Plan == nil
		own := slotless || a.Kind != factoryd.ApprovalPlan && slices.ContainsFunc(cases, func(o factoryd.Case) bool { return approvalOf(a, o) })
		if own && !approvalOf(a, shown) {
			continue // its own case shows it
		}
		ap := c.approvalCard(ctx, a, act, v.byID, v.csrf, v.approves(k.Repository), true)
		ap.Filter, ap.Writer = v.filter, v.writes
		ap.buildDecide()
		ap.Asks = slotless
		if own {
			ap.Hosted = true // its case's parts div shows the ask
			row.Approval = &ap
		} else {
			row.Extras = append(row.Extras, ap)
		}
	}
	return need{Since: since, Card: k, Escalated: true, CSRF: v.csrf, Case: &row}
}

// planSlotOf is the slot of plan case k of card bt: its face's ask and why
// are the plan's one line, never the revision's text, and its choices are
// the question's, the one the face recommends checked.
// A case that names no revision and no index has no slot.
func (c *Console) planSlotOf(ctx context.Context, face *factoryd.Face, bt factoryd.BoardTask, k factoryd.Case, cases []factoryd.Case) *planSlot {
	revision, index := factInt(k, "revision"), factInt(k, "index")
	if revision == 0 || index == 0 {
		return nil
	}
	confirm := k.Kind == factoryd.CasePlanConfirmation
	plan := fmt.Sprintf("Plan #%d, revision %d", bt.IssueNumber, revision)
	open := 0
	for _, o := range cases {
		if o.Kind == factoryd.CasePlanQuestion && factInt(o, "revision") == revision {
			open++
		}
	}
	switch {
	case confirm:
		face.Ask = "Confirm the spec and the criteria, or request changes."
		face.Why = plan + ": Confirm fixes the spec and the criteria, Request changes sends your words to the next planner round."
	default:
		face.Why = fmt.Sprintf("%s: %d %s open; the planner asks all of them in one turn, and the next revision starts once each is answered.", plan, open, plural(open, "question"))
	}
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

// casesInView reads the case each card showed, which a board read names
// by case=<card>:<case>.
func casesInView(r *http.Request) map[string]int64 {
	out := map[string]int64{}
	for _, v := range r.URL.Query()["case"] {
		card, id, ok := strings.Cut(v, ":")
		if n, err := strconv.ParseInt(id, 10, 64); ok && err == nil {
			out[card] = n
		}
	}
	return out
}

// approvalOf says whether approval request a is the body of case k: a
// merge request of the merge case that names it, a plan's of its plan
// question or confirmation, which the case's own form answers.
func approvalOf(a factoryd.ApprovalRequest, k factoryd.Case) bool {
	switch k.Kind {
	case factoryd.CaseMergeApproval:
		id, _ := json.Marshal(k.Facts["approval"])
		return a.Kind == factoryd.ApprovalMerge && string(id) == strconv.FormatInt(a.ID, 10)
	case factoryd.CasePlanCriteria:
		id, _ := json.Marshal(k.Facts["approval"])
		return slices.Contains([]string{factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun}, a.Kind) && string(id) == strconv.FormatInt(a.ID, 10)
	case factoryd.CasePlanQuestion, factoryd.CasePlanConfirmation:
		return a.Kind == factoryd.ApprovalPlan
	}
	return false
}

// caseBriefOf is the brief the case that holds request a wrote, "" while it
// has none.
func (c *Console) caseBriefOf(ctx context.Context, a factoryd.ApprovalRequest, hosts []string) string {
	cases, err := c.cfg.OpenCasesOf(ctx, a.TaskID)
	if err != nil {
		log.Printf("console: the cases of %s: %v", a.TaskID, err)
		return ""
	}
	for _, k := range cases {
		if approvalOf(a, k) {
			return sanitize.Outbound(scrubText(k.Brief), hosts)
		}
	}
	return ""
}

// caseFragment answers GET /projects/{name}/cards/{id}/cases/{case}: the
// card's case at the human with that id as Needs you shows it, which htmx
// swaps in for the card's row.
func (c *Console) caseFragment(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	caseID, err := strconv.ParseInt(r.PathValue("case"), 10, 64)
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
	cs, cerr := c.caseSetOf(ctx, shown.Repository)
	if cerr != nil {
		log.Printf("console: the cases of %s: %v", shown.Name, cerr)
		http.Error(w, "cannot read the cases", http.StatusInternalServerError)
		return
	}
	if err != nil || !slices.ContainsFunc(cs.byCard[id], func(o factoryd.Case) bool { return o.ID == caseID }) {
		http.NotFound(w, r)
		return
	}
	v.byID[bt.ID] = bt.Task
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	c.render(w, "case", c.caseNeed(ctx, v, button(r), bt, k, cs, caseID))
}

// offer is one button of a case's face: an action with its effect in a
// line and the inputs of its arguments, named "<action>.<argument>".
type offer struct {
	Name, Label, Effect string
	Args                []offerArg
	Recommended         bool
	By                  string // who recommended it
	// Aria names the row the button decides: an approval face's buttons
	// repeat across the board's cards, so each says which request of
	// which task it answers. Empty leaves the label as the partial
	// builds it.
	Aria       string
	NoteOnStep bool // the note stays on the case's step: no comment goes on the issue
}

type offerArg struct {
	Field, Label string
	Options      []string
	Number       bool
	Float        bool
}

// offersOf are the buttons of a face as its ask stored them when the case
// reached the human (or when Act last refused a press on it), the action the ask
// preselected first, each with the inputs of its arguments; the refusals the
// ask stored when only the actions every case takes remain.
func offersOf(face factoryd.Face, timeStop bool) ([]offer, []factoryd.ActionRefusal) {
	var out []offer
	for _, a := range face.Offers {
		o := offer{NoteOnStep: factoryd.IsBoardAction(a.Value), Name: a.Value, Label: a.Label, Effect: a.Effect, Recommended: a.Value == face.Preselected && face.Preselected != ""}
		if o.Recommended {
			o.By = face.Source
		}
		for _, arg := range factoryd.ActionArgs[a.Value] {
			o.Args = append(o.Args, offerArg{Field: a.Value + "." + arg.Name, Label: arg.Label, Options: arg.Options, Number: arg.Number, Float: arg.Float})
		}
		if a.Value == factoryd.ActExtend && timeStop {
			slices.SortStableFunc(o.Args, func(x, y offerArg) int {
				return cmp.Compare(btoi(strings.HasSuffix(y.Field, ".minutes")), btoi(strings.HasSuffix(x.Field, ".minutes")))
			})
		}
		if o.Recommended {
			out = slices.Insert(out, 0, o)
		} else {
			out = append(out, o)
		}
	}
	return out, face.Refusals
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
