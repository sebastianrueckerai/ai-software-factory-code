package console

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"maps"
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
// for the file's own, into its project file as the member, an approver,
// through commitProjectFile. Full auto writes autonomous with merge_rule
// auto, autonomous merge_rule human; manual and assisted leave the rule.
// The change records one event naming the member.
func (c *Console) doSetAutonomy(ctx context.Context, name, state string, backlogCap int, a actor) error {
	if line := autonomyRefusal(state, backlogCap); line != "" {
		return refuse(http.StatusBadRequest, line)
	}
	start := time.Now()
	from, repo := policy.AutonomyManual, ""
	if v, ok := c.inForce(ctx); ok {
		if p, ok := v.Projects[name]; ok {
			from, repo = p.AutonomyState(), p.Repository
		}
	}
	pr, err := c.commitProjectFile(ctx, a, name, fmt.Sprintf("Project %s: autonomy %s", name, state), autonomyEdit(state, backlogCap), nil)
	if err != nil {
		return err
	}
	detail := map[string]any{"from": from, "to": state, "pr": pr}
	if backlogCap > 0 && backlogCap != policy.DefaultBacklogCap {
		detail["backlog_cap"] = backlogCap
	}
	c.event(context.WithoutCancel(ctx), factoryd.Event{Repository: repo, Kind: factoryd.EventAutonomyChanged,
		Actor: a.s.user, Via: cmp.Or(a.via, factoryd.ViaChat), Detail: detail})
	log.Printf("console: %s set %s to %s via %s (%s#%d) in %.1fs", a.s.user, name, state, a.via, c.cfg.InfraRepo, pr, time.Since(start).Seconds())
	return nil
}

// commitProjectFile changes project name's file on the configuration
// repository's main as member a, through the path a project's filing
// takes, and answers the pull request's number. Under the repository's
// commit lock it reads the file with the config token, applies edit to
// its YAML with every other line and comment kept, and judges the tree
// with the change and extra's files the loader's way. extra maps a path
// under the configuration directory to its text, "" deleting the file. A
// change that leaves every file as it is refuses.
// One commit with message lands on a new branch, its pull request opens
// with message as its title, and the member merges it.
func (c *Console) commitProjectFile(ctx context.Context, a actor, name, message string, edit func(doc *yaml.Node) error, extra map[string]string) (int64, error) {
	l := docLock(c.cfg.InfraRepo.String())
	l.Lock()
	defer l.Unlock()
	path := policy.ProjectFile(name)
	raw, blob, present, err := c.infraFile(ctx, path)
	switch {
	case err != nil:
		return 0, err
	case !present:
		return 0, refuse(http.StatusNotFound, "There is no file "+path+" on infra's main.")
	}
	out, err := editProjectFile(raw, edit)
	if err != nil {
		return 0, refuse(http.StatusConflict, err.Error())
	}
	changes := map[string][]byte{path: out}
	files := []map[string]string{{"operation": "update", "path": c.cfg.ConfigDir + "/" + path, "content": base64.StdEncoding.EncodeToString(out), "sha": blob}}
	for _, p := range slices.Sorted(maps.Keys(extra)) {
		old, blob, present, err := c.infraFile(ctx, p)
		if err != nil {
			return 0, err
		}
		text := extra[p]
		change := map[string]string{"path": c.cfg.ConfigDir + "/" + p}
		switch {
		case text == "" && !present, text != "" && present && string(old) == text:
			continue
		case text == "":
			change["operation"], change["sha"], changes[p] = "delete", blob, nil
		case present:
			change["operation"], change["sha"], change["content"], changes[p] = "update", blob, base64.StdEncoding.EncodeToString([]byte(text)), []byte(text)
		default:
			change["operation"], change["content"], changes[p] = "create", base64.StdEncoding.EncodeToString([]byte(text)), []byte(text)
		}
		files = append(files, change)
	}
	if len(files) == 1 && bytes.Equal(out, raw) {
		return 0, refuse(http.StatusConflict, "Nothing changes: "+path+" already holds this.")
	}
	if c.cfg.CheckChange != nil {
		refused, err := c.cfg.CheckChange(ctx, changes)
		switch {
		case err != nil:
			log.Printf("console: check %s against infra's head: %v", path, err)
			return 0, refuse(http.StatusBadGateway, "cannot read the configuration at infra's head; see the factoryd log")
		case len(refused) > 0:
			return 0, refuse(http.StatusUnprocessableEntity, "The project file would not load: "+strings.Join(refused, "; "))
		}
	}
	infra := c.infraAPI()
	branch := fmt.Sprintf("config-%s-%d", name, time.Now().UnixNano())
	if err := c.as(ctx, a.s, http.MethodPost, infra+"/contents", map[string]any{
		"branch": "main", "new_branch": branch, "message": a.attribution().Message(message), "files": files},
		http.StatusCreated, nil); err != nil {
		return 0, &forgejoRefusal{"commit the project file", err}
	}
	var pull struct {
		Number int64 `json:"number"`
	}
	if err := c.as(ctx, a.s, http.MethodPost, infra+"/pulls", map[string]string{"head": branch, "base": "main", "title": message},
		http.StatusCreated, &pull); err != nil {
		return 0, &forgejoRefusal{"open the pull request", err}
	}
	if err := c.mergeInfra(ctx, a.s, pull.Number); err != nil {
		return 0, &forgejoRefusal{fmt.Sprintf("merge the pull request #%d", pull.Number), err}
	}
	if c.cfg.PolicyReload != nil {
		if err := c.cfg.PolicyReload(ctx); err != nil {
			log.Printf("console: the policy after %s#%d: %v", c.cfg.InfraRepo, pull.Number, err)
		}
	}
	return pull.Number, nil
}

// infraFile is path, under the configuration directory, on infra's main
// with its blob sha, read with the config token; present false when there
// is no such file.
func (c *Console) infraFile(ctx context.Context, path string) (content []byte, blob string, present bool, err error) {
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.api(ctx, c.cfg.InfraToken, http.MethodGet, c.infraAPI()+"/contents/"+c.cfg.ConfigDir+"/"+path+"?ref=main", nil, http.StatusOK, &file); err != nil {
		if apiStatus(err) == http.StatusNotFound {
			return nil, "", false, nil
		}
		return nil, "", false, &forgejoRefusal{"read " + path, err}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.Content))
	if err != nil {
		return nil, "", false, refuse(http.StatusInternalServerError, "cannot decode "+path)
	}
	return raw, file.SHA, true, nil
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

// intakeButton answers POST /projects/{name}/intake, the chat header's
// intake switch: closed refuses the wake's own filings — the turns the
// Waker starts without the member's ask — and open takes them again; the
// member's own ask always files and starts. It answers 204.
func (c *Console) intakeButton(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	closed := r.PostFormValue("intake") == "closed"
	p, ok, err := c.projectOf(r.Context(), name)
	if err != nil || !ok {
		http.Error(w, "there is no project "+name, http.StatusNotFound)
		return
	}
	b := button(r)
	if err := c.cfg.Chat.SetChatIntake(r.Context(), p.Repository, closed, b.s.user, time.Now()); err != nil {
		log.Printf("console: the intake of %s: %v", name, err)
		http.Error(w, "cannot set the intake of "+name, http.StatusInternalServerError)
		return
	}
	from, to := "open", "closed"
	if !closed {
		from, to = "closed", "open"
	}
	c.event(context.WithoutCancel(r.Context()), factoryd.Event{Repository: p.Repository, Kind: factoryd.EventIntakeChanged,
		Actor: b.s.user, Via: b.via, Detail: map[string]any{"from": from, "to": to}})
	log.Printf("console: %s set the intake of %s %s", b.s.user, name, to)
	w.WriteHeader(http.StatusNoContent)
}

// editProjectFile is the project file raw with edit applied to its
// YAML, written back with two-space indents, so every line edit leaves
// alone keeps its comments, order and style.
func editProjectFile(raw []byte, edit func(doc *yaml.Node) error) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("the project file is no YAML: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the project file is no YAML map")
	}
	if err := edit(&doc); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(&doc); err != nil {
		return nil, err
	}
	if err := e.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// keyIndex is the index of key in mapping m's content, -1 for none.
func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// autonomyEdit sets the project file to autonomy state with backlog_cap:
// full auto is autonomous with merge_rule auto, autonomous takes
// merge_rule human, manual and assisted keep the rule; a cap of 0 keeps
// the file's, another lands only away from the default, and an existing
// key set to the default comes off.
func autonomyEdit(state string, backlogCap int) func(doc *yaml.Node) error {
	return func(doc *yaml.Node) error {
		m := doc.Content[0]
		scalar := func(v, tag string) *yaml.Node {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}
		}
		set := func(key, v string) {
			if i := keyIndex(m, key); i >= 0 {
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
		switch i := keyIndex(m, "backlog_cap"); {
		case backlogCap == 0:
		case backlogCap != policy.DefaultBacklogCap && i >= 0:
			m.Content[i+1].Value, m.Content[i+1].Tag, m.Content[i+1].Style = strconv.Itoa(backlogCap), "!!int", 0
		case backlogCap != policy.DefaultBacklogCap:
			m.Content = append(m.Content, scalar("backlog_cap", "!!str"), scalar(strconv.Itoa(backlogCap), "!!int"))
		case i >= 0:
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
		}
		return nil
	}
}

// docMax bounds one document's content.
const docMax = 512 << 10

// docShown bounds the current content a stale compare-and-swap returns.
const docShown = 20 << 10

// docRootOK is the write door's allow-list, factoryd's own DocRootOK: the door
// and the webhook's rows share it.
func docRootOK(path string) bool { return factoryd.DocRootOK(path) }

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

// docRepoOf checks a document door's repository, token and path, and
// answers the repository: a read opens any file of the repository.
func (c *Console) docRepoOf(docRepo, path string) (factoryd.Repo, error) {
	switch {
	case docRepo == "":
		return factoryd.Repo{}, refuse(http.StatusConflict, "the project names no document repository (chat.doc_repo); there are no documents")
	case c.cfg.DocsToken == "":
		return factoryd.Repo{}, refuse(http.StatusConflict, "no docs writer token is seated (FORGEJO_DOCS_TOKEN); the documents cannot be reached")
	case !docPathOK(path):
		return factoryd.Repo{}, refuse(http.StatusBadRequest, path+" is no file path: name a path from the repository's root, with no . or .. segments")
	}
	repo, err := factoryd.SplitRepo(docRepo)
	if err != nil {
		return factoryd.Repo{}, refuse(http.StatusConflict, "the document repository is no owner/name")
	}
	return repo, nil
}

// docWriteRepoOf is docRepoOf for the write doors, which keep to the
// document roots.
func (c *Console) docWriteRepoOf(docRepo, path string) (factoryd.Repo, error) {
	if !docRootOK(path) {
		return factoryd.Repo{}, refuse(http.StatusBadRequest, path+" is outside the document roots: docs/roadmap.md, docs/plans/, docs/board/ or docs/<name>.md")
	}
	return c.docRepoOf(docRepo, path)
}

// docAPI is repo's API path.
func docAPI(repo factoryd.Repo) string {
	return "/api/v1/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

// docFile is path's content and blob sha on repo's master; present false
// when there is no such file.
func (c *Console) docFile(ctx context.Context, repo factoryd.Repo, path string) (content, blob string, present bool, err error) {
	return c.docFileAt(ctx, repo, path, "master")
}

// docFileAt is path's content and blob sha at ref — master, or a commit
// the door merges a stale save over; present false when there is no such
// file at it.
func (c *Console) docFileAt(ctx context.Context, repo factoryd.Repo, path, ref string) (content, blob string, present bool, err error) {
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.api(ctx, c.cfg.DocsToken, http.MethodGet, docAPI(repo)+"/contents/"+path+"?ref="+url.QueryEscape(ref), nil, http.StatusOK, &file); err != nil {
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

// readDoc is read_doc: path at master with its base_sha256 and line count,
// its first docShown bytes, its lines from..to, or the lines holding find,
// by literal text or a regular expression.
func (c *Console) readDoc(ctx context.Context, docRepo, path, find string, from, to int, regex bool) (string, error) {
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
	return readTextDocument(fmt.Sprintf("%s on %s@master", path, docRepo), []byte(content), from, to, find, regex)
}

// readTextDocument is the reading engine read_doc and the chat's
// attachment reader share: label's head with its bytes, its lines and
// base_sha256, then the first docShown bytes, the lines from..to, or the
// lines holding find, by literal text or a regular expression.
func readTextDocument(label string, data []byte, from, to int, find string, regex bool) (string, error) {
	content := string(data)
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	if content == "" {
		lines = nil
	}
	head := fmt.Sprintf("%s: %d bytes, %d lines, base_sha256 %s.", label, len(content), len(lines), factoryd.ContentHash(content))
	if find != "" && (from > 0 || to > 0) {
		return head + " find and a line range do not combine: find searches every line, from and to read one.", nil
	}
	if from != 0 || to != 0 {
		if from < 0 || to < 0 || from > len(lines) || (from > 0 && to > 0 && from > to) {
			return head + fmt.Sprintf(" from %d and to %d name no line range of its %d lines: name from at or before to, inside lines 1 to %d.", from, to, len(lines), len(lines)), nil
		}
		var b strings.Builder
		size := 0
		for i := max(from, 1); i <= min(cmp.Or(to, len(lines)), len(lines)); i++ {
			if l := lines[i-1]; size+len(l) > docShown {
				fmt.Fprint(&b, "… a narrower range fits: raise from or lower to.\n")
				break
			} else {
				size += len(l)
				fmt.Fprintf(&b, "line %d: %s\n", i, l)
			}
		}
		return head + "\n\n" + b.String(), nil
	}
	if find == "" {
		if len(content) > docShown {
			return head + " Its first 20 KiB; read lines with from and to, find the ones holding a word, a pattern with regex.\n\n" + textutil.Truncate(content, docShown), nil
		}
		return head + "\n\n" + content, nil
	}
	var match func(string) bool
	if regex {
		expr, err := regexp.Compile(find)
		if err != nil {
			return head + fmt.Sprintf(" find %q is no regular expression: %v.", find, err), nil
		}
		match = expr.MatchString
	} else {
		needle := strings.ToLower(find)
		match = func(l string) bool { return strings.Contains(strings.ToLower(l), needle) }
	}
	var b strings.Builder
	n, size := 0, 0
	for i, l := range lines {
		if !match(l) {
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
		if regex {
			return head + fmt.Sprintf(" No line matches the pattern %q: widen it, or read a line range with from and to.", find), nil
		}
		return head + fmt.Sprintf(" No line holds %q. find matches literal text, case-insensitively: try a word of the document, a pattern with regex, or a line range with from and to.", find), nil
	}
	return head + "\n\n" + b.String(), nil
}

// docWritten is the write door's one event per commit it lands, whatever
// wrote through it: the chat's write_doc or the member's save in the
// console (roadmap 277, the 2026-10-15 plan, item 4). project is the
// repository of the project the write belongs to.
func (c *Console) docWritten(ctx context.Context, a actor, project, path, hash, commit string) {
	c.event(context.WithoutCancel(ctx), factoryd.Event{Repository: project, Kind: factoryd.EventDocWritten,
		Actor: a.s.user, Via: a.via, Detail: map[string]any{"path": path, "hash": hash, "commit": commit}})
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
// project is the repository of the project the write belongs to, which
// the door's one doc_written event names.
func (c *Console) doWriteDoc(ctx context.Context, a actor, docRepo, project, path string, edit docEdit) (string, error) {
	repo, err := c.docWriteRepoOf(docRepo, path)
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
	c.docWritten(ctx, a, project, path, factoryd.ContentHash(next), committed.Commit.SHA)
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
