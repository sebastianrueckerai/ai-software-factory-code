package gateway

import (
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"

	// the embedded tz database: a provider entry's tz, such as
	// Asia/Singapore, loads in any image, with no host zone files.
	_ "time/tzdata"
)

// RefusedProviderLimit is the decision of a call the provider refused
// with a named reset: the gateway answers 429 with the reset and the
// provider's words, and the class freezes until then.
const RefusedProviderLimit = "refused_provider_limit"

// defaultResetPatterns is the default pattern set the gateway reads a
// provider's body with, in order; a provider's own patterns replace it.
// Each names the datetime in its first capture group.
var defaultResetPatterns = []*regexp.Regexp{
	// a datetime after words such as "reset at": zai's five-hour limit,
	// with or without an offset.
	regexp.MustCompile(`(?i)reset (?:at|time|on)[:\s]+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?)`),
}

// resetHeaderStamps are the layouts a reset header's value may carry; a
// value that parses in none of them is a relative hint or unknown, and
// never freezes.
var resetHeaderStamps = []string{time.RFC3339, "2006-01-02 15:04:05Z07:00", "2006-01-02T15:04:05Z0700"}

// bodyStamps are the layouts a body pattern's datetime may carry, offset
// forms first so an offset always reads as is.
var bodyStamps = []string{
	time.RFC3339,
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z0700",
	time.RFC3339Nano,
}

// naiveStamps are the layouts a body pattern's naive datetime may carry;
// such a stamp reads in the provider's zone, UTC without an entry.
var naiveStamps = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"}

// parseStamp reads s as an absolute moment: an offset form as is, a naive
// form in loc.
func parseStamp(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range bodyStamps {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	for _, layout := range naiveStamps {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}

	return time.Time{}, false
}

// ParseProviderReset reads the absolute reset moment a provider's refusal
// names: pure string work, no model call and no I/O. In order: a reset
// header in RFC 3339 or ISO 8601 with its offset; a Retry-After in
// HTTP-date form (the seconds form is a relative hint and never freezes);
// a datetime in the body after the pattern set's words, read in the
// provider's zone. entry's patterns replace defaultResetPatterns; its tz
// turns a naive stamp into a moment, UTC without an entry. A relative
// hint in any unit, a bare status and an unreadable body return false.
func ParseProviderReset(h http.Header, body []byte, entry *policy.ProviderLimit) (time.Time, bool) {
	// reset-named headers first, whatever the map's random order: a
	// reset header beats an HTTP-date Retry-After.
	for name, values := range h {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "retry-after") || !strings.Contains(lower, "reset") {
			continue
		}
		for _, v := range values {
			for _, layout := range resetHeaderStamps {
				if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
					return t, true
				}
			}
		}
	}
	for name, values := range h {
		if !strings.Contains(strings.ToLower(name), "retry-after") {
			continue
		}
		for _, v := range values {
			// the seconds form is a relative hint; only the
			// HTTP-date form names a moment.
			if _, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				continue
			}
			if t, err := http.ParseTime(v); err == nil {
				return t, true
			}
		}
	}
	patterns := defaultResetPatterns
	if entry != nil && len(entry.Patterns) > 0 {
		patterns = make([]*regexp.Regexp, 0, len(entry.Patterns))
		for _, p := range entry.Patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				continue
			}
			patterns = append(patterns, re)
		}
	}
	loc := time.UTC
	if entry != nil {
		loc = entry.Location()
	}
	for _, re := range patterns {
		if m := re.FindSubmatch(body); m != nil {
			if t, ok := parseStamp(string(m[1]), loc); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// heldClasses are the classes a call is held under: the task's own, or for
// a factoryd call on the project's chat task, which holds no class of its
// own, every class whose alias of the purpose the call runs on, the card
// classes the brief and the check-in read a case with.
func heldClasses(doc *policy.Document, task Task, purpose, model string) []string {
	if purpose == "" || !slices.Contains(task.Classes, policy.ClassChat) {
		return task.Classes
	}
	var classes []string
	for _, name := range slices.Sorted(maps.Keys(doc.Classes)) {
		if doc.FactoryModel([]string{name}, purpose) == model {
			classes = append(classes, name)
		}
	}
	return classes
}
