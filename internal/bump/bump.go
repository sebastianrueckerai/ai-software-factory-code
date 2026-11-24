package bump

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Files are the repository's files a bump reads and writes: the two lists,
// the Dockerfiles, go.mod and go.sum, the project's images and the plugin's
// package files. go.sum and the lockfile move by go and npm
// (cmd/upstream-bump), the rest by Bump.
var Files = []string{"upstream-images.txt", "upstream-files.txt", "Dockerfile", "Dockerfile.runner", "Dockerfile.sandbox",
	"go.mod", "go.sum", "factory/images.yaml", "opencode-plugin/package.json", "opencode-plugin/package-lock.json"}

var dockerfiles = []string{"Dockerfile", "Dockerfile.runner", "Dockerfile.sandbox"}

// Report is what a bump did, in lines for the pull request's text.
type Report struct {
	// Moved names each pin that moved: its line, from and to.
	Moved []string
	// Newer names the newer lines a rule left: a newer major, or a newer
	// minor of a patch line.
	Newer []string
	// Failed names each pin the bump could not resolve, which it kept, and
	// why.
	Failed []string
}

// Text is the report as the pull request's description.
func (r Report) Text() string {
	var b strings.Builder
	b.WriteString("The factory's lookup moved the pins below to the newest release of their lines: upstream-bump, run in the bump pod.\n")
	for _, part := range []struct {
		head  string
		lines []string
	}{
		{"Moved", r.Moved},
		{"Newer lines exist, which the rules of upstream-images.txt or a pin's major leave", r.Newer},
		{"Not resolved, kept as they were", r.Failed},
	} {
		if len(part.lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s:\n", part.head)
		for _, l := range part.lines {
			b.WriteString("- " + l + "\n")
		}
	}
	return b.String()
}

// pass is one bump over the files.
type pass struct {
	ctx    context.Context
	w      *Web
	files  map[string]string
	report Report
	// images: each image line's reference that moved, old to new
	images map[string]string
	// downloads: each file line that moved
	downloads []download
}

type download struct {
	pkg, oldVersion, newVersion, oldURL, newURL, oldSum, newSum string
	// from and to are the release's tags
	from, to string
}

// Bump moves the pins in files, each of Files by its path, to the newest
// release of their lines, and keeps every place that names a pin in step.
// It returns the files it changed. A pin it cannot resolve stays as it is
// and stands in the report with why.
func Bump(ctx context.Context, w *Web, files map[string]string) (map[string]string, Report) {
	p := &pass{ctx: ctx, w: w, files: maps.Clone(files), images: map[string]string{}}
	p.imageList()
	p.fileList()
	p.dockerfiles()
	p.goMod()
	p.projectImages()
	p.plugin()
	changed := map[string]string{}
	for path, s := range p.files {
		if s != files[path] {
			changed[path] = s
		}
	}
	return changed, p.report
}

func (p *pass) moved(format string, a ...any) {
	p.report.Moved = append(p.report.Moved, fmt.Sprintf(format, a...))
}
func (p *pass) failed(format string, a ...any) {
	p.report.Failed = append(p.report.Failed, fmt.Sprintf(format, a...))
}

// imageList moves the lines of upstream-images.txt: each line its own
// lead's first, then the with lines after the line they follow.
func (p *pass) imageList() {
	const list = "upstream-images.txt"
	var lines []imageLine
	names := map[string]int{}
	for _, raw := range strings.Split(p.files[list], "\n") {
		if !pinLine(raw) {
			continue
		}
		l, err := parseImageLine(raw)
		if err != nil {
			p.failed("%s: %v", list, err)
			continue
		}
		lines = append(lines, l)
		if name, _, _, ok := splitImage(l.ref); ok && !strings.HasPrefix(l.ref, "https://") {
			names[name]++
		}
	}
	leads := map[string]string{}
	for _, follow := range []bool{false, true} {
		for _, l := range lines {
			if (l.rule == RuleWith) != follow || l.rule == RuleHold {
				continue
			}
			now, err := p.imageLine(l, leads, names)
			if err != nil {
				p.failed("%s: %v", strings.Fields(l.ref)[0], err)
				continue
			}
			if now != l.ref {
				p.images[l.ref] = now
			}
		}
	}
	p.files[list] = rewriteImages(p.files[list], p.images)
}

// imageLine is the line's reference moved to the newest of its line.
func (p *pass) imageLine(l imageLine, leads map[string]string, names map[string]int) (string, error) {
	if strings.HasPrefix(l.ref, "https://") {
		u, sum, _ := strings.Cut(l.ref, "@sha256:")
		d, err := p.download(u, sum, "")
		if err != nil {
			return "", err
		}
		if d.from != d.to {
			p.moved("%s → %s", d.from, d.to)
		}
		return d.newURL + "@sha256:" + d.newSum, nil
	}
	name, tag, digest, ok := splitImage(l.ref)
	if !ok {
		return "", fmt.Errorf("not name:tag@sha256:<digest>")
	}
	pick := tag
	switch l.rule {
	case RuleWith:
		lead, ok := leads[l.lead]
		if !ok {
			return "", fmt.Errorf("with %s: no line of that name leads it (one, not held, in the list)", l.lead)
		}
		v, ok := parseVersion(tag, false)
		lv, lok := parseVersion(lead, false)
		if !ok || !lok || len(v.nums) != len(lv.nums) {
			return "", fmt.Errorf("with %s: its tag %s and the lead's %s are not versions of the same parts", l.lead, tag, lead)
		}
		pick = v.prefix + joinNums(lv.nums) + v.suffix
	default:
		tags, err := imageSource(name).tags(p.ctx, p.w)
		if err != nil {
			return "", err
		}
		var newer string
		pick, newer = Newest(tag, tags, false, l.rule == RulePatch)
		if newer != "" {
			p.report.Newer = append(p.report.Newer, name+" "+newer)
		}
	}
	now, err := amd64Digest(p.ctx, p.w, name, pick)
	if err != nil {
		return "", err
	}
	if names[name] == 1 {
		leads[name] = pick
	}
	switch {
	case pick != tag:
		p.moved("%s %s → %s", name, tag, pick)
	case now != digest:
		p.moved("%s %s: a new digest", name, tag)
	}
	return name + ":" + pick + "@" + now, nil
}

func joinNums(nums []int) string {
	s := make([]string, len(nums))
	for i, n := range nums {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// download is the file at u moved to the newest release of its line,
// with the sha256 of the program's own download. A file whose release
// stays, or whose URL names no release, keeps its URL and its sum. name,
// when set, leads its newer lines.
func (p *pass) download(u, sum, name string) (download, error) {
	d := download{oldURL: u, newURL: u, oldSum: sum, newSum: sum}
	src, tag, ok := fileSource(u)
	if !ok {
		return d, nil
	}
	tags, err := src.tags(p.ctx, p.w)
	if err != nil {
		return d, err
	}
	pick, newer := Newest(tag, tags, true, false)
	if newer != "" {
		p.report.Newer = append(p.report.Newer, strings.TrimSpace(name+" "+newer))
	}
	d.from, d.to = tag, pick
	if pick == tag {
		return d, nil
	}
	o, _ := parseVersion(tag, true)
	n, _ := parseVersion(pick, true)
	d.oldVersion, d.newVersion = joinNums(o.nums), joinNums(n.nums)
	d.newURL = strings.ReplaceAll(u, d.oldVersion, d.newVersion)
	d.newSum, err = p.w.sum(p.ctx, d.newURL)
	return d, err
}

// fileList moves the lines of upstream-files.txt.
func (p *pass) fileList() {
	const list = "upstream-files.txt"
	lines := strings.Split(p.files[list], "\n")
	for i, raw := range lines {
		if !pinLine(raw) {
			continue
		}
		l, err := parseFileLine(raw)
		if err != nil {
			p.failed("%s: %v", list, err)
			continue
		}
		var d download
		if l.url == "https://vuln.go.dev/vulndb.zip" {
			d = download{oldURL: l.url, newURL: l.url, oldSum: l.sum, oldVersion: l.version}
			d.newVersion, d.newSum, err = vulnDB(p.ctx, p.w)
		} else {
			d, err = p.download(l.url, l.sum, l.pkg)
		}
		if err != nil {
			p.failed("%s %s: %v", l.pkg, l.version, err)
			continue
		}
		now := l
		if d.oldVersion != d.newVersion {
			now.version = strings.ReplaceAll(l.version, d.oldVersion, d.newVersion)
			now.file = strings.ReplaceAll(l.file, d.oldVersion, d.newVersion)
		}
		now.url, now.sum = d.newURL, d.newSum
		if now == l {
			continue
		}
		d.pkg, d.oldVersion, d.newVersion = l.pkg, l.version, now.version
		p.downloads = append(p.downloads, d)
		lines[i] = now.String()
		if now.version != l.version {
			p.moved("%s %s → %s", l.pkg, l.version, now.version)
		} else {
			p.moved("%s %s: a new sha256", l.pkg, l.version)
		}
	}
	p.files[list] = strings.Join(lines, "\n")
}

var (
	addChecksum   = regexp.MustCompile(`(?m)^ADD --checksum=sha256:([0-9a-f]{64}) .*$`)
	debianSnapArg = regexp.MustCompile(`(?m)^ARG DEBIAN_SNAPSHOT=\S+$`)
	pgdgDist      = regexp.MustCompile(`apt-archive\.postgresql\.org/pub/repos/apt (\S+)-archive`)
	pgdgArgs      = regexp.MustCompile(`(?m)^ARG (POSTGRESQL_VERSION|LIBPQ_VERSION|POSTGRESQL_COMMON_VERSION)=(\S+)$`)
)

// dockerfiles keeps the Dockerfiles in step: a FROM by its image line's
// reference, a download by its sum and the version ARG its store path
// names, DEBIAN_SNAPSHOT with the Debian image, and the PostgreSQL
// packages at what their archive serves now.
func (p *pass) dockerfiles() {
	for _, df := range dockerfiles {
		s := p.files[df]
		for old, now := range p.images {
			if !strings.HasPrefix(old, "https://") {
				s = strings.ReplaceAll(s, old, now)
			}
		}
		for _, d := range p.downloads {
			s = p.moveDownload(df, s, d)
		}
		p.files[df] = s
	}
	p.debianSnapshot()
	p.postgresql()
}

// moveDownload moves d in Dockerfile s: each ADD of its old sum takes the
// new one, and the ARG the ADD's store path names
// (generic/<package>/v${ARG}/) its new version.
func (p *pass) moveDownload(df, s string, d download) string {
	arg := regexp.MustCompile(`/generic/` + regexp.QuoteMeta(d.pkg) + `/([^/$]*)\$\{(\w+)\}([^/]*)/`)
	for _, add := range addChecksum.FindAllStringSubmatch(s, -1) {
		if add[1] != d.oldSum {
			continue
		}
		s = strings.Replace(s, add[0], strings.Replace(add[0], d.oldSum, d.newSum, 1), 1)
		m := arg.FindStringSubmatch(add[0])
		if m == nil || d.oldVersion == d.newVersion {
			continue
		}
		value := func(v string) string { return strings.TrimSuffix(strings.TrimPrefix(v, m[1]), m[3]) }
		old := "ARG " + m[2] + "=" + value(d.oldVersion)
		line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(old) + `$`)
		if !line.MatchString(s) {
			p.failed("%s: %s %s: no line %q to move", df, d.pkg, d.newVersion, old)
			continue
		}
		s = line.ReplaceAllLiteralString(s, "ARG "+m[2]+"="+value(d.newVersion))
	}
	return s
}

// debianSnapshot moves DEBIAN_SNAPSHOT to the time the new Debian image was
// made from, when the image moved.
func (p *pass) debianSnapshot() {
	var image string
	for old, now := range p.images {
		if strings.HasPrefix(old, "docker.io/library/debian:") {
			image = now
		}
	}
	if image == "" {
		return
	}
	name, tag, _, _ := splitImage(image)
	snap, err := debianSnapshot(p.ctx, p.w, name, tag)
	if err != nil {
		p.failed("DEBIAN_SNAPSHOT: %v", err)
		return
	}
	for _, df := range dockerfiles {
		if m := debianSnapArg.FindString(p.files[df]); m != "" && m != "ARG DEBIAN_SNAPSHOT="+snap {
			p.files[df] = strings.Replace(p.files[df], m, "ARG DEBIAN_SNAPSHOT="+snap, 1)
			p.movedOnce("DEBIAN_SNAPSHOT %s → %s", strings.TrimPrefix(m, "ARG DEBIAN_SNAPSHOT="), snap)
		}
	}
}

// postgresql moves the PostgreSQL ARGs to the newest version their PGDG
// dist serves: the server within its major, as its package's name holds.
func (p *pass) postgresql() {
	served := map[string]map[string]string{}
	for _, df := range dockerfiles {
		s := p.files[df]
		dist := pgdgDist.FindStringSubmatch(s)
		args := pgdgArgs.FindAllStringSubmatch(s, -1)
		if dist == nil || args == nil {
			continue
		}
		newest, ok := served[dist[1]]
		if !ok {
			var err error
			if newest, err = pgdgNewest(p.ctx, p.w, dist[1]); err != nil {
				p.failed("%s: PostgreSQL: %v", df, err)
			}
			served[dist[1]] = newest
		}
		for _, a := range args {
			pkg := map[string]string{"LIBPQ_VERSION": "libpq5", "POSTGRESQL_COMMON_VERSION": "postgresql-common"}[a[1]]
			if a[1] == "POSTGRESQL_VERSION" {
				major, _, _ := strings.Cut(a[2], ".")
				pkg = "postgresql-" + major
			}
			now := newest[pkg]
			if now == "" || now == a[2] {
				continue
			}
			s = strings.Replace(s, a[0], "ARG "+a[1]+"="+now, 1)
			p.movedOnce("%s %s → %s", pkg, a[2], now)
		}
		p.files[df] = s
	}
}

func (p *pass) movedOnce(format string, a ...any) {
	if line := fmt.Sprintf(format, a...); !slices.Contains(p.report.Moved, line) {
		p.report.Moved = append(p.report.Moved, line)
	}
}

var (
	goLine      = regexp.MustCompile(`(?m)^docker\.io/library/golang:(\d+\.\d+\.\d+)@`)
	toolchain   = regexp.MustCompile(`(?m)^toolchain go\S+$`)
	projectPin  = regexp.MustCompile(`(?m)^(\s*url: )(\S+)\n(\s*sha256: )([0-9a-f]{64})$`)
	goDownload  = regexp.MustCompile(`^https://go\.dev/dl/go(\d+\.\d+\.\d+)\.`)
	pluginEntry = regexp.MustCompile(`"@opencode-ai/plugin": "([^"]+)"`)
)

// theGo is the one Go of the builds: the version of the plain golang line.
func (p *pass) theGo() string {
	if m := goLine.FindStringSubmatch(p.files["upstream-images.txt"]); m != nil {
		return m[1]
	}
	return ""
}

// goMod moves go.mod's toolchain to the one Go.
func (p *pass) goMod() {
	if v := p.theGo(); v != "" {
		p.files["go.mod"] = toolchain.ReplaceAllLiteralString(p.files["go.mod"], "toolchain go"+v)
	}
}

// projectImages moves factory/images.yaml's upstream downloads: Go's to
// the one Go, every other to its upstream-files.txt line's move.
func (p *pass) projectImages() {
	const path = "factory/images.yaml"
	p.files[path] = projectPin.ReplaceAllStringFunc(p.files[path], func(pin string) string {
		m := projectPin.FindStringSubmatch(pin)
		u, sum := m[2], m[4]
		if g := goDownload.FindStringSubmatch(u); g != nil {
			if v := p.theGo(); v != "" && v != g[1] {
				u = strings.Replace(u, "go"+g[1]+".", "go"+v+".", 1)
				s, err := p.w.sum(p.ctx, u)
				if err != nil {
					p.failed("%s: %v", path, err)
					return pin
				}
				sum = s
			}
		}
		for _, d := range p.downloads {
			if d.oldURL == u {
				u, sum = d.newURL, d.newSum
			}
		}
		return m[1] + u + "\n" + m[3] + sum
	})
}

// plugin keeps the opencode plugin's version with the opencode line's.
func (p *pass) plugin() {
	const path = "opencode-plugin/package.json"
	for _, d := range p.downloads {
		if d.pkg == "opencode" {
			v := strings.TrimPrefix(d.newVersion, "v")
			p.files[path] = pluginEntry.ReplaceAllLiteralString(p.files[path], `"@opencode-ai/plugin": "`+v+`"`)
		}
	}
}
