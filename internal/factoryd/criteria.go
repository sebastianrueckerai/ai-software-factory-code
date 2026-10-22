package factoryd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The criteria gate (the spec-intake concept, piece 4): on the plan's
// integration card, at the approval, it runs the plan's test and invariant
// criteria once against the head and reports every criterion not met as a
// finding on the approval card. Judgement and human criteria are outside
// the gate: their proof is the review's verdict or the approver's
// decision.

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
// "TestXxx in <package>" (a test bash check runs) and "go test <suite>
// passes" (an invariant). The charsets admit no shell metacharacter, so
// the fields quoted into the sandbox script cannot leave their argument.
var (
	criterionTestField      = regexp.MustCompile(`^(Test[A-Za-z0-9_]*) in ([A-Za-z0-9_./-]+)$`)
	criterionInvariantField = regexp.MustCompile(`^go test ([A-Za-z0-9_./-]+) passes$`)
	// criterionMarker reads a marker line the script prints, one per
	// criterion: its exit, run and fail counts and one line of evidence.
	// The fields parse positionally, so a forged suffix in the evidence
	// cannot flip them.
	criterionMarker = regexp.MustCompile(`^__criterion ([a-z][a-z0-9_]{1,31}) exit=(-?[0-9]+) ran=([0-9]+) fail=([0-9]+)(?: (.*))?$`)
)

// criterionRun is one criterion of the plan the gate runs in the sandbox:
// kind test names its test and its package, kind invariant its suite.
type criterionRun struct {
	id        string
	test, pkg string
	suite     string
}

// criteriaRuns parses the plan's test and invariant criteria into runs;
// judgement and human criteria are outside the gate. A test field off the
// house format is a finding, never a run: what cannot be parsed is not
// executed.
func criteriaRuns(p Plan) (runs []criterionRun, findings []string) {
	for _, c := range p.Criteria {
		switch c.Kind {
		case CriterionTest:
			if m := criterionTestField.FindStringSubmatch(c.Test); m != nil {
				runs = append(runs, criterionRun{id: c.ID, test: m[1], pkg: m[2]})
				continue
			}
			findings = append(findings, fmt.Sprintf("criterion %s: test %q is not TestXxx in <package>", c.ID, c.Test))
		case CriterionInvariant:
			if m := criterionInvariantField.FindStringSubmatch(c.Test); m != nil {
				runs = append(runs, criterionRun{id: c.ID, suite: m[1]})
				continue
			}
			findings = append(findings, fmt.Sprintf("criterion %s: suite %q is not go test <suite> passes", c.ID, c.Test))
		}
	}
	return runs, findings
}

// criteriaScript is the sandbox run: one go test per criterion, in the
// plan's order, every output to a file of its own, so the script's stdout
// is the marker lines alone and the tail keeps them whole. The test kind
// runs with -v: its "=== RUN" lines count the named test ("=== RUN" and
// three spaces, a subtest's name carries a slash); the fail count greps
// the test's own "--- FAIL" line, the evidence the first failed test's.
func criteriaScript(runs []criterionRun) string {
	var b strings.Builder
	for i, r := range runs {
		fmt.Fprintf(&b, "o=/tmp/k%d\n", i+1)
		if r.suite != "" {
			fmt.Fprintf(&b, "go test %s >\"$o\" 2>&1\n", r.suite)
		} else {
			fmt.Fprintf(&b, "go test -v -run '^%s$' %s >\"$o\" 2>&1\n", r.test, r.pkg)
		}
		b.WriteString("rc=$?\n")
		if r.suite != "" {
			b.WriteString("ran=$(grep -c '^ok' \"$o\")\nfail=$(grep -c '^FAIL' \"$o\")\n")
		} else {
			fmt.Fprintf(&b, "ran=$(grep -c '^=== RUN   %s$' \"$o\")\nfail=$(grep -c '^--- FAIL: %s ' \"$o\")\n", r.test, r.test)
		}
		b.WriteString("ev=$(grep -m1 '^--- FAIL: ' \"$o\")\n[ -n \"$ev\" ] || ev=$(head -n 1 \"$o\")\n")
		fmt.Fprintf(&b, "printf '__criterion %s exit=%%s ran=%%s fail=%%s %%s\\n' \"$rc\" \"$ran\" \"$fail\" \"$ev\"\n", r.id)
	}
	return b.String()
}

// criterionVerdict is one criterion's counts in its marker line.
type criterionVerdict struct {
	exit, ranCount, failCount int
	evidence                  string
}

// parseCriteriaMarkers reads the run's marker lines from the output tail.
// The last marker of a criterion wins; a line that matches no marker is
// test output the script never wrote to stdout.
func parseCriteriaMarkers(output string) map[string]criterionVerdict {
	out := map[string]criterionVerdict{}
	for _, line := range strings.Split(output, "\n") {
		m := criterionMarker.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		exit, _ := strconv.Atoi(m[2])
		ran, _ := strconv.Atoi(m[3])
		fail, _ := strconv.Atoi(m[4])
		out[m[1]] = criterionVerdict{exit: exit, ranCount: ran, failCount: fail, evidence: m[5]}
	}
	return out
}

// shortenEvidence keeps a criterion's evidence line readable on a card.
func shortenEvidence(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// criteriaReport judges the runs against their markers: findings for the
// approval card, lines for the comment, entries for the timeline. A
// criterion whose marker the tail lost is "no verdict from the run": a
// gate that runs and reports nothing does not pass.
func criteriaReport(runs []criterionRun, markers map[string]criterionVerdict) (findings, lines []string, entries []map[string]any) {
	for _, r := range runs {
		v, ok := markers[r.id]
		ev := shortenEvidence(v.evidence)
		line, verdict := "", "passed"
		switch {
		case !ok:
			verdict = "no verdict"
			line = fmt.Sprintf("criterion %s: no verdict from the run", r.id)
		case r.suite != "" && v.exit != 0:
			verdict = "failed"
			line = fmt.Sprintf("criterion %s: suite %s failed: %s", r.id, r.suite, ev)
		case r.suite != "" && v.ranCount == 0:
			verdict = "no tests"
			line = fmt.Sprintf("criterion %s: suite %s ran no tests", r.id, r.suite)
		case r.suite != "":
			line = fmt.Sprintf("criterion %s: suite %s passed", r.id, r.suite)
		case v.ranCount == 0:
			verdict = "no test"
			line = fmt.Sprintf("criterion %s: no test %s in %s", r.id, r.test, r.pkg)
		case v.failCount > 0:
			verdict = "failed"
			line = fmt.Sprintf("criterion %s: %s in %s failed: %s", r.id, r.test, r.pkg, ev)
		case v.exit != 0:
			verdict = "failed"
			line = fmt.Sprintf("criterion %s: %s in %s did not run green (exit %d): %s", r.id, r.test, r.pkg, v.exit, ev)
		default:
			line = fmt.Sprintf("criterion %s: %s in %s passed", r.id, r.test, r.pkg)
		}
		lines = append(lines, line)
		if verdict != "passed" {
			findings = append(findings, line)
		}
		entries = append(entries, map[string]any{"id": r.id, "verdict": verdict, "exit": v.exit})
	}
	return findings, lines, entries
}

// GateCriteria runs the plan's criteria against the pull request head in a
// fresh sandbox with no agent: the worker's repro mode runs a
// factoryd-written script (the task's own reproduction stays untouched),
// one pod for every criterion together. Not the plan's integration card:
// not run, the timeline says why. A plan that names no criteria names no
// proof: that is a finding, for the human. The namespace and the token go
// when it ends, however it ends.
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
	p, err := r.acceptedPlan(ctx, t.Parent, t.ID)
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
	runs, findings := criteriaRuns(p)
	if len(p.Criteria) == 0 {
		findings = append(findings, "criteria gate: the plan names no criteria")
	}
	// the plan's stored hold-outs run here too, beside the criteria: their
	// reds never reach the card's findings or the comment (blind by
	// routing and storage), they go to the settlement as Holdouts
	hos := r.gateHoldouts(ctx, t.Parent)
	entries := make([]map[string]any, 0, len(runs))
	holdoutEntries := make([]map[string]any, 0, len(hos))
	detail := func() map[string]any {
		return gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "criteria": entries, "holdouts": holdoutEntries})
	}
	// nothing to run (an empty plan, or every runnable criterion off the
	// format): the sandbox would report nothing
	if len(runs) == 0 && len(hos) == 0 {
		if len(findings) > 0 {
			r.commentOnce(ctx, t, repo, pr, r.untrusted(t, fmt.Sprintf("factoryd criteria gate on %s: no criterion ran.\n\n%s",
				commit, fenced(strings.Join(findings, "\n")))))
		}
		r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "criteria": entries, "findings": findings}))
		return CriteriaResult{Findings: findings}, nil
	}
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		return CriteriaResult{}, err
	}
	defer done()
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
	run := t
	run.ReproCommand = criteriaScript(runs) + holdoutScript(hos)
	rep, err := r.runRepro(ctx, run, repo, pullRef(pr), commit, "")
	if err != nil {
		return cannotStart(err)
	}
	markers := parseCriteriaMarkers(rep.Output)
	var runFindings, lines []string
	runFindings, lines, entries = criteriaReport(runs, markers)
	findings = append(findings, runFindings...)
	met := len(runs) - len(runFindings)
	holdouts := make([]HoldoutGateResult, 0, len(hos))
	hmarkers := parseHoldoutMarkers(rep.Output)
	for _, h := range hos {
		dry, ev := holdoutDryVerdict(hmarkers[h.Idx], holdoutRan(hmarkers, h.Idx))
		holdoutEntries = append(holdoutEntries, map[string]any{"id": h.ID, "verdict": dry})
		if dry == "red" || dry == HoldoutUnrunnable {
			holdouts = append(holdouts, HoldoutGateResult{Idx: h.Idx, ID: h.ID, Verdict: dry, Evidence: ev})
		}
	}
	r.commentOnce(ctx, t, repo, pr, r.untrusted(t, criteriaComment(commit, met, lines)))
	r.record(ctx, t.ID, "gate_criteria", detail())
	return CriteriaResult{Findings: findings, Holdouts: holdouts}, nil
}

// criteriaComment is the gate's comment on the pull request: the head, how
// many criteria it met, and one line per criterion.
func criteriaComment(commit string, met int, lines []string) string {
	return fmt.Sprintf("factoryd criteria gate on %s: %d of %d criteria met (judgement and human criteria ride the review and the approval).\n\n%s",
		commit, met, len(lines), fenced(strings.Join(lines, "\n")))
}
