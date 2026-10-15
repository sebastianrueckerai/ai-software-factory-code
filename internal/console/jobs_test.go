package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakeJobs answers the cluster's CronJobs, or err.
type fakeJobs struct {
	rows []factoryd.CronJobState
	err  error
}

func (f fakeJobs) CronJobs(context.Context) ([]factoryd.CronJobState, error) { return f.rows, f.err }

// jobsSection is the page's section of the cluster's jobs, "" without one.
func jobsSection(page string) string {
	i := strings.Index(page, `<section aria-labelledby="jobs">`)
	if i < 0 {
		return ""
	}
	return page[i : i+strings.Index(page[i:], "</section>")]
}

// The Resources page lists the cluster's jobs after the projects,
// read-only: the schedule in words, each last run as a coarse age with
// its full time in the title, never for none. Without the port it has
// no such section.
func TestResourcesPageListsTheClustersJobs(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	if page := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck).Body.String(); strings.Contains(page, "Jobs of the cluster") {
		t.Fatal("the page lists jobs without the port")
	}
	now := time.Now()
	ran, dumped := now.Add(-3*time.Hour-10*time.Minute), now.Add(-50*time.Hour)
	r.c.cfg.Jobs = fakeJobs{rows: []factoryd.CronJobState{
		{Namespace: "platform", Name: "cnpg-backup", Schedule: "0 2 * * *", LastScheduled: ran, LastSuccessful: ran},
		{Namespace: "factory", Name: "forgejo-dump", Schedule: "30 2 * * *", Suspended: true, LastScheduled: dumped},
		{Namespace: "platform", Name: "openbao-config", Schedule: "*/10 * * * *"},
	}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /resources = %d: %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	jobs := jobsSection(page)
	if jobs == "" || strings.Index(page, "Jobs of the cluster") < strings.LastIndex(page, `<section class="project"`) {
		t.Fatalf("no jobs section after the projects:\n%s", page)
	}
	utc := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
	for _, want := range []string{
		`<h3 id="jobs">Jobs of the cluster</h3>`,
		`<th scope="col">Name</th><th scope="col">Namespace</th><th scope="col">Schedule</th><th scope="col">Last scheduled</th><th scope="col">Last successful</th><th scope="col">Suspended</th>`,
		`<tr><td>cnpg-backup</td><td>platform</td><td>daily 02:00</td><td title="` + utc(ran) + `">3 h ago</td><td title="` + utc(ran) + `">3 h ago</td><td>no</td></tr>`,
		`<tr><td>forgejo-dump</td><td>factory</td><td>daily 02:30</td><td title="` + utc(dumped) + `">2 d ago</td><td>never</td><td>yes</td></tr>`,
		// a cron the factory cannot word shows as the expression
		`<tr><td>openbao-config</td><td>platform</td><td>*/10 * * * *</td><td>never</td><td>never</td><td>no</td></tr>`,
	} {
		if !strings.Contains(jobs, want) {
			t.Errorf("the jobs section lacks %q:\n%s", want, jobs)
		}
	}
	if strings.Contains(jobs, "<form") || strings.Contains(jobs, "<button") {
		t.Errorf("the jobs section acts:\n%s", jobs)
	}
}

// A cluster that does not answer leaves the section one line, and the
// rest of the page as it was.
func TestResourcesPageSaysTheClusterDidNotAnswer(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	without := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck).Body.String()
	r.c.cfg.Jobs = fakeJobs{err: errors.New("kubernetes: forbidden")}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /resources = %d: %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	jobs := jobsSection(page)
	if jobs != `<section aria-labelledby="jobs">`+"\n"+`<h3 id="jobs">Jobs of the cluster</h3>`+"\n"+`<p>The cluster did not answer.</p>`+"\n" {
		t.Errorf("the jobs section = %q", jobs)
	}
	if rest := strings.Replace(page, jobs+"</section>\n", "", 1); rest != without || !strings.Contains(rest, `<h3 id="project-demo-app/demo-app">`) {
		t.Errorf("the rest of the page differs from the page without jobs:\n%s", rest)
	}
}
