package console

import (
	"cmp"
	"context"
	"encoding/json"
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
	Answers        bool
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
// desk, each card's cases at the human oldest first, and each card's live
// ask, the newest live case an answer on the card ends. The minute pass
// withdraws an older ask the live one replaced.
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
		slices.SortFunc(list, func(a, b factoryd.Case) int { return cmp.Compare(a.ID, b.ID) })
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
// the oldest when that case is no longer at the human; the row waits
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
	face := factoryd.CaseFace(shown, bt.Task, cs.desk.Asks[shown.ID])
	var slot *planSlot
	if planCase(shown) {
		slot = c.planSlotOf(ctx, face, shown)
	}
	hosts := v.policy.Document.LinkHosts
	face = v.scrubbed(face)
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
	for _, a := range v.pending {
		if a.TaskID == shown.TaskID && approvalOf(a, shown) {
			ap := c.approvalCard(ctx, a, act, v.byID, v.csrf, v.approves(k.Repository), true)
			ap.Filter, ap.Writer = v.filter, v.writes
			row.Approval = &ap
			switch {
			case ap.Healed:
				var healed factoryd.Face
				row.Case, healed = healedFace(shown, bt.Task)
				row.Face = v.scrubbed(healed)
			case shown.Kind == factoryd.CasePlanCriteria:
				row.Case = openPromises(shown, ap)
				row.Face = v.scrubbed(factoryd.CaseFace(row.Case, bt.Task, factoryd.CaseAsk{}))
			}
			break
		}
	}
	return need{Since: since, Card: k, CSRF: v.csrf, Case: &row}
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
		return a.Kind == factoryd.ApprovalResource
	case factoryd.CasePlanCriteria:
		id, _ := json.Marshal(k.Facts["approval"])
		return slices.Contains([]string{factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun}, a.Kind) && string(id) == strconv.FormatInt(a.ID, 10)
	case factoryd.CasePlanQuestion, factoryd.CasePlanConfirmation:
		return a.Kind == factoryd.ApprovalPlan
	}
	return false
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
	v.byID[bt.ID], v.cases = bt.Task, cs
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	c.render(w, "face", c.faceOf(ctx, v, c.caseNeed(ctx, v, button(r), bt, k, cs, caseID), true))
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
