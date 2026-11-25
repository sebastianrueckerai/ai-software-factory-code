package reproducible

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
)

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

// storePath is the download's <package>/<version>/<file> in the store,
// or "" when the URL reads anywhere but the store's generic packages.
func storePath(url string) string {
	rest, ok := strings.CutPrefix(url, pins.StoreGeneric)
	if !ok || strings.Contains(rest, "?") {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return rest
}

// Every file the builds download with ADD is the store's copy of a line of
// upstream-files.txt at the same sha256 (roadmap 203, step 2):
// scripts/upstream-files fills the store from the list, so a download no
// line names has no store copy to read, and a sha256 that differs pins
// bytes the line does not name. The build VM reads the list's buildkit
// line itself (internal/factoryd/buildvm.go), the runner image's download.
// Hand-derived from the two Dockerfiles as they stand: ten lines, the
// runner image's eight downloads and the sandbox's two, opencode and
// gotestsum; node and the PGDG key serve both images.
func TestUpstreamFilesNameEveryDownload(t *testing.T) {
	listed := pins.Files()
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
	if len(want) != 10 {
		t.Errorf("the builds download %d files, want the ten of the two Dockerfiles", len(want))
	}
	if bk := pins.FileOf("buildkit"); want[bk.StorePath()] != bk.SHA256 || bk.SHA256 == "" {
		t.Errorf("upstream-files.txt: the build VM's buildkit line %q is no download of Dockerfile.runner", bk)
	}
	seen := map[string]bool{}
	for _, l := range listed {
		p := l.StorePath()
		sha, ok := want[p]
		if !ok {
			t.Errorf("upstream-files.txt: %s names no ADD download of the Dockerfiles", l.URL)
			continue
		}
		if seen[p] {
			t.Errorf("upstream-files.txt: %s twice", l.URL)
		}
		if l.SHA256 != sha {
			t.Errorf("upstream-files.txt: %s pins sha256 %s, its download pins %s", l.URL, l.SHA256, sha)
		}
		seen[p] = true
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("upstream-files.txt: no line names the store's %s", p)
		}
	}
}

// The lists' parser takes the lines of each list's shapes, skips blanks
// and comments, and names every other line with the shape it wants; a
// line's store copy and its text follow its fields. Hand-derived fixtures.
func TestPinsParseTheLists(t *testing.T) {
	sum := strings.Repeat("a", 64)
	files, err := pins.ParseFiles("# a comment\n\n" +
		"tool v1 tool.tar.gz https://example.org/dl/v1/tool.tar.gz " + sum + "\n" +
		"short v1 tool.tar.gz https://example.org/tool.tar.gz\n" +
		"plain v1 tool.tar.gz http://example.org/tool.tar.gz " + sum + "\n" +
		"bad v1 tool.tar.gz https://example.org/tool.tar.gz abc\n" +
		"other v1 other.tar.gz https://example.org/tool.tar.gz " + sum + "\n" +
		"a/b v1 tool.tar.gz https://example.org/tool.tar.gz " + sum)
	want := []pins.File{{Package: "tool", Version: "v1", Name: "tool.tar.gz", URL: "https://example.org/dl/v1/tool.tar.gz", SHA256: sum}}
	if !slices.Equal(files, want) {
		t.Errorf("files %+v, want %+v", files, want)
	}
	shape := "want <package> <version> <file> <https URL> <64 hex of the file's sha256>"
	store := "the store takes generic/<package>/<version>/<file>, the file its URL's name"
	if got, wantErr := fmt.Sprint(err), `"short v1 tool.tar.gz https://example.org/tool.tar.gz": `+shape+"\n"+
		`"plain v1 tool.tar.gz http://example.org/tool.tar.gz `+sum+`": `+shape+"\n"+
		`"bad v1 tool.tar.gz https://example.org/tool.tar.gz abc": `+shape+"\n"+
		`"other v1 other.tar.gz https://example.org/tool.tar.gz `+sum+`": `+store+"\n"+
		`"a/b v1 tool.tar.gz https://example.org/tool.tar.gz `+sum+`": `+store; got != wantErr {
		t.Errorf("files error\n%s\nwant\n%s", got, wantErr)
	}
	if got := files[0].StoreURL(); got != "https://git.aisoftwarefactory.world/api/packages/upstream/generic/tool/v1/tool.tar.gz" {
		t.Errorf("StoreURL = %s", got)
	}
	if got := files[0].String(); got != "tool v1 tool.tar.gz https://example.org/dl/v1/tool.tar.gz "+sum {
		t.Errorf("String = %s", got)
	}
	digest := "@sha256:" + sum
	images, err := pins.ParseImages("# images\n" +
		"example.org/a:1" + digest + "\n" +
		"example.org/b:2" + digest + " bootstrap\n" +
		"https://example.org/c-1.tgz" + digest + " helm\n" +
		"https://example.org/d.bin" + digest + " onto example.org/a:1\n" +
		"example.org/e:3" + digest + " later\n" +
		"https://example.org/f.bin" + digest + " onto\n" +
		"example.org/g:4" + digest + " patch\n" +
		"example.org/h:5" + digest + " bootstrap with example.org/g\n" +
		"https://example.org/i.bin" + digest + " onto example.org/a:1 hold\n" +
		"example.org/j:6" + digest + " hold patch\n" +
		"example.org/l:8" + digest + " bootstrap hold with example.org/g\n" +
		"example.org/m:9" + digest + " hold with\n" +
		"example.org/k:7" + digest + " bootstrap with\n")
	wantImages := []pins.Image{
		{Ref: "example.org/a:1" + digest},
		{Ref: "example.org/b:2" + digest, Kind: "bootstrap"},
		{Ref: "https://example.org/c-1.tgz" + digest, Kind: "helm"},
		{Ref: "https://example.org/d.bin" + digest, Kind: "onto", Onto: "example.org/a:1"},
		{Ref: "example.org/g:4" + digest, Rule: "patch"},
		{Ref: "example.org/h:5" + digest, Kind: "bootstrap", Rule: "with", Lead: "example.org/g"},
		{Ref: "https://example.org/i.bin" + digest, Kind: "onto", Onto: "example.org/a:1", Rule: "hold"},
		{Ref: "example.org/l:8" + digest, Kind: "bootstrap", Rule: "hold", Lead: "example.org/g"},
	}
	if !slices.Equal(images, wantImages) {
		t.Errorf("images %+v, want %+v", images, wantImages)
	}
	kinds := "want a reference, then nothing, bootstrap, helm or onto <name>:<tag>, then nothing, patch, hold, with <name> or hold with <name>"
	if got, wantErr := fmt.Sprint(err), `"example.org/e:3`+digest+` later": `+kinds+"\n"+
		`"https://example.org/f.bin`+digest+` onto": `+kinds+"\n"+
		`"example.org/j:6`+digest+` hold patch": `+kinds+"\n"+
		`"example.org/m:9`+digest+` hold with": `+kinds+"\n"+
		`"example.org/k:7`+digest+` bootstrap with": `+kinds; got != wantErr {
		t.Errorf("images error\n%s\nwant\n%s", got, wantErr)
	}
}

// No build reaches an upstream for its files (roadmap 203, step 2): the
// three Dockerfiles and the build VM's cloud-init carry no URL of the
// hosts upstream-files.txt downloads from — the store's copies answer
// every read, and a missing one fails the build instead of falling back.
func TestBuildsNameNoUpstreamFileURL(t *testing.T) {
	banned := []string{"https://github.com/", "https://nodejs.org/", "https://code.forgejo.org/", "https://www.postgresql.org/", "https://vuln.go.dev/"}
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
// naming the file, and a store's copy of other bytes than the line's
// sha256 fails it too: the store keeps a version's first upload, so the
// line can never be served. go.sum's modules and the opencode lockfile's npm
// packages ride as one tarball each, versioned by their lockfile's
// sha256, and no code of a dependency runs to fill them: go runs on its
// local toolchain with -mod=mod, npm ci with --ignore-scripts. A stateful
// stub curl stands in for the store (a PUT is a later GET's answer), stub
// go and npm for the caches: the second run uploads nothing.
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
		"out=; verb=GET; up=; prev=\n" +
		"for a; do [ \"$prev\" = -o ] && out=$a; [ \"$prev\" = -X ] && verb=$a; [ \"$prev\" = --upload-file ] && up=$a; prev=$a; done\n" +
		"case \"$a\" in\n" +
		generic + "/*)\n" +
		"  name=$(printf %s \"${a##*generic/}\" | tr / _)\n" +
		"  if [ \"$verb\" = PUT ]; then cp \"$up\" " + dir + "/state/$name; exit 0; fi\n" +
		"  [ -f " + dir + "/state/$name ] || exit 22\n" +
		"  if [ -n \"$out\" ]; then cp " + dir + "/state/$name \"$out\"; else cat " + dir + "/state/$name; fi ;;\n" +
		"https://example.org/held.txt) printf 'held bytes' > \"$out\" ;;\n" +
		"https://example.org/new.tar.gz) printf 'new bytes' > \"$out\" ;;\n" +
		"https://example.org/rot.bin) printf 'rot bytes' > \"$out\" ;;\n" +
		"*) exit 22 ;;\n" +
		"esac\n"
	goStub := "#!/bin/sh\necho \"go $* GOTOOLCHAIN=$GOTOOLCHAIN GOFLAGS=$GOFLAGS\" >> " + dir + "/calls\n" +
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
	write(filepath.Join("state", "held_v1_held.txt"), "held bytes")
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
		"go mod download GOTOOLCHAIN=local GOFLAGS=-mod=mod\n",
		"go env GOMODCACHE GOTOOLCHAIN=local GOFLAGS=-mod=mod\n",
		"npm ci --ignore-scripts --cache ",
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
	// the store holds other bytes under the line's version: the copy
	// fails naming it, and fetches nothing
	write(filepath.Join("state", "moved_v4_moved.zip"), "old bytes")
	write("moved", "moved v4 moved.zip https://example.org/new.tar.gz "+newsum+"\n")
	before, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(script, "moved")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REGISTRY_USER=u", "REGISTRY_PASSWORD=p")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "moved v4 moved.zip: the store holds sha256 "+sum("old bytes")+", the line pins "+newsum) {
		t.Errorf("upstream-files on a store copy of other bytes: %v: %s, want moved.zip named with both sums", err, out)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.TrimPrefix(string(calls), string(before)), "https://example.org/") {
		t.Errorf("a file the store holds was fetched:\n%s", calls)
	}
}
