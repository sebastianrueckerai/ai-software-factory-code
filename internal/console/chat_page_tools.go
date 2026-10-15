package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The console's pages as the project agent's tools (roadmap 158, ruling
// 261): every read a page makes is a read tool, never behind an OK, and
// each shows exactly what its page shows to the same member, down to the
// refusal. The writes call the same functions the pages' POST handlers
// call.

// chatTimeLayout is the minute-precision UTC stamp every page read uses.
const chatTimeLayout = "2006-01-02 15:04"

// alertSeverity is an alert's class of problem: what it is, then what
// raised it; the /alerts page's Kind and Source columns in one word.
func alertSeverity(al factoryd.Alert) string {
	s := strings.ReplaceAll(al.Kind, "_", " ")
	if al.Source != "" {
		s += " " + al.Source
	}
	return s
}

// alertTitle is an alert's one-line subject: the task it names, else its
// fingerprint.
func alertTitle(al factoryd.Alert) string {
	if al.TaskID != "" {
		return al.TaskID
	}
	if al.Fingerprint != "" {
		return al.Fingerprint
	}
	return "no task"
}

// alertsText is the /alerts page as the agent reads it: open alerts, or
// all with state all, each with id, severity, title, since and the verdict
// if any, newest first, over the same rows the page lists.
func (c *Console) alertsText(ctx context.Context, a actor, state string) (string, error) {
	switch state {
	case "open", "all":
	default:
		return "", refuse(http.StatusBadRequest, "state must be open or all")
	}
	rows, err := c.cfg.Alerts.Alerts(ctx, alertsLimit)
	if err != nil {
		log.Printf("console: alerts: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the alerts")
	}
	var b strings.Builder
	n := 0
	for _, al := range rows {
		if state != "all" && al.Verdict != "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d %s %q since %s UTC", al.ID, alertSeverity(al), alertTitle(al), al.RaisedAt.UTC().Format(chatTimeLayout))
		if al.Verdict != "" {
			b.WriteString(", verdict " + al.Verdict)
			if al.ResolvedBy != "" {
				b.WriteString(" by " + al.ResolvedBy)
			}
		} else {
			b.WriteString(", open")
		}
		b.WriteString("\n")
	}
	head, none := "Open alerts:", "No open alerts."
	if state == "all" {
		head, none = "Alerts:", "No alerts."
	}
	if n == 0 {
		return none, nil
	}
	return head + "\n" + strings.TrimSuffix(b.String(), "\n"), nil
}

// resourcesText is the /resources page as the agent reads it: each
// commissioned cloud resource with id, kind, state and cost per month by
// project, its commission's promote and release on the row the page puts
// them, promote only for an approver.
func (c *Console) resourcesText(ctx context.Context, a actor) (string, error) {
	rows, err := c.cfg.Resources.Commissions(ctx, time.Now().Add(-closedSince))
	if err != nil {
		log.Printf("console: resources: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the resource registry")
	}
	if len(rows) == 0 {
		return "No commissioned resources.", nil
	}
	actions, approver, now := actionRows(rows), c.isApprover(ctx, a.s.user, c.writersOwn()), time.Now()
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(x, y cloudres.Resource) int { return cmp.Compare(x.Repo, y.Repo) })
	var b strings.Builder
	for i, res := range rows {
		if i == 0 || rows[i-1].Repo != res.Repo {
			b.WriteString(res.Repo + ":\n")
		}
		fmt.Fprintf(&b, "  %d %s %s %.4f EUR/month", res.ID, res.Kind, res.State, res.MonthlyEUR)
		if actions[res.HoldID] == res.ID {
			if approver && promotable(res, now) {
				b.WriteString(", promotable")
			}
			b.WriteString(", releasable")
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// mirrorWords is a project's GitHub export as the projects page's export
// row says it.
func mirrorWords(m *factoryd.Mirror) string {
	if m == nil {
		return "none"
	}
	mode := "mirror"
	if m.Mode == factoryd.ModePullRequests {
		mode = "pull requests"
	}
	return m.Remote + " (" + mode + ", " + m.State + ")"
}

// projectsText is the /projects page as the agent reads it: every
// project with its name, objects, process, merge rule, state, GitHub
// export state and a factory project's environments with their deploy
// state.
func (c *Console) projectsText(ctx context.Context, a actor) (string, error) {
	ps, err := c.cfg.Projects.Projects(ctx)
	if err != nil {
		log.Printf("console: projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	}
	var b strings.Builder
	for _, x := range ps {
		state := "not registered"
		switch {
		case !x.File:
			state = "no file: kept for its tokens"
		case x.Objects == policy.ObjectsSeed:
			state = "made by the infra repository and the seed"
		case !x.ReadyAt.IsZero():
			state = "ready since " + x.ReadyAt.UTC().Format(chatTimeLayout) + " UTC"
		case x.Minted():
			state = "registered, not ready yet"
		}
		export := "none"
		if c.cfg.Mirrors != nil && x.Objects == policy.ObjectsFactory {
			m, ok, err := c.cfg.Mirrors.Mirror(ctx, x.Name, factoryd.MirrorExport)
			if err != nil {
				log.Printf("console: the export of %s: %v", x.Name, err)
				return "", refuse(http.StatusInternalServerError, "cannot read the projects' exports")
			}
			if ok {
				export = mirrorWords(&m)
			}
		}
		fmt.Fprintf(&b, "%s (%s): objects %s; process %s; merge rule %s; %s; GitHub export %s",
			x.Name, x.Repository, x.Objects, cmp.Or(x.Process, "none"), x.MergeRule, state, export)
		if x.Objects == policy.ObjectsFactory && len(x.Environments) > 0 {
			var envs []string
			for _, e := range c.envRows(ctx, x) {
				envs = append(envs, e.Words())
			}
			b.WriteString("; environments " + strings.Join(envs, ", "))
		}
		b.WriteString("\n")
	}
	if len(ps) == 0 {
		return "No projects.", nil
	}
	return "Projects (" + strconv.Itoa(len(ps)) + "):\n" + strings.TrimSuffix(b.String(), "\n"), nil
}

// policyText is the /policy page as the agent reads it: the limits and
// the budgets with each project's month cap, the risk classes with their
// paths, and the approvers.
func (c *Console) policyText(ctx context.Context, a actor) (string, error) {
	v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	switch {
	case err != nil:
		log.Printf("console: current policy: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the policy")
	case !ok:
		return "", refuse(http.StatusServiceUnavailable, "no policy version loaded")
	}
	d := v.Tree.Document
	var b strings.Builder
	fmt.Fprintf(&b, "Policy version %d (commit %s).\nLimits and budgets:\n%s", v.ID, v.Commit, limitLines(d))
	fmt.Fprintf(&b, "  day cap: €%.2f\n  month cap: €%.2f\n", d.Budget.DayEUR, d.Budget.MonthEUR)
	names := slices.Sorted(maps.Keys(v.Projects))
	for _, name := range names {
		if p := v.Projects[name]; p.Budget != nil {
			fmt.Fprintf(&b, "  project %s month cap: €%.2f\n", name, p.Budget.MonthEUR)
		}
	}
	r := d.Risk
	fmt.Fprintf(&b, "Risk classes (most severe first, default %s):\n", r.Default)
	for _, cl := range r.Classes {
		line := "  " + cl.Name + ": " + cl.Outcome
		if cl.Migration {
			line += ", migration"
		}
		b.WriteString(line + "\n")
	}
	for _, rule := range r.Rules {
		fmt.Fprintf(&b, "  %s -> %s\n", strings.Join(rule.Paths, ", "), rule.Class)
	}
	fmt.Fprintf(&b, "Approvers:\n  factory: %s\n", strings.Join(d.Approvers, ", "))
	for _, name := range names {
		if p := v.Projects[name]; len(p.Approvers) > 0 {
			fmt.Fprintf(&b, "  %s: %s\n", name, strings.Join(p.Approvers, ", "))
		}
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// costsWindow is the ledger's span a page read covers: from the first day
// of the current UTC month to today, which holds the last 14 days too.
func costsWindow(now time.Time) (from, to time.Time) {
	to = now.UTC()
	return time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC), to
}

// parseCostsDay is one costs date param, YYYY-MM-DD, in UTC; empty answers
// ok false, which means the window's default.
func parseCostsDay(v string) (time.Time, bool, bool) {
	if v == "" {
		return time.Time{}, false, true
	}
	d, err := time.Parse(policy.DateLayout, v)
	if err != nil {
		return time.Time{}, false, false
	}
	return d.UTC(), true, true
}

// costsDaysRefuse is a malformed or reversed costs date window's refusal.
func costsDaysRefuse() (string, error) {
	return "", refuse(http.StatusBadRequest, "from and to must be dates YYYY-MM-DD, from not after to")
}

// costsByText breaks the ledger down by day, card (task) or tier over
// that window, in EUR, with the totals line. A tier is a model alias, the
// ref of a model row after its purpose/ prefix; the other kinds stand as
// their kind. from and to are YYYY-MM-DD dates, both included; when either
// is empty the window is costsWindow's current month.
func (c *Console) costsByText(ctx context.Context, a actor, by, from, to string) (string, error) {
	switch by {
	case "day", "card", "tier":
	default:
		return "", refuse(http.StatusBadRequest, "by must be day, card or tier")
	}
	f, fok, ok := parseCostsDay(from)
	t, tok, ok2 := parseCostsDay(to)
	if !ok || !ok2 {
		return costsDaysRefuse()
	}
	if fok && tok && f.After(t) {
		return costsDaysRefuse()
	}
	var winFrom, winTo time.Time
	switch {
	case fok && tok:
		winFrom, winTo = f, t
	case fok:
		_, winTo = costsWindow(time.Now())
		winFrom = f
	case tok:
		winFrom, _ = costsWindow(time.Now())
		winTo = t
	default:
		winFrom, winTo = costsWindow(time.Now())
	}
	ledger, err := c.cfg.Costs.Costs(ctx, winFrom, winTo)
	if err != nil {
		log.Printf("console: costs: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the ledger")
	}
	var lines []string
	var grand float64
	switch by {
	case "day":
		for i := len(ledger.Days) - 1; i >= 0; i-- {
			d := ledger.Days[i]
			parts := make([]string, 0, len(d.ByKind))
			for _, k := range slices.Sorted(maps.Keys(d.ByKind)) {
				parts = append(parts, fmt.Sprintf("%s %.4f", k, d.ByKind[k]))
			}
			lines = append(lines, fmt.Sprintf("  %s %.4f%s", d.Day, d.Total, tailWords(parts)))
			grand += d.Total
		}
	case "card":
		for _, t := range ledger.Tasks {
			lines = append(lines, fmt.Sprintf("  %s %.4f", t.TaskID, t.EUR))
			grand += t.EUR
		}
	case "tier":
		tiers := map[string]float64{}
		for _, t := range ledger.Tasks {
			for _, l := range t.Lines {
				tier := l.Kind
				if l.Kind == policy.KindModel {
					tier = l.Ref[strings.LastIndexByte(l.Ref, '/')+1:]
				}
				tiers[tier] += l.EUR
			}
		}
		for _, k := range slices.Sorted(maps.Keys(tiers)) {
			lines = append(lines, fmt.Sprintf("  %s %.4f", k, tiers[k]))
			grand += tiers[k]
		}
	}
	if len(lines) == 0 {
		return "No spend recorded.", nil
	}
	head := map[string]string{"day": "By day", "card": "By card", "tier": "By tier"}[by]
	return head + " " + ledger.From + " to " + ledger.To + ", EUR:\n" +
		strings.Join(lines, "\n") + fmt.Sprintf("\ntotal: %.4f EUR", grand), nil
}

// tailWords is a breakdown's parts behind their total, " (model 1.00, ci 0.10)" style.
func tailWords(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// doAlertVerdict is POST /alerts/{id}/verdict's action: the factory
// approver's verdict on an open escalation alert.
func (c *Console) doAlertVerdict(ctx context.Context, a actor, id int64, v string) (string, error) {
	if v != factoryd.VerdictReal && v != factoryd.VerdictNoise {
		return "", refuse(http.StatusBadRequest, "the verdict is real or noise")
	}
	switch err := c.cfg.Alerts.SetAlertVerdict(ctx, id, v, a.s.user); {
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusConflict, fmt.Sprintf("alert %d is no open human alert (any more); reload the page", id))
	case err != nil:
		log.Printf("console: verdict on alert %d: %v", id, err)
		return "", refuse(http.StatusInternalServerError, "cannot record the verdict")
	}
	log.Printf("console: %s judged alert %d %s via %s", a.s.user, id, v, a.via)
	c.alertVerdictRecord(ctx, a, id, v)
	c.cfg.Metrics.Intervened(ctx, factoryd.InterventionAlertVerdict, "none", "none")
	return fmt.Sprintf("Alert %d is judged %s.", id, v), nil
}

// alertVerdictRecord writes alert id's verdict under the project its task
// belongs to, as the actor a.
func (c *Console) alertVerdictRecord(ctx context.Context, a actor, id int64, v string) {
	repo, taskID := c.alertRepository(ctx, id)
	if repo == "" {
		log.Printf("console: alert %d: no project to record the verdict %s under", id, v)
		return
	}
	c.record(ctx, a, repo, taskID, 0, factoryd.EventAlertVerdict, map[string]any{"alert": id, "verdict": v})
}

// doPromoteResource is the resources page's Promote button: make the
// commission standing (project-owned, no lease, no end with its task),
// which must fit the standing block of the project's file in force.
func (c *Console) doPromoteResource(ctx context.Context, a actor, id int64) (string, error) {
	if err := c.resourceChange(ctx, a, "promote", id, func(ctx context.Context, id int64, by string) error {
		v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
		if err != nil || !ok {
			return fmt.Errorf("current policy: loaded %v, %v", ok, err)
		}
		return c.cfg.Resources.Promote(ctx, id, by, func(repo string) policy.Standing { return v.EnvelopeOf(repo).Standing })
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Resource %d is standing now.", id), nil
}

// doReleaseResource is the resources page's Release button: end the
// commission's lease now; the gateway's collector deletes it on its next
// pass.
func (c *Console) doReleaseResource(ctx context.Context, a actor, id int64) (string, error) {
	if err := c.resourceChange(ctx, a, "release", id, c.cfg.Resources.EndLease); err != nil {
		return "", err
	}
	return fmt.Sprintf("Resource %d is released; the collector deletes it.", id), nil
}

// resourceChange runs the change on commission id, with its log line and
// its event row; the access table has judged who may.
func (c *Console) resourceChange(ctx context.Context, a actor, what string, id int64, change func(ctx context.Context, id int64, by string) error) error {
	if err := change(ctx, id, a.s.user); err != nil {
		log.Printf("console: %s %s resource %d: %v", a.s.user, what, id, err)
		switch {
		case errors.Is(err, cloudres.ErrLeaseEnded):
			return refuse(http.StatusConflict, "cannot "+what+": its lease has ended or it was released; the collector deletes it")
		case errors.Is(err, cloudres.ErrBeyondStanding):
			_, why, _ := strings.Cut(err.Error(), cloudres.ErrBeyondStanding.Error()+": ")
			return refuse(http.StatusConflict, "cannot "+what+": "+why+"; raise the standing block in the project's file first")
		case errors.Is(err, cloudres.ErrNotOpen):
			return refuse(http.StatusConflict, "cannot "+what+": the resource is no longer open")
		default:
			return refuse(http.StatusInternalServerError, "cannot "+what+"; see the factoryd log")
		}
	}
	log.Printf("console: %s %sd the commission of resource %d via %s", a.s.user, what, id, a.via)
	kind := factoryd.EventResourcePromoted
	if what == "release" {
		kind = factoryd.EventResourceReleased
	}
	c.resourceRecord(ctx, a, id, kind)
	return nil
}

// resourceRecord writes what a did to commission id under its own
// project.
func (c *Console) resourceRecord(ctx context.Context, a actor, id int64, kind string) {
	res, ok, err := c.cfg.Resources.Commission(ctx, id)
	switch {
	case err != nil:
		log.Printf("console: resource %d: %v", id, err)
		return
	case !ok, res.Repo == "":
		log.Printf("console: resource %d: no project to record %s under", id, kind)
		return
	}
	c.record(ctx, a, res.Repo, res.OwnerRef, 0, kind, map[string]any{"resource": id, "kind": res.Kind})
}
