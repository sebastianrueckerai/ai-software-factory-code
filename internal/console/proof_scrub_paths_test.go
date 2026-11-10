package console

import (
	"context"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

func TestErrTextScrubsUserinfoAndBearer(t *testing.T) {
	maskedLeak(t, "errText", errText(leakyError()))
}

func leakyAPIError() *apiError {
	return &apiError{method: "POST", path: "/api/v1/x", status: 409,
		body: []byte(`{"message":"push to ` + leakUser + ` refused; ` + leakBearer + `"}`)}
}

func TestForgejoCauseScrubsTheAPIReason(t *testing.T) {
	maskedLeak(t, "forgejoCause of an apiError", forgejoCause(leakyAPIError()))
	maskedLeak(t, "forgejoCause of a refusal", forgejoCause(&forgejoRefusal{what: "merge", err: leakyAPIError()}))
	maskedLeak(t, "forgejoCause of a plain error", forgejoCause(leakyError()))
}

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
