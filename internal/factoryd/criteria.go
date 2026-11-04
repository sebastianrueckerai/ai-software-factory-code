package factoryd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The criteria gate (the spec-intake concept, piece 4): on the plan's
// integration card, at the approval, it runs the plan's invariant
// criteria once against the head, writes each one's gate verdict row and
// reports every one not met as a finding on the approval card. A test
// criterion has no stored proof yet: its row is unmet, and no finding.
// Judgement and human criteria are outside the gate: their proof is the
// review's verdict or the approver's decision.

// CriteriaResult is GateCriteria's verdict: Skipped names why the gate did
// not run (no plan filed, or the card is not the plan's integration
// card); Findings are the criteria not met, for the approval card;
// Escalate is the start escalation of a sandbox that could not start,
// whose answer runs the gate again.
type CriteriaResult struct {
	Skipped  string   `json:",omitempty"`
	Findings []string `json:",omitempty"`
	Escalate string   `json:",omitempty"`
	// Holdouts are the plan's hold-outs that ran red or unrunnable at
	// this head, for the settlement activity; the card and the comment
	// never see them.
	Holdouts []HoldoutGateResult `json:",omitempty"`
}

// maxProofMessage caps a proof's message on the timeline.
const maxProofMessage = 4 << 10

// criterionRun is one run of the gate for a test or invariant criterion
// of the plan.
type criterionRun struct {
	id, kind    string
	suite, name string             // name "" runs the whole suite
	file        *harness.ProofFile // a stored proof's file, written into the tree first
	held        string             // the line of a criterion the gate cannot run: an unmet row, no finding
}

// gateRuns are the runs the gate makes for p's criteria, in plan order:
// an invariant runs its suite through goPackage; a test criterion is held.
func gateRuns(p Plan) []criterionRun {
	var runs []criterionRun
	for _, c := range p.Criteria {
		switch c.Kind {
		case CriterionInvariant:
			runs = append(runs, criterionRun{id: c.ID, kind: c.Kind, suite: goPackage(c.Suite)})
		case CriterionTest:
			runs = append(runs, criterionRun{id: c.ID, kind: c.Kind, held: "criterion " + c.ID + ": no stored proof yet"})
		}
	}
	return runs
}

// goPackage reads a criterion's package as a directory of the repository:
// go test reads a bare path as a standard library package, and a leading
// dash as a flag.
func goPackage(p string) string {
	first, _, _ := strings.Cut(p, "/")
	if strings.HasPrefix(p, "-") || (first != "" && !strings.Contains(first, ".")) {
		return "./" + p
	}
	return p
}

// proof is the run's entry in the runner's list.
func (c criterionRun) proof() harness.Proof {
	return harness.Proof{ID: c.id, Suite: c.suite, Name: c.name, File: c.file}
}

// gateProof is one criterion's run as the gate_criteria timeline line records it.
type gateProof struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Suite   string `json:"suite,omitempty"`
	Name    string `json:"name,omitempty"`
	Outcome string `json:"outcome"` // a proof* constant; "" for a held criterion
	Met     bool   `json:"met"`
	Line    string `json:"line"`
	Message string `json:"message,omitempty"` // at most maxProofMessage
}

// shortenEvidence keeps a proof's message readable on a card: one line,
// at most 200 bytes.
func shortenEvidence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 200 {
		return s
	}
	n := 200
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// criteriaReport judges the runs by their outcomes: findings for the
// approval card, lines for the comment, entries for the timeline. A run
// without an outcome reported nothing: a gate that runs and reports
// nothing does not pass. A held run is its line alone, unmet and no
// finding: the builder cannot act on it.
func criteriaReport(runs []criterionRun, outcomes map[string]proofOutcome) (findings, lines []string, proofs []gateProof) {
	proofs = make([]gateProof, 0, len(runs))
	for _, r := range runs {
		g := gateProof{ID: r.id, Kind: r.kind, Suite: r.suite, Name: r.name, Line: r.held}
		if r.held == "" {
			o, ok := outcomes[r.id]
			if !ok {
				o = proofOutcome{proofNoReport, "the run reported nothing"}
			}
			g.Outcome, g.Met, g.Line, g.Message = o.Outcome, o.Outcome == proofPassed, criterionLine(r, o), textutil.Truncate(o.Message, maxProofMessage)
			if !g.Met {
				findings = append(findings, g.Line)
			}
		}
		lines = append(lines, g.Line)
		proofs = append(proofs, g)
	}
	return findings, lines, proofs
}

// criterionLine is a run's line for the comment and the card.
func criterionLine(r criterionRun, o proofOutcome) string {
	what := r.name + " in " + r.suite
	if r.name == "" {
		what = "suite " + r.suite
	}
	s := what
	switch o.Outcome {
	case proofPassed:
		s += " passed"
	case proofFailed:
		s += " failed"
	case proofMissing:
		s = "no test " + what
		if r.name == "" {
			s = what + " ran no tests"
		}
	case proofSkipped:
		s += " skipped"
		if r.name == "" {
			s += " every test"
		}
	}
	switch ev := shortenEvidence(o.Message); {
	case ev == "":
	case o.Outcome == proofPassed:
		s += " (" + ev + ")"
	default:
		s += ": " + ev
	}
	return "criterion " + r.id + ": " + s
}

// gateRows are the gate's verdict rows: met for a criterion that passed,
// unmet for any other, its line the evidence.
func gateRows(proofs []gateProof) []CriterionVerdict {
	rows := make([]CriterionVerdict, 0, len(proofs))
	for _, g := range proofs {
		verdict := "unmet"
		if g.Met {
			verdict = "met"
		}
		rows = append(rows, CriterionVerdict{Criterion: g.ID, Verdict: verdict, Evidence: g.Line, Kind: g.Kind})
	}
	return rows
}

// GateCriteria runs the plan's criteria against the pull request head in a
// fresh sandbox with no agent: one runner pod runs every gate run that is
// not held and every stored hold-out through the tree's check test. Not the plan's
// integration card: not run, the timeline says why. A plan that names no
// criteria names no proof: that is a finding, for the human. The
// namespace and the token go when it ends, however it ends.
func (r *Runner) GateCriteria(ctx context.Context, taskID string, pr int64, commit string) (CriteriaResult, error) {
	// the namespace wait, the image pull and the tests take minutes
	defer beatUntilDone(ctx, "criteria")()
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return CriteriaResult{}, err
	}
	if t.Source != SourcePlan || t.Parent == "" {
		r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "skipped": "no plan filed"}))
		return CriteriaResult{Skipped: "no plan filed"}, nil
	}
	p, err := r.filedPlan(ctx, t.Parent, t.ID)
	if err != nil {
		return CriteriaResult{}, err
	}
	role := RoleFeature
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == t.PlanKey }); i >= 0 {
		role = p.Cards[i].Role
	}
	if role != RoleIntegration {
		r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "skipped": "not the plan's integration card"}))
		return CriteriaResult{Skipped: "not the plan's integration card"}, nil
	}
	runs := gateRuns(p)
	// the plan's stored hold-outs run here too, after the criteria, so no
	// criterion's run sees a hold-out's file: their outcomes never reach
	// the card's findings, the comment or a verdict row (blind by routing
	// and storage), they go to the settlement as Holdouts
	hos := r.gateHoldouts(ctx, t.Parent)
	var entries []harness.Proof
	for _, c := range runs {
		if c.held == "" {
			entries = append(entries, c.proof())
		}
	}
	for _, h := range hos {
		entries = append(entries, holdoutProof(h))
	}
	var outcomes map[string]proofOutcome
	if len(entries) > 0 {
		// a sandbox that cannot start is the start escalation, as a
		// reproduction's is
		cannotStart := func(err error) (CriteriaResult, error) {
			var f *startFailure
			if !errors.As(err, &f) {
				return CriteriaResult{}, err
			}
			r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "start_failed": f.text}))
			return CriteriaResult{Escalate: f.escalation(thenCriteria)}, nil
		}
		done, err := r.reproSandbox(ctx, &t, repo)
		if err != nil {
			return cannotStart(err)
		}
		defer done()
		outcomes, err = r.runProofs(ctx, t, repo, pullRef(pr), commit, "", entries)
		// a pod that ended early leaves the criteria it did not reach
		// unmet; a hold-out it did not reach is no verdict, so the
		// activity fails and runs again
		var ended *proofsEnded
		if errors.As(err, &ended) && !slices.ContainsFunc(hos, func(h Holdout) bool { return slices.Contains(ended.unreached, holdoutEntry(h.Idx)) }) {
			err = nil
		}
		if err != nil {
			return cannotStart(err)
		}
	}
	findings, lines, proofs := criteriaReport(runs, outcomes)
	met := 0
	for _, g := range proofs {
		if g.Met {
			met++
		}
	}
	if len(p.Criteria) == 0 {
		findings = append(findings, "criteria gate: the plan names no criteria")
	}
	var holdouts []HoldoutGateResult
	holdoutEntries := make([]map[string]any, 0, len(hos))
	for _, h := range hos {
		dry, ev := holdoutDryVerdict(outcomes[holdoutEntry(h.Idx)])
		holdoutEntries = append(holdoutEntries, map[string]any{"id": holdoutEntry(h.Idx), "verdict": dry})
		if dry == "red" || dry == HoldoutUnrunnable {
			holdouts = append(holdouts, HoldoutGateResult{Idx: h.Idx, ID: h.ID, Verdict: dry, Evidence: ev})
		}
	}
	if err := r.store.PutCriteriaVerdicts(ctx, t.ID, CriteriaSourceGate, FactoryDecider, gateRows(proofs)); err != nil {
		return CriteriaResult{}, err
	}
	switch {
	case len(entries) > 0:
		r.commentOnce(ctx, t, repo, pr, r.untrusted(t, criteriaComment(commit, met, lines)))
	case len(findings) > 0:
		// nothing ran: an empty plan
		r.commentOnce(ctx, t, repo, pr, r.untrusted(t, fmt.Sprintf("factoryd criteria gate on %s: no criterion ran.\n\n%s",
			commit, fenced(strings.Join(findings, "\n")))))
	}
	r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit,
		"proofs": proofs, "holdouts": holdoutEntries, "findings": findings}))
	return CriteriaResult{Findings: findings, Holdouts: holdouts}, nil
}

// criteriaComment is the gate's comment on the pull request: the head, how
// many criteria it met, and one line per criterion.
func criteriaComment(commit string, met int, lines []string) string {
	return fmt.Sprintf("factoryd criteria gate on %s: %d of %d criteria met (judgement and human criteria ride the review and the approval).\n\n%s",
		commit, met, len(lines), fenced(strings.Join(lines, "\n")))
}
