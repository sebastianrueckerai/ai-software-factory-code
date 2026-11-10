package console

import (
	"errors"
	"net/http"
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
