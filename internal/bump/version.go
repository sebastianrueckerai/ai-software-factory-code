// Package bump moves the pins of the factory's outside inputs to the
// newest release of their lines: the weekly bump's lookup, run by
// cmd/upstream-bump in the bump pod.
package bump

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// dotted is a version's numbers: at least two, joined by dots. A tag with
// one number (3, 22-bookworm-slim) names a line, not a version.
var dotted = regexp.MustCompile(`\d+(?:\.\d+)+`)

type version struct {
	prefix, suffix string
	nums           []int
}

// parseVersion reads tag as a version. An image tag's numbers start it,
// after an optional v; a release tag's may follow any prefix
// (temporal-1.8.0, histogram-quantile/v0.0.1).
func parseVersion(tag string, release bool) (version, bool) {
	at := dotted.FindStringIndex(tag)
	if at == nil {
		return version{}, false
	}
	v := version{prefix: tag[:at[0]], suffix: tag[at[1]:]}
	if !release && v.prefix != "" && v.prefix != "v" {
		return version{}, false
	}
	for _, n := range strings.Split(tag[at[0]:at[1]], ".") {
		i, err := strconv.Atoi(n)
		if err != nil {
			return version{}, false
		}
		v.nums = append(v.nums, i)
	}
	return v, true
}

// kin: same prefix, suffix and number of parts. A pre-release differs in
// its suffix, so it is never kin of a release.
func (v version) kin(o version) bool {
	return v.prefix == o.prefix && v.suffix == o.suffix && len(v.nums) == len(o.nums)
}

// Newest is the newest tag of cur's line among tags, never older than cur,
// and the newest kin tag the line leaves out, "" for none: a newer major,
// or with patch a newer minor. A line is cur's first number, with patch
// its first two. A tag that is no version is its own line.
func Newest(cur string, tags []string, release, patch bool) (pick, newer string) {
	c, ok := parseVersion(cur, release)
	if !ok {
		return cur, ""
	}
	keep := 1
	if patch {
		keep = 2
	}
	best, other := c, version{}
	pick = cur
	for _, tag := range tags {
		v, ok := parseVersion(tag, release)
		if !ok || !c.kin(v) {
			continue
		}
		if slices.Equal(v.nums[:min(keep, len(v.nums))], c.nums[:min(keep, len(c.nums))]) {
			if slices.Compare(v.nums, best.nums) > 0 {
				best, pick = v, tag
			}
		} else if slices.Compare(v.nums, c.nums) > 0 && (other.nums == nil || slices.Compare(v.nums, other.nums) > 0) {
			other, newer = v, tag
		}
	}
	return pick, newer
}

// debianLess orders two Debian package versions as dpkg does, the epoch
// aside: runs of digits compare as numbers, other runs character by
// character with letters before other characters and ~ before all, even
// the end.
func debianLess(a, b string) bool {
	order := func(s string, i int) int {
		switch {
		case i >= len(s) || s[i] >= '0' && s[i] <= '9':
			return 0
		case s[i] == '~':
			return -1
		case s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z':
			return int(s[i])
		}
		return int(s[i]) + 256
	}
	digit := func(s string, i int) bool { return i < len(s) && s[i] >= '0' && s[i] <= '9' }
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for i < len(a) && !digit(a, i) || j < len(b) && !digit(b, j) {
			if x, y := order(a, i), order(b, j); x != y {
				return x < y
			}
			i, j = i+1, j+1
		}
		x, y := 0, 0
		for ; digit(a, i); i++ {
			x = x*10 + int(a[i]-'0')
		}
		for ; digit(b, j); j++ {
			y = y*10 + int(b[j]-'0')
		}
		if x != y {
			return x < y
		}
	}
	return false
}
