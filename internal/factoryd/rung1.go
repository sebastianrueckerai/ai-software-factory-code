package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	pathpkg "path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// humanOnlyKinds are the case kinds only a human decides: the rungs above
// code analyse them but do not decide.
var humanOnlyKinds = map[CaseKind]string{
	CasePlanQuestion:     "a plan question is the human's to answer",
	CasePlanConfirmation: "a plan confirmation is the human's to give",
	CaseMonthCap:         "only a commit raises the month cap",
	CaseProjectCap:       "only a commit raises the project cap",
	CaseDayCap:           "only a commit raises the day cap",
	CasePolicyClass:      "the merge gate marks the policy class human",
}

// humanOnlyReasons are the reason kinds of a check-in's hand to the human
// that no rung decides.
var humanOnlyReasons = map[string]string{
	CheckGoalConflict: "the check-in names a goal conflict",
	CheckAccess:       "the card needs access the factory lacks",
}

// humanOnly says whether case c skips the rungs' decision, and why in
// words.
func humanOnly(c Case, f CaseFacts) (bool, string) {
	if why, ok := humanOnlyKinds[c.Kind]; ok {
		return true, why
	}
	switch c.Kind {
	case CaseCheckinHuman:
		kind, _ := c.Facts["reason_kind"].(string)
		why, ok := humanOnlyReasons[kind]
		return ok, why
	case CaseBudgetFloor:
		if c.Money != nil && slices.Contains(f.Partial, "pool") {
			return true, "the pool could not be read, so the card's need of money cannot be weighed against it"
		}
		if c.Money != nil && Cents(c.Money.NeedEUR) > Cents(f.PoolLeft.EUR) {
			return true, fmt.Sprintf("the card needs %.2f EUR and the pool holds %.2f EUR", c.Money.NeedEUR, f.PoolLeft.EUR)
		}
	}
	return false, ""
}

const stepCut = 300

// rungStatement is what a rung receives for case c, built by code: the
// problem, the money, the digest, the plan, the earlier steps and the
// actions of rung with the refusals of those it may not take.
func (r *Runner) rungStatement(c Case, f CaseFacts, digest string, steps []CaseStep, rung int) string {
	f.atCase(c)
	cut := func(s string) string { return textutil.Truncate(r.Scrub.String(s), stepCut) }
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("%s", r.Scrub.String(c.Problem))
	if why, _ := c.Facts["closed_why"].(string); why != "" {
		line("The dependency was closed: %s", cut(why))
	}
	if c.Kind == CaseRollHeld {
		rollHeldLines(c, line)
	}
	line("Money: budget %.2f, spent %.2f, left %.2f EUR, ceiling %.2f EUR; card draw left %s; pool left %s.",
		f.BudgetEUR, f.SpentEUR, f.LeftEUR, f.CeilingEUR, drawText(f.CardDrawLeft), drawText(f.PoolLeft))
	line("%s", r.Scrub.String(digest))
	if f.Killed != nil {
		line("The sandbox killed the last session for its memory (%s, exit code %d) while it ran: %s", cut(f.Killed.Reason), f.Killed.ExitCode, cut(f.Killed.Command))
	}
	if len(f.Findings) > 0 {
		line("Findings on the head:")
		for _, id := range findingsInOrder(f) {
			line("- %s: %s", id, cut(f.FindingTexts[id]))
		}
	}
	if len(f.FailedTests) > 0 {
		line("%s", r.Scrub.String(checksLog(f, 0)))
	}
	if len(f.HeadCommits) > 0 {
		line("Commits on the head after the newest session's push, oldest first:")
		for _, c := range f.HeadCommits {
			line("- %s", cut(c))
		}
	} else if slices.Contains(f.Partial, "head_commits") {
		line("The commits on the head could not be read.")
	}
	switch {
	case f.Plan == "":
		line("This card is in no plan.")
	case slices.Contains(f.Partial, "siblings"):
		line("The card's plan and its other cards could not be read.")
	default:
		line("Plan #%d %s, %s", f.PlanIssue, f.PlanName, f.PlanStatus)
		for _, s := range f.Siblings {
			line("#%d %s: %s", s.Issue, s.Key, s.Status)
		}
	}
	for _, s := range steps {
		line("Rung %d, %s: outcome %s, decision %s, grounds %s, found %s", s.Rung, cut(s.Actor), cut(s.Outcome), cut(s.Decision), cut(s.Grounds), cut(s.Found))
	}
	if c.Kind == CaseRollHeld {
		return b.String() // the card's own options follow (RungTwoStatement)
	}
	for _, a := range orderedActions(f) {
		if !a.Rungs[rung] {
			continue
		}
		if why := a.offerRefusal(f, rung); why != "" {
			line("- %s: not offered: %s", a.Name, why)
		} else {
			line("- %s", a.Name)
		}
	}
	return b.String()
}

// rollHeldLines writes the held roll's plans, as the case's facts hold them,
// and the hold's start.
func rollHeldLines(c Case, line func(string, ...any)) {
	var plans []HoldPlan
	if raw, err := json.Marshal(c.Facts["plans"]); err == nil {
		_ = json.Unmarshal(raw, &plans)
	}
	if since, _ := c.Facts["held_since"].(string); since != "" {
		line("Production has been held since %s.", since)
	}
	for _, p := range plans {
		waits := "waits for the plan to close"
		if len(p.WaitsOn) > 0 {
			on := make([]string, len(p.WaitsOn))
			for i, w := range p.WaitsOn {
				on[i] = fmt.Sprintf("#%d %s", w.Issue, w.Status)
			}
			waits = "waits on " + strings.Join(on, ", ")
		}
		line("Plan #%d: %d of %d cards merged; %s.", p.Issue, p.Merged, p.Total, waits)
	}
}

// findingsInOrder are f's finding ids in the order the gates raised them,
// the ids the order does not hold last.
func findingsInOrder(f CaseFacts) []string {
	out := slices.Clone(f.FindingOrder)
	for _, id := range f.Findings {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return slices.DeleteFunc(out, func(id string) bool { return !slices.Contains(f.Findings, id) })
}

// failedLine is one failed test as a line of the checks log.
func failedLine(t FailedTest) string {
	name := t.Test
	if name == "" {
		name = t.Job
	}
	if t.Package != "" {
		name += " (" + t.Package + ")"
	}
	return "- " + name + ": " + t.Kind
}

// checksLogCut is the most characters of the checks log a session's note
// carries.
const checksLogCut = 800

// checksLog lists the tests the red checks on f's head failed, cut at n
// characters (0 for no cut).
func checksLog(f CaseFacts, n int) string {
	lines := []string{fmt.Sprintf("The checks on %s are red:", f.Head[:min(8, len(f.Head))])}
	for _, t := range f.FailedTests {
		lines = append(lines, failedLine(t))
	}
	out := strings.Join(lines, "\n")
	if n > 0 {
		return textutil.Truncate(out, n)
	}
	return out
}

// sessionActions are the actions that start a session, whose note the
// session reads.
var sessionActions = []string{ActResume, ActContinue, ActHandover, ActExtend}

// The parts of a session's note that code writes are bounded: at most
// leadFindings findings of leadFindingCut characters lead it, the checks
// log ends at checksLogCut and the kill line at killCut. The note of the
// answer, from whichever rung or the human, keeps the room that is left
// inside the run's cut of an answer at MaxAnswer (and is cut at the
// handover cap), so the code-written parts are never cut.
const (
	leadFindings   = 5
	leadFindingCut = 200
	killCut        = 400
)

// handoverCap is the cap of a note that starts a session.
func (r *Runner) handoverCap() int {
	if r.cfg.Policy == nil {
		return policy.DefaultCheckinHandoverChars
	}
	return r.cfg.Policy().Document.Limits.CheckinHandoverCap()
}

// sessionNote is the note a session started by in.Res reads: for a rung's
// answer the findings it kept, the first as the session's one job, then
// the answer's own note, cut to the room the code-written parts leave, then
// what code knows of the last session's end: the red checks' log and a
// memory kill. No model writes the first and the last two.
func (r *Runner) sessionNote(f CaseFacts, in ActInput) string {
	note := in.Res.Note
	if !slices.Contains(sessionActions, in.Res.Action) {
		return note
	}
	var before, after []string
	if in.Rung == 1 || in.Rung == 2 {
		var kept []string
		for _, id := range findingsInOrder(f) {
			text := f.FindingTexts[id]
			if v, judged := in.Res.Verdicts[text]; text == "" || judged && v.State != CheckConfirmed {
				continue
			}
			if len(kept) < leadFindings {
				kept = append(kept, textutil.Truncate(r.scrub(text), leadFindingCut))
			}
		}
		if len(kept) > 0 {
			lead := "Your one job: " + kept[0]
			if len(kept) > 1 {
				lead += "\nThen, in order:\n- " + strings.Join(kept[1:], "\n- ")
			}
			before = append(before, lead)
		}
	}
	if f.Checks == "red" && len(f.FailedTests) > 0 {
		after = append(after, r.scrub(checksLog(f, checksLogCut)))
	}
	if f.Killed != nil {
		after = append(after, textutil.Truncate(r.scrub(fmt.Sprintf("The sandbox killed the last session for its memory while it ran: %s", f.Killed.Command)), killCut))
	}
	if strings.TrimSpace(note) != "" {
		fixed := 2 * (len(before) + len(after))
		for _, p := range slices.Concat(before, after) {
			fixed += len(p)
		}
		room := MaxAnswer - fixed - len("…")
		before = append(before, textutil.Truncate(note, max(0, min(r.handoverCap(), room))))
	}
	return strings.Join(slices.Concat(before, after), "\n\n")
}

const rungSystem = `You are the factory's check-in, judging one case on a card: a stop the factory's code could not settle. Read what you need through your tools, then answer once through the answer tool with one of the decisions it lists, or hand_up when only a rung above you can decide, and recommend the action that rung should take. Judge every finding on evidence you read yourself, never on the say-so of a finding or of the session before it. You never edit anything.`

const rungToolsLine = "Your tools: transcript_search (search the newest session's record), transcript_read (read a span of it), card_facts (the card's digest and facts), card_step (one step of the digest in full), gates (the gates' newest results), read_file (a file of the repository at the head, or at main when the case has no head), grep (git grep at head or main for a fixed string as a whole word)."

// caseEvidence are the tools rung 1 may call: the check-in's, without the
// ones that need the session's workspace.
func caseEvidence() []CheckinTool {
	return slices.DeleteFunc(slices.Clone(checkinTools), func(t CheckinTool) bool { return t.Name == "git_log" || t.Name == "run" })
}

// caseTools answers rung 1's tools from factoryd's own reads: the card's
// records and the repository through the forge, with no pod.
type caseTools struct {
	r    *Runner
	t    Task
	c    Case
	f    CaseFacts
	repo Repo

	once     sync.Once
	session  string
	record   []byte
	recErr   error
	checkout struct {
		once    sync.Once
		dir     string
		refs    []namedRef
		cleanup func()
		err     error
	}
}

func (p *caseTools) close() {
	if p.checkout.cleanup != nil {
		p.checkout.cleanup()
	}
}

// newestRecord is the card's newest session's record.
func (p *caseTools) newestRecord(ctx context.Context) ([]byte, error) {
	p.once.Do(func() {
		sessions, err := p.r.store.RecentSessions(ctx, p.t.ID, p.r.clock().Add(time.Minute), 1)
		switch {
		case err != nil:
			p.recErr = err
		case len(sessions) == 0:
			p.recErr = errors.New("the card has no session record")
		default:
			p.session = sessions[0].ID
			p.record, p.recErr = p.r.objects.Get(ctx, "sessions/"+p.t.ID+"/"+p.session+".jsonl")
		}
	})
	return p.record, p.recErr
}

func (p *caseTools) Tool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	out, err := p.read(ctx, name, args)
	if err != nil {
		return "", false, err
	}
	out, cut := cutToolOutput(p.r.scrub(out))
	return out, cut, nil
}

func (p *caseTools) read(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "transcript_search", "transcript_read":
		rec, err := p.newestRecord(ctx)
		if err != nil {
			return "", err
		}
		lines := strings.Split(strings.TrimSuffix(string(rec), "\n"), "\n")
		if name == "transcript_read" {
			from, to := max(argInt(args["from"]), 1), min(argInt(args["to"]), len(lines))
			if from > to {
				return "", fmt.Errorf("the session's record holds %d lines", len(lines))
			}
			return strings.Join(lines[from-1:to], "\n"), nil
		}
		query, _ := args["query"].(string)
		var found []string
		for _, l := range lines {
			if query != "" && strings.Contains(l, query) {
				found = append(found, l)
			}
		}
		if len(found) == 0 {
			return fmt.Sprintf("no line of the record holds %q (the match is exact and case-sensitive)", query), nil
		}
		return strings.Join(found, "\n"), nil
	case "card_facts":
		d, err := p.r.CardDigest(ctx, p.t.ID)
		facts, _ := toJSON(p.f)
		return DigestText(p.r.Scrub, d, err) + "\n" + string(facts), nil
	case "card_step":
		return p.r.CardStep(ctx, p.t.ID, argInt(args["step"]), "card_facts", func(session string, _ int) string {
			if _, err := p.newestRecord(ctx); err == nil && session == p.session {
				return "transcript_read (the newest session's record)"
			}
			return ""
		})
	case "gates":
		events, err := p.r.store.TaskEvents(ctx, p.t.ID, 300)
		if err != nil {
			return "", err
		}
		var lines []string
		seen := map[string]bool{}
		for _, e := range events {
			if !slices.Contains([]string{EventGateHead, EventModelReview, EventGateAudit}, e.Kind) || seen[e.Kind] {
				continue
			}
			seen[e.Kind] = true
			b, _ := toJSON(e.Detail)
			lines = append(lines, fmt.Sprintf("%s at %s: %s", e.Kind, e.At.UTC().Format(time.RFC3339), b))
		}
		if len(lines) == 0 {
			return "no gate has judged the card yet", nil
		}
		return strings.Join(lines, "\n"), nil
	case "read_file":
		return p.readFile(ctx, args)
	case "grep":
		return p.grep(ctx, args)
	}
	return "", fmt.Errorf("%s is not a tool of this conversation", name)
}

// readFile reads a file of the repository at the case's head, at main when
// the case has none.
func (p *caseTools) readFile(ctx context.Context, args map[string]any) (string, error) {
	raw, _ := args["path"].(string)
	path := pathpkg.Clean(strings.TrimPrefix(raw, "/workspace/"))
	if path == "." || path == ".." || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%q is no path inside the repository", raw)
	}
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	ref := prBase
	if p.f.Head != "" {
		ref = p.f.Head
	}
	b, err := p.r.forgejo.FileAt(ctx, p.repo.Owner, p.repo.Name, ref, strings.Join(segments, "/"))
	if err != nil {
		return "", fmt.Errorf("read %s at %s: %w", path, ref, err)
	}
	return string(b), nil
}

// grep searches the checkout of the head and main, or of main alone for a
// case with no pull request, for a fixed string as a whole word.
func (p *caseTools) grep(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", errors.New("grep needs a pattern")
	}
	ref, _ := args["ref"].(string)
	prefix, _ := args["path"].(string)
	p.checkout.once.Do(func() {
		var pr int64
		if p.f.PR > 0 && p.f.Head != "" {
			pr = p.f.PR
		}
		p.checkout.dir, p.checkout.refs, p.checkout.cleanup, p.checkout.err = checkoutRefs(ctx, p.r.forgejo, p.repo, pr, p.f.Head)
	})
	if p.checkout.err != nil {
		return "", fmt.Errorf("the repository could not be fetched: %w", p.checkout.err)
	}
	refs := p.checkout.refs
	if ref != "" {
		if refs = slices.DeleteFunc(slices.Clone(refs), func(n namedRef) bool { return n.Name != ref }); len(refs) == 0 {
			return "", fmt.Errorf("ref %q is not searched here: use head or main", ref)
		}
	}
	hits, err := gitGrep(ctx, p.checkout.dir, refs, pattern)
	if errors.Is(err, errNoMatch) {
		return "no match", nil
	}
	if err != nil {
		return "", err
	}
	var lines []string
	for _, h := range hits {
		if strings.HasPrefix(h.Path, prefix) {
			lines = append(lines, fmt.Sprintf("%s:%s:%d:%s", h.Ref, h.Path, h.Line, h.Text))
		}
	}
	if len(lines) == 0 {
		return "no match", nil
	}
	return strings.Join(lines, "\n"), nil
}

// rungResolution is the resolution answer a asks for: its decision with
// the arguments of the action's row, the draw as the row takes it, and
// the verdicts keyed by the finding's text.
func rungResolution(a RungAnswer, f CaseFacts) Resolution {
	res := Resolution{Action: a.Decision, Note: a.Note}
	var draw policy.Draw
	if a.Draw != nil {
		draw = policy.Draw{Sessions: a.Draw.Sessions, AgentTime: time.Duration(a.Draw.AgentTimeS) * time.Second, EUR: a.Draw.EUR}
	}
	switch a.Decision {
	case ActHandover:
		res.Args = map[string]any{"model": a.Model}
	case ActExtend:
		res.Args = map[string]any{"sessions": float64(draw.Sessions), "agent_time_s": draw.AgentTime.Seconds()}
		draw.Sessions, draw.AgentTime = 0, 0
	case ActFund:
		res.Args = map[string]any{"eur": draw.EUR}
		draw.EUR = 0
	}
	if !draw.IsZero() {
		res.Fund = &draw
	}
	for id, v := range a.Verdicts {
		if text := f.FindingTexts[id]; text != "" {
			if res.Verdicts == nil {
				res.Verdicts = map[string]CheckVerdict{}
			}
			res.Verdicts[text] = CheckVerdict{State: v.Verdict, Evidence: v.Evidence}
		}
	}
	return res
}

// verdictDid are the verdicts of a as steps' actions, by finding id.
func (r *Runner) verdictDid(a RungAnswer) []CaseAction {
	var did []CaseAction
	for _, id := range slices.Sorted(maps.Keys(a.Verdicts)) {
		v := a.Verdicts[id]
		did = append(did, CaseAction{Action: "verdict", Args: map[string]any{"id": id, "verdict": v.Verdict, "evidence": r.scrub(v.Evidence)}})
	}
	return did
}

// answerFound is what an answer found: how it read the stop, and each
// verdict with its evidence.
func (r *Runner) answerFound(a RungAnswer) string {
	parts := []string{"the stop reads as " + a.ReasonKind}
	for _, id := range slices.Sorted(maps.Keys(a.Verdicts)) {
		v := a.Verdicts[id]
		parts = append(parts, fmt.Sprintf("finding %s %s: %s", id, v.Verdict, v.Evidence))
	}
	return textutil.Truncate(r.scrub(strings.Join(parts, "; ")), 600)
}

// inProgress is the in-progress step of rung on steps, if any.
func inProgress(steps []CaseStep, rung int) (CaseStep, bool) {
	i := slices.IndexFunc(steps, func(s CaseStep) bool { return s.Rung == rung && s.Outcome == StepInProgress })
	if i < 0 {
		return CaseStep{}, false
	}
	return steps[i], true
}

// rungWorks is the rung, 1 or 2, that holds the open case c with a step in
// progress, 0 for none.
func (r *Runner) rungWorks(ctx context.Context, c Case) (int, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || c.Status != CaseOpen {
		return 0, err
	}
	for _, rung := range []int{1, 2} {
		if _, ok := inProgress(steps, rung); ok {
			return rung, nil
		}
	}
	return 0, nil
}

// startRung1 starts rung 1 on case c as a step in progress due after the
// rung's bound, and a goroutine that runs its conversation, once: a case
// that holds a step of rung 1 already starts none, and neither does a card
// whose cases hold limit steps of rung 1 (atLimit): the count and the step
// are one write in the store.
func (r *Runner) startRung1(ctx context.Context, t Task, c Case, limit int) (started, atLimit bool, err error) {
	lock, _ := r.caseLocks.LoadOrStore(c.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 1 }) {
		return false, false, err
	}
	due := r.clock().Add(r.cfg.Policy().WaitsOf(t.Repository).RungAnswer)
	id, atLimit, err := r.store.AppendRung1Step(ctx, t.ID, limit, CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider, Outcome: StepInProgress, DueAt: &due})
	if err != nil || atLimit {
		return false, atLimit, err
	}
	if err := r.store.SetCaseWake(ctx, c.ID, 1, nil, &due); err != nil {
		return false, false, err
	}
	r.rungs.Add(1)
	run := func() {
		defer r.rungs.Done()
		r.rung1(context.WithoutCancel(ctx), c.ID, id)
	}
	if r.launchRung1 != nil {
		r.launchRung1(run)
	} else {
		go run()
	}
	return true, false, nil
}

// rung1 runs rung 1's conversation on case caseID, whose step stepID is in
// progress, and carries out its answer through Act. Whatever does not
// resolve the case ends the step handed up and climbs.
func (r *Runner) rung1(ctx context.Context, caseID, stepID int64) {
	defer func() {
		if p := recover(); p != nil {
			r.rung1Panicked(ctx, caseID, stepID, fmt.Sprint(p))
		}
	}()
	r.rung1Run(ctx, caseID, stepID)
}

// rung1Panicked logs a panic of rung 1's conversation, scrubbed, and hands
// the case up with it named: a goroutine's panic would stop factoryd.
func (r *Runner) rung1Panicked(ctx context.Context, caseID, stepID int64, what string) {
	what = r.scrub(what)
	log.Printf("case %d: rung 1 panicked: %s", caseID, what)
	defer func() {
		if p := recover(); p != nil {
			log.Printf("case %d: rung 1's hand-up after its panic panicked: %s", caseID, r.scrub(fmt.Sprint(p)))
		}
	}()
	c, err := r.store.CaseByID(ctx, caseID)
	if err != nil {
		return
	}
	t, _, err := r.task(ctx, c.TaskID)
	if err != nil {
		return
	}
	r.rungUp(ctx, t, c, CaseFacts{}, stepID, RungOutcome{}, "", "", "rung 1 stopped on an internal error: "+what, nil)
}

func (r *Runner) rung1Run(ctx context.Context, caseID, stepID int64) {
	c, err := r.store.CaseByID(ctx, caseID)
	if err != nil || c.Status != CaseOpen {
		return
	}
	t, repo, err := r.task(ctx, c.TaskID)
	if err != nil {
		log.Printf("case %d: rung 1 reads its card: %v", caseID, r.scrub(err.Error()))
		return
	}
	var f CaseFacts
	out, err := func() (RungOutcome, error) {
		head, _ := c.Facts["head"].(string)
		model, _ := c.Facts["model"].(string)
		var err error
		if f, err = r.tripFacts(ctx, t, head, model); err != nil {
			return RungOutcome{}, err
		}
		f.atCase(c)
		return r.rungConversation(ctx, t, repo, c, f, stepID)
	}()
	if err != nil {
		r.rungUp(ctx, t, c, f, stepID, RungOutcome{}, "", "", "rung 1 could not run: "+r.scrub(err.Error()), nil)
		return
	}
	if out.End != RungAnswered {
		a := RungAnswer{}
		if out.Answer != nil {
			a = *out.Answer
		}
		found := out.Found
		if out.End == RungHandedUp {
			found = r.answerFound(a)
		}
		r.rungUp(ctx, t, c, f, stepID, out, a.Recommendation.Action, r.scrub(a.Recommendation.Grounds), found, r.verdictDid(a))
		return
	}
	a := *out.Answer
	found := r.answerFound(a)
	res := rungResolution(a, f)
	_, err = r.Act(ctx, ActInput{TaskID: t.ID, CaseID: c.ID, By: CheckinDecider, Via: ViaFactoryd, Rung: 1, Res: res,
		Read: nonNil(out.Read), Found: found, CostEUR: out.CostEUR, Did: r.verdictDid(a)})
	var refused ErrActRefused
	var ended ErrCaseEnded
	switch {
	case err == nil, errors.As(err, &ended), errors.Is(err, ErrNotFound):
		return
	case errors.As(err, &refused):
		r.rungUp(ctx, t, c, f, stepID, out, a.Recommendation.Action, r.scrub(a.Recommendation.Grounds),
			found+"; rung 1's answer "+a.Decision+" was refused: "+r.scrub(err.Error()), r.verdictDid(a))
	default:
		r.rungUp(ctx, t, c, f, stepID, out, a.Recommendation.Action, r.scrub(a.Recommendation.Grounds),
			found+"; rung 1's answer "+a.Decision+" could not be carried out: "+r.scrub(err.Error()), r.verdictDid(a))
	}
}

// judgmentTask is the project's chat task of this month, which pays the
// judgment calls: rung 1's and the check-in moments'.
func (r *Runner) judgmentTask(ctx context.Context, t Task) (Task, error) {
	v := r.cfg.Policy()
	name, project, ok := v.ProjectOf(t.Repository)
	if !ok {
		return Task{}, fmt.Errorf("repository %s names no project", t.Repository)
	}
	return r.store.EnsureChatTask(ctx, name, t.Repository, v.Document.ChatOf(project).MonthlyEUR, r.clock())
}

// rungConversation runs rung 1's conversation on case c: the statement
// from the card's facts, digest and steps, the check-in's alias and caps,
// and the calls booked on the project's chat task.
func (r *Runner) rungConversation(ctx context.Context, t Task, repo Repo, c Case, f CaseFacts, stepID int64) (RungOutcome, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return RungOutcome{}, err
	}
	steps = slices.DeleteFunc(steps, func(s CaseStep) bool { return s.ID == stepID })
	d, derr := r.CardDigest(ctx, t.ID)
	statement := r.rungStatement(c, f, DigestText(r.Scrub, d, derr), steps, 1)
	v := r.cfg.Policy()
	chat, err := r.judgmentTask(ctx, t)
	if err != nil {
		return RungOutcome{}, err
	}
	tools := &caseTools{r: r, t: t, c: c, f: f, repo: repo}
	defer tools.close()
	limits := v.Document.Limits
	out := r.runRung(ctx, r.checkin, chat.ID, r.callModel(t, policy.PurposeCheckin), f, 1, rungSystem+" "+rungToolsLine, statement,
		caseEvidence(), tools, limits.CheckinCallCap(), limits.CheckinTimeCap())
	for i := range out.Read {
		out.Read[i].Args = r.scrub(out.Read[i].Args)
	}
	return out, nil
}

// rungUp ends rung 1's step stepID handed up, with the recommendation its
// answer made as the decision and grounds, and found as the rung found
// it, and climbs: the human is asked once, with the recommendation. A step
// that ended already (expired) was climbed from by its end.
func (r *Runner) rungUp(ctx context.Context, t Task, c Case, f CaseFacts, stepID int64, out RungOutcome, decision, grounds, found string, did []CaseAction) {
	found = textutil.Truncate(r.scrub(found), 1200)
	err := r.store.EndCaseStep(ctx, CaseStep{ID: stepID, Outcome: StepHandedUp, Found: found, Did: nonNil(did), Decision: decision,
		Grounds: textutil.Truncate(grounds, 1200), CostEUR: out.CostEUR, Read: nonNil(out.Read)})
	if errors.Is(err, ErrNotFound) {
		return
	}
	if err == nil {
		_, err = r.climb(ctx, t, c, f, 1, found)
	}
	if err != nil {
		log.Printf("case %d: rung 1's hand-up: %v", c.ID, r.scrub(err.Error()))
	}
}
