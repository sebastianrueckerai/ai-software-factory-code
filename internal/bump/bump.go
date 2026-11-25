package bump

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
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
	Moved []string `json:"moved,omitempty"`
	// Held names each line hold keeps that has news: a newer version, a
	// lookup that failed, or a lead that moved.
	Held []string `json:"held,omitempty"`
	// HeldKept counts the held lines with none.
	HeldKept int `json:"held_kept,omitempty"`
	// Newer names the newer lines a rule left: a newer major, or a newer
	// minor of a patch line.
	Newer []string `json:"newer,omitempty"`
	// Failed names each pin the bump could not resolve, which it kept, and
	// why: the pin, then ": " and the error.
	Failed []string `json:"failed,omitempty"`
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
		{"Held by rule (hold in the lists), not moved", r.held()},
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

func (r Report) held() []string {
	if r.HeldKept == 0 {
		return r.Held
	}
	return append(slices.Clone(r.Held), fmt.Sprintf("held lines with nothing newer: %d", r.HeldKept))
}

// pass is one bump over the files.
type pass struct {
	ctx    context.Context
	w      *Web
	files  map[string]string
	report Report
	// images: each image line's reference that moved, old to new
	images map[string]string
	// tags: each image name whose tag moved, to its new tag
	tags map[string]string
	// leads: each image name that one line of the list holds, not held, to
	// the tag it has now
	leads map[string]string
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
	p := &pass{ctx: ctx, w: w, files: maps.Clone(files), images: map[string]string{}, tags: map[string]string{}, leads: map[string]string{}}
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
	lines, err := pins.ParseImages(p.files[list])
	if err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			p.failed("%s: %s", list, e)
		}
	}
	names := map[string]int{}
	for _, l := range lines {
		if name, _, _, ok := splitImage(l.Ref); ok && !strings.HasPrefix(l.Ref, "https://") {
			names[name]++
		}
	}
	for _, follow := range []bool{false, true} {
		for _, l := range lines {
			if (l.Rule == pins.RuleWith) != follow || l.Rule == pins.RuleHold {
				continue
			}
			now, err := p.imageLine(l, names)
			if err != nil {
				p.failed("%s: %v", strings.Fields(l.Ref)[0], err)
				continue
			}
			if now != l.Ref {
				p.images[l.Ref] = now
			}
		}
	}
	for _, l := range lines {
		if l.Rule == pins.RuleHold {
			p.heldImage(l)
		}
	}
	p.files[list] = rewriteImages(p.files[list], p.images)
}

// heldImage looks held line l up, as the bump looks up any line, and
// leaves it as it is.
func (p *pass) heldImage(l pins.Image) {
	if strings.HasPrefix(l.Ref, "https://") {
		u, _, _ := strings.Cut(l.Ref, "@")
		tag, pick, newer, _, err := p.releases(u, false)
		p.held(u, tag, l.Lead, cmp.Or(newer, pick), err)
		return
	}
	name, tag, _, ok := splitImage(l.Ref)
	if !ok {
		p.held(l.Ref, "", l.Lead, "", nil)
		return
	}
	tags, err := imageSource(name).tags(p.ctx, p.w)
	pick, newer := Newest(tag, tags, false, false)
	p.held(name, tag, l.Lead, cmp.Or(newer, pick), err)
}

// held reports held line name at tag: its newest version when that is
// newer, a lookup that failed, and the move of the line it names to move
// with by hand. A line with none of these only counts.
func (p *pass) held(name, tag, lead, newest string, err error) {
	line := strings.TrimSpace(name + " " + tag)
	news := err != nil
	switch {
	case err != nil:
		line += ": not looked up: " + err.Error()
	case newest != "" && newest != tag:
		line = fmt.Sprintf("%s held at %s, newest %s", name, tag, newest)
		news = true
	}
	if to, moved := p.tags[lead]; moved && lead != "" {
		line += fmt.Sprintf(": %s moved to %s; %s is held and moves by hand with it", lead, to, name)
		news = true
	}
	if !news {
		p.report.HeldKept++
		return
	}
	p.report.Held = append(p.report.Held, line)
}

// imageLine is the line's reference moved to the newest of its line.
func (p *pass) imageLine(l pins.Image, names map[string]int) (string, error) {
	if strings.HasPrefix(l.Ref, "https://") {
		u, sum, _ := strings.Cut(l.Ref, "@sha256:")
		d, err := p.download(u, sum, "", l.Rule == pins.RulePatch)
		if err != nil {
			return "", err
		}
		if d.from != d.to {
			p.moved("%s → %s", d.from, d.to)
		}
		return d.newURL + "@sha256:" + d.newSum, nil
	}
	name, tag, digest, ok := splitImage(l.Ref)
	if !ok {
		return "", fmt.Errorf("not name:tag@sha256:<digest>")
	}
	pick := tag
	switch l.Rule {
	case pins.RuleWith:
		lead, err := p.lead(l.Lead)
		if err != nil {
			return "", err
		}
		v, ok := parseVersion(tag, false)
		lv, lok := parseVersion(lead, false)
		if !ok || !lok || len(v.nums) != len(lv.nums) {
			return "", fmt.Errorf("with %s: its tag %s and the lead's %s are not versions of the same parts", l.Lead, tag, lead)
		}
		pick = v.prefix + joinNums(lv.nums) + v.suffix
	default:
		tags, err := imageSource(name).tags(p.ctx, p.w)
		if err != nil {
			return "", err
		}
		var newer string
		pick, newer = Newest(tag, tags, false, l.Rule == pins.RulePatch)
		if newer != "" {
			p.report.Newer = append(p.report.Newer, name+" "+newer)
		}
	}
	now, err := amd64Digest(p.ctx, p.w, name, pick)
	if err != nil {
		return "", err
	}
	if names[name] == 1 {
		p.leads[name] = pick
	}
	switch {
	case pick != tag:
		p.tags[name] = pick
		p.moved("%s %s → %s", name, tag, pick)
	case now != digest:
		p.moved("%s %s: a new digest", name, tag)
	}
	return name + ":" + pick + "@" + now, nil
}

// lead is the tag of the one line of image name that the list does not
// hold.
func (p *pass) lead(name string) (string, error) {
	tag, ok := p.leads[name]
	if !ok {
		return "", fmt.Errorf("with %s: no line of that name leads it (one, not held, in the list)", name)
	}
	return tag, nil
}

func joinNums(nums []int) string {
	s := make([]string, len(nums))
	for i, n := range nums {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// releases is the release tag of the file at u, the newest of its line
// among its maker's (in its minor with patch), and the newest the line
// leaves out. ok is false for a URL that names no release.
func (p *pass) releases(u string, patch bool) (tag, pick, newer string, ok bool, err error) {
	src, tag, ok := fileSource(u)
	if !ok {
		return "", "", "", false, nil
	}
	tags, err := src.tags(p.ctx, p.w)
	if err != nil {
		return tag, tag, "", true, err
	}
	pick, newer = Newest(tag, tags, true, patch)
	return tag, pick, newer, true, nil
}

// download is the file at u moved to the newest release of its line,
// with the sha256 of the program's own download. A file whose release
// stays, or whose URL names no release, keeps its URL and its sum. name,
// when set, leads its newer lines.
func (p *pass) download(u, sum, name string, patch bool) (download, error) {
	d := download{oldURL: u, newURL: u, oldSum: sum, newSum: sum}
	tag, pick, newer, ok, err := p.releases(u, patch)
	if !ok || err != nil {
		return d, err
	}
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
	return p.sumAt(d)
}

// sumAt moves d's URL from its old version to its new one and takes the
// sha256 of the file there.
func (p *pass) sumAt(d download) (download, error) {
	d.newURL = strings.ReplaceAll(d.oldURL, d.oldVersion, d.newVersion)
	var err error
	d.newSum, err = p.w.sum(p.ctx, d.newURL)
	return d, err
}

// follow is the file of line l moved to the version of the image line it
// follows: the numbers of the lead's tag in place of the line's.
func (p *pass) follow(l pins.File) (download, error) {
	d := download{oldURL: l.URL, newURL: l.URL, oldSum: l.SHA256, newSum: l.SHA256}
	lead, err := p.lead(l.Lead)
	if err != nil {
		return d, err
	}
	v, ok := parseVersion(l.Version, true)
	lv, lok := parseVersion(lead, false)
	if !ok || !lok || len(v.nums) != len(lv.nums) {
		return d, fmt.Errorf("with %s: its version %s and the lead's %s are not versions of the same parts", l.Lead, l.Version, lead)
	}
	d.oldVersion, d.newVersion = joinNums(v.nums), joinNums(lv.nums)
	if d.oldVersion == d.newVersion {
		return d, nil
	}
	return p.sumAt(d)
}

// heldFile looks held file line l up, as heldImage does.
func (p *pass) heldFile(l pins.File) {
	tag, pick, newer, ok, err := p.releases(l.URL, false)
	if !ok {
		tag = l.Version
	}
	p.held(l.Package, tag, l.Lead, cmp.Or(newer, pick), err)
}

// fileList moves the lines of upstream-files.txt.
func (p *pass) fileList() {
	const list = "upstream-files.txt"
	lines, err := pins.ParseFiles(p.files[list])
	if err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			p.failed("%s: %s", list, e)
		}
	}
	moved := map[string]string{}
	for _, l := range lines {
		var d download
		switch {
		case l.Rule == pins.RuleHold:
			p.heldFile(l)
			continue
		case l.Rule == pins.RuleWith:
			d, err = p.follow(l)
		case l.URL == "https://vuln.go.dev/vulndb.zip":
			d = download{oldURL: l.URL, newURL: l.URL, oldSum: l.SHA256, oldVersion: l.Version}
			d.newVersion, d.newSum, err = vulnDB(p.ctx, p.w, l.Version, l.SHA256)
		default:
			d, err = p.download(l.URL, l.SHA256, l.Package, l.Rule == pins.RulePatch)
		}
		if err != nil {
			p.failed("%s %s: %v", l.Package, l.Version, err)
			continue
		}
		now := l
		if d.oldVersion != d.newVersion {
			now.Version = strings.ReplaceAll(l.Version, d.oldVersion, d.newVersion)
			now.Name = strings.ReplaceAll(l.Name, d.oldVersion, d.newVersion)
		}
		now.URL, now.SHA256 = d.newURL, d.newSum
		if now == l {
			continue
		}
		if now.Version == l.Version {
			// the store keeps a version's first upload for good
			p.failed("%s %s: %s is other bytes under the same version, which the store never serves; the line stays", l.Package, l.Version, now.URL)
			continue
		}
		d.pkg, d.oldVersion, d.newVersion = l.Package, l.Version, now.Version
		p.downloads = append(p.downloads, d)
		moved[l.String()] = now.String()
		p.moved("%s %s → %s", l.Package, l.Version, now.Version)
	}
	p.files[list] = rewriteLines(p.files[list], moved)
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
// dist serves: the server within its major, as its package's name holds,
// and libpq within its major too, a newer one named.
func (p *pass) postgresql() {
	served := map[string]map[string][]string{}
	for _, df := range dockerfiles {
		s := p.files[df]
		dist := pgdgDist.FindStringSubmatch(s)
		args := pgdgArgs.FindAllStringSubmatch(s, -1)
		if dist == nil || args == nil {
			continue
		}
		versions, ok := served[dist[1]]
		if !ok {
			var err error
			if versions, err = pgdgVersions(p.ctx, p.w, dist[1]); err != nil {
				p.failed("%s: PostgreSQL: %v", df, err)
			}
			served[dist[1]] = versions
		}
		for _, a := range args {
			major, _, _ := strings.Cut(a[2], ".")
			pkg := map[string]string{"POSTGRESQL_VERSION": "postgresql-" + major, "LIBPQ_VERSION": "libpq5", "POSTGRESQL_COMMON_VERSION": "postgresql-common"}[a[1]]
			now, newer := a[2], ""
			for _, v := range versions[pkg] {
				switch {
				case a[1] == "LIBPQ_VERSION" && !strings.HasPrefix(v, major+"."):
					if debianLess(now, v) {
						newer = v
					}
				case debianLess(now, v):
					now = v
				}
			}
			if newer != "" {
				p.newerOnce(pkg + " " + newer)
			}
			if now == a[2] {
				continue
			}
			s = strings.Replace(s, a[0], "ARG "+a[1]+"="+now, 1)
			p.movedOnce("%s %s → %s", pkg, a[2], now)
		}
		p.files[df] = s
	}
}

func (p *pass) newerOnce(line string) {
	if !slices.Contains(p.report.Newer, line) {
		p.report.Newer = append(p.report.Newer, line)
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
