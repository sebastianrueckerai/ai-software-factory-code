package factoryd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The criteria gate (the spec-intake concept, piece 4): on the plan's
// integration card, at the approval, it runs the plan's test and invariant
// criteria once against the head, writes each one's gate verdict row and
// reports every criterion not met as a finding on the approval card.
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

// The house format of the criteria's test fields, as the gate reads them:
// "TestXxx in <package>" (one test) and "go test <suite> passes" (an
// invariant: every test of the suite).
var (
	criterionTestField      = regexp.MustCompile(`^(Test[A-Za-z0-9_]*) in ([A-Za-z0-9_./-]+)$`)
	criterionInvariantField = regexp.MustCompile(`^go test ([A-Za-z0-9_./-]+) passes$`)
)

// maxProofMessage caps a proof's message on the timeline.
const maxProofMessage = 4 << 10

// criterionRun is one test or invariant criterion of the plan: kind test
// names its test and its package, kind invariant its suite; refused is the
// finding on a field off the house format, which never runs.
type criterionRun struct {
	id, kind  string
	test, pkg string
	suite     string
	refused   string
}

// criteriaRuns are the plan's test and invariant criteria, in plan order;
// judgement and human criteria are outside the gate. What cannot be
// parsed is not executed.
func criteriaRuns(p Plan) []criterionRun {
	var runs []criterionRun
	for _, c := range p.Criteria {
		run := criterionRun{id: c.ID, kind: c.Kind}
		switch c.Kind {
		case CriterionTest:
			if m := criterionTestField.FindStringSubmatch(c.Test); m != nil {
				run.test, run.pkg = m[1], goPackage(m[2])
			} else {
				run.refused = fmt.Sprintf("criterion %s: test %q is not TestXxx in <package>", c.ID, c.Test)
			}
		case CriterionInvariant:
			if m := criterionInvariantField.FindStringSubmatch(c.Test); m != nil {
				run.suite = goPackage(m[1])
			} else {
				run.refused = fmt.Sprintf("criterion %s: suite %q is not go test <suite> passes", c.ID, c.Test)
			}
		default:
			continue
		}
		runs = append(runs, run)
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
	if c.kind == CriterionInvariant {
		return harness.Proof{ID: c.id, Suite: c.suite}
	}
	return harness.Proof{ID: c.id, Suite: c.pkg, Name: c.test}
}

// gateProof is one criterion's run as the gate_criteria timeline line records it.
type gateProof struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Suite   string `json:"suite,omitempty"`
	Name    string `json:"name,omitempty"`
	Outcome string `json:"outcome"` // a proof* constant; "" for a refused criterion
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
	for !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// criteriaReport judges the runs by their outcomes: findings for the
// approval card, lines for the comment, entries for the timeline. A run
// without an outcome reported nothing: a gate that runs and reports
// nothing does not pass.
func criteriaReport(runs []criterionRun, outcomes map[string]proofOutcome) (findings, lines []string, proofs []gateProof) {
	proofs = make([]gateProof, 0, len(runs))
	for _, r := range runs {
		p := r.proof()
		g := gateProof{ID: r.id, Kind: r.kind, Suite: p.Suite, Name: p.Name, Line: r.refused}
		if r.refused == "" {
			o, ok := outcomes[r.id]
			if !ok {
				o = proofOutcome{proofNoReport, "the run reported nothing"}
			}
			g.Outcome, g.Met, g.Line, g.Message = o.Outcome, o.Outcome == proofPassed, criterionLine(r, o), textutil.Truncate(o.Message, maxProofMessage)
		}
		lines = append(lines, g.Line)
		if !g.Met {
			findings = append(findings, g.Line)
		}
		proofs = append(proofs, g)
	}
	return findings, lines, proofs
}

// criterionLine is a run's line for the comment and the card.
func criterionLine(r criterionRun, o proofOutcome) string {
	what := r.test + " in " + r.pkg
	if r.kind == CriterionInvariant {
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
		if r.kind == CriterionInvariant {
			s = what + " ran no tests"
		}
	case proofSkipped:
		s += " skipped"
		if r.kind == CriterionInvariant {
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
// fresh sandbox with no agent: one runner pod runs every criterion and
// every stored hold-out through the tree's check test. Not the plan's
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
	runs := criteriaRuns(p)
	// the plan's stored hold-outs run here too, after the criteria, so no
	// criterion's run sees a hold-out's file: their outcomes never reach
	// the card's findings, the comment or a verdict row (blind by routing
	// and storage), they go to the settlement as Holdouts
	hos := r.gateHoldouts(ctx, t.Parent)
	var entries []harness.Proof
	for _, c := range runs {
		if c.refused == "" {
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
		if outcomes, err = r.runProofs(ctx, t, repo, pullRef(pr), commit, "", entries); err != nil {
			return cannotStart(err)
		}
	}
	findings, lines, proofs := criteriaReport(runs, outcomes)
	met := len(lines) - len(findings)
	if len(p.Criteria) == 0 {
		findings = append(findings, "criteria gate: the plan names no criteria")
	}
	var holdouts []HoldoutGateResult
	holdoutEntries := make([]map[string]any, 0, len(hos))
	for _, h := range hos {
		dry, ev := holdoutDryVerdict(outcomes[holdoutEntry(h.Idx)])
		holdoutEntries = append(holdoutEntries, map[string]any{"id": h.ID, "verdict": dry})
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
		// nothing ran: an empty plan, or every runnable criterion off the
		// format
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
