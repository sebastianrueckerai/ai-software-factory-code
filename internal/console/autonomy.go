package console

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The autonomy level's change (roadmap 239): the selector on the project
// screen and the chat's set_autonomy write the keys into the project's
// file in the configuration repository's main, one commit, and record the
// change as one event. The policy loader activates the commit on its next
// poll. write_doc commits the project's documents the same way, through
// the same per-repository lock.

// docLocks holds one commit lock per repository.
var docLocks = struct {
	sync.Mutex
	m map[string]*sync.Mutex
}{m: map[string]*sync.Mutex{}}

func docLock(repository string) *sync.Mutex {
	docLocks.Lock()
	defer docLocks.Unlock()
	if docLocks.m[repository] == nil {
		docLocks.m[repository] = &sync.Mutex{}
	}
	return docLocks.m[repository]
}

// autonomyRefusal is the change's refusal; "" for a sound one.
func autonomyRefusal(level string, backlogCap int) string {
	switch {
	case !slices.Contains(policy.AutonomyValues, level):
		return "autonomy must be manual, assisted or autonomous"
	case backlogCap < 0:
		return "backlog_cap must not be negative"
	}
	return ""
}

// doSetAutonomy writes project name's autonomy level and backlog cap into
// its project file in the configuration repository's main as one commit,
// as the factory's config token, and records the change as one event
// naming the member who made it.
func (c *Console) doSetAutonomy(ctx context.Context, name, level string, backlogCap int, a actor) error {
	if line := autonomyRefusal(level, backlogCap); line != "" {
		return refuse(http.StatusBadRequest, line)
	}
	from, repo := policy.AutonomyManual, ""
	if v, ok := c.inForce(ctx); ok {
		if p, ok := v.Projects[name]; ok {
			from, repo = p.AutonomyState(), p.Repository
		}
	}
	path := c.cfg.ConfigDir + "/" + policy.ProjectFile(name)
	infra := "/api/v1/repos/" + url.PathEscape(c.cfg.InfraRepo.Owner) + "/" + url.PathEscape(c.cfg.InfraRepo.Name)
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.api(ctx, c.cfg.InfraToken, http.MethodGet, infra+"/contents/"+path+"?ref=main", nil, http.StatusOK, &file); err != nil {
		return &forgejoRefusal{"read the project file", err}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.Content))
	if err != nil {
		return refuse(http.StatusInternalServerError, "cannot decode the project file")
	}
	out, err := editProjectFile(raw, level, backlogCap)
	if err != nil {
		return refuse(http.StatusConflict, err.Error())
	}
	l := docLock(c.cfg.InfraRepo.String())
	l.Lock()
	defer l.Unlock()
	message := fmt.Sprintf("Project %s: autonomy %s (console, %s)", name, level, a.s.user)
	if err := c.api(ctx, c.cfg.InfraToken, http.MethodPost, infra+"/contents", map[string]any{
		"branch": "main", "message": message,
		"files": []map[string]string{{"operation": "update", "path": path,
			"content": base64.StdEncoding.EncodeToString(out), "sha": file.SHA}}},
		http.StatusOK, nil); err != nil {
		return &forgejoRefusal{"commit the project file", err}
	}
	detail := map[string]any{"from": from, "to": level}
	if backlogCap > 0 && backlogCap != policy.DefaultBacklogCap {
		detail["backlog_cap"] = backlogCap
	}
	c.event(context.WithoutCancel(ctx), factoryd.Event{Repository: repo, Kind: factoryd.EventAutonomyChanged,
		Actor: a.s.user, Via: cmp.Or(a.via, factoryd.ViaChat), Detail: detail})
	log.Printf("console: %s set %s to %s via %s", a.s.user, name, level, a.via)
	return nil
}

// autonomyButton answers POST /projects/{name}/autonomy, the project
// screen's selector: the level and the backlog cap into the project file
// as one commit, then back to the project's status.
func (c *Console) autonomyButton(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	level := r.PostFormValue("autonomy")
	cap := 0
	if s := r.PostFormValue("backlog_cap"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			http.Error(w, "backlog_cap must be a positive count", http.StatusBadRequest)
			return
		}
		cap = n
	}
	if err := c.doSetAutonomy(r.Context(), name, level, cap, button(r)); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects/"+name+"/status", http.StatusSeeOther)
}

// editProjectFile is the project file with autonomy and backlog_cap set,
// its comments and key order kept: backlog_cap lands only away from the
// default, and an existing key at the default comes off.
func editProjectFile(raw []byte, level string, backlogCap int) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("the project file is no YAML: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the project file is no YAML map")
	}
	m := doc.Content[0]
	scalar := func(v, tag string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}
	}
	index := func(key string) int {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				return i
			}
		}
		return -1
	}
	if i := index("autonomy"); i >= 0 {
		m.Content[i+1].SetString(level)
	} else {
		m.Content = append(m.Content, scalar("autonomy", "!!str"), scalar(level, "!!str"))
	}
	switch {
	case backlogCap > 0 && backlogCap != policy.DefaultBacklogCap:
		if i := index("backlog_cap"); i >= 0 {
			m.Content[i+1].SetString(strconv.Itoa(backlogCap))
		} else {
			m.Content = append(m.Content, scalar("backlog_cap", "!!str"), scalar(strconv.Itoa(backlogCap), "!!int"))
		}
	default:
		if i := index("backlog_cap"); i >= 0 {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// docMax bounds one document's content.
const docMax = 512 << 10

// docShown bounds the current content a stale compare-and-swap returns.
const docShown = 20 << 10

// docRootOK is the path allow-list: docs/roadmap.md, a file under
// docs/plans/ or docs/board/, a new docs/<name>.md; no escapes.
func docRootOK(path string) bool {
	if !strings.HasPrefix(path, "docs/") || !strings.HasSuffix(path, ".md") {
		return false
	}
	if strings.Contains(path, "..") || strings.Contains(path, "//") {
		return false
	}
	rest := strings.TrimPrefix(path, "docs/")
	return rest == "roadmap.md" || strings.HasPrefix(rest, "plans/") || strings.HasPrefix(rest, "board/") ||
		rest != "" && !strings.Contains(rest, "/")
}

// roadmapItemRef matches a roadmap item's opening marker.
var roadmapItemRef = regexp.MustCompile(`^-\s+\*\*(\d+)\.`)

// roadmapFindings are the structure check's findings on a roadmap edit:
// item numbers unique, every item keeping its Sources and its
// Where-it-stands line.
func roadmapFindings(content string) []string {
	var out []string
	seen := map[int]bool{}
	number, sources, stands := -1, false, false
	check := func() {
		if number >= 0 {
			if !sources {
				out = append(out, fmt.Sprintf("item %d lost its Sources line", number))
			}
			if !stands {
				out = append(out, fmt.Sprintf("item %d lost its Where-it-stands line", number))
			}
		}
	}
	for l := range strings.Lines(content) {
		if m := roadmapItemRef.FindStringSubmatch(l); m != nil {
			check()
			n, _ := strconv.Atoi(m[1])
			if seen[n] {
				out = append(out, fmt.Sprintf("item %d appears twice", n))
			}
			seen[n] = true
			number = n
			sources = strings.Contains(l, "Sources:")
			stands = strings.Contains(l, "Where it stands:")
			continue
		}
		if number >= 0 {
			sources = sources || strings.Contains(l, "Sources:")
			stands = stands || strings.Contains(l, "Where it stands:")
		}
	}
	check()
	if len(seen) == 0 {
		out = append(out, "the roadmap holds no numbered items")
	}
	return out
}

// doWriteDoc commits content to path under docs/ of the project's
// document repository, straight to master, as the docs writer (the
// 2026-10-13 plan's one door, roadmap 239's additions): the allow-list,
// the size cap, the roadmap's structure check, the repository's one
// commit at a time, and the content-hash compare-and-swap, whose stale
// refusal carries the current content.
func (c *Console) doWriteDoc(ctx context.Context, a actor, docRepo, path, content, baseHash string) (string, error) {
	switch {
	case docRepo == "":
		return "", refuse(http.StatusConflict, "the project names no document repository (chat.doc_repo); nothing to write")
	case c.cfg.DocsToken == "":
		return "", refuse(http.StatusConflict, "no docs writer token is seated (FORGEJO_DOCS_TOKEN); nothing to write")
	case !docRootOK(path):
		return "", refuse(http.StatusBadRequest, path+" is outside the document roots: docs/roadmap.md, docs/plans/, docs/board/ or docs/<name>.md")
	case strings.TrimSpace(content) == "":
		return "", refuse(http.StatusBadRequest, "an empty document writes nothing; delete is not a chat's to do")
	case len(content) > docMax:
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("the document is over the cap of %d bytes", docMax))
	}
	if path == "docs/roadmap.md" {
		if f := roadmapFindings(content); len(f) > 0 {
			return "", refuse(http.StatusUnprocessableEntity, "The roadmap edit breaks the file's structure: "+strings.Join(f, "; ")+".")
		}
	}
	repo, err := factoryd.SplitRepo(docRepo)
	if err != nil {
		return "", refuse(http.StatusConflict, "the document repository is no owner/name")
	}
	l := docLock(docRepo)
	l.Lock()
	defer l.Unlock()
	base := "/api/v1/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	present := true
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodGet, base+"/contents/"+path+"?ref=master", nil, http.StatusOK, &file); err != nil {
		var api *apiError
		if !errors.As(err, &api) || api.status != http.StatusNotFound {
			return "", &forgejoRefusal{"read the document", err}
		}
		present = false
	}
	current := ""
	if present {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.Content))
		if err != nil {
			return "", refuse(http.StatusInternalServerError, "cannot decode the document")
		}
		current = string(raw)
		if baseHash == "" {
			return "", refuse(http.StatusConflict, docCurrent(path, current, "it exists; write against what is there"))
		}
		sum := sha256.Sum256([]byte(current))
		if baseHash != hex.EncodeToString(sum[:]) {
			return "", refuse(http.StatusConflict, docCurrent(path, current, "the file changed since you read it"))
		}
	} else if baseHash != "" {
		return "", refuse(http.StatusConflict, path+" does not exist; write it without base_sha256")
	}
	message := fmt.Sprintf("docs: %s (chat, %s)", path, a.s.user)
	change := map[string]string{"operation": "create", "path": path, "content": base64.StdEncoding.EncodeToString([]byte(content))}
	if present {
		change["operation"] = "update"
		change["sha"] = file.SHA
	}
	var committed struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodPost, base+"/contents", map[string]any{
		"branch": "master", "message": message, "files": []map[string]string{change}},
		http.StatusOK, &committed); err != nil {
		return "", &forgejoRefusal{"commit the document", err}
	}
	log.Printf("console: %s wrote %s on %s via %s", a.s.user, path, docRepo, a.via)
	return fmt.Sprintf("Committed %s to %s@master: %s.", cmp.Or(committed.Commit.SHA, "the change"), docRepo, path), nil
}

// docCurrent is the refusal a stale or missing compare-and-swap answers:
// the state and the file's current content, which the writer re-applies
// its edit on top of.
func docCurrent(path, current, why string) string {
	return fmt.Sprintf("%s: %s. Its current content:\n\n%s", path, why,
		textutil.Truncate(current, docShown))
}

// capNote is the backlog cap's note in a level change's answer.
func capNote(cap int) string {
	if cap > 0 && cap != policy.DefaultBacklogCap {
		return fmt.Sprintf(" with a backlog cap of %d", cap)
	}
	return ""
}
