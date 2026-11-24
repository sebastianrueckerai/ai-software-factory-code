package bump

import "testing"

func TestNewestOfALine(t *testing.T) {
	for _, c := range []struct {
		name, cur      string
		tags           []string
		release, patch bool
		pick, newer    string
	}{
		{name: "a patch moves", cur: "1.26.10", tags: []string{"1.26.9", "1.26.10", "1.26.11"}, pick: "1.26.11"},
		{name: "numbers compare as numbers", cur: "1.108.3", tags: []string{"1.108.9", "1.108.16", "1.108.3"}, pick: "1.108.16"},
		{name: "a minor moves within the major", cur: "v0.33.0", tags: []string{"v0.33.0", "v0.34.2", "v0.35.1"}, pick: "v0.35.1"},
		{name: "the suffix stays its own", cur: "1.26.10-alpine", tags: []string{"1.26.11", "1.26.11-alpine", "1.26.12-alpine3.22"}, pick: "1.26.11-alpine"},
		{name: "no pre-release", cur: "v0.33.0", tags: []string{"v0.34.0-rc1", "v0.35.0-beta.1", "v0.34.2", "1.0.0rc1"}, pick: "v0.34.2"},
		{name: "a newer major is named", cur: "v1.19.5", tags: []string{"v1.19.6", "v2.0.0", "v2.4.1", "v2.5.0-rc.1"}, pick: "v1.19.6", newer: "v2.4.1"},
		{name: "patch keeps the minor", cur: "1.26.10", tags: []string{"1.26.11", "1.27.2", "1.27.3-alpine"}, patch: true, pick: "1.26.11", newer: "1.27.2"},
		{name: "patch keeps a two-part version", cur: "1.25-bookworm", tags: []string{"1.25-bookworm", "1.26-bookworm", "1.25.9-bookworm"}, patch: true, pick: "1.25-bookworm", newer: "1.26-bookworm"},
		{name: "two parts take two parts", cur: "18.6-system-trixie", tags: []string{"18.7-system-trixie", "18.7.1-system-trixie", "19.0-system-trixie"}, pick: "18.7-system-trixie", newer: "19.0-system-trixie"},
		{name: "four parts", cur: "25.12.11.4", tags: []string{"25.12.11.10", "25.12.12.1", "26.1.1.1"}, pick: "25.12.12.1", newer: "26.1.1.1"},
		{name: "the v stays", cur: "v1.10.1", tags: []string{"1.10.3", "v1.10.2"}, pick: "v1.10.2"},
		{name: "never back", cur: "1.2.3", tags: []string{"1.2.1", "1.2.2"}, pick: "1.2.3"},
		{name: "none listed keeps it", cur: "1.2.3", pick: "1.2.3"},
		{name: "a word is no version", cur: "bookworm-slim", tags: []string{"trixie-slim", "bookworm-20261123-slim"}, pick: "bookworm-slim"},
		{name: "one number is no version", cur: "22-bookworm-slim", tags: []string{"24-bookworm-slim", "22.11.0-bookworm-slim"}, pick: "22-bookworm-slim"},
		{name: "a bare major is no version", cur: "3", tags: []string{"3", "4", "3.1.0"}, pick: "3"},
		{name: "an image tag's version starts it", cur: "act-22.04", tags: []string{"act-24.04", "act-22.10"}, pick: "act-22.04"},
		{name: "a release tag carries a prefix", cur: "temporal-1.8.0", tags: []string{"temporal-1.9.1", "temporal-operator-0.2.0", "1.9.2"}, release: true, pick: "temporal-1.9.1"},
		{name: "a prefix may hold a digit", cur: "k8s-infra-0.17.1", tags: []string{"k8s-infra-0.17.2", "signoz-0.152.0"}, release: true, pick: "k8s-infra-0.17.2"},
		{name: "a prefix may hold a slash", cur: "histogram-quantile/v0.0.1", tags: []string{"histogram-quantile/v0.0.2", "v0.151.0"}, release: true, pick: "histogram-quantile/v0.0.2"},
		{name: "go's own prefix", cur: "go1.26.10", tags: []string{"go1.26.11", "go1.27.1", "go1.27rc2"}, release: true, patch: true, pick: "go1.26.11", newer: "go1.27.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pick, newer := Newest(c.cur, c.tags, c.release, c.patch)
			if pick != c.pick || newer != c.newer {
				t.Errorf("Newest(%q) = %q, newer %q; want %q, newer %q", c.cur, pick, newer, c.pick, c.newer)
			}
		})
	}
}

func TestDebianVersionsCompareByTheirRuns(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{
		{"17.11-1.pgdg12+2", "17.12-1.pgdg12+1", true},
		{"17.9-1.pgdg12+1", "17.10-1.pgdg12+1", true},
		{"19.0-1.pgdg12+1", "19.0-2.pgdg12+1", true},
		{"18.7-1.pgdg12+2", "18.7-1.pgdg12+1", false},
		{"295.pgdg12+1", "297.pgdg12+1", true},
		{"19~beta1-1.pgdg12+1", "19.0-1.pgdg12+1", true},
		{"17.12-1.pgdg12+1", "17.12-1.pgdg12+1", false},
	} {
		if got := debianLess(c.a, c.b); got != c.less {
			t.Errorf("debianLess(%q, %q) = %v, want %v", c.a, c.b, got, c.less)
		}
	}
}
