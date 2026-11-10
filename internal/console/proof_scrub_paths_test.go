package console

import (
	"errors"
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
