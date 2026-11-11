package factoryd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Stand-ins of one reserved shape: an email of a listed login, of a
// configured host or of GitHub's noreply domain, and a mention of a listed
// login. The leftover check exempts exactly these, and an npm scope
// ("@types/node").
const (
	standInEmail   = "human@example.invalid"
	standInMention = "@human"
)

// The shapes that stay in a masked fixture only by mistake, by the class
// the export names when it fails on one, and the list of the export's
// environment a human edits so the next export masks or keeps it. Each
// action, taken, clears the refusal.
var fixtureShapes = []struct {
	class, action string
	re            *regexp.Regexp
	exempt        func(m *fixtureMasker, match, rest string) bool
}{
	{"an email address", "to mask it, add its login or its mail domain to the export's FIXTURE_MASK (comma-separated)",
		regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.|\[\.\])[A-Za-z0-9.\[\]-]*[A-Za-z]`),
		func(_ *fixtureMasker, match, _ string) bool { return match == standInEmail }},
	{"an @mention", "to mask a login, add it to the export's FIXTURE_MASK; to keep a word that names no person, such as a code annotation, add it to FIXTURE_KEEP (both comma-separated)",
		regexp.MustCompile(`(?:^|[^A-Za-z0-9_\]])(@[A-Za-z0-9_-]+)`),
		func(m *fixtureMasker, match, rest string) bool {
			return match == standInMention || npmScope.MatchString(rest) ||
				slices.ContainsFunc(m.keep, func(k string) bool { return strings.EqualFold(strings.TrimPrefix(k, "@"), match[1:]) })
		}},
}

// npmScope is what follows the scope of an npm package name, "@scope/name":
// a package, no login.
var npmScope = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9._-]*`)

// publicCodeRepository is the code repository's own path on GitHub, owner
// and name, public by design: a URL or an import path to it stays. It is
// read from this package's import path, the module's.
var publicCodeRepository = strings.TrimPrefix(strings.Join(strings.SplitN(reflect.TypeFor[Fixture]().PkgPath(), "/", 4)[:3], "/"), "github.com/")

const (
	hostSuffixes = `(?:svc|internal|local|lan|home(?:\.|\[\.\])arpa|cluster(?:\.|\[\.\])local)`
	ipv4Pattern  = `(?:\d{1,3}(?:\.|\[\.\])){3}\d{1,3}`
	pathPart     = `(?:/[^\s"'<>)\];,]*|[?#][^\s"'<>)\];,]+)` // a path, or a query or fragment with no path
	pathTail     = pathPart + `?`
	urlTail      = `(?::\d+)?` + pathTail
	schemePart   = `\b[a-z][a-z0-9+.-]*://(?:[^\s/@]*@)?`
)

// The shapes the mask replaces whole, in the order it applies them.
var (
	// publicRepo ends where the repository's name does: its last group is
	// the character after it, given back.
	publicRepo = regexp.MustCompile(`(?i)(?:\b(?:https?|hxxps?)://)?github(?:\.|\[\.\])com/` + regexp.QuoteMeta(publicCodeRepository) + `(?:\.git)?` + pathTail + `(\.?(?:[^\w.-]|$))`)
	botEmails  = []maskTerm{
		{re: regexp.MustCompile(`(?i)factory-agent@factory(?:\.|\[\.\])local`), with: "[factory agent]"},
		{re: regexp.MustCompile(`(?i)factory-wip@factory(?:\.|\[\.\])local`), with: "[factory wip]"},
		{re: regexp.MustCompile(`(?i)noreply@anthropic(?:\.|\[\.\])com`), with: "[noreply]"},
	}
	emailShape  = regexp.MustCompile(`([A-Za-z0-9._%+-]+)@([A-Za-z0-9-]+(?:(?:\.|\[\.\])[A-Za-z0-9-]+)+)`)
	githubEmail = regexp.MustCompile(`(?i)^(?:users\.)?noreply(?:\.|\[\.\])github(?:\.|\[\.\])com$`)
	githubScp   = maskTerm{re: regexp.MustCompile(`(?i)\b[\w.-]+@github(?:\.|\[\.\])com:[^\s"'<>)\];,]+`), with: "[git remote]"}
	ipURL       = maskTerm{re: regexp.MustCompile(`(?i)` + schemePart + `(?:` + ipv4Pattern + `|\[[0-9a-f:.]+\])` + urlTail), with: "host.example"}
	insideURL   = maskTerm{re: regexp.MustCompile(`(?i)` + schemePart + `(?:(?:[a-z0-9-]+(?:\.|\[\.\]))+` + hostSuffixes + `|localhost)\b` + urlTail), with: "host.example"}
	userAtIP    = maskTerm{re: regexp.MustCompile(`\b[\w.-]+@(` + ipv4Pattern + `|\[?[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*\]?)`), with: "user@$1"}
	insideHost  = maskTerm{re: regexp.MustCompile(`(?i)\b(?:(?:[a-z0-9-]+(?:\.|\[\.\]))+` + hostSuffixes + `|localhost)\b(?::\d+)?`), with: "host.example"}
	urlPath     = maskTerm{re: regexp.MustCompile(`(?i)(` + schemePart + `(?:[^\s/?#"'<>)\]\[]|\[\.\])+)` + pathPart), with: "${1}/[path]"}
	// ipv6Run is a run of hex digits, colons and dots after the start, a
	// non-word character or a bracket that opens no index, with its zone
	// and the word characters that follow it.
	ipv6Run = regexp.MustCompile(`(^\[?|[^\w\[]\[?)([0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*)(%[\w.-]+)?(\w*)`)
	ipv6    = maskTerm{re: ipv6Run, fn: maskIPv6}
	ipv4    = maskTerm{re: regexp.MustCompile(`\b` + ipv4Pattern + `\b`), with: "192.0.2.1"}
)

// maskTerm is a pattern and what stands for it: with, or fn's answer to
// the match's groups.
type maskTerm struct {
	re   *regexp.Regexp
	with string
	fn   func(groups []string) string
}

func (t maskTerm) apply(s string) string {
	if t.fn != nil {
		return replaceGroups(t.re, s, t.fn)
	}
	return t.re.ReplaceAllString(s, t.with)
}

// replaceGroups is s with each match of re replaced by fn's answer to the
// match's groups, as they matched in s: matched again alone, an anchor or a
// boundary could read the match otherwise.
func replaceGroups(re *regexp.Regexp, s string, fn func(groups []string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		groups := make([]string, len(loc)/2)
		for i := range groups {
			if loc[2*i] >= 0 {
				groups[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(fn(groups))
		last = loc[1]
	}
	return b.String() + s[last:]
}

// maskIPv6 is an IPv6 address standing alone, with its zone, as a
// placeholder. Any other run stays: one inside a word or an index, one with
// no digit or one group, or one net.ParseIP refuses (a time, a scope
// operator, a slice).
func maskIPv6(p []string) string {
	run := p[2]
	groups := strings.FieldsFunc(run, func(r rune) bool { return r == ':' })
	if p[4] != "" || !strings.ContainsAny(run, "0123456789") || strings.Count(run, ":") < 2 || len(groups) < 2 {
		return p[0]
	}
	for _, c := range []string{run, strings.TrimRight(run, ".")} {
		if net.ParseIP(c) != nil {
			return p[1] + "2001:db8::1" + run[len(c):]
		}
	}
	return p[0]
}

// hostPattern is host as a pattern, in its defanged form too.
func hostPattern(host string) string {
	return strings.ReplaceAll(regexp.QuoteMeta(host), `\.`, `(?:\.|\[\.\])`)
}

// escapedHostPattern is host as a regular expression writes it, each
// separator behind a backslash (forgejo\-http\.forgejo), the backslash
// optional so a half-escaped form matches too.
func escapedHostPattern(host string) string {
	var b strings.Builder
	for _, r := range host {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteString(`\\*` + regexp.QuoteMeta(string(r)))
	}
	return b.String()
}

// termRegexp matches term as a whole word, whatever case, a host in its
// defanged form too.
func termRegexp(term string) *regexp.Regexp {
	q := hostPattern(term)
	if r, _ := utf8.DecodeRuneInString(term); r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
		q = `\b` + q
	}
	if r, _ := utf8.DecodeLastRuneInString(term); r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
		q += `\b`
	}
	return regexp.MustCompile(`(?i)` + q)
}

// fixtureMasker masks one fixture's strings.
type fixtureMasker struct {
	before     []maskTerm // the shapes that carry an @ and are not an email
	steps      []maskTerm
	logins     []string
	configured map[string]bool
	keep       []string
}

// newMasker is the mask of the export: the repositories the factory serves
// (the card's own is owner/repo, the policy's other projects and the
// config repository owner/repo-N), the configured hosts with their URLs,
// the logins the card's records name, the export's Mask terms (a dotted
// one a host too), and the shapes of this file.
func (r *fixtureRun) newMasker() *fixtureMasker {
	m := &fixtureMasker{configured: map[string]bool{}, keep: r.Keep}
	logins := slices.Clone(r.Mask)
	for l := range r.logins {
		logins = append(logins, l)
	}
	slices.SortFunc(logins, func(a, b string) int { return len(b) - len(a) })
	m.logins = slices.Compact(logins)
	named := slices.Clone(r.Hosts)
	for _, t := range r.Mask {
		if strings.Contains(t, ".") {
			named = append(named, t)
		}
	}
	var hosts []string
	for _, h := range named {
		if h != "" && !m.configured[strings.ToLower(h)] {
			m.configured[strings.ToLower(h)] = true
			hosts = append(hosts, h)
		}
	}
	slices.SortFunc(hosts, func(a, b string) int { return len(b) - len(a) })
	var configured, bare []maskTerm
	for _, h := range hosts {
		hp := hostPattern(h)
		configured = append(configured,
			maskTerm{re: regexp.MustCompile(`(?i)\b[\w.-]+@` + hp + `:[^\s"'<>)\];,]+`), with: "[git remote]"},
			maskTerm{re: regexp.MustCompile(`(?i)` + schemePart + hp + `\b` + urlTail), with: "host.example"})
		bare = append(bare, maskTerm{re: regexp.MustCompile(`(?i)\b` + hp + `\b` + urlTail), with: "host.example"},
			maskTerm{re: regexp.MustCompile(`(?i)\b` + escapedHostPattern(h) + `\b(?:\\*:\d+)?`), with: "host.example"})
	}
	var repos []string
	for _, p := range r.Policy.Projects {
		repos = append(repos, p.Repository)
	}
	repos = append(repos, r.Repositories...)
	slices.Sort(repos)
	var repoTerms []maskTerm
	repoTerms = append(repoTerms, maskTerm{re: termRegexp(r.t.Repository), with: "owner/repo"})
	n := 0
	for _, repo := range slices.Compact(repos) {
		if repo == "" || repo == r.t.Repository {
			continue
		}
		n++
		repoTerms = append(repoTerms, maskTerm{re: termRegexp(repo), with: fmt.Sprintf("owner/repo-%d", n)})
	}
	var loginTerms []maskTerm
	for _, l := range m.logins {
		loginTerms = append(loginTerms, maskTerm{re: termRegexp(l), with: RoleHuman})
	}
	m.before = slices.Concat(botEmails, []maskTerm{githubScp}, configured, []maskTerm{ipURL, insideURL, userAtIP})
	m.steps = slices.Concat(bare, []maskTerm{urlPath, insideHost}, repoTerms, []maskTerm{ipv6, ipv4}, loginTerms)
	return m
}

// emails replaces an email of a listed login, of a configured host or of
// GitHub's noreply domain by the stand-in; any other stays for the
// leftover check.
func (m *fixtureMasker) emails(s string) string {
	return replaceGroups(emailShape, s, func(parts []string) string {
		local := strings.ToLower(parts[1])
		domain := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(parts[2], "[.]", "."), "[", ""))
		if githubEmail.MatchString(parts[2]) || m.configured[domain] || slices.ContainsFunc(m.logins, func(l string) bool { return strings.ToLower(l) == local }) {
			return standInEmail
		}
		return parts[0]
	})
}

// str is s as it may leave: NFKC (a full-width sign is the sign), the
// public code repository set aside, the URLs and remotes that carry an @,
// the emails, then the other steps, then the set-aside text back.
func (m *fixtureMasker) str(s string) string {
	s = norm.NFKC.String(s)
	var kept []string
	s = replaceGroups(publicRepo, s, func(p []string) string {
		kept = append(kept, strings.TrimSuffix(p[0], p[1]))
		return fmt.Sprintf("\uE000%d\uE001", len(kept)-1) + p[1]
	})
	for _, t := range m.before {
		s = t.apply(s)
	}
	s = m.emails(s)
	for _, t := range m.steps {
		s = t.apply(s)
	}
	for i, u := range kept {
		s = strings.Replace(s, fmt.Sprintf("\uE000%d\uE001", i), u, 1)
	}
	return s
}

// mask is f with the configured terms replaced in every text and key, and
// refused when a shape that names a person or an inside host remains. The
// refusal names the JSON path and the class, never the value.
func (r *fixtureRun) mask(f Fixture) (Fixture, error) {
	body, err := json.Marshal(f)
	if err != nil {
		return Fixture{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return Fixture{}, err
	}
	m := r.newMasker()
	str := m.str
	var walk func(v any, path string) (any, error)
	walk = func(v any, path string) (any, error) {
		switch v := v.(type) {
		case string:
			s := str(v)
			return s, m.shapeOf(s, path)
		case map[string]any:
			out := make(map[string]any, len(v))
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				x, nk := v[k], str(k)
				if err := m.shapeOf(nk, path+"{key}"); err != nil {
					return nil, err
				}
				for n := 2; ; n++ {
					if _, taken := out[nk]; !taken {
						break
					}
					nk = fmt.Sprintf("%s_%d", str(k), n)
				}
				y, err := walk(x, path+"."+nk)
				if err != nil {
					return nil, err
				}
				out[nk] = y
			}
			return out, nil
		case []any:
			out := make([]any, len(v))
			for i, x := range v {
				y, err := walk(x, fmt.Sprintf("%s[%d]", path, i))
				if err != nil {
					return nil, err
				}
				out[i] = y
			}
			return out, nil
		}
		return v, nil
	}
	masked, err := walk(tree, "$")
	if err != nil {
		return Fixture{}, err
	}
	if body, err = json.Marshal(masked); err != nil {
		return Fixture{}, err
	}
	var out Fixture
	err = json.Unmarshal(body, &out)
	return out, err
}

// shapeOf refuses s when a shape that names a person remains: an email or
// a mention the mask does not know or keep. The error names the class, the
// position and the action that clears it, never the text.
func (m *fixtureMasker) shapeOf(s, path string) error {
	for _, sh := range fixtureShapes {
		for _, loc := range sh.re.FindAllStringSubmatchIndex(s, -1) {
			start, end := loc[0], loc[1]
			if len(loc) > 2 && loc[2] >= 0 {
				start, end = loc[2], loc[3]
			}
			if !sh.exempt(m, s[start:end], s[end:]) {
				return fmt.Errorf("the fixture holds %s at %s, byte %d: %s", sh.class, path, start, sh.action)
			}
		}
	}
	return nil
}
