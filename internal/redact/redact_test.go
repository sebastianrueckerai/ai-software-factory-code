package redact

import "testing"

func TestStringKeepsCommitsDigestsAndPaths(t *testing.T) {
	s := New()
	for in, want := range map[string]string{
		`head 3f786850e387550fdab836ed7e6dc881de23001b pushed`:                                                              `head 3f786850e387550fdab836ed7e6dc881de23001b pushed`,
		`{"head":"3f786850e387550fdab836ed7e6dc881de23001b"}`:                                                               `{"head":"3f786850e387550fdab836ed7e6dc881de23001b"}`,
		`image forgejo.example/sf/ci-runner@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08 pulled`: `image forgejo.example/sf/ci-runner@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08 pulled`,
		`sf/demo-app-with-a-long-name/services/payments-api`:                                                                `sf/demo-app-with-a-long-name/services/payments-api`,
		`internal/factoryd/some_really_long_file_name_here_test.go`:                                                         `internal/factoryd/some_really_long_file_name_here_test.go`,
		`GET /api/v1/repos/sf/demo-app-with-a-long-name/pulls/9/files 404`:                                                  `GET /api/v1/repos/sf/demo-app-with-a-long-name/pulls/9/files 404`,
		`key hash 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08 blocked`:                                 `key hash [redacted] blocked`,
		`head 3F786850E387550FDAB836ED7E6DC881DE23001B pushed`:                                                              `head [redacted] pushed`,
		`id 3f786850e387550fdab836ed7e6dc881de23001b0 seen`:                                                                 `id [redacted] seen`,
		`digest sha256:9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08 bad`:                                `digest [redacted] bad`,
		`callback /hooks/0123456789abcdef0123456789abcdef01234567 failed`:                                                   `callback [redacted] failed`,
		`secret wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY rejected`:                                                          `secret [redacted] rejected`,
		`token sf/aB3dE5gH7jK9mN1pQ3sT5vW7yZ9bC1dE3f rejected`:                                                              `token [redacted] rejected`,
	} {
		if got := s.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

// A run of 32 or more key characters is a key only when it holds a digit,
// a + or an =: a long identifier and a path in any case are text.
func TestStringKeysHoldADigitAPlusOrAnEquals(t *testing.T) {
	s := New()
	for in, want := range map[string]string{
		`go test ./internal/factoryd/ -run TestWorkingStateCutsOnRuneBoundaries -count=1`: `go test ./internal/factoryd/ -run TestWorkingStateCutsOnRuneBoundaries -count=1`,
		`wrote /workspace/repo/web/src/components/ProjectBoardDrawer.tsx`:                 `wrote /workspace/repo/web/src/components/ProjectBoardDrawer.tsx`,
		`wrote src/main/java/com/example/service/OrderService.java`:                       `wrote src/main/java/com/example/service/OrderService.java`,
		`- [pending] name it TestSizeTripSeesACheckpointQueuedBehindAnAnswer`:             `- [pending] name it TestSizeTripSeesACheckpointQueuedBehindAnAnswer`,
		`key Xk9vB2mQ7pL4sT8wZ1cF6hJ3nR5yU0eAgDiK2oPq set`:                                `key [redacted] set`,
		`key Xk9vB2mQ7pL4sT8wZ1cF6hJ3nR5yU0eA/gD+iK2o set`:                                `key [redacted] set`,
		`key abcdefghijklmnop+qrstuvwxyzABCDEFGH set`:                                     `key [redacted] set`,
		`key abcdefghijklmnopqrstuvwxyzABCDEF== set`:                                      `key [redacted] set`,
		`secret wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY rejected`:                        `secret [redacted] rejected`,
		`head 3f786850e387550fdab836ed7e6dc881de23001b pushed`:                            `head 3f786850e387550fdab836ed7e6dc881de23001b pushed`,
	} {
		if got := s.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStringRemovesKeys(t *testing.T) {
	s := New("hunter2-master", "short")
	for in, want := range map[string]string{
		"Authorization: Bearer abc.def": "Authorization: Bearer [redacted]",
		// the gateway's refused_auth reason (auth.go), redacted live as
		// "missing bearer [redacted]"
		"missing bearer token":                      "missing bearer token",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==": "Authorization: Basic [redacted]",
		"clone https://x-access-token:ghp_abc123shortpass@git.example/sf/repo.git failed":        "clone https://[redacted]@git.example/sf/repo.git failed",
		"litellm refused key sk-task-key-1: blocked":                                             "litellm refused key [redacted]: blocked",
		"token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln refused":                                "token [redacted] refused",
		"hetzner token 0123456789abcdefghijABCDEFGHIJ0123456789 refused":                         "hetzner token [redacted] refused",
		"hetzner token AAAAAAAAAAAAAAAAAAAAbbbbbbbbbbbbbbbbbbbb01234567890123456789wxyz refused": "hetzner token [redacted] refused",
		"fal key 0b7e3f1a-2c4d-4e5f-8a9b-1c2d3e4f5a6b:0123456789abcdef0123456789abcdef refused":  "fal key [redacted]:[redacted] refused",
		"replicate token r8_0123456789abcdefghijABCDEFGHIJ01234 refused":                         "replicate token [redacted] refused",
		"firecrawl key fc-0123456789abcdef0123456789abcdef refused":                              "firecrawl key [redacted] refused",
		"openbao token hvs.CAESIJ0123456789abcdefghijklmnopqrstuvwxyz expired":                   "openbao token hvs.[redacted] expired",
		"key -----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\nQyNTUxOQAAACD=\n-----END OPENSSH PRIVATE KEY----- rejected": "key [pem redacted] rejected",
		"the master key hunter2-master is in use": "the master key [redacted] is in use",
		"a short secret stays short":              "a short secret stays short",
		`model "small" is not allowed for capability classes ["coder"] under policy version 3`: `model "small" is not allowed for capability classes ["coder"] under policy version 3`,
	} {
		if got := s.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
