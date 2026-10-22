package factoryd

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// aptPackages are the packages the sandbox image installs with apt, read from
// Dockerfile.sandbox with the versions left off. The image's RUN block carries
// two apt-get installs on one logical line, so every occurrence counts.
func aptPackages(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../Dockerfile.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\\\n", " "), "\n") {
		for _, part := range strings.Split(line, "apt-get install -y --no-install-recommends ")[1:] {
			part, _, _ = strings.Cut(part, "&&")
			for _, f := range strings.Fields(part) {
				name, _, _ := strings.Cut(f, "=")
				pkgs = append(pkgs, name)
			}
		}
	}
	return pkgs
}

// The brief's list of tools is the image's: a package added to
// Dockerfile.sandbox without a line here, or a line here with no package,
// fails.
func TestSandboxToolsAreTheImages(t *testing.T) {
	image := aptPackages(t)
	var named []string
	for _, tool := range sandboxTools {
		named = append(named, tool.apt...)
	}
	for _, p := range image {
		if !slices.Contains(named, p) && !slices.Contains(sandboxIncidental, p) {
			t.Errorf("Dockerfile.sandbox installs %s, which sandboxTools and sandboxIncidental do not name", p)
		}
	}
	for _, p := range named {
		if !slices.Contains(image, p) {
			t.Errorf("sandboxTools names %s, which Dockerfile.sandbox does not install", p)
		}
	}
	for _, p := range sandboxIncidental {
		if !slices.Contains(image, p) {
			t.Errorf("sandboxIncidental names %s, which Dockerfile.sandbox does not install", p)
		}
	}
	b, _ := os.ReadFile("../../Dockerfile.sandbox")
	if !strings.Contains(string(b), "golang:1.26.") || !strings.Contains(string(b), "ARG NODE_VERSION=24.") {
		t.Error("Dockerfile.sandbox no longer carries Go 1.26 and Node 24, which SandboxFacts states")
	}
}

func TestSandboxFactsNameToolsNamesAndNoBrowser(t *testing.T) {
	got := SandboxFacts()
	for _, want := range []string{"git", "ripgrep (rg)", "gcc and make", "Go 1.26", "Node 24 with npm", "PostgreSQL 17",
		"proxy.golang.org", "registry.npmjs.org", "no browser"} {
		if !strings.Contains(got, want) {
			t.Errorf("SandboxFacts lacks %q:\n%s", want, got)
		}
	}
}

// The harness init container copies what the factory's image holds:
// each path harnessCopy reads is where Dockerfile.sandbox installs it.
func TestHarnessCopyReadsWhatTheImageInstalls(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, install := range []string{
		"install -m 0755 /tmp/opencode /usr/local/bin/opencode",
		"COPY --from=build /out/agent-worker /usr/local/bin/agent-worker",
		"COPY --from=opencode-plugin --chown=1000:1000 /opencode-home/.config/opencode /opt/opencode-home/.config/opencode",
	} {
		if !strings.Contains(string(b), install) {
			t.Errorf("Dockerfile.sandbox lacks %q", install)
		}
	}
	for _, read := range []string{"/usr/local/bin/opencode ", "/usr/local/bin/agent-worker ", "cp -a /opt/opencode-home /factory/"} {
		if !strings.Contains(harnessCopy, read) {
			t.Errorf("harnessCopy %q does not read %q", harnessCopy, read)
		}
	}
}
