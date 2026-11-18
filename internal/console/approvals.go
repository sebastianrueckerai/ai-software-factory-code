package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// criterionRow is one criterion of a criteria or merge approval card: the
// criterion, its standing verdict and the evidence behind it.
type criterionRow struct {
	factoryd.PlanCriterion
	Verdict  string
	Evidence string
	Word     string                 // "blind" or "open" for a met gate row, from its evidence
	Artifact *factoryd.GateArtifact // a human criterion's artifact as the gate's newest run wrote it; nil without one
	CutLine  string                 // "the first 8 KiB of <n> KiB" when the card shows the artifact's first maxCardArtifact bytes
	Human    bool                   // Met and Not met are offered: a human criterion on a merge card
}

// Words are the row's promise as a face names it.
func (r criterionRow) Words() string { return factoryd.PromiseOf(r.PlanCriterion) }

// Detail is the row's claim beside a promise in other words, "" where the
// promise is the claim.
func (r criterionRow) Detail() string {
	if r.Promise == "" {
		return ""
	}
	return r.Claim
}

// maxCardArtifact is the most of an artifact a merge card shows.
const maxCardArtifact = 8 << 10

// approvalCard is a pending approval request with its rows, as its kind's
// card shows them. The request's Healed: a merge request opened on red
// checks whose recorded head's checks are green now (#1013, a re-run), so
// the card offers its Approve again and says so.
type approvalCard struct {
	factoryd.Request
	IDSuffix string // on the item's ids: "-d" in the drawer, so the column's copy keeps its own
	IssueURL string
	Issue    int64
	PlanName string // a plan approval: the plan's name, its title while it has none
	PRURL    string
	Monthly  float64
	// HeadUnread: the head's current checks could not be read (#1013), so
	// the card shows the face its frozen spec gives it, which may be
	// stale; the card says so rather than fail silently.
	HeadUnread bool
	// HeadUnreadWhy: that read's error, scrubbed, named in the card's
	// problem sentence.
	HeadUnreadWhy string
	Resource      cloudres.ResourceSpec
	// Criteria are the card's rows of the plan's promises; GateLines the
	// met gate rows of a criteria request that ask nothing, one line each.
	Criteria  []criterionRow
	GateLines []criterionRow
	// a plan: the summary and the cards in filing order
	PlanSummary string
	PlanRows    []factoryd.PlanRow
	// PlanOutcome and PlanGlossary are the spec's, PlanStill what its later
	// rounds plan and Criteria its criteria as rows with no verdict: the
	// drawer's layers under the decision, the fold holding the last three.
	PlanOutcome  string
	PlanGlossary []string
	PlanStill    []string
	Questions    []planQuestion
	Holdouts     []planHoldout
	Open         string // "<n> questions open" while any waits for its answer
	// Confirm is the waiting confirmation itself, which the plan's own
	// confirmation answers.
	Confirm *planQuestion
	Problem string // why the human's last action on the card was refused
	// TaskStatus is its task's, where the task's card stands once the
	// request is decided
	TaskStatus string
	// Filter is the board's filter, which the card's forms keep.
	Filter   boardFilter
	CSRF     string
	Approver bool
	// Writer: the viewer writes the project, so the card's title opens
	// its task's drawer, which holds the card's facts in full
	Writer bool
	// Opened is the drawer copy of a plan card's merge card alone: the
	// plan's opened proofs, each beside the pull request's version.
	// OpenedUnread: they could not be read, and the card says so.
	Opened       []proofView
	OpenedUnread bool
}

// proofView is an opened proof as the drawer shows it: the original
// code, and the pull request's version at the request's head when it
// differs. Missing: the pull request holds no file at the path.
type proofView struct {
	Criterion, Path, Original, Repository string
	Missing, Changed                      bool
}

// Waiting is how long the request has waited for a member, in minutes
// or hours and minutes; empty when its filing time is unknown.
func (k approvalCard) Waiting() string {
	if k.CreatedAt.IsZero() {
		return ""
	}
	return minutes(time.Since(k.CreatedAt))
}

// Rerun: the card is a factory pull request's merge request on red
// checks, whose approval offers their re-run instead of a merge (roadmap
// 264) — unless the head's checks healed (#1013), when it merges again.
func (k approvalCard) Rerun() bool {
	return k.Kind == factoryd.ApprovalMerge && k.Merge.Factory && k.Merge.RedChecks && !k.Healed
}

// Returns: the card is an agent's merge request on red checks, which cannot
// merge: it offers the return to the agent and the close, not Approve —
// unless the head's checks healed (#1013), when it merges again.
func (k approvalCard) Returns() bool {
	return k.Kind == factoryd.ApprovalMerge && !k.Merge.Factory && k.Merge.RedChecks && !k.Healed
}

// planQuestion is a plan's question as its card shows it, numbered from 1.
type planQuestion struct {
	Index int
	factoryd.PlanQuestion
	// Picks are its choices as the card shows them: masked, named by place
	Picks []planChoice
}

// ChoiceWords are the question's choices as one line, masked as the card
// shows them, for a question listed behind the ask.
func (q planQuestion) ChoiceWords() string {
	labels := make([]string, len(q.Picks))
	for i, p := range q.Picks {
		labels[i] = p.Label
	}
	return strings.Join(labels, " / ")
}

// planHoldout is a plan's hold-out as its card shows it: the claim with
// its inputs and expected numbers, its state's words, and whether it is
// a page of its own (a proposal, the round's confirmation unanswered).
type planHoldout struct {
	factoryd.HoldoutRow
	Blurb string
}

// holdoutBlurbs are the hold-out states' words.
var holdoutBlurbs = map[string]string{factoryd.ProofProposed: "waits for your answer", factoryd.ProofOkayed: "kept", factoryd.ProofDropped: "dropped"}

func (c *Console) approvalCard(ctx context.Context, a factoryd.ApprovalRequest, act actor, tasks map[string]factoryd.Task, csrf string, approver, face bool) approvalCard {
	t, ok := tasks[a.TaskID]
	r, err := factoryd.ReadRequest(ctx, requestReads{c.cfg.Tasks, c.cfg.Events}, a, t, func(ctx context.Context, project, sha string) (bool, error) {
		return c.headChecksGreen(ctx, act, project, sha)
	})
	k := approvalCard{Request: r, CSRF: csrf, Approver: approver}
	if a.MonthlyEUR != nil {
		k.Monthly = *a.MonthlyEUR
	}
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + a.Project
	k.TaskStatus = t.Status
	if t.Track == policy.TrackPlan {
		k.PlanName = cmp.Or(t.PlanName, factoryd.CardTitle(t))
	}
	if ok && t.IssueNumber > 0 {
		k.IssueURL, k.Issue = fmt.Sprintf("%s/issues/%d", base, t.IssueNumber), t.IssueNumber
	}
	if r.HeadErr != nil {
		log.Printf("console: approval request %d: %v", a.ID, r.HeadErr)
		k.HeadUnread, k.HeadUnreadWhy = true, forgejoCause(r.HeadErr)
	}
	switch a.Kind {
	case factoryd.ApprovalMerge:
		k.PRURL = fmt.Sprintf("%s/pulls/%d", base, a.PRNumber)
		if !face {
			var oerr error
			if k.Opened, oerr = c.openedProofsOf(ctx, act, t, a.Project, k.Merge.HeadSHA); oerr != nil {
				log.Printf("console: approval request %d: %v", a.ID, oerr)
				k.OpenedUnread = true
			}
		}
		if err == nil {
			k.Criteria, err = c.promiseRows(ctx, r.Promises, r.Integration)
		}
		if err != nil {
			k.Problem = "The console could not read the criterion rows: " + errText(err) + ". Reload the board."
		}
	case factoryd.ApprovalResource:
		err = json.Unmarshal(a.Spec, &k.Resource)
	case factoryd.ApprovalPlan:
		if err != nil {
			k.Problem = "The plan could not be read: " + errText(err) + ". Open the plan's approval again, or read the plan on its issue."
		}
		k.planOf()
	case factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		for _, p := range r.Promises {
			row := evidenceRow(criterionRow{PlanCriterion: p.PlanCriterion, Verdict: p.Verdict, Evidence: p.Evidence})
			if p.GateLine() {
				// a gate row that asks nothing: one line
				k.GateLines = append(k.GateLines, row)
				continue
			}
			k.Criteria = append(k.Criteria, row)
		}
	}
	if err != nil {
		log.Printf("console: approval request %d: %v", a.ID, err)
	}
	// the board's copy whispers: every part of the ask its first line,
	// the top findings alone — the drawer carries the whole text.
	// This is the one place a merge face is assembled, so a field
	// that grows cannot grow the column's card.
	if face && k.Kind == factoryd.ApprovalMerge {
		for i, w := range k.Merge.WhyHuman {
			k.Merge.WhyHuman[i] = faceLine(w, 200)
		}
		k.Merge.VerdictSummary = faceLine(k.Merge.VerdictSummary, 300)
		if len(k.Merge.Review.Findings) > 3 {
			k.Merge.Review.Findings = k.Merge.Review.Findings[:3]
		}
		for i, f := range k.Merge.Review.Findings {
			k.Merge.Review.Findings[i] = faceLine(f, 200)
		}
		k.Merge.Recommendation = faceLine(k.Merge.Recommendation, 300)
	}
	return k
}

// requestReads are the console's reads a request's rows come from.
type requestReads struct {
	Tasks
	Events
}

// openedProofsOf are the opened proofs of plan card t's plan when t is its
// integration card or a delta card, each read at head in project as a's
// human; nil for any other card.
func (c *Console) openedProofsOf(ctx context.Context, a actor, t factoryd.Task, project, head string) ([]proofView, error) {
	if t.Source != factoryd.SourcePlan || t.Parent == "" || head == "" {
		return nil, nil
	}
	_, p, ok, err := c.planTable(ctx, t.Parent, 0)
	if err != nil || !ok || !factoryd.CriteriaCard(p, t.PlanKey) {
		return nil, err
	}
	rows, err := c.cfg.Tasks.PlanProofs(ctx, t.Parent)
	if err != nil {
		return nil, fmt.Errorf("proofs of %s: %w", t.Parent, err)
	}
	var out []proofView
	for _, pp := range rows {
		if pp.State != factoryd.ProofOpened {
			continue
		}
		if a.s == nil {
			return nil, errors.New("the actor carries no session, so the pull request's files were not read")
		}
		route, err := rawRoute(project, pp.Path, head)
		if err != nil {
			return nil, err
		}
		v := proofView{Criterion: cmp.Or(pp.Criterion, pp.ID), Path: pp.Path, Original: pp.Code}
		var raw []byte
		err = c.as(ctx, a.s, http.MethodGet, route, nil, http.StatusOK, &raw)
		switch ae := (*apiError)(nil); {
		case errors.As(err, &ae) && ae.status == http.StatusNotFound:
			v.Missing = true
		case err != nil:
			return nil, &forgejoRefusal{what: "read " + pp.Path + " at " + head, err: err}
		case string(raw) != pp.Code:
			v.Changed, v.Repository = true, string(raw)
		}
		out = append(out, v)
	}
	return out, nil
}

// evidenceRow is row with its evidence read by factoryd.GateEvidence: a met
// gate row's blind or open as its Word, and its line as the full brief
// shows it, as the row names its promise already.
func evidenceRow(row criterionRow) criterionRow {
	state, line := factoryd.GateEvidence(row.ID, row.Evidence)
	if row.Verdict == "met" && (row.Kind == factoryd.CriterionTest || row.Kind == factoryd.CriterionInvariant) {
		row.Word = state
	}
	row.Evidence = line
	return row
}

// promiseRows are a merge card's rows of its plan's promises, standing on
// the plan's integration card home, as the card shows them: a met gate
// row's blind or open moved from its evidence to its Word, and each human
// promise's with its artifact, cut to maxCardArtifact bytes.
func (c *Console) promiseRows(ctx context.Context, promises []factoryd.PromiseRow, home string) ([]criterionRow, error) {
	var rows []criterionRow
	for _, p := range promises {
		row := evidenceRow(criterionRow{PlanCriterion: p.PlanCriterion, Verdict: p.Verdict, Evidence: p.Evidence})
		if p.Kind == factoryd.CriterionHuman {
			row.Human = true
			b, err := c.cfg.Timelines.Get(ctx, factoryd.ArtifactKey(home, p.ID))
			if err != nil {
				return nil, fmt.Errorf("the artifact of %s: %w", p.ID, err)
			}
			if b != nil {
				a := &factoryd.GateArtifact{}
				if err := json.Unmarshal(b, a); err != nil {
					return nil, fmt.Errorf("the artifact of %s: %w", p.ID, err)
				}
				if len(a.Content) > maxCardArtifact {
					row.CutLine = fmt.Sprintf("the first %d KiB of %d KiB", maxCardArtifact>>10, (len(a.Content)+1023)>>10)
					a.Content = strings.ToValidUTF8(a.Content[:maxCardArtifact], "")
				}
				row.Artifact = a
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// scrubLayers masks the model-written plan layers the drawer's copy holds
// under the decision, as the case face masks its texts.
func (k *approvalCard) scrubLayers(hosts []string) {
	k.PlanSummary = sanitize.Outbound(scrubText(k.PlanSummary), hosts)
	k.PlanOutcome = sanitize.Outbound(scrubText(k.PlanOutcome), hosts)
	for i, g := range k.PlanGlossary {
		k.PlanGlossary[i] = sanitize.Outbound(scrubText(g), hosts)
	}
	for i, s := range k.PlanStill {
		k.PlanStill[i] = sanitize.Outbound(scrubText(s), hosts)
	}
	for i, row := range k.Criteria {
		row.Promise = sanitize.Outbound(scrubText(row.Promise), hosts)
		row.Claim = sanitize.Outbound(scrubText(row.Claim), hosts)
		row.Suite = sanitize.Outbound(scrubText(row.Suite), hosts)
		k.Criteria[i] = row
	}
	for i := range k.Questions {
		q := &k.Questions[i]
		q.Text, q.Answer = sanitize.Outbound(scrubText(q.Text), hosts), sanitize.Outbound(scrubText(q.Answer), hosts)
	}
	for i, w := range k.TakeOffWhys {
		k.TakeOffWhys[i] = sanitize.Outbound(scrubText(w), hosts)
	}
}

// planOf fills plan card k from its request's revision: the plan's
// layers, its promises as rows with no verdict, its questions and its
// round's extra promises.
func (k *approvalCard) planOf() {
	p := k.Plan
	k.PlanSummary, k.PlanRows, k.PlanStill = p.Summary, factoryd.PlanRows(p), p.Outline
	if p.Spec != nil {
		k.PlanOutcome, k.PlanGlossary = p.Spec.Outcome, p.Spec.Glossary
	}
	for _, c := range p.Criteria {
		k.Criteria = append(k.Criteria, criterionRow{PlanCriterion: c})
	}
	confirmAt := -1
	open := 0
	for i, q := range k.PlanQuestions {
		pq := planQuestion{Index: i + 1, PlanQuestion: q}
		for j, ch := range q.Choices {
			pq.Picks = append(pq.Picks, planChoice{Index: j + 1, Label: scrubText(ch)})
		}
		k.Questions = append(k.Questions, pq)
		if q.Kind == factoryd.QuestionConfirmation && !q.Answered() {
			confirmAt = i
		}
		if !q.Answered() {
			open++
		}
	}
	if confirmAt >= 0 {
		k.Confirm = &k.Questions[confirmAt]
	}
	if open > 0 {
		k.Open = fmt.Sprintf("%d %s open", open, plural(open, "question"))
	}
	for _, h := range k.HoldoutRows {
		k.Holdouts = append(k.Holdouts, planHoldout{HoldoutRow: h, Blurb: holdoutBlurbs[h.State]})
	}
}

// planTable is revision n (0 the newest) of plan task id's plan: the
// revision and its plan; false when there is none.
func (c *Console) planTable(ctx context.Context, id string, n int) (factoryd.PlanRevision, factoryd.Plan, bool, error) {
	rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, id, n)
	if err != nil || !ok {
		return rev, factoryd.Plan{}, false, err
	}
	var p factoryd.Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return rev, factoryd.Plan{}, false, fmt.Errorf("revision %d of %s: %w", rev.Revision, id, err)
	}
	return rev, p, true, nil
}

// approve approves the approval request the path names (doApprove); a
// deny has its own door (deny), so an approve that names another action is
// refused rather than approving.
func (c *Console) approve(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		if action := r.PostFormValue("action"); action != "" && action != "approve" {
			return "", refuse(http.StatusBadRequest, "The answer "+action+" is not one the card shows.")
		}
		return c.doApprove(ctx, a, id)
	})
}

// deny denies the approval request the path names with the human's
// reason (doDeny).
func (c *Console) deny(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		return c.doDeny(ctx, a, id, r.PostFormValue("action"), r.PostFormValue("note"))
	})
}

// decideCriterion answers POST /approvals/{id}/criterion: doDecide with
// the form's criterion, verb and reason, through decideButton.
func (c *Console) decideCriterion(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		return c.doDecide(ctx, a, id, r.PostFormValue("criterion"), r.PostFormValue("verb"), r.PostFormValue("reason"))
	})
}

// confirm records the human's confirmation that the plan's delivered
// cards meet its criteria, with the answer's note as its line
// (doConfirmCriteria).
func (c *Console) confirm(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, func(ctx context.Context, a actor, id int64) (string, error) {
		return c.doConfirmCriteria(ctx, a, id, strings.TrimSpace(r.PostFormValue("note")))
	})
}

// rerun runs the red checks of the factory pull request card whose merge
// request the path names again (doRerun).
func (c *Console) rerun(w http.ResponseWriter, r *http.Request) {
	c.decideButton(w, r, c.doRerun)
}

// decideButton runs a button's decision on the approval request the path
// names and answers the press.
func (c *Console) decideButton(w http.ResponseWriter, r *http.Request, decision func(ctx context.Context, a actor, id int64) (string, error)) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, err = decision(r.Context(), button(r), id)
	c.pressed(w, r, err)
}

// review submits a pull request review on q's pull request as its decider
// (posterOf), so the agent's decision is never the human's review.
func (c *Console) review(ctx context.Context, a actor, q factoryd.ApprovalRequest, event, body, commit string) error {
	repo, err := factoryd.SplitRepo(q.Project)
	if err != nil {
		return err
	}
	if a, err = c.posterOf(ctx, a, q.Project); err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), q.PRNumber)
	return c.as(ctx, a.s, http.MethodPost, path, map[string]string{"event": event, "body": body, "commit_id": commit}, http.StatusOK, nil)
}

// headChecksGreen reads whether commit sha's checks are green in project
// now, as a's member: a merge request opened on red checks heals once the
// same head's checks turn green (#1013: a re-run) and may merge then. An
// actor that carries no session — the pulse's render, which no member
// asked for — fails closed as unread instead of following the nil
// session: a member session is not required to paint a card. Green
// mirrors factoryd's checksResult: a head that reports no statuses
// is still pending, and warning and skipped count as green.
func (c *Console) headChecksGreen(ctx context.Context, a actor, project, sha string) (bool, error) {
	if a.s == nil {
		return false, &forgejoRefusal{what: "read the head's checks", err: errors.New("the actor carries no session, so the head's current checks were not read")}
	}
	repo, err := factoryd.SplitRepo(project)
	if err != nil {
		return false, err
	}
	var cs factoryd.CombinedStatus
	path := fmt.Sprintf("/api/v1/repos/%s/%s/commits/%s/status", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(sha))
	if err := c.as(ctx, a.s, http.MethodGet, path, nil, http.StatusOK, &cs); err != nil {
		return false, &forgejoRefusal{what: "read the head's checks", err: err}
	}
	return len(cs.Statuses) > 0 && (cs.State == "success" || cs.State == "warning" || cs.State == "skipped"), nil
}
