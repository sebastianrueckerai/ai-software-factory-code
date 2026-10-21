package console

// The document read (roadmap 277, the 2026-10-15 plan, item 1): the
// drawer's document mode reads one document of the project's document
// repository through the same door the chat's read_doc takes — the
// repository from the project's chat block, the path check, the docs
// writer token. Whoever sees the project's board reads it, as its
// event stream's rule does: the project's writers, the factory's
// writers, Forgejo's readers of its repository.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// docRead answers GET /projects/{name}/docs/{path...}: the document
// rendered with its content hash, its head commit and that commit's time;
// ?raw=1 answers the text byte for byte, ?peek=1 its first paragraph as
// plain text.
func (c *Console) docRead(w http.ResponseWriter, r *http.Request) {
	p, ok := c.pathProject(w, r)
	if !ok {
		return
	}
	sees, _, err := c.follows(r, p)
	switch {
	case err != nil:
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return
	case !sees:
		http.NotFound(w, r)
		return
	}
	docPath := r.PathValue("path")
	v, _ := c.inForce(r.Context())
	repo, err := c.docRepoOf(v.Document.ChatOf(v.Projects[p.Name]).DocRepo, docPath)
	if err != nil {
		c.fail(w, r, err)
		return
	}
	text, _, present, err := c.docFile(r.Context(), repo, docPath)
	switch {
	case err != nil:
		c.fail(w, r, err)
		return
	case !present:
		http.Error(w, docPath+" does not exist on master.", http.StatusNotFound)
		return
	}
	if !r.URL.Query().Has("raw") && !r.URL.Query().Has("peek") {
		c.docRendered(w, r, repo, docPath, text)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Has("peek") {
		text = docPeek(text)
	}
	if _, err := w.Write([]byte(text)); err != nil { // #nosec G104 -- best-effort response write
		log.Printf("console: the document %s: %v", docPath, err)
	}
}

// docAnswer is the rendered document with its facts: its path, its title
// (the first heading's plain text, else its file name), its HTML, the
// content hash the door's compare-and-swap names, the head commit and
// its time, and Forgejo's page of the file.
type docAnswer struct {
	Path    string    `json:"path"`
	Title   string    `json:"title"`
	HTML    string    `json:"html"`
	SHA256  string    `json:"sha256"`
	Commit  string    `json:"commit"`
	At      time.Time `json:"at"`
	Forgejo string    `json:"forgejo"`
}

// docRendered answers one document rendered with its facts.
func (c *Console) docRendered(w http.ResponseWriter, r *http.Request, repo factoryd.Repo, docPath, text string) {
	commit, at, err := c.docCommit(r.Context(), repo, docPath)
	if err != nil {
		c.fail(w, r, err)
		return
	}
	html, err := docRender(text, docPath)
	if err != nil {
		log.Printf("console: render %s: %v", docPath, err)
		http.Error(w, "cannot render the document", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(docAnswer{Path: docPath, Title: docTitle(text, docPath), HTML: html,
		SHA256: factoryd.ContentHash(text), Commit: commit, At: at,
		Forgejo: fmt.Sprintf("%s/%s/src/branch/master/%s", strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/"), repo, docPath)}); err != nil { // #nosec G104 -- best-effort
		log.Printf("console: the document %s: %v", docPath, err)
	}
}

// docCommit is path's head commit on master and that commit's time.
func (c *Console) docCommit(ctx context.Context, repo factoryd.Repo, docPath string) (string, time.Time, error) {
	var rows []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodGet,
		docAPI(repo)+"/commits?sha=master&path="+url.QueryEscape(docPath)+"&limit=1", nil, http.StatusOK, &rows); err != nil {
		return "", time.Time{}, &forgejoRefusal{"read the document's commit", err}
	}
	if len(rows) == 0 {
		return "", time.Time{}, nil
	}
	return rows[0].SHA, rows[0].Commit.Committer.Date, nil
}

// docAuthor is path's head commit's author and that commit's time: who
// changed the document last and when, the stale save's merge and clash
// facts (the 2026-10-15 plan, item 5). The author is the commit's Forgejo
// account, else the name git recorded.
func (c *Console) docAuthor(ctx context.Context, repo factoryd.Repo, docPath string) (string, time.Time, error) {
	var rows []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodGet,
		docAPI(repo)+"/commits?sha=master&path="+url.QueryEscape(docPath)+"&limit=1", nil, http.StatusOK, &rows); err != nil {
		return "", time.Time{}, &forgejoRefusal{"read the document's commit", err}
	}
	if len(rows) == 0 {
		return "", time.Time{}, nil
	}
	return cmp.Or(rows[0].Author.Login, rows[0].Commit.Author.Name), rows[0].Commit.Committer.Date, nil
}

// The peek's and the title's shapes, the engine's rules (docs.js: plain,
// firstParagraph, headTitle).
var (
	docLinkText = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	docCode     = regexp.MustCompile("`([^`]*)`")
	docBold     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	docEm       = regexp.MustCompile(`\*([^*]+)\*`)
	docMarks    = regexp.MustCompile("[*`\\[\\]]")
	docHeading  = regexp.MustCompile(`(?m)^#{1,4} +(.+)$`)
	docHeadLine = regexp.MustCompile(`^#{1,4} `)
	docUL       = regexp.MustCompile(`^\s*[-*] +`)
	docOL       = regexp.MustCompile(`^\s*\d+\. +`)
	docHR       = regexp.MustCompile(`^ {0,3}[-*_]( *[-*_]){2,} *$`) // a rule, with no backreferences to say ( \1)
)

// docPlain is s with its inline Markdown taken away.
func docPlain(s string) string {
	for _, re := range []*regexp.Regexp{docLinkText, docCode, docBold, docEm} {
		s = re.ReplaceAllString(s, "$1")
	}
	return docMarks.ReplaceAllString(s, "")
}

// docTitle is md's first heading as plain text, else the file's name
// without .md.
func docTitle(md, at string) string {
	if m := docHeading.FindStringSubmatch(md); m != nil {
		if t := strings.TrimSpace(docPlain(m[1])); t != "" {
			return t
		}
	}
	return strings.TrimSuffix(path.Base(at), ".md")
}

// startsDocBlock is the engine's block start: a code fence, a heading, a
// rule, a quote or a list line.
func startsDocBlock(l string) bool {
	return strings.HasPrefix(l, "```") || docHeadLine.MatchString(l) || docHR.MatchString(l) ||
		strings.HasPrefix(l, ">") || docUL.MatchString(l) || docOL.MatchString(l)
}

// docPeek is md's first paragraph as plain text, the hover peek's text:
// headings, lists, quotes and code are skipped until a paragraph, which is
// cut at 280 bytes with an ellipsis.
func docPeek(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	blank := func(l string) bool { return strings.TrimSpace(l) == "" }
	for i := 0; i < len(lines); {
		l := lines[i]
		if blank(l) || startsDocBlock(l) {
			if strings.HasPrefix(l, "```") {
				for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
				}
			}
			i++
			continue
		}
		var p []string
		for i < len(lines) && !blank(lines[i]) && !startsDocBlock(lines[i]) {
			p = append(p, strings.TrimSpace(lines[i]))
			i++
		}
		return docCut(docPlain(strings.Join(p, " ")))
	}
	return ""
}

// docCut is s at the engine's 280 bytes, the cut moved back to a whole
// rune and trailing spaces dropped before the ellipsis.
func docCut(s string) string {
	if len(s) <= 280 {
		return s
	}
	end := 280
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return strings.TrimRight(s[:end], " \t") + "…"
}
