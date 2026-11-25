package follow

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
)

type lists struct {
	images []pins.Image
	files  []pins.File
}

// outcome is what one marker comes to: have and want are the line (or for
// a copy the sha256 of the file) as it stands and as the list says it.
type outcome struct {
	behind                 bool
	have, want             string
	before, after, version string
	copyFile               string
	bytes                  []byte
}

// splitRef cuts an image reference name:tag@sha256:hex into its parts.
func splitRef(ref string) (name, tag, digest string) {
	i := strings.LastIndex(ref, "@sha256:")
	if i < 0 {
		return ref, "", ""
	}
	name, digest = ref[:i], ref[i+1:]
	if j := strings.LastIndex(name, ":"); j >= 0 && !strings.Contains(name[j:], "/") {
		name, tag = name[:j], name[j+1:]
	}
	return name, tag, digest
}

func (l lists) file(pkg string) (pins.File, error) {
	for _, f := range l.files {
		if f.Package == pkg {
			return f, nil
		}
	}
	return pins.File{}, fmt.Errorf("no line of %s has the package %s: fix the key, or add the line to the code repository's list", FilesList, pkg)
}

// image finds the one image line the key names: its name, or name:pattern
// where several lines share a name.
func (l lists) image(key string) (name, tag, digest string, err error) {
	want, pat := key, ""
	if i := strings.LastIndex(key, ":"); i >= 0 && !strings.Contains(key[i:], "/") {
		want, pat = key[:i], key[i+1:]
	}
	var re *regexp.Regexp
	if pat != "" {
		re = regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(pat), `\*`, `\d+`) + "$")
	}
	var tags, hits []string
	for _, img := range l.images {
		if strings.HasPrefix(img.Ref, "https://") {
			continue
		}
		n, t, d := splitRef(img.Ref)
		if n != want {
			continue
		}
		tags = append(tags, t)
		if re == nil || re.MatchString(t) {
			hits = append(hits, t)
			name, tag, digest = n, t, d
		}
	}
	if len(hits) > 1 {
		return "", "", "", fmt.Errorf("the key fits the lines of %s: %s: write # pin %s:<tag pattern> that fits one", ImagesList, strings.Join(tags, ", "), want)
	}
	if name == "" {
		return "", "", "", fmt.Errorf("no line of %s has this name%s: fix the key, or add the line to the code repository's list", ImagesList, map[bool]string{true: " and tag", false: ""}[re != nil])
	}
	return name, tag, digest, nil
}

func (l lists) apply(files map[string][]byte, file, line, verb, arg string, store Store) (outcome, error) {
	switch verb {
	case "copy":
		return l.copy(files, file, line, arg, store)
	case "file":
		return l.file1(line, arg)
	case "chart":
		for _, img := range l.images {
			if img.Kind != "helm" {
				continue
			}
			url, _, _ := strings.Cut(img.Ref, "@")
			base := strings.TrimSuffix(path.Base(url), ".tgz")
			if v, ok := strings.CutPrefix(base, arg+"-"); ok && versionRe.MatchString(v) {
				if digestRe.MatchString(line) {
					return outcome{}, errors.New("the line holds a digest, a chart marker names a version: put the marker above the version line")
				}
				return rewrite(line, "", "", v, true)
			}
		}
		return outcome{}, fmt.Errorf("no helm line of %s has the archive %s-<version>.tgz: fix the key, or add the line to the code repository's list", ImagesList, arg)
	case "image-of":
		for _, img := range l.images {
			url, sum, _ := strings.Cut(img.Ref, "@sha256:")
			if img.Kind != "onto" || !strings.HasSuffix(url, "/"+arg) {
				continue
			}
			tag := sum[:12]
			digest, err := store.FileImageDigest(arg, tag)
			if err != nil && !errors.Is(err, ErrOffline) {
				return outcome{}, fmt.Errorf("the store's digest for files/%s:%s: %w", arg, tag, err)
			}
			return rewrite(line, tag, digest, tag, false)
		}
		return outcome{}, fmt.Errorf("no onto line of %s has a URL ending in /%s: fix the key, or add the line to the code repository's list", ImagesList, arg)
	}
	_, tag, digest, err := l.image(arg)
	if err != nil {
		return outcome{}, err
	}
	return rewrite(line, tag, digest, tag, true)
}

func (l lists) copy(files map[string][]byte, file, line, pkg string, store Store) (outcome, error) {
	f, err := l.file(pkg)
	if err != nil {
		return outcome{}, err
	}
	p, err := copyPath(file, line)
	if err != nil {
		return outcome{}, err
	}
	data, ok := files[p]
	if !ok {
		return outcome{}, fmt.Errorf("the line names %s, which the tree does not hold: the file is a copy, create it first or fix the name", p)
	}
	have := sumHex(data)
	o := outcome{copyFile: p, have: have, want: f.SHA256, behind: have != f.SHA256, before: "sha256:" + have[:12], after: f.Version, version: f.Version}
	if !o.behind {
		return o, nil
	}
	b, err := store.FileCopy(f)
	switch {
	case errors.Is(err, ErrOffline):
	case err != nil:
		return outcome{}, fmt.Errorf("the store's copy of %s: %w", f.StorePath(), err)
	case sumHex(b) != f.SHA256:
		return outcome{}, fmt.Errorf("the store's copy of %s has the sha256 %s, the line says %s: the store holds a wrong file, copy it again", f.StorePath(), sumHex(b), f.SHA256)
	default:
		o.bytes = b
	}
	return o, nil
}

// file1 rewrites the last scalar of a file marker's line by what stands
// there: a sha256, a URL or a version.
func (l lists) file1(line, pkg string) (outcome, error) {
	f, err := l.file(pkg)
	if err != nil {
		return outcome{}, err
	}
	at := scalarRe.FindAllStringIndex(line, -1)
	if len(at) == 0 {
		return outcome{}, errors.New("the line holds no value: put the marker directly above the line that holds the version, URL or sha256")
	}
	last := at[len(at)-1]
	old := line[last[0]:last[1]]
	var want string
	switch {
	case hexRe.MatchString(old):
		want = f.SHA256
	case strings.HasPrefix(old, "https://"):
		want = f.URL
	case strings.Contains(old, "/") || digestRe.MatchString(old):
		return outcome{}, fmt.Errorf("the value %q is no version, URL or 64 hex: the marker is above a line of another shape", old)
	default:
		want = f.Version
		if downgrade(old, want) {
			return outcome{}, downgradeError(old, want)
		}
	}
	nl := line[:last[0]] + want + line[last[1]:]
	return outcome{behind: nl != line, have: line, want: nl, before: short(old), after: short(want), version: f.Version}, nil
}

func short(v string) string {
	if hexRe.MatchString(v) {
		return "sha256:" + v[:12]
	}
	return v
}

// rewrite sets the value of line, by its shape, to tag and digest (a
// digest "" keeps the one the line holds) or to version; ordered says the
// tags are versions, whose order a downgrade breaks, not hashes: tag@sha256:digest,
// else a sha256 digest alone, else the line's last scalar.
func rewrite(line, tag, digest, version string, ordered bool) (outcome, error) {
	o := outcome{have: line, version: version}
	tf := func(old, nw string) string {
		if ordered {
			return tagFor(old, nw)
		}
		return nw
	}
	finish := func(nl, before, after string) (outcome, error) {
		if ordered && nl != line && downgrade(before, after) {
			return outcome{}, downgradeError(before, after)
		}
		o.want, o.behind, o.before, o.after = nl, nl != line, before, after
		return o, nil
	}
	if m := splitRe.FindStringSubmatchIndex(line); m != nil && tag != "" {
		old, oldD := line[m[4]:m[5]], line[m[8]:m[9]]
		nt, nd := tf(old, tag), orElse(digest, oldD)
		nl := line[:m[4]] + nt + line[m[5]:m[8]] + nd + line[m[9]:]
		b, a := show(old, oldD, nt, nd)
		return finish(nl, b, a)
	}
	var out strings.Builder
	prev, first := 0, true
	var before, after string
	for _, m := range tagDigestRe.FindAllStringSubmatchIndex(line, -1) {
		pre, old, oldD := line[m[2]:m[3]], line[m[4]:m[5]], line[m[6]:m[7]]
		if pre != ":" && !versionRe.MatchString(old) || tag == "" {
			continue
		}
		nt, nd := tf(old, tag), orElse(digest, oldD)
		out.WriteString(line[prev:m[4]] + nt + "@" + nd)
		prev = m[7]
		if first {
			before, after = show(old, oldD, nt, nd)
			first = false
		}
	}
	if !first {
		out.WriteString(line[prev:])
		return finish(out.String(), before, after)
	}
	if digestRe.MatchString(line) {
		if digest == "" && tag == "" {
			return outcome{}, errors.New("the line holds a digest, and the key's line has none to give: the marker is above a line of another shape")
		}
		if digest == "" {
			return outcome{have: line, want: line, version: version}, nil
		}
		old := digestRe.FindString(line)
		nl := digestRe.ReplaceAllString(line, digest)
		return finish(nl, "sha256:"+old[7:19], "sha256:"+digest[7:19])
	}
	at := scalarRe.FindAllStringIndex(line, -1)
	if len(at) == 0 || !versionRe.MatchString(line[at[len(at)-1][0]:at[len(at)-1][1]]) {
		return outcome{}, errors.New("the value has no known shape: it must hold tag@sha256:<64 hex>, a sha256:<64 hex> digest, or a version as its last scalar. Put the marker directly above the line that holds the value")
	}
	last := at[len(at)-1]
	old := line[last[0]:last[1]]
	nv := tf(old, version)
	return finish(line[:last[0]]+nv+line[last[1]:], old, nv)
}

// show names the old and the new value: the tags, or tag and short digest
// where the tag stays.
func show(oldT, oldD, newT, newD string) (string, string) {
	if oldT != newT {
		return oldT, newT
	}
	return oldT + "@" + oldD[7:19], newT + "@" + newD[7:19]
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// tagFor is the tag the tree gets for the list's new one: a tag that is a
// bare version in the tree (a chart that appends its own suffix) stays
// bare.
func tagFor(old, tag string) string {
	if bareVer.MatchString(old) && !bareVer.MatchString(tag) {
		if p := numPrefix.FindString(tag); p != "" {
			if !strings.HasPrefix(old, "v") {
				p = strings.TrimPrefix(p, "v")
			}
			return p
		}
	}
	return tag
}

func versionOf(s string) ([]int, bool) {
	p := numPrefix.FindString(s)
	if p == "" {
		return nil, false
	}
	var out []int
	for _, n := range strings.Split(strings.TrimPrefix(p, "v"), ".") {
		v, err := strconv.Atoi(n)
		if err != nil {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// downgrade is whether the new version is older than the old one, both
// read by their leading number run; values that are no versions never are.
func downgrade(old, nw string) bool {
	a, ok := versionOf(old)
	b, ok2 := versionOf(nw)
	return ok && ok2 && slices.Compare(b, a) < 0
}

func downgradeError(old, nw string) error {
	return fmt.Errorf("the tree holds %s, newer than the list's %s: a human moved the tree by hand and not the list. Move the line in the code repository, or put the value back", old, nw)
}
