package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	// Promotable: not promoted, not released, lease still running.
	Promotable bool
}

type projectView struct {
	Project     string
	Envelope    cloudres.EnvelopeRecord
	HasEnvelope bool // false: the policy's envelope_default applies on the first request
	Rows        []resourceRow
}

// resources lists the agent project's commissions by project and task,
// with each project's envelope and its edit form.
func (c *Console) resources(w http.ResponseWriter, r *http.Request) {
	rows, err := c.cfg.Resources.Commissions(r.Context(), time.Now().Add(-closedSince))
	if err != nil {
		log.Printf("console: resources: %v", err)
		http.Error(w, "cannot read the resource registry", http.StatusInternalServerError)
		return
	}
	envs, err := c.cfg.Resources.Envelopes(r.Context())
	if err != nil {
		log.Printf("console: envelopes: %v", err)
		http.Error(w, "cannot read the envelopes", http.StatusInternalServerError)
		return
	}
	byProject := map[string]*projectView{}
	view := func(project string) *projectView {
		if v, ok := byProject[project]; ok {
			return v
		}
		v := &projectView{Project: project}
		byProject[project] = v
		return v
	}
	for _, e := range envs {
		v := view(e.Project)
		v.Envelope, v.HasEnvelope = e, true
	}
	if v := view(c.cfg.Repo.String()); !v.HasEnvelope {
		p, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
		if err != nil || !ok {
			log.Printf("console: current policy: loaded %v, %v", ok, err)
		}
		v.Envelope = cloudres.EnvelopeRecord{Project: v.Project, Envelope: p.Document.EnvelopeDefault}
	}
	actions := map[int64]int{} // commission (hold) -> index of its row with the buttons
	monthly := map[int64]float64{}
	for _, res := range rows {
		if slices.Contains(cloudres.OpenStates, res.State) {
			monthly[res.HoldID] += res.PricePerHourEUR * cloudres.HoursPerMonth
		}
		v := view(res.Repo)
		row := resourceRow{Resource: res, Open: slices.Contains(cloudres.OpenStates, res.State),
			Promotable: !res.Promoted && res.ReleasedBy == "" && time.Now().Before(res.LeaseExpires)}
		v.Rows = append(v.Rows, row)
		i := len(v.Rows) - 1
		if !row.Open || res.HoldID == 0 {
			continue
		}
		if j, ok := actions[res.HoldID]; !ok || (res.Kind == cloudres.KindServer && v.Rows[j].Kind != cloudres.KindServer) {
			actions[res.HoldID] = i
		}
	}
	var projects []*projectView
	for _, v := range byProject {
		for i := range v.Rows {
			if j, ok := actions[v.Rows[i].HoldID]; ok && j == i && v.Rows[i].Open {
				v.Rows[i].Actions = true
				v.Rows[i].PromoteNote = fmt.Sprintf("promoted, about %.2f EUR a month against the envelope's %.2f EUR (%.4f used or reserved this month)",
					monthly[v.Rows[i].HoldID], v.Envelope.MonthlyEUR, v.Envelope.Use.MonthEUR)
			}
		}
		projects = append(projects, v)
	}
	slices.SortFunc(projects, func(a, b *projectView) int { return cmp.Compare(a.Project, b.Project) })
	s := current(r)
	c.render(w, "resources.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Projects": projects, "Approver": c.approver(r),
		"Saved": r.URL.Query().Get("saved")})
}

func (c *Console) resourceID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// promote makes the commission project-owned: no lease, no end with its
// task (the collector keeps it).
func (c *Console) promote(w http.ResponseWriter, r *http.Request) {
	c.resourceChange(w, r, "promote", c.cfg.Resources.Promote)
}

// release ends the commission's lease now; the gateway's collector deletes
// it on its next pass (every 2 minutes).
func (c *Console) release(w http.ResponseWriter, r *http.Request) {
	c.resourceChange(w, r, "release", c.cfg.Resources.EndLease)
}

func (c *Console) resourceChange(w http.ResponseWriter, r *http.Request, what string, change func(ctx context.Context, id int64, by string) error) {
	id, ok := c.resourceID(w, r)
	if !ok {
		return
	}
	user := current(r).user
	if err := change(r.Context(), id, user); err != nil {
		log.Printf("console: %s %s resource %d: %v", user, what, id, err)
		switch {
		case errors.Is(err, cloudres.ErrLeaseEnded):
			http.Error(w, "cannot "+what+": its lease has ended or it was released; the collector deletes it", http.StatusConflict)
		case errors.Is(err, cloudres.ErrNotOpen):
			http.Error(w, "cannot "+what+": the resource is no longer open", http.StatusConflict)
		default:
			http.Error(w, "cannot "+what+"; see the factoryd log", http.StatusInternalServerError)
		}
		return
	}
	log.Printf("console: %s %sd the commission of resource %d", user, what, id)
	http.Redirect(w, r, "/resources", http.StatusSeeOther)
}

var serverType = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// setEnvelope saves a project's envelope as the human. Only projects the
// console shows (those with an envelope, and the code repository) are
// editable.
func (c *Console) setEnvelope(w http.ResponseWriter, r *http.Request) {
	project := r.PostFormValue("project")
	known := project == c.cfg.Repo.String()
	if !known {
		envs, err := c.cfg.Resources.Envelopes(r.Context())
		if err != nil {
			http.Error(w, "cannot read the envelopes", http.StatusInternalServerError)
			return
		}
		known = slices.ContainsFunc(envs, func(e cloudres.EnvelopeRecord) bool { return e.Project == project })
	}
	if !known {
		http.Error(w, "unknown project "+project, http.StatusBadRequest)
		return
	}
	env, problem := parseEnvelope(r)
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	user := current(r).user
	if err := c.cfg.Resources.SetEnvelope(r.Context(), project, env, user); err != nil {
		log.Printf("console: %s set the envelope of %s: %v", user, project, err)
		http.Error(w, "cannot save the envelope", http.StatusInternalServerError)
		return
	}
	log.Printf("console: %s set the envelope of %s to %+v", user, project, env)
	http.Redirect(w, r, "/resources?saved="+url.QueryEscape(project), http.StatusSeeOther)
}

// parseEnvelope reads the edit form; a non-empty problem says what is
// wrong. Zero values are allowed: they close the project to commissions
// without a human.
func parseEnvelope(r *http.Request) (policy.Envelope, string) {
	var env policy.Envelope
	monthly, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("monthly_eur")), 64)
	if err != nil || math.IsNaN(monthly) || monthly < 0 || monthly > 100000 {
		return env, "monthly EUR must be a number from 0 to 100000"
	}
	env.MonthlyEUR = math.Round(monthly*100) / 100
	for _, t := range strings.FieldsFunc(r.PostFormValue("server_types"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if !serverType.MatchString(t) {
			return env, "server type " + strconv.Quote(t) + " is not a type name like cx23"
		}
		if !slices.Contains(env.ServerTypes, t) {
			env.ServerTypes = append(env.ServerTypes, t)
		}
	}
	if len(env.ServerTypes) > 20 {
		return env, "at most 20 server types"
	}
	if env.ServerTypes == nil {
		env.ServerTypes = []string{}
	}
	for _, f := range []struct {
		name, label string
		max         int
		into        *int
	}{
		{"max_count", "max servers", 1000, &env.MaxCount},
		{"max_lease_hours", "max lease hours", 8760, &env.MaxLeaseHours},
		{"volume_max_gb", "max volume GB", 10240, &env.VolumeMaxGB},
	} {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(f.name)))
		if err != nil || n < 0 || n > f.max {
			return env, f.label + " must be a whole number from 0 to " + strconv.Itoa(f.max)
		}
		*f.into = n
	}
	if env.VolumeMaxGB != 0 && env.VolumeMaxGB < policy.MinVolumeGB {
		return env, "max volume GB must be 0 (no volumes) or at least " + strconv.Itoa(policy.MinVolumeGB)
	}
	return env, ""
}
