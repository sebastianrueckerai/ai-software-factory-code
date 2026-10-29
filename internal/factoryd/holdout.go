package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The hold-outs (the spec-intake concept, piece 6): criteria the confirmed
// spec's proof does not name, blind to the planner and the builder by
// routing and storage. A confirmation round's zone proposes them, the
// review model drafting each criterion and its test against the
// repository beside the project's past revert reasons, and the dry run
// on the main head before the form closes: red before, green after.
// Stored hold-outs run at the criteria gate of the plan's integration
// card; a red one goes to an audit session, never to the card, and the
// plan's close waits until the audit, the builder or the human settles
// it.

// The hold-out states: a proposal waits for the human's okay or drop;
// unrunnable is a draft that never ran, whose only click is drop; stored
// runs at the criteria gate; opened is one an audit ruled a real gap,
// its criterion public.
const (
	HoldoutProposed   = "proposed"
	HoldoutDropped    = "dropped"
	HoldoutStored     = "stored"
	HoldoutUnrunnable = "unrunnable"
	HoldoutOpened     = "opened"
)

// Hold-out origins, the audit's three verdicts, the catch ledger's kinds
// and finders (the recording rule, roadmap 285).
const (
	HoldoutDrafted    = "drafted"
	HoldoutMechanical = "mechanical"
	AuditWrongTest    = "wrong_test"
	AuditRealGap      = "real_gap"
	AuditSpecQuestion = "spec_question"
	CatchHoldoutRed   = "holdout_red"
	CatchAudit        = "audit"
	FinderGate        = "gate"
	FinderAuditor     = "auditor"
)

// thenAudit is the way on an audit sandbox that could not start.
const thenAudit = "then answer to run the hold-out audit again, or close the card"

// Holdout is one hold-out criterion of a plan task, as stored: round is
// the plan revision whose confirmation proposed it, idx its number on
// the task, which never changes.
type Holdout struct {
	Idx   int    `json:"idx"`
	Round int    `json:"round"`
	ID    string `json:"id"`
	// Origin says what drafted it: the spec, or a past failure it cites.
	Origin string `json:"origin"`
	// Criterion is the hold-out's claim in the spec's words; TestName the
	// test its draft defines, TestCode the draft itself.
	Criterion string `json:"criterion"`
	State     string `json:"state"`
	// DryHead is the main head the draft ran on, DryVerdict red, green or
	// unrunnable, DryOutput one line of why.
	DryHead    string `json:"dry_head"`
	DryVerdict string `json:"dry_verdict"`
	DryOutput  string `json:"dry_output"`
	// AuditVerdict and AuditReason hold what an audit made of a red one at
	// the gate, Rewrite the test its rewrite filed; RedCycles counts its
	// reds at the gate after the audit opened it.
	AuditVerdict string `json:"audit_verdict"`
	AuditReason  string `json:"audit_reason"`
	Rewrite      string `json:"rewrite"`
	RedCycles    int    `json:"red_cycles"`
	TestName     string `json:"test_name"`
	TestCode     string `json:"test_code"`
}

// ErrHoldoutsOpen refuses a confirmation answer while its round's
// proposals wait for the human's okay or drop.
var ErrHoldoutsOpen = errors.New("the round's hold-outs wait for your okay or drop")

// ErrNoHoldout names a hold-out the task does not hold.
var ErrNoHoldout = errors.New("no such hold-out")

// ErrHoldoutClick refuses a click the hold-out's state takes not: a row
// decided already, or an okay on a draft that could not run.
var ErrHoldoutClick = errors.New("the hold-out takes no such click now")

var (
	// holdoutMarker reads the dry run's marker line of one draft, as
	// criterionMarker reads a criterion's: its index, exit, run and fail
	// counts and one line of evidence.
	holdoutMarker = regexp.MustCompile(`^__holdout ([0-9]+) exit=(-?[0-9]+) ran=([0-9]+) fail=([0-9]+)(?: (.*))?$`)
	// holdoutID admits a hold-out id: the criterion charset, so a marker
	// line cannot forge one.
	holdoutID = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	// holdoutTestName admits a drafted test's name.
	holdoutTestName = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)
)

// One round's and one draft's bounds.
const (
	maxHoldouts    = 5
	maxHoldoutCode = 4 << 10
	maxHoldoutText = 200
	heredocMarker  = "FACTORYD_HOLDOUT"
	holdoutPkg     = "holdout"
)

// pullRef is a pull request's head ref, branchRef a branch's: what a
// repro pod fetches.
func pullRef(pr int64) string { return fmt.Sprintf("refs/pull/%d/head", pr) }

func branchRef(branch string) string { return "refs/heads/" + branch }

// holdoutScript is the dry run: every draft written into a scratch
// package of its own inside the repository, one go test per draft, the
// markers alone on stdout, as criteriaScript runs the criteria.
func holdoutScript(drafts []Holdout) string {
	var b strings.Builder
	for _, h := range drafts {
		dir := fmt.Sprintf(".%s%d", holdoutPkg, h.Idx)
		fmt.Fprintf(&b, "mkdir -p %s\ncat > %s/holdout_test.go <<'%s%d'\npackage %s\n%s\n%s%d\n", dir, dir, heredocMarker, h.Idx, holdoutPkg, h.TestCode, heredocMarker, h.Idx)
		fmt.Fprintf(&b, "o=/tmp/ho%d\ngo test -v -run '^%s$' ./%s >\"$o\" 2>&1\nrc=$?\n", h.Idx, h.TestName, dir)
		fmt.Fprintf(&b, "ran=$(grep -c '^=== RUN   %s$' \"$o\")\nfail=$(grep -c '^--- FAIL: %s ' \"$o\")\n", h.TestName, h.TestName)
		b.WriteString(`ev=$(grep -m1 '^--- FAIL: ' "$o")
[ -n "$ev" ] || ev=$(head -n 1 "$o")
`)
		fmt.Fprintf(&b, "printf '__holdout %d exit=%%s ran=%%s fail=%%s %%s\\n' \"$rc\" \"$ran\" \"$fail\" \"$ev\"\n", h.Idx)
	}
	return b.String()
}

// parseHoldoutMarkers reads the dry run's marker lines from the output
// tail; the last marker of a draft wins.
func parseHoldoutMarkers(output string) map[int]criterionVerdict {
	out := map[int]criterionVerdict{}
	for _, line := range strings.Split(output, "\n") {
		m := holdoutMarker.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		exit, _ := strconv.Atoi(m[2])
		ran, _ := strconv.Atoi(m[3])
		fail, _ := strconv.Atoi(m[4])
		out[idx] = criterionVerdict{exit: exit, ranCount: ran, failCount: fail, evidence: m[5]}
	}
	return out
}

// holdoutDryVerdict judges one draft's marker: red is a hold-out's shape
// (the test runs and fails), green already passes (a guard to keep, or a
// wrong test the audit settles later), unrunnable never ran (no such
// test, or the code does not build).
func holdoutDryVerdict(v criterionVerdict, ok bool) (dry, evidence string) {
	if !ok {
		return HoldoutUnrunnable, "no verdict from the run"
	}
	ev := shortenEvidence(v.evidence)
	switch {
	case v.failCount > 0:
		return "red", ev
	case v.ranCount == 0:
		return HoldoutUnrunnable, ev
	case v.exit != 0:
		return HoldoutUnrunnable, ev
	default:
		return "green", ev
	}
}

// holdoutProposal is the review model's proposal of one hold-out, as its
// JSON call answers; From cites the past failure a mechanical proposal
// names, "" for one drafted from the spec.
type holdoutProposal struct {
	ID        string `json:"id"`
	Criterion string `json:"criterion"`
	TestName  string `json:"test_name"`
	TestCode  string `json:"test_code"`
	From      string `json:"from"`
}

// askHoldouts runs a confirmation round's pipeline: the review model
// proposes criteria with drafted tests in one call, the drafts dry-run on
// the main head in one sandbox, drafts that cannot run get one corrective
// pass and dry-run again, and the round's rows land with their verdicts.
// The zone shows what landed; the human's only clicks are okay and drop.
// A failure leaves the round empty, the confirmation question stands on
// its own, and the timeline names why.
func (r *Runner) askHoldouts(ctx context.Context, t Task, revision int, p Plan) error {
	defer beatUntilDone(ctx, "holdouts")()
	rows, err := r.store.PlanHoldouts(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, h := range rows {
		if h.Round == revision {
			return nil // an activity retry: the round asked already
		}
	}
	if err := r.store.SweepPlanHoldouts(ctx, t.ID); err != nil {
		return err
	}
	reverts, err := r.store.PastReverts(ctx, t.Repository)
	if err != nil {
		return err
	}
	alias := r.callModel(t, policy.PurposeReview)
	drafts := []Holdout{}
	if alias != "" {
		if drafts, err = r.proposeHoldouts(ctx, t, revision, p, alias, reverts); err != nil {
			r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
			return nil
		}
	}
	if len(drafts) == 0 {
		return nil
	}
	_, repo, err := r.task(ctx, t.ID)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	branch, err := r.forgejo.RepoDefaultBranch(ctx, repo.Owner, repo.Name)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, branch)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": err.Error()})
		return nil
	}
	// the planner session's sandbox went with its session: the dry runs
	// build their own
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": "dry run: " + err.Error()})
		return nil
	}
	defer done()
	run := t
	for pass := 0; ; pass++ {
		pending := false
		for _, h := range drafts {
			if h.DryVerdict == "" {
				pending = true
			}
		}
		if !pending || pass > 1 {
			break
		}
		run.ReproCommand = holdoutScript(drafts)
		rep, err := r.runRepro(ctx, run, repo, branchRef(branch), head, "")
		if err != nil {
			r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": "dry run: " + err.Error()})
			return nil
		}
		markers := parseHoldoutMarkers(rep.Output)
		for i, h := range drafts {
			if h.DryVerdict != "" {
				continue
			}
			drafts[i].DryVerdict, drafts[i].DryOutput = holdoutDryVerdict(markers[h.Idx], holdoutRan(markers, h.Idx))
			drafts[i].DryHead = head
		}
		if pass == 0 {
			again, err := r.redraftHoldouts(ctx, t, p, alias, drafts)
			if err != nil {
				r.record(ctx, t.ID, "holdouts_not_drafted", map[string]any{"round": revision, "reason": "redraft: " + err.Error()})
			}
			for i, h := range drafts {
				if h.DryVerdict == HoldoutUnrunnable && again[h.ID] != "" {
					drafts[i].TestCode = again[h.ID]
					drafts[i].DryVerdict, drafts[i].DryOutput = "", ""
				}
			}
		}
	}
	verdicts := make(map[string]string, len(drafts))
	for i, h := range drafts {
		drafts[i].State = HoldoutProposed
		if h.DryVerdict == HoldoutUnrunnable {
			drafts[i].State = HoldoutUnrunnable
		}
		verdicts[h.ID] = h.DryVerdict
	}
	inserted, err := r.store.PutPlanHoldouts(ctx, t.ID, revision, drafts)
	if err != nil || !inserted {
		return err
	}
	r.record(ctx, t.ID, "holdouts_proposed", map[string]any{"round": revision, "verdicts": verdicts})
	return nil
}

// holdoutRan tells whether the run reported on idx at all.
func holdoutRan(markers map[int]criterionVerdict, idx int) bool {
	_, ok := markers[idx]
	return ok
}

// gateHoldouts are the plan task's hold-outs the criteria gate runs: the
// stored ones and the opened ones, in idx order. A read that fails runs
// the gate without them and names itself in the timeline.
func (r *Runner) gateHoldouts(ctx context.Context, parent string) []Holdout {
	rows, err := r.store.PlanHoldouts(ctx, parent)
	if err != nil {
		r.record(ctx, parent, "gate_criteria", map[string]any{"holdouts_unread": err.Error()})
		return nil
	}
	var out []Holdout
	for _, h := range rows {
		if h.State == HoldoutStored || h.State == HoldoutOpened {
			out = append(out, h)
		}
	}
	return out
}

// proposeHoldouts is the one model call: the confirmed spec, the plan's
// criteria and the project's past failures in, the proposals out. Ids
// off the charset, drafts off the bounds and code without its test are
// dropped; an id a plan criterion or an earlier proposal holds gains _h.
func (r *Runner) proposeHoldouts(ctx context.Context, t Task, revision int, p Plan, alias string, reverts []string) ([]Holdout, error) {
	spec := p.Spec
	if spec == nil {
		spec = &Spec{}
	}
	input := struct {
		Spec         *Spec           `json:"spec"`
		Criteria     []PlanCriterion `json:"criteria"`
		PastFailures []string        `json:"past_failures"`
		Max          int             `json:"max"`
	}{Spec: spec, Criteria: p.Criteria, PastFailures: reverts, Max: maxHoldouts}
	content, _, err := r.supervisor.Chat(ctx, t.ID, alias, policy.PurposeReview,
		r.cfg.Policy().Document.Limits.SupervisorTimeout, holdoutSystem, input)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Holdouts []holdoutProposal `json:"holdouts"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil {
		return nil, fmt.Errorf("the answer is not the hold-outs json: %w", err)
	}
	takenRows, err := r.store.PlanHoldouts(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	next := 1
	taken := map[string]bool{}
	for _, h := range takenRows {
		taken[h.ID] = true
		if h.Idx >= next {
			next = h.Idx + 1
		}
	}
	for _, c := range p.Criteria {
		taken[c.ID] = true
	}
	out := []Holdout{}
	for _, pr := range answer.Holdouts {
		if len(out) >= maxHoldouts {
			break
		}
		id := strings.ToLower(strings.TrimSpace(pr.ID))
		if !holdoutID.MatchString(id) {
			continue
		}
		if taken[id] {
			id += "_h"
		}
		if taken[id] || !holdoutID.MatchString(id) {
			continue
		}
		taken[id] = true
		name := strings.TrimSpace(pr.TestName)
		code := strings.TrimSpace(pr.TestCode)
		criterion := strings.Join(strings.Fields(pr.Criterion), " ")
		if !holdoutTestName.MatchString(name) || criterion == "" || len(criterion) > maxHoldoutText ||
			code == "" || len(code) > maxHoldoutCode || strings.Contains(code, heredocMarker) ||
			!strings.Contains(code, "func "+name+"(") {
			continue
		}
		origin := HoldoutDrafted
		if strings.TrimSpace(pr.From) != "" {
			origin = HoldoutMechanical
		}
		out = append(out, Holdout{Round: revision, ID: id, Origin: origin,
			Criterion: r.untrusted(t, criterion), TestName: name, TestCode: code})
	}
	for i := range out {
		out[i].Idx = next + i
	}
	return out, nil
}

// redraftHoldouts is the corrective pass: the drafts that could not run,
// with why, back through the model once; the answer maps id to new code.
func (r *Runner) redraftHoldouts(ctx context.Context, t Task, p Plan, alias string, drafts []Holdout) (map[string]string, error) {
	if alias == "" {
		return nil, nil
	}
	spec := p.Spec
	if spec == nil {
		spec = &Spec{}
	}
	type failed struct {
		ID       string `json:"id"`
		TestName string `json:"test_name"`
		TestCode string `json:"test_code"`
		Output   string `json:"output"`
	}
	in := struct {
		Spec   *Spec    `json:"spec"`
		Failed []failed `json:"failed"`
	}{Spec: spec}
	for _, h := range drafts {
		if h.DryVerdict == HoldoutUnrunnable {
			in.Failed = append(in.Failed, failed{ID: h.ID, TestName: h.TestName, TestCode: h.TestCode, Output: h.DryOutput})
		}
	}
	if len(in.Failed) == 0 {
		return nil, nil
	}
	content, _, err := r.supervisor.Chat(ctx, t.ID, alias, policy.PurposeReview,
		r.cfg.Policy().Document.Limits.SupervisorTimeout, holdoutFixSystem, in)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Holdouts []holdoutProposal `json:"holdouts"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil {
		return nil, fmt.Errorf("the answer is not the hold-outs json: %w", err)
	}
	out := map[string]string{}
	for _, pr := range answer.Holdouts {
		id, code := strings.TrimSpace(pr.ID), strings.TrimSpace(pr.TestCode)
		if id != "" && code != "" && len(code) <= maxHoldoutCode && !strings.Contains(code, heredocMarker) {
			out[id] = code
		}
	}
	return out, nil
}

const holdoutSystem = `You propose hold-out criteria for a software feature's spec: criteria its named proof does not cover, each with a Go test that fails on the repository's current main head and passes once the feature is built.
Rules:
- Propose at most the max named in the input; prefer none over a weak one.
- Each hold-out: an id (lowercase letters, digits, underscores), the criterion as one sentence in the spec's words, a test name (TestXxx), and the test's complete Go code (package holdout, standard library only, no network, no environment).
- The test must compile against the repository as it is on main and must fail there for the feature's absence.
- A hold-out may name one of the project's past failures (the from field cites it); a gap in the spec itself is drafted from the spec.
Answer one JSON object: {"holdouts":[{"id","criterion","test_name","test_code","from"}]}.`

const holdoutFixSystem = `These drafted Go tests could not run on the repository's main head; each carries its output. Rewrite each test so it compiles there and still fails for the feature's absence, keeping its id and its test name. Answer one JSON object: {"holdouts":[{"id","test_code"}]}.`

// DecideHoldout is the console's okay or drop of hold-out idx of round
// round of plan id, as a writer of its project: the round must be the
// newest (ErrStalePlan) and no confirmation answered at it or after
// (ErrAnswered), as a changed spec asks the confirmation again;
// ErrHoldoutClick names a click the row's state takes not, ErrNoHoldout
// a row the task does not hold.
func DecideHoldout(ctx context.Context, store Store, taskID string, round, idx int, verb, by string) error {
	if _, err := pendingPlan(ctx, store, taskID); err != nil {
		return err
	}
	newest, ok, err := store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return err
	}
	if !ok || newest.Revision != round {
		return ErrStalePlan{Newest: newest.Revision}
	}
	if confirmed, answered, err := store.PlanConfirmationAnswered(ctx, taskID); err != nil {
		return err
	} else if answered && confirmed >= round {
		return ErrAnswered
	}
	state := map[string]string{"okay": HoldoutStored, "drop": HoldoutDropped}[verb]
	if state == "" {
		return fmt.Errorf("%w: %s", ErrHoldoutClick, verb)
	}
	return store.DecidePlanHoldout(ctx, taskID, round, idx, state)
}

// holdoutAudit is what an audit session made of a red hold-out.
type holdoutAudit struct {
	Verdict  string
	Reason   string
	Rewrite  string
	Escalate string
}

// startAudit marks the audit of idx running on the task, which alone may
// take its verdict; endAudit clears it.
func (r *Runner) startAudit(taskID string, idx int) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.audits == nil {
		r.audits = map[string]int{}
	}
	r.audits[taskID] = idx
}

func (r *Runner) endAudit(taskID string) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	delete(r.audits, taskID)
}

// auditHoldout runs one audit session on the integration card's task: a
// report-mode sandbox of its own with a small brief, the criterion, the
// test and its output, the spec, one question; everything else by tool.
// It writes no code and holds no workspace; the verdict arrives through
// the file_verdict tool, and the row keeps it.
func (r *Runner) auditHoldout(ctx context.Context, t Task, repo Repo, pin int64, ho Holdout, evidence string) (holdoutAudit, error) {
	defer beatUntilDone(ctx, "holdout-audit")()
	p, err := r.filedPlan(ctx, t.Parent, t.ID)
	if err != nil {
		return holdoutAudit{}, err
	}
	r.startAudit(t.ID, ho.Idx)
	defer r.endAudit(t.ID)
	end, err := r.reportSession(ctx, t, repo, t.ModelAlias, t.Standing(), false, pin, auditGuide(p, ho, evidence), nil, nil)
	if err != nil {
		return holdoutAudit{}, err
	}
	if end.start != nil {
		return holdoutAudit{Escalate: end.start.escalation(thenAudit)}, nil
	}
	if end.escalate != "" {
		return holdoutAudit{Escalate: end.escalate}, nil
	}
	rows, err := r.store.PlanHoldouts(ctx, t.Parent)
	if err != nil {
		return holdoutAudit{}, err
	}
	for _, row := range rows {
		if row.Idx != ho.Idx {
			continue
		}
		if row.AuditVerdict == "" {
			why := cmp.Or(end.report, end.why)
			return holdoutAudit{Escalate: fmt.Sprintf("the audit session of hold-out %s ended without a verdict: %s. Read its words and settle the hold-out on the issue; an answer runs the gate again.",
				ho.ID, textutil.Truncate(strings.TrimSpace(why), 500))}, nil
		}
		return holdoutAudit{Verdict: row.AuditVerdict, Reason: row.AuditReason, Rewrite: row.Rewrite}, nil
	}
	return holdoutAudit{}, ErrNoHoldout
}

// auditGuide is the audit's whole brief: the criterion, the test and what
// it did at the gate, the spec, and the one question with its three
// answers.
func auditGuide(p Plan, ho Holdout, evidence string) string {
	var b strings.Builder
	b.WriteString("You audit one hold-out criterion of this card's plan. You change no code; read whatever else you need with your tools.\n\n")
	fmt.Fprintf(&b, "Hold-out idx %d (file_verdict names it): %s\n", ho.Idx, ho.Criterion)
	fmt.Fprintf(&b, "The test (dry-run on main head %s: %s):\n%s\n", ho.DryHead, ho.DryVerdict, fenced(ho.TestCode))
	fmt.Fprintf(&b, "At this card's gate it: %s\n", evidence)
	if p.Spec != nil {
		if o := strings.TrimSpace(p.Spec.Outcome); o != "" {
			fmt.Fprintf(&b, "\nThe spec's outcome: %s\n", o)
		}
		if len(p.Spec.Glossary) > 0 {
			fmt.Fprintf(&b, "Glossary: %s\n", strings.Join(p.Spec.Glossary, "; "))
		}
	}
	b.WriteString("\nIs this test the right one for the spec? Answer exactly one verdict with the file_verdict tool:\n")
	b.WriteString("- wrong_test: it does not test the spec's sentence. File it with no test_code to drop it, or with test_code: the rewrite's complete Go code (package holdout, standard library only), which must still fail on the base head, and a quote of the spec sentence it tests (quote, at least 20 characters).\n")
	b.WriteString("- real_gap: the feature truly misses the spec; the criterion becomes public and the builder works it.\n")
	b.WriteString("- spec_question: the spec itself cannot settle it; the reason is the question for the human.\n")
	return b.String()
}

// HoldoutVerdict is the audit session's filing of one hold-out's verdict
// through the gateway, as FilePlan is the planner's filing of its plan.
type HoldoutVerdict struct {
	TaskID   string `json:"task_id"`
	Idx      int    `json:"idx"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
	Quote    string `json:"quote"`
	TestCode string `json:"test_code"`
}

// FileVerdict stores an audit session's verdict on the hold-out it
// audited: only the audit of that idx running on that task may file (the
// in-flight marker), once (a second filing is ErrAnswered). A rewrite
// must quote the spec sentence it tests and keep its test's name; a real
// gap or a spec question files no code. Findings name what to correct;
// the agent files again.
func (r *Runner) FileVerdict(ctx context.Context, in HoldoutVerdict) ([]string, error) {
	t, _, err := r.task(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	if in.Verdict != AuditWrongTest && in.Verdict != AuditRealGap && in.Verdict != AuditSpecQuestion {
		return []string{"file_verdict: the verdict is one of wrong_test, real_gap, spec_question"}, nil
	}
	reason := strings.Join(strings.Fields(in.Reason), " ")
	if len(reason) > 2*maxHoldoutText {
		return []string{fmt.Sprintf("file_verdict: the reason is longer than %d characters", 2*maxHoldoutText)}, nil
	}
	if in.Verdict == AuditSpecQuestion && reason == "" {
		return []string{"file_verdict: a spec question carries its question as the reason"}, nil
	}
	rows, err := r.store.PlanHoldouts(ctx, t.Parent)
	if err != nil {
		return nil, err
	}
	var ho *Holdout
	for i := range rows {
		if rows[i].Idx == in.Idx {
			ho = &rows[i]
		}
	}
	if ho == nil || (ho.State != HoldoutStored && ho.State != HoldoutOpened) {
		return []string{"file_verdict: the card holds no such hold-out"}, nil
	}
	r.liveMu.Lock()
	live := r.audits[t.ID] == in.Idx
	r.liveMu.Unlock()
	if !live {
		return []string{"file_verdict: no audit of this hold-out is running"}, nil
	}
	rewrite := ""
	if in.Verdict == AuditWrongTest {
		rewrite = strings.TrimSpace(in.TestCode)
		if rewrite != "" {
			var findings []string
			if len(rewrite) > maxHoldoutCode {
				findings = append(findings, fmt.Sprintf("file_verdict: the rewrite is longer than %d characters", maxHoldoutCode))
			}
			if strings.Contains(rewrite, heredocMarker) {
				findings = append(findings, "file_verdict: the rewrite carries a marker the sandbox writes")
			}
			if !strings.Contains(rewrite, "func "+ho.TestName+"(") {
				findings = append(findings, fmt.Sprintf("file_verdict: the rewrite keeps its test's name %s", ho.TestName))
			}
			quote := strings.TrimSpace(in.Quote)
			if len(quote) < 20 {
				findings = append(findings, "file_verdict: the quote of the spec sentence is at least 20 characters")
			} else {
				p, err := r.filedPlan(ctx, t.Parent, t.ID)
				if err != nil {
					return nil, err
				}
				spec := ""
				if p.Spec != nil {
					spec = p.Spec.Outcome + "\n" + strings.Join(p.Spec.Glossary, "\n")
				}
				if !strings.Contains(spec, quote) {
					findings = append(findings, "file_verdict: the quote is not the spec's words; quote the sentence of the outcome or the glossary the rewrite tests")
				}
			}
			if len(findings) > 0 {
				return findings, nil
			}
		}
	} else if strings.TrimSpace(in.TestCode) != "" || strings.TrimSpace(in.Quote) != "" {
		return []string{fmt.Sprintf("file_verdict: a %s files no test and no quote", in.Verdict)}, nil
	}
	if err := r.store.SetHoldoutAudit(ctx, t.Parent, in.Idx, in.Verdict, reason, rewrite); err != nil {
		return nil, err
	}
	return nil, nil
}

// AuditVerdictHandler is the gateway's way in to FileVerdict, as
// PlanFileHandler is its way in to FilePlan: a filed verdict answers
// filed true, findings answer what the agent corrects.
func (r *Runner) AuditVerdictHandler(w http.ResponseWriter, req *http.Request) {
	var in HoldoutVerdict
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "holdout verdict: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.TaskID == "" || in.Idx == 0 {
		http.Error(w, "holdout verdict: task_id and idx are required", http.StatusBadRequest)
		return
	}
	findings, err := r.FileVerdict(req.Context(), in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Filed    bool     `json:"filed"`
		Findings []string `json:"findings"`
	}{len(findings) == 0, findings}) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
}

// HoldoutGateResult is one hold-out the criteria gate reports red or
// unrunnable at the card's head, for the settlement; its evidence is one
// line of the run.
type HoldoutGateResult struct {
	Idx      int    `json:"idx"`
	ID       string `json:"id"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

// HoldoutSettlement is what settling the gate's red hold-outs came to:
// Ask an escalation for the human (a spec question, a rewrite that bends,
// a hold-out that stayed red after its criterion was opened, an audit
// without a verdict) whose answer runs the gate again; Findings route
// the card back to a session; else the hold-outs are settled and the
// gate runs again.
type HoldoutSettlement struct {
	Ask      string   `json:",omitempty"`
	Findings []string `json:",omitempty"`
	Settled  bool
}

// SettleHoldouts settles the hold-outs the criteria gate reported red or
// unrunnable on the integration card: each goes to one audit session,
// whose verdict drops the test or rewrites it (a rewrite dry-runs on the
// base head first: green there is bending, and the hold-out keeps its
// own test), opens the criterion as a real gap, or asks the human the
// spec question. A hold-out the audit opened that stays red asks the
// human. Every red and every audit verdict lands in the recording rule's
// ledger. One activity: the store, the sandbox and the session all run
// here, so the workflow stays deterministic.
func (r *Runner) SettleHoldouts(ctx context.Context, taskID string, pin int64, pr int64, commit string, reds []HoldoutGateResult) (HoldoutSettlement, error) {
	// the audit sessions and the dry runs take minutes
	defer beatUntilDone(ctx, "holdouts")()
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return HoldoutSettlement{}, err
	}
	rows, err := r.store.PlanHoldouts(ctx, t.Parent)
	if err != nil {
		return HoldoutSettlement{}, err
	}
	byIdx := make(map[int]Holdout, len(rows))
	for _, h := range rows {
		byIdx[h.Idx] = h
	}
	var findings []string
	for _, red := range reds {
		ho, holds := byIdx[red.Idx]
		if !holds {
			continue // decided away while the gate ran
		}
		if ho.State == HoldoutOpened {
			// red again after the audit opened it: the human decides, and
			// the cycle lands in the ledger
			if _, err := r.store.BumpHoldoutRed(ctx, t.Parent, ho.Idx); err != nil {
				return HoldoutSettlement{}, err
			}
			if _, err := r.store.PutPlanCatches(ctx, t.Parent, ho.Round, CatchHoldoutRed, FinderGate,
				[]string{fmt.Sprintf("hold-out %s red at the gate again: %s", ho.ID, red.Evidence)}); err != nil {
				return HoldoutSettlement{}, err
			}
			return HoldoutSettlement{Ask: fmt.Sprintf("hold-out %s (%s) stayed red after its criterion was opened: %s. Close the card, or answer on the issue; an answer runs the gate again.", ho.ID, ho.Criterion, red.Evidence)}, nil
		}
		if ho.State != HoldoutStored {
			continue // dropped or waiting: not the gate's to settle
		}
		if _, err := r.store.PutPlanCatches(ctx, t.Parent, ho.Round, CatchHoldoutRed, FinderGate,
			[]string{fmt.Sprintf("hold-out %s %s at the gate: %s", ho.ID, red.Verdict, red.Evidence)}); err != nil {
			return HoldoutSettlement{}, err
		}
		res, err := r.auditHoldout(ctx, t, repo, pin, ho, red.Evidence)
		if err != nil {
			return HoldoutSettlement{}, err
		}
		if res.Escalate != "" {
			return HoldoutSettlement{Ask: res.Escalate}, nil
		}
		switch res.Verdict {
		case AuditWrongTest:
			if res.Rewrite == "" {
				continue // dropped: the row keeps the verdict
			}
			// the bending guard: the rewrite must fail on the base head,
			// the head the hold-out's own draft ran on
			rw := ho
			rw.TestCode = res.Rewrite
			dry, ev, err := r.rewriteOnBase(ctx, t, repo, pr, commit, rw)
			var f *startFailure
			if errors.As(err, &f) {
				return HoldoutSettlement{Ask: f.escalation(thenCriteria)}, nil
			}
			if err != nil {
				return HoldoutSettlement{}, err
			}
			if dry != "red" {
				// bending: the rewrite is refused, the hold-out keeps its
				// own test, and the audit fields clear for the next audit
				if err := r.store.ClearHoldoutAudit(ctx, t.Parent, ho.Idx); err != nil {
					return HoldoutSettlement{}, err
				}
				return HoldoutSettlement{Ask: fmt.Sprintf("the audit's rewrite of hold-out %s %s on the base head (%s), so it tests nothing: kept out, the hold-out keeps its own test. Okay runs the gate again; request changes sends the audit's words to the builder, or close the card.", ho.ID, dry, ev)}, nil
			}
			if err := r.store.SetHoldoutTest(ctx, t.Parent, ho.Idx, res.Rewrite, dry, ev); err != nil {
				return HoldoutSettlement{}, err
			}
		case AuditRealGap:
			if _, err := r.store.OpenHoldout(ctx, t.Parent, ho.Idx); err != nil {
				return HoldoutSettlement{}, err
			}
			detail := fmt.Sprintf("hold-out %s: the audit found a real gap: %s", ho.ID, res.Reason)
			if _, err := r.store.PutPlanCatches(ctx, t.Parent, ho.Round, CatchAudit, FinderAuditor, []string{detail}); err != nil {
				return HoldoutSettlement{}, err
			}
			findings = append(findings, fmt.Sprintf("%s (hold-out %s): the audit found a real gap the spec names and the plan's proof does not cover; work the criterion.", ho.Criterion, ho.ID))
		case AuditSpecQuestion:
			return HoldoutSettlement{Ask: fmt.Sprintf("hold-out %s asks a question the spec cannot settle: %s. Answer on the issue; a comment starts a new planner round, an answer here runs the gate again.", ho.ID, res.Reason)}, nil
		}
	}
	if len(findings) > 0 {
		return HoldoutSettlement{Findings: findings}, nil
	}
	return HoldoutSettlement{Settled: true}, nil
}

// rewriteOnBase dry-runs the audit's rewrite at the base head the
// hold-out's draft ran on, inside the pull request's history: red is a
// rewrite that holds, anything else bends.
func (r *Runner) rewriteOnBase(ctx context.Context, t Task, repo Repo, pr int64, commit string, ho Holdout) (string, string, error) {
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		return "", "", err
	}
	defer done()
	run := t
	run.ReproCommand = holdoutScript([]Holdout{ho})
	rep, err := r.runRepro(ctx, run, repo, pullRef(pr), commit, ho.DryHead)
	if err != nil {
		return "", "", err
	}
	m := parseHoldoutMarkers(rep.Output)
	dry, ev := holdoutDryVerdict(m[ho.Idx], holdoutRan(m, ho.Idx))
	return dry, ev, nil
}

// PutPlanHoldouts stores round's hold-out rows, the proposals with their
// dry-run verdicts: false when the round holds rows already, so an
// activity retry adds nothing.
func (s *PGStore) PutPlanHoldouts(ctx context.Context, id string, round int, drafts []Holdout) (bool, error) {
	b, err := json.Marshal(drafts)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO plan_holdouts (task_id, idx, round, id, origin, criterion, state, test_name, test_code, dry_head, dry_verdict, dry_output)
		SELECT $1, (h->>'idx')::int, $2, h->>'id', h->>'origin', h->>'criterion', h->>'state', h->>'test_name', h->>'test_code', h->>'dry_head', h->>'dry_verdict', h->>'dry_output'
		FROM jsonb_array_elements($3::jsonb) h
		WHERE NOT EXISTS (SELECT 1 FROM plan_holdouts WHERE task_id = $1 AND round = $2)
		ON CONFLICT DO NOTHING`, id, round, b)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PlanHoldouts are task id's hold-out rows in idx order, every round's.
func (s *PGStore) PlanHoldouts(ctx context.Context, id string) ([]Holdout, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, round, id, origin, criterion, state, test_name, test_code,
		dry_head, dry_verdict, dry_output, audit_verdict, audit_reason, rewrite, red_cycles FROM plan_holdouts
		WHERE task_id = $1 ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Holdout
	for rows.Next() {
		var h Holdout
		if err := rows.Scan(&h.Idx, &h.Round, &h.ID, &h.Origin, &h.Criterion, &h.State, &h.TestName, &h.TestCode,
			&h.DryHead, &h.DryVerdict, &h.DryOutput, &h.AuditVerdict, &h.AuditReason, &h.Rewrite, &h.RedCycles); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UndecidedHoldouts is how many of round's hold-outs wait for the
// human's okay or drop.
func (s *PGStore) UndecidedHoldouts(ctx context.Context, id string, round int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plan_holdouts
		WHERE task_id = $1 AND round = $2 AND state IN ('proposed', 'unrunnable')`, id, round).Scan(&n)
	return n, err
}

// DecidePlanHoldout stores the human's click on hold-out idx of round as
// its new state (stored or dropped): a row decided already, or a click
// its state takes not, is ErrHoldoutClick; a row the task does not hold
// is ErrNoHoldout.
func (s *PGStore) DecidePlanHoldout(ctx context.Context, id string, round, idx int, state string) error {
	var decided, holds bool
	err := s.db.QueryRowContext(ctx, `WITH done AS (UPDATE plan_holdouts SET state = $4, decided_at = now()
			WHERE task_id = $1 AND round = $2 AND idx = $3
				AND (($4 = 'dropped' AND state IN ('proposed', 'unrunnable'))
					OR ($4 = 'stored' AND state = 'proposed' AND dry_verdict IN ('red', 'green')))
			RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM done), EXISTS (SELECT 1 FROM plan_holdouts WHERE task_id = $1 AND idx = $3)`,
		id, round, idx, state).Scan(&decided, &holds)
	switch {
	case err != nil:
		return err
	case decided:
		return nil
	case holds:
		return ErrHoldoutClick
	default:
		return ErrNoHoldout
	}
}

// SweepPlanHoldouts drops every undecided hold-out of task id: the round
// closed (its confirmation answered, or a newer round asked), and a
// proposal no click can reach is not a hold-out.
func (s *PGStore) SweepPlanHoldouts(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plan_holdouts SET state = 'dropped', decided_at = now()
		WHERE task_id = $1 AND state IN ('proposed', 'unrunnable')`, id)
	return err
}

// SetHoldoutAudit keeps an audit's verdict on hold-out idx: a verdict
// there already is ErrAnswered, a row the task does not hold
// ErrNoHoldout. A wrong test without a rewrite drops the hold-out with
// its verdict.
func (s *PGStore) SetHoldoutAudit(ctx context.Context, id string, idx int, verdict, reason, rewrite string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plan_holdouts SET audit_verdict = $3, audit_reason = $4, rewrite = $5,
		state = CASE WHEN $3 = 'wrong_test' AND $5 = '' THEN 'dropped' ELSE state END,
		decided_at = CASE WHEN $3 = 'wrong_test' AND $5 = '' THEN now() ELSE decided_at END
		WHERE task_id = $1 AND idx = $2 AND audit_verdict = '' AND state IN ('stored', 'opened')`,
		id, idx, verdict, reason, rewrite)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	switch {
	case err != nil:
		return err
	case n > 0:
		return nil
	}
	var holds bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM plan_holdouts WHERE task_id = $1 AND idx = $2)`, id, idx).Scan(&holds); err != nil {
		return err
	}
	if holds {
		return ErrAnswered
	}
	return ErrNoHoldout
}

// ClearHoldoutAudit empties the audit fields of hold-out idx: a rewrite
// the bending guard refused, so the next audit files anew.
func (s *PGStore) ClearHoldoutAudit(ctx context.Context, id string, idx int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plan_holdouts SET audit_verdict = '', audit_reason = '', rewrite = ''
		WHERE task_id = $1 AND idx = $2`, id, idx)
	return err
}

// SetHoldoutTest replaces hold-out idx's test with an audit's rewrite
// that failed on the base head, and clears the audit fields: the gate
// runs the rewrite, a later red audits anew.
func (s *PGStore) SetHoldoutTest(ctx context.Context, id string, idx int, code, verdict, output string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plan_holdouts SET test_code = $3, dry_verdict = $4, dry_output = $5,
		audit_verdict = '', audit_reason = '', rewrite = '' WHERE task_id = $1 AND idx = $2`,
		id, idx, code, verdict, output)
	return err
}

// BumpHoldoutRed counts hold-out idx's reds at the gate after the audit
// opened it.
func (s *PGStore) BumpHoldoutRed(ctx context.Context, id string, idx int) (int, error) {
	var cycles int
	err := s.db.QueryRowContext(ctx, `UPDATE plan_holdouts SET red_cycles = red_cycles + 1
		WHERE task_id = $1 AND idx = $2 RETURNING red_cycles`, id, idx).Scan(&cycles)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoHoldout
	}
	return cycles, err
}

// OpenHoldout publishes hold-out idx's criterion: the audit found a real
// gap, and the builder works it.
func (s *PGStore) OpenHoldout(ctx context.Context, id string, idx int) (string, error) {
	var hid string
	err := s.db.QueryRowContext(ctx, `UPDATE plan_holdouts SET state = 'opened'
		WHERE task_id = $1 AND idx = $2 AND state IN ('stored', 'opened') RETURNING id`, id, idx).Scan(&hid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoHoldout
	}
	return hid, err
}

// PastReverts are the project's last revert objectives, one line each:
// the material a hold-out draft may cite, so the project's own failures
// shape its proofs.
func (s *PGStore) PastReverts(ctx context.Context, repository string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT objective FROM tasks
		WHERE repository = $1 AND source = 'revert' ORDER BY created_at DESC LIMIT 3`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var objective string
		if err := rows.Scan(&objective); err != nil {
			return nil, err
		}
		if line, _, _ := strings.Cut(objective, "\n"); strings.TrimSpace(line) != "" {
			out = append(out, textutil.Truncate(strings.TrimSpace(line), maxHoldoutText))
		}
	}
	return out, rows.Err()
}
