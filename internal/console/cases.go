package console

import (
	"cmp"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// caseRow is the case of a card that the Needs you column shows: its
// five parts, its place among the card's cases (At of Of, newest first)
// and, for a merge or a plan's case, the approval whose body answers it.
// Answers: the case's own forms answer it, which only the card's newest
// ask takes.
type caseRow struct {
	Case       factoryd.Case
	Face       factoryd.Face
	Since      time.Time
	At, Of     int
	Project    string
	Approval   *approvalCard
	Answers    bool
	Recommends bool
}

// Waiting is how long the case has waited at the human.
func (r caseRow) Waiting() string {
	if r.Since.IsZero() {
		return ""
	}
	return minutes(time.Since(r.Since))
}

// Prev and Next are the positions the carousel's arrows go to, 0 at an end.
func (r caseRow) Prev() int {
	if r.At > 1 {
		return r.At - 1
	}
	return 0
}

func (r caseRow) Next() int {
	if r.At < r.Of {
		return r.At + 1
	}
	return 0
}

// humanCases are repository's at_human cases by card, each card's newest
// first.
func (c *Console) humanCases(ctx context.Context, repository string) (map[string][]factoryd.Case, error) {
	open, err := c.cfg.OpenCasesIn(ctx, repository)
	if err != nil {
		return nil, err
	}
	by := map[string][]factoryd.Case{}
	for _, k := range open {
		if k.Status == factoryd.CaseAtHuman {
			by[k.TaskID] = append(by[k.TaskID], k)
		}
	}
	for _, cs := range by {
		slices.SortFunc(cs, func(a, b factoryd.Case) int { return cmp.Compare(b.ID, a.ID) })
	}
	return by, nil
}

// faceTime is when case k was last put at the human: the start of its
// newest rung 3 step, else its opening.
func faceTime(k factoryd.Case, steps []factoryd.CaseStep) time.Time {
	for _, s := range slices.Backward(steps) {
		if s.Rung == 3 {
			return s.StartedAt
		}
	}
	return k.OpenedAt
}

// caseNeed is card k's row in Needs you with case n of its cases (1 is
// the newest) in view; it waits since its oldest case was faced.
func (c *Console) caseNeed(ctx context.Context, v *boardView, act actor, k card, cases []factoryd.Case, n int) need {
	since := time.Time{}
	var inView []factoryd.CaseStep
	for i, k := range cases {
		steps, err := c.cfg.CaseSteps(ctx, k.ID)
		if err != nil {
			log.Printf("console: the steps of case %d: %v", k.ID, err)
		}
		if at := faceTime(k, steps); since.IsZero() || at.Before(since) {
			since = at
		}
		if i == n-1 {
			inView = steps
		}
	}
	row := c.caseRowOf(ctx, v, act, k, cases, n, inView)
	return need{Since: since, Card: k, Escalated: true, CSRF: v.csrf, Case: &row}
}

// caseRowOf is case n of cases as card k shows it, steps its steps.
func (c *Console) caseRowOf(ctx context.Context, v *boardView, act actor, k card, cases []factoryd.Case, n int, steps []factoryd.CaseStep) caseRow {
	cs := cases[n-1]
	digest, err := c.cfg.CardDigest(ctx, cs.TaskID)
	if err != nil {
		log.Printf("console: the digest of %s: %v", cs.TaskID, err)
		digest = factoryd.CardDigest{}
	}
	face := factoryd.CaseFace(cs, steps, factoryd.FactsOf(cs), digest)
	hosts := v.policy.Document.LinkHosts
	for _, s := range []*string{&face.Ask, &face.Money, &face.Why, &face.Info, &face.Recommendation} {
		*s = sanitize.Outbound(scrubText(*s), hosts)
	}
	row := caseRow{Case: cs, Face: face, Since: faceTime(cs, steps), At: n, Of: len(cases), Project: v.filter.Project}
	if i := slices.IndexFunc(cases, func(o factoryd.Case) bool { return factoryd.AnsweredOnCard(o.Kind) }); i >= 0 {
		row.Answers = i == n-1
	}
	for _, a := range v.pending {
		if a.TaskID == cs.TaskID && approvalOf(a, cs) {
			ap := c.approvalCard(ctx, a, act, v.byID, v.csrf, v.approves(k.Repository), true)
			ap.Filter, ap.Writer = v.filter, v.writes
			row.Approval = &ap
			break
		}
	}
	return row
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

// caseFragment answers GET /projects/{name}/cards/{id}/cases/{n}: case n
// of the card's at_human cases, newest first, as the Needs you column
// shows it, which htmx swaps in for the card's row.
func (c *Console) caseFragment(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	n, err := strconv.Atoi(r.PathValue("n"))
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
	cases, cerr := c.humanCases(ctx, shown.Repository)
	if cerr != nil {
		log.Printf("console: the cases of %s: %v", shown.Name, cerr)
		http.Error(w, "cannot read the cases", http.StatusInternalServerError)
		return
	}
	mine := cases[id]
	if err != nil || n < 1 || n > len(mine) {
		http.NotFound(w, r)
		return
	}
	v.byID[bt.ID] = bt.Task
	k := c.boardCard(ctx, v, bt)
	c.finishCards(ctx, v, func(fn func(*card)) { fn(&k) })
	nd := c.caseNeed(ctx, v, button(r), k, mine, n)
	c.render(w, "case", nd)
}
