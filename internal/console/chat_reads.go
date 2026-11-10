package console

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// cardText is the card tool's answer: the card's digest, a blank line,
// then the card in full, with a pointer to the read that holds what it
// only names.
func (t *turn) cardText(in toolInput) (string, error) {
	ctx := t.r.Context()
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	v, ok := t.c.inForce(ctx)
	if !ok {
		return "", refuse(http.StatusInternalServerError, "no policy in force")
	}
	bt, _, err := t.c.cfg.Tasks.BoardTask(ctx, k.ID)
	if err != nil {
		return "", err
	}
	deps, err := t.c.cfg.Tasks.Dependencies(ctx, k.ID)
	if err != nil {
		return "", err
	}
	dependents, err := t.c.cfg.Tasks.Dependents(ctx, k.ID)
	if err != nil {
		return "", err
	}
	steps, err := t.c.readTimeline(ctx, k.ID)
	if err != nil {
		return "", err
	}
	all, err := t.c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(factoryd.DigestText(t.c.cfg.CardDigest(ctx, k.ID)) + "\n" + cardLine(k))
	if k.Track != "" {
		fmt.Fprintf(&b, "\ntrack %s of process %s", k.Track, k.Process)
	}
	if k.Accent != "" {
		b.WriteString(", accent " + k.Accent)
	}
	base := strings.TrimSuffix(t.c.cfg.ForgejoPublicURL, "/") + "/" + k.Repository
	if k.IssueNumber > 0 {
		fmt.Fprintf(&b, "\nissue %s/issues/%d", base, k.IssueNumber)
	}
	fmt.Fprintf(&b, "\nsessions: %d of %d", k.Usage.Attempts, factoryd.SessionCap(k, v.Document.Limits.Sessions)+k.Usage.SessionsAdded())
	if f := capFactsFrom(k, v.Document.Limits.Sessions, bt.Escalation, all); f.capped {
		capName := "session cap"
		if factoryd.AtTimeCap(bt.Escalation) {
			capName = "agent-time cap"
		}
		fmt.Fprintf(&b, "\nat its %s: %d of %d sessions used, %d granted", capName, f.attempts, f.sessions, f.extra)
	}
	calls, err := t.c.cfg.Costs.ModelCalls(ctx, k.ID)
	if err != nil {
		return "", err
	}
	b.WriteString("\n" + strings.Join(costLines(calls), "\n"))
	for _, d := range deps {
		fmt.Fprintf(&b, "\ndepends on #%d (%s)", d.Issue, factoryd.StatusMeaning(d.Status))
	}
	for _, d := range dependents {
		fmt.Fprintf(&b, "\nneeded by #%d (%s)", d.Issue, factoryd.StatusMeaning(d.Status))
	}
	for _, f := range []struct{ name, v string }{{"the question it waits on", bt.Escalation}, {"the choices it offers", numbered(bt.Choices)}} {
		if f.v != "" {
			fmt.Fprintf(&b, "\n%s: %s", f.name, textutil.Truncate(f.v, 2000))
		}
	}
	qs, err := t.c.planQuestions(ctx, k)
	if err != nil {
		return "", err
	}
	asked := "\nQuestions:"
	for i, q := range qs {
		if !q.Answered() {
			b.WriteString(asked + "\n" + factoryd.QuestionLine(i+1, q))
			asked = ""
		}
	}
	if k.PRNumber > 0 {
		fmt.Fprintf(&b, "\npull request #%d %s/pulls/%d", k.PRNumber, base, k.PRNumber)
		if head := prHead(steps); head != "" {
			b.WriteString(", head " + head)
		}
		b.WriteString(", " + prState(steps))
	}
	if k.GitHubPRState != "" {
		b.WriteString("\nGitHub: ")
		if k.GitHubPR > 0 {
			fmt.Fprintf(&b, "pull request #%d %s, ", k.GitHubPR, k.GitHubPRURL)
		}
		b.WriteString(k.GitHubPRState)
	}
	for _, q := range all {
		if q.TaskID != k.ID || q.Project != t.p.Repository {
			continue
		}
		fmt.Fprintf(&b, "\n%s request %d pending", q.Kind, q.ID)
		if factoryd.BriefUnfunded(q) {
			b.WriteString("; its brief is unwritten: the card's budget cannot pay for it, and a fund queues it again")
		}
		var plan factoryd.PlanSpec
		if q.Kind == factoryd.ApprovalPlan && json.Unmarshal(q.Spec, &plan) == nil && plan.Revision > 0 {
			fmt.Fprintf(&b, " for revision %d", plan.Revision)
		}
		var spec factoryd.MergeSpec
		if q.Kind == factoryd.ApprovalMerge && json.Unmarshal(q.Spec, &spec) == nil {
			fmt.Fprintf(&b, " for pull request #%d, risk %s, review %s", q.PRNumber, spec.RiskClass, cmp.Or(spec.Review.Verdict, "none"))
			if spec.Review.Verdict != "" {
				strict := "no"
				if si := spec.Review.StrictImprovement; si != nil && *si {
					strict = "yes"
				}
				b.WriteString(", strict improvement " + strict)
			}
			if spec.HeadSHA != "" {
				b.WriteString(", head " + spec.HeadSHA[:min(len(spec.HeadSHA), 7)])
			}
			if spec.RedChecks {
				m := checksOn(steps, spec.HeadSHA)
				red := redJobs(m)
				switch {
				case m != nil && m["state"] == string(factoryd.ChecksSuccess):
					b.WriteString("; checks red at open, green since")
				case len(red) > 0:
					b.WriteString("; checks red: " + strings.Join(red, ", "))
				default:
					b.WriteString("; checks red")
				}
				if len(spec.FailedTests) > 0 {
					b.WriteString("; " + failedLine(spec.FailedTests, spec.FailedTestsMore))
				}
			}
			if len(spec.WhyHuman) > 0 {
				b.WriteString("; a human decides because " + strings.Join(spec.WhyHuman, ", "))
			}
			if spec.Recommendation != "" {
				b.WriteString("; recommendation: " + spec.Recommendation)
			}
		}
	}
	if m := newest(steps, "model_review"); m != nil {
		findings, _ := m["findings"].([]any)
		fmt.Fprintf(&b, "\nreviews: %s (%s): see pr", verdict(m), textutil.Plural(len(findings), "finding", "findings"))
	}
	if m := newest(steps, "audit"); m != nil {
		fmt.Fprintf(&b, "\naudit: %s: see pr", verdict(m))
	}
	if n := newestChecksCount(steps, pushedHead(steps)); n != "" {
		fmt.Fprintf(&b, "\nchecks: %s: see pr", n)
	}
	if k.Track == policy.TrackPlan || planChild(k) {
		b.WriteString("\nplan: see plan")
	}
	if n := len(every(steps, "session_start")); n > 0 {
		fmt.Fprintf(&b, "\nsessions: %d: see session", n)
	}
	for _, f := range []struct{ name, v string }{{"objective", k.Objective}, {"the agent's summary", k.AgentSummary}} {
		if f.v != "" {
			fmt.Fprintf(&b, "\n%s: %s", f.name, textutil.Truncate(f.v, 2000))
		}
	}
	return b.String(), nil
}

// planChild: t is a card a plan filed.
func planChild(t factoryd.Task) bool { return t.Source == factoryd.SourcePlan && t.Parent != "" }

// every are the steps named step, oldest first.
func every(steps []map[string]any, step string) []map[string]any {
	var out []map[string]any
	for _, m := range steps {
		if m["step"] == step {
			out = append(out, m)
		}
	}
	return out
}

// newest is the newest of the steps named step, nil for none.
func newest(steps []map[string]any, step string) map[string]any {
	if all := every(steps, step); len(all) > 0 {
		return all[len(all)-1]
	}
	return nil
}

// verdict is a review or audit step's verdict.
func verdict(m map[string]any) string {
	v, _ := m["verdict"].(string)
	return cmp.Or(v, "no verdict")
}

// checksOn is the newest checks step on head, nil for none.
func checksOn(steps []map[string]any, head string) map[string]any {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i]["step"] == "checks" && steps[i]["commit"] == head {
			return steps[i]
		}
	}
	return nil
}

// redJobs are the jobs checks step m found neither passed nor skipped.
func redJobs(m map[string]any) []string {
	var red []string
	jobs, _ := m["jobs"].([]any)
	for _, j := range jobs {
		job, _ := j.(map[string]any)
		if st, _ := job["status"].(string); st != "success" && st != "skipped" {
			name, _ := job["name"].(string)
			red = append(red, name)
		}
	}
	return red
}

// prHead is a card's pull request's head by its timeline: the newest
// commit a session pushed, a roll committed, the merge queue synced to or
// a re-run of the checks made; "" for none.
func prHead(steps []map[string]any) string {
	for i := len(steps) - 1; i >= 0; i-- {
		var head any
		switch m := steps[i]; m["step"] {
		case "pushed", "roll_commit":
			head = m["commit"]
		case "queue_synced":
			head = m["to"]
		case "checks_rerun":
			head = m["head"]
		}
		if s, _ := head.(string); s != "" {
			return s
		}
	}
	return ""
}

// prState is how a card's pull request stands by its timeline; a signal
// factoryd ignored is another pull request's.
func prState(steps []map[string]any) string {
	for i := len(steps) - 1; i >= 0; i-- {
		switch m := steps[i]; {
		case m["step"] == "merged" || m["step"] == "signal" && m["signal"] == factoryd.SignalPRMerged && m["ignored"] == nil:
			return "merged"
		case m["step"] == "signal" && m["signal"] == factoryd.SignalPRClosed && m["ignored"] == nil:
			return "closed"
		}
	}
	return "open"
}

// planText is the plan tool's answer: the newest revision of card in's
// plan, or of the plan it is a card of, with the cards filed.
func (t *turn) planText(in toolInput) (string, error) {
	ctx := t.r.Context()
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	plan := k
	switch {
	case planChild(k):
		if plan, err = t.c.cardIn(ctx, t.a, k.Parent); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "card of plan #%d\n", plan.IssueNumber)
	case k.Track != policy.TrackPlan:
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d is no plan and belongs to none.", k.IssueNumber))
	}
	rev, p, ok, err := t.c.planTable(ctx, plan.ID, 0)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Plan #%d has no revision yet.", plan.IssueNumber))
	}
	cards, err := t.c.cfg.Tasks.PlanCards(ctx, plan.ID)
	if err != nil {
		return "", err
	}
	issues := map[string]int64{}
	for _, c := range cards {
		issues[c.PlanKey] = c.IssueNumber
	}
	fmt.Fprintf(&b, "revision %d, sha256 %s\nsummary: %s", rev.Revision, rev.SHA256, p.Summary)
	if items := factoryd.RoundItems(p, cards); len(items) > 0 {
		b.WriteString("\nrounds:")
		for _, item := range items {
			b.WriteString("\n- " + item.Text)
		}
	}
	if len(p.TakeOffMain) > 0 {
		b.WriteString("\ntakes off main: " + strings.Join(p.TakeOffMain, ", "))
	}
	if totals, err := t.c.cfg.Costs.CallTotals(ctx, []string{plan.ID}); err != nil {
		return "", err
	} else if line := callsLine(totals[plan.ID]); line != "" {
		b.WriteString("\n" + line + " to date")
	}
	qs, err := t.c.cfg.Tasks.PlanQuestions(ctx, plan.ID, rev.Revision)
	if err != nil {
		return "", err
	}
	for i, q := range qs {
		b.WriteString("\n" + factoryd.QuestionLine(i+1, q))
		if q.Answered() {
			fmt.Fprintf(&b, " answered: %s (%s)", q.Reply(), q.AnsweredBy)
		}
	}
	for n := rev.Revision - 1; len(qs) == 0 && n > 0; n-- {
		earlier, err := t.c.cfg.Tasks.PlanQuestions(ctx, plan.ID, n)
		if err != nil {
			return "", err
		}
		var answers []string
		for i, q := range earlier {
			if q.Answered() {
				answers = append(answers, fmt.Sprintf("\n%d. %s -> %s (%s)", i+1, q.Text, q.Reply(), q.AnsweredBy))
			}
		}
		if len(answers) > 0 {
			fmt.Fprintf(&b, "\nbuilt from the answers of revision %d:%s", n, strings.Join(answers, ""))
			break
		}
	}
	for _, row := range factoryd.PlanRows(p) {
		issue := "not filed"
		if n := issues[row.Key]; n > 0 {
			issue = fmt.Sprintf("issue #%d", n)
		}
		fmt.Fprintf(&b, "\n%s %s %q %s depends on %s %s", row.Key, row.Role, row.Title, row.Size, cmp.Or(strings.Join(row.DependsOn, ", "), "none"), issue)
	}
	for _, c := range cards {
		b.WriteString("\n" + cardLine(c))
	}
	return b.String(), nil
}

// prText is the pr tool's answer: card in's pull request as its timeline
// and Forgejo know it.
func (t *turn) prText(in toolInput) (string, error) {
	ctx := t.r.Context()
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	if k.PRNumber == 0 {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d has no pull request.", k.IssueNumber))
	}
	steps, err := t.c.readTimeline(ctx, k.ID)
	if err != nil {
		return "", err
	}
	files, more, err := t.c.prFiles(t.r, k.Repository, k.PRNumber)
	if err != nil {
		return "", &forgejoRefusal{what: "read the pull request's files", err: err}
	}
	field := func(m map[string]any, key string) string { s, _ := m[key].(string); return s }
	var b strings.Builder
	fmt.Fprintf(&b, "pull request #%d of card #%d\nchecks: ", k.PRNumber, k.IssueNumber)
	if m := newest(steps, "checks"); m != nil {
		b.WriteString(cmp.Or(field(m, "state"), "no state"))
		jobs, _ := m["jobs"].([]any)
		for _, j := range jobs {
			job, _ := j.(map[string]any)
			run, _ := job["run_s"].(float64)
			fmt.Fprintf(&b, "\n  %s %s %d s", field(job, "name"), field(job, "status"), int(run))
		}
	} else {
		b.WriteString("none")
	}
	reviews := every(steps, "model_review")
	if len(reviews) == 0 {
		b.WriteString("\nreview: none")
	}
	for i := len(reviews) - 1; i >= 0; i-- {
		m := reviews[i]
		fmt.Fprintf(&b, "\nreview round %d: %s", i+1, verdict(m))
		if i < len(reviews)-1 {
			continue
		}
		if s := field(m, "summary"); s != "" {
			b.WriteString(": " + s)
		}
		findings, _ := m["findings"].([]any)
		for n, f := range findings {
			fmt.Fprintf(&b, "\n  %d. %v", n+1, f)
		}
		if s := field(m, "note"); s != "" {
			b.WriteString("\n  note: " + s)
		}
	}
	b.WriteString("\naudit: ")
	if m := newest(steps, "audit"); m != nil {
		b.WriteString(verdict(m))
		findings, _ := m["findings"].([]any)
		for _, f := range findings {
			finding, _ := f.(map[string]any)
			at := field(finding, "file")
			if line, _ := finding["line"].(float64); line > 0 {
				at += fmt.Sprintf(":%d", int(line))
			}
			fmt.Fprintf(&b, "\n  %s: %s", strings.TrimSpace(at+" "+field(finding, "item")), field(finding, "why"))
		}
	} else {
		b.WriteString("none")
	}
	var added, deleted int
	for _, f := range files {
		fmt.Fprintf(&b, "\n%s +%d -%d", f.Filename, f.Additions, f.Deletions)
		added, deleted = added+f.Additions, deleted+f.Deletions
	}
	fmt.Fprintf(&b, "\n%s, +%d -%d", textutil.Plural(len(files), "file", "files"), added, deleted)
	if more {
		b.WriteString(" (more files than listed)")
	}
	if in.WithDiff {
		diff, err := t.diff(in)
		if err != nil {
			return "", err
		}
		b.WriteString("\n\n" + diff)
	}
	return b.String(), nil
}

// ciText is the ci tool's answer: the combined status of commit in.SHA,
// else of ref in.Ref's head (default main), read as the member.
func (t *turn) ciText(in toolInput) (string, error) {
	ref, sha := cmp.Or(in.Ref, "main"), strings.ToLower(in.SHA)
	if len(ref) >= 200 || strings.Contains(ref, "..") {
		return "", refuse(http.StatusBadRequest, `The ref must be a branch name under 200 bytes, without "..".`)
	}
	if sha != "" && (len(sha) < 7 || len(sha) > 64 || strings.Trim(sha, "0123456789abcdef") != "") {
		return "", refuse(http.StatusBadRequest, "The sha must be 7 to 64 hex digits.")
	}
	repo, err := factoryd.SplitRepo(t.p.Repository)
	if err != nil {
		return "", err
	}
	ctx, at := t.r.Context(), fmt.Sprintf("/api/v1/repos/%s/%s/", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	head := "commit "
	if sha == "" {
		var branch struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		err := t.c.as(ctx, t.a.s, http.MethodGet, at+"branches/"+url.PathEscape(ref), nil, http.StatusOK, &branch)
		if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no branch %s.", ref))
		}
		if err != nil {
			return "", &forgejoRefusal{what: "read the branch", err: err}
		}
		sha, head = branch.Commit.ID, ref+" at "
	}
	var cs struct {
		State    string                  `json:"state"`
		Statuses []factoryd.CommitStatus `json:"statuses"`
	}
	if err := t.c.as(ctx, t.a.s, http.MethodGet, at+"commits/"+url.PathEscape(sha)+"/status", nil, http.StatusOK, &cs); err != nil {
		return "", &forgejoRefusal{what: "read the commit's status", err: err}
	}
	head += sha[:min(len(sha), 7)]
	if len(cs.Statuses) == 0 {
		return head + ": no CI status yet", nil
	}
	lines := []string{head + ": " + cs.State}
	for _, s := range cs.Statuses {
		line := s.Context + ": " + s.State
		if s.Description != "" {
			line += " (" + s.Description + ")"
		}
		if s.TargetURL != "" {
			line += " " + s.TargetURL
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// fileLimit bounds the file the agent reads; binarySniff is how much of
// it a NUL byte marks binary in.
const (
	fileLimit   = 64 << 10
	binarySniff = 8 << 10
)

// rawRoute is Forgejo's raw file route of path p at ref in repository
// owner/name, every segment of p escaped.
func rawRoute(repository, p, ref string) (string, error) {
	repo, err := factoryd.SplitRepo(repository)
	if err != nil {
		return "", err
	}
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("/api/v1/repos/%s/%s/raw/%s?ref=%s", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), strings.Join(segments, "/"), url.QueryEscape(ref)), nil
}

// fileText is the file tool's answer: the file at a ref of the project's
// repository, read raw as the member, its first 64 KiB with the line to
// read on from, or its lines from..to as one numbered window under the
// same cap.
func (t *turn) fileText(in toolInput) (string, error) {
	if in.Ref == "" || in.Path == "" {
		return "", refuse(http.StatusBadRequest, "The file read needs a ref and a path.")
	}
	p := path.Clean(strings.TrimLeft(in.Path, "/"))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("The path %s leaves the repository.", in.Path))
	}
	route, err := rawRoute(t.p.Repository, p, in.Ref)
	if err != nil {
		return "", err
	}
	var raw []byte
	err = t.c.as(t.r.Context(), t.a.s, http.MethodGet, route, nil, http.StatusOK, &raw)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no %s at %s.", p, in.Ref))
	}
	if err != nil {
		return "", &forgejoRefusal{what: "read the file", err: err}
	}
	if bytes.IndexByte(raw[:min(len(raw), binarySniff)], 0) >= 0 {
		return "", refuse(http.StatusConflict, fmt.Sprintf("%s at %s is binary (%s bytes).", p, in.Ref, answered(raw, len(raw))))
	}
	if in.Ask != "" {
		label := fmt.Sprintf("%s at %s", p, in.Ref)
		if cut := cutText(raw, fileLimit); len(cut) < len(raw) {
			label = fmt.Sprintf("%s (the first %s of %s)", label, imageSize(int64(len(cut))), imageSize(int64(len(raw))))
			raw = cut
		}
		return t.withDigest(label, in.Ask, string(raw))
	}
	if s, ok := lineWindow(fmt.Sprintf("%s at %s", p, in.Ref), raw, in.From, in.To, fileLimit); ok {
		return s, nil
	}
	if len(raw) > fileLimit {
		next := bytes.Count(raw[:fileLimit], []byte{'\n'}) + 1
		return string(raw[:fileLimit]) + fmt.Sprintf("\n(cut at 64 KiB of %s bytes; read on from line %d)", answered(raw, len(raw)), next), nil
	}
	return string(raw), nil
}

// splitLines splits raw into its lines, dropping the empty one a final
// newline adds.
func splitLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineWindow answers name's lines from..to under limit, its head naming
// the whole; asked no range it answers "" and reports false.
func lineWindow(name string, raw []byte, from, to, limit int) (string, bool) {
	if from == 0 && to == 0 {
		return "", false
	}
	lines := splitLines(raw)
	head := fmt.Sprintf("%s: %s bytes, %d lines.", name, answered(raw, len(raw)), len(lines))
	if from < 0 || to < 0 || from > len(lines) || (from > 0 && to > 0 && from > to) {
		return head + fmt.Sprintf(" from %d and to %d name no line range of its %d lines: name from at or before to, inside lines 1 to %d.", from, to, len(lines), len(lines)), true
	}
	var b strings.Builder
	size := 0
	for i := max(from, 1); i <= min(cmp.Or(to, len(lines)), len(lines)); i++ {
		if l := lines[i-1]; size+len(l) > limit {
			fmt.Fprint(&b, "… a narrower range fits: raise from or lower to.\n")
			break
		} else {
			size += len(l)
			fmt.Fprintf(&b, "line %d: %s\n", i, l)
		}
	}
	return head + "\n\n" + b.String(), true
}

// docText is the read_doc tool's answer: an asked read digests the
// document's text under it on the small alias, else the read as today.
func (t *turn) docText(in toolInput) (string, error) {
	if in.Ask != "" {
		repo, err := t.c.docRepoOf(t.cfg.DocRepo, in.Path)
		if err != nil {
			return "", err
		}
		content, _, present, err := t.c.docFile(t.r.Context(), repo, in.Path)
		switch {
		case err != nil:
			return "", err
		case present:
			label := in.Path
			if cut := cutText([]byte(content), docShown); len(cut) < len(content) {
				label = fmt.Sprintf("%s (the first %s of %s)", label, imageSize(int64(len(cut))), imageSize(int64(len(content))))
				content = string(cut)
			}
			return t.withDigest(label, in.Ask, content)
		}
	}
	return t.c.readDoc(t.r.Context(), t.cfg.DocRepo, in.Path, in.Find, in.From, in.To, in.Regex)
}

// subPrefix cleans a sub-tree prefix, the tree and grep reads' path
// argument: "" is the whole tree, a path that leaves the repository a
// refusal.
func subPrefix(in toolInput) (string, error) {
	if in.Path == "" {
		return "", nil
	}
	p := path.Clean(strings.TrimLeft(in.Path, "/"))
	switch {
	case p == ".":
		return "", nil
	case p == ".." || strings.HasPrefix(p, "../"):
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("The path %s leaves the repository.", in.Path))
	}
	return p, nil
}

// treeCut bounds the tree read's lines: the first treeCut files, the
// count line naming the rest.
const treeCut = 500

// treeText is the tree tool's answer: the project repository's whole
// tree at a ref, one line a file with its size, the first treeCut of
// them, the count line naming the rest and Forgejo's own truncation.
func (t *turn) treeText(in toolInput) (string, error) {
	if in.Ref == "" {
		return "", refuse(http.StatusBadRequest, "The tree read needs a ref.")
	}
	prefix, err := subPrefix(in)
	if err != nil {
		return "", err
	}
	repo, err := factoryd.SplitRepo(t.p.Repository)
	if err != nil {
		return "", err
	}
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	err = t.c.as(t.r.Context(), t.a.s, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/git/trees/%s?recursive=true",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(in.Ref)), nil, http.StatusOK, &tree)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no tree at %s.", in.Ref))
	}
	if err != nil {
		return "", &forgejoRefusal{what: "read the tree", err: err}
	}
	var b strings.Builder
	files := 0
	for _, e := range tree.Tree {
		if e.Type != "blob" || (prefix != "" && !strings.HasPrefix(e.Path, prefix)) {
			continue
		}
		files++
		if files <= treeCut {
			fmt.Fprintf(&b, "%s %d\n", e.Path, e.Size)
		}
	}
	if files == 0 {
		if prefix == "" {
			return fmt.Sprintf("%s holds no files.", in.Ref), nil
		}
		return fmt.Sprintf("There are no files under %s at %s.", prefix, in.Ref), nil
	}
	switch {
	case files > treeCut && tree.Truncated:
		fmt.Fprintf(&b, "(cut at %d of %d %s; Forgejo's tree answer is itself truncated)\n", treeCut, files, plural(files, "file"))
	case files > treeCut:
		fmt.Fprintf(&b, "(cut at %d of %d %s)\n", treeCut, files, plural(files, "file"))
	case tree.Truncated:
		b.WriteString("(Forgejo's tree answer is itself truncated)\n")
	}
	return b.String(), nil
}

// The grep read's bounds: the tarball it reads is at most archiveLimit
// bytes, the walk opens at most grepFiles files and of each its first
// fileLimit bytes, stops at grepMatches and shows a line's first
// grepLine characters.
const (
	archiveLimit = 100 << 20
	grepFiles    = 2000
	grepMatches  = 50
	grepLine     = 200
)

// grepText is the grep tool's answer: the files of the project's
// repository at a ref whose lines match a pattern, read from the ref's
// one tarball in memory, one line a match and a count line naming every
// bound of the walk.
func (t *turn) grepText(in toolInput) (string, error) {
	if in.Pattern == "" || in.Ref == "" {
		return "", refuse(http.StatusBadRequest, "The grep read needs a pattern and a ref.")
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("The grep pattern %q does not compile: %v.", in.Pattern, err))
	}
	prefix, err := subPrefix(in)
	if err != nil {
		return "", err
	}
	repo, err := factoryd.SplitRepo(t.p.Repository)
	if err != nil {
		return "", err
	}
	raw, err := t.c.callAs(t.r.Context(), t.a.s, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/archive/%s.tar.gz",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(in.Ref)), nil, http.StatusOK, archiveLimit+1)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no archive at %s.", in.Ref))
	}
	if err != nil {
		return "", &forgejoRefusal{what: "read the archive", err: err}
	}
	if int64(len(raw)) > archiveLimit {
		return "", refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("The archive of %s is over the grep read's limit of %s.", in.Ref, imageSize(archiveLimit)))
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", &forgejoRefusal{what: "read the archive", err: err}
	}
	tr := tar.NewReader(gz)
	var b strings.Builder
	files, opened, matches := 0, 0, 0
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", &forgejoRefusal{what: "read the archive", err: err}
		}
		if hd.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(hd.Name, "./")
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		files++
		if opened >= grepFiles || matches >= grepMatches {
			continue
		}
		head := make([]byte, fileLimit)
		n, _ := io.ReadFull(tr, head)
		head, opened = head[:n], opened+1
		if bytes.IndexByte(head[:min(n, binarySniff)], 0) >= 0 {
			continue
		}
		lines := strings.Split(string(head), "\n")
		if bytes.HasSuffix(head, []byte{'\n'}) {
			lines = lines[:len(lines)-1]
		}
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			matches++
			if matches > grepMatches {
				break
			}
			fmt.Fprintf(&b, "%s:%d:%s\n", name, i+1, string(cutText([]byte(line), grepLine)))
		}
	}
	shown, cut := min(matches, grepMatches), ""
	if files > opened || matches > grepMatches {
		cut = ", cut"
	}
	matchWord := "matches"
	if shown == 1 {
		matchWord = "match"
	}
	fmt.Fprintf(&b, "(scanned %d of %d %s, %d %s%s)\n", opened, files, plural(files, "file"), shown, matchWord, cut)
	return b.String(), nil
}

// answered is n, a count of bytes of raw, an answer api read: "at least
// n" when api cut it.
func answered(raw []byte, n int) string {
	if len(raw) >= answerMax {
		return "at least " + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// maxWebResults bounds one web search's pages; pageLimit bounds a fetched
// page before its digest.
const (
	maxWebResults = 10
	pageLimit     = 64 << 10
)

// webSearch is one paid web search on the chat task's account: the
// findings' titles, urls and snippets go into the thread as found, no
// digest between.
func (t *turn) webSearch(in toolInput) (string, error) {
	if strings.TrimSpace(in.Query) == "" {
		return "", &refusal{http.StatusBadRequest, "The web search needs a query."}
	}
	if in.Limit < 0 || in.Limit > maxWebResults {
		return "", &refusal{http.StatusBadRequest, "The web search's limit is 1 to 10."}
	}
	limit := cmp.Or(in.Limit, 5)
	text, err := t.c.cfg.Web.Search(t.r.Context(), t.task, in.Query, limit)
	if err != nil {
		return "", t.webErr(err)
	}
	var pages []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
	}
	if json.Unmarshal([]byte(text), &pages) == nil {
		if len(pages) == 0 {
			return "The web search found nothing.", nil
		}
		var b strings.Builder
		for i, p := range pages {
			fmt.Fprintf(&b, "%d. %s\n%s\n%s\n", i+1, cmp.Or(p.Title, "(no title)"), p.URL, strings.TrimSpace(p.Description))
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	// the door's answer is not the findings' list: it goes in whole, cut
	if strings.TrimSpace(text) == "" {
		return "The web search found nothing.", nil
	}
	return string(cutText([]byte(text), pageLimit)), nil
}

// webScrape is one paid page fetch on the chat task's account: the page
// never enters the thread, one digest call on the cheap alias answers the
// ask under the page's url as its label.
func (t *turn) webScrape(in toolInput) (string, error) {
	if strings.TrimSpace(in.URL) == "" {
		return "", &refusal{http.StatusBadRequest, "The web page read needs a url."}
	}
	page, err := t.c.cfg.Web.Scrape(t.r.Context(), t.task, in.URL)
	if err != nil {
		return "", t.webErr(err)
	}
	if strings.TrimSpace(page) == "" {
		return "The web page at " + in.URL + " answered nothing.", nil
	}
	if len(page) > pageLimit {
		page = string(cutText([]byte(page), pageLimit)) + "\n\n(the page was cut at 64 KiB)"
	}
	return t.withDigest(in.URL, cmp.Or(in.Ask, "Summarise this page."), page)
}

// webErr words a web door failure for the member: a refusal the policy
// chose names itself, a status the door answered names its code, and any
// other failure keeps its own shape for the dispatch's sentence.
func (t *turn) webErr(err error) error {
	var ref *policy.Refusal
	var st *factoryd.StatusError
	switch {
	case errors.As(err, &ref):
		return &refusal{http.StatusPaymentRequired, "The web tool was refused: " + ref.Reason + "."}
	case errors.As(err, &st):
		return &refusal{http.StatusBadGateway, "The web tool's call failed with status " + strconv.Itoa(st.Code) + "."}
	}
	return err
}

// failedLine names the first chatFailedTests of a merge request's red
// entries and counts the rest.
func failedLine(tests []factoryd.FailedTest, more int) string {
	shown := tests[:min(len(tests), chatFailedTests)]
	parts := make([]string, len(shown))
	for i, f := range shown {
		parts[i] = f.String()
	}
	line := "red in the logs: " + strings.Join(parts, ", ")
	if rest := len(tests) - len(shown) + more; rest > 0 {
		line += fmt.Sprintf(", and %d more", rest)
	}
	return line
}

const chatFailedTests = 10
