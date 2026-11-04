package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The prover (blind proofs): once a plan's foundation card is on main, or
// at confirmation for a plan without one, the criteria sweep opens one
// prover task, a report task of the plan at main's head. Its session
// writes one test per criterion of kind test, judgement and human and per
// okayed hold-out, runs each itself and files the set through file_proofs,
// which checks the set's shape at once. After the session factoryd runs
// the last filing on main and stores its rows in plan_proofs, flagging a
// proof green or unrunnable there; a flagged proof opens at the gate
// without a blind try. The code lives in plan_proofs, the prover's sandbox
// and the runner's memory alone.

const (
	maxProofCode = 16 << 10
	maxFilings   = 3
)

// proverDryRun is the dry run's share of the prover's agent time; a var
// for tests, whose activities end after 10 minutes.
var proverDryRun = reproActivityTimeout

// proofFile is a test file name the project's tools run.
var proofFile = regexp.MustCompile(`(_test\.go|\.(test|spec)\.(ts|tsx|js|mjs))$`)

// proofName admits a test's name: check test puts it into go test's -run
// pattern unescaped, so letters, digits, _ and the / of a subtest alone.
var proofName = regexp.MustCompile(`^[A-Za-z0-9_/]+$`)

// What the prover reports of its own run on main.
const (
	onMainRed        = "red"
	onMainGreen      = "green"
	onMainUnrunnable = "unrunnable"
)

// FiledProof is one entry of a file_proofs call.
type FiledProof struct {
	Criterion string `json:"criterion"`
	Path      string `json:"path"`
	Suite     string `json:"suite"`
	Name      string `json:"name"`
	Code      string `json:"code"`
	Outcome   string `json:"outcome"`
	Artifact  string `json:"artifact,omitempty"`
}

// ProofsFiled is a filing's answer: its number, the filings left, and every
// finding that refused it.
type ProofsFiled struct {
	Filing   int      `json:"filing"`
	Left     int      `json:"left"`
	Findings []string `json:"findings"`
}

// proofDue is a proof the prover owes: a criterion of kind test, judgement
// or human, or a hold-out (kind test, holdout its idx; 0 for a criterion).
type proofDue struct {
	id, kind string
	holdout  int
}

// logged is how the plan's timeline names d: a hold-out by its entry id
// alone.
func (d proofDue) logged() string {
	if d.holdout > 0 {
		return holdoutEntry(d.holdout)
	}
	return d.id
}

// proofsDue: p's criteria of kind test, judgement and human in plan order,
// then the hold-out rows okayed, filed, stored or opened, in idx order,
// each by its id in the prover's brief (briefID).
func proofsDue(p Plan, rows []PlanProof) []proofDue {
	var due []proofDue
	for _, c := range p.Criteria {
		if c.Kind == CriterionTest || c.Kind == CriterionJudgement || c.Kind == CriterionHuman {
			due = append(due, proofDue{id: c.ID, kind: c.Kind})
		}
	}
	for _, pp := range rows {
		if pp.Origin == ProofOriginHoldout && slices.Contains([]string{ProofOkayed, ProofFiled, ProofStored, ProofOpened}, pp.State) {
			due = append(due, proofDue{id: briefID(p, pp), kind: CriterionTest, holdout: pp.Idx})
		}
	}
	return due
}

// briefID is hold-out h's id in the prover's brief and filing: its own,
// or its entry id once a criterion of p holds the same, since the planner,
// blind to the hold-outs, may give a later criterion that id.
func briefID(p Plan, h PlanProof) string {
	if slices.ContainsFunc(p.Criteria, func(c PlanCriterion) bool { return c.ID == h.ID }) {
		return holdoutEntry(h.Idx)
	}
	return h.ID
}

// proofFinding is one refusal of a filing: text the prover reads, and
// logged, the same for the plan's timeline, where a hold-out is its entry
// id alone and none of its test's path, suite or name shows.
type proofFinding struct{ text, logged string }

// holdoutWords tells an entry of proofs whose test shares a path, a suite
// or a name with a hold-out's entry: the timeline logs it without them.
func holdoutWords(due []proofDue, proofs []FiledProof) func(FiledProof) bool {
	words := map[string]bool{}
	for _, e := range proofs {
		if slices.ContainsFunc(due, func(d proofDue) bool { return d.holdout > 0 && d.id == e.Criterion }) {
			words["path "+path.Clean(e.Path)], words["suite "+e.Suite], words["name "+e.Name] = true, true, true
		}
	}
	return func(e FiledProof) bool {
		return words["path "+path.Clean(e.Path)] || words["suite "+e.Suite] || words["name "+e.Name]
	}
}

// proofFindings are the refusals of a filing of proofs against due, the
// shape alone: per entry in filing order, then the proofs missing in due
// order. No test runs here; the dry run after the session is the check.
func proofFindings(due []proofDue, proofs []FiledProof) []string {
	var out []string
	for _, f := range checkProofs(due, proofs) {
		out = append(out, f.text)
	}
	return out
}

func checkProofs(due []proofDue, proofs []FiledProof) []proofFinding {
	var out []proofFinding
	owed := map[string]proofDue{}
	for _, d := range due {
		if _, ok := owed[d.id]; !ok {
			owed[d.id] = d
		}
	}
	quote := func(s string) string { return strconv.Quote(textutil.Truncate(s, 200)) }
	shares := holdoutWords(due, proofs)
	filed := map[string]bool{}
	paths := map[string]proofDue{}
	for _, e := range proofs {
		d, ok := owed[e.Criterion]
		if !ok {
			out = append(out, proofFinding{fmt.Sprintf("file_proofs: %s is no criterion or hold-out of this plan", textutil.Truncate(e.Criterion, 64)),
				"file_proofs: an entry names no criterion or hold-out of this plan"})
			continue
		}
		if filed[d.id] {
			out = append(out, proofFinding{"file_proofs: " + d.id + " is filed twice", "file_proofs: " + d.logged() + " is filed twice"})
			continue
		}
		filed[d.id] = true
		masked := d.holdout > 0 || shares(e)
		// refuse records a finding on this entry: detail for the prover,
		// public, the same less the test's own words, for the log of an
		// entry that is a hold-out or shares a word with one
		refuse := func(detail, public string) {
			f := proofFinding{text: "file_proofs: " + d.id + ": " + detail}
			f.logged = f.text
			if masked {
				f.logged = "file_proofs: " + d.logged() + ": " + public
			}
			out = append(out, f)
		}
		const inside = "a relative path inside the repository"
		p := path.Clean(e.Path)
		first, _, _ := strings.Cut(p, "/")
		_, workflows := WorkflowDir(first)
		switch other, taken := paths[p]; {
		case path.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../"):
			refuse("path "+quote(e.Path)+": "+inside, "path: "+inside)
		case workflows:
			refuse("path "+quote(e.Path)+": closed to agents, the template owns it", "path: closed to agents, the template owns it")
		case !proofFile.MatchString(p):
			const test = "a test file the project's tools run, its name ending in _test.go, .test.ts or .spec.ts"
			refuse("path "+quote(e.Path)+": "+test, "path: "+test)
		case taken:
			f := proofFinding{text: fmt.Sprintf("file_proofs: %s: path %s is %s's too", d.id, quote(e.Path), other.id)}
			f.logged = f.text
			if masked || other.holdout > 0 {
				f.logged = fmt.Sprintf("file_proofs: %s: path is %s's too", d.logged(), other.logged())
			}
			out = append(out, f)
		default:
			paths[p] = d
		}
		if !suitePath.MatchString(e.Suite) || slices.Contains(strings.Split(e.Suite, "/"), "..") {
			refuse("suite "+quote(e.Suite)+": a package path such as ./proofs/"+d.id, "suite: a package path inside the repository")
		}
		switch {
		case e.Name == "":
			refuse("name is empty", "name is empty")
		case !proofName.MatchString(e.Name):
			const name = "a test's name of letters, digits, _ and the / of a subtest, such as TestSizeBound or TestSizeBound/over"
			refuse("name "+quote(e.Name)+": "+name, "name: "+name)
		}
		switch {
		case strings.TrimSpace(e.Code) == "":
			refuse("code is empty", "code is empty")
		case len(e.Code) > maxProofCode:
			size := fmt.Sprintf("code is %d bytes; at most %d", len(e.Code), maxProofCode)
			refuse(size, size)
		}
		switch e.Outcome {
		case onMainRed, onMainGreen, onMainUnrunnable:
		default:
			refuse("outcome "+quote(e.Outcome)+": red, green or unrunnable", "outcome: red, green or unrunnable")
		}
		ran := "your run on main was " + e.Outcome + ": "
		switch {
		case d.kind == CriterionTest && e.Outcome == onMainGreen:
			refuse(ran+"a test of kind test must fail there, where the feature is not built yet", ran+"a test of kind test must fail there, where the feature is not built yet")
		case d.kind == CriterionTest && e.Outcome == onMainUnrunnable:
			refuse(ran+"a test of kind test must run there and fail", ran+"a test of kind test must run there and fail")
		case d.kind != CriterionTest && e.Outcome == onMainUnrunnable:
			why := ran + "a test of kind " + d.kind + " must run there and write its artifact"
			refuse(why, why)
		}
		switch {
		case d.kind == CriterionTest && e.Artifact != "":
			refuse("a test of kind test writes no artifact", "a test of kind test writes no artifact")
		case d.kind != CriterionTest && !artifactOf(d.id, e.Artifact):
			why := fmt.Sprintf("a %s criterion names the file its test writes under CHECK_ARTIFACTS: %s or %s.<ext>", d.kind, d.id, d.id)
			refuse(why, why)
		}
	}
	for _, d := range due {
		if !filed[d.id] {
			filed[d.id] = true
			out = append(out, proofFinding{"file_proofs: no proof for " + d.id, "file_proofs: no proof for " + d.logged()})
		}
	}
	return out
}

// artifactOf tells whether name is criterion id's artifact: id, or id with
// one extension of letters and digits.
func artifactOf(id, name string) bool {
	if name == id {
		return true
	}
	ext, ok := strings.CutPrefix(name, id+".")
	return ok && ext != "" && !strings.ContainsFunc(ext, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	})
}

// FileProofs is the file_proofs tool's hand-over: one filing of the
// running prover of a plan, its shape checked against the proofs it owes.
// A filing with no finding is the plan's rows in state filed, which the
// next accepted one replaces; each filing, accepted or refused, is a
// proofs_filed event on the plan, and three are the prover's limit, read
// from those events, so a restart keeps the count and the last filing.
func (r *Runner) FileProofs(ctx context.Context, taskID string, proofs []FiledProof) (ProofsFiled, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return ProofsFiled{}, err
	}
	switch {
	case t.Source != SourceProver || t.Parent == "":
		return ProofsFiled{Findings: []string{"file_proofs: the task is no plan's prover"}}, nil
	case t.Status != "running":
		return ProofsFiled{Findings: []string{"file_proofs: the task is " + t.Status + ", not running"}}, nil
	}
	if ended, err := r.planEnded(ctx, t.Parent); err != nil || ended != "" {
		if ended != "" {
			return ProofsFiled{Findings: []string{"file_proofs: " + ended + ", so no proof of it is filed: end the session"}}, err
		}
		return ProofsFiled{}, err
	}
	p, revision, err := planOfStore(ctx, r.store, t.Parent, t.ID)
	if err != nil {
		return ProofsFiled{}, err
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	if err != nil {
		return ProofsFiled{}, err
	}
	filings, passed, err := r.filings(ctx, t)
	if err != nil {
		return ProofsFiled{}, err
	}
	if filings >= maxFilings {
		limit := "file_proofs: three filings are this session's limit, and none of them passed: end the session"
		if passed > 0 {
			limit = fmt.Sprintf("file_proofs: three filings are this session's limit, and filing %d, the last one that passed, stands: end the session", passed)
		}
		return ProofsFiled{Filing: filings, Findings: []string{limit}}, nil
	}
	due := proofsDue(p, rows)
	found := checkProofs(due, proofs)
	if len(found) == 0 {
		if err := r.store.FilePlanProofs(ctx, t.Parent, filedRows(p, revision, due, proofs)); err != nil {
			return ProofsFiled{}, err
		}
	}
	filing := filings + 1
	r.timeline.recordEvent(ctx, t.Parent, EventProofsFiled, filingDetail(t.ID, filing, due, proofs, found))
	res := ProofsFiled{Filing: filing, Left: maxFilings - filing}
	for _, f := range found {
		res.Findings = append(res.Findings, f.text)
	}
	return res, nil
}

// proverEvents bounds the plan's events a filing count reads, newest
// first: far more than a plan records.
const proverEvents = 1000

// filings is how many file_proofs calls of prover t its plan's
// proofs_filed events count, and passed the number of the last one that
// passed, 0 for none.
func (r *Runner) filings(ctx context.Context, t Task) (n, passed int, err error) {
	events, err := r.store.TaskEvents(ctx, t.Parent, proverEvents)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range events {
		if e.Kind != EventProofsFiled || e.Detail["prover"] != t.ID {
			continue
		}
		n++
		if refusals := reflect.ValueOf(e.Detail["refusals"]); refusals.Kind() == reflect.Slice && refusals.Len() == 0 {
			passed = max(passed, detailNumber(e.Detail["filing"]))
		}
	}
	return n, passed, nil
}

// detailNumber is an event detail's number: an int as recorded, a
// float64 as read back from the row's JSON.
func detailNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// planEnded is why plan id takes no proof any more, "" while it does: its
// close (plan_closed) or a person's close of it.
func (r *Runner) planEnded(ctx context.Context, id string) (string, error) {
	plan, _, err := r.task(ctx, id)
	if err != nil {
		return "", err
	}
	if _, closed, err := r.store.PlanClosed(ctx, id); err != nil || closed || plan.Status == "closed" {
		if closed || plan.Status == "closed" {
			return "plan " + id + " is closed", err
		}
		return "", err
	}
	return "", nil
}

// proverSummary is prover t's summary in factoryd's own words, never the
// model's: how many proofs its last accepted filing holds and that
// filing's number, that it filed none, or that its plan ended.
func (r *Runner) proverSummary(ctx context.Context, t Task) (string, error) {
	if ended, err := r.planEnded(ctx, t.Parent); err != nil || ended != "" {
		if ended != "" {
			return ended + ": no proof of it is stored", nil
		}
		return "", err
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	if err != nil {
		return "", err
	}
	n := 0
	for _, pp := range rows {
		if pp.State == ProofFiled {
			n++
		}
	}
	if n == 0 {
		return "the prover filed no proofs", nil
	}
	_, passed, err := r.filings(ctx, t)
	if err != nil {
		return "", err
	}
	summary := "filed " + textutil.Plural(n, "proof", "proofs")
	if passed > 0 {
		summary += fmt.Sprintf(", filing %d", passed)
	}
	return summary, nil
}

// proverSessionDue tells whether prover t runs a session in this attempt
// of its activity: none for a plan that ended (planEnded), whose prover
// ends in factoryd's words, nor on a retry the time no longer holds
// (sessionDue).
func (r *Runner) proverSessionDue(ctx context.Context, t Task) (bool, error) {
	if ended, err := r.planEnded(ctx, t.Parent); err != nil || ended != "" {
		return false, err
	}
	info := activity.GetInfo(ctx)
	return r.sessionDue(ctx, t, info.Attempt, info.Deadline)
}

// sessionDue: a first attempt runs its session, whose time reportTime
// checks against the class's agent time; a retry with under minReportTime
// left before deadline once the grace and the dry run's share are kept
// goes to the dry run with a filing that stands, and with none is a
// terminal error that names the times.
func (r *Runner) sessionDue(ctx context.Context, t Task, attempt int32, deadline time.Time) (bool, error) {
	left := time.Until(deadline) - sessionGrace - proverDryRun
	if attempt <= 1 || left >= minReportTime {
		return true, nil
	}
	rows, err := r.store.PlanProofs(ctx, t.Parent)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(rows, func(pp PlanProof) bool { return pp.State == ProofFiled }) {
		return false, nil
	}
	return false, terminalError("the prover's run has %s left once the dry run's %s are kept, under the %s a session needs, and no filing stands",
		max(left, 0).Round(time.Second), proverDryRun, minReportTime)
}

// filedRows are an accepted filing's rows in due order: a criterion's
// with its claim, inputs and expected numbers in revision, a hold-out's by
// its idx; every path cleaned.
func filedRows(p Plan, revision int, due []proofDue, proofs []FiledProof) []PlanProof {
	byID := map[string]FiledProof{}
	for _, e := range proofs {
		byID[e.Criterion] = e
	}
	out := make([]PlanProof, 0, len(due))
	for _, d := range due {
		e := byID[d.id]
		pp := PlanProof{State: ProofFiled, Path: path.Clean(e.Path), Suite: e.Suite, Name: e.Name, Code: e.Code, Artifact: e.Artifact}
		if d.holdout > 0 {
			pp.Idx, pp.ID, pp.Origin = d.holdout, d.id, ProofOriginHoldout
		} else {
			pp.Round, pp.ID, pp.Criterion, pp.Origin = revision, d.id, d.id, ProofOriginCriterion
			if i := slices.IndexFunc(p.Criteria, func(c PlanCriterion) bool { return c.ID == d.id }); i >= 0 {
				pp.Claim, pp.Inputs, pp.Expected = p.Criteria[i].Claim, p.Criteria[i].Inputs, p.Criteria[i].Expected
			}
		}
		out = append(out, pp)
	}
	return out
}

// filingDetail is a filing's proofs_filed detail: each entry of a due
// criterion with its test's path, suite, name and the prover's outcome on
// main, one of its three words or invalid; a hold-out's, and a
// criterion's that shares a hold-out's path, suite or name, by its id and
// outcome alone, a hold-out by its entry id; an entry that names no
// criterion by its outcome alone; the refusals as the timeline names
// them. Never a test's code, and no word of the prover's but these.
func filingDetail(prover string, filing int, due []proofDue, proofs []FiledProof, found []proofFinding) map[string]any {
	short := func(s string) string { return textutil.Truncate(s, 200) }
	shares := holdoutWords(due, proofs)
	entries := make([]map[string]any, 0, len(proofs))
	for _, e := range proofs {
		outcome := "invalid"
		if slices.Contains([]string{onMainRed, onMainGreen, onMainUnrunnable}, e.Outcome) {
			outcome = e.Outcome
		}
		i := slices.IndexFunc(due, func(d proofDue) bool { return d.id == e.Criterion })
		switch {
		case i < 0:
			entries = append(entries, map[string]any{"outcome": outcome})
		case due[i].holdout > 0 || shares(e):
			entries = append(entries, map[string]any{"criterion": due[i].logged(), "outcome": outcome})
		default:
			entries = append(entries, map[string]any{"criterion": e.Criterion, "path": short(e.Path), "suite": short(e.Suite),
				"name": short(e.Name), "outcome": outcome})
		}
	}
	refusals := make([]string, 0, len(found))
	for _, f := range found {
		refusals = append(refusals, textutil.Truncate(f.logged, 500))
	}
	return map[string]any{"prover": prover, "filing": filing, "proofs": entries, "refusals": refusals}
}

// ProofsFileHandler answers POST /internal/proofs {"task_id", "proofs"}: 200 with ProofsFiled, findings included.
func ProofsFileHandler(r *Runner) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			TaskID string       `json:"task_id"`
			Proofs []FiledProof `json:"proofs"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, harness.MaxProofsList)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.TaskID == "" {
			http.Error(w, "task_id is required", http.StatusBadRequest)
			return
		}
		res, err := r.FileProofs(req.Context(), in.TaskID, in.Proofs)
		if err != nil {
			status := http.StatusInternalServerError
			if isTerminal(err) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
	}
}

// The prover's guide: the surface its tests bind to, and how it works.
const (
	proverSurface = "Test each criterion at a surface that stays put while the cards are built: the HTTP API through the server constructor " +
		"the foundation names (internal/server.New unless it names another), the command line of a binary, the store through its test harness, " +
		"seeded and read back, or the files and output the feature writes. Reach into an internal contract only when a criterion cannot be stated at a surface."
	proverWork = "How to work: write each test in a file of its own that the project's tools run, such as proofs/<id>/<id>_test.go, and change no other file. " +
		"Run each one as the gate will: mkdir -p /tmp/art && CHECK_REPORT=/tmp/<id>.xml CHECK_ARTIFACTS=/tmp/art bash check test <suite> <name>. " +
		"A test of kind test, and every hold-out, must fail on main for the feature's absence: a test green before the feature exists proves nothing. " +
		"A judgement or human test must run on main and write its artifact. Then file the whole set with file_proofs: per criterion its id, path, suite, name, code, " +
		"your run's outcome on main and, for judgement and human, the artifact's file name. " +
		"A refusal names what to fix; file again, three filings at most; the last filing stands. You push nothing."
)

// sentence is s trimmed and ending in a full stop, unless it ends in one
// or another stop of its own.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

// proverGuide is the prover's brief: the opening, the spec, one line per
// criterion of kind test, judgement and human and per hold-out, the
// surface rule, the foundation's contracts when the plan has a foundation
// card, and the way of work. A judgement's prompt stays out: the model
// that judges the artifact reads it, not the test's writer.
func proverGuide(p Plan, holdouts []PlanProof, foundation *PlanCard) string {
	var b strings.Builder
	b.WriteString("You are the prover. You write one test per criterion and hold-out below, against the repository's main head, " +
		"before the plan's cards build the feature. The builder never sees your tests until each has run once at the gate.\n\n")
	if spec := specBlock(p); spec != "" {
		b.WriteString(spec + "\n\n")
	}
	b.WriteString("The criteria and hold-outs to prove:\n")
	for _, c := range p.Criteria {
		switch c.Kind {
		case CriterionTest:
			fmt.Fprintf(&b, "- %s (test): %s Inputs: %s Expected: %s\n", c.ID, sentence(c.Claim), sentence(c.Inputs), sentence(c.Expected))
		case CriterionJudgement:
			fmt.Fprintf(&b, "- %s (judgement): %s The artifact shows: %s Write it to $CHECK_ARTIFACTS/%s.<ext>.\n", c.ID, sentence(c.Claim), sentence(c.Inputs), c.ID)
		case CriterionHuman:
			fmt.Fprintf(&b, "- %s (human): %s The artifact shows: %s Write it to $CHECK_ARTIFACTS/%s.<ext>.\n", c.ID, sentence(c.Claim), sentence(c.Evidence), c.ID)
		}
	}
	for _, h := range holdouts {
		fmt.Fprintf(&b, "- %s (hold-out, kind test): %s Inputs: %s Expected: %s\n", briefID(p, h), sentence(h.Claim), sentence(h.Inputs), sentence(h.Expected))
	}
	b.WriteString("\n" + proverSurface + "\n")
	if foundation != nil {
		fmt.Fprintf(&b, "\nThe foundation card %s is on main. Its brief named the contracts every card builds on; read them there:\n%s\n",
			foundation.Key, strings.TrimSpace(foundation.Brief))
	}
	b.WriteString("\n" + proverWork)
	return b.String()
}

// proverBrief is plan's prover guide: its filed plan, its okayed and filed
// hold-outs and its first foundation card.
func (r *Runner) proverBrief(ctx context.Context, plan Task) (string, error) {
	p, err := r.filedPlan(ctx, plan.ID, plan.ID)
	if err != nil {
		return "", err
	}
	rows, err := r.store.PlanProofs(ctx, plan.ID)
	if err != nil {
		return "", err
	}
	var holdouts []PlanProof
	for _, pp := range rows {
		if pp.Origin == ProofOriginHoldout && (pp.State == ProofOkayed || pp.State == ProofFiled) {
			holdouts = append(holdouts, pp)
		}
	}
	var foundation *PlanCard
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Role == RoleFoundation }); i >= 0 {
		foundation = &p.Cards[i]
	}
	return proverGuide(p, holdouts, foundation), nil
}

// storeProofs dry-runs the filed rows on main at t.SHA and stores them flagged; once per plan.
// The pod fetches main's branch at its head and checks out t.SHA, the
// commit the prover's session read. The rows say whether the store ran: a
// plan with stored rows and none filed stores nothing again, whether or
// not its event landed. Else a plan closed meanwhile stores nothing, its
// line on the prover's timeline saying so, and no row filed is a terminal
// error in factoryd's words. A pod that cannot start or ends early returns
// its error, and the activity runs again.
func (r *Runner) storeProofs(ctx context.Context, t Task, repo Repo, plan Task) error {
	rows, err := r.store.PlanProofs(ctx, plan.ID)
	if err != nil {
		return err
	}
	var filed []PlanProof
	stored := false
	for _, pp := range rows {
		switch pp.State {
		case ProofFiled:
			filed = append(filed, pp)
		case ProofStored, ProofOpened:
			stored = true
		}
	}
	if len(filed) == 0 && stored {
		return nil
	}
	if ended, err := r.planEnded(ctx, plan.ID); err != nil || ended != "" {
		if ended != "" {
			r.record(ctx, t.ID, "proofs_not_stored", map[string]any{"reason": ended})
		}
		return err
	}
	if len(filed) == 0 {
		return terminalError("the prover filed no proofs")
	}
	defer beatUntilDone(ctx, "prover")()
	p, _, err := planOfStore(ctx, r.store, plan.ID, t.ID)
	if err != nil {
		return err
	}
	branch, err := r.forgejo.RepoDefaultBranch(ctx, repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, branch)
	if err != nil {
		return err
	}
	done, err := r.reproSandbox(ctx, &t, repo)
	if err != nil {
		return err
	}
	defer done()
	entries := make([]harness.Proof, len(filed))
	for i, pp := range filed {
		entries[i] = storedProof(pp)
	}
	runs, runErr, err := r.proofRuns(ctx, t, repo, branchRef(branch), head, t.SHA, entries)
	if err != nil {
		return err
	}
	flags := map[int]string{}
	ids := make([]string, 0, len(filed))
	flagged := []map[string]any{}
	for i, pp := range filed {
		kind := CriterionTest
		if c := slices.IndexFunc(p.Criteria, func(c PlanCriterion) bool { return pp.Origin == ProofOriginCriterion && c.ID == pp.Criterion }); c >= 0 {
			kind = p.Criteria[c].Kind
		}
		run, ok := runs[entries[i].ID]
		ids = append(ids, entries[i].ID)
		if flag := proofFlag(kind, proofResult(run, ok, runErr, pp.Name), run, pp.Artifact); flag != "" {
			flags[pp.Idx] = flag
			flagged = append(flagged, map[string]any{"criterion": entries[i].ID, "flag": flag})
		}
	}
	if err := r.store.StorePlanProofs(ctx, plan.ID, flags); err != nil {
		return err
	}
	r.timeline.recordEvent(ctx, plan.ID, EventProofsStored, map[string]any{"prover": t.ID, "commit": t.SHA, "stored": ids, "flagged": flagged})
	return nil
}

// proofFlag is what a proof's dry run on main flags: a test that passes
// is green on main, one that does not fail is unrunnable; a judgement or
// human test that did not run to a verdict, or wrote no artifact, is
// unrunnable. "" is none: the proof runs blind at the gate.
func proofFlag(kind string, o proofOutcome, run harness.ProofRun, artifact string) string {
	if kind == CriterionTest {
		switch o.Outcome {
		case proofFailed:
			return ""
		case proofPassed:
			return ProofFlagGreen
		}
		return ProofFlagUnrunnable
	}
	if _, wrote := run.Artifacts[artifact]; wrote && (o.Outcome == proofFailed || o.Outcome == proofPassed) {
		return ""
	}
	return ProofFlagUnrunnable
}

// proverPass opens the prover of each open plan that owes proofs, once its
// foundation card has merged, or at once for a plan without one: one
// report task of the plan at main's head, its workflow started. A plan the
// policy cannot serve, with no prover class of session report or one whose
// agent time leaves the session too little, waits, its id on one log line.
func (r *Runner) proverPass(ctx context.Context) {
	ids, err := r.store.PlansWithoutProver(ctx)
	if err != nil {
		log.Printf("prover pass: the plans without a prover: %v", err)
		return
	}
	v := r.cfg.Policy()
	var waiting []string
	why := ""
	for _, id := range ids {
		plan, repo, err := r.task(ctx, id)
		if err != nil {
			log.Printf("prover pass: plan %s: %v", id, err)
			continue
		}
		if ready, err := r.proverDue(ctx, plan); err != nil || !ready {
			if err != nil {
				log.Printf("prover pass: plan %s: %v", id, err)
			}
			continue
		}
		if unfit := proverUnfit(v, plan.Repository); unfit != "" {
			waiting, why = append(waiting, id), unfit
			continue
		}
		head, err := r.forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, "")
		if err != nil {
			log.Printf("prover pass: plan %s: main's head: %v", id, err)
			continue
		}
		if _, err := openReport(ctx, r.store, r.cfg.Workflows, v.Document.Budget, plan, SourceProver, policy.ClassProver, head); err != nil {
			log.Printf("prover pass: plan %s: %v", id, err)
		}
	}
	if len(waiting) > 0 {
		log.Printf("prover pass: plans %s wait for their prover: %s", strings.Join(waiting, ", "), why)
	}
}

// proverDue tells whether plan owes proofs and its foundation card, when
// it has one, has merged.
func (r *Runner) proverDue(ctx context.Context, plan Task) (bool, error) {
	if plan.Status == "closed" {
		return false, nil
	}
	p, _, err := planOfStore(ctx, r.store, plan.ID, plan.ID)
	if err != nil {
		return false, err
	}
	rows, err := r.store.PlanProofs(ctx, plan.ID)
	if err != nil || len(proofsDue(p, rows)) == 0 {
		return false, err
	}
	card, named, err := r.foundationCard(ctx, plan.ID, p)
	return !named || card.MergeCommit != "", err
}

// foundationCard is plan id's first foundation card by p; named tells
// whether p names one, and a card not filed yet is the zero Task.
func (r *Runner) foundationCard(ctx context.Context, id string, p Plan) (card Task, named bool, err error) {
	keys := cardsOf(p.Cards, RoleFoundation)
	if len(keys) == 0 {
		return Task{}, false, nil
	}
	cards, err := r.store.PlanCards(ctx, id)
	if i := slices.IndexFunc(cards, func(c Task) bool { return c.PlanKey == keys[0] }); i >= 0 {
		card = cards[i]
	}
	return card, true, err
}

// planProofCounts: wanted is len(proofsDue(p, rows)), stored the due
// proofs whose row is stored or opened.
func planProofCounts(p Plan, rows []PlanProof) (stored, wanted int) {
	due := proofsDue(p, rows)
	for _, d := range due {
		if slices.ContainsFunc(rows, func(pp PlanProof) bool {
			of := pp.Origin == ProofOriginCriterion && pp.Criterion == d.id
			if d.holdout > 0 {
				of = pp.Origin == ProofOriginHoldout && pp.Idx == d.holdout
			}
			return of && (pp.State == ProofStored || pp.State == ProofOpened)
		}) {
			stored++
		}
	}
	return stored, len(due)
}

// planProofState is plan task id's newest revision and its proofs by it
// (planProofCounts); a plan with no revision owes none.
func (r *Runner) planProofState(ctx context.Context, id string) (p Plan, stored, wanted int, err error) {
	rev, ok, err := r.store.PlanRevision(ctx, id, 0)
	if err != nil || !ok {
		return Plan{}, 0, 0, err
	}
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return Plan{}, 0, 0, fmt.Errorf("plan %s revision %d: %w", id, rev.Revision, err)
	}
	rows, err := r.store.PlanProofs(ctx, id)
	if err != nil {
		return Plan{}, 0, 0, err
	}
	stored, wanted = planProofCounts(p, rows)
	return p, stored, wanted, nil
}

// proverUnfit is why v cannot run a prover on repository: no prover class
// of session report, or an agent time that leaves its session under the
// least once the dry run's share is kept; "" when it can.
func proverUnfit(v policy.Version, repository string) string {
	c, ok := v.Document.Classes[policy.ClassProver]
	if !ok || c.Session != policy.SessionReport {
		return "the policy has no " + policy.ClassProver + " class of session report"
	}
	_, project, _ := v.ProjectOf(repository)
	agentTime, _ := v.Document.TimeOf(policy.ClassProver, v.Processes[project.Process], project)
	if _, err := reportTime(agentTime, proverDryRun); err != nil {
		return errMessage(err)
	}
	return ""
}
