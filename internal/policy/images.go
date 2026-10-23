package policy

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxImages is the most image names one project may declare.
	MaxImages = 4
	// MaxImageBytes caps a built image's compressed size; the pin step
	// enforces it.
	MaxImageBytes = 4 << 30
)

var (
	imageNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	aptNameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	upstreamNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	digestRE       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	buildKeyRE     = regexp.MustCompile(`^k-[0-9a-f]{24}$`)
	sha256RE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envKeyRE       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	hostLabelRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	fileRE         = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	safePathRE     = regexp.MustCompile(`^/[A-Za-z0-9._/+-]*$`)
)

// IsImageName reports whether name is a valid image name: lowercase
// letters, digits and dashes, at most 32.
func IsImageName(name string) bool { return imageNameRE.MatchString(name) }

// MaxToolName and MaxToolNeed bound an ask for a sandbox tool, a session's
// request_tool and the chat's request_image, in characters.
const (
	MaxToolName = 80
	MaxToolNeed = 600
)

// ToolAskRefusal is why tool and need make no ask for a sandbox tool, ""
// when they make one: each 1 to its bound in characters, on one line
// without control characters.
func ToolAskRefusal(tool, need string) string {
	if n, m := utf8.RuneCountInString(tool), utf8.RuneCountInString(need); n == 0 || n > MaxToolName || m == 0 || m > MaxToolNeed ||
		strings.ContainsFunc(tool+need, unicode.IsControl) {
		return fmt.Sprintf("tool (1 to %d characters) and need (1 to %d characters) are required, each on one line without control characters", MaxToolName, MaxToolNeed)
	}
	return ""
}

// validate returns one finding per breach of the pin of image name.
func (p ImagePin) validate(name string) []string {
	var out []string
	bad := func(format string, a ...any) {
		out = append(out, fmt.Sprintf("image %q: "+format, append([]any{name}, a...)...))
	}
	if !imageNameRE.MatchString(name) {
		bad("name: lowercase letters, digits and dashes, at most 32")
	}
	if !digestRE.MatchString(p.Digest) {
		bad("digest %q: want sha256:<64 hex>", p.Digest)
	}
	if !buildKeyRE.MatchString(p.Key) {
		bad("key %q: want k-<24 hex>", p.Key)
	}
	if len(p.Tools) == 0 {
		bad("tools: at least one")
	}
	for _, h := range p.Egress {
		if !validHost(h) {
			bad("egress %q must be a lowercase hostname with a dot, no wildcard or IP", h)
		}
	}
	return out
}

// ImagesFile is factory/images.yaml at the root of a project's repository:
// the tools its agent sessions need, one sandbox image per name on top of the
// factory's base image. The name default is the image of every card that
// names none. Its values end up in a Dockerfile, so Validate also guards
// against injection.
type ImagesFile struct {
	Images map[string]ImageSpec `yaml:"images"`
}

// ImageSpec is one image: the tools added to the base image and the
// environment, PATH entries and egress hosts that go with them.
type ImageSpec struct {
	// Apt lists Debian packages from the base image's dated snapshot, so
	// versions are fixed and not pinned here.
	Apt []string `yaml:"apt,omitempty"`
	// Upstream lists downloads pinned by SHA-256.
	Upstream []UpstreamLine    `yaml:"upstream,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
	// Path lists directories appended to PATH.
	Path []string `yaml:"path,omitempty"`
	// Egress lists hostnames the session may reach beyond the defaults.
	Egress []string `yaml:"egress,omitempty"`
}

// UpstreamLine is one pinned download. An archive (.tar.gz, .tgz, .tar.xz)
// unpacks into To with Strip leading path components removed; any other file
// is placed at To, which lies in /opt or /usr/local.
type UpstreamLine struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	To     string `yaml:"to"`
	Strip  int    `yaml:"strip,omitempty"`
}

// ParseImagesFile decodes strictly (an unknown key is a finding) and returns
// the decode error or Validate's findings; a nil slice means the file is good.
func ParseImagesFile(data []byte) (ImagesFile, []string) {
	var f ImagesFile
	if err := decodeOne(bytes.TrimSpace(data), &f, true); err != nil {
		return f, []string{"images.yaml: " + err.Error()}
	}
	return f, f.Validate()
}

// Validate returns one finding per breach, images in name order.
func (f ImagesFile) Validate() []string {
	if len(f.Images) == 0 {
		return []string{"images.yaml: images must name at least one image"}
	}
	var out []string
	if len(f.Images) > MaxImages {
		out = append(out, fmt.Sprintf("images.yaml: at most %d images, found %d", MaxImages, len(f.Images)))
	}
	names := make([]string, 0, len(f.Images))
	for n := range f.Images {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		out = append(out, f.Images[n].validate(n)...)
	}
	return out
}

func (s ImageSpec) validate(name string) []string {
	var out []string
	bad := func(format string, a ...any) {
		out = append(out, fmt.Sprintf("image %q: ", name)+fmt.Sprintf(format, a...))
	}
	if !imageNameRE.MatchString(name) {
		bad("name must match %s", imageNameRE)
	}
	if len(s.Apt) == 0 && len(s.Upstream) == 0 {
		bad("declares no tool: apt or upstream needs an entry, or the image equals the base image")
	}

	seen := map[string]bool{}
	for _, p := range s.Apt {
		switch {
		case !aptNameRE.MatchString(p):
			bad("apt %q must match %s (a Debian package name without a version)", p, aptNameRE)
		case seen[p]:
			bad("apt %q is listed twice", p)
		}
		seen[p] = true
	}

	names, urls := map[string]bool{}, map[string]bool{}
	for _, u := range s.Upstream {
		bad := func(format string, a ...any) {
			bad("upstream %q: "+format, append([]any{u.Name}, a...)...)
		}
		if !upstreamNameRE.MatchString(u.Name) {
			bad("name must match %s", upstreamNameRE)
		}
		if names[u.Name] {
			bad("name is used twice")
		}
		names[u.Name] = true
		if msg := checkUpstreamURL(u.URL); msg != "" {
			bad("url %q %s", u.URL, msg)
		} else if urls[u.URL] {
			bad("url %q is used twice", u.URL)
		}
		urls[u.URL] = true
		if !sha256RE.MatchString(u.SHA256) {
			bad("sha256 %q must be 64 lowercase hex characters", u.SHA256)
		}
		if !cleanAbs(u.To) || !underLocal(u.To) {
			bad("to %q must be an absolute, clean path of letters, digits and ._/+- in /opt or /usr/local", u.To)
		}
		switch {
		case u.Strip < 0 || u.Strip > 8:
			bad("strip %d must be 0 to 8", u.Strip)
		case u.Strip != 0 && !u.IsArchive():
			bad("strip %d needs an archive url (.tar.gz, .tgz or .tar.xz)", u.Strip)
		}
	}

	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := s.Env[k]
		switch {
		case !envKeyRE.MatchString(k):
			bad("env %q must match %s", k, envKeyRE)
		case k == "PATH" || k == "HOME" || strings.HasPrefix(k, "OPENCODE_") || strings.HasPrefix(k, "FACTORY_"):
			bad("env %q is owned by the harness (PATH entries go in path)", k)
		case strings.HasSuffix(strings.ToUpper(k), "_PROXY") || strings.HasPrefix(strings.ToUpper(k), "LD_") || strings.HasPrefix(strings.ToUpper(k), "GIT_"):
			bad("env %q would reach the harness's git or loader: no *_PROXY, LD_* or GIT_* key", k)
		case len(v) > 1024 || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }):
			bad("env %q value must be at most 1024 bytes without control characters", k)
		}
	}

	seen = map[string]bool{}
	for _, p := range s.Path {
		switch {
		case !cleanAbs(p) || strings.Contains(p, ":"):
			bad("path %q must be an absolute, clean directory of letters, digits and ._/+-", p)
		case seen[p]:
			bad("path %q is listed twice", p)
		}
		seen[p] = true
	}

	seen = map[string]bool{}
	for _, h := range s.Egress {
		switch {
		case !validHost(h):
			bad("egress %q must be a lowercase hostname with a dot, no wildcard or IP", h)
		case seen[h]:
			bad("egress %q is listed twice", h)
		}
		seen[h] = true
	}
	return out
}

func checkUpstreamURL(raw string) string {
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= 0x20 || r >= 0x7f || strings.ContainsRune("\"'`\\$;|<>{}", r) }) {
		return "must be printable ASCII without spaces or any of \"'`\\$;|<>{}"
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "does not parse"
	case u.Scheme != "https":
		return "must use https"
	case u.Hostname() == "":
		return "needs a host"
	case u.User != nil:
		return "must not carry user info"
	case strings.Contains(raw, "%"):
		return "must not use percent-encoding"
	case !fileRE.MatchString(path.Base(u.Path)) || path.Base(u.Path) == "." || path.Base(u.Path) == ".." || strings.HasSuffix(u.Path, "/"):
		return "must end in a file name of letters, digits and ._+-"
	}
	return ""
}

// underLocal reports whether p is /opt or /usr/local or below them, where
// nothing the harness runs looks.
func underLocal(p string) bool {
	for _, dir := range []string{"/opt", "/usr/local"} {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func cleanAbs(p string) bool {
	return safePathRE.MatchString(p) && path.Clean(p) == p
}

// File is the last path segment of the URL, the name the download keeps.
func (u UpstreamLine) File() string {
	p, _ := url.Parse(u.URL)
	return path.Base(p.Path)
}

// IsArchive reports whether the download unpacks (.tar.gz, .tgz, .tar.xz).
func (u UpstreamLine) IsArchive() bool {
	f := u.File()
	for _, ext := range []string{".tar.gz", ".tgz", ".tar.xz"} {
		if strings.HasSuffix(f, ext) {
			return true
		}
	}
	return false
}

func validHost(h string) bool {
	if len(h) > 253 || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if len(l) > 63 || !hostLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}
