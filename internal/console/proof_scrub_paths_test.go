package console

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const (
	leakUser   = "https://bob:hunter2@git.example/x"
	leakBearer = "Authorization: Bearer abcdef0123456789"
)

func leakyError() error {
	return errors.New("clone " + leakUser + " refused; " + leakBearer)
}

// maskedLeak fails unless text holds neither secret and holds the masked
// form redact.Scrubber produces.
func maskedLeak(t *testing.T, path, text string) {
	t.Helper()
	for _, secret := range []string{"hunter2", "bob:", "abcdef0123456789"} {
		if strings.Contains(text, secret) {
			t.Errorf("%s leaks %q: %q", path, secret, text)
		}
	}
	for _, mask := range []string{"https://[redacted]@git.example/x", "Bearer [redacted]"} {
		if !strings.Contains(text, mask) {
			t.Errorf("%s lacks the mask %q: %q", path, mask, text)
		}
	}
}

// errText masks URL userinfo and a bearer value.
func TestErrTextScrubsUserinfoAndBearer(t *testing.T) {
	maskedLeak(t, "errText", errText(leakyError()))
}

const leakMessage = "push to " + leakUser + " refused; " + leakBearer

func leakyAPIError() *apiError {
	return &apiError{method: "POST", path: "/api/v1/x", status: 409, body: []byte(`{"message":"` + leakMessage + `"}`)}
}

// forgejoCause masks the reason of an apiError, a refusal and a plain error.
func TestForgejoCauseScrubsTheAPIReason(t *testing.T) {
	maskedLeak(t, "forgejoCause of an apiError", forgejoCause(leakyAPIError()))
	maskedLeak(t, "forgejoCause of a refusal", forgejoCause(&forgejoRefusal{what: "merge", err: leakyAPIError()}))
	maskedLeak(t, "forgejoCause of a plain error", forgejoCause(leakyError()))
}

// errSentence masks the error on each of its arms.
func TestErrSentenceScrubsEveryArm(t *testing.T) {
	a := actor{s: &session{user: "sebastian"}, via: "chat"}
	for name, err := range map[string]error{
		"api refusal":   &forgejoRefusal{what: "merge", err: leakyAPIError()},
		"plain refusal": &forgejoRefusal{what: "merge", err: leakyError()},
		"say refusal":   &forgejoRefusal{what: "merge", err: leakyAPIError(), say: "The merge failed: %s. Try again."},
		"catch-all":     leakyError(),
	} {
		maskedLeak(t, "errSentence "+name, errSentence(a, err))
	}
}

// The merge refusal and the panel's kept refusal carry Forgejo's reason masked.
func TestMergeRefusalScrubsForgejoReason(t *testing.T) {
	s := newStatusRig(t)
	s.infra.set(openPull)
	s.infra.mergeStatus = http.StatusConflict
	s.infra.mergeBody = string(leakyAPIError().body)
	rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck)
	maskedLeak(t, "the merge refusal", rec.Body.String())
	maskedLeak(t, "the panel's kept refusal", s.c.runs["linkshort"].merge)
}

func chatActorOf(t *testing.T, r *rig, s *session) actor {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/chat", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	p, ok, err := r.c.projectOf(req.Context(), "ai-software-factory-code")
	if err != nil || !ok {
		t.Fatalf("the chat's project: %v, %v", ok, err)
	}
	return actor{s: s, repository: p.Repository, via: factoryd.ViaChat}
}

// The answer, answers and hold-out refusals embed errText.
func TestRawErrorRefusalsAreScrubbed(t *testing.T) {
	r, _ := batchAnswerRig(t)
	_, s := r.loggedIn(t)
	a := chatActorOf(t, r, s)
	ctx := context.Background()
	r.c.cfg.AnswerPlan = func(context.Context, string, int, int, string, string, string) error { return leakyError() }
	_, err := r.c.doAnswerPlanQuestion(ctx, a, "t-p", 1, 1, "", "60 seconds")
	maskedLeak(t, "the answer refusal", err.Error())
	r.c.cfg.AnswerPlanQuestions = func(context.Context, string, int, []factoryd.PlanAnswer, string) error { return leakyError() }
	_, err = r.c.doAnswerPlanQuestions(ctx, a, "t-p", 1, []PlanAnswerInput{{Question: 1, Text: "60 seconds"}})
	maskedLeak(t, "the answers refusal", err.Error())
	r.c.cfg.DecideHoldout = func(context.Context, string, int, int, string, string) error { return leakyError() }
	err = r.c.doDecideHoldout(ctx, a, "t-p", 1, 1, "okay")
	maskedLeak(t, "the hold-out refusal", err.Error())
}

// The analysis refusal embeds errText.
func TestAnalysisRefusalIsScrubbed(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	r.c.cfg.AttachAnalysis = func(context.Context, string, string, string, factoryd.Event) error { return leakyError() }
	tool, _ := r.c.chatTool("attach_analysis")
	_, _, err := tool.run(trn, toolInput{TaskID: "t-e", Found: "found"})
	maskedLeak(t, "the analysis refusal", err.Error())
}

// The grant refusal embeds errText.
func TestGrantRefusalIsScrubbed(t *testing.T) {
	r, _, g := newChatRig(t)
	verdictRig(r)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	r.c.cfg.GrantCriteria = func(context.Context, string, string, int64) (int, error) { return 0, leakyError() }
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "answer_criteria", `{"task_id":"t-plan","action":"grant"}`)}, {say("No.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"grant the plan one more bounded card"}})
	maskedLeak(t, "the grant refusal", strings.Join(events, "\n"))
}

// forgejoFailed shows Forgejo's reason, masked, without method, path, status or body.
func TestForgejoFailedKeepsTheRawBodyOut(t *testing.T) {
	r, _, _ := newChatRig(t)
	ck, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodPost, "/board", nil)
	req.AddCookie(ck)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	rec := httptest.NewRecorder()
	r.c.forgejoFailed(rec, req, "read the labels", leakyAPIError())
	body := rec.Body.String()
	maskedLeak(t, "forgejoFailed", body)
	for _, raw := range []string{"POST", "/api/v1/x", "status 409", `{"message"`} {
		if strings.Contains(body, raw) {
			t.Errorf("forgejoFailed holds the raw apiError text %q: %q", raw, body)
		}
	}
}

// The status panel masks the error of the main read.
func TestStatusPanelScrubsTheMainError(t *testing.T) {
	s := newStatusRig(t)
	s.force.load("linkshort")
	s.row(factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort", Objects: "factory", Agents: "coder", File: true, GitToken: "g"})
	s.mainErr = leakyError()
	maskedLeak(t, "the status panel", html.UnescapeString(s.status(t)))
}

// A Forgejo token is 40 hex characters, which the generic rules keep as a
// commit id: only the boot secrets the Config carries mask it.
func TestErrTextMasksTheConfiguredSecret(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef01234567"
	r := newRig(t)
	cfg := r.c.cfg
	if got := errText(errors.New("forgejo said " + secret)); !strings.Contains(got, secret) {
		t.Fatalf("the default scrubber already masks the commit-id-shaped %q: %q", secret, got)
	}
	cfg.Scrub = redact.New(secret)
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scrubber.Store(redact.New()) })
	got := errText(errors.New("forgejo said " + secret))
	if strings.Contains(got, secret) || got != "forgejo said [redacted]" {
		t.Errorf("errText = %q, want the configured secret masked", got)
	}
}

// reason is Forgejo's JSON message when there is one, else the status
// text, and never the raw body.
func TestAPIErrorReasonNeverShowsTheRawBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a JSON message", 409, `{"message":"the branch is protected"}`, "the branch is protected"},
		{"an empty body", 502, ``, "502 Bad Gateway"},
		{"an HTML page", 502, `<html><body>upstream sent https://bob:hunter2@git.example/x</body></html>`, "502 Bad Gateway"},
		{"JSON without a message", 500, `{"errors":["x"]}`, "500 Internal Server Error"},
		{"a status with no text", 599, `nope`, "599"},
	} {
		got := (&apiError{method: "GET", path: "/x", status: tc.status, body: []byte(tc.body)}).reason()
		if got != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A document save Forgejo refuses answers Forgejo's message only, masked.
func TestDocsSaveRefusalIsScrubbed(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	withDocs(t, r, map[string]string{"docs/plans/cache.md": docsPlan})
	ck, s := r.loggedIn(t)
	r.c.cfg.HTTP = &http.Client{Transport: failStatus{http.DefaultTransport,
		"repos/sf/docs/contents", http.StatusBadGateway, leakUser + " " + leakBearer}}
	body := `{"text":"# The cache plan\n\nThe store: Postgres.\n","base":"` + factoryd.ContentHash(docsPlan) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/docs/docs/plans/cache.md", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	rec := r.do(req, ck)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the failed save = %d: %s", rec.Code, rec.Body)
	}
	maskedLeak(t, "the editor's refusal", rec.Body.String())
	for _, raw := range []string{"forgejo GET", "status 502", "/api/v1/"} {
		if strings.Contains(rec.Body.String(), raw) {
			t.Errorf("the editor's refusal holds the raw apiError text %q: %s", raw, rec.Body)
		}
	}
}

// A filing Forgejo refuses answers Forgejo's message only, masked, and the
// cause comes before what the filing leaves behind.
func TestFailedFilingRefusalIsScrubbed(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	infra.mu.Lock()
	infra.branches, infra.pullsStatus = map[string]string{}, http.StatusUnprocessableEntity
	infra.pullsBody = string(leakyAPIError().body)
	infra.mu.Unlock()
	rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
	maskedLeak(t, "the filing's refusal", rec.Body.String())
	if strings.Contains(rec.Body.String(), "status 422") || strings.Contains(rec.Body.String(), "/api/v1/") {
		t.Errorf("the filing's refusal holds the raw apiError text: %s", rec.Body)
	}
}

// halfReadPolicy answers a version with ok false: a read that failed after
// it filled the value.
type halfReadPolicy struct{ Policies }

func (h halfReadPolicy) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{Tree: policy.Tree{Document: policy.Document{TimeZone: "Europe/Berlin"}}}, false, errors.New("the policy table did not answer")
}

// A failed policy read leaves the routine drawer an empty version, so its
// schedule names UTC, not a zone the failed read half filled.
func TestRoutineDrawerDropsAHalfReadPolicy(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	ck, _ := r.loggedIn(t)
	r.c.cfg.Policies = halfReadPolicy{r.c.cfg.Policies}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/routines/ai-software-factory-code/weekly-bump/drawer", nil), ck)
	body := rec.Body.String()
	if !strings.Contains(body, "The configuration in force could not be read: the policy table did not answer. Reload the board.") {
		t.Errorf("the drawer lacks its problem sentence:\n%s", body)
	}
	if strings.Contains(body, "Europe/Berlin") || !strings.Contains(body, "UTC") {
		t.Errorf("the drawer's schedule zone is not UTC:\n%s", body)
	}
}

// A plan card whose revision is not recorded says so in words.
func TestApprovalPlanCardNamesAnUnrecordedRevision(t *testing.T) {
	r, _ := batchAnswerRig(t)
	ck, _ := r.loggedIn(t)
	r.c.cfg.Tasks = failingPlans{r.c.cfg.Tasks, nil}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := "The plan could not be read: revision 1 of t-p is not recorded. Open the plan&#39;s approval again, or read the plan on its issue."
	if !strings.Contains(body, want) {
		t.Errorf("the approval card lacks %q:\n%s", want, body)
	}
}

// The GitHub sync's failure reaches its event masked.
func TestGithubSyncEventIsScrubbed(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.mirrors.rows["demo-app export"] = factoryd.Mirror{Project: "demo-app", Direction: factoryd.MirrorExport, Remote: "acme/app",
		Username: "u", Token: ghToken, State: "created", Mode: factoryd.ModePullRequests}
	r.syncErr = leakyError()
	before := r.events.count()
	if rec := r.do(post("/projects/demo-app/export/sync", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("Sync latest = %d %q", rec.Code, rec.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); r.events.count() == before && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	rows := r.events.all()[before:]
	if len(rows) != 1 {
		t.Fatalf("%d events, want one", len(rows))
	}
	result, _ := rows[0].Detail["result"].(string)
	maskedLeak(t, "the sync event", result)
}

// The status panel's reconcile failure is masked.
func TestStatusPanelScrubsTheReconcileError(t *testing.T) {
	s := newStatusRig(t)
	s.force.load("linkshort")
	s.reconcileErr = leakyError().Error()
	maskedLeak(t, "the status panel", html.UnescapeString(s.status(t)))
}

// The status panel's pull-request read names the whole sentence with
// Forgejo's message, masked.
func TestStatusPanelPullReadNamesForgejosMessage(t *testing.T) {
	s := newStatusRig(t)
	s.c.cfg.HTTP = &http.Client{Transport: failStatus{http.DefaultTransport, "/pulls/12", http.StatusBadGateway,
		leakMessage}}
	got := html.UnescapeString(s.status(t))
	maskedLeak(t, "the status panel", got)
	want := "The pull request could not be read: push to https://[redacted]@git.example/x refused; Authorization: Bearer [redacted]. " +
		"Load the page again, or read the pull request on Forgejo."
	if !strings.Contains(got, want) {
		t.Errorf("the panel lacks %q:\n%s", want, got)
	}
}

// The merge handler's pull-request read answers Forgejo's message, masked.
func TestMergePullReadRefusalIsScrubbed(t *testing.T) {
	s := newStatusRig(t)
	s.infra.set(openPull)
	s.c.cfg.HTTP = &http.Client{Transport: failStatus{http.DefaultTransport, "/pulls/12", http.StatusBadGateway,
		leakMessage}}
	rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the merge = %d: %s", rec.Code, rec.Body)
	}
	maskedLeak(t, "the merge's pull-request read", rec.Body.String())
}

// The board's head-checks problem is Forgejo's message, masked.
func TestBoardHeadUnreadWhyIsScrubbed(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.rows[1].Spec = specRedChecks()
	r.c.cfg.HTTP = &http.Client{Transport: failStatus{http.DefaultTransport, "commits/abc123/status", http.StatusBadGateway,
		leakMessage}}
	body := html.UnescapeString(r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String())
	maskedLeak(t, "the board's head-checks problem", body)
}

// A branch read Forgejo refuses answers Forgejo's message only, masked.
func TestBranchReadRefusalIsScrubbed(t *testing.T) {
	r := newRig(t)
	withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	r.c.cfg.HTTP = &http.Client{Transport: failStatus{http.DefaultTransport, "/branches/", http.StatusInternalServerError, leakMessage}}
	rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the filing = %d: %s", rec.Code, rec.Body)
	}
	maskedLeak(t, "the branch read refusal", rec.Body.String())
	for _, raw := range []string{"forgejo GET", "status 500", "/api/v1/"} {
		if strings.Contains(rec.Body.String(), raw) {
			t.Errorf("the branch read refusal holds the raw apiError text %q: %s", raw, rec.Body)
		}
	}
}

// failAfter is an http.RoundTripper that answers the nth and later branch
// reads with 502; every other request goes to base.
type failAfter struct {
	base http.RoundTripper
	n    *int
}

func (f failAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/branches/") {
		*f.n++
		if *f.n > 1 {
			return failAnswer(http.StatusBadGateway, "the branch read did not answer"), nil
		}
	}
	return f.base.RoundTrip(req)
}

// A refused commit whose undo cannot read the branch names no deletion
// after a 4xx and only a conditional one after a 5xx.
func TestFailedCommitWithAnUnreadableBranch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"a refusal", http.StatusUnprocessableEntity, "Forgejo refused to commit the project's file: 422 Unprocessable Entity; " +
			"another filing of linkshort holds the name. Choose another name, or open that filing's project when it lands."},
		{"a server error", http.StatusInternalServerError, "Forgejo refused to commit the project's file: 500 Internal Server Error. " +
			"Branch project-linkshort may be left. If no other filing of linkshort is in progress, delete it on Forgejo, then file the project again."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			infra := withInfra(t, r)
			r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
			ck, s := r.loggedIn(t)
			infra.mu.Lock()
			infra.branches, infra.contentsStatus = map[string]string{}, tc.status
			infra.mu.Unlock()
			reads := 0
			r.c.cfg.HTTP = &http.Client{Transport: failAfter{http.DefaultTransport, &reads}}
			rec := r.do(post("/projects", linkshortForm(s.csrf)), ck)
			if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusBadGateway || !strings.HasSuffix(got, tc.want) {
				t.Errorf("the filing = %d %q, want 502 ending %q", rec.Code, got, tc.want)
			}
		})
	}
}
