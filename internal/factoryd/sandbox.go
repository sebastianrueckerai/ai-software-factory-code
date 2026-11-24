package factoryd

import (
	"maps"
	"slices"
	"strings"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// sandboxTools are the tools of the sandbox base as the brief names them,
// each with the apt packages of Dockerfile.sandbox behind it. A test holds
// this table to the Dockerfile.
var sandboxTools = []struct {
	text string
	apt  []string
}{
	{"git", []string{"git"}},
	{"curl", []string{"curl"}},
	{"ripgrep (rg)", []string{"ripgrep"}},
	{"ssh", []string{"openssh-client"}},
	{"gcc and make", []string{"gcc", "make"}},
	{"python3", []string{"python3"}},
	{"PostgreSQL 17 (server and client, for tests)", []string{"postgresql-17", "postgresql-client-17"}},
}

// factoryTools are what the factory's image adds to the base, from stages
// of the image.
var factoryTools = []string{"Go 1.26", "Node 24 with npm"}

// FactoryStack is what the factory's image adds to the sandbox base as an
// image of factory/images.yaml: Go as the factory repository's own
// factory/images.yaml pins go.dev/dl's tarball, Node at upstream-files.txt's
// line, the Go env and PATH of the image's final stage, and its
// registries. A test holds it to Dockerfile.sandbox.
func FactoryStack() policy.ImageSpec {
	node := pins.FileOf("node")
	return policy.ImageSpec{
		Upstream: []policy.UpstreamLine{
			factoryGo(),
			{Name: "node", URL: node.URL, SHA256: node.SHA256, To: "/usr/local", Strip: 1},
		},
		Env:    map[string]string{"GOTOOLCHAIN": "local", "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org"},
		Path:   []string{"/usr/local/go/bin", "/home/worker/go/bin"},
		Egress: slices.Clone(sandboxRegistries),
	}
}

// factoryGo is the go line of the factory repository's factory/images.yaml,
// its images in name order; a line named go without a URL when none has one.
func factoryGo() policy.UpstreamLine {
	images := pins.ImagesFile().Images
	for _, name := range slices.Sorted(maps.Keys(images)) {
		if i := slices.IndexFunc(images[name].Upstream, func(u policy.UpstreamLine) bool { return u.Name == "go" }); i >= 0 {
			return images[name].Upstream[i]
		}
	}
	return policy.UpstreamLine{Name: "go"}
}

// sandboxIncidental are the image's packages that are no tool the agent
// is told about.
var sandboxIncidental = []string{"ca-certificates", "tzdata", "xz-utils", "libc6-dev", "libpq5", "postgresql-common", "postgresql-client-common"}

// SandboxFacts is the brief's statement of what the sandbox has and
// reaches: the factory's image for a nil m, else the base's tools, m's and
// m's registries. The names are those the sandbox's egress policy and DNS
// allowlist are built from (sandboxRegistries for the factory's image).
// How a missing tool comes is request_tool's where the session's tools
// hold it; where they hold commission_server, a sentence follows on
// reaching outside hosts from a throwaway server.
func SandboxFacts(m *imagegen.Manifest, tools []string) string {
	noInstall := "A tool those hosts do not serve cannot be installed at runtime."
	if slices.Contains(tools, policy.ToolRequestTool) {
		noInstall = "A tool those hosts do not serve cannot be installed at runtime: request_tool asks the factory to add it to one of the project's images, and the card restarts on that image."
	}
	if slices.Contains(tools, policy.ToolCommission) {
		noInstall += " commission_server rents a throwaway server that reaches outside hosts over SSH: run the lookups there and release it when done."
	}
	has := make([]string, len(sandboxTools))
	for i, t := range sandboxTools {
		has[i] = t.text
	}
	if m == nil {
		return "The sandbox has " + strings.Join(append(has, factoryTools...), ", ") + ". It reaches the factory's gateway and Git server and " +
			strings.Join(sandboxRegistries, ", ") + " (the Go module proxy and the npm registry); every other name fails to resolve. " + noInstall
	}
	reach := "no package registries"
	if len(m.Egress) > 0 {
		reach = strings.Join(m.Egress, ", ")
	}
	return "The sandbox has " + strings.Join(append(has, m.Tools...), ", ") + ". It reaches the factory's gateway and Git server and " +
		reach + "; every other name fails to resolve. " + noInstall
}
