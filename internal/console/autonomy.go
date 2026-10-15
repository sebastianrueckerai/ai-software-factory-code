package console

import (
	"cmp"
	"context"
	"encoding/base64"
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
	"time"

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
func autonomyRefusal(state string, backlogCap int) string {
	switch {
	case !slices.Contains(policy.AutonomyStates, state):
		return "autonomy must be manual, assisted, autonomous or full auto"
	case backlogCap < 0:
		return "backlog_cap must not be negative"
	}
	return ""
}

// doSetAutonomy writes project name's autonomy state and backlog cap, 0
// for the file's own, into its project file on the configuration
// repository's main as the member, an approver, through the path a
// project's filing takes: the file judged the loader's way, a branch with
// the one change, its pull request and its merge. Full auto writes
// autonomous with merge_rule auto, autonomous merge_rule human; manual and
// assisted leave the rule.
// The change records one event naming the member.
func (c *Console) doSetAutonomy(ctx context.Context, name, state string, backlogCap int, a actor) error {
	if line := autonomyRefusal(state, backlogCap); line != "" {
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
	out, err := editProjectFile(raw, state, backlogCap)
	if err != nil {
		return refuse(http.StatusConflict, err.Error())
	}
	if c.cfg.CheckProject != nil {
		refused, err := c.cfg.CheckProject(ctx, name, out)
		switch {
		case err != nil:
			log.Printf("console: check projects/%s.yaml against infra's head: %v", name, err)
			return refuse(http.StatusBadGateway, "cannot read the configuration at infra's head; see the factoryd log")
		case len(refused) > 0:
			return refuse(http.StatusUnprocessableEntity, "The project file would not load: "+strings.Join(refused, "; "))
		}
	}
	l := docLock(c.cfg.InfraRepo.String())
	l.Lock()
	defer l.Unlock()
	branch := fmt.Sprintf("autonomy-%s-%d", name, time.Now().Unix())
	title := fmt.Sprintf("Project %s: autonomy %s", name, state)
	if err := c.as(ctx, a.s, http.MethodPost, infra+"/contents", map[string]any{
		"branch": "main", "new_branch": branch, "message": a.attribution().Message(title),
		"files": []map[string]string{{"operation": "update", "path": path, "content": base64.StdEncoding.EncodeToString(out), "sha": file.SHA}}},
		http.StatusCreated, nil); err != nil {
		return &forgejoRefusal{"commit the project file", err}
	}
	var pull struct {
		Number int64 `json:"number"`
	}
	if err := c.as(ctx, a.s, http.MethodPost, infra+"/pulls", map[string]string{"head": branch, "base": "main", "title": title},
		http.StatusCreated, &pull); err != nil {
		return &forgejoRefusal{"open the pull request", err}
	}
	if err := c.mergeInfra(ctx, a.s, pull.Number); err != nil {
		return &forgejoRefusal{fmt.Sprintf("merge the pull request #%d", pull.Number), err}
	}
	detail := map[string]any{"from": from, "to": state, "pr": pull.Number}
	if backlogCap > 0 && backlogCap != policy.DefaultBacklogCap {
		detail["backlog_cap"] = backlogCap
	}
	c.event(context.WithoutCancel(ctx), factoryd.Event{Repository: repo, Kind: factoryd.EventAutonomyChanged,
		Actor: a.s.user, Via: cmp.Or(a.via, factoryd.ViaChat), Detail: detail})
	log.Printf("console: %s set %s to %s via %s (%s#%d)", a.s.user, name, state, a.via, c.cfg.InfraRepo, pull.Number)
	return nil
}

// autonomyButton answers POST /projects/{name}/autonomy, the chat's level
// control (C14): the state and the backlog cap into the project file, then
// 204.
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
	w.WriteHeader(http.StatusNoContent)
}

// editProjectFile is the project file at autonomy state with backlog_cap
// set, its comments and key order kept: full auto is autonomous with
// merge_rule auto, autonomous takes merge_rule human, manual and assisted
// keep the rule; a cap of 0 keeps the file's, another lands only away
// from the default, and an existing key set to the default comes off.
func editProjectFile(raw []byte, state string, backlogCap int) ([]byte, error) {
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
	set := func(key, v string) {
		if i := index(key); i >= 0 {
			m.Content[i+1].SetString(v)
		} else {
			m.Content = append(m.Content, scalar(key, "!!str"), scalar(v, "!!str"))
		}
	}
	switch state {
	case policy.AutonomyFullAuto:
		set("autonomy", policy.AutonomyAutonomous)
		set("merge_rule", policy.MergeAuto)
	case policy.AutonomyAutonomous:
		set("autonomy", state)
		set("merge_rule", policy.MergeHuman)
	default:
		set("autonomy", state)
	}
	switch {
	case backlogCap == 0:
	case backlogCap != policy.DefaultBacklogCap:
		if i := index("backlog_cap"); i >= 0 {
			m.Content[i+1].Value, m.Content[i+1].Tag, m.Content[i+1].Style = strconv.Itoa(backlogCap), "!!int", 0
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

// docFinds bounds the lines a find answers.
const docFinds = 12

// docRepoOf checks a document tool's repository, token and path, and
// answers the repository.
func (c *Console) docRepoOf(docRepo, path string) (factoryd.Repo, error) {
	switch {
	case docRepo == "":
		return factoryd.Repo{}, refuse(http.StatusConflict, "the project names no document repository (chat.doc_repo); there are no documents")
	case c.cfg.DocsToken == "":
		return factoryd.Repo{}, refuse(http.StatusConflict, "no docs writer token is seated (FORGEJO_DOCS_TOKEN); the documents cannot be reached")
	case !docRootOK(path):
		return factoryd.Repo{}, refuse(http.StatusBadRequest, path+" is outside the document roots: docs/roadmap.md, docs/plans/, docs/board/ or docs/<name>.md")
	}
	repo, err := factoryd.SplitRepo(docRepo)
	if err != nil {
		return factoryd.Repo{}, refuse(http.StatusConflict, "the document repository is no owner/name")
	}
	return repo, nil
}

// docAPI is repo's API path.
func docAPI(repo factoryd.Repo) string {
	return "/api/v1/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

// docFile is path's content and blob sha on repo's master; present false
// when there is no such file.
func (c *Console) docFile(ctx context.Context, repo factoryd.Repo, path string) (content, blob string, present bool, err error) {
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodGet, docAPI(repo)+"/contents/"+path+"?ref=master", nil, http.StatusOK, &file); err != nil {
		var api *apiError
		if errors.As(err, &api) && api.status == http.StatusNotFound {
			return "", "", false, nil
		}
		return "", "", false, &forgejoRefusal{"read the document", err}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.Content))
	if err != nil {
		return "", "", false, refuse(http.StatusInternalServerError, "cannot decode the document")
	}
	return string(raw), file.SHA, true, nil
}

// readDoc is read_doc: path at master with its base_sha256, its first
// docShown bytes or the lines holding find.
func (c *Console) readDoc(ctx context.Context, docRepo, path, find string) (string, error) {
	repo, err := c.docRepoOf(docRepo, path)
	if err != nil {
		return "", err
	}
	content, _, present, err := c.docFile(ctx, repo, path)
	switch {
	case err != nil:
		return "", err
	case !present:
		return path + " does not exist on master: write_doc with content makes it.", nil
	}
	head := fmt.Sprintf("%s on %s@master: %d bytes, base_sha256 %s.", path, docRepo, len(content), factoryd.ContentHash(content))
	if find == "" {
		if len(content) > docShown {
			return head + " Its first 20 KiB; find narrows to the lines holding a word.\n\n" + textutil.Truncate(content, docShown), nil
		}
		return head + "\n\n" + content, nil
	}
	var b strings.Builder
	n, size := 0, 0
	for i, l := range strings.Split(content, "\n") {
		if !strings.Contains(strings.ToLower(l), strings.ToLower(find)) {
			continue
		}
		if n == docFinds || size+len(l) > docShown {
			fmt.Fprintf(&b, "… more lines hold it: narrow find.\n")
			break
		}
		n, size = n+1, size+len(l)
		fmt.Fprintf(&b, "line %d: %s\n", i+1, l)
	}
	if n == 0 {
		return head + fmt.Sprintf(" No line holds %q.", find), nil
	}
	return head + "\n\n" + b.String(), nil
}

// docEdit is write_doc's change: Old replaced by New in an existing file,
// or Content for a new one, against the file read with hash Base.
type docEdit struct{ Old, New, Content, Base string }

// doWriteDoc commits edit to path under docs/ of the project's document
// repository, straight to master, as the docs writer (the 2026-10-13
// plan's one door, roadmap 239's additions): the allow-list, the size cap,
// the roadmap's structure check, the repository's one commit at a time,
// and the content-hash compare-and-swap, whose stale refusal names the
// file's current hash and whether the edit's passage is still there.
func (c *Console) doWriteDoc(ctx context.Context, a actor, docRepo, path string, edit docEdit) (string, error) {
	repo, err := c.docRepoOf(docRepo, path)
	if err != nil {
		return "", err
	}
	switch {
	case edit.Content != "" && edit.Old != "":
		return "", refuse(http.StatusBadRequest, "content writes a new file and old and new edit an existing one: send one of the two")
	case edit.Content == "" && edit.Old == "":
		return "", refuse(http.StatusBadRequest, "an edit names the passage old and its replacement new; a new file takes content")
	}
	l := docLock(docRepo)
	l.Lock()
	defer l.Unlock()
	current, blob, present, err := c.docFile(ctx, repo, path)
	if err != nil {
		return "", err
	}
	next := edit.Content
	hash := factoryd.ContentHash(current)
	switch {
	case present && edit.Content != "":
		return "", refuse(http.StatusConflict, fmt.Sprintf("%s exists: edit a passage of it with old and new, base_sha256 %s", path, hash))
	case present && edit.Base != hash && strings.Count(current, edit.Old) == 1:
		return "", refuse(http.StatusConflict, fmt.Sprintf("%s changed since it was read: its base_sha256 is now %s, and the passage old is still there once, so the same edit with that base_sha256 applies", path, hash))
	case present && edit.Base != hash:
		return "", refuse(http.StatusConflict, docCurrent(path, current, "it changed since it was read and the passage old is no longer there once"))
	case present && strings.Count(current, edit.Old) == 0:
		return "", refuse(http.StatusUnprocessableEntity, "the passage old is not in "+path+": read_doc it with find and copy the passage exactly")
	case present && strings.Count(current, edit.Old) > 1:
		return "", refuse(http.StatusUnprocessableEntity, fmt.Sprintf("the passage old is in %s %d times: widen it until it is there once", path, strings.Count(current, edit.Old)))
	case present:
		next = strings.Replace(current, edit.Old, edit.New, 1)
	case edit.Old != "":
		return "", refuse(http.StatusNotFound, path+" does not exist on master: a new file takes content")
	case edit.Base != "":
		return "", refuse(http.StatusConflict, path+" does not exist on master: write it without base_sha256")
	}
	switch {
	case strings.TrimSpace(next) == "":
		return "", refuse(http.StatusBadRequest, "the document would be empty; delete is not a chat's to do")
	case len(next) > docMax:
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("the document would be %d bytes, over the cap of %d", len(next), docMax))
	}
	if path == "docs/roadmap.md" {
		if f := roadmapFindings(next); len(f) > 0 {
			return "", refuse(http.StatusUnprocessableEntity, "The roadmap edit breaks the file's structure: "+strings.Join(f, "; ")+".")
		}
	}
	change := map[string]string{"operation": "create", "path": path, "content": base64.StdEncoding.EncodeToString([]byte(next))}
	if present {
		change["operation"], change["sha"] = "update", blob
	}
	var committed struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodPost, docAPI(repo)+"/contents", map[string]any{
		"branch": "master", "message": a.attribution().Message("docs: " + path), "files": []map[string]string{change}},
		http.StatusCreated, &committed); err != nil {
		return "", &forgejoRefusal{"commit the document", err}
	}
	log.Printf("console: %s wrote %s on %s via %s", a.s.user, path, docRepo, a.via)
	return fmt.Sprintf("Committed %s to %s@master: %s, now base_sha256 %s.", cmp.Or(committed.Commit.SHA, "the change"), docRepo, path,
		factoryd.ContentHash(next)), nil
}

// docCurrent is the refusal a stale compare-and-swap answers when the
// edit's passage is gone: the state, the file's current hash and content,
// which the writer re-applies its edit on top of.
func docCurrent(path, current, why string) string {
	return fmt.Sprintf("%s: %s. Its base_sha256 is now %s; its current content:\n\n%s", path, why,
		factoryd.ContentHash(current), textutil.Truncate(current, docShown))
}

// capNote is the backlog cap's note in a level change's answer.
func capNote(cap int) string {
	if cap > 0 && cap != policy.DefaultBacklogCap {
		return fmt.Sprintf(" with a backlog cap of %d", cap)
	}
	return ""
}
