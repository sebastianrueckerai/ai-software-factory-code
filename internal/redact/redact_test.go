package redact

import "testing"

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
		`model "coding-cheap" is not allowed for capability classes ["coder"] under policy version 3`: `model "coding-cheap" is not allowed for capability classes ["coder"] under policy version 3`,
	} {
		if got := s.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
