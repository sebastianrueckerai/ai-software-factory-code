package factoryd

import (
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
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

// sandboxIncidental are the image's packages that are no tool the agent
// is told about.
var sandboxIncidental = []string{"ca-certificates", "tzdata", "xz-utils", "libc6-dev", "libpq5", "postgresql-common", "postgresql-client-common"}

// SandboxFacts is the brief's statement of what the sandbox has and
// reaches: the factory's image for a nil m, else the base's tools, m's and
// m's registries. The names are those the sandbox's egress policy and DNS
// allowlist are built from (sandboxRegistries for the factory's image).
func SandboxFacts(m *imagegen.Manifest) string {
	tools := make([]string, len(sandboxTools))
	for i, t := range sandboxTools {
		tools[i] = t.text
	}
	if m == nil {
		return "The sandbox has " + strings.Join(append(tools, factoryTools...), ", ") + ". It reaches the factory's gateway and Git server and " +
			strings.Join(sandboxRegistries, ", ") + " (the Go module proxy and the npm registry); every other name fails to resolve. " +
			"It has no browser and none can be installed, since the download hosts are not reachable: a check that needs a screenshot or a page in a browser cannot run here."
	}
	reach := "no package registries"
	if len(m.Egress) > 0 {
		reach = strings.Join(m.Egress, ", ")
	}
	return "The sandbox has " + strings.Join(append(tools, m.Tools...), ", ") + ". It reaches the factory's gateway and Git server and " +
		reach + "; every other name fails to resolve. A tool it lacks cannot be installed: the download hosts are not reachable."
}
