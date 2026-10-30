package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

const holdProject = "ai-software-factory-code"

type deployCall struct{ repository, by, via string }

// holdRig is a rig whose project sf/code is held 7 h 30 min by plan #12
// (3 of 5 merged); the returned slice records each Deploy now.
func holdRig(t *testing.T) (*rig, *[]deployCall) {
	t.Helper()
	r := newRig(t)
	r.repoWriters["sf/code"] = []string{"sebastian", "morgan"}
	r.forgetWriters()
	r.c.cfg.Hold = func(_ context.Context, repository string) (factoryd.Hold, error) {
		if repository != "sf/code" {
			return factoryd.Hold{}, nil
		}
		return factoryd.Hold{Since: time.Now().Add(-7*time.Hour - 30*time.Minute), Groups: []factoryd.Group{
			{Plan: "t-pl", Issue: 12, Children: map[string]int{"merged": 2, "done": 1, "queued": 2}}}}, nil
	}
	calls := &[]deployCall{}
	r.c.cfg.DeployNow = func(_ context.Context, repository, by, via string) (string, error) {
		*calls = append(*calls, deployCall{repository, by, via})
		return "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d", nil
	}
	return r, calls
}

func TestBoardShowsTheProductionHold(t *testing.T) {
	r, _ := holdRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`production held · 7 h</span>`, `plan #12 · 3 of 5 merged`, `action="/projects/` + holdProject + `/deploy-now"`,
		`<p class="sp-note warn">Held for 7 h: plan #12 is open on main.`, "Deploy now</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the approver's board lacks %q", want)
		}
	}

	wck, _ := memberLogin(t, r, "morgan")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), wck).Body.String()
	for _, want := range []string{`production held · 7 h</span>`, `plan #12 · 3 of 5 merged`} {
		if !strings.Contains(body, want) {
			t.Errorf("the writer's board lacks %q", want)
		}
	}
	if strings.Contains(body, "deploy-now") {
		t.Error("the writer's board carries the Deploy now form")
	}

	r.c.cfg.Hold = func(context.Context, string) (factoryd.Hold, error) { return factoryd.Hold{}, nil }
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "production held") {
		t.Error("a free production shows the hold")
	}
}

func TestBoardShowsTheHoldWithoutMirrors(t *testing.T) {
	r, _ := holdRig(t)
	r.c.cfg.Mirrors = nil
	ck, _ := r.loggedIn(t)
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, "production held · 7 h") {
		t.Error("the board without mirrors lacks the hold cell")
	}
}

func TestBoardNamesAHoldItCannotRead(t *testing.T) {
	r, _ := holdRig(t)
	r.c.cfg.Hold = func(context.Context, string) (factoryd.Hold, error) { return factoryd.Hold{}, context.DeadlineExceeded }
	ck, _ := r.loggedIn(t)
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, "the production hold could not be read") {
		t.Error("an unreadable hold goes unnamed")
	}
}

func TestBoardTextNamesTheHold(t *testing.T) {
	d := boardData{}
	d.Status.Hold = &holdLine{For: "7 h", Reason: "plan #12 is open on main", Groups: []string{"plan #12 · 3 of 5 merged"}}
	if got := boardWords(d); !strings.HasPrefix(got, "Production held for 7 h: plan #12 is open on main.\n") {
		t.Errorf("boardText = %q, want it to start with the hold", got)
	}
	if got := boardWords(boardData{}); strings.Contains(got, "Production held") {
		t.Errorf("a free production reads %q", got)
	}
}

func TestDeployNowButton(t *testing.T) {
	r, calls := holdRig(t)
	ck, s := r.loggedIn(t)
	path := "/projects/" + holdProject + "/deploy-now"

	req := post(path, url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	rec := r.do(req, ck)
	want := "Deploy now: main of " + holdProject + " at 9d9d9d9 goes to production. A merge after it waits for the open plans again."
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("Deploy now = %d, want 200 with %q: %s", rec.Code, want, rec.Body)
	}
	if len(*calls) != 1 || (*calls)[0] != (deployCall{"sf/code", "sebastian", factoryd.ViaButton}) {
		t.Fatalf("DeployNow calls = %+v, want one for sf/code by sebastian via the button", *calls)
	}

	if rec := r.do(post(path, url.Values{}), ck); rec.Code != http.StatusForbidden || len(*calls) != 1 {
		t.Errorf("without csrf = %d with %d calls, want 403 and still 1", rec.Code, len(*calls))
	}

	wck, ws := memberLogin(t, r, "morgan")
	if rec := r.do(post(path, url.Values{"csrf": {ws.csrf}}), wck); rec.Code != http.StatusForbidden || len(*calls) != 1 {
		t.Errorf("writer = %d with %d calls, want 403 and still 1", rec.Code, len(*calls))
	}
}

func TestDeployNowRefusesAFreeProduction(t *testing.T) {
	r, _ := holdRig(t)
	r.c.cfg.DeployNow = func(context.Context, string, string, string) (string, error) { return "", factoryd.ErrNotHeld }
	ck, s := r.loggedIn(t)
	rec := r.do(post("/projects/"+holdProject+"/deploy-now", url.Values{"csrf": {s.csrf}}), ck)
	want := "Production of " + holdProject + " is not held: every merge goes to production on its own."
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("Deploy now on a free production = %d %q, want 409 %q", rec.Code, rec.Body, want)
	}
}
