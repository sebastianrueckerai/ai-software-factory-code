package factoryd

import (
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const (
	imagerBase = "git.aisoftwarefactory.world/sf/sandbox-base@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	// printf '{"Apt":["jq"],"Upstream":null,"Env":null,"Path":null,"Egress":null}\n<the base digest>\n1' | sha256sum
	keyDefault = "k-e8e539498d5e035a1e0dea2d"
	// the same over {"Apt":["chromium"],"Upstream":null,"Env":null,"Path":null,"Egress":["cdn.example.org"]}
	keyBrowser      = "k-cfdd7cd93002923a3d192ddd"
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
		case by == "ci":
			if r.Method != http.MethodPost || p != codeAPI+"/actions/workflows/project-image.yml/dispatches" {
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
	s.inForce = policy.Version{Commit: s.main, Tree: tree}
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
	removed []string // "task on dependency" of every dependency removed
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

func (b *imageBook) RemoveTaskDeps(_ context.Context, id string, dependsOn []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range dependsOn {
		b.removed = append(b.removed, id+" on "+d)
	}
	return nil
}

func (b *imageBook) Task(_ context.Context, id string) (Task, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	return t, ok, nil
}

// imageSignals records the imager's signals as "<task> <signal> <image>:
// <refusal>".
type imageSignals struct {
	mu  sync.Mutex
	got []string
}

func (w *imageSignals) Start(context.Context, string) error                    { return nil }
func (w *imageSignals) StartRoutineRun(context.Context, RoutineRunInput) error { return nil }
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
// factory/images.yaml images (none for ""), an empty registry and the
// imager as factoryd wires it.
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
	fj := &imagerForgejo{images: map[string]string{}, commits: map[string]map[string]string{}}
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
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	book := &imageBook{builds: map[string]imageBuild{}, tasks: map[string]Task{}}
	signals := &imageSignals{}
	im := &Imager{Forgejo: f, Config: config, Policy: fj.version, Store: book, Registry: reg, Key: &key.PublicKey, Base: imagerBase,
		Builder: Repo{Owner: "sf", Name: "ai-software-factory-code"}, BuildToken: "ci-tok", Ladder: imagerLadder, Now: func() time.Time { return now },
		Workflows: signals}
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
	r.registry[repository+"/manifests/"+sha256Of(manifest)] = manifest
	if signer != nil {
		maps.Copy(r.registry, cosignSigned(signer, repository, sha256Of(index)))
	}
	return sha256Of(index)
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
// each with its refusal, which drops the card's dependency on the tool
// card. A woken park is not signalled again, and the park of a card that
// ended is not read.
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
	if want := []string{"t-d on t-tool-d", "t-e on t-tool-e"}; !reflect.DeepEqual(r.book.removed, want) {
		t.Fatalf("dependencies removed %q, want %q: a refused card waits on its tool card no more", r.book.removed, want)
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
			if a.Kind != AlertImage || a.Source != "pin" || a.Fingerprint != "demo-app #1 "+c.code || a.Detail["project"] != "demo-app" || a.Detail["pull_request"] != int64(1) {
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
		if prs, _, _ := r.fj.state(); len(prs) != 1 || prs[0].state != "open" || len(r.book.alerts) != 1 || r.book.alerts[0].Fingerprint != "demo-app #1 check" {
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
// failed, and keeps asking.
func TestImagerAlertsABuildThatNeverLands(t *testing.T) {
	r := newImagerRig(t, demoImages, nil)
	t0 := *r.now
	for n := range 5 {
		*r.now = t0.Add(time.Duration(n) * 46 * time.Minute)
		r.pass(t)
		if got := len(r.book.alerts); got != 0 && n < 3 || got != 1 && n >= 3 {
			t.Fatalf("dispatch %d: alerts %+v", n+1, r.book.alerts)
		}
	}
	if _, dispatches, _ := r.fj.state(); len(dispatches) != 5 {
		t.Fatalf("%d dispatches, want 5", len(dispatches))
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
	for i, fp := range []string{"a", "a", "b"} {
		id, err := s.InsertAlert(ctx, Alert{Kind: AlertImage, Source: "file", Fingerprint: fp})
		if err != nil || (id == 0) != (i == 1) {
			t.Fatalf("alert %d (%s): id %d %v, want one row per fingerprint", i, fp, id, err)
		}
	}
}
