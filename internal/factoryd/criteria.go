package factoryd

import (
	"bytes"
	"cmp"
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
// integration card and on a delta card, at the approval, it runs the
// plan's invariant criteria, its test criteria's proofs and its hold-outs
// against the head, writes each criterion's gate verdict row on the
// integration card and reports every one not met as a finding. A stored
// proof's run is its one blind try: green meets its criterion and the
// proof stays stored; any other outcome, or a flag from its dry run on
// main, opens it, and its finding hands the builder its code and its
// output. An opened proof runs from the repository. A test or judgement
// criterion whose proof is not stored yet makes no run: a test one's row
// is unmet and says why, and no finding; while the plan's prover may
// still store any criterion's proof, the gate asks the human to wait. A
// test or judgement criterion the store left without a proof came after
// the prover filed: its line is a finding, a test one's unmet row too,
// and it never opens. A judgement or human criterion's proof runs as a
// test criterion's and writes an artifact, which the gate stores for the
// console: one model call judges a judgement criterion's, the human
// settles a human one. A proof that wrote no artifact has no verdict and
// opens; a judgement criterion without a verdict is an unmet row.

// CriteriaResult is GateCriteria's verdict: Skipped names why the gate did
// not run (no plan filed, or a card of the plan that is neither its
// integration card nor a delta card); Findings are the criteria not met
// and the proofs opened, for the approval card; Escalate is the start
// escalation of a sandbox that could not start, or proofsNotStored, whose
// answer runs the gate again.
type CriteriaResult struct {
	Skipped  string   `json:",omitempty"`
	Findings []string `json:",omitempty"`
	Escalate string   `json:",omitempty"`
	// Holdouts is read by the workflow; the gate leaves it empty, since
	// no audit settles a hold-out.
	Holdouts []HoldoutGateResult `json:",omitempty"`
}

// maxProofMessage caps a proof's message on the timeline.
const maxProofMessage = 4 << 10

// proofsNotStored is the gate's ask while the plan's prover may still
// store a criterion's proof.
const proofsNotStored = "The plan's prover stored no proof yet. Once it does, answer to run the criteria gate again."

// The states of a gate run of a proof.
const (
	proofBlind = "blind" // a stored proof's run: its blind try
	proofOpen  = "open"  // an opened proof's run, from the repository
)

// The outcome words of an opening that are no proof* word.
const (
	proofFlagged    = "flagged"     // no run: a stored proof its dry run on main flagged
	proofNoArtifact = "no artifact" // a judgement or human proof's run that wrote no artifact under its name
)

// criterionRun is one run of the gate: a criterion of the plan, or a
// hold-out.
type criterionRun struct {
	id, kind    string             // id: the criterion's, or holdoutEntry(idx) for a hold-out
	suite, name string             // name "" runs the whole suite
	file        *harness.ProofFile // a stored proof's file, written into the tree first
	held        string             // the line of a criterion the gate cannot run: an unmet row
	late        bool               // held for want of a proof the store left out: a finding too
	stored      *PlanProof         // the row the run proves; nil for an invariant and a held run
}

// state is proofBlind for a stored proof, proofOpen for an opened one,
// "" for any other run.
func (c criterionRun) state() string {
	if c.stored == nil {
		return ""
	}
	switch c.stored.State {
	case ProofStored:
		return proofBlind
	case ProofOpened:
		return proofOpen
	}
	return ""
}

// label is "criterion <id>", or "hold-out <ID>" for a hold-out.
func (c criterionRun) label() string {
	if c.holdout() {
		return "hold-out " + c.stored.ID
	}
	return "criterion " + c.id
}

func (c criterionRun) holdout() bool { return c.stored != nil && c.stored.Origin == ProofOriginHoldout }

// flagged: a stored proof its dry run on main flagged, which has no blind
// try.
func (c criterionRun) flagged() bool { return c.state() == proofBlind && c.stored.Flag != "" }

// gateRuns are the runs the gate makes for p's criteria on its proof
// rows, in plan order, then the stored and opened hold-outs in idx order:
// an invariant runs its suite through goPackage; any other criterion and
// a hold-out run their stored proof blind with its file (a flagged one
// makes no run and opens), or their opened one by suite and name from the
// tree. A test or judgement criterion without a stored or opened row is
// held, late when the store happened without it; a human one makes no
// run.
func gateRuns(p Plan, proofs []PlanProof) []criterionRun {
	runnable := func(pp PlanProof) bool { return pp.State == ProofStored || pp.State == ProofOpened }
	proofRun := func(id, kind string, pp PlanProof) criterionRun {
		e := storedProof(pp)
		return criterionRun{id: id, kind: kind, suite: e.Suite, name: e.Name, file: e.File, stored: &pp}
	}
	stored := slices.ContainsFunc(proofs, runnable)
	var runs []criterionRun
	for _, c := range p.Criteria {
		if c.Kind == CriterionInvariant {
			runs = append(runs, criterionRun{id: c.ID, kind: c.Kind, suite: goPackage(c.Suite)})
			continue
		}
		i := slices.IndexFunc(proofs, func(pp PlanProof) bool {
			return pp.Origin == ProofOriginCriterion && pp.Criterion == c.ID && runnable(pp)
		})
		switch {
		case i >= 0:
			runs = append(runs, proofRun(c.ID, c.Kind, proofs[i]))
		case c.Kind == CriterionHuman:
		case stored:
			runs = append(runs, criterionRun{id: c.ID, kind: c.Kind, held: "Criterion " + c.ID + " has no stored proof: the prover filed before a later revision added it.", late: true})
		default:
			runs = append(runs, criterionRun{id: c.ID, kind: c.Kind, held: "criterion " + c.ID + ": no stored proof yet"})
		}
	}
	for _, pp := range proofs {
		if pp.Origin == ProofOriginHoldout && runnable(pp) {
			runs = append(runs, proofRun(holdoutEntry(pp.Idx), CriterionTest, pp))
		}
	}
	return runs
}

// storedProof is a proof row as a runner entry: its criterion's id, or
// holdoutEntry(Idx) for a hold-out; its suite through goPackage; its file
// for every state but opened, whose test the tree holds.
func storedProof(pp PlanProof) harness.Proof {
	e := harness.Proof{ID: pp.Criterion, Suite: goPackage(pp.Suite), Name: pp.Name}
	if pp.Origin == ProofOriginHoldout {
		e.ID = holdoutEntry(pp.Idx)
	}
	if pp.State != ProofOpened {
		e.File = &harness.ProofFile{Path: pp.Path, Content: pp.Code}
	}
	return e
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

// gateProof is one run as the gate_criteria timeline line records it: a
// criterion's, or an opened hold-out's by its own id.
type gateProof struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Suite   string `json:"suite,omitempty"`
	Name    string `json:"name,omitempty"`
	Outcome string `json:"outcome"` // a proof* constant; "" for a run with no outcome
	Met     bool   `json:"met"`
	Line    string `json:"line"`
	Message string `json:"message,omitempty"` // at most maxProofMessage
	State   string `json:"state,omitempty"`   // proofBlind or proofOpen
	Path    string `json:"path,omitempty"`    // a proof's file
	Holdout bool   `json:"holdout,omitempty"` // an opened hold-out, by its own id
	Changed bool   `json:"changed,omitempty"` // an opened proof whose file at the head is not its original
	// Artifact is a judgement or human proof's artifact, by name, when
	// its run wrote it as text
	Artifact string `json:"artifact,omitempty"`
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

// gateResult is what the gate read for one run, by its id.
type gateResult struct {
	outcome   proofOutcome
	output    string            // the check's output as the worker sent it; an opening keeps its tail
	ran       bool              // check test started for it: a blind proof it never started had no try
	absent    bool              // an opened proof whose path holds no file at the head: no run
	changed   bool              // an opened proof whose file at the head is not its code
	artifacts map[string][]byte // the run's files under CHECK_ARTIFACTS
	dropped   []string          // the artifacts its line had no room for
}

// openedProof is a proof this run opens.
type openedProof struct {
	run     criterionRun
	outcome string // a proof* word, proofNoArtifact, or proofFlagged
	output  string // the report's message for failed and skipped, else the check's output tail; "" when flagged; at most maxProofMessage
	text    string // "The criterion: ..." or "The hold-out: ...", the claim with its inputs and expected numbers
}

// gateReport is criteriaReport's reading of one gate.
type gateReport struct {
	findings  []string
	lines     []string // the criteria's, then the opened hold-outs'
	met       int      // the criteria met
	proofs    []gateProof
	opened    []openedProof
	holdouts  []map[string]any // the blind hold-outs that stay stored, by entry id, each with its holdoutDryVerdict
	artifacts []GateArtifact   // the judgement and human criteria's artifacts this run wrote, in plan order
	// noVerdict are the unmet rows of the judgement criteria that ran and
	// left the judgement nothing to read, each saying why
	noVerdict []CriterionVerdict
}

// criteriaReport judges the runs of plan p at head by their results:
// findings for the approval card, lines for the comment, entries for the
// timeline, the proofs it opens. A run without a result reported nothing:
// a gate that runs and reports nothing does not pass. A held run is its
// line alone, unmet and no finding: the builder cannot act on it; a late
// one is a finding too, for the human. A stored proof that passed its
// blind try meets its criterion; one whose try the runner never made is
// its line; any other outcome, and a flag, opens it. A judgement or human
// proof's run that wrote its artifact as text is an artifact for the
// judgement or the human and keeps its output off every surface; one
// that wrote no artifact opens, whatever its report says, and one whose
// artifact did not fit the run's line is a finding and stays stored. A
// judgement criterion that ran and left the judgement nothing to read is
// an unmet row, so no earlier head's row stands. A blind hold-out that
// stays stored, green or never tried, is no line, no entry and no
// finding: its verdict alone, on the hold-outs.
func criteriaReport(p Plan, head string, runs []criterionRun, results map[string]gateResult) gateReport {
	rep := gateReport{proofs: make([]gateProof, 0, len(runs)), holdouts: []map[string]any{}}
	for _, c := range runs {
		g := gateProof{ID: c.id, Kind: c.kind, Suite: c.suite, Name: c.name, Line: c.held, State: c.state()}
		if c.stored != nil {
			g.Path = c.stored.Path
		}
		if c.holdout() {
			g.ID, g.Holdout = c.stored.ID, true
		}
		open := func(outcome, output string) {
			o := openedProof{run: c, outcome: outcome, output: output, text: claimText(p, c)}
			rep.opened = append(rep.opened, o)
			rep.findings = append(rep.findings, openedFinding(o, head))
		}
		res, ok := results[c.id]
		switch {
		case c.held != "":
			if c.late {
				rep.findings = append(rep.findings, c.held)
			}
		case c.flagged():
			g.Line = c.label() + ": its proof was flagged " + c.stored.Flag + " by its dry run, so it had no blind try"
			open(proofFlagged, "")
		case res.absent:
			g.Line = fmt.Sprintf("%s: its open proof is not at %s in %s", c.label(), c.stored.Path, head[:min(7, len(head))])
			rep.findings = append(rep.findings, g.Line+": commit the test below there.\n"+c.name+" in "+c.suite+":\n"+fenced(c.stored.Code))
		default:
			o := res.outcome
			if !ok {
				o = proofOutcome{proofNoReport, "the run reported nothing"}
			}
			g.Outcome, g.Met, g.Line = o.Outcome, o.Outcome == proofPassed, criterionLine(c, o)
			g.Message, g.Changed = textutil.Truncate(o.Message, maxProofMessage), res.changed
			blind := c.state() == proofBlind
			output := textutil.Tail(res.output, maxProofMessage)
			if o.Outcome == proofFailed || o.Outcome == proofSkipped {
				output = g.Message
			}
			switch {
			case c.kind == CriterionJudgement || c.kind == CriterionHuman:
				name, at := c.stored.Artifact, head[:min(7, len(head))]
				content, wrote := res.artifacts[name]
				g.Met = false
				switch {
				case wrote && (!utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0):
					g.Message, g.Line = "", c.label()+": its artifact "+name+" is not text: no verdict"
					rep.findings = append(rep.findings, g.Line)
				case wrote:
					g.Message, g.Line, g.Artifact = "", c.label()+": "+c.name+" in "+c.suite+" wrote "+name, name
					rep.artifacts = append(rep.artifacts, GateArtifact{Criterion: c.id, Name: name, Head: head, Content: string(content)})
				case slices.Contains(res.dropped, name):
					g.Message, g.Line = "", c.label()+": artifact "+name+" was too large for the run's line"
					rep.findings = append(rep.findings, g.Line)
				case !res.ran:
					rep.findings = append(rep.findings, g.Line)
				case blind:
					g.Line = c.label() + ": its blind proof wrote no artifact " + name + " at " + at
					open(proofNoArtifact, output)
				default:
					g.Line = c.label() + ": its open proof wrote no artifact " + name + " at " + at
					rep.findings = append(rep.findings, g.Line)
				}
			case blind && c.holdout() && (g.Met || !res.ran):
				dry, _ := holdoutDryVerdict(o)
				rep.holdouts = append(rep.holdouts, map[string]any{"id": c.id, "verdict": dry})
				continue
			case g.Met:
			case blind && res.ran:
				open(o.Outcome, output)
			default:
				rep.findings = append(rep.findings, g.Line)
			}
		}
		if c.kind == CriterionJudgement && c.held == "" && g.Artifact == "" {
			why := strings.TrimSuffix(strings.TrimPrefix(g.Line, c.label()+": "), ": no verdict")
			rep.noVerdict = append(rep.noVerdict, CriterionVerdict{Criterion: c.id, Verdict: "unmet", Evidence: "no verdict: " + why, Kind: c.kind})
		}
		if g.Met && !g.Holdout {
			rep.met++
		}
		rep.lines = append(rep.lines, g.Line)
		rep.proofs = append(rep.proofs, g)
	}
	return rep
}

// claimText is run c's claim with its inputs and expected numbers: its
// criterion's in p, or its hold-out's; a judgement or human criterion's
// with what its artifact shows.
func claimText(p Plan, c criterionRun) string {
	what, claim, inputs, expected := "criterion", "", "", ""
	if c.holdout() {
		what, claim, inputs, expected = "hold-out", c.stored.Claim, c.stored.Inputs, c.stored.Expected
	} else if i := slices.IndexFunc(p.Criteria, func(pc PlanCriterion) bool { return pc.ID == c.id }); i >= 0 {
		pc := p.Criteria[i]
		if pc.Kind == CriterionJudgement || pc.Kind == CriterionHuman {
			// a judgement criterion names what its artifact shows in its
			// inputs, a human one in its evidence
			return fmt.Sprintf("The criterion: %s. The artifact shows: %s.", strings.TrimSuffix(pc.Claim, "."), strings.TrimSuffix(cmp.Or(pc.Inputs, pc.Evidence), "."))
		}
		claim, inputs, expected = pc.Claim, pc.Inputs, pc.Expected
	}
	return fmt.Sprintf("The %s: %s. Inputs: %s. Expected: %s.", what,
		strings.TrimSuffix(claim, "."), strings.TrimSuffix(inputs, "."), strings.TrimSuffix(expected, "."))
}

// openedFinding is the builder's finding for a proof that opens at head:
// why, where to commit its test and what to do, its claim, its code and
// the output of its run.
func openedFinding(o openedProof, head string) string {
	at := head[:min(7, len(head))]
	why := map[string]string{
		proofFailed:     "its blind proof ran red at " + at,
		proofMissing:    "its blind proof ran no test of its name at " + at,
		proofSkipped:    "its blind proof skipped at " + at,
		proofNoReport:   "its blind proof wrote no report at " + at,
		proofNoArtifact: "its blind proof wrote no artifact " + o.run.stored.Artifact + " at " + at,
		proofFlagged:    "its proof was flagged " + o.run.stored.Flag + " by its dry run, so it had no blind try",
	}[o.outcome]
	s := o.run.label() + ": " + why + ". It is open now. Commit the test below at " + o.run.stored.Path + " and make it pass: " +
		"fix the implementation, or the test where it is wrong, or raise the question on the card when the criterion cannot hold.\n" +
		o.text + "\n" + o.run.name + " in " + o.run.suite + ":\n" + fenced(o.run.stored.Code)
	if o.output != "" {
		s += "\nIts output:\n" + fenced(o.output)
	}
	return s
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
	return r.label() + ": " + s
}

// gateRows are the gate's verdict rows, one per criterion of kind test
// and invariant: met for one that passed, unmet for any other, its line
// the evidence, after its state when it has one.
func gateRows(proofs []gateProof) []CriterionVerdict {
	rows := make([]CriterionVerdict, 0, len(proofs))
	for _, g := range proofs {
		if g.Holdout || (g.Kind != CriterionTest && g.Kind != CriterionInvariant) {
			continue
		}
		verdict, evidence := "unmet", g.Line
		if g.Met {
			verdict = "met"
		}
		if g.State != "" {
			evidence = g.State + ": " + g.Line
		}
		rows = append(rows, CriterionVerdict{Criterion: g.ID, Verdict: verdict, Evidence: evidence, Kind: g.Kind})
	}
	return rows
}

// CriteriaCard says whether the card of key answers plan p's criteria:
// p's integration card, or a delta card, whose key p does not hold.
func CriteriaCard(p Plan, key string) bool {
	i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == key })
	return i < 0 || p.Cards[i].Role == RoleIntegration
}

// openedProofs are the proofs in state opened of p, t's plan, when t is its integration card or a delta card; nil for any other task.
func (r *Runner) openedProofs(ctx context.Context, t Task, p Plan) ([]PlanProof, error) {
	if t.Source != SourcePlan || t.Parent == "" || !CriteriaCard(p, t.PlanKey) {
		return nil, nil
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	return slices.DeleteFunc(rows, func(pp PlanProof) bool { return pp.State != ProofOpened }), err
}

// codeLines is the lines of code as a diff counts them: a last line
// without its newline counts too.
func codeLines(code string) int {
	n := strings.Count(code, "\n")
	if code != "" && !strings.HasSuffix(code, "\n") {
		n++
	}
	return n
}

// proofAtHead reads path at commit: present false when the path holds no
// file.
func (r *Runner) proofAtHead(ctx context.Context, repo Repo, commit, path string) (content []byte, present bool, err error) {
	content, err = r.forgejo.FileAt(ctx, repo.Owner, repo.Name, commit, path)
	switch {
	case errors.Is(err, errForgejoNotFound):
		return nil, false, nil
	case errors.Is(err, ErrAnswerTooLong):
		// cut at Forgejo's answer bound, past any proof's code: it differs
		return content, true, nil
	}
	return content, err == nil, err
}

// GateCriteria runs the plan's criteria against the pull request head in a
// fresh sandbox with no agent: one runner pod runs every gate run that is
// not held, flagged or absent at the head, the hold-outs last, through the
// tree's check test. A plan card that is neither the integration card
// nor a delta card: not run, the timeline says why. A read of the proof
// rows, or of an opened proof's file, that fails is the activity's error,
// so it runs again. A plan that names no criteria names no proof: that is
// a finding, for the human. While proofsPending, the gate runs nothing,
// writes the held rows and asks proofsNotStored. The rows land on the
// plan's integration card; each proof the gate opens is a proof_opened
// event on the card and on the plan, once per head, before its row turns
// opened. The artifacts the run wrote are stored on the integration card,
// and each judgement criterion's is judged (judgeArtifacts) before the
// comment. The namespace and the token go when it ends, however it ends.
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
	if !CriteriaCard(p, t.PlanKey) {
		const skipped = "not the plan's integration card or a delta card"
		r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "skipped": skipped}))
		return CriteriaResult{Skipped: skipped}, nil
	}
	home, err := integrationCard(ctx, r.store, t.Parent, p)
	if err != nil {
		return CriteriaResult{}, err
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	if err != nil {
		return CriteriaResult{}, err
	}
	waits, err := r.proofsPending(ctx, t, rows, p)
	if err != nil {
		return CriteriaResult{}, err
	}
	// the hold-outs run after the criteria, so no criterion's run sees a
	// hold-out's file; a green one's verdict reaches the timeline line
	// alone, by entry id
	runs := gateRuns(p, rows)
	if waits {
		// no proof is stored: the held rows stand, and the answer runs the whole gate
		var held []criterionRun
		for _, c := range runs {
			if c.held != "" {
				held = append(held, c)
			}
		}
		rep := criteriaReport(p, commit, held, nil)
		if err := r.store.PutCriteriaVerdicts(ctx, home.ID, CriteriaSourceGate, FactoryDecider, gateRows(rep.proofs)); err != nil {
			return CriteriaResult{}, err
		}
		r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit, "proofs": rep.proofs, "escalate": proofsNotStored}))
		return CriteriaResult{Escalate: proofsNotStored}, nil
	}
	results := map[string]gateResult{}
	var entries []harness.Proof
	for _, c := range runs {
		switch {
		case c.held != "" || c.flagged():
			continue
		case c.state() == proofOpen:
			content, present, err := r.proofAtHead(ctx, repo, commit, c.stored.Path)
			if err != nil {
				return CriteriaResult{}, fmt.Errorf("read %s at the head: %w", c.stored.Path, err)
			}
			if !present {
				results[c.id] = gateResult{absent: true}
				continue
			}
			results[c.id] = gateResult{changed: string(content) != c.stored.Code}
		}
		entries = append(entries, c.proof())
	}
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
		ran, outcomes, err := r.runProofs(ctx, t, repo, pullRef(pr), commit, "", entries)
		// a pod that ended early leaves the criteria it did not reach
		// unmet; a hold-out it did not reach is no verdict, so the
		// activity fails and runs again
		var ended *proofsEnded
		if errors.As(err, &ended) && !slices.ContainsFunc(runs, func(c criterionRun) bool { return c.holdout() && slices.Contains(ended.unreached, c.id) }) {
			err = nil
		}
		if err != nil {
			return cannotStart(err)
		}
		for _, e := range entries {
			run, ok := ran[e.ID]
			g := results[e.ID]
			g.outcome, g.output, g.ran, g.artifacts, g.dropped = outcomes[e.ID], run.Output, ok && run.Started, run.Artifacts, run.Dropped
			results[e.ID] = g
		}
	}
	rep := criteriaReport(p, commit, runs, results)
	if len(p.Criteria) == 0 {
		rep.findings = append(rep.findings, "criteria gate: the plan names no criteria")
	}
	opened := make([]string, 0, len(rep.opened))
	for _, o := range rep.opened {
		opened = append(opened, cmp.Or(o.run.stored.Criterion, o.run.stored.ID))
	}
	if err := r.store.PutCriteriaVerdicts(ctx, home.ID, CriteriaSourceGate, FactoryDecider, gateRows(rep.proofs)); err != nil {
		return CriteriaResult{}, err
	}
	if err := r.store.PutCriteriaVerdicts(ctx, home.ID, CriteriaSourceJudgement, FactoryDecider, rep.noVerdict); err != nil {
		return CriteriaResult{}, err
	}
	for _, o := range rep.opened {
		if err := r.openProof(ctx, t, commit, o); err != nil {
			return CriteriaResult{}, err
		}
	}
	judged, met, err := r.judgeArtifacts(ctx, t, home.ID, p, rep.artifacts)
	if err != nil {
		return CriteriaResult{}, err
	}
	rep.findings = append(rep.findings, judged...)
	of := 0
	for _, c := range p.Criteria {
		if gateJudged(c.Kind) {
			of++
		}
	}
	switch {
	case slices.ContainsFunc(runs, func(c criterionRun) bool { return c.held == "" }):
		r.commentOnce(ctx, t, repo, pr, r.untrusted(t, criteriaComment(commit, rep.met+met, of, rep.lines)))
	case len(rep.findings) > 0:
		// nothing ran: an empty plan
		r.commentOnce(ctx, t, repo, pr, r.untrusted(t, fmt.Sprintf("factoryd criteria gate on %s: no criterion ran.\n\n%s",
			commit, fenced(strings.Join(rep.findings, "\n")))))
	}
	r.record(ctx, t.ID, "gate_criteria", gateEnd(t, GateCriteria, map[string]any{"pr": pr, "commit": commit,
		"proofs": rep.proofs, "holdouts": rep.holdouts, "opened": opened, "findings": rep.findings}))
	return CriteriaResult{Findings: rep.findings}, nil
}

// openProof records o's opening at head on card t and on its plan, unless
// the timeline holds it already, and then turns its row opened, whatever
// the store answers: a retry after a stop between the two records no
// second event.
func (r *Runner) openProof(ctx context.Context, t Task, head string, o openedProof) error {
	pp := o.run.stored
	id := cmp.Or(pp.Criterion, pp.ID)
	detail := map[string]any{"criterion": id, "head": head, "outcome": o.outcome, "message": o.output,
		"path": pp.Path, "suite": o.run.suite, "name": o.run.name, "code": pp.Code}
	if o.outcome == proofFlagged {
		detail["message"] = pp.Flag
	}
	if o.run.holdout() {
		detail["holdout"] = pp.Claim + ": " + pp.Inputs + ": " + pp.Expected
	}
	for _, task := range []string{t.ID, t.Parent} {
		if !r.timeline.hasEvent(ctx, task, EventProofOpened, map[string]string{"criterion": id, "head": head}) {
			r.timeline.recordEvent(ctx, task, EventProofOpened, detail)
		}
	}
	_, err := r.store.OpenPlanProof(ctx, t.Parent, pp.Idx)
	return err
}

// proofsPending tells whether card t's plan p owes a criterion's proof
// (proofsDue) its prover may still store: no store happened, and the
// prover is open and not final, or not opened yet under a policy that can
// run one while the plan's foundation card, when it names one, has not
// ended other than done (proverDue opens none then).
func (r *Runner) proofsPending(ctx context.Context, t Task, rows []PlanProof, p Plan) (bool, error) {
	if len(proofsDue(p, nil)) == 0 ||
		slices.ContainsFunc(rows, func(pp PlanProof) bool { return pp.State == ProofStored || pp.State == ProofOpened }) {
		return false, nil
	}
	prover, ok, err := r.store.ReportOf(ctx, t.Parent, SourceProver)
	if err != nil || ok {
		return ok && !Final(prover.Status), err
	}
	card, named, err := r.foundationCard(ctx, t.Parent, p)
	if err != nil || (named && Final(card.Status) && card.Status != "done") {
		return false, err
	}
	return proverUnfit(r.cfg.Policy(), t.Repository) == "", nil
}

// criteriaComment is the gate's comment on the pull request: the head, how
// many of the gate's of criteria it met, the judgement's among them, and
// its lines.
func criteriaComment(commit string, met, of int, lines []string) string {
	return fmt.Sprintf("factoryd criteria gate on %s: %d of %d criteria met (human criteria ride the approval).\n\n%s",
		commit, met, of, fenced(strings.Join(lines, "\n")))
}
