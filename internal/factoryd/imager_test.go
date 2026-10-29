package factoryd

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const (
	imagerBase = "git.aisoftwarefactory.world/sf/sandbox-base@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	// printf '{"Apt":["jq"],"Upstream":null,"Env":null,"Path":null,"Egress":null}\n<the base digest>\n2' | sha256sum
	keyDefault = "k-21da109c06519bda5dc9e83b"
	// the same over {"Apt":["chromium"],"Upstream":null,"Env":null,"Path":null,"Egress":["cdn.example.org"]}
	keyBrowser      = "k-927695707916775c36d611ff"
	infraAPI        = "/api/v1/repos/sf/ai-software-factory-infra"
	forkAPI         = "/api/v1/repos/factory-agent/ai-software-factory-infra"
	codeAPI         = "/api/v1/repos/sf/ai-software-factory-code"
	demoProjectFile = "factory/projects/demo-app.yaml"
	demoImages      = "images:\n  default:\n    apt: [jq]\n"
	bothImages      = "images:\n  default:\n    apt: [jq]\n  browser:\n    apt: [chromium]\n    egress: [cdn.example.org]\n"
)

// imagerForgejo is a scripted Forgejo for the imager: each project
// repository's factory/images.yaml on main, the dispatches of the code
// repository's project-image.yml, and sf/ai-software-factory-infra with
// its commits, main, the bot's fork and the pull requests. The bot reads
// every repository, owns its fork and merges; factory-config's token reads
// the infra repository; factory-ci's dispatches workflows only.
type imagerForgejo struct {
	mu         sync.Mutex
	images     map[string]string            // factory/images.yaml by repository
	commits    map[string]map[string]string // the infra repository's commits: every file by its path
	main       string
	fork       map[string]forkBranch // the fork's branches by name; nil: no fork
	prs        []*imagerPR
	dispatches []string
	writes     []string
	comments   []string
	refuse     int // Forgejo answers the bot's merge 405 (1) or 409 (2)
	inForce    policy.Version
	// packages are sf's container package versions by package name, which
	// the site admin lists and deletes, created the created_at of a version
	// by name (none: long ago); lists counts the lists, the next failLists of
	// which answer 500; with bumpOnList a list loads a new version of main's
	// commit, with moveOnList a person merges on main
	packages   map[string][]string
	created    map[string]string
	lists      int
	failLists  int
	bumpOnList bool
	moveOnList bool
	issues     []string          // the issues filed on demo-app/demo-app, title and body
	checks     map[string]string // the combined state of a commit's checks by sha, "" for none
	failIssues int               // the next issues filed on demo-app/demo-app answer 500
	// denyDispatch refuses factory-ci's dispatches, as a token without
	// Actions write is refused
	denyDispatch bool
}

type forkBranch struct{ head, base string }

type imagerPR struct {
	number            int64
	head, title, body string
	base, state       string
	merged            bool
	mergedBy          string
}

func (s *imagerForgejo) commit(files map[string]string) string {
	sha := fmt.Sprintf("c%d", len(s.commits)+1)
	s.commits[sha] = files
	return sha
}

func (s *imagerForgejo) wire(pr *imagerPR) map[string]any {
	b := s.fork[pr.head]
	return map[string]any{"number": pr.number, "title": pr.title, "body": pr.body, "state": pr.state, "merged": pr.merged, "mergeable": true,
		"merge_base": b.base, "base": map[string]string{"ref": pr.base},
		"head": map[string]any{"ref": pr.head, "sha": b.head, "repo": map[string]string{"full_name": "factory-agent/ai-software-factory-infra"}}}
}

func (s *imagerForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		by := "nobody"
		if u, pw, ok := r.BasicAuth(); ok && u == "factory-agent" && pw == "bot-pw" {
			by = "bot"
		} else if ok && u == "sebastian" && pw == "admin-pw" {
			by = "admin"
		} else if a := r.Header.Get("Authorization"); a == "token config-tok" {
			by = "config"
		} else if a == "token ci-tok" {
			by = "ci"
		}
		p, q := r.URL.Path, r.URL.Query()
		if r.Method != http.MethodGet {
			s.writes = append(s.writes, r.Method+" "+strings.TrimPrefix(p, "/api/v1/repos/")+" by "+by)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		pr := func() *imagerPR {
			var n int
			fmt.Sscanf(strings.TrimPrefix(p, infraAPI+"/pulls/"), "%d", &n)
			return s.prs[n-1]
		}
		content := func(c string) {
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(c)), "encoding": "base64", "sha": blobOf(c)})
		}
		switch {
		case by == "nobody":
			http.NotFound(w, r)
		case by == "admin" && r.Method == http.MethodGet && p == "/api/v1/packages/sf" && q.Get("page") != "1":
			fmt.Fprint(w, `[]`)
		case by == "admin" && r.Method == http.MethodGet && p == "/api/v1/packages/sf":
			s.lists++
			if s.failLists > 0 {
				s.failLists--
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if s.bumpOnList {
				s.inForce.ID++
			}
			if s.moveOnList {
				s.main = s.commit(maps.Clone(s.commits[s.main]))
			}
			out := []map[string]string{}
			for _, name := range slices.Sorted(maps.Keys(s.packages)) {
				if strings.Contains(name, strings.ToLower(q.Get("q"))) {
					for _, v := range s.packages[name] {
						out = append(out, map[string]string{"type": "container", "name": name, "version": v, "created_at": cmp.Or(s.created[v], "2026-01-01T00:00:00Z")})
					}
				}
			}
			json.NewEncoder(w).Encode(out)
		case by == "admin" && r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v1/packages/sf/container/"):
			name, version, _ := strings.Cut(strings.TrimPrefix(p, "/api/v1/packages/sf/container/"), "/")
			if !slices.Contains(s.packages[name], version) {
				http.NotFound(w, r)
				return
			}
			s.packages[name] = slices.DeleteFunc(s.packages[name], func(v string) bool { return v == version })
			w.WriteHeader(http.StatusNoContent)
		case by == "admin":
			t.Errorf("unexpected %s %s by the admin", r.Method, r.URL)
			http.NotFound(w, r)
		case by == "ci":
			if r.Method != http.MethodPost || p != codeAPI+"/actions/workflows/project-image.yml/dispatches" || s.denyDispatch {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			b, _ := json.Marshal(body)
			s.dispatches = append(s.dispatches, string(b))
			w.WriteHeader(http.StatusNoContent)
		case by == "config" && (r.Method != http.MethodGet || !strings.HasPrefix(p, infraAPI+"/")):
			http.NotFound(w, r)
		case by == "config" && p == infraAPI+"/branches/main":
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, s.main)
		case by == "config" && strings.HasPrefix(p, infraAPI+"/raw/"):
			c, ok := s.commits[q.Get("ref")][strings.TrimPrefix(p, infraAPI+"/raw/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, c)
		case by == "config" && strings.HasPrefix(p, infraAPI+"/git/trees/"):
			sha := strings.TrimPrefix(p, infraAPI+"/git/trees/")
			if _, ok := s.commits[sha]; ok {
				fmt.Fprintf(w, `{"total_count":1,"tree":[{"path":"factory","type":"tree","mode":"040000","sha":"T%s"}]}`, sha)
				return
			}
			var entries []treeEntry
			for path, c := range s.commits[strings.TrimPrefix(sha, "T")] {
				if rel, ok := strings.CutPrefix(path, "factory/"); ok {
					entries = append(entries, treeEntry{Path: rel, Type: "blob", Mode: "100644", SHA: blobOf(c)})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(entries), "tree": entries})
		case by == "config":
			t.Errorf("unexpected %s %s by config", r.Method, r.URL)
			http.NotFound(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(p, infraAPI+"/commits/") && strings.HasSuffix(p, "/status"):
			fmt.Fprintf(w, `{"state":%q,"statuses":[]}`, s.checks[strings.TrimSuffix(strings.TrimPrefix(p, infraAPI+"/commits/"), "/status")])
		case r.Method == http.MethodGet && p == infraAPI+"/pulls" && q.Get("state") == "open":
			out := []map[string]any{}
			for _, pr := range s.prs {
				if pr.state == "open" && q.Get("page") == "1" {
					out = append(out, s.wire(pr))
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && p == infraAPI+"/pulls":
			head, ok := strings.CutPrefix(body["head"].(string), "factory-agent:")
			if _, cut := s.fork[head]; !ok || !cut || body["base"] != "main" {
				t.Errorf("pull request %v", body)
			}
			s.prs = append(s.prs, &imagerPR{number: int64(len(s.prs) + 1), head: head, title: body["title"].(string), body: body["body"].(string), base: "main", state: "open"})
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(s.wire(s.prs[len(s.prs)-1]))
		case r.Method == http.MethodGet && strings.HasPrefix(p, infraAPI+"/pulls/") && strings.HasSuffix(p, "/files"):
			out := []map[string]string{}
			if b := s.fork[pr().head]; q.Get("page") == "1" {
				for _, path := range changed(s.commits[b.base], s.commits[b.head]) {
					out = append(out, map[string]string{"filename": path})
				}
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasPrefix(p, infraAPI+"/pulls/") && strings.HasSuffix(p, "/merge"):
			pr, b := pr(), s.fork[pr().head]
			switch {
			case s.refuse == 1:
				http.Error(w, `{"message":"User not allowed to merge PR"}`, http.StatusMethodNotAllowed)
				return
			case s.refuse == 2:
				http.Error(w, `{"message":"merge conflict"}`, http.StatusConflict)
				return
			case body["Do"] != "merge" || body["head_commit_id"] != b.head || pr.state != "open":
				http.Error(w, `{"message":"head moved"}`, http.StatusConflict)
				return
			}
			files := maps.Clone(s.commits[s.main])
			for _, path := range changed(s.commits[b.base], s.commits[b.head]) {
				files[path] = s.commits[b.head][path]
			}
			s.main = s.commit(files)
			pr.state, pr.merged, pr.mergedBy = "closed", true, by
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasPrefix(p, infraAPI+"/pulls/") && body["state"] == "closed":
			pr().state = "closed"
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && strings.HasPrefix(p, infraAPI+"/issues/") && strings.HasSuffix(p, "/comments"):
			var n int
			fmt.Sscanf(strings.TrimPrefix(p, infraAPI+"/issues/"), "%d", &n)
			s.comments = append(s.comments, fmt.Sprintf("#%d: %s", n, body["body"]))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && p == forkAPI && s.fork == nil:
			http.NotFound(w, r)
		case r.Method == http.MethodGet && p == forkAPI || r.Method == http.MethodPost && p == infraAPI+"/forks":
			if s.fork == nil {
				s.fork = map[string]forkBranch{"main": {head: s.main}}
				w.WriteHeader(http.StatusAccepted)
			}
			fmt.Fprint(w, `{"fork":true,"private":true,"parent":{"full_name":"sf/ai-software-factory-infra"}}`)
		case p == forkAPI+"/sync_fork" && r.Method == http.MethodGet:
			behind := 0
			if s.fork["main"].head != s.main {
				behind = 1
			}
			fmt.Fprintf(w, `{"allowed":true,"commits_behind":%d}`, behind)
		case p == forkAPI+"/sync_fork" && r.Method == http.MethodPost:
			s.fork["main"] = forkBranch{head: s.main}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, forkAPI+"/branches/"):
			name := strings.TrimPrefix(p, forkAPI+"/branches/")
			b, ok := s.fork[name]
			switch {
			case !ok:
				http.NotFound(w, r)
			case r.Method == http.MethodDelete:
				delete(s.fork, name)
				w.WriteHeader(http.StatusNoContent)
			default:
				fmt.Fprintf(w, `{"commit":{"id":%q}}`, b.head)
			}
		case r.Method == http.MethodGet && strings.HasPrefix(p, forkAPI+"/contents/"):
			ref := q.Get("ref")
			if b, ok := s.fork[ref]; ok {
				ref = b.head
			}
			c, ok := s.commits[ref][strings.TrimPrefix(p, forkAPI+"/contents/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			content(c)
		case r.Method == http.MethodPost && p == forkAPI+"/contents":
			from := s.fork[body["branch"].(string)]
			files := maps.Clone(s.commits[from.head])
			for _, op := range body["files"].([]any) {
				op := op.(map[string]any)
				path := op["path"].(string)
				c, _ := base64.StdEncoding.DecodeString(op["content"].(string))
				if old, exists := files[path]; op["operation"] != "update" || !exists || op["sha"] != blobOf(old) {
					t.Errorf("%v of %s with sha %v, exists %v", op["operation"], path, op["sha"], exists)
				}
				files[path] = string(c)
			}
			sha := s.commit(files)
			if nb, _ := body["new_branch"].(string); nb != "" {
				s.fork[nb] = forkBranch{head: sha, base: from.head}
			} else {
				s.fork[body["branch"].(string)] = forkBranch{head: sha, base: from.base}
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"commit":{"sha":%q}}`, sha)
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/contents/factory/images.yaml"):
			repo := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/repos/"), "/contents/factory/images.yaml")
			c, ok := s.images[repo]
			if q.Get("ref") != "main" || !ok {
				http.NotFound(w, r)
				return
			}
			content(c)
		case r.Method == http.MethodPost && p == "/api/v1/repos/demo-app/demo-app/issues" && s.failIssues > 0:
			s.failIssues--
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodPost && p == "/api/v1/repos/demo-app/demo-app/issues":
			s.issues = append(s.issues, body["title"].(string)+"\n\n"+body["body"].(string))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d}`, 100+len(s.issues))
		case r.Method == http.MethodPut && strings.HasPrefix(p, "/api/v1/repos/demo-app/demo-app/issues/") && strings.HasSuffix(p, "/labels"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.Count(strings.TrimPrefix(p, "/api/v1/repos/"), "/") == 1:
			fmt.Fprint(w, `{"default_branch":"main"}`)
		default:
			t.Errorf("unexpected %s %s by %s", r.Method, r.URL, by)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// load makes main's tree the version in force, as the loader does.
func (s *imagerForgejo) load(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	files := map[string][]byte{}
	for path, c := range s.commits[s.main] {
		if rel, ok := strings.CutPrefix(path, "factory/"); ok {
			files[rel] = []byte(c)
		}
	}
	tree, err := CheckTree(files, imagerLadder)
	if err != nil {
		t.Fatalf("main's tree: %v", err)
	}
	s.inForce = policy.Version{ID: s.inForce.ID + 1, Commit: s.main, Tree: tree}
}

func (s *imagerForgejo) version() policy.Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inForce
}

// file is path at the head of the fork's branch, or main's for "main".
func (s *imagerForgejo) file(branch, path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if branch == "main" {
		return s.commits[s.main][path]
	}
	return s.commits[s.fork[branch].head][path]
}

// tamper commits files on the fork's branch as someone other than the pass.
func (s *imagerForgejo) tamper(branch string, files map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.fork[branch]
	all := maps.Clone(s.commits[b.head])
	maps.Copy(all, files)
	s.fork[branch] = forkBranch{head: s.commit(all), base: b.base}
}

// moveMain commits files on main as a person does.
func (s *imagerForgejo) moveMain(files map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := maps.Clone(s.commits[s.main])
	maps.Copy(all, files)
	s.main = s.commit(all)
}

func (s *imagerForgejo) state() ([]imagerPR, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prs []imagerPR
	for _, pr := range s.prs {
		prs = append(prs, *pr)
	}
	return prs, slicesClone(s.dispatches), slicesClone(s.writes)
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

var imagerLadder = ModelLadder{Start: "factory/small", Stronger: "factory/big"}.Aliases()

// imageBook is the imager's store: image_builds and the alerts, one per
// kind and fingerprint as the alerts table's unique index keeps them.
type imageBook struct {
	mu      sync.Mutex
	builds  map[string]imageBuild // by project/image/key
	alerts  []Alert
	parks   []ImagePark
	tasks   map[string]Task
	pins    map[string]ImagePinRow // by project/image
	used    map[string]bool        // the store repositories of the images a card names lately
	usedErr error                  // ImageUsed's answer when set
	asked   []string               // ImageUsed's questions, "<repository> <image> <store> since <time>"
}

func (b *imageBook) ImagePins(context.Context) ([]ImagePinRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ImagePinRow
	for _, k := range slices.Sorted(maps.Keys(b.pins)) {
		out = append(out, b.pins[k])
	}
	return out, nil
}

func (b *imageBook) SaveImagePin(_ context.Context, r ImagePinRow) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pins[r.Project+"/"+r.Image] = r
	return nil
}

func (b *imageBook) DeleteImagePin(_ context.Context, project, image string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pins, project+"/"+image)
	return nil
}

func (b *imageBook) ImageUsed(_ context.Context, repository, image, store string, since time.Time) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, repository+" "+image+" "+store+" since "+since.Format(time.RFC3339))
	return b.used[store], b.usedErr
}

func (b *imageBook) Project(_ context.Context, name string) (Project, bool, error) {
	return demoProject(), name == "demo-app", nil
}

func (b *imageBook) CreateTask(_ context.Context, t Task) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tasks[t.ID] = t
	return nil
}

func (b *imageBook) OpenImageParks(context.Context) ([]ImagePark, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ImagePark
	for _, p := range b.parks {
		if t, ok := b.tasks[p.Task]; p.WokenAt == nil && !(ok && (Final(t.Status) || t.Status == "merged")) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (b *imageBook) WakeImagePark(_ context.Context, task string, created time.Time, refused string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, p := range b.parks {
		if p.Task == task && p.CreatedAt.Equal(created) && p.WokenAt == nil {
			now := time.Now()
			b.parks[i].WokenAt, b.parks[i].Refused = &now, refused
		}
	}
	return nil
}

func (b *imageBook) ImageReadAt(_ context.Context, project, image, key string) (time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds[project+"/"+image+"/"+key].read, nil
}

func (b *imageBook) Task(_ context.Context, id string) (Task, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	return t, ok, nil
}

// imageSignals records the imager's signals as "<task> <signal> <image>:
// <refusal>", and a reopen as "reopen <task> <signal>".
type imageSignals struct {
	mu  sync.Mutex
	got []string
}

func (w *imageSignals) Start(context.Context, string) error                    { return nil }
func (w *imageSignals) StartRoutineRun(context.Context, RoutineRunInput) error { return nil }
func (w *imageSignals) Reopen(_ context.Context, taskID, name string, _ TaskSignal) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, "reopen "+taskID+" "+name)
	return nil
}
func (w *imageSignals) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, taskID+" "+name+" "+sig.Image+": "+sig.Refusal)
	return nil
}

type imageBuild struct {
	dispatched, read time.Time
	dispatches       int
}

func (b *imageBook) ImageDispatches(_ context.Context, project, image, key string) (time.Time, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.builds[project+"/"+image+"/"+key]
	return r.dispatched, r.dispatches, nil
}

func (b *imageBook) RecordImageDispatch(_ context.Context, project, image, key string, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.builds[project+"/"+image+"/"+key]
	r.dispatched = at
	r.dispatches++
	b.builds[project+"/"+image+"/"+key] = r
	return nil
}

func (b *imageBook) ResetImageDispatches(_ context.Context, project, image, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.builds[project+"/"+image+"/"+key]; ok {
		r.dispatches = 0
		b.builds[project+"/"+image+"/"+key] = r
	}
	return nil
}

func (b *imageBook) RecordImageRead(_ context.Context, project, image, key string, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.builds[project+"/"+image+"/"+key]
	r.read = at
	b.builds[project+"/"+image+"/"+key] = r
	return nil
}

func (b *imageBook) InsertAlert(_ context.Context, a Alert) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, have := range b.alerts {
		if have.Kind == a.Kind && have.Fingerprint == a.Fingerprint {
			return 0, nil
		}
	}
	b.alerts = append(b.alerts, a)
	return int64(len(b.alerts)), nil
}

type imagerRig struct {
	signals  *imageSignals
	fj       *imagerForgejo
	im       *Imager
	book     *imageBook
	registry map[string]string
	tokens   *int
	now      *time.Time
	key      *ecdsa.PrivateKey // the images key
}

// newImagerRig is sf/ai-software-factory-infra with main's factory/ the
// policy fixture, edited by edit, in force, demo-app/demo-app's
// factory/images.yaml images (none for ""), a registry holding the
// images key's signature of every digest main pins and the imager as
// factoryd wires it.
func newImagerRig(t *testing.T, images string, edit func(main map[string]string)) *imagerRig {
	t.Helper()
	fixture, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	main := map[string]string{}
	for rel, b := range fixture {
		main["factory/"+rel] = string(b)
	}
	if edit != nil {
		edit(main)
	}
	fj := &imagerForgejo{images: map[string]string{}, commits: map[string]map[string]string{}, packages: map[string][]string{}}
	if images != "" {
		fj.images["demo-app/demo-app"] = images
	}
	fj.main = fj.commit(main)
	fj.load(t)
	srv := fj.server(t)
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", HTTP: srv.Client()}
	config := &ForgejoConfig{Forgejo: f, Token: "config-tok", Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Branch: "main", Dir: "factory"}
	entries := map[string]string{}
	reg, tokens := testRegistry(t, entries)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for project, c := range fj.inForce.Projects {
		for image, pin := range c.Images {
			maps.Copy(entries, cosignSigned(key, imageRepository(project, image), pin.Digest))
		}
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	book := &imageBook{builds: map[string]imageBuild{}, tasks: map[string]Task{}, pins: map[string]ImagePinRow{}, used: map[string]bool{}}
	signals := &imageSignals{}
	im := &Imager{Forgejo: f, Config: config, Policy: fj.version, Store: book, Registry: reg, Key: &key.PublicKey, Base: imagerBase,
		Builder: Repo{Owner: "sf", Name: "ai-software-factory-code"}, BuildToken: "ci-tok", Ladder: imagerLadder, Now: func() time.Time { return now },
		Workflows: signals, OpenBao: &fakeKV{data: map[string]string{"username": "sebastian", "password": "admin-pw"}}}
	return &imagerRig{signals: signals, fj: fj, im: im, book: book, registry: entries, tokens: tokens, now: &now, key: key}
}

func (r *imagerRig) pass(t *testing.T) {
	t.Helper()
	if err := r.im.pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

// publish pushes the image of tag into repository as the builder does, an
// index around a linux/amd64 manifest of layers of these sizes, signed by
// signer unless nil, and returns the index's digest.
func (r *imagerRig) publish(repository, tag string, signer *ecdsa.PrivateKey, layers ...int64) string {
	var ls []string
	for i, n := range layers {
		ls = append(ls, fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:%064x","size":%d}`, i, n))
	}
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:c0","size":7777},"layers":[` + strings.Join(ls, ",") + `]}`
	index := strings.Replace(testIndexTemplate, "IMG", sha256Of(manifest), 1)
	r.registry[repository+"/manifests/"+tag] = index
	r.registry[repository+"/manifests/"+sha256Of(index)] = index
	r.registry[repository+"/manifests/"+sha256Of(manifest)] = manifest
	if signer != nil {
		maps.Copy(r.registry, cosignSigned(signer, repository, sha256Of(index)))
	}
	return sha256Of(index)
}

// build publishes the image of tag into repository as publish does, and
// answers its digest and the versions the store lists for it: the tag, the
// manifest its index names, and, signed, cosign's tag and signature
// manifest.
func (r *imagerRig) build(repository, tag string, signer *ecdsa.PrivateKey, layers ...int64) (string, []string) {
	before := maps.Clone(r.registry)
	digest := r.publish(repository, tag, signer, layers...)
	var versions []string
	for k := range r.registry {
		v, ok := strings.CutPrefix(k, repository+"/manifests/")
		if _, had := before[k]; ok && !had && v != digest {
			versions = append(versions, v)
		}
	}
	slices.Sort(versions)
	return digest, versions
}

// versions are the versions of sf's package name, sorted, and how many
// lists the admin read.
func (s *imagerForgejo) versions(name string) ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(slices.Values(s.packages[name])), s.lists
}

// A declared image no build has made is dispatched to the trusted builder
// with its file, the base and its key, once: not again within 45 minutes,
// and again after.
func TestImagerDispatchesAnUnbuiltImageOnce(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.pass(t)
	want := `{"inputs":{"base":"` + imagerBase + `","file":"` + base64.StdEncoding.EncodeToString([]byte(demoImages)) +
		`","image":"default","key":"` + keyDefault + `","project":"demo-app"},"ref":"main"}`
	if _, dispatches, writes := r.fj.state(); !reflect.DeepEqual(dispatches, []string{want}) ||
		!reflect.DeepEqual(writes, []string{"POST sf/ai-software-factory-code/actions/workflows/project-image.yml/dispatches by ci"}) {
		t.Fatalf("dispatches %q, writes %q; want one, %q", dispatches, writes, want)
	}
	t0 := *r.now
	if got := r.book.builds["demo-app/default/"+keyDefault]; got != (imageBuild{dispatched: t0, read: t0, dispatches: 1}) {
		t.Fatalf("image_builds row %+v, want dispatched once and read at %v", got, t0)
	}

	*r.now = t0.Add(44 * time.Minute)
	r.pass(t)
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 1 {
		t.Fatalf("44 minutes on: %d dispatches, want the one", len(dispatches))
	}
	if got := r.book.builds["demo-app/default/"+keyDefault]; got != (imageBuild{dispatched: t0, read: *r.now, dispatches: 1}) {
		t.Fatalf("image_builds row %+v, want dispatched once at %v, read %v", got, t0, *r.now)
	}

	*r.now = t0.Add(46 * time.Minute)
	r.pass(t)
	if _, dispatches, _ := r.fj.state(); !reflect.DeepEqual(dispatches, []string{want, want}) {
		t.Fatalf("46 minutes on: dispatches %q, want the second", dispatches)
	}
	if _, _, writes := r.fj.state(); len(writes) != 2 || len(r.book.alerts) != 0 {
		t.Fatalf("writes %q, alerts %+v: want the two dispatches only", writes, r.book.alerts)
	}
}

// Two ready images, signed by the images key, open one pull request from
// the bot's fork whose only change appends the exact images block to the
// project file; the next pass merges it as the bot, and the pass after,
// with the pins in force, writes nothing.
func TestImagerPinsReadyImagesThroughOnePullRequest(t *testing.T) {
	r := newImagerRig(t, bothImages, nil)
	digestDefault := r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000, 2345)
	digestBrowser := r.publish("sf/sandbox-demo-app.browser", keyBrowser, r.key, 5000)
	before := r.fj.file("main", demoProjectFile)
	r.pass(t)

	block := "images:\n" +
		"  browser:\n" +
		"    digest: " + digestBrowser + "\n" +
		"    key: " + keyBrowser + "\n" +
		"    tools:\n" +
		"      - chromium\n" +
		"    egress:\n" +
		"      - cdn.example.org\n" +
		"  default:\n" +
		"    digest: " + digestDefault + "\n" +
		"    key: " + keyDefault + "\n" +
		"    tools:\n" +
		"      - jq\n"
	if got := r.fj.file("factory-pins/demo-app", demoProjectFile); got != before+block {
		t.Fatalf("the branch's project file:\n%s\nwant:\n%s", got, before+block)
	}
	body := "factoryd pins the sandbox images factory/images.yaml of demo-app/demo-app declares: this pull request changes only the images block of factory/projects/demo-app.yaml.\n\n" +
		"factoryd merges it itself once it has checked that, and that the tree at its head passes factoryd's config check; otherwise it stays open and factoryd raises an alert naming why.\n"
	writes := []string{"POST sf/ai-software-factory-infra/forks by bot", "POST factory-agent/ai-software-factory-infra/contents by bot", "POST sf/ai-software-factory-infra/pulls by bot"}
	prs, dispatches, got := r.fj.state()
	if want := []imagerPR{{number: 1, head: "factory-pins/demo-app", title: "Image pins for demo-app", body: body, base: "main", state: "open"}}; !reflect.DeepEqual(prs, want) {
		t.Fatalf("pull requests %+v, want %+v", prs, want)
	}
	if len(dispatches) != 0 || !reflect.DeepEqual(got, writes) {
		t.Fatalf("dispatches %q, writes %q; want none, %q", dispatches, got, writes)
	}

	r.pass(t)
	writes = append(writes, "POST sf/ai-software-factory-infra/pulls/1/merge by bot")
	if prs, _, got := r.fj.state(); !prs[0].merged || prs[0].mergedBy != "bot" || !reflect.DeepEqual(got, writes) {
		t.Fatalf("pull request %+v, writes %q; want merged by the bot, %q", prs[0], got, writes)
	}
	if got := r.fj.file("main", demoProjectFile); got != before+block {
		t.Fatalf("main's project file:\n%s", got)
	}
	r.fj.load(t)
	r.pass(t)
	if _, _, got := r.fj.state(); !reflect.DeepEqual(got, writes) || len(r.book.alerts) != 0 {
		t.Fatalf("with the pins in force: writes %q, alerts %+v; want no more", got, r.book.alerts)
	}
}

// demoPinned is demo-app's project file with comments around an images
// block that pins default at its desired key and old, which images.yaml no
// longer declares.
func demoPinned(main map[string]string) {
	main[demoProjectFile] = strings.Replace(main[demoProjectFile], "merge_rule: auto\n",
		"merge_rule: auto\n# the pins: factoryd's\nimages:\n"+
			"  default:\n    digest: sha256:"+strings.Repeat("d", 64)+"\n    key: "+keyDefault+"\n    tools: [jq]\n"+
			"  old:\n    digest: sha256:"+strings.Repeat("e", 64)+"\n    key: k-"+strings.Repeat("0", 24)+"\n    tools: [make]\n    egress: [old.example.org]\n"+
			"\n# objects: the registration's\n", 1)
}

// A pinned name images.yaml no longer declares leaves the block, every
// other byte of the file kept; once the file is gone the whole block
// leaves, written on the open pull request's branch in place, and the pass
// after merges it.
func TestImagerDropsAnUndeclaredPin(t *testing.T) {
	r := newImagerRig(t, demoImages, demoPinned)
	before := r.fj.file("main", demoProjectFile)
	r.pass(t)
	head, rest, _ := strings.Cut(before, "# the pins: factoryd's\n")
	_, tail, _ := strings.Cut(rest, "    egress: [old.example.org]\n")
	want := head + "# the pins: factoryd's\nimages:\n  default:\n    digest: sha256:" + strings.Repeat("d", 64) + "\n    key: " + keyDefault + "\n    tools:\n      - jq\n" + tail
	if !strings.HasPrefix(tail, "\n# objects: the registration's\nobjects: factory\n") {
		t.Fatalf("the fixture moved: %q", tail)
	}
	if got := r.fj.file("factory-pins/demo-app", demoProjectFile); got != want {
		t.Fatalf("the branch's project file:\n%s\nwant:\n%s", got, want)
	}
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 0 {
		t.Fatalf("dispatches %q: default is built", dispatches)
	}

	r.fj.mu.Lock()
	delete(r.fj.images, "demo-app/demo-app")
	r.fj.mu.Unlock()
	r.pass(t)
	want = head + "# the pins: factoryd's\n" + tail
	if got := r.fj.file("factory-pins/demo-app", demoProjectFile); got != want {
		t.Fatalf("the branch without images.yaml:\n%s\nwant:\n%s", got, want)
	}
	writes := []string{"POST sf/ai-software-factory-infra/forks by bot", "POST factory-agent/ai-software-factory-infra/contents by bot",
		"POST sf/ai-software-factory-infra/pulls by bot", "POST factory-agent/ai-software-factory-infra/contents by bot"}
	if prs, _, got := r.fj.state(); len(prs) != 1 || prs[0].state != "open" || !reflect.DeepEqual(got, writes) {
		t.Fatalf("pull requests %+v, writes %q; want the one open, %q", prs, got, writes)
	}
	r.pass(t)
	if got := r.fj.file("main", demoProjectFile); got != want {
		t.Fatalf("main after the merge:\n%s\nwant:\n%s", got, want)
	}
}

// demoBothPinned pins demo-app's default and browser images at the keys
// bothImages desires.
func demoBothPinned(main map[string]string) {
	main[demoProjectFile] = strings.Replace(main[demoProjectFile], "merge_rule: auto\n", "merge_rule: auto\nimages:\n"+
		"  browser:\n    digest: sha256:"+strings.Repeat("b", 64)+"\n    key: "+keyBrowser+"\n    tools: [chromium]\n    egress: [cdn.example.org]\n"+
		"  default:\n    digest: sha256:"+strings.Repeat("d", 64)+"\n    key: "+keyDefault+"\n    tools: [jq]\n", 1)
}

// At the end of each pass the imager wakes the parked cards whose wait is
// over: a tool card that merged once a pass begun after the merge reads the
// pin in force at its desired key; a park without a tool card once its
// image is pinned; a tool card closed unmerged at once, and one merged
// without declaring the image once a pass after the merge reads the file,
// each with its refusal. A woken park is not signalled again, and the park
// of a card that ended is not read.
func TestImagerWakesParkedCards(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	t0 := *r.now
	merged := t0.Add(time.Minute)
	park := func(task, target, tool string, card *Task) {
		p := ImagePark{Task: task, Project: "demo-app", Target: target, Tool: tool, CreatedAt: t0.Add(-time.Hour)}
		if card != nil {
			p.ToolTask, p.ToolIssue = card.ID, card.IssueNumber
			r.book.tasks[card.ID] = *card
		}
		r.book.parks = append(r.book.parks, p)
	}
	park("t-a", "browser", "chromium", &Task{ID: "t-tool-a", Status: "merged", IssueNumber: 60, FinishedAt: &merged})
	park("t-b", "browser", "", nil)
	park("t-c", "video", "", nil)
	park("t-d", "browser", "chromium", &Task{ID: "t-tool-d", Status: "closed", IssueNumber: 61, FinishedAt: &merged})
	park("t-e", "docs", "pandoc", &Task{ID: "t-tool-e", Status: "done", IssueNumber: 62, FinishedAt: &merged})
	park("t-f", "browser", "chromium", &Task{ID: "t-tool-f", Status: "running", IssueNumber: 63})
	park("t-g", "browser", "", nil)
	r.book.tasks["t-g"] = Task{ID: "t-g", Status: "closed"}

	r.pass(t)
	if want := []string{"t-b image_ready browser: ", "t-d image_ready browser: #61 closed without a merge"}; !reflect.DeepEqual(r.signals.got, want) {
		t.Fatalf("a pass before the merges: signals %q, want %q", r.signals.got, want)
	}
	*r.now = merged.Add(time.Minute)
	r.pass(t)
	want := []string{"t-b image_ready browser: ", "t-d image_ready browser: #61 closed without a merge",
		"t-a image_ready browser: ", "t-e image_ready docs: #62 merged without declaring image docs"}
	if !reflect.DeepEqual(r.signals.got, want) {
		t.Fatalf("a pass after the merges: signals %q, want %q", r.signals.got, want)
	}
	*r.now = merged.Add(2 * time.Minute)
	r.pass(t)
	if !reflect.DeepEqual(r.signals.got, want) {
		t.Fatalf("a third pass: signals %q, want no more", r.signals.got)
	}
	for _, p := range r.book.parks {
		woken := p.Task == "t-a" || p.Task == "t-b" || p.Task == "t-d" || p.Task == "t-e"
		if (p.WokenAt != nil) != woken {
			t.Errorf("park %s woken %v, want %v", p.Task, p.WokenAt, woken)
		}
	}
	if p := r.book.parks[3]; p.Refused != "#61 closed without a merge" {
		t.Fatalf("t-d's park = %+v, want its refusal", p)
	}
	if _, _, writes := r.fj.state(); len(writes) != 0 {
		t.Fatalf("writes %q, want none: the pins are at their desired keys", writes)
	}
}

// A pin in force at a key older than a merged tool card's file wakes
// nothing: the pass after the merge reads the new key, which the pin does
// not carry yet.
func TestImagerWaitsForTheMergedKey(t *testing.T) {
	newBrowser := "images:\n  default:\n    apt: [jq]\n  browser:\n    apt: [chromium, ffmpeg]\n    egress: [cdn.example.org]\n"
	r := newImagerRig(t, newBrowser, demoBothPinned)
	merged := r.now.Add(-time.Minute)
	r.book.tasks["t-tool"] = Task{ID: "t-tool", Status: "merged", IssueNumber: 60, FinishedAt: &merged}
	r.book.parks = []ImagePark{{Task: "t-a", Project: "demo-app", Target: "browser", Tool: "ffmpeg", ToolTask: "t-tool", ToolIssue: 60, CreatedAt: merged.Add(-time.Hour)}}
	r.pass(t)
	if len(r.signals.got) != 0 || r.book.parks[0].WokenAt != nil {
		t.Fatalf("signals %q, park %+v: want none while the pin is at the old key", r.signals.got, r.book.parks[0])
	}
}

// A pin at its desired key whose build the store lacks, as after a re-seed,
// is built again: the pass dispatches it, and once the store holds the key
// signed, pins the digest the build made.
func TestImagerRebuildsAPinTheStoreLacks(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	clear(r.registry)
	r.pass(t)
	_, dispatches, writes := r.fj.state()
	if len(dispatches) != 2 || !strings.Contains(dispatches[0], `"key":"`+keyBrowser+`"`) || !strings.Contains(dispatches[1], `"key":"`+keyDefault+`"`) || len(writes) != 2 {
		t.Fatalf("dispatches %q, writes %q; want both images built again", dispatches, writes)
	}
	digestBrowser := r.publish("sf/sandbox-demo-app.browser", keyBrowser, r.key, 5000)
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	if got := r.fj.file("factory-pins/demo-app", demoProjectFile); !strings.Contains(got, "    digest: "+digestBrowser+"\n    key: "+keyBrowser+"\n") {
		t.Fatalf("the pin branch's project file:\n%s\nwant browser at %s", got, digestBrowser)
	}
}

// A verified digest stays verified for an hour: a store lost while
// factoryd runs is built again at the first pass past the hour.
func TestImagerRebuildsAStoreLostWhileRunning(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	t0 := *r.now
	r.pass(t)
	clear(r.registry)
	*r.now = t0.Add(59 * time.Minute)
	r.pass(t)
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 0 {
		t.Fatalf("within the hour: dispatches %q, want none", dispatches)
	}
	*r.now = t0.Add(time.Hour)
	r.pass(t)
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 2 {
		t.Fatalf("after the hour: dispatches %q, want both images built again", dispatches)
	}
}

// Once a key's signed build lands, its failed dispatches are forgotten: a
// later loss of the store builds it again at once, with no alert, however
// many dispatches it took before.
func TestImagerForgetsFailedBuildsOnceTheKeyLands(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	t0 := *r.now
	for n := range 3 {
		*r.now = t0.Add(time.Duration(n) * 46 * time.Minute)
		r.pass(t)
	}
	*r.now = t0.Add(3 * 46 * time.Minute)
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	r.pass(t)
	if got := r.book.builds["demo-app/default/"+keyDefault].dispatches; got != 0 {
		t.Fatalf("dispatches after the build landed = %d, want 0", got)
	}
	r.fj.load(t)
	clear(r.registry)
	*r.now = t0.Add(3*46*time.Minute + time.Hour)
	r.pass(t)
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 4 || len(r.book.alerts) != 0 || r.book.builds["demo-app/default/"+keyDefault].dispatches != 1 {
		t.Fatalf("after the store's loss: %d dispatches, alerts %+v, row %+v; want the fourth, none, one dispatch counted",
			len(dispatches), r.book.alerts, r.book.builds["demo-app/default/"+keyDefault])
	}
}

// A dispatch Forgejo refuses records no build; once it has failed for 45
// minutes, the pass raises one alert naming why.
func TestImagerAlertsADispatchThatFails(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.fj.mu.Lock()
	r.fj.denyDispatch = true
	r.fj.mu.Unlock()
	t0 := *r.now
	for _, after := range []time.Duration{0, 44 * time.Minute, 45 * time.Minute, 90 * time.Minute} {
		*r.now = t0.Add(after)
		if err := r.im.pass(context.Background()); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("%v on: pass = %v, want the refused dispatch", after, err)
		}
		if got := len(r.book.alerts); got != 0 && after < 45*time.Minute || got != 1 && after >= 45*time.Minute {
			t.Fatalf("%v on: alerts %+v", after, r.book.alerts)
		}
	}
	a := r.book.alerts[0]
	if reason, _ := a.Detail["reason"].(string); a.Source != "dispatch" || a.Fingerprint != "dispatch sf/sandbox-demo-app.default:"+keyDefault ||
		!strings.HasPrefix(reason, "the dispatch of project-image.yml of sf/ai-software-factory-code for sf/sandbox-demo-app.default:"+keyDefault+" has failed since 2026-10-22T12:00:00Z: ") {
		t.Fatalf("alert %+v", a)
	}
	if got := r.book.builds["demo-app/default/"+keyDefault]; got.dispatches != 0 {
		t.Fatalf("image_builds row %+v, want no dispatch recorded", got)
	}
}

// A tool card that merged an image whose builds fail wakes its parked card
// with the refusal once buildTries builds left no signed image, not
// before: the card goes on to a human with the cause.
func TestImagerWakesAParkWhoseImageNeverBuilds(t *testing.T) {
	r := newImagerRig(t, bothImages, nil)
	t0 := *r.now
	merged := t0.Add(-time.Minute)
	r.book.tasks["t-tool"] = Task{ID: "t-tool", Status: "merged", IssueNumber: 60, FinishedAt: &merged}
	r.book.parks = []ImagePark{{Task: "t-a", Project: "demo-app", Target: "browser", Tool: "chromium", ToolTask: "t-tool", ToolIssue: 60, CreatedAt: merged.Add(-time.Hour)}}
	for n := range 3 {
		*r.now = t0.Add(time.Duration(n) * 46 * time.Minute)
		r.pass(t)
		if len(r.signals.got) != 0 {
			t.Fatalf("pass %d: signals %q, want none while builds may land", n+1, r.signals.got)
		}
	}
	*r.now = t0.Add(3 * 46 * time.Minute)
	r.pass(t)
	refusal := "#60 merged, but image browser's build failed 3 times; the builder's run shows why"
	if want := []string{"t-a image_ready browser: " + refusal}; !reflect.DeepEqual(r.signals.got, want) || r.book.parks[0].Refused != refusal {
		t.Fatalf("signals %q, park %+v; want %q", r.signals.got, r.book.parks[0], want)
	}
}

// Forgejo's 405 while main's required checks run on the pull request's
// head is a wait: no refusal counts and no alert rises, however many
// passes it lasts, and the pass after the checks merges.
func TestImagerWaitsForPendingChecks(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	r.fj.mu.Lock()
	r.fj.refuse, r.fj.checks = 1, map[string]string{r.fj.fork["factory-pins/demo-app"].head: "pending"}
	r.fj.mu.Unlock()
	for range 3 {
		r.pass(t)
	}
	if prs, _, _ := r.fj.state(); len(prs) != 1 || prs[0].merged || prs[0].state != "open" || len(r.book.alerts) != 0 || len(r.im.refusals) != 0 {
		t.Fatalf("pull requests %+v, alerts %+v, refusals %v; want #1 open, none", prs, r.book.alerts, r.im.refusals)
	}
	r.fj.mu.Lock()
	r.fj.refuse = 0
	r.fj.mu.Unlock()
	r.pass(t)
	if prs, _, _ := r.fj.state(); !prs[0].merged {
		t.Fatalf("pull requests %+v; want #1 merged", prs)
	}
}

// A finding that persists across the pull requests main's changes cut
// afresh raises one alert per project and code, whichever pull request
// carries it.
func TestImagerAlertsAPersistentFindingOnce(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.fj.moveMain(map[string]string{"factory/processes/maintenance.yaml": r.fj.file("main", "factory/processes/maintenance.yaml") + "unknown_key: 1\n"})
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	r.pass(t)
	r.fj.moveMain(map[string]string{"factory/routines/weekly-bump.md": "a person's edit\n"})
	for range 3 {
		r.pass(t)
	}
	prs, _, _ := r.fj.state()
	if len(prs) != 2 || prs[0].state != "closed" || prs[1].state != "open" || len(r.book.alerts) != 1 || r.book.alerts[0].Fingerprint != "demo-app check" {
		t.Fatalf("pull requests %+v, alerts %+v; want #1 closed, #2 open, the one check alert", prs, r.book.alerts)
	}
}

// The configuration repository's tree holds no images file: the imager
// reads none there, whatever its project's agents.
func TestImagerSkipsTheConfigurationRepository(t *testing.T) {
	r := newImagerRig(t, "", func(main map[string]string) {
		main["factory/projects/ai-software-factory-infra.yaml"] = strings.NewReplacer("agents: none", "agents: coder\nprocess: maintenance",
			"tokens: {read: forgejo-config, chat: forgejo-chat}", "tokens: {read: forgejo-config, actions: forgejo-ci-registrar, merge: forgejo-merge, chat: forgejo-chat}").
			Replace(main["factory/projects/ai-software-factory-infra.yaml"])
	})
	if c := r.fj.version().Projects["ai-software-factory-infra"]; c.Agents != policy.AgentsCoder {
		t.Fatalf("the infra project's agents = %q, want coder", c.Agents)
	}
	r.fj.mu.Lock()
	r.fj.images["sf/ai-software-factory-infra"] = demoImages
	r.fj.mu.Unlock()
	r.pass(t)
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 0 {
		t.Fatalf("dispatches %q, want none", dispatches)
	}
}

// An invalid images.yaml raises one alert per blob, however many passes
// read it, and the project's pins stay: no pull request, no dispatch.
func TestImagerAlertsAnInvalidFileOnce(t *testing.T) {
	bad := "images:\n  default:\n    apt: [\"jq;rm\"]\n"
	r := newImagerRig(t, bad, demoPinned)
	r.pass(t)
	r.pass(t)
	want := []Alert{{Kind: AlertImage, Source: "file", Fingerprint: "demo-app factory/images.yaml " + blobOf(bad), Detail: map[string]any{
		"project": "demo-app", "repository": "demo-app/demo-app", "blob": blobOf(bad),
		"findings": []string{`image "default": apt "jq;rm" must match ^[a-z0-9][a-z0-9+.-]+$ (a Debian package name without a version)`}}}}
	if !reflect.DeepEqual(r.book.alerts, want) {
		t.Fatalf("alerts %+v, want %+v", r.book.alerts, want)
	}
	if prs, dispatches, writes := r.fj.state(); len(prs)+len(dispatches)+len(writes) != 0 {
		t.Fatalf("pull requests %+v, dispatches %q, writes %q; want none", prs, dispatches, writes)
	}
	worse := bad + "  x:\n    apt: []\n"
	r.fj.mu.Lock()
	r.fj.images["demo-app/demo-app"] = worse
	r.fj.mu.Unlock()
	r.pass(t)
	if len(r.book.alerts) != 2 || r.book.alerts[1].Fingerprint != "demo-app factory/images.yaml "+blobOf(worse) {
		t.Fatalf("alerts %+v, want a second for the new blob", r.book.alerts)
	}
}

// A tag signed by the factory's key is not ready: the images key alone
// makes a project image, so the pass builds it again. The other way round,
// the roll's verification with the factory's key refuses an image the
// images key signed, and verifyProjectImage takes only the images key's,
// reading the registry once per digest.
func TestImagerTakesTheImagesKeyOnly(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	factoryKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	byFactory := r.publish("sf/sandbox-demo-app.default", keyDefault, factoryKey, 1000)
	r.pass(t)
	if prs, dispatches, _ := r.fj.state(); len(prs) != 0 || len(dispatches) != 1 {
		t.Fatalf("pull requests %+v, dispatches %q; want none, one", prs, dispatches)
	}
	byImages := r.publish("sf/sandbox-demo-app.browser", keyBrowser, r.key, 2000)

	ctx := context.Background()
	tok, err := r.im.Registry.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var refusal ErrUnsigned
	if err := r.im.Registry.Verify(ctx, tok, "sf/sandbox-demo-app.browser", byImages, &factoryKey.PublicKey); !errors.As(err, &refusal) {
		t.Fatalf("the roll's verification of an images-key image: %v, want a refusal", err)
	}
	runner := &Runner{Imager: r.im}
	if err := runner.verifyProjectImage(ctx, "demo-app", "default", byFactory); !errors.As(err, &refusal) {
		t.Fatalf("a factory-key image: %v, want a refusal", err)
	}
	if err := runner.verifyProjectImage(ctx, "demo-app", "browser", byImages); err != nil {
		t.Fatalf("an images-key image: %v", err)
	}
	reads := *r.tokens
	if err := runner.verifyProjectImage(ctx, "demo-app", "browser", byImages); err != nil || *r.tokens != reads {
		t.Fatalf("again: %v, %d token reads, want none (cached)", err, *r.tokens-reads)
	}
	if err := runner.verifyProjectImage(ctx, "linkshort", "browser", byImages); err == nil {
		t.Fatal("the digest verified under another project's image")
	}
}

// An image above policy.MaxImageBytes compressed raises one alert and is
// never pinned, nor built again.
func TestImagerRefusesAnImageTooLarge(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	digest := r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 3<<30, 1<<30+1)
	r.pass(t)
	r.pass(t)
	want := []Alert{{Kind: AlertImage, Source: "size", Fingerprint: "sf/sandbox-demo-app.default@" + digest, Detail: map[string]any{
		"project": "demo-app", "image": "default", "digest": digest, "bytes": int64(4<<30 + 1), "max": int64(policy.MaxImageBytes)}}}
	if !reflect.DeepEqual(r.book.alerts, want) {
		t.Fatalf("alerts %+v, want %+v", r.book.alerts, want)
	}
	if prs, dispatches, writes := r.fj.state(); len(prs)+len(dispatches)+len(writes) != 0 {
		t.Fatalf("pull requests %+v, dispatches %q, writes %q; want none", prs, dispatches, writes)
	}
}

// brokenMaintenance breaks a process file of main the way a person's push
// might, which the loader refuses.
func brokenMaintenance(main map[string]string) {
	main["factory/processes/maintenance.yaml"] += "unknown_key: 1\n"
}

// The merge is the bot's only for a pull request that changes the project
// file's images block and nothing else, at a head whose tree passes the
// config check: a second file, another field, a tree that fails the check
// and Forgejo's own refusal, once it repeats, each leave it open and raise
// one alert naming why; main has not moved, so nothing is cut again.
func TestImagerAutoMergeRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(main map[string]string)
		tamper map[string]string
		refuse int
		code   string
		reason string
	}{
		{name: "a second file", tamper: map[string]string{"factory/projects/linkshort.yaml": "# yaml-language-server: $schema=../schema/project.schema.json\n"},
			code: "files", reason: "pull request #1 changes factory/projects/demo-app.yaml, factory/projects/linkshort.yaml; an image pin changes factory/projects/demo-app.yaml only"},
		{name: "another field", tamper: nil, code: "fields",
			reason: "pull request #1 changes factory/projects/demo-app.yaml beyond its images block"},
		{name: "a failing tree", edit: brokenMaintenance, code: "check"},
		{name: "a refused merge", refuse: 1, code: "merge"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newImagerRig(t, demoImages, nil)
			if c.edit != nil {
				r.fj.mu.Lock()
				files := maps.Clone(r.fj.commits[r.fj.main])
				c.edit(files)
				r.fj.main = r.fj.commit(files)
				r.fj.mu.Unlock()
			}
			r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
			r.pass(t)
			switch {
			case c.tamper != nil:
				r.fj.tamper("factory-pins/demo-app", c.tamper)
			case c.code == "fields":
				r.fj.tamper("factory-pins/demo-app", map[string]string{demoProjectFile: strings.Replace(r.fj.file("factory-pins/demo-app", demoProjectFile), "merge_rule: auto", "merge_rule: human", 1)})
			}
			r.fj.refuse = c.refuse
			r.pass(t)
			if first := len(r.book.alerts); first != 1 && c.code != "merge" || first != 0 && c.code == "merge" {
				t.Fatalf("after the first judgement: alerts %+v; a refusal alerts only once it repeats", r.book.alerts)
			}
			r.pass(t)
			prs, _, writes := r.fj.state()
			if len(prs) != 1 || prs[0].state != "open" || len(writes) != 3 && c.refuse == 0 || len(writes) != 5 && c.refuse != 0 {
				t.Fatalf("pull requests %+v, writes %q; want it open, unmerged", prs, writes)
			}
			if len(r.book.alerts) != 1 {
				t.Fatalf("alerts %+v, want one", r.book.alerts)
			}
			a := r.book.alerts[0]
			reason, _ := a.Detail["reason"].(string)
			if a.Kind != AlertImage || a.Source != "pin" || a.Fingerprint != "demo-app "+c.code || a.Detail["project"] != "demo-app" || a.Detail["pull_request"] != int64(1) {
				t.Fatalf("alert %+v", a)
			}
			switch c.code {
			case "check":
				if !strings.HasPrefix(reason, "the tree at the head of pull request #1 fails factoryd's config check: processes/maintenance.yaml: ") {
					t.Fatalf("reason %q", reason)
				}
			case "merge":
				if !strings.HasPrefix(reason, "Forgejo refused the bot's merge of pull request #1: ") || !strings.Contains(reason, "User not allowed to merge PR") {
					t.Fatalf("reason %q", reason)
				}
			default:
				if reason != c.reason {
					t.Fatalf("reason %q, want %q", reason, c.reason)
				}
			}
		})
	}
}

// A refusal that does not repeat, Forgejo's 405 while it checks the pull
// request again, then a 409, raises nothing: the pass after merges.
func TestImagerRetriesATransientRefusal(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	for _, refuse := range []int{1, 2, 0} {
		r.fj.refuse = refuse
		r.pass(t)
	}
	if prs, _, _ := r.fj.state(); len(prs) != 1 || !prs[0].merged || len(r.book.alerts) != 0 {
		t.Fatalf("pull requests %+v, alerts %+v; want #1 merged, none", prs, r.book.alerts)
	}
}

// A pull request whose tree fails the check, or whose merge Forgejo
// refuses, on a branch cut before main's head is closed, and the next pass
// cuts it afresh from main, where it merges: once per change of main, and
// not while main stays as it was.
func TestImagerRecutsAPullRequestMainMovedPast(t *testing.T) {
	t.Run("main fixed after a cut from a broken main", func(t *testing.T) {
		r := newImagerRig(t, demoImages, nil)
		fixed := r.fj.file("main", "factory/processes/maintenance.yaml")
		r.fj.moveMain(map[string]string{"factory/processes/maintenance.yaml": fixed + "unknown_key: 1\n"})
		r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
		r.pass(t)
		r.pass(t)
		r.pass(t)
		if prs, _, _ := r.fj.state(); len(prs) != 1 || prs[0].state != "open" || len(r.book.alerts) != 1 || r.book.alerts[0].Fingerprint != "demo-app check" {
			t.Fatalf("main still broken: pull requests %+v, alerts %+v; want #1 open, one check alert", prs, r.book.alerts)
		}
		r.fj.moveMain(map[string]string{"factory/processes/maintenance.yaml": fixed})
		r.pass(t)
		r.pass(t)
		r.pass(t)
		r.pass(t)
		prs, _, writes := r.fj.state()
		if len(prs) != 2 || prs[0].state != "closed" || prs[0].merged || !prs[1].merged || prs[1].mergedBy != "bot" {
			t.Fatalf("pull requests %+v; want #1 closed unmerged, #2 merged by the bot", prs)
		}
		want := []string{"POST sf/ai-software-factory-infra/forks by bot", "POST factory-agent/ai-software-factory-infra/contents by bot", "POST sf/ai-software-factory-infra/pulls by bot",
			"PATCH sf/ai-software-factory-infra/pulls/1 by bot", "POST sf/ai-software-factory-infra/issues/1/comments by bot",
			"DELETE factory-agent/ai-software-factory-infra/branches/factory-pins/demo-app by bot", "POST factory-agent/ai-software-factory-infra/sync_fork by bot",
			"POST factory-agent/ai-software-factory-infra/contents by bot", "POST sf/ai-software-factory-infra/pulls by bot",
			"POST sf/ai-software-factory-infra/pulls/2/merge by bot"}
		if !reflect.DeepEqual(writes, want) {
			t.Fatalf("writes %q\nwant %q", writes, want)
		}
		r.fj.mu.Lock()
		comments := slicesClone(r.fj.comments)
		r.fj.mu.Unlock()
		if len(comments) != 1 || !strings.HasPrefix(comments[0], "#1: Closed by factoryd: its branch was cut from c2, main is at c4 now, and the tree at the head of pull request #1 fails factoryd's config check: processes/maintenance.yaml: ") ||
			!strings.HasSuffix(comments[0], "; the next pass cuts it afresh from main.") {
			t.Fatalf("comments %q", comments)
		}
		if !strings.Contains(r.fj.file("main", demoProjectFile), "    key: "+keyDefault+"\n") || len(r.book.alerts) != 1 {
			t.Fatalf("main's project file:\n%s\nalerts %+v", r.fj.file("main", demoProjectFile), r.book.alerts)
		}
	})
	t.Run("a conflict after main moved", func(t *testing.T) {
		r := newImagerRig(t, demoImages, nil)
		r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
		r.pass(t)
		r.fj.moveMain(map[string]string{"factory/routines/weekly-bump.md": "a person's edit\n"})
		r.fj.refuse = 2
		r.pass(t)
		r.fj.refuse = 0
		r.pass(t)
		r.pass(t)
		prs, _, _ := r.fj.state()
		if len(prs) != 2 || prs[0].state != "closed" || prs[0].merged || !prs[1].merged || len(r.book.alerts) != 0 {
			t.Fatalf("pull requests %+v, alerts %+v; want #1 closed, #2 merged, none", prs, r.book.alerts)
		}
		if got := r.fj.file("main", "factory/routines/weekly-bump.md"); got != "a person's edit\n" {
			t.Fatalf("the person's edit is gone: %q", got)
		}
	})
}

// A pin pull request whose block main holds already, as the images file
// went back to what main pins, is closed with a comment saying so.
func TestImagerClosesAnOutdatedPullRequest(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	r.fj.mu.Lock()
	delete(r.fj.images, "demo-app/demo-app")
	r.fj.mu.Unlock()
	r.pass(t)
	r.pass(t)
	prs, _, writes := r.fj.state()
	if len(prs) != 1 || prs[0].state != "closed" || prs[0].merged {
		t.Fatalf("pull requests %+v; want #1 closed unmerged", prs)
	}
	want := []string{"POST sf/ai-software-factory-infra/forks by bot", "POST factory-agent/ai-software-factory-infra/contents by bot", "POST sf/ai-software-factory-infra/pulls by bot",
		"PATCH sf/ai-software-factory-infra/pulls/1 by bot", "POST sf/ai-software-factory-infra/issues/1/comments by bot"}
	r.fj.mu.Lock()
	comments := slicesClone(r.fj.comments)
	r.fj.mu.Unlock()
	if !reflect.DeepEqual(writes, want) || !reflect.DeepEqual(comments, []string{"#1: Closed by factoryd: main's factory/projects/demo-app.yaml is already what it would make it."}) {
		t.Fatalf("writes %q, comments %q", writes, comments)
	}
}

// A pull request from the pin branch into another base than main is no pin
// pull request: the pass opens its own into main and merges that one.
func TestImagerLeavesAPullRequestIntoAnotherBase(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	r.fj.mu.Lock()
	r.fj.fork = map[string]forkBranch{"main": {head: r.fj.main}, "factory-pins/demo-app": {head: r.fj.main, base: r.fj.main}}
	r.fj.prs = []*imagerPR{{number: 1, head: "factory-pins/demo-app", title: "a person's", base: "release", state: "open"}}
	r.fj.mu.Unlock()
	r.publish("sf/sandbox-demo-app.default", keyDefault, r.key, 1000)
	r.pass(t)
	r.pass(t)
	prs, _, _ := r.fj.state()
	if len(prs) != 2 || prs[0].state != "open" || prs[0].merged || prs[1].base != "main" || !prs[1].merged {
		t.Fatalf("pull requests %+v; want #1 untouched, #2 into main merged", prs)
	}
}

// After buildTries dispatches of one key leave no signed image, the pass
// raises one alert naming the project, image and key and that the builder
// failed, and asks again once a day from the last dispatch.
func TestImagerAlertsABuildThatNeverLands(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	t0 := *r.now
	for n := range 5 {
		*r.now = t0.Add(time.Duration(n) * 46 * time.Minute)
		r.pass(t)
		if got := len(r.book.alerts); got != 0 && n < 3 || got != 1 && n >= 3 {
			t.Fatalf("pass %d: alerts %+v", n+1, r.book.alerts)
		}
	}
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 3 {
		t.Fatalf("%d dispatches, want 3: none for a day after the third", len(dispatches))
	}
	third := t0.Add(2 * 46 * time.Minute)
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{24*time.Hour - time.Minute, 3}, {24 * time.Hour, 4}, {24*time.Hour + 46*time.Minute, 4}} {
		*r.now = third.Add(c.at)
		r.pass(t)
		if _, dispatches, _ := r.fj.state(); len(dispatches) != c.want {
			t.Fatalf("%v after the third: %d dispatches, want %d", c.at, len(dispatches), c.want)
		}
	}
	want := []Alert{{Kind: AlertImage, Source: "build", Fingerprint: "build sf/sandbox-demo-app.default:" + keyDefault, Detail: map[string]any{
		"project": "demo-app", "image": "default", "key": keyDefault, "dispatches": 3,
		"reason": "3 builds of sf/sandbox-demo-app.default:" + keyDefault + " left no image signed by the images key: the builder project-image.yml of sf/ai-software-factory-code failed, its runs say why"}}}
	if !reflect.DeepEqual(r.book.alerts, want) {
		t.Fatalf("alerts %+v\nwant %+v", r.book.alerts, want)
	}
}

// withImages replaces the top-level images block, from its key to the last
// indented line before the next top-level key, and keeps every other byte:
// the comments before, inside the next key's lead and after; with no block
// it appends one, and an empty block removes it.
func TestWithImagesKeepsEveryOtherByte(t *testing.T) {
	block := "images:\n  a:\n    digest: sha256:aa\n"
	for _, c := range []struct{ name, file, block, want string }{
		{"replaced between comments",
			"# head\nrepository: a/b\n# before\nimages:\n  old:\n    digest: sha256:00\n\n  # inside\n    key: k\n\n# after\nnotify: [x]\n# end\n", block,
			"# head\nrepository: a/b\n# before\n" + block + "\n# after\nnotify: [x]\n# end\n"},
		{"removed", "repository: a/b\nimages:\n  old: {}\nnotify: [x]\n", "", "repository: a/b\nnotify: [x]\n"},
		{"a flow block", "images: {}\nnotify: [x]\n", block, block + "notify: [x]\n"},
		{"at the end", "repository: a/b\n# before\nimages:\n  old: {}\n  # trailing\n", block, "repository: a/b\n# before\n" + block},
		{"appended", "repository: a/b\n# end\n", block, "repository: a/b\n# end\n" + block},
		{"appended after a last line without newline", "repository: a/b", block, "repository: a/b\n" + block},
		{"none to remove", "repository: a/b\n", "", "repository: a/b\n"},
		{"a nested images key stays", "chat:\n  images: x\n", block, "chat:\n  images: x\n" + block},
	} {
		if got := string(withImages([]byte(c.file), c.block)); got != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
}

// The registry's compressed size of an image is the sum of its layers': an
// index's linux/amd64 manifest's, a plain manifest's own.
func TestRegistryLayerBytes(t *testing.T) {
	manifest := `{"schemaVersion":2,"config":{"digest":"sha256:c0","size":9},"layers":[{"digest":"sha256:a","size":3},{"digest":"sha256:b","size":4}]}`
	index := strings.Replace(testIndexTemplate, "IMG", sha256Of(manifest), 1)
	reg, _ := testRegistry(t, map[string]string{"sf/x/manifests/k": index, "sf/x/manifests/" + sha256Of(manifest): manifest})
	ctx := context.Background()
	tok, err := reg.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := reg.Resolve(ctx, tok, "sf/x", "k")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := reg.LayerBytes(ctx, tok, "sf/x", m.Platform); err != nil || n != 7 {
		t.Fatalf("layer bytes %d %v, want 7", n, err)
	}
	if _, err := reg.LayerBytes(ctx, tok, "sf/x", "sha256:"+strings.Repeat("f", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing manifest: %v", err)
	}
}

// image_builds keeps the last dispatch, the count of dispatches and the
// last read of each key apart;
// an image alert is one row per fingerprint.
func TestPGStoreImageBuilds(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE image_builds; DELETE FROM alerts WHERE kind = 'image'`); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	row := func() (dispatched, read *time.Time) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT dispatched_at, read_at FROM image_builds WHERE project = 'demo' AND image = 'default' AND key = 'k-1'`).Scan(&dispatched, &read); err != nil {
			t.Fatal(err)
		}
		return dispatched, read
	}
	if at, n, err := s.ImageDispatches(ctx, "demo", "default", "k-1"); err != nil || !at.IsZero() || n != 0 {
		t.Fatalf("no row: %v %d %v", at, n, err)
	}
	if err := s.RecordImageRead(ctx, "demo", "default", "k-1", t0); err != nil {
		t.Fatal(err)
	}
	if at, n, err := s.ImageDispatches(ctx, "demo", "default", "k-1"); err != nil || !at.IsZero() || n != 0 {
		t.Fatalf("read, not dispatched: %v %d %v", at, n, err)
	}
	for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(2 * time.Minute)} {
		if err := s.RecordImageDispatch(ctx, "demo", "default", "k-1", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordImageRead(ctx, "demo", "default", "k-1", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if at, n, err := s.ImageDispatches(ctx, "demo", "default", "k-1"); err != nil || !at.Equal(t0.Add(2*time.Minute)) || n != 2 {
		t.Fatalf("dispatched %v %d times %v, want the last, twice", at, n, err)
	}
	if d, r := row(); !d.Equal(t0.Add(2*time.Minute)) || !r.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("row %v %v", d, r)
	}
	if at, n, err := s.ImageDispatches(ctx, "demo", "default", "k-2"); err != nil || !at.IsZero() || n != 0 {
		t.Fatalf("another key: %v %d %v", at, n, err)
	}
	// a key whose build landed forgets its dispatches, and keeps when the last was
	if err := s.ResetImageDispatches(ctx, "demo", "default", "k-1"); err != nil {
		t.Fatal(err)
	}
	if at, n, err := s.ImageDispatches(ctx, "demo", "default", "k-1"); err != nil || !at.Equal(t0.Add(2*time.Minute)) || n != 0 {
		t.Fatalf("after the reset: %v %d %v, want the last dispatch and none counted", at, n, err)
	}
	if err := s.ResetImageDispatches(ctx, "demo", "default", "k-9"); err != nil {
		t.Fatalf("a reset of a key with no row: %v", err)
	}
	for i, fp := range []string{"a", "a", "b"} {
		id, err := s.InsertAlert(ctx, Alert{Kind: AlertImage, Source: "file", Fingerprint: fp})
		if err != nil || (id == 0) != (i == 1) {
			t.Fatalf("alert %d (%s): id %d %v, want one row per fingerprint", i, fp, id, err)
		}
	}
}

// After a pin merges and comes in force, the next pass keeps of the image's
// package exactly the pinned build and the build pinned before it, each
// with cosign's signature, and deletes every other version, once: the pass
// after lists nothing. The pin first seen deletes nothing, and a package
// whose name the image's starts stays whole.
func TestImagerKeepsThePinnedAndThePreviousBuild(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	const repo = "sf/sandbox-demo-app.default"
	keyOld, keyAncient, keyUnsigned := "k-"+strings.Repeat("1", 24), "k-"+strings.Repeat("0", 24), "k-"+strings.Repeat("2", 24)
	_, ancient := r.build(repo, keyAncient, r.key, 800)
	digestOld, old := r.build(repo, keyOld, r.key, 900)
	_, unsigned := r.build(repo, keyUnsigned, nil, 950)
	digestNew, current := r.build(repo, keyDefault, r.key, 1000)
	if len(ancient) != 4 || len(old) != 4 || len(unsigned) != 2 || len(current) != 4 {
		t.Fatalf("builds: %q %q %q %q", ancient, old, unsigned, current)
	}
	r.fj.moveMain(map[string]string{demoProjectFile: r.fj.file("main", demoProjectFile) +
		"images:\n  default:\n    digest: " + digestOld + "\n    key: " + keyOld + "\n    tools: [jq]\n"})
	r.fj.load(t)
	all := slices.Sorted(slices.Values(slices.Concat(ancient, old, unsigned, current)))
	r.fj.mu.Lock()
	r.fj.packages["sandbox-demo-app.default"] = slices.Clone(all)
	r.fj.packages["sandbox-demo-app.default2"] = []string{keyAncient}
	r.fj.mu.Unlock()

	r.pass(t)
	r.pass(t)
	if prs, _, _ := r.fj.state(); len(prs) != 1 || !prs[0].merged {
		t.Fatalf("pull requests %+v; want the pin merged", prs)
	}
	if got, lists := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, all) || lists != 0 {
		t.Fatalf("before the merged pin is in force: versions %q, %d lists; want all, none", got, lists)
	}
	r.fj.load(t)
	r.pass(t)
	if got, _ := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, slices.Sorted(slices.Values(slices.Concat(old, current)))) {
		t.Fatalf("versions kept %q\nwant the pinned build's %q and the previous one's %q", got, current, old)
	}
	if got, _ := r.fj.versions("sandbox-demo-app.default2"); !slices.Equal(got, []string{keyAncient}) {
		t.Fatalf("another package lost versions: %q", got)
	}
	want := ImagePinRow{Project: "demo-app", Image: "default", Key: keyDefault, Digest: digestNew, PreviousKey: keyOld, PreviousDigest: digestOld,
		Since: *r.now, Retained: true}
	if got := r.book.pins["demo-app/default"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v\nwant %+v", got, want)
	}
	_, lists := r.fj.versions("")
	r.pass(t)
	if _, again := r.fj.versions(""); again != lists {
		t.Fatalf("a pass with the pin unchanged listed the store again")
	}
}

// A name that leaves the pins in force keeps its last pin's build for 30
// days after the unpin is first seen, its other builds going at once, then
// loses every version and its row; the name still pinned keeps its package,
// and a project absent from the version in force is remove-project's.
func TestImagerKeepsAnUnpinnedImagesLastBuildFor30Days(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	const repo = "sf/sandbox-demo-app.browser"
	_, older := r.build(repo, "k-"+strings.Repeat("4", 24), r.key, 700)
	digestBrowser, last := r.build(repo, keyBrowser, r.key, 750)
	r.fj.moveMain(map[string]string{demoProjectFile: strings.Replace(r.fj.file("main", demoProjectFile), "sha256:"+strings.Repeat("b", 64), digestBrowser, 1)})
	r.fj.load(t)
	gone := ImagePinRow{Project: "gone-app", Image: "default", Key: keyDefault, Digest: "sha256:" + strings.Repeat("9", 64), Since: r.now.Add(-time.Hour)}
	r.book.pins["gone-app/default"] = gone
	r.fj.mu.Lock()
	r.fj.packages["sandbox-demo-app.browser"] = slices.Concat(older, last)
	r.fj.packages["sandbox-demo-app.default"] = []string{keyDefault}
	r.fj.packages["sandbox-gone-app.default"] = []string{keyDefault}
	r.fj.images["demo-app/demo-app"] = demoImages
	r.fj.mu.Unlock()
	r.pass(t)
	r.pass(t)
	if got, lists := r.fj.versions("sandbox-demo-app.browser"); len(got) != 8 || lists != 0 {
		t.Fatalf("before the unpin is in force: versions %q, %d lists", got, lists)
	}
	r.fj.load(t)
	t0 := *r.now
	r.pass(t)
	if got, _ := r.fj.versions("sandbox-demo-app.browser"); !slices.Equal(got, last) {
		t.Fatalf("the unpinned image keeps %q; want its last build %q", got, last)
	}
	if row := r.book.pins["demo-app/browser"]; !row.UnpinnedAt.Equal(t0) || !row.Retained || row.Key != keyBrowser {
		t.Fatalf("the unpinned row %+v", row)
	}
	*r.now = t0.Add(30*24*time.Hour - time.Minute)
	r.pass(t)
	if got, _ := r.fj.versions("sandbox-demo-app.browser"); !slices.Equal(got, last) {
		t.Fatalf("29 days on, the unpinned image keeps %q", got)
	}
	*r.now = t0.Add(30 * 24 * time.Hour)
	r.pass(t)
	if got, _ := r.fj.versions("sandbox-demo-app.browser"); len(got) != 0 {
		t.Fatalf("30 days on, the unpinned image keeps %q", got)
	}
	if _, ok := r.book.pins["demo-app/browser"]; ok || r.book.pins["demo-app/default"].Key != keyDefault {
		t.Fatalf("rows %+v; want browser's gone", r.book.pins)
	}
	if got, _ := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, []string{keyDefault}) {
		t.Fatalf("the pinned image keeps %q", got)
	}
	if got, _ := r.fj.versions("sandbox-gone-app.default"); !slices.Equal(got, []string{keyDefault}) || !reflect.DeepEqual(r.book.pins["gone-app/default"], gone) {
		t.Fatalf("a project absent from the version in force: versions %q, row %+v", got, r.book.pins["gone-app/default"])
	}
}

// A sweep deletes only while infra's main is the commit in force: a pin
// merged there and not loaded yet, whose key the declaration no longer
// desires, keeps its build through the retry of a sweep that failed, no
// sweep lists the store while main is past the commit, and once loaded it
// is the pin the sweep keeps.
func TestImagerSweepsOnlyAtTheCommitInForce(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	const repo = "sf/sandbox-demo-app.default"
	keyOld, keyMid := "k-"+strings.Repeat("1", 24), "k-"+strings.Repeat("2", 24)
	digestOld, old := r.build(repo, keyOld, r.key, 900)
	digestMid, mid := r.build(repo, keyMid, r.key, 950)
	digestNew, merged := r.build(repo, keyDefault, r.key, 1000)
	r.fj.moveMain(map[string]string{demoProjectFile: r.fj.file("main", demoProjectFile) +
		"images:\n  default:\n    digest: " + digestMid + "\n    key: " + keyMid + "\n    tools: [jq]\n"})
	r.fj.load(t)
	r.book.pins["demo-app/default"] = ImagePinRow{Project: "demo-app", Image: "default", Key: keyMid, Digest: digestMid,
		PreviousKey: keyOld, PreviousDigest: digestOld, Since: *r.now}
	all := slices.Sorted(slices.Values(slices.Concat(old, mid, merged)))
	r.fj.mu.Lock()
	r.fj.packages["sandbox-demo-app.default"] = slices.Clone(all)
	r.fj.failLists = 1
	r.fj.mu.Unlock()
	if err := r.im.pass(context.Background()); err == nil || !strings.Contains(err.Error(), "retention: image default of demo-app: ") {
		t.Fatalf("pass 1: %v; want the failed sweep", err)
	}
	r.pass(t)
	if prs, _, _ := r.fj.state(); len(prs) != 1 || !prs[0].merged {
		t.Fatalf("pull requests %+v; want the pin of %s merged", prs, keyDefault)
	}
	r.fj.mu.Lock()
	r.fj.images["demo-app/demo-app"] = "images:\n  default:\n    apt: [jq, make]\n"
	r.fj.mu.Unlock()
	r.pass(t)
	if got, lists := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, all) || lists != 1 || r.book.pins["demo-app/default"].Retained {
		t.Fatalf("main past the commit in force: versions %q, %d lists, row %+v; want all, the failed list alone, the sweep due", got, lists, r.book.pins["demo-app/default"])
	}
	r.fj.load(t)
	r.pass(t)
	if got, _ := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, slices.Sorted(slices.Values(slices.Concat(mid, merged)))) {
		t.Fatalf("versions kept %q; want the merged pin's and the one before", got)
	}
	if row := r.book.pins["demo-app/default"]; row.Key != keyDefault || row.Digest != digestNew || row.PreviousKey != keyMid || !row.Retained {
		t.Fatalf("row %+v", row)
	}
}

// A sweep reads main's head before it lists the store and again, with the
// version in force, right before its deletes: once a person merged on
// main, or the version in force is no longer the one whose pins it keeps,
// it deletes nothing, no pass lists the store while main is past the
// commit in force, and a pass at the commit in force sweeps again.
func TestImagerSweepStopsWhenMainOrTheVersionMoves(t *testing.T) {
	for _, moves := range []string{"version", "main"} {
		t.Run(moves, func(t *testing.T) {
			r := newImagerRig(t, demoImages, nil)
			const repo = "sf/sandbox-demo-app.default"
			_, stale := r.build(repo, "k-"+strings.Repeat("1", 24), r.key, 900)
			digest, current := r.build(repo, keyDefault, r.key, 1000)
			r.fj.moveMain(map[string]string{demoProjectFile: r.fj.file("main", demoProjectFile) +
				"images:\n  default:\n    digest: " + digest + "\n    key: " + keyDefault + "\n    tools: [jq]\n"})
			r.fj.load(t)
			r.book.pins["demo-app/default"] = ImagePinRow{Project: "demo-app", Image: "default", Key: keyDefault, Digest: digest,
				PreviousKey: "k-" + strings.Repeat("3", 24), PreviousDigest: "sha256:" + strings.Repeat("3", 64), Since: *r.now}
			all := slices.Sorted(slices.Values(slices.Concat(stale, current)))
			r.fj.mu.Lock()
			r.fj.packages["sandbox-demo-app.default"] = slices.Clone(all)
			r.fj.bumpOnList, r.fj.moveOnList = moves == "version", moves == "main"
			r.fj.mu.Unlock()
			r.pass(t)
			if got, lists := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, all) || lists != 1 || r.book.pins["demo-app/default"].Retained {
				t.Fatalf("versions %q, %d lists, row %+v; want all, one list, the sweep due", got, lists, r.book.pins["demo-app/default"])
			}
			r.fj.mu.Lock()
			r.fj.bumpOnList, r.fj.moveOnList = false, false
			r.fj.mu.Unlock()
			if moves == "main" {
				r.pass(t)
				if got, lists := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, all) || lists != 1 {
					t.Fatalf("main past the commit in force: versions %q, %d lists; want all, no list", got, lists)
				}
				r.fj.load(t)
			}
			r.pass(t)
			if got, _ := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, current) || !r.book.pins["demo-app/default"].Retained {
				t.Fatalf("at the commit in force: versions %q, row %+v; want %q", got, r.book.pins["demo-app/default"], current)
			}
		})
	}
}

// A sweep keeps the build of each image's desired key, pinned or not: the
// build an open pin pull request carries survives the sweep of the pins
// before it, and so does a version pushed within dispatchWait, a build in
// flight.
func TestImagerKeepsTheBuildAPinPullRequestCarries(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	const repo = "sf/sandbox-demo-app.default"
	keyOld, keyMid := "k-"+strings.Repeat("1", 24), "k-"+strings.Repeat("2", 24)
	digestOld, old := r.build(repo, keyOld, r.key, 900)
	digestMid, mid := r.build(repo, keyMid, r.key, 950)
	_, stale := r.build(repo, "k-"+strings.Repeat("5", 24), r.key, 850)
	_, current := r.build(repo, keyDefault, r.key, 1000)
	r.fj.moveMain(map[string]string{demoProjectFile: r.fj.file("main", demoProjectFile) +
		"images:\n  default:\n    digest: " + digestMid + "\n    key: " + keyMid + "\n    tools: [jq]\n"})
	r.fj.load(t)
	r.book.pins["demo-app/default"] = ImagePinRow{Project: "demo-app", Image: "default", Key: keyMid, Digest: digestMid,
		PreviousKey: keyOld, PreviousDigest: digestOld, Since: *r.now}
	inFlight := "sha256:" + strings.Repeat("7", 64)
	r.fj.mu.Lock()
	r.fj.packages["sandbox-demo-app.default"] = slices.Concat(old, mid, stale, current, []string{inFlight})
	r.fj.created = map[string]string{inFlight: r.now.Add(-time.Minute).Format(time.RFC3339)}
	r.fj.mu.Unlock()
	r.pass(t)
	if prs, _, _ := r.fj.state(); len(prs) != 1 || prs[0].state != "open" {
		t.Fatalf("pull requests %+v; want the pin of %s open", prs, keyDefault)
	}
	if got, _ := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, slices.Sorted(slices.Values(slices.Concat(old, mid, current, []string{inFlight})))) {
		t.Fatalf("versions %q; want the pins', the pull request's and the one in flight", got)
	}
	r.pass(t)
	r.fj.load(t)
	r.pass(t)
	if got, _ := r.fj.versions("sandbox-demo-app.default"); !slices.Equal(got, slices.Sorted(slices.Values(slices.Concat(mid, current, []string{inFlight})))) {
		t.Fatalf("once the pull request's pin is in force: versions %q", got)
	}
}

// pathLog carries requests and records their paths.
type pathLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *pathLog) RoundTrip(req *http.Request) (*http.Response, error) {
	l.mu.Lock()
	l.paths = append(l.paths, req.URL.Path)
	l.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

// An image unpinned and then pinned again at its last build is pinned
// anew: its unpin and removal card cleared, its 30 days started again, and
// the sweep keeps that build, asking the registry of no pin before it, as
// there was none.
func TestImagerRepinsAnUnpinnedImage(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	const repo = "sf/sandbox-demo-app.browser"
	digestBrowser, last := r.build(repo, keyBrowser, r.key, 750)
	r.fj.moveMain(map[string]string{demoProjectFile: strings.Replace(r.fj.file("main", demoProjectFile), "sha256:"+strings.Repeat("b", 64), digestBrowser, 1)})
	r.fj.load(t)
	declare := func(images string) {
		r.fj.mu.Lock()
		r.fj.images["demo-app/demo-app"] = images
		r.fj.mu.Unlock()
	}
	r.fj.mu.Lock()
	r.fj.packages["sandbox-demo-app.browser"] = slices.Clone(last)
	r.fj.mu.Unlock()
	declare(demoImages)
	r.pass(t)
	r.pass(t)
	r.fj.load(t)
	r.pass(t)
	row := r.book.pins["demo-app/browser"]
	if row.UnpinnedAt.IsZero() {
		t.Fatalf("row %+v; want the unpin seen", row)
	}
	row.RemovalTask = "t-removal"
	r.book.pins["demo-app/browser"] = row
	declare(bothImages)
	r.pass(t)
	r.pass(t)
	if prs, _, _ := r.fj.state(); len(prs) != 2 || !prs[1].merged {
		t.Fatalf("pull requests %+v; want the second, pinning browser again, merged", prs)
	}
	r.fj.load(t)
	reads := &pathLog{}
	r.im.Registry.HTTP = &http.Client{Transport: reads}
	*r.now = r.now.Add(time.Hour)
	r.pass(t)
	want := ImagePinRow{Project: "demo-app", Image: "browser", Key: keyBrowser, Digest: digestBrowser, Since: *r.now, Retained: true}
	if got := r.book.pins["demo-app/browser"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v\nwant %+v", got, want)
	}
	if got, _ := r.fj.versions("sandbox-demo-app.browser"); !slices.Equal(got, last) {
		t.Fatalf("versions %q; want %q", got, last)
	}
	for _, p := range reads.paths {
		if strings.HasSuffix(p, "/manifests/") || strings.HasSuffix(p, "/manifests/sha256-") {
			t.Fatalf("the sweep asked the registry for %s", p)
		}
	}
	if len(reads.paths) == 0 {
		t.Fatal("the sweep read no registry")
	}
}

// An image but default whose name has been pinned for 30 days, which no
// card named or ran on in those 30 days, gets one card on its project's
// repository that removes it. The check runs once a day, failed or not,
// and a restart of factoryd does not run it sooner. An open card, or one
// finished within 30 days (merged: the unpin lands meanwhile; declined:
// the 30 days start again), files no other.
func TestImagerFilesOneRemovalCardForAnUnusedImage(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	t0, day := *r.now, 24*time.Hour
	r.book.pins["demo-app/default"] = ImagePinRow{Project: "demo-app", Image: "default", Key: keyDefault, Digest: "sha256:" + strings.Repeat("d", 64),
		Since: t0.Add(-60 * day), Retained: true}
	r.book.pins["demo-app/browser"] = ImagePinRow{Project: "demo-app", Image: "browser", Key: keyBrowser, Digest: "sha256:" + strings.Repeat("b", 64),
		Since: t0.Add(-29 * day), Retained: true}
	at := func(d time.Duration) error {
		*r.now = t0.Add(d)
		return r.im.pass(context.Background())
	}
	issues := func() []string {
		r.fj.mu.Lock()
		defer r.fj.mu.Unlock()
		return slicesClone(r.fj.issues)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
	}
	must(at(0))
	r.book.usedErr = errors.New("the store is down")
	if err := at(25 * time.Hour); err == nil || !strings.Contains(err.Error(), "the store is down") {
		t.Fatalf("pass = %v; want the failed check", err)
	}
	r.book.usedErr = nil
	old := r.im
	r.im = &Imager{Forgejo: old.Forgejo, Config: old.Config, Policy: old.Policy, Store: old.Store, Registry: old.Registry, Key: old.Key, Base: old.Base,
		Builder: old.Builder, BuildToken: old.BuildToken, Ladder: old.Ladder, Now: old.Now, Workflows: old.Workflows, OpenBao: old.OpenBao}
	must(at(26 * time.Hour))
	r.book.used["sf/sandbox-demo-app.browser"] = true
	must(at(50 * time.Hour))
	if got := issues(); len(got) != 0 || len(r.book.asked) != 2 {
		t.Fatalf("issues %q, asked %q; want none, the checks of 25 h and 50 h", got, r.book.asked)
	}

	r.book.used["sf/sandbox-demo-app.browser"] = false
	must(at(75 * time.Hour))
	title := "Remove image browser (unused for 30 days)"
	body := "Image browser of project demo-app has been pinned for 30 days, and no card ran on it in that time.\n\n" +
		"Delete the browser entry from factory/images.yaml and change nothing else; where it is the file's only entry, delete the file, as an images file declares one image at least. " +
		"After this card merges, the factory unpins the image, keeps its last build in the store for 30 days, then deletes it.\n\n" +
		"Acceptance:\n- factory/images.yaml no longer declares image browser.\n- factory/images.yaml passes the factory's check, which the policy gate runs on this pull request.\n\n" +
		"Size: small\nRung: cheap"
	if got := issues(); !slices.Equal(got, []string{title + "\n\n" + body}) {
		t.Fatalf("issues %q", got)
	}
	card := r.book.tasks[r.book.pins["demo-app/browser"].RemovalTask]
	if card.Source != SourceImageRemoval || card.Repository != "demo-app/demo-app" || card.IssueNumber != 101 || card.Status != "queued" ||
		card.Objective != title+"\n\n"+body || card.Parent != "" || card.Rung != RungCheap || card.Size != "small" || card.BudgetEUR <= 0 {
		t.Fatalf("the card %+v", card)
	}

	must(at(76 * time.Hour))
	must(at(100 * time.Hour))
	declined := t0.Add(100 * time.Hour)
	card.Status, card.FinishedAt = "closed", &declined
	r.book.tasks[card.ID] = card
	must(at(125 * time.Hour))
	if got := issues(); len(got) != 1 {
		t.Fatalf("with the card open, then declined a day ago: issues %q", got)
	}
	must(at(100*time.Hour + 30*day))
	second := r.book.tasks[r.book.pins["demo-app/browser"].RemovalTask]
	if got := issues(); len(got) != 2 || second.ID == card.ID {
		t.Fatalf("30 days after the decline: issues %q, row %+v", got, r.book.pins["demo-app/browser"])
	}
	merged := t0.Add(100*time.Hour + 30*day)
	second.Status, second.FinishedAt = "merged", &merged
	r.book.tasks[second.ID] = second
	must(at(125*time.Hour + 30*day))
	asked := []string{
		"demo-app/demo-app browser sf/sandbox-demo-app.browser since 2026-09-23T13:00:00Z",
		"demo-app/demo-app browser sf/sandbox-demo-app.browser since 2026-09-24T14:00:00Z",
		"demo-app/demo-app browser sf/sandbox-demo-app.browser since 2026-09-25T15:00:00Z",
		"demo-app/demo-app browser sf/sandbox-demo-app.browser since 2026-10-26T16:00:00Z",
	}
	if got := issues(); len(got) != 2 || !slices.Equal(r.book.asked, asked) {
		t.Fatalf("with the second card merged: issues %q, asked %q; want 2, %q", got, r.book.asked, asked)
	}
}

// The removal card's id is on the image's row before the card is filed: a
// filing that failed files at the next day's check under that id, once.
func TestImagerFilesTheRemovalCardUnderItsRowsID(t *testing.T) {
	r := newImagerRig(t, bothImages, demoBothPinned)
	t0, day := *r.now, 24*time.Hour
	r.book.pins["demo-app/browser"] = ImagePinRow{Project: "demo-app", Image: "browser", Key: keyBrowser, Digest: "sha256:" + strings.Repeat("b", 64),
		Since: t0.Add(-31 * day), Retained: true}
	r.fj.mu.Lock()
	r.fj.failIssues = 1
	r.fj.mu.Unlock()
	if err := r.im.pass(context.Background()); err == nil || !strings.Contains(err.Error(), "unused images: image browser of demo-app: ") {
		t.Fatalf("pass = %v; want the failed filing", err)
	}
	id := r.book.pins["demo-app/browser"].RemovalTask
	if id == "" || len(r.book.tasks) != 0 {
		t.Fatalf("row %+v, cards %+v; want the card's id on the row and no card", r.book.pins["demo-app/browser"], r.book.tasks)
	}
	*r.now = t0.Add(day - time.Minute)
	r.pass(t)
	*r.now = t0.Add(day)
	r.pass(t)
	r.fj.mu.Lock()
	issues := slicesClone(r.fj.issues)
	r.fj.mu.Unlock()
	if card := r.book.tasks[id]; card.Source != SourceImageRemoval || len(r.book.tasks) != 1 || len(issues) != 1 || r.book.pins["demo-app/browser"].RemovalTask != id {
		t.Fatalf("cards %+v, issues %d, row %+v; want the one card under %s", r.book.tasks, len(issues), r.book.pins["demo-app/browser"], id)
	}
}

// image_pins keeps one row per project image, replaced whole, and
// DeleteProjectImages drops a project's image_pins and image_builds rows.
// ImageUsed finds a card of the project's repository that names the image
// or ran on a build of it, unfinished however old or finished since the
// time asked, and none that names another image or is another
// repository's.
func TestPGStoreImagePins(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE image_pins; TRUNCATE image_builds`); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rows := func() []ImagePinRow {
		t.Helper()
		got, err := s.ImagePins(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			got[i].Since = got[i].Since.UTC()
			if !got[i].UnpinnedAt.IsZero() {
				got[i].UnpinnedAt = got[i].UnpinnedAt.UTC()
			}
			if !got[i].CheckedAt.IsZero() {
				got[i].CheckedAt = got[i].CheckedAt.UTC()
			}
		}
		return got
	}
	def := ImagePinRow{Project: "pins", Image: "default", Key: "k-1", Digest: "sha256:1", Since: t0, Retained: true}
	browser := ImagePinRow{Project: "pins", Image: "browser", Key: "k-2", Digest: "sha256:2", Since: t0.Add(time.Hour)}
	other := ImagePinRow{Project: "other", Image: "default", Key: "k-9", Digest: "sha256:9", Since: t0}
	for _, r := range []ImagePinRow{def, browser, other} {
		if err := s.SaveImagePin(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if got := rows(); !reflect.DeepEqual(got, []ImagePinRow{other, browser, def}) {
		t.Fatalf("rows %+v", got)
	}
	changed := ImagePinRow{Project: "pins", Image: "default", Key: "k-3", Digest: "sha256:3", PreviousKey: "k-1", PreviousDigest: "sha256:1",
		Since: t0, UnpinnedAt: t0.Add(2 * time.Hour), RemovalTask: "t-remove", CheckedAt: t0.Add(3 * time.Hour)}
	if err := s.SaveImagePin(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteImagePin(ctx, "pins", "browser"); err != nil {
		t.Fatal(err)
	}
	if got := rows(); !reflect.DeepEqual(got, []ImagePinRow{other, changed}) {
		t.Fatalf("rows %+v", got)
	}
	if err := s.RecordImageRead(ctx, "pins", "default", "k-3", t0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteProjectImages(ctx, "pins"); err != nil || n != 2 {
		t.Fatalf("deleted %d image rows, %v; want the pin's and the build's", n, err)
	}
	if got := rows(); !reflect.DeepEqual(got, []ImagePinRow{other}) {
		t.Fatalf("rows after the project's went %+v", got)
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	repo := "used" + suffix + "/app"
	store := func(image string) string { return "sf/sandbox-used" + suffix + "." + image }
	now := time.Now()
	since := now.Add(-30 * 24 * time.Hour)
	card := func(id, repository, image, ranOn, status string, finished *time.Time, created time.Time) {
		t.Helper()
		id += "-" + suffix
		if err := s.CreateTask(ctx, Task{ID: id, Status: "running", Repository: repository, Objective: id, Image: image, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
		if ranOn != "" {
			if err := s.SetTaskImage(ctx, id, "git.example.org/"+ranOn+"@sha256:"+strings.Repeat("a", 64), nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = $2, finished_at = $3, created_at = $4 WHERE id = $1`, id, status, finished, created); err != nil {
			t.Fatal(err)
		}
	}
	used := func(image string) bool {
		t.Helper()
		ok, err := s.ImageUsed(ctx, repo, image, store(image), since)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	longAgo, lately := now.Add(-31*24*time.Hour), now.Add(-29*24*time.Hour)
	card("t-old", repo, "browser", store("browser"), "done", &longAgo, longAgo)
	card("t-longer", repo, "browser2", store("browser2"), "running", nil, now)
	card("t-elsewhere", "other"+suffix+"/app", "browser", store("browser"), "running", nil, now)
	if used("browser") {
		t.Fatal("a card finished 31 days ago, one on another image or one of another repository counts")
	}
	card("t-lately", repo, "browser", store("browser"), "failed", &lately, longAgo)
	card("t-plan", repo, "video", "", "waiting-deps", nil, now.Add(-40*24*time.Hour))
	if !used("browser") || !used("video") {
		t.Fatalf("a card finished 29 days ago: %v; a plan card naming the image, waiting since 40 days: %v", used("browser"), used("video"))
	}
}
