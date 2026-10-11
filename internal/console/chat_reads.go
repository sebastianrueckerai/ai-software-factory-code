package console

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// cardText is the card tool's answer: the card in full, with a pointer to
// the read that holds what it only names.
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
	b.WriteString(cardLine(k))
	if k.Lane != "" {
		fmt.Fprintf(&b, "\nlane %s of process %s", k.Lane, k.Process)
	}
	if k.Accent != "" {
		b.WriteString(", accent " + k.Accent)
	}
	base := strings.TrimSuffix(t.c.cfg.ForgejoPublicURL, "/") + "/" + k.Repository
	if k.IssueNumber > 0 {
		fmt.Fprintf(&b, "\nissue %s/issues/%d", base, k.IssueNumber)
	}
	fmt.Fprintf(&b, "\nsessions: %d of %d", k.Usage.Attempts, v.Document.Limits.Sessions+k.Usage.ExtraAttempts)
	if f := capFactsFrom(k, v.Document.Limits.Sessions, bt.Escalation, all); f.capped {
		fmt.Fprintf(&b, "\nat its session cap: %d of %d sessions used, %d granted", f.attempts, f.sessions, f.extra)
	}
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
	if k.PRNumber > 0 {
		fmt.Fprintf(&b, "\npull request #%d %s/pulls/%d", k.PRNumber, base, k.PRNumber)
		if head := prHead(steps); head != "" {
			b.WriteString(", head " + head)
		}
		b.WriteString(", " + prState(steps))
	}
	for _, q := range all {
		if q.TaskID != k.ID || q.Project != t.p.Repository {
			continue
		}
		fmt.Fprintf(&b, "\n%s request %d pending", q.Kind, q.ID)
		var spec factoryd.MergeSpec
		if q.Kind == factoryd.ApprovalMerge && json.Unmarshal(q.Spec, &spec) == nil {
			fmt.Fprintf(&b, " for pull request #%d, risk %s", q.PRNumber, spec.RiskClass)
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
	if n := newestChecksCount(steps); n != "" {
		fmt.Fprintf(&b, "\nchecks: %s: see pr", n)
	}
	if k.Lane == policy.LanePlan || planChild(k) {
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

// prHead is a card's pull request's head by its timeline: the newest
// commit a session pushed, a roll committed or the merge queue synced
// to; "" for none.
func prHead(steps []map[string]any) string {
	for i := len(steps) - 1; i >= 0; i-- {
		var head any
		switch m := steps[i]; m["step"] {
		case "pushed", "roll_commit":
			head = m["commit"]
		case "queue_synced":
			head = m["to"]
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
	case k.Lane != policy.LanePlan:
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d is no plan and belongs to none.", k.IssueNumber))
	}
	rev, summary, rows, ok, err := t.c.planTable(ctx, plan.ID, 0)
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
	fmt.Fprintf(&b, "revision %d, sha256 %s\nsummary: %s", rev.Revision, rev.SHA256, summary)
	for _, row := range rows {
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
		return "", &forgejoRefusal{"read the pull request's files", err}
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

// fileLimit bounds the file the agent reads; binarySniff is how much of
// it a NUL byte marks binary in.
const (
	fileLimit   = 64 << 10
	binarySniff = 8 << 10
)

// fileText is the file tool's answer: the file at a ref of the project's
// repository, read raw as the member.
func (t *turn) fileText(in toolInput) (string, error) {
	if in.Ref == "" || in.Path == "" {
		return "", refuse(http.StatusBadRequest, "The file read needs a ref and a path.")
	}
	p := path.Clean(strings.TrimLeft(in.Path, "/"))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("The path %s leaves the repository.", in.Path))
	}
	repo, err := factoryd.SplitRepo(t.p.Repository)
	if err != nil {
		return "", err
	}
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	var raw []byte
	err = t.c.as(t.r.Context(), t.a.s, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/raw/%s?ref=%s",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), strings.Join(segments, "/"), url.QueryEscape(in.Ref)), nil, http.StatusOK, &raw)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no %s at %s.", p, in.Ref))
	}
	if err != nil {
		return "", &forgejoRefusal{"read the file", err}
	}
	if bytes.IndexByte(raw[:min(len(raw), binarySniff)], 0) >= 0 {
		return "", refuse(http.StatusConflict, fmt.Sprintf("%s at %s is binary (%s bytes).", p, in.Ref, answered(raw, len(raw))))
	}
	if len(raw) > fileLimit {
		return string(raw[:fileLimit]) + fmt.Sprintf("\n(cut at 64 KiB of %s)", answered(raw, len(raw))), nil
	}
	return string(raw), nil
}

// answered is n, a count of bytes of raw, an answer api read: "at least
// n" when api cut it.
func answered(raw []byte, n int) string {
	if len(raw) >= answerMax {
		return "at least " + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
