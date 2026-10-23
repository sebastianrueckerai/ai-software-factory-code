// Package imagegen renders a project's declared sandbox image (policy.ImagesFile)
// into a Dockerfile for the trusted builder.
package imagegen

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Version is bumped when Render's output changes for the same inputs.
const Version = "2"

const storeBase = "https://git.aisoftwarefactory.world/api/packages/upstream/generic/"

var baseRefRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?(/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$`)

// Manifest names what an image holds: apt names then upstream names in file
// order, and the egress hosts its session may reach.
type Manifest struct {
	Tools  []string `json:"tools"`
	Egress []string `json:"egress"`
}

// BuildKey is "k-" and the first 24 hex characters of sha256 over the
// canonical JSON of spec, a newline, baseDigest, a newline and Version.
func BuildKey(spec policy.ImageSpec, baseDigest string) string {
	// No json tags: the key hashes Go field names, so renaming a field changes every key.
	b, _ := json.Marshal(spec)
	sum := sha256.Sum256([]byte(string(b) + "\n" + baseDigest + "\n" + Version))
	return fmt.Sprintf("k-%x", sum[:12])
}

// StoreURL is where the store holds a copy of an upstream line.
func StoreURL(u policy.UpstreamLine) string {
	return storeBase + u.Name + "/" + u.SHA256 + "/" + u.File()
}

// stageRoots are the image's directories an upstream line may fill, as
// policy restricts its to.
var stageRoots = []string{"/opt", "/usr/local"}

// Render returns the Dockerfile, the store copy list (upstream-files.txt
// lines) and the manifest of spec built on baseRef, which must pin a digest.
// Upstream files unpack as root in a stage of their own, under /stage, and
// the image takes /stage/opt and /stage/usr/local by COPY after its apt
// step, with nothing run after: a symlink one line plants cannot carry a
// later line's write outside those directories.
func Render(spec policy.ImageSpec, baseRef string) (dockerfile []byte, copies []byte, m Manifest, err error) {
	if !baseRefRE.MatchString(baseRef) {
		return nil, nil, m, fmt.Errorf("base %q must be <registry path>@sha256:<64 hex>", baseRef)
	}
	if findings := (policy.ImagesFile{Images: map[string]policy.ImageSpec{"x": spec}}).Validate(); len(findings) > 0 {
		return nil, nil, m, errors.New(strings.Join(findings, "\n"))
	}
	m = Manifest{Tools: slices.Clone(spec.Apt), Egress: slices.Clone(spec.Egress)}
	if m.Tools == nil {
		m.Tools = []string{}
	}
	if m.Egress == nil {
		m.Egress = []string{}
	}
	var d, c strings.Builder
	staged := map[string]bool{}
	if len(spec.Upstream) > 0 {
		fmt.Fprintf(&d, "FROM %s AS inputs\nUSER root\n", baseRef)
	}
	for _, u := range spec.Upstream {
		m.Tools = append(m.Tools, u.Name)
		in, to := "/tmp/factory-inputs/"+u.Name, "/stage"+u.To
		fmt.Fprintf(&d, "ADD --checksum=sha256:%s %s %s\n", u.SHA256, StoreURL(u), in)
		if u.IsArchive() {
			fmt.Fprintf(&d, "RUN mkdir -p %[1]s && tar -xf %[2]s -C %[1]s --strip-components=%[3]d --no-same-owner && rm %[2]s\n", to, in, u.Strip)
		} else {
			fmt.Fprintf(&d, "RUN install -D -m 0755 %[1]s %[2]s && rm %[1]s\n", in, to)
		}
		for _, root := range stageRoots {
			if u.To == root || strings.HasPrefix(u.To, root+"/") {
				staged[root] = true
			}
		}
		fmt.Fprintf(&c, "%s %s %s %s %s\n", u.Name, u.SHA256, u.File(), u.URL, u.SHA256)
	}
	fmt.Fprintf(&d, "FROM %s\nUSER root\n", baseRef)
	if len(spec.Apt) > 0 {
		fmt.Fprintf(&d, "RUN apt-get update && apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n", strings.Join(spec.Apt, " "))
	}
	for _, root := range stageRoots {
		if staged[root] {
			fmt.Fprintf(&d, "COPY --from=inputs /stage%[1]s/ %[1]s/\n", root)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		fmt.Fprintf(&d, "ENV %s=%s\n", k, quote(spec.Env[k]))
	}
	if len(spec.Path) > 0 {
		fmt.Fprintf(&d, "ENV PATH=%s:$PATH\n", strings.Join(spec.Path, ":"))
	}
	label, _ := json.Marshal(m)
	fmt.Fprintf(&d, "LABEL world.aisoftwarefactory.manifest=%s\nUSER 1000:1000\n", quote(string(label)))
	return []byte(d.String()), []byte(c.String()), m, nil
}

// quote double-quotes s for a Dockerfile instruction.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(s) + `"`
}
