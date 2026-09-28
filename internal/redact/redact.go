// Package redact is the factory's one scrubber for text that leaves a
// process: audit log records, git errors, the gate supervisor's bundle.
package redact

import (
	"regexp"
	"strings"
)

// Scrubber replaces PEM blocks, the configured secrets, URL userinfo,
// bearer and basic credentials, JWTs, sk- keys and any run of 32 or more
// key characters with a marker.
type Scrubber struct {
	secrets []string
}

var (
	pemBlock   = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----`)
	userinfo   = regexp.MustCompile(`(\w+://)[^/\s@]+@`)
	credential = regexp.MustCompile(`(?i)\b(bearer|basic)\s+([^\s"',;]+)`)
	// shortWord is a credential position holding a word ("missing bearer
	// token"), not a credential
	shortWord = regexp.MustCompile(`^[A-Za-z]{1,7}$`)
	keyLike   = regexp.MustCompile(`\beyJ[\w-]+\.[\w-]+\.[\w-]*|\bsk-[\w-]{6,}|[\w+/=-]{32,}`)
)

// minSecret keeps a short configured value from redacting common words.
const minSecret = 8

// New is a scrubber for secrets besides the generic rules; values shorter
// than 8 characters are ignored.
func New(secrets ...string) *Scrubber {
	s := &Scrubber{}
	for _, v := range secrets {
		if len(v) >= minSecret {
			s.secrets = append(s.secrets, v)
		}
	}
	return s
}

// String is v with every rule applied.
func (s *Scrubber) String(v string) string {
	v = pemBlock.ReplaceAllString(v, "[pem redacted]")
	for _, secret := range s.secrets {
		v = strings.ReplaceAll(v, secret, "[redacted]")
	}
	v = userinfo.ReplaceAllString(v, "${1}[redacted]@")
	v = credential.ReplaceAllStringFunc(v, func(m string) string {
		p := credential.FindStringSubmatch(m)
		if shortWord.MatchString(p[2]) {
			return m
		}
		return p[1] + " [redacted]"
	})
	return keyLike.ReplaceAllString(v, "[redacted]")
}
