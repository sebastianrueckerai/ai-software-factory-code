package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// closedSince is how long /resources keeps showing a closed resource.
const closedSince = 7 * 24 * time.Hour

type resourceRow struct {
	cloudres.Resource
	Open bool
	// Actions marks the row carrying its commission's promote and release
	// (the server while it is open): both act on the whole commission.
	Actions bool
	// PromoteNote is what promotion would cost a month against the
	// project's envelope (on the Actions row).
	PromoteNote string
	// Promotable: leased, not released, lease still running.
	Promotable bool
}

// envelopeView is a project's envelope as its file says, the default for a
// repository no file names, and what the project holds against it.
type envelopeView struct {
	policy.Envelope
	Use         cloudres.EnvelopeUse
	StandingUse cloudres.StandingUse
	// File links the project's file at the version in force; "" for the
	// default envelope.
	File string
}

// envelopeOf is repo's envelope in version v with its use.
func (c *Console) envelopeOf(ctx context.Context, v policy.Version, repo string) (envelopeView, error) {
	e := envelopeView{Envelope: v.EnvelopeOf(repo)}
	if name, _, ok := v.ProjectOf(repo); ok {
		e.File = c.fileLink(v, name)
	}
	var err, serr error
	e.Use, err = c.cfg.Resources.Use(ctx, repo)
	e.StandingUse, serr = c.cfg.Resources.Standing(ctx, repo)
	return e, errors.Join(err, serr)
}

// fileLink is project name's file at version v's commit.
func (c *Console) fileLink(v policy.Version, name string) string {
	return c.configTree(v.Commit) + "/" + policy.DirProjects + "/" + name + ".yaml"
}

type projectView struct {
	Project  string // the repository
	Envelope envelopeView
	Rows     []resourceRow
}

// jobsView is the cluster's jobs, or Failed when the cluster did not
// answer.
type jobsView struct {
	Rows   []factoryd.CronJobState
	Failed bool
}

// jobSchedule is a job's cron in words, as a routine's card says it; a
// ScheduledBackup's six-field cron loses its seconds first when they are
// 0.
func jobSchedule(cron string) string {
	if f := strings.Fields(cron); len(f) == 6 && f[0] == "0" {
		cron = strings.Join(f[1:], " ")
	}
	return policy.Trigger{Cron: cron}.Words()
}

// jobAge is how long ago t was, coarse; never when t is zero.
func jobAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return coarse(time.Since(t)) + " ago"
}

// resources lists the agent project's commissions by project and task,
// with each project's envelope, read-only: it is the project's file.
func (c *Console) resources(w http.ResponseWriter, r *http.Request) {
	rows, err := c.cfg.Resources.Commissions(r.Context(), time.Now().Add(-closedSince))
	if err != nil {
		log.Printf("console: resources: %v", err)
		http.Error(w, "cannot read the resource registry", http.StatusInternalServerError)
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	byProject := map[string]*projectView{}
	view := func(repo string) *projectView {
		if pv, ok := byProject[repo]; ok {
			return pv
		}
		pv := &projectView{Project: repo}
		byProject[repo] = pv
		return pv
	}
	for _, p := range v.Projects {
		view(p.Repository)
	}
	actions, now := actionRows(rows), time.Now()
	monthly := map[int64]float64{}
	for _, res := range rows {
		open := slices.Contains(cloudres.OpenStates, res.State)
		if open {
			monthly[res.HoldID] += res.MonthlyEUR
		}
		pv := view(res.Repo)
		pv.Rows = append(pv.Rows, resourceRow{Resource: res, Open: open, Actions: actions[res.HoldID] == res.ID, Promotable: promotable(res, now)})
	}
	var projects []*projectView
	for repo, pv := range byProject {
		if pv.Envelope, err = c.envelopeOf(r.Context(), v, repo); err != nil {
			log.Printf("console: envelope use of %s: %v", repo, err)
			http.Error(w, "cannot read the envelopes' use", http.StatusInternalServerError)
			return
		}
		for i := range pv.Rows {
			if pv.Rows[i].Actions {
				pv.Rows[i].PromoteNote = fmt.Sprintf("standing, about %.4f EUR a month against the standing block's %.2f EUR (%.4f standing now)",
					monthly[pv.Rows[i].HoldID], pv.Envelope.Standing.MonthlyEUR, pv.Envelope.StandingUse.MonthlyEUR)
			}
		}
		projects = append(projects, pv)
	}
	slices.SortFunc(projects, func(a, b *projectView) int { return cmp.Compare(a.Project, b.Project) })
	var jobs *jobsView
	if c.cfg.Jobs != nil {
		jobs = &jobsView{}
		if jobs.Rows, err = c.cfg.Jobs.CronJobs(r.Context()); err != nil {
			log.Printf("console: the cluster's jobs: %v", err)
			jobs.Failed = true
		}
	}
	s := current(r)
	c.render(w, "resources.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Projects": projects, "Jobs": jobs,
		"Approver": c.approver(r)})
}

// resourceButton is a resources page button: do on the path's commission,
// then back to the page.
func (c *Console) resourceButton(do func(context.Context, actor, int64) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, err := do(r.Context(), button(r), id); err != nil {
			c.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/resources", http.StatusSeeOther)
	}
}

// actionRows maps each open commission (hold) to the resource whose row
// carries its promote and release: its server, else its first open row.
func actionRows(rows []cloudres.Resource) map[int64]int64 {
	actions := map[int64]int64{}
	server := map[int64]bool{}
	for _, res := range rows {
		if !slices.Contains(cloudres.OpenStates, res.State) || res.HoldID == 0 || server[res.HoldID] {
			continue
		}
		if _, ok := actions[res.HoldID]; !ok || res.Kind == cloudres.KindServer {
			actions[res.HoldID], server[res.HoldID] = res.ID, res.Kind == cloudres.KindServer
		}
	}
	return actions
}

// promotable is whether res may be made standing: leased, not released,
// its lease still running.
func promotable(res cloudres.Resource, now time.Time) bool {
	return res.Lifetime != cloudres.LifetimeStanding && res.ReleasedBy == "" && now.Before(res.LeaseExpires)
}
