package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
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

// factoryAccess refuses a factory-level read to anyone but the writers of
// the factory's own code project, in the words the /alerts, /resources,
// /costs, /policy and /projects pages answer with.
func (c *Console) factoryAccess(ctx context.Context, a actor) error {
	switch _, ok, err := c.writer(ctx, a.s.user); {
	case err != nil:
		log.Printf("console: writers: %v", err)
		return refuse(http.StatusServiceUnavailable, "cannot check repository writers")
	case !ok:
		return refuse(http.StatusForbidden, "Only the writers of the factory's own code see the factory's pages.")
	}
	return nil
}

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
	if err := c.factoryAccess(ctx, a); err != nil {
		return "", err
	}
	switch state {
	case "open", "all", "":
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
	if n == 0 {
		return "No open alerts.", nil
	}
	head := "Open alerts:"
	if state == "all" {
		head = "Alerts:"
	}
	return head + "\n" + strings.TrimSuffix(b.String(), "\n"), nil
}

// promotable is whether the resources page gives res its promote button:
// leased, not released, its lease still running.
func promotable(res cloudres.Resource, now time.Time) bool {
	return res.Lifetime != cloudres.LifetimeStanding && res.ReleasedBy == "" && now.Before(res.LeaseExpires)
}

// resourcesText is the /resources page as the agent reads it: each
// commissioned cloud resource with id, kind, project, state, cost per
// month and whether it may be promoted or released, grouped by project
// like the page's tables.
func (c *Console) resourcesText(ctx context.Context, a actor) (string, error) {
	if err := c.factoryAccess(ctx, a); err != nil {
		return "", err
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the policy")
	}
	rows, err := c.cfg.Resources.Commissions(ctx, time.Now().Add(-closedSince))
	if err != nil {
		log.Printf("console: resources: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the resource registry")
	}
	repos := []string{}
	seen := map[string]bool{}
	add := func(repo string) {
		if !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}
	for _, p := range v.Projects {
		add(p.Repository)
	}
	for _, res := range rows {
		add(res.Repo)
	}
	slices.Sort(repos)
	now := time.Now()
	var b strings.Builder
	n := 0
	for _, repo := range repos {
		grouped := slices.DeleteFunc(slices.Clone(rows), func(res cloudres.Resource) bool { return res.Repo != repo })
		if len(grouped) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d):\n", repo, len(grouped))
		for _, res := range grouped {
			n++
			open := slices.Contains(cloudres.OpenStates, res.State)
			fmt.Fprintf(&b, "  %d %s %s %.4f EUR/month", res.ID, res.Kind, res.State, res.MonthlyEUR)
			switch {
			case promotable(res, now) && open:
				b.WriteString(", promotable, releasable")
			case open:
				b.WriteString(", releasable")
			default:
				b.WriteString(", closed")
			}
			b.WriteString("\n")
		}
	}
	if n == 0 {
		return "No commissioned resources.", nil
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

// projectsText is the /projects page as the agent reads it: each project
// the member may see, the coder projects for a chat (the board's rule),
// every project on the factory's own code project, with its name, objects,
// process, merge rule, state and GitHub export state.
func (c *Console) projectsText(ctx context.Context, a actor, p factoryd.Project) (string, error) {
	if err := c.factoryAccess(ctx, a); err != nil {
		return "", err
	}
	ps, err := c.cfg.Projects.Projects(ctx)
	if err != nil {
		log.Printf("console: projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	}
	if p.Repository != c.writersOwn() {
		ps = slices.DeleteFunc(ps, func(x factoryd.Project) bool { return !x.Coder() })
	}
	var b strings.Builder
	for _, x := range ps {
		state := "not registered"
		switch {
		case !x.File:
			state = "no file: kept for its tokens"
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
		fmt.Fprintf(&b, "%s %s objects %s process %s merge %s: %s, GitHub export: %s\n",
			x.Name, x.Repository, x.Objects, cmp.Or(x.Process, "none"), x.MergeRule, state, export)
	}
	if len(ps) == 0 {
		return "No projects.", nil
	}
	return "Projects (" + strconv.Itoa(len(ps)) + "):\n" + strings.TrimSuffix(b.String(), "\n"), nil
}

// policyText is the /policy page as the agent reads it: the limits, the
// budgets with each project's month cap, the risk classes with their
// paths, and the approvers. The budget lines reuse the system tool's
// wording, so no second copy of it exists.
func (c *Console) policyText(ctx context.Context, a actor, repository string) (string, error) {
	if err := c.factoryAccess(ctx, a); err != nil {
		return "", err
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		return "", refuse(http.StatusServiceUnavailable, "no policy version loaded")
	}
	d := v.Tree.Document
	var b strings.Builder
	fmt.Fprintf(&b, "Policy version %d (commit %s).\n", v.ID, v.Commit)
	fmt.Fprintf(&b, "Limits:\n  sessions per card: %d\n  agent time: %s\n", d.Limits.Sessions, d.Limits.AgentTime)
	b.WriteString("Budgets:\n")
	budget := d.Budget
	fmt.Fprintf(&b, "  task budget: €%.2f default, €%.2f maximum\n", budget.TaskDefaultEUR, budget.TaskMaxEUR)
	fmt.Fprintf(&b, "  day cap: €%.2f\n", budget.DayEUR)
	if math.IsInf(budget.MonthEUR, 1) {
		b.WriteString("  month cap: none\n")
	} else {
		fmt.Fprintf(&b, "  month cap: €%.2f\n", budget.MonthEUR)
	}
	names := slices.Sorted(maps.Keys(v.Projects))
	for _, name := range names {
		p := v.Projects[name]
		if p.Budget != nil {
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
		p := v.Projects[name]
		if len(p.Approvers) > 0 {
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

// costsByText breaks the ledger down by day, card (task) or tier (the
// ledger's kind) over that window, in EUR, with the totals line; "" keeps
// the day rows. The /costs page's day table and its cards are the same
// rows, so the tool shows what the page shows.
func (c *Console) costsByText(ctx context.Context, a actor, by string) (string, error) {
	if err := c.factoryAccess(ctx, a); err != nil {
		return "", err
	}
	switch by {
	case "", "day", "card", "tier":
	default:
		return "", refuse(http.StatusBadRequest, "by must be day, card or tier")
	}
	by = cmp.Or(by, "day")
	from, to := costsWindow(time.Now())
	ledger, err := c.cfg.Costs.Costs(ctx, from, to)
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
		kinds := map[string]float64{}
		for _, d := range ledger.Days {
			for k, eur := range d.ByKind {
				kinds[k] += eur
			}
		}
		for _, k := range slices.Sorted(maps.Keys(kinds)) {
			lines = append(lines, fmt.Sprintf("  %s %.4f", k, kinds[k]))
			grand += kinds[k]
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

// pulseText is GET /projects/{name}/pulse's fragment as the agent reads
// it: the project's newest pulse, refused where the route refuses a
// member who does not read the project.
func (c *Console) pulseText(ctx context.Context, a actor, p factoryd.Project) (string, error) {
	reads, err := c.reads(ctx, a.s, p)
	switch {
	case err != nil:
		log.Printf("console: may %s read %s: %v", a.s.user, p.Name, err)
		return "", refuse(http.StatusServiceUnavailable, "cannot check who may read the project")
	case !reads:
		return "", refuse(http.StatusForbidden, "you read no project of "+p.Repository)
	}
	if c.cfg.Pulses == nil {
		return "", refuse(http.StatusNotFound, "Project "+p.Name+" has no pulse")
	}
	row, ok, err := c.cfg.Pulses.NewestPulse(ctx, p.Repository)
	if err != nil {
		log.Printf("console: the pulse of %s: %v", p.Repository, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the pulse")
	}
	if !ok {
		return "", refuse(http.StatusNotFound, "Project "+p.Name+" has no pulse yet")
	}
	return row.Sentence, nil
}

// doAlertVerdict is POST /alerts/{id}/verdict's action: the factory
// approver's verdict on an open escalation alert.
func (c *Console) doAlertVerdict(ctx context.Context, a actor, id int64, v string) (string, error) {
	if err := c.approves(ctx, a, c.writersOwn(), "judge an alert"); err != nil {
		return "", err
	}
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
// commission standing, which must fit the project file's standing block.
func (c *Console) doPromoteResource(ctx context.Context, a actor, id int64) (string, error) {
	v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the policy")
	}
	if err := c.resourceChangeDo(ctx, a, "promote", id, func(ctx context.Context, id int64, by string) error {
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
	if err := c.resourceChangeDo(ctx, a, "release", id, c.cfg.Resources.EndLease); err != nil {
		return "", err
	}
	return fmt.Sprintf("Resource %d is released; the collector deletes it.", id), nil
}

// resourceChangeDo is resourceChange without the http.ResponseWriter: the
// change, its log line and its event row. Like the page's buttons, only
// promote needs a factory approver; a writer releases.
func (c *Console) resourceChangeDo(ctx context.Context, a actor, what string, id int64, change func(ctx context.Context, id int64, by string) error) error {
	if what == "promote" {
		if err := c.approves(ctx, a, c.writersOwn(), "authorize spend"); err != nil {
			return err
		}
	}
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
