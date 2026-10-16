package factoryd

import "strings"

// sandboxTools are the tools of the sandbox image as the brief names them,
// each with the apt packages of Dockerfile.sandbox behind it; Go and Node
// come from stages of the image. A test holds this table to the Dockerfile.
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
	{"Go 1.26", nil},
	{"Node 24 with npm", nil},
}

// sandboxIncidental are the image's packages that are no tool the agent
// is told about.
var sandboxIncidental = []string{"ca-certificates", "tzdata", "xz-utils", "libc6-dev", "libpq5", "postgresql-common", "postgresql-client-common"}

// SandboxFacts is the brief's statement of what the sandbox has and
// reaches. The names are sandboxRegistries, the list the sandbox's egress
// policy and DNS allowlist are built from.
func SandboxFacts() string {
	tools := make([]string, len(sandboxTools))
	for i, t := range sandboxTools {
		tools[i] = t.text
	}
	return "The sandbox has " + strings.Join(tools, ", ") + ". It reaches the factory's gateway and Git server and " +
		strings.Join(sandboxRegistries, ", ") + " (the Go module proxy and the npm registry); every other name fails to resolve. " +
		"It has no browser and none can be installed, since the download hosts are not reachable: a check that needs a screenshot or a page in a browser cannot run here."
}
