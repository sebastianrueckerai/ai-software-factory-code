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
	// Extras are the card's pending approvals no case of its own answers, under the carousel
	Extras  []approvalCard
	Answers bool
	// Query is the board's filter, which the arrows' reads carry
	Query string
	// Placed: the row is the card's place on the board, which a card swap replaces; the wide view
	// keeps the card's place in its state column
	Placed bool
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
	}
	return cs, nil
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
	for _, a := range v.pending {
		if a.TaskID != shown.TaskID {
			continue
		}
		own := slices.ContainsFunc(cases, func(o factoryd.Case) bool { return approvalOf(a, o) })
		if own && !approvalOf(a, shown) {
			continue // its own case shows it
		}
		ap := c.approvalCard(ctx, a, act, v.byID, v.csrf, v.approves(k.Repository), true)
		ap.Filter, ap.Writer = v.filter, v.writes
		if own {
			row.Approval = &ap
		} else {
			row.Extras = append(row.Extras, ap)
		}
	}
	return need{Since: since, Card: k, Escalated: true, CSRF: v.csrf, Case: &row}
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
// question or confirmation.
func approvalOf(a factoryd.ApprovalRequest, k factoryd.Case) bool {
	switch k.Kind {
	case factoryd.CaseMergeApproval:
		id, _ := json.Marshal(k.Facts["approval"])
		return a.Kind == factoryd.ApprovalMerge && string(id) == strconv.FormatInt(a.ID, 10)
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
	v.byID[bt.ID] = bt.Task
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	c.render(w, "case", c.caseNeed(ctx, v, button(r), bt, k, cs, caseID))
}
