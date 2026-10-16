package reproducible

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// upstreamFileLine is one line of upstream-files.txt: a file's package
// name, version, file name, URL and sha256.
type upstreamFileLine struct {
	pkg, version, name, url, sha string
}

func readUpstreamFiles(t *testing.T) []upstreamFileLine {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "upstream-files.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []upstreamFileLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		text := sc.Text()
		fields := strings.Fields(text)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) != 5 || !strings.HasPrefix(fields[3], "https://") || !hex64.MatchString(fields[4]) {
			t.Errorf("upstream-files.txt: %q: want <package> <version> <file> <https URL> <64 hex of the file's sha256>", text)
			continue
		}
		l := upstreamFileLine{pkg: fields[0], version: fields[1], name: fields[2], url: fields[3], sha: fields[4]}
		if strings.ContainsAny(l.pkg, "/") || strings.ContainsAny(l.version, "/") || strings.ContainsAny(l.name, "/") || l.name != l.url[strings.LastIndex(l.url, "/")+1:] {
			t.Errorf("upstream-files.txt: %q: the store takes generic/<package>/<version>/<file>, the file its URL's name", text)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

var (
	addLine  = regexp.MustCompile(`^ADD\s+(?:--\S+\s+)*(https://\S+)\s+\S+$`)
	argRef   = regexp.MustCompile(`\$\{([A-Z_]+)\}`)
	checksum = regexp.MustCompile(`--checksum=sha256:([0-9a-f]{64})\b`)
)

// dockerfileARGs is the file's ARG defaults, by name: a global ARG
// before the first FROM holds for every stage, and a bare ARG in a
// stage re-names one without a value of its own.
func dockerfileARGs(lines []string) map[string]string {
	args := map[string]string{}
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, "ARG ")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(rest) {
			if k, v, ok := strings.Cut(f, "="); ok {
				if _, dup := args[k]; !dup {
					args[k] = v
				}
			}
		}
	}
	return args
}

// addDownloads is every ADD download of the Dockerfile at path: its URL
// with the file's ARG defaults filled in, by its --checksum sha256.
// Every URL must be the store's copy of an upstream-files.txt line:
// /api/packages/upstream/generic/<package>/<version>/<file>.
func addDownloads(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := logicalLines(string(raw))
	args := dockerfileARGs(lines)
	downloads := map[string]string{}
	for _, line := range lines {
		m := addLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		url := argRef.ReplaceAllStringFunc(m[1], func(s string) string {
			v, ok := args[s[2:len(s)-1]]
			if !ok {
				t.Fatalf("%s: ADD %s reads %s, which no ARG of the file fills", path, m[1], s)
			}
			return v
		})
		c := checksum.FindStringSubmatch(line)
		if c == nil {
			t.Fatalf("%s: ADD %s carries no --checksum", path, url)
		}
		if prev, ok := downloads[url]; ok && prev != c[1] {
			t.Fatalf("%s: %s is pinned at two sha256", path, url)
		}
		downloads[url] = c[1]
	}
	return downloads
}

// genericStoreURL is the store's copy of a line of upstream-files.txt:
// https://git.aisoftwarefactory.world/api/packages/upstream/generic/<package>/<version>/<file>.
const genericStoreURL = "https://git.aisoftwarefactory.world/api/packages/upstream/generic/"

// storePath is the download's <package>/<version>/<file> in the store,
// or "" when the URL reads anywhere but the store's generic packages.
func storePath(url string) string {
	rest, ok := strings.CutPrefix(url, genericStoreURL)
	if !ok || strings.Contains(rest, "?") {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return rest
}

var (
	buildVMVersion = regexp.MustCompile(`buildkitVersion\s*=\s*"([^"]+)"`)
	buildVMSHA256  = regexp.MustCompile(`buildkitSHA256\s*=\s*"([0-9a-f]{64})"`)
	buildVMTarURL  = regexp.MustCompile(`https://git\.aisoftwarefactory\.world/api/packages/upstream/generic/buildkit/\{\{\.Version\}\}/[^\s'"]+`)
)

// buildVMsTarball is the buildkit tarball the build VM's cloud-init
// curls (ruling 110, internal/factoryd/buildvm.go), pinned by
// buildkitSHA256: its store path and sha256 as upstream-files.txt names
// them.
func buildVMsTarball(t *testing.T) (path, sha string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "factoryd", "buildvm.go"))
	if err != nil {
		t.Fatal(err)
	}
	v := buildVMVersion.FindStringSubmatch(string(raw))
	s := buildVMSHA256.FindStringSubmatch(string(raw))
	u := buildVMTarURL.FindString(string(raw))
	if v == nil || s == nil || u == "" {
		t.Fatal("buildvm.go: no buildkit version, sha256 and store tarball URL of the VM's user data")
	}
	return storePath(strings.ReplaceAll(u, "{{.Version}}", v[1])), s[1]
}

// Every file the builds download with ADD and the build VM's buildkit
// tarball are the store's copy of a line of upstream-files.txt at the
// same sha256 (roadmap 203, step 2): scripts/upstream-files fills the
// store from the list, so a download no line names has no store copy to
// read, and a sha256 that differs pins bytes the line does not name.
// Hand-derived from the two Dockerfiles and buildvm.go as they stand:
// eight lines, the runner image's seven downloads (its buildkit tarball
// also the build VM's) and opencode's; node and the PGDG key serve both
// images.
func TestUpstreamFilesNameEveryDownload(t *testing.T) {
	listed := readUpstreamFiles(t)
	if len(listed) == 0 {
		t.Fatal("upstream-files.txt lists no file")
	}
	want := map[string]string{}
	for _, f := range []string{"Dockerfile.runner", "Dockerfile.sandbox"} {
		for url, sha := range addDownloads(t, filepath.Join("..", "..", f)) {
			p := storePath(url)
			if p == "" {
				t.Fatalf("%s: ADD %s reads outside the store's generic packages", f, url)
			}
			want[p] = sha
		}
	}
	path, sha := buildVMsTarball(t)
	if path == "" {
		t.Fatal("buildvm.go: the VM's buildkit tarball reads outside the store's generic packages")
	}
	if prev, ok := want[path]; ok && prev != sha {
		t.Fatalf("the build VM's %s is pinned at two sha256", path)
	}
	want[path] = sha
	if len(want) != 8 {
		t.Errorf("the builds download %d files, want the eight of the two Dockerfiles and the build VM", len(want))
	}
	seen := map[string]bool{}
	for _, l := range listed {
		p := l.pkg + "/" + l.version + "/" + l.name
		sha, ok := want[p]
		if !ok {
			t.Errorf("upstream-files.txt: %s names no ADD download of the Dockerfiles nor the build VM's tarball", l.url)
			continue
		}
		if seen[p] {
			t.Errorf("upstream-files.txt: %s twice", l.url)
		}
		if l.sha != sha {
			t.Errorf("upstream-files.txt: %s pins sha256 %s, its download pins %s", l.url, l.sha, sha)
		}
		seen[p] = true
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("upstream-files.txt: no line names the store's %s", p)
		}
	}
}

// No build reaches an upstream for its files (roadmap 203, step 2): the
// three Dockerfiles and the build VM's cloud-init carry no URL of the
// hosts upstream-files.txt downloads from — the store's copies answer
// every read, and a missing one fails the build instead of falling back.
func TestBuildsNameNoUpstreamFileURL(t *testing.T) {
	banned := []string{"https://github.com/", "https://nodejs.org/", "https://code.forgejo.org/", "https://www.postgresql.org/"}
	for _, f := range []string{"Dockerfile", "Dockerfile.sandbox", "Dockerfile.runner"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range banned {
			if strings.Contains(string(raw), host) {
				t.Errorf("%s: the build reaches %s", f, host)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "factoryd", "buildvm.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range banned {
		if strings.Contains(string(raw), host) {
			t.Errorf("buildvm.go: the build VM reaches %s", host)
		}
	}
}

// scripts/upstream-files uploads only what the store lacks: a file the
// store holds is left alone, a missing one is downloaded, checked
// against its line's sha256 and PUT, and a mismatch fails the copy
// naming the file. go.sum's modules and the opencode lockfile's npm
// packages ride as one tarball each, versioned by their lockfile's
// sha256. A stateful stub curl stands in for the store (a PUT is a
// later GET's answer), stub go and npm for the caches: the second run
// uploads nothing.
func TestUpstreamFilesUploadWhatTheStoreLacks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(body string) string {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	}
	heldsum, newsum := sum("held bytes"), sum("new bytes")
	gosum, locksum := sum("go.sum bytes\n"), sum("lock bytes\n")
	write("files", "held v1 held.txt https://example.org/held.txt "+heldsum+"\n"+
		"new v2 new.tar.gz https://example.org/new.tar.gz "+newsum+"\n")
	write("go.sum", "go.sum bytes\n")
	if err := os.MkdirAll(filepath.Join(dir, "opencode-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("opencode-plugin", "package-lock.json"), "lock bytes\n")
	const generic = "https://git.aisoftwarefactory.world/api/packages/upstream/generic"
	curl := "#!/bin/sh\necho \"curl $*\" >> " + dir + "/calls\n" +
		"out=; verb=GET; prev=\n" +
		"for a; do [ \"$prev\" = -o ] && out=$a; [ \"$prev\" = -X ] && verb=$a; prev=$a; done\n" +
		"case \"$a\" in\n" +
		generic + "/*)\n" +
		"  name=$(printf %s \"${a##*generic/}\" | tr / _)\n" +
		"  if [ \"$verb\" = PUT ]; then touch " + dir + "/state/$name; exit 0; fi\n" +
		"  [ -f " + dir + "/state/$name ] && exit 0\n" +
		"  exit 22 ;;\n" +
		"https://example.org/held.txt) printf 'held bytes' > \"$out\" ;;\n" +
		"https://example.org/new.tar.gz) printf 'new bytes' > \"$out\" ;;\n" +
		"https://example.org/rot.bin) printf 'rot bytes' > \"$out\" ;;\n" +
		"*) exit 22 ;;\n" +
		"esac\n"
	goStub := "#!/bin/sh\necho \"go $*\" >> " + dir + "/calls\n" +
		"case \"$1 $2\" in\n" +
		"\"env GOMODCACHE\") echo " + dir + "/modcache ;;\n" +
		"\"mod download\") ;;\n" +
		"*) exit 1 ;;\n" +
		"esac\n"
	npmStub := "#!/bin/sh\necho \"npm $*\" >> " + dir + "/calls\n" +
		"cache=; prev=\n" +
		"for a; do [ \"$prev\" = --cache ] && cache=$a; prev=$a; done\n" +
		"mkdir -p \"$cache/_cacache/content-v2\"\n" +
		"printf 'x' > \"$cache/_cacache/content-v2/sha256\"\n"
	for name, body := range map[string]string{"curl": curl, "go": goStub, "npm": npmStub} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"state", filepath.Join("modcache", "cache", "download", "example.com", "mod", "@v")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("state", "held_v1_held.txt"), "")
	write(filepath.Join("modcache", "cache", "download", "example.com", "mod", "@v", "list"), "v1.0.0\n")
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "upstream-files"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(list string) string {
		t.Helper()
		cmd := exec.Command(script, list)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REGISTRY_USER=u", "REGISTRY_PASSWORD=p")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("upstream-files: %v: %s", err, out)
		}
		calls, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		return string(calls)
	}
	first := run("files")
	for _, want := range []string{
		generic + "/held/v1/held.txt",
		generic + "/new/v2/new.tar.gz",
		generic + "/go-modules/" + gosum + "/go-modules.tar.gz",
		generic + "/npm-cache/" + locksum + "/npm-cache.tar.gz",
		"-X PUT --upload-file ",
		"go mod download",
		"go env GOMODCACHE",
		"npm ci --cache ",
		"https://example.org/new.tar.gz",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the copy's calls name no %s:\n%s", want, first)
		}
	}
	if strings.Contains(first, "https://example.org/held.txt") {
		t.Errorf("the store's held file was downloaded:\n%s", first)
	}
	second := run("files")
	for _, l := range strings.Split(strings.TrimPrefix(second, first), "\n") {
		switch {
		case l == "":
		case strings.HasPrefix(l, "curl ") && !strings.Contains(l, "-X PUT"):
		default:
			t.Errorf("the second run works beyond a read of the store: %s", l)
		}
	}
	// a wrong sha256 fails the copy naming the file
	write("rot", "rot v3 rot.bin https://example.org/rot.bin "+newsum+"\n")
	cmd := exec.Command(script, "rot")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REGISTRY_USER=u", "REGISTRY_PASSWORD=p")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "rot.bin") {
		t.Errorf("upstream-files on a wrong sha256: %v: %s, want rot.bin named", err, out)
	}
}
