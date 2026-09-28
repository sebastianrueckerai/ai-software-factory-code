package console

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type projectRow struct {
	factoryd.Project
	// Notify is who gets pinged, NotifyDefault when that is the policy's
	// approvers.
	NotifyList    []string
	NotifyDefault bool
	Envelope      cloudres.EnvelopeRecord
	Minted        bool
	GatewayYAML   string
}

// projects lists every project with its settings, its envelope's two
// blocks and the standing monthly line, the edit forms for approvers, and
// the registration form.
func (c *Console) projects(w http.ResponseWriter, r *http.Request) {
	ps, err := c.cfg.Projects.Projects(r.Context())
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	envs, err := c.cfg.Resources.Envelopes(r.Context())
	if err != nil {
		log.Printf("console: envelopes: %v", err)
		http.Error(w, "cannot read the envelopes", http.StatusInternalServerError)
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
	}
	var rows []projectRow
	for _, p := range ps {
		row := projectRow{Project: p, NotifyList: p.NotifyOrApprovers(v.Document.Approvers), NotifyDefault: len(p.Notify) == 0,
			Minted: p.Minted(), GatewayYAML: factoryd.ListenerYAML(p),
			Envelope: cloudres.EnvelopeRecord{Project: p.Repository, Envelope: v.Document.DefaultEnvelope()}}
		if i := slices.IndexFunc(envs, func(e cloudres.EnvelopeRecord) bool { return e.Project == p.Repository }); i >= 0 {
			row.Envelope = envs[i]
		}
		rows = append(rows, row)
	}
	s := current(r)
	c.render(w, "projects.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Projects": rows, "Approver": c.approver(r),
		"Default": v.Document.Project(), "Saved": r.URL.Query().Get("saved"), "Registered": r.URL.Query().Get("registered")})
}

// registerProject records a project from the form and reconciles it;
// its organisation, repository, CI user and tokens, which only the Forgejo
// admin can make, wait for factoryd register-project.
func (c *Console) registerProject(w http.ResponseWriter, r *http.Request) {
	v, _, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil {
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	def := v.Document.Project()
	envs, err := factoryd.ParseEnvironments(lines(r.PostFormValue("environments")), lines(r.PostFormValue("hostnames")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	p := factoryd.Project{Name: name, Repository: name + "/" + name,
		MergeRule: cmpOrForm(r.PostFormValue("merge_rule"), def.MergeRule), Namespaces: words(r.PostFormValue("namespaces")),
		Environments: envs, Quota: def.Quota, Secrets: words(r.PostFormValue("secrets")), Managed: true}
	if err := p.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	user := current(r).user
	if err := c.cfg.Projects.CreateProject(r.Context(), p); err != nil {
		log.Printf("console: %s registers %s: %v", user, p.Name, err)
		if errors.Is(err, factoryd.ErrProjectExists) {
			http.Error(w, "project "+p.Name+" or one of its namespaces is registered already", http.StatusConflict)
			return
		}
		http.Error(w, "cannot record the project", http.StatusInternalServerError)
		return
	}
	log.Printf("console: %s registered project %s (%s)", user, p.Name, p.Repository)
	c.reconcile(r.Context(), p)
	http.Redirect(w, r, "/projects?registered="+url.QueryEscape(p.Name), http.StatusSeeOther)
}

// projectSettings saves the merge rule, notify list and quota, then
// reconciles: main's protection follows the rule, the ResourceQuotas the
// quota, at once.
func (c *Console) projectSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok, err := c.cfg.Projects.Project(r.Context(), name)
	if err != nil {
		http.Error(w, "cannot read the project", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.MergeRule = r.PostFormValue("merge_rule")
	if !p.Managed && p.MergeRule == policy.MergeAuto {
		http.Error(w, "the factory's own repositories keep their protection in the infra repository: their merge rule stays human", http.StatusBadRequest)
		return
	}
	p.Notify = words(r.PostFormValue("notify"))
	p.Quota = policy.Quota{CPU: strings.TrimSpace(r.PostFormValue("quota_cpu")), Memory: strings.TrimSpace(r.PostFormValue("quota_memory")),
		Storage: strings.TrimSpace(r.PostFormValue("quota_storage"))}
	if err := settingsProblem(p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	user := current(r).user
	if err := c.cfg.Projects.SetProjectSettings(r.Context(), name, p.MergeRule, p.Notify, p.Quota); err != nil {
		log.Printf("console: %s sets project %s: %v", user, name, err)
		http.Error(w, "cannot save the settings", http.StatusInternalServerError)
		return
	}
	log.Printf("console: %s set project %s: merge rule %s, notify %v, quota %+v", user, name, p.MergeRule, p.Notify, p.Quota)
	c.reconcile(r.Context(), p)
	http.Redirect(w, r, "/projects?saved="+url.QueryEscape(name), http.StatusSeeOther)
}

// settingsProblem checks what the settings form changes, for any row, the
// factory's own included.
func settingsProblem(p factoryd.Project) error {
	check := factoryd.Project{Name: "x", Repository: "x/x", MergeRule: p.MergeRule, Notify: p.Notify, Namespaces: []string{"x"}, Quota: p.Quota}
	return check.Validate()
}

// reconcile runs the registrar on p within the request; each object's
// outcome goes to the log.
func (c *Console) reconcile(ctx context.Context, p factoryd.Project) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	if err := c.cfg.Reconcile(ctx, p); err != nil {
		log.Printf("console: reconcile project %s: %v", p.Name, err)
	}
}

func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func cmpOrForm(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}
