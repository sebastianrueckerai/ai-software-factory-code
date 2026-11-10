package reproducible

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOpencodePluginVersionsFollowTheDockerfileArg holds the plugin's
// lockfile to Dockerfile.sandbox's OPENCODE_VERSION: the weekly bump
// (PR #1598) raised the ARG and left the lockfile behind, and only the
// 40-minute image build noticed.
func TestOpencodePluginVersionsFollowTheDockerfileArg(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.sandbox"))
	if err != nil {
		t.Fatalf("read Dockerfile.sandbox: %v", err)
	}
	m := regexp.MustCompile(`ARG OPENCODE_VERSION=(\S+)`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("Dockerfile.sandbox holds no ARG OPENCODE_VERSION=...")
	}
	want := strings.TrimPrefix(m[1], "v")

	repo := filepath.Join("..", "..", "opencode-plugin")
	pkg, err := os.ReadFile(filepath.Join(repo, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkgJSON struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(pkg, &pkgJSON); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	pkgVersion := pkgJSON.Dependencies["@opencode-ai/plugin"]

	lock, err := os.ReadFile(filepath.Join(repo, "package-lock.json"))
	if err != nil {
		t.Fatalf("read package-lock.json: %v", err)
	}
	var lockJSON struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lock, &lockJSON); err != nil {
		t.Fatalf("parse package-lock.json: %v", err)
	}
	pluginVersion := lockJSON.Packages["node_modules/@opencode-ai/plugin"].Version
	sdkVersion := lockJSON.Packages["node_modules/@opencode-ai/sdk"].Version

	t.Run("main", func(t *testing.T) {
		for _, c := range []struct{ what, got string }{
			{"opencode-plugin/package.json dependency", pkgVersion},
			{"opencode-plugin/package-lock.json node_modules/@opencode-ai/plugin", pluginVersion},
			{"opencode-plugin/package-lock.json node_modules/@opencode-ai/sdk", sdkVersion},
		} {
			if c.got != want {
				t.Errorf("%s is %q, Dockerfile.sandbox ARG OPENCODE_VERSION is %q: all three must read %s", c.what, c.got, m[1], want)
			}
		}
	})

	// One table case with a mismatched lockfile, to show the failure text
	// this test is meant to produce: exactly the drift PR #1598 caused.
	t.Run("mismatched lockfile fails", func(t *testing.T) {
		for _, tc := range []struct {
			arg   string
			pkg   string
			plug  string
			sdk   string
			fails bool
		}{
			{arg: "v1.18.32", pkg: "1.18.32", plug: "1.18.32", sdk: "1.18.32", fails: false},
			{arg: "v1.19.5", pkg: "1.18.32", plug: "1.18.32", sdk: "1.18.32", fails: true},
			{arg: "v1.19.5", pkg: "1.19.5", plug: "1.19.5", sdk: "1.18.32", fails: true},
		} {
			want := strings.TrimPrefix(tc.arg, "v")
			var msgs []string
			for _, c := range []struct{ what, got string }{
				{"opencode-plugin/package.json dependency", tc.pkg},
				{"opencode-plugin/package-lock.json node_modules/@opencode-ai/plugin", tc.plug},
				{"opencode-plugin/package-lock.json node_modules/@opencode-ai/sdk", tc.sdk},
			} {
				if c.got != want {
					msgs = append(msgs, fmt.Sprintf("%s is %q, Dockerfile.sandbox ARG OPENCODE_VERSION is %q: all three must read %s", c.what, c.got, tc.arg, want))
				}
			}
			if (len(msgs) > 0) != tc.fails {
				t.Errorf("arg %s: caught %v, want failure %v", tc.arg, msgs, tc.fails)
			}
		}
	})
}
