package console

import (
	"cmp"
	"context"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// caseRow is the case of a card that Needs you shows: its five parts, its
// page among the card's pages (At of Of), the pages the arrows go to and,
// for a merge or a plan's case, the approval whose body answers it. A
// page that is a part of a case's ask holds the part in its case's facts.
// Answers: the case's own forms answer it, which only the card's live ask
// takes.
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
// desk, the cases at the human each card read shows, its helpers' among
// them (factoryd.CardCases), and each task's live ask, the newest live
// case an answer on the task ends. The minute pass withdraws an older ask
// the live one replaced.
type caseSet struct {
	desk    factoryd.CaseDesk
	byCard  map[string][]factoryd.Case
	liveAsk map[string]factoryd.Case
}

// caseSetOf reads repository's case set with the cases cards show.
func (c *Console) caseSetOf(ctx context.Context, repository string, cards ...factoryd.BoardTask) (caseSet, error) {
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
	cs.adopt(cards...)
	return cs, nil
}

// adopt files the cases each card of cards shows, its helpers' among them.
func (cs caseSet) adopt(cards ...factoryd.BoardTask) {
	for _, bt := range cards {
		cs.byCard[bt.ID] = factoryd.CardCases(bt, cs.desk.Cases)
	}
}

// askingCards are the cards that show a case at the human in cs but that
// cards, the board's read, does not hold (factoryd.AskingCards), filed in
// cs, so Needs you holds the row whatever page the card is on.
func (c *Console) askingCards(ctx context.Context, cs caseSet, cards []factoryd.BoardTask) []factoryd.BoardTask {
	out, err := factoryd.AskingCards(ctx, c.cfg.Tasks, cs.desk.Cases, cards)
	if err != nil {
		log.Printf("console: the cards of the cases at the human: %v", err)
	}
	cs.adopt(out...)
	return out
}

// requestPage says case k is a page of a card's carousel made of a
// resource request (factoryd.CardPages).
func requestPage(k factoryd.Case) bool { return k.ID < 0 }

// faceAt is when case k was last put at the human: its newest ask's time,
// else its opening.
func (cs caseSet) faceAt(k factoryd.Case) time.Time {
	if at := cs.desk.Asks[k.ID].At; !at.IsZero() {
		return at
	}
	return k.OpenedAt
}

// caseNeed is card bt's row in Needs you with the page inView of its
// pages (factoryd.CardPages) in view, the first when that page is no
// longer there; the row waits since its oldest case was faced.
func (c *Console) caseNeed(ctx context.Context, v *boardView, act actor, bt factoryd.BoardTask, k card, cs caseSet, inView string) need {
	// each request whose body answers a page, read once
	read := map[int64]*approvalCard{}
	pages := factoryd.CardPages(bt, cs.byCard[bt.ID], cs.desk.Asks, v.pending, func(a factoryd.ApprovalRequest) factoryd.Request {
		ap, ok := v.requests[a.ID]
		if !ok {
			ap = c.approvalCard(ctx, a, act, v.byID, v.csrf, v.approves(k.Repository), true)
		}
		ap.Filter, ap.Writer = v.filter, v.writes
		read[a.ID] = &ap
		return ap.Request
	})
	since := time.Time{}
	for _, p := range pages {
		if f := cs.faceAt(p.Case); since.IsZero() || f.Before(since) {
			since = f
		}
	}
	if since.IsZero() {
		since = waitingSince(bt)
	}
	at := max(slices.IndexFunc(pages, func(p factoryd.CardPage) bool { return p.Key() == inView }), 0)
	shown := pages[at]
	var ap *approvalCard
	if shown.Request != nil {
		ap = read[shown.Request.ID]
	}
	// a helper's case asks on its parent and acts on the helper
	owner := shown.Owner
	row := caseRow{Case: shown.Case, Since: cmp.Or(cs.faceAt(shown.Case), since), At: at + 1, Of: len(pages), Key: shown.Key(),
		Project: v.filter.Project, Query: v.filter.query(), Approval: ap,
		Answers: cs.liveAsk[owner.ID].ID == shown.Case.ID && shown.Case.CommentID == owner.QuestionComment}
	if owner.ID != bt.ID {
		o := c.card(owner.Task)
		o.Filter, o.MayWriteSummary, o.CSRF = k.Filter, k.MayWriteSummary, k.CSRF
		row.Owner = &o
	}
	if at > 0 {
		row.PrevKey = pages[at-1].Key()
	}
	if at+1 < len(pages) {
		row.NextKey = pages[at+1].Key()
	}
	if factoryd.PlanCase(shown.Case) && shown.Part == 0 {
		if slot := planSlotOf(shown.Face, shown.Case); slot != nil {
			hosts := v.policy.Document.LinkHosts
			for i := range slot.Choices {
				slot.Choices[i].Label = sanitize.Outbound(scrubText(slot.Choices[i].Label), hosts)
			}
			row.Plan = slot
		}
	}
	row.Face = v.scrubbed(shown.Face)
	return need{Since: since, Card: k, CSRF: v.csrf, Case: &row}
}

// planSlotOf is the form of plan case k, whose face is face: the
// question's choices, the one the face recommends checked, or the
// confirmation. A case that names no revision and no index has none.
func planSlotOf(face factoryd.Face, k factoryd.Case) *planSlot {
	revision, index := factoryd.FactInt(k, "revision"), factoryd.FactInt(k, "index")
	if revision == 0 || index == 0 {
		return nil
	}
	confirm := k.Kind == factoryd.CasePlanConfirmation
	slot := &planSlot{Revision: revision, Index: index, Confirm: confirm}
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

// waitsForFace says request a shows through its case's pages alone, once
// the case is at the human: a merge's, a plan's and a plan's criteria
// request's. A resource request opens no case and has its own page.
func waitsForFace(a factoryd.ApprovalRequest) bool {
	return a.Kind == factoryd.ApprovalMerge || a.Kind == factoryd.ApprovalPlan || a.Kind == factoryd.ApprovalCriteria ||
		a.Kind == factoryd.ApprovalCriteriaRerun
}

// climbing is the live case that holds request q while it climbs its
// rungs, false once it is at the human or with no live case.
func (c *Console) climbing(ctx context.Context, q factoryd.ApprovalRequest) (factoryd.Case, bool, error) {
	open, err := c.cfg.OpenCasesOf(ctx, q.TaskID)
	if err != nil {
		return factoryd.Case{}, false, err
	}
	i := slices.IndexFunc(open, func(k factoryd.Case) bool { return factoryd.RequestAnswers(q, k) })
	if i < 0 || open[i].Status == factoryd.CaseAtHuman {
		return factoryd.Case{}, false, nil
	}
	return open[i], true, nil
}

// faced says a case at the human holds request a.
func (cs caseSet) faced(a factoryd.ApprovalRequest) bool {
	return slices.ContainsFunc(cs.desk.Cases, func(k factoryd.Case) bool {
		return k.Status == factoryd.CaseAtHuman && k.TaskID == a.TaskID && factoryd.RequestAnswers(a, k)
	})
}

// planFaced says a pending plan request of card taskID has its face: a
// case of its revision at the human.
func planFaced(v *boardView, cs caseSet, taskID string) bool {
	return slices.ContainsFunc(v.pending, func(a factoryd.ApprovalRequest) bool {
		return a.Kind == factoryd.ApprovalPlan && a.TaskID == taskID && cs.faced(a)
	})
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
		http.Error(w, "The card's asks could not be read. Reload the board.", http.StatusInternalServerError)
		return
	}
	if !cs.holds(bt.Task) {
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
