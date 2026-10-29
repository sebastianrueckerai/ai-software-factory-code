// Package redact is the factory's one scrubber for text that leaves a
// process: audit log records, git errors, the gate supervisor's bundle,
// the session record.
package redact

import (
	"regexp"
	"strings"
)

// Scrubber replaces PEM blocks, the configured secrets, URL userinfo,
// bearer and basic credentials, JWTs, sk- keys and any run of 32 or more
// key characters that holds a digit, a + or an = and is not a commit id, a
// sha256 digest or a lowercase path with a marker.
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
	keyLike   = regexp.MustCompile(`\beyJ[\w-]+\.[\w-]+\.[\w-]*|\bsk-[\w-]{6,}`)
	longRun   = regexp.MustCompile(`(?:sha256:)?[\w+/=-]{32,}`)
	// gitSHA and digest are what the 32-character rule keeps: a commit id,
	// an image or blob digest
	gitSHA    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digest    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	lowerPath = regexp.MustCompile(`^[a-z0-9/_-]+$`)
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
	v = s.Values(pemBlock.ReplaceAllString(v, "[pem redacted]"))
	v = userinfo.ReplaceAllString(v, "${1}[redacted]@")
	v = credential.ReplaceAllStringFunc(v, func(m string) string {
		p := credential.FindStringSubmatch(m)
		if shortWord.MatchString(p[2]) {
			return m
		}
		return p[1] + " [redacted]"
	})
	// a run that holds a key shape goes whole, so no part of the key stays
	v = longRun.ReplaceAllStringFunc(v, func(m string) string {
		if keyLike.MatchString(m) {
			return "[redacted]"
		}
		return m
	})
	v = keyLike.ReplaceAllString(v, "[redacted]")
	return longRun.ReplaceAllStringFunc(v, func(m string) string {
		// a long identifier or a path holds none of the three; a random key
		// of 32 characters almost always does
		if !strings.ContainsAny(strings.TrimPrefix(m, "sha256:"), "0123456789+=") || gitSHA.MatchString(m) || digest.MatchString(m) || repoPath(m) {
			return m
		}
		return "[redacted]"
	})
}

// Values is v with the configured secrets masked and no other rule
// applied: for text that a full scrub judges later, whose hashes must stay
// until then.
func (s *Scrubber) Values(v string) string {
	for _, secret := range s.secrets {
		v = strings.ReplaceAll(v, secret, "[redacted]")
	}
	return v
}

// repoPath is a run that is a lowercase path, every long segment of it
// words joined by - or _: a repository, file or URL path, not a key.
func repoPath(run string) bool {
	if !strings.Contains(run, "/") || !lowerPath.MatchString(run) {
		return false
	}
	for _, seg := range strings.Split(run, "/") {
		if len(seg) >= 32 && !strings.ContainsAny(seg, "-_") {
			return false
		}
	}
	return true
}
