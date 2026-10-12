package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
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
	return c.cfg.ConfigRepo + "/src/commit/" + url.PathEscape(v.Commit) + "/" + c.cfg.ConfigDir + "/" + policy.DirProjects + "/" + name + ".yaml"
}

type projectView struct {
	Project  string // the repository
	Envelope envelopeView
	Rows     []resourceRow
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
	actions := map[int64]int{} // commission (hold) -> index of its row with the buttons
	monthly := map[int64]float64{}
	for _, res := range rows {
		if slices.Contains(cloudres.OpenStates, res.State) {
			monthly[res.HoldID] += res.MonthlyEUR
		}
		pv := view(res.Repo)
		row := resourceRow{Resource: res, Open: slices.Contains(cloudres.OpenStates, res.State),
			Promotable: res.Lifetime != cloudres.LifetimeStanding && res.ReleasedBy == "" && time.Now().Before(res.LeaseExpires)}
		pv.Rows = append(pv.Rows, row)
		i := len(pv.Rows) - 1
		if !row.Open || res.HoldID == 0 {
			continue
		}
		if j, ok := actions[res.HoldID]; !ok || (res.Kind == cloudres.KindServer && pv.Rows[j].Kind != cloudres.KindServer) {
			actions[res.HoldID] = i
		}
	}
	var projects []*projectView
	for repo, pv := range byProject {
		if pv.Envelope, err = c.envelopeOf(r.Context(), v, repo); err != nil {
			log.Printf("console: envelope use of %s: %v", repo, err)
			http.Error(w, "cannot read the envelopes' use", http.StatusInternalServerError)
			return
		}
		for i := range pv.Rows {
			if j, ok := actions[pv.Rows[i].HoldID]; ok && j == i && pv.Rows[i].Open {
				pv.Rows[i].Actions = true
				pv.Rows[i].PromoteNote = fmt.Sprintf("standing, about %.4f EUR a month against the standing block's %.2f EUR (%.4f standing now)",
					monthly[pv.Rows[i].HoldID], pv.Envelope.Standing.MonthlyEUR, pv.Envelope.StandingUse.MonthlyEUR)
			}
		}
		projects = append(projects, pv)
	}
	slices.SortFunc(projects, func(a, b *projectView) int { return cmp.Compare(a.Project, b.Project) })
	s := current(r)
	c.render(w, "resources.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Projects": projects, "Approver": c.approver(r)})
}

func (c *Console) resourceID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// promote makes the commission standing: project-owned, no lease, no end
// with its task (the collector keeps it until released).
// It must fit the standing block of the project's file in force.
func (c *Console) promote(w http.ResponseWriter, r *http.Request) {
	id, ok := c.resourceID(w, r)
	if !ok {
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	c.resourceChange(w, r, "promote", func(ctx context.Context, id int64, by string) error {
		return c.cfg.Resources.Promote(ctx, id, by, func(repo string) policy.Standing { return v.EnvelopeOf(repo).Standing })
	}, id)
}

// release ends the commission's lease now; the gateway's collector deletes
// it on its next pass (every 2 minutes).
func (c *Console) release(w http.ResponseWriter, r *http.Request) {
	id, ok := c.resourceID(w, r)
	if !ok {
		return
	}
	c.resourceChange(w, r, "release", c.cfg.Resources.EndLease, id)
}

// resourceChange answers a button whose change (promote or release) ran on
// commission id, sharing resourceChangeDo's checks and refusals.
func (c *Console) resourceChange(w http.ResponseWriter, r *http.Request, what string, change func(ctx context.Context, id int64, by string) error, id int64) {
	if err := c.resourceChangeDo(r.Context(), button(r), what, id, change); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/resources", http.StatusSeeOther)
}
