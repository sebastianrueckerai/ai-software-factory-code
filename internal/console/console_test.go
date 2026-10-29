package console

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// htmxSHA256 pins the vendored htmx 2.0.11 (npm htmx.org-2.0.11.tgz,
// dist/htmx.min.js, integrity sha512-Thx/WtpeOQqSrqBCw/A1cwGJGg4U...).
const htmxSHA256 = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"

func TestVendoredHTMXIsPinned(t *testing.T) {
	b, err := staticFS.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != htmxSHA256 {
		t.Fatalf("static/htmx.min.js sha256 = %x, want %s", sum, htmxSHA256)
	}
}

// fakeForgejo is the OAuth2 provider and the API, recording what it was asked.
type fakeForgejo struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	user      string
	challenge string // from the authorize URL the test followed
	grants    []url.Values
	calls     []string // "METHOD path auth"
	issues    []map[string]any
	comments  map[string]string // path -> body
	expiresIn int
	refuse    bool            // token endpoint answers 400
	spent     map[string]bool // refresh tokens used once: Forgejo invalidates them on use
	tokenN    int
	failAPI   bool

	reviews    []map[string]string
	failReview bool
	closed     []string // "path state"
	labelled   []string // "path [label ids]"
	commentN   int64    // forges a fresh comment id per POST, as Forgejo does
	deleted    []string // DELETE paths
	patched    []string // "path {body}" per PATCH of an issue
	assets     []asset  // multipart uploads to an issue's or a comment's assets
	failAssets bool     // asset uploads answer 500

	statuses map[string]string // commit sha -> its combined status as Forgejo answers it
	pullHead string            // PR 9's current head; empty answers no pull read
	// posted are the comment bodies per path, in the order they came
	posted map[string][]string
	// scene is a lab scene's repository, answered after the rig's own:
	// its labels are labLabels, and file numbers an issue filed on it and
	// makes it a card
	scene string
	file  func(issue map[string]any) int64
}

// asset is an upload as Forgejo's asset endpoints read it: the query's name
// and the file of the form field attachment.
type asset struct {
	path, name, field, file string
	data                    []byte
}

func newFakeForgejo(t *testing.T) *fakeForgejo {
	f := &fakeForgejo{t: t, user: "sebastian", comments: map[string]string{}, posted: map[string][]string{}, expiresIn: 3600, spent: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// commentBodies are the bodies posted as comments on path, in order.
func (f *fakeForgejo) commentBodies(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posted[path])
}

func (f *fakeForgejo) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/login/oauth/access_token" {
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("token form: %v", err)
		}
		f.grants = append(f.grants, r.PostForm)
		if f.refuse {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if rt := r.PostForm.Get("refresh_token"); r.PostForm.Get("grant_type") == "refresh_token" {
			if f.spent[rt] {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			f.spent[rt] = true
		}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				http.Error(w, `{"error":"unauthorized_client","error_description":"failed PKCE code challenge"}`, http.StatusBadRequest)
				return
			}
		}
		f.tokenN++
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", f.tokenN),
			"refresh_token": fmt.Sprintf("refresh-%d", f.tokenN), "token_type": "bearer", "expires_in": f.expiresIn})
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	if f.failAPI {
		http.Error(w, "boom", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		json.NewEncoder(w).Encode(map[string]any{"login": f.user})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/labels":
		// two a page, as a Forgejo whose MAX_RESPONSE_ITEMS is below the
		// limit asked for; an empty page ends the list
		pages := map[string][]factoryd.RepoLabel{
			"1": {{ID: 1, Name: "agent"}, {ID: 2, Name: "human"}},
			"2": {{ID: 7, Name: "priority/p1"}, {ID: 9, Name: "state/done"}},
			"3": {{ID: 3, Name: "backlog"}},
		}
		json.NewEncoder(w).Encode(append([]factoryd.RepoLabel{}, pages[r.URL.Query().Get("page")]...))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/linkshort/linkshort/labels":
		pages := map[string][]factoryd.RepoLabel{
			"1": {{ID: 21, Name: "agent"}, {ID: 22, Name: "human"}},
			"2": {{ID: 23, Name: "backlog"}, {ID: 24, Name: "plan"}},
		}
		json.NewEncoder(w).Encode(append([]factoryd.RepoLabel{}, pages[r.URL.Query().Get("page")]...))
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/labels"):
		var body struct {
			Labels []int64 `json:"labels"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.labelled = append(f.labelled, fmt.Sprintf("%s %v", r.URL.Path, body.Labels))
		w.Write([]byte(`[]`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/pulls/31/files":
		// two pages, as a pull request of more files than a page holds
		pages := map[string]string{
			"1": `[{"filename":"internal/console/board.go","status":"modified","additions":120,"deletions":38},` +
				`{"filename":"internal/console/drawer_test.go","status":"added","additions":85,"deletions":0}]`,
			"2": `[{"filename":"internal/console/old.go","status":"removed","additions":0,"deletions":12}]`,
		}
		w.Write([]byte(cmp.Or(pages[r.URL.Query().Get("page")], "[]")))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/pulls/32/files":
		// a pull request of more files than the drawer reads: every page full
		var files []map[string]any
		for i := range 50 {
			files = append(files, map[string]any{"filename": fmt.Sprintf("gen/p%s-%d.go", r.URL.Query().Get("page"), i), "status": "added", "additions": 1})
		}
		json.NewEncoder(w).Encode(files)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/pulls/9" && f.pullHead != "":
		json.NewEncoder(w).Encode(map[string]any{"head": map[string]any{"sha": f.pullHead}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/pulls/9/files":
		pages := map[string]string{"1": `[{"filename":"a.go","status":"modified","additions":40,"deletions":3},` +
			`{"filename":"b.go","status":"modified","additions":5,"deletions":5}]`}
		w.Write([]byte(cmp.Or(pages[r.URL.Query().Get("page")], "[]")))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.EscapedPath(), "/api/v1/repos/sf/code/raw/"):
		// the files at main, by their escaped paths
		files := map[string]string{"README.md": strings.Repeat("r", 1200), "bin/x": "\x00" + strings.Repeat("b", 99),
			"docs/a%20b.md": "spaced", "big.txt": strings.Repeat("g", 70<<10), "huge.txt": strings.Repeat("h", 1<<20+1)}
		body, ok := files[strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/repos/sf/code/raw/")]
		if !ok || r.URL.Query().Get("ref") != "main" {
			http.Error(w, `{"message":"The target couldn't be found."}`, http.StatusNotFound)
			return
		}
		io.WriteString(w, body)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/linkshort/linkshort/issues":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 42})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/sf/code/issues":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 41})
	case r.Method == http.MethodGet && f.scene != "" && r.URL.Path == "/api/v1/repos/"+f.scene+"/labels":
		labels := []factoryd.RepoLabel{}
		if r.URL.Query().Get("page") == "1" {
			labels = labLabels
		}
		json.NewEncoder(w).Encode(labels)
	case r.Method == http.MethodPost && f.scene != "" && r.URL.Path == "/api/v1/repos/"+f.scene+"/issues":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": f.file(body)})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.reviews = append(f.reviews, body)
		if f.failReview {
			http.Error(w, `{"message":"review refused"}`, http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(`{"id":12}`))
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/code/issues/"):
		// the issue as it stands after the edit: the rig's issues are titled
		// "Fix the cache" with the body "It drops entries." until edited
		raw, _ := io.ReadAll(r.Body)
		f.patched = append(f.patched, r.URL.Path+" "+string(raw))
		var body map[string]string
		json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"title": cmp.Or(body["title"], "Fix the cache"), "body": cmp.Or(body["body"], "It drops entries.")})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/code/pulls/"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.closed = append(f.closed, r.URL.Path+" "+body["state"])
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assets"):
		if f.failAssets {
			http.Error(w, `{"message":"asset refused"}`, http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for field, files := range r.MultipartForm.File {
			for _, fh := range files {
				file, _ := fh.Open()
				data, _ := io.ReadAll(file)
				file.Close()
				f.assets = append(f.assets, asset{path: r.URL.Path, name: r.URL.Query().Get("name"), field: field, file: fh.Filename, data: data})
			}
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.comments[r.URL.Path] = body["body"]
		f.posted[r.URL.Path] = append(f.posted[r.URL.Path], body["body"])
		f.commentN++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": f.commentN})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/code/issues/"):
		f.deleted = append(f.deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/branches/main":
		fmt.Fprintf(w, `{"name":"main","commit":{"id":%q}}`, ciHead)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/code/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
		// a commit without statuses: Forgejo answers an empty combined status
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/code/commits/"), "/status")
		io.WriteString(w, cmp.Or(f.statuses[sha], `{"state":"","statuses":null}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/pulls/9.diff":
		io.WriteString(w, "diff --git a/cache.go b/cache.go\n-\treturn nil\n+\treturn c.entries[k]\n")
	default:
		http.NotFound(w, r)
	}
}

// fakeTasks is the board's task store. read is called, with the mutex held,
// on every Task read; a test may set it to change the rows while a request
// runs (item 141: factoryd ends an accepted plan and files its cards seconds
// after the console's first read of it).
type fakeTasks struct {
	mu    sync.Mutex
	tasks []factoryd.BoardTask
	plans map[string][]factoryd.PlanRevision // by task, oldest first
	// questions are the plans' questions by "task revision"
	questions map[string][]factoryd.PlanQuestion
	// holdouts are the plans' hold-out rows by task, idx order
	holdouts map[string][]factoryd.Holdout
	// criteria are the criteria verdict rows by task
	criteria map[string][]factoryd.CriterionVerdict
	deps     map[string][]string // the tasks a task depends on
	carries  map[string][]string // the cards a roll task carries
	read     func(s *fakeTasks, id string)
	// fundedEUR is the month's chat funding FundCard's pool counts, events
	// the ledger its card_funded row goes to
	fundedEUR float64
	events    *fakeEvents
	// parks are the open image parks, every project's
	parks []factoryd.ImagePark
}

// ProjectImageParks are the parks of project's cards.
func (s *fakeTasks) ProjectImageParks(_ context.Context, project string) ([]factoryd.ImagePark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.ImagePark
	for _, p := range s.parks {
		if p.Project == project {
			out = append(out, p)
		}
	}
	return out, nil
}

// FundCard raises task id's budget and records e, both or neither, as the
// store's one transaction does.
func (s *fakeTasks) FundCard(ctx context.Context, id string, eur float64, bounds *factoryd.FundBounds, e factoryd.Event) (float64, float64, error) {
	if bounds != nil {
		if factoryd.Cents(s.fundedEUR)+factoryd.Cents(eur) > factoryd.Cents(bounds.PoolEUR) {
			return 0, 0, &factoryd.PoolSpentError{Funded: s.fundedEUR}
		}
		for _, k := range s.tasks {
			if k.ID == id && factoryd.Cents(k.BudgetEUR)+factoryd.Cents(eur) > factoryd.Cents(bounds.CardCapEUR) {
				return 0, 0, &factoryd.CardCapError{Budget: k.BudgetEUR}
			}
		}
		e.Detail["decider"] = "agent"
	}
	from, to, err := s.RaiseTaskBudget(ctx, id, eur)
	if err != nil {
		return 0, 0, err
	}
	e.Detail["from_eur"], e.Detail["to_eur"], e.Detail["amount_eur"] = from, to, eur
	if _, err := s.events.Record(ctx, e); err != nil {
		s.RaiseTaskBudget(ctx, id, -eur)
		return 0, 0, err
	}
	return from, to, nil
}

// Dependencies are the tasks task id depends on, those not done first.
func (s *fakeTasks) Dependencies(_ context.Context, id string) ([]factoryd.Dependency, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var open, done []factoryd.Dependency
	for _, t := range s.tasks {
		d := factoryd.Dependency{TaskID: t.ID, Issue: t.IssueNumber, Key: t.PlanKey, Status: t.Status}
		switch {
		case !slices.Contains(s.deps[id], t.ID):
		case t.Status == "done":
			done = append(done, d)
		default:
			open = append(open, d)
		}
	}
	return append(open, done...), nil
}

func (s *fakeTasks) TaskStatuses(_ context.Context, ids []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, id := range ids {
		for _, t := range s.tasks {
			if t.ID == id {
				out[id] = t.Status
			}
		}
	}
	return out, nil
}

func (s *fakeTasks) Dependents(_ context.Context, id string) ([]factoryd.Dependency, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Dependency
	for _, t := range s.tasks {
		if slices.Contains(s.deps[t.ID], id) {
			out = append(out, factoryd.Dependency{TaskID: t.ID, Issue: t.IssueNumber, Key: t.PlanKey, Status: t.Status})
		}
	}
	return out, nil
}

func (s *fakeTasks) CountAdmitted(_ context.Context, repository string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, bt := range s.tasks {
		t := bt.Task
		if t.Repository != repository || t.Kind != "agent" || t.Unbound() {
			continue
		}
		if slices.Contains([]string{"waiting-deps", "running", "in-review", "closing"}, t.Status) ||
			(t.Status == "queued" && t.AdmittedAt != nil) {
			n++
		}
	}
	return n, nil
}

func (s *fakeTasks) CountQueue(_ context.Context, repository string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, bt := range s.tasks {
		t := bt.Task
		if t.Repository != repository || t.Kind != "agent" || t.Unbound() || t.AdmittedAt != nil || factoryd.Final(t.Status) {
			continue
		}
		n++
	}
	return n, nil
}

func (s *fakeTasks) PlanRevision(_ context.Context, id string, n int) (factoryd.PlanRevision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revs := s.plans[id]
	for i := len(revs) - 1; i >= 0; i-- {
		if n == 0 || revs[i].Revision == n {
			return revs[i], true, nil
		}
	}
	return factoryd.PlanRevision{}, false, nil
}

func (s *fakeTasks) PlanQuestions(_ context.Context, id string, revision int) ([]factoryd.PlanQuestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.questions[fmt.Sprintf("%s %d", id, revision)]), nil
}

func (s *fakeTasks) PlanHoldouts(_ context.Context, id string) ([]factoryd.Holdout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.holdouts[id]), nil
}

// answer is factoryd.PGStore.AnswerPlanQuestion on the fake's questions.
func (s *fakeTasks) answer(id string, revision, idx int, answer, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	qs := s.questions[fmt.Sprintf("%s %d", id, revision)]
	switch {
	case idx < 1 || idx > len(qs):
		return factoryd.ErrNotFound
	case qs[idx-1].Answer != "":
		return factoryd.ErrAnswered
	}
	qs[idx-1].Answer, qs[idx-1].AnsweredBy = answer, by
	return nil
}

// decide is factoryd.DecideHoldout on the fake's hold-outs: the round's
// confirmation unanswered, the row's state taking the verb.
func (s *fakeTasks) decide(id string, round, idx int, verb, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.questions[fmt.Sprintf("%s %d", id, round)] {
		if q.Kind == factoryd.QuestionConfirmation && q.Answer != "" {
			return factoryd.ErrAnswered
		}
	}
	state := map[string]string{"okay": factoryd.HoldoutStored, "drop": factoryd.HoldoutDropped}[verb]
	if state == "" {
		return fmt.Errorf("%w: %s", factoryd.ErrHoldoutClick, verb)
	}
	for i, h := range s.holdouts[id] {
		if h.Round != round || h.Idx != idx {
			continue
		}
		takes := (state == factoryd.HoldoutDropped && (h.State == factoryd.HoldoutProposed || h.State == factoryd.HoldoutUnrunnable)) ||
			(state == factoryd.HoldoutStored && h.State == factoryd.HoldoutProposed && (h.DryVerdict == "red" || h.DryVerdict == "green"))
		if !takes {
			return factoryd.ErrHoldoutClick
		}
		s.holdouts[id][i].State = state
		return nil
	}
	for _, h := range s.holdouts[id] {
		if h.Idx == idx {
			return factoryd.ErrHoldoutClick
		}
	}
	return factoryd.ErrNoHoldout
}

func (s *fakeTasks) CriteriaVerdicts(_ context.Context, taskID string) ([]factoryd.CriterionVerdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.criteria[taskID], nil
}

func (s *fakeTasks) PlanCards(_ context.Context, plan string) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Task
	for _, t := range s.tasks {
		if t.Parent == plan && t.Source == factoryd.SourcePlan {
			out = append(out, t.Task)
		}
	}
	return out, nil
}

func (s *fakeTasks) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.read != nil {
		s.read(s, id)
	}
	for _, t := range s.tasks {
		if t.ID == id {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *fakeTasks) RollCarries(_ context.Context, id string) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Task
	for _, t := range s.tasks {
		if slices.Contains(s.carries[id], t.ID) {
			out = append(out, t.Task)
		}
	}
	return out, nil
}

func (s *fakeTasks) TaskByIssue(_ context.Context, repository string, issue int64) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.Repository == repository && t.IssueNumber == issue {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

// BoardTasks lists repository's open cards, as the store does, in its
// order: a chat task is none, and every final card is gone.
func (s *fakeTasks) BoardTasks(_ context.Context, repository string) ([]factoryd.BoardTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cards := slices.DeleteFunc(slices.Clone(s.tasks), func(t factoryd.BoardTask) bool {
		return t.Repository != repository || t.Kind == factoryd.KindChat || factoryd.Final(t.Status)
	})
	factoryd.SortBoardTasks(cards)
	return cards, nil
}

func (s *fakeTasks) FinishedTasks(_ context.Context, q factoryd.FinishedQuery) ([]factoryd.BoardTask, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page, rest := factoryd.FinishedPage(s.tasks, q)
	return page, rest, nil
}

func (s *fakeTasks) BoardTask(_ context.Context, id string) (factoryd.BoardTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.tasks, func(t factoryd.BoardTask) bool { return t.ID == id })
	if i < 0 {
		return factoryd.BoardTask{}, false, nil
	}
	return s.tasks[i], true, nil
}

// set replaces the tasks; the escalated ones carry reason. ChainLive is
// DeployLive over the set, as the store reads the column: a card whose
// change no green chain covers is not live, done or not.
func (s *fakeTasks) set(reason string, tasks ...factoryd.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = nil
	for _, t := range tasks {
		b := factoryd.BoardTask{Task: t}
		if t.Status == "waiting-human" {
			b.Escalation = reason
		}
		s.tasks = append(s.tasks, b)
	}
	for i := range s.tasks {
		s.tasks[i].ChainLive = factoryd.DeployLive(s.tasks, s.tasks[i])
	}
}

// SetHumanSummary stores task id's human summary; "" restores the
// generated title (the column back to NULL).
func (s *fakeTasks) SetHumanSummary(_ context.Context, id, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			s.tasks[i].HumanSummary = summary
			return nil
		}
	}
	return fmt.Errorf("task %s: %w", id, factoryd.ErrNotFound)
}

// SetObjective stores task id's objective.
func (s *fakeTasks) SetObjective(_ context.Context, id, objective string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			s.tasks[i].Objective = objective
			return nil
		}
	}
	return fmt.Errorf("task %s: %w", id, factoryd.ErrNotFound)
}

// RaiseTaskBudget raises task id's budget by eur, as the store's one
// statement does, and answers the budget before and after.
func (s *fakeTasks) RaiseTaskBudget(_ context.Context, id string, eur float64) (float64, float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			from := s.tasks[i].BudgetEUR
			s.tasks[i].BudgetEUR += eur
			return from, s.tasks[i].BudgetEUR, nil
		}
	}
	return 0, 0, fmt.Errorf("task %s: %w", id, factoryd.ErrNotFound)
}

func (s *fakeTasks) AddTaskDeps(_ context.Context, id string, dependsOn []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deps == nil {
		s.deps = map[string][]string{}
	}
	for _, d := range dependsOn {
		if !slices.Contains(s.deps[id], d) {
			s.deps[id] = append(s.deps[id], d)
		}
	}
	return nil
}

func (s *fakeTasks) RemoveTaskDeps(_ context.Context, id string, dependsOn []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps[id] = slices.DeleteFunc(s.deps[id], func(d string) bool { return slices.Contains(dependsOn, d) })
	return nil
}

// SetTaskAccent stores task id's accent colour; "" removes it.
func (s *fakeTasks) SetTaskAccent(_ context.Context, id, accent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			s.tasks[i].Accent = accent
			return nil
		}
	}
	return fmt.Errorf("task %s: %w", id, factoryd.ErrNotFound)
}

// fakeWriters are each project's writers by repository, the factory's own
// sf/code among them (Config.Writers).
type fakeWriters struct {
	mu    sync.Mutex
	by    map[string][]string
	asked int
	err   error
	delay time.Duration
}

// writes is Config.Writes over the same lists, by repository.
func (w *fakeWriters) writes(_ context.Context, repository, user string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return false, w.err
	}
	return slices.Contains(w.by[repository], user), nil
}

func (w *fakeWriters) list(context.Context) (string, map[string][]string, error) {
	time.Sleep(w.delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked++
	if w.err != nil {
		return "", nil, w.err
	}
	return "sf/code", maps.Clone(w.by), nil
}

type fakeObjects map[string][]byte

func (o fakeObjects) Put(_ context.Context, k string, b []byte) error { o[k] = b; return nil }
func (o fakeObjects) Get(_ context.Context, k string) ([]byte, error) { return o[k], nil }
func (o fakeObjects) Delete(_ context.Context, k string) error        { delete(o, k); return nil }

// fakeApprovals is the approval_requests table.
type fakeApprovals struct {
	mu   sync.Mutex
	rows []factoryd.ApprovalRequest
}

func (a *fakeApprovals) ApprovalRequests(_ context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.ApprovalRequest
	for _, r := range a.rows {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *fakeApprovals) DecideApproval(_ context.Context, id int64, kind, status, by, note string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, r := range a.rows {
		if r.ID == id && r.Kind == kind && r.Status == "pending" {
			a.rows[i].Status, a.rows[i].DecidedBy, a.rows[i].DecisionNote = status, by, note
			return nil
		}
	}
	return factoryd.ErrNotFound
}

func (a *fakeApprovals) RetryApprovalBrief(_ context.Context, id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, r := range a.rows {
		if r.ID == id && r.Status == "pending" && r.Brief == "" {
			a.rows[i].BriefError = ""
		}
	}
	return nil
}

func (a *fakeApprovals) get(id int64) factoryd.ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.rows {
		if r.ID == id {
			return r
		}
	}
	return factoryd.ApprovalRequest{}
}

// fakeResources is cloudres.AgentStore: it records the calls.
type fakeResources struct {
	rows     []cloudres.Resource
	use      map[string]cloudres.EnvelopeUse
	standing map[string]cloudres.StandingUse
	calls    []string
}

func (f *fakeResources) Commissions(_ context.Context, since time.Time) ([]cloudres.Resource, error) {
	f.calls = append(f.calls, "commissions")
	return f.rows, nil
}

func (f *fakeResources) Use(_ context.Context, repo string) (cloudres.EnvelopeUse, error) {
	return f.use[repo], nil
}

func (f *fakeResources) Standing(_ context.Context, repo string) (cloudres.StandingUse, error) {
	return f.standing[repo], nil
}

// Promote records the standing block it was given for sf/code and for
// demo-app/demo-app.
func (f *fakeResources) Promote(_ context.Context, id int64, by string, standing func(repo string) policy.Standing) error {
	f.calls = append(f.calls, fmt.Sprintf("promote %d by %s under %v and %v", id, by, standing("sf/code"), standing("demo-app/demo-app")))
	if id == 410 {
		return fmt.Errorf("resource %d: %w", id, cloudres.ErrLeaseEnded)
	}
	if id == 500 {
		return errors.New(`pq: deadlock detected in "cloud_resources"`)
	}
	if id == 420 {
		return fmt.Errorf("resource %d: %w: server type cx53 is not in the standing types [cx23]", id, cloudres.ErrBeyondStanding)
	}
	return nil
}

func (f *fakeResources) EndLease(_ context.Context, id int64, by string) error {
	f.calls = append(f.calls, fmt.Sprintf("release %d by %s", id, by))
	if id == 404 {
		return fmt.Errorf("resource %d: %w", id, cloudres.ErrNotOpen)
	}
	return nil
}

// Commission answers a row by id from rows; 13 and any unknown id have
// none.
func (f *fakeResources) Commission(_ context.Context, id int64) (cloudres.Resource, bool, error) {
	for _, r := range f.rows {
		if r.ID == id {
			return r, true, nil
		}
	}
	return cloudres.Resource{}, false, nil
}

// fakeAlerts is the alerts table.
type fakeAlerts struct {
	rows     []factoryd.Alert
	verdicts []string // "id verdict by"
}

func (a *fakeAlerts) Alerts(_ context.Context, limit int) ([]factoryd.Alert, error) {
	return a.rows[:min(limit, len(a.rows))], nil
}

func (a *fakeAlerts) SetAlertVerdict(_ context.Context, id int64, verdict, by string) error {
	for i, r := range a.rows {
		if r.ID == id && r.Kind == factoryd.AlertEscalation && r.Verdict == "" {
			a.rows[i].Verdict, a.rows[i].ResolvedBy = verdict, by
			a.verdicts = append(a.verdicts, fmt.Sprintf("%d %s %s", id, verdict, by))
			return nil
		}
	}
	return factoryd.ErrNotFound
}

func (a *fakeAlerts) AlertByID(_ context.Context, id int64) (factoryd.Alert, bool, error) {
	for _, r := range a.rows {
		if r.ID == id {
			return r, true, nil
		}
	}
	return factoryd.Alert{}, false, nil
}

type fakeLedger struct {
	mu        sync.Mutex
	from, to  time.Time
	costs     policy.Costs
	monthCaps map[string]float64 // budget.month_eur by project file
	processes map[string]policy.Process
	chat      *policy.ChatConfig // the policy's chat block
	commands  policy.Commands    // commands.yaml
	approvers []string           // the policy's; nil: sebastian
	files     map[string]policy.ProjectConfig
	calls     map[string][]policy.ModelCall // by task
	risk      policy.Risk
	routines  []policy.Routine  // policy.yaml's
	briefs    map[string]string // the routines' briefs by path
	down      bool              // no policy in force can be read
	// premiumCap is the policy's budget.premium_month_eur and premium the
	// month's spend on the premium class
	premiumCap, premium float64
}

// testProcesses are the shipped two processes: maintenance builds direct
// only, spec has a plan track.
func testProcesses() map[string]policy.Process {
	stages := []policy.Stage{
		{Name: "screening", OnFindings: "human", When: "outsider"},
		{Name: "checks", OnFindings: "agent", Enabled: true},
		{Name: "merge", OnFindings: "human", Enabled: true},
	}
	return map[string]policy.Process{
		"maintenance": {Tracks: policy.Tracks{Direct: policy.Track{Class: "coder"}},
			Stages: stages, Merge: policy.Merge{Queue: "none"}},
		"spec": {Tracks: policy.Tracks{Direct: policy.Track{Class: "coder"},
			Plan: &policy.Track{Class: "planner", Plan: &policy.PlanShape{MaxCards: 12, Foundation: true, Integration: true}}},
			Dependencies: &policy.Dependencies{WaitUntil: "done", OnFailed: "human"}, Stages: stages, Merge: policy.Merge{Queue: "project"}},
	}
}

func (l *fakeLedger) Costs(_ context.Context, from, to time.Time) (policy.Costs, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.from, l.to = from, to
	return l.costs, nil
}

// PremiumMonthSpend is the fake's premium spend, whatever the month.
func (l *fakeLedger) PremiumMonthSpend(context.Context, time.Time) (float64, error) {
	return l.premium, nil
}

// DaySpend is the fake's total of one day; it records no window, as it
// is not the costs read the window tests watch.
func (l *fakeLedger) DaySpend(_ context.Context, day time.Time) (float64, error) {
	var eur float64
	for _, d := range l.costs.Days {
		if d.Day == day.Format(policy.DateLayout) {
			eur += d.Total
		}
	}
	return eur, nil
}

func (l *fakeLedger) ModelCalls(_ context.Context, taskID string) ([]policy.ModelCall, error) {
	return l.calls[taskID], nil
}

// CallTotals sums the tasks' calls' tokens and EUR as the store's one
// query does over the audit rows.
func (l *fakeLedger) CallTotals(_ context.Context, tasks []string) (map[string]policy.CallTotals, error) {
	out := map[string]policy.CallTotals{}
	for _, id := range tasks {
		var t policy.CallTotals
		for _, c := range l.calls[id] {
			t.Prompt, t.Cached, t.Output = t.Prompt+c.PromptTokens, t.Cached+c.CachedTokens, t.Output+c.OutputTokens
			t.ModeledEUR += c.EUR
		}
		if t != (policy.CallTotals{}) {
			out[id] = t
		}
	}
	return out, nil
}

func (l *fakeLedger) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	if l.down {
		return policy.Version{}, false, errors.New("the ledger is down")
	}
	v := policy.Version{ID: 4, Commit: "c9d98bb0aa11", LoadedAt: time.Date(2026, 9, 27, 22, 50, 7, 0, time.UTC),
		ActivatedAt: time.Date(2026, 9, 27, 22, 56, 38, 0, time.UTC),
		Tree: policy.Tree{Document: policy.Document{Version: 1, Budget: policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50, PremiumMonthEUR: l.premiumCap},
			Classes:         map[string]policy.Class{"coder": {Models: []string{"factory/small"}}},
			Limits:          policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
			Approvers:       []string{"sebastian"},
			EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20},
			Chat:            l.chat, Risk: l.risk, Routines: l.routines},
			Projects: map[string]policy.ProjectConfig{
				"demo-app": {Repository: "demo-app/demo-app", MergeRule: "auto", Objects: "factory", Agents: "coder", Namespaces: []string{"demo-staging", "demo-prod"},
					Envelope: &policy.Envelope{MonthlyEUR: 30, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 3, MaxLeaseHours: 8},
					Standing: &policy.Standing{MonthlyEUR: 12, ServerTypes: []string{"cx23"}, MaxCount: 2}},
				"infra": {Repository: "sf/infra", MergeRule: "human", Objects: "seed", Agents: "none"},
			},
			Processes: l.processes, Commands: l.commands, Briefs: l.briefs}}
	maps.Copy(v.Tree.Projects, l.files)
	if l.approvers != nil {
		v.Document.Approvers = l.approvers
	}
	for name, eur := range l.monthCaps {
		p := v.Tree.Projects[name]
		p.Budget = &policy.ProjectBudget{MonthEUR: eur}
		v.Tree.Projects[name] = p
	}
	return v, true, nil
}

type rig struct {
	c         *Console
	fg        *fakeForgejo
	tasks     *fakeTasks
	writers   *fakeWriters
	objects   fakeObjects
	ledger    *fakeLedger
	approvals *fakeApprovals
	resources *fakeResources
	alerts    *fakeAlerts
	projects  *fakeProjects
	sessions  *fakeSessions
	events    *fakeEvents
	chat      *fakeChat
	// gateway is the lab's scripted model, nil in the tests
	gateway *chatGateway
	// infra is the lab's configuration tree, nil in the tests
	infra    *infraFake
	mirrors  *fakeMirrors
	routines *fakeRoutines
	deps     *fakeDeps
	figures  *fakeFigures
	// repoWriters are the writers of each project's repository, the
	// writer lists' map (writers.by)
	repoWriters map[string][]string
	signalErr   error
	retried     []string // "task by user" per RetryPlanCard
	retryErr    error
	handed      []string // "task by user" per GoCard
	goErr       error
	metrics     *sdkmetric.ManualReader
	invited     []string // "repository user permission" per AddCollaborator
	issueDeps   []string // "repository#issue on #dependency", the bot's issue dependencies
	closed      []string // "task by user" per CloseCard
	reran       []string // "repository@sha" per RerunStall
	rerunErr    error
	dismissed   []string // "repository@sha by user" per DismissStall
	dismissErr  error
	synced      []string // "project by user" per GitHubSync
	syncErr     error
	syncGate    chan struct{} // GitHubSync waits on it, when set
	// chatToken is every project's chat token, the wake's identity
	chatToken string
	// imageAsks are "repository: tool | need | image, by user" per
	// RequestImage, which answers imageAsk and imageErr, judging the
	// filing of imageAsk by its admission first
	imageAsks []string
	imageAsk  factoryd.ImageAsk
	imageErr  error
}

var monthly = 8.99

// writerOf logs user in, as a further session of the rig, as a writer of
// repository alone.
func (r *rig) writerOf(t *testing.T, user, repository string) (*http.Cookie, *session) {
	t.Helper()
	r.repoWriters[repository] = append(r.repoWriters[repository], user)
	return memberLogin(t, r, user)
}

// readerOfCode logs user in as a writer of demo-app alone whom Forgejo
// shows the code project's repository: a reader of its board.
func (r *rig) readerOfCode(t *testing.T, user string) (*http.Cookie, *session) {
	t.Helper()
	r.c.cfg.HTTP = &http.Client{Transport: readerForgejo{http.DefaultTransport}}
	return r.writerOf(t, user, "demo-app/demo-app")
}

// forgetWriters drops the console's writer lists and its writes answers,
// so its next check reads the rig's anew.
func (r *rig) forgetWriters() {
	r.c.mu.Lock()
	r.c.writersT, r.c.written = time.Time{}, nil
	r.c.mu.Unlock()
}

func newRig(t *testing.T) *rig {
	fg := newFakeForgejo(t)
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	repoWriters := map[string][]string{"sf/code": {"sebastian"}, "demo-app/demo-app": {"sebastian"}, "linkshort/linkshort": {"sebastian"}}
	r := &rig{fg: fg, tasks: &fakeTasks{}, writers: &fakeWriters{by: repoWriters}, objects: fakeObjects{}, ledger: &fakeLedger{processes: testProcesses()},
		approvals: &fakeApprovals{rows: []factoryd.ApprovalRequest{
			{ID: 3, Kind: "resource", TaskID: "t-res", Project: "sf/code", Status: "pending", MonthlyEUR: &monthly,
				Reason: "load test (beyond the envelope: a lease of 300 minutes exceeds the envelope's 4 h)",
				Spec: json.RawMessage(`{"server_type":"cx33","purpose":"load test","lease_minutes":300,"volume_gb":0,"location":"fsn1",` +
					`"reservation_eur":0.072,"hourly_eur":0.0144,"beyond_envelope":["a lease of 300 minutes exceeds the envelope's 4 h"]}`)},
			{ID: 4, Kind: "merge", TaskID: "t-m", Project: "sf/code", PRNumber: 9, Status: "pending",
				Spec: json.RawMessage(`{"risk_class":"high","migration":true,"migrations":["internal/factoryd/migrations/0018_x.sql"],` +
					`"verdict_summary":"adds the column safely","review":{"verdict":"approve","summary":"adds the column safely",` +
					`"verdicts":{"internal/api.go: the handler leaks the client":{"state":"dropped","evidence":"grep finds the client closed in handler.go:40","round":1},` +
					`"a follow-up files the docs":{"state":"remark"}}},"head_sha":"abc123",` +
					`"recommendation":"Adds the column; merge it.","why_human":["the project's merge rule is human","risk class high is requires_approval"],` +
					`"earlier_reviews":[{"verdict":"concerns","summary":"the column lacks a default","findings":["0018_x.sql: add a default"],` +
					`"verdicts":{"0018_x.sql: add a default":{"state":"dropped","evidence":"main already held the default","round":1}}}],` +
					`"audit":{"verdict":"findings","model":"big","deployment":"zai/glm-5.3","findings":[` +
					`{"item":"secret in the diff: private key","file":"internal/x/x_test.go","line":14,"why":"an added line has the shape of a private key (pre-check)"},` +
					`{"item":"the audit gave no verdict","why":"the answer is not one JSON object: EOF"}]}}`)},
			{ID: 5, Kind: "merge", TaskID: "t-old", Project: "sf/code", PRNumber: 8, Status: "denied"},
		}},
		resources: &fakeResources{}, alerts: &fakeAlerts{}, projects: &fakeProjects{rows: projectRows()}, metrics: reader,
		figures:     &fakeFigures{report: metricsFiguresReport()},
		events:      &fakeEvents{bus: factoryd.NewEventBus()},
		mirrors:     &fakeMirrors{rows: map[string]factoryd.Mirror{}, push: map[string][]factoryd.PushMirror{}},
		routines:    &fakeRoutines{},
		deps:        &fakeDeps{},
		repoWriters: repoWriters,
		chatToken:   "chat-token"}
	r.tasks.events = r.events
	r.chat = &fakeChat{tasks: r.tasks}
	r.sessions = newFakeSessions()
	c, err := New(Config{
		Metrics:   metrics,
		PublicURL: "https://console.example", ForgejoURL: fg.srv.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Sessions: r.sessions,
		Tasks:    r.tasks, Writers: r.writers.list, Writes: r.writers.writes, Timelines: r.objects, Policies: r.ledger, Costs: r.ledger, Dependencies: r.deps,
		ConfigRepo: "https://git.example/sf/infra", ConfigDir: "factory",
		Approvals: r.approvals, Resources: r.resources, Alerts: r.alerts,
		DeploymentHealth: func(ctx context.Context, p factoryd.Project, deployment, namespace string) (string, error) {
			return factoryd.DeploymentHealthText(ctx, healthStub{}, p, deployment, namespace, time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC))
		},
		Projects: r.projects, Events: r.events, Figures: r.figures, Bus: r.events.bus, Chat: r.chat, Mirrors: r.mirrors, Routines: r.routines,
		ChatToken: func(factoryd.Project) string { return r.chatToken },
		GitHubSync: func(_ context.Context, project, by string) (string, error) {
			if r.syncGate != nil {
				<-r.syncGate
			}
			r.synced = append(r.synced, project+" by "+by)
			return "Pull request #3 brings GitHub's default branch of acme/app at abc1234 to main.", r.syncErr
		},
		ApproveMerge: func(_ context.Context, taskID string, pr int64, approver string) error {
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL approved %s #%d by %s", taskID, pr, approver))
			return r.signalErr
		},
		RerunChecks: func(_ context.Context, taskID string, pr int64, by string) error {
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL rerun_checks %s #%d by %s", taskID, pr, by))
			return r.signalErr
		},
		CloseFactoryPR: func(_ context.Context, taskID string, pr int64, by, words string) error {
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL changes_requested %s #%d by %s: %s", taskID, pr, by, words))
			return r.signalErr
		},
		AnswerPlan: func(ctx context.Context, taskID string, revision, idx int, answer, by string) error {
			if newest, _, _ := r.tasks.PlanRevision(ctx, taskID, 0); newest.Revision != revision {
				return factoryd.ErrStalePlan{Newest: newest.Revision}
			}
			if err := r.tasks.answer(taskID, revision, idx, answer, by); err != nil {
				return err
			}
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL plan_answered %s %d %d by %s", taskID, revision, idx, by))
			return nil
		},
		AnswerPlanQuestions: func(ctx context.Context, taskID string, revision int, answers []factoryd.PlanAnswer, by string) error {
			if newest, _, _ := r.tasks.PlanRevision(ctx, taskID, 0); newest.Revision != revision {
				return factoryd.ErrStalePlan{Newest: newest.Revision}
			}
			for _, a := range answers {
				if err := r.tasks.answer(taskID, revision, a.Idx, a.Text, by); err != nil {
					return err
				}
			}
			fg.mu.Lock()
			defer fg.mu.Unlock()
			for _, a := range answers {
				fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL plan_answered %s %d %d by %s", taskID, revision, a.Idx, by))
			}
			return nil
		},
		DecideHoldout: func(ctx context.Context, taskID string, revision, idx int, verb, by string) error {
			if newest, _, _ := r.tasks.PlanRevision(ctx, taskID, 0); newest.Revision != revision {
				return factoryd.ErrStalePlan{Newest: newest.Revision}
			}
			if err := r.tasks.decide(taskID, revision, idx, verb, by); err != nil {
				return err
			}
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL holdout_decided %s %d %d %s by %s", taskID, revision, idx, verb, by))
			return nil
		},
		CloseCard: func(_ context.Context, taskID, by string) error {
			r.closed = append(r.closed, taskID+" by "+by)
			return nil
		},
		RerunStall: func(_ context.Context, repository, sha string) error {
			r.reran = append(r.reran, repository+"@"+sha)
			return r.rerunErr
		},
		DismissStall: func(_ context.Context, repository, sha, by string) error {
			r.dismissed = append(r.dismissed, repository+"@"+sha+" by "+by)
			return r.dismissErr
		},
		RetryPlanCard: func(_ context.Context, taskID, by string) (int64, error) {
			r.retried = append(r.retried, taskID+" by "+by)
			return 9, r.retryErr
		},
		GoCard: func(_ context.Context, taskID, by string) (int64, error) {
			r.handed = append(r.handed, taskID+" by "+by)
			return 16, r.goErr
		},
		RequestImage: func(_ context.Context, repository, tool, need, image, by string, admit func(string) error) (factoryd.ImageAsk, error) {
			r.imageAsks = append(r.imageAsks, repository+": "+tool+" | "+need+" | "+image+", by "+by)
			if r.imageAsk.Filed {
				if err := admit("Add " + tool + " to image " + r.imageAsk.Target); err != nil {
					return factoryd.ImageAsk{}, err
				}
			}
			return r.imageAsk, r.imageErr
		},
		AddCollaborator: func(_ context.Context, repo factoryd.Repo, user, permission string) error {
			r.invited = append(r.invited, repo.String()+" "+user+" "+permission)
			return nil
		},
		SetIssueDependency: func(_ context.Context, repo factoryd.Repo, number, dependency int64, remove bool) error {
			edge := fmt.Sprintf("%s#%d on #%d", repo, number, dependency)
			r.issueDeps = slices.DeleteFunc(r.issueDeps, func(e string) bool { return e == edge })
			if !remove {
				r.issueDeps = append(r.issueDeps, edge)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	return r
}

func (r *rig) do(req *http.Request, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	r.c.ServeHTTP(rec, req)
	return rec
}

func cookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// login runs /login and the callback as Forgejo would answer them and
// returns the session cookie (nil when none was set) and the callback's
// response.
func (r *rig) login(t *testing.T) (*http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	r.fg.mu.Lock()
	r.fg.challenge = loc.Query().Get("code_challenge")
	r.fg.mu.Unlock()
	state := cookie(t, rec, loginCookie)
	cb := r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")), nil), state)
	return cookie(t, cb, sessionCookie), cb
}

// readSession is the login of ck as a second Console value over the same
// store reads it: the restart case (roadmap 164). nil when no row answers.
func (r *rig) readSession(t *testing.T, ck *http.Cookie) *session {
	t.Helper()
	other, err := New(r.c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	other.writers, other.writersT = r.c.writers, time.Now().Add(time.Hour) // the writer check stays cached: no extra Forgejo read
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(ck)
	s, err := other.session(req)
	if err != nil {
		t.Fatalf("the session read: %v", err)
	}
	return s
}

// tokens are the session's OAuth tokens as they stand in its row now.
func (r *rig) tokens(t *testing.T, ck *http.Cookie) (access, refresh string) {
	t.Helper()
	hash := sessionIDHash(ck.Value)
	row, ok := r.sessions.row(hash)
	if !ok {
		t.Fatal("no session row")
	}
	access, err := openToken(r.c.cfg.SessionKey, row.Access, hash)
	if err != nil {
		t.Fatalf("the stored access token: %v", err)
	}
	refresh, err = openToken(r.c.cfg.SessionKey, row.Refresh, hash)
	if err != nil {
		t.Fatalf("the stored refresh token: %v", err)
	}
	return access, refresh
}

func (r *rig) loggedIn(t *testing.T) (*http.Cookie, *session) {
	t.Helper()
	ck, cb := r.login(t)
	if ck == nil || cb.Code != http.StatusSeeOther {
		t.Fatalf("login: callback = %d, session cookie %v", cb.Code, ck)
	}
	s := r.readSession(t, ck)
	if s == nil {
		t.Fatal("loggedIn: the fresh login's row does not read back")
	}
	return ck, s
}

func post(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestLoginRedirectCarriesPKCEAndExplicitScope(t *testing.T) {
	r := newRig(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != "https://git.example/login/oauth/authorize" {
		t.Fatalf("redirect to %s, want the public Forgejo authorize URL", got)
	}
	q := loc.Query()
	want := map[string]string{"client_id": "cid", "redirect_uri": "https://console.example/auth/callback", "response_type": "code",
		"scope": "read:user write:repository write:issue", "code_challenge_method": "S256"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("code_challenge")) != 43 || q.Get("state") == "" {
		t.Errorf("code_challenge %q / state %q: want a S256 challenge and a state", q.Get("code_challenge"), q.Get("state"))
	}
	ck := cookie(t, rec, loginCookie)
	if ck == nil || !strings.HasPrefix(ck.Value, q.Get("state")+".") || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie %+v: want the state, HttpOnly, Secure, Lax", ck)
	}
}

func TestLoginExchangesCodeInClusterWithVerifierAndSecret(t *testing.T) {
	r := newRig(t)
	ck, cb := r.login(t)
	if cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d to %q, want 303 to /", cb.Code, cb.Header().Get("Location"))
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" {
		t.Fatalf("session cookie %+v: want HttpOnly, Secure, Lax, path /", ck)
	}
	g := r.fg.grants[0]
	if g.Get("grant_type") != "authorization_code" || g.Get("code") != "the-code" || g.Get("client_secret") != "csecret" ||
		g.Get("redirect_uri") != "https://console.example/auth/callback" || g.Get("code_verifier") == "" {
		t.Fatalf("code exchange form %v", g)
	}
	if !strings.Contains(r.fg.calls[0], "GET /api/v1/user Bearer access-1") {
		t.Fatalf("calls %v: want the user read with the new token", r.fg.calls)
	}
	s := r.readSession(t, ck)
	if s == nil {
		t.Fatal("the fresh login's row does not read back")
	}
	if s.user != "sebastian" || s.access != "access-1" || s.refresh != "refresh-1" || strings.Contains(ck.Value, "access") {
		t.Fatalf("session %+v: tokens stay server-side", s)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(body, "access-1") || strings.Contains(body, "refresh-1") {
		t.Fatal("a token reached the page")
	}
}

func TestCallbackRefusesAForeignState(t *testing.T) {
	r := newRig(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	state := cookie(t, rec, loginCookie)
	cb := r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state=other", nil), state)
	if cb.Code != http.StatusBadRequest || cookie(t, cb, sessionCookie) != nil || len(r.fg.grants) != 0 {
		t.Fatalf("foreign state: %d, grants %d: want 400, no exchange, no session", cb.Code, len(r.fg.grants))
	}
	st := strings.SplitN(state.Value, ".", 2)[0]
	cb = r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+st, nil))
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("no state cookie: %d, want 400", cb.Code)
	}
}

// An anonymous /login keeps nothing on the server: the state and the PKCE
// verifier travel in the signed login cookie, and a cookie whose parts or
// signature were changed, or that expired, is refused before any exchange.
func TestLoginKeepsNoServerStateAndRefusesATamperedCookie(t *testing.T) {
	r := newRig(t)
	for range 1000 {
		r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	}
	if n := serverState(r.c); n != 0 {
		t.Fatalf("1000 anonymous logins left %d entries of server state", n)
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	ck := cookie(t, rec, loginCookie)
	parts := strings.Split(ck.Value, ".")
	if len(parts) != 4 {
		t.Fatalf("login cookie %q: want state.verifier.expiry.mac", ck.Value)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	sum := sha256.Sum256([]byte(parts[1]))
	if loc.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || loc.Query().Get("state") != parts[0] {
		t.Fatal("the cookie's verifier and state are not the ones the authorize URL carries")
	}
	flip := func(s string) string {
		if s[len(s)-1] == 'A' {
			return s[:len(s)-1] + "B"
		}
		return s[:len(s)-1] + "A"
	}
	for name, v := range map[string]string{
		"verifier swapped": strings.Join([]string{parts[0], randomToken(), parts[2], parts[3]}, "."),
		"expiry extended":  strings.Join([]string{parts[0], parts[1], "99999999999", parts[3]}, "."),
		"mac changed":      strings.Join([]string{parts[0], parts[1], parts[2], flip(parts[3])}, "."),
		"expired":          r.c.loginCookieValue(parts[0], parts[1], time.Now().Add(-time.Second)),
		"no mac":           strings.Join(parts[:3], "."),
	} {
		cb := r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+parts[0], nil),
			&http.Cookie{Name: loginCookie, Value: v})
		if cb.Code != http.StatusBadRequest || cookie(t, cb, sessionCookie) != nil {
			t.Errorf("%s: %d, want 400 and no session", name, cb.Code)
		}
	}
	if len(r.fg.grants) != 0 {
		t.Fatalf("a refused cookie reached the code exchange %d times", len(r.fg.grants))
	}
	if _, err := New(Config{SessionKey: []byte("short")}); err == nil {
		t.Fatal("a session key under 32 bytes was accepted")
	}
}

func TestNonWriterGets403AndNoSession(t *testing.T) {
	r := newRig(t)
	r.fg.user = "mallory"
	ck, cb := r.login(t)
	if cb.Code != http.StatusForbidden || ck == nil || ck.MaxAge >= 0 || len(r.sessions.rows) != 0 {
		t.Fatalf("non-writer: %d, cookie %+v, %d sessions: want 403, cookie deleted, none", cb.Code, ck, len(r.sessions.rows))
	}
	if !strings.Contains(cb.Body.String(), "mallory writes none of the factory's projects") {
		t.Fatalf("403 body: %s", cb.Body)
	}
}

func TestWriterRemovedLosesTheSession(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	clear(r.repoWriters)
	r.repoWriters["sf/code"] = []string{"someone-else"}
	r.forgetWriters()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck); rec.Code != http.StatusForbidden {
		t.Fatalf("GET / after removal = %d, want 403", rec.Code)
	}
	if len(r.sessions.rows) != 0 {
		t.Fatal("session kept after the writer was removed")
	}
}

func TestPagesRequireASession(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/", "/board", "/tasks/new", "/tasks/t-1/timeline", "/costs", "/policy"} {
		rec := r.do(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("GET %s = %d to %q, want 303 to /login", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/board", nil)
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx poll without session = %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	for _, path := range []string{"/tasks", "/tasks/t-1/answer", "/logout"} {
		if rec := r.do(post(path, url.Values{"csrf": {"x"}})); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s = %d, want 401", path, rec.Code)
		}
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), &http.Cookie{Name: sessionCookie, Value: "forged"}); rec.Code != http.StatusSeeOther {
		t.Errorf("forged session = %d, want 303", rec.Code)
	}
}

func TestPostWithoutCSRFTokenIsRefused(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	form := url.Values{"title": {"x"}, "track": {"direct"}, "project": {"ai-software-factory-code"}, "body": {"Do the thing.\n\nAcceptance:\n- It works\n"}, "size": {"small"}}
	for name, tok := range map[string]string{"missing": "", "wrong": "nope"} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		if tok != "" {
			f.Set("csrf", tok)
		}
		if rec := r.do(post("/tasks", f), ck); rec.Code != http.StatusForbidden {
			t.Errorf("%s token: %d, want 403", name, rec.Code)
		}
	}
	if len(r.fg.issues) != 0 {
		t.Fatal("an issue was filed without a valid token")
	}
	req := post("/tasks", form)
	req.Header.Set("X-CSRF-Token", s.csrf)
	if rec := r.do(req, ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("htmx header token: %d, want 303", rec.Code)
	}
}

func TestBoardRendersColumnsFromTheStore(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("cannot reach the database", []factoryd.Task{
		{ID: "t-q", Kind: "agent", Status: "queued", Objective: "queued work", Repository: "sf/code", BudgetEUR: 5, IssueNumber: 11},
		{ID: "t-r", Kind: "agent", Status: "closing", Objective: "closing work", Repository: "sf/code", BudgetEUR: 5, SpentEUR: 1.23, PRNumber: 12},
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", BudgetEUR: 5, IssueNumber: 13},
		{ID: "t-v", Kind: "agent", Status: "in-review", Objective: "review work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-d", Kind: "agent", Status: "done", Objective: "done work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-f", Kind: "agent", Status: "failed", Objective: "failed work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-h", Kind: "human", Status: "waiting-human", Objective: "human work", Repository: "sf/code", IssueNumber: 14},
		{ID: "t-hd", Kind: "human", Status: "done", Objective: "finished human work", Repository: "sf/code"},
	}...)
	// the reason comes from the task's row; the board reads no timeline
	r.objects["sessions/t-e/timeline.jsonl"] = []byte(`{"step":"escalated","reason":"old reason","comment":4}
`)
	// the state columns are the wide view's
	rec := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	cols := []string{"Queued", "Running", "Waiting on a human", "In review", "Done", "Failed"}
	objs := []string{"queued work", "closing work", "stuck work", "review work", "done work", "failed work"}
	for i, col := range cols {
		start := strings.Index(body, ">"+col+" <span")
		if start < 0 {
			t.Fatalf("column %q missing", col)
		}
		end := strings.Index(body[start:], "</section>")
		if !strings.Contains(body[start:start+end], objs[i]) {
			t.Errorf("column %q lacks %q", col, objs[i])
		}
	}
	for _, want := range []string{
		`href="https://git.example/sf/code/pulls/12"`,
		"cannot reach the database", `action="/tasks/t-e/answer"`, `name="csrf" value="` + s.csrf + `"`,
		"human work", "load test", "8.9900 EUR per month",
		`data-project="ai-software-factory-code"`, `<script src="/static/htmx.min.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	// the issue, the timeline and the spend under 80 percent live in the drawer
	for id, wants := range map[string][]string{
		"t-q": {`href="https://git.example/sf/code/issues/11"`, `href="/tasks/t-q/timeline"`},
		"t-r": {"€1.23 of €5.00"},
	} {
		drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer", nil), ck).Body.String()
		for _, want := range wants {
			if !strings.Contains(drawer, want) {
				t.Errorf("the drawer of %s lacks %q:\n%s", id, want, drawer)
			}
		}
	}
	if strings.Contains(body, "old reason") || strings.Contains(body, "finished human work") {
		t.Error("board shows a superseded escalation or a finished human task")
	}
	if strings.Count(body, `/answer" hx-post=`) != 1 {
		t.Error("want exactly one answer box: the escalated agent task")
	}
	frag := r.do(httptest.NewRequest(http.MethodGet, "/board", nil), ck).Body.String()
	if !strings.HasPrefix(frag, `<div id="board"`) || strings.Contains(frag, "<html") || !strings.Contains(frag, "stuck work") {
		t.Fatalf("/board is not the bare board fragment: %.200s", frag)
	}
}

// A done card whose change is not live yet stays in progress, wearing
// where its deploy chain sits (roadmap 297): chain running while the chain
// runs, waiting for the chain until a run carries its change, chain red at
// the failed end states. A done card whose chain went green sits in Done,
// and one that finished before the chain began recording runs does too.
// A done card the chain holds at a not-green verdict — a skipped carry is
// not live — is on no surface at all, invisible until a green head
// covers it. A roll task is no card.
func TestBoardTracksEachCardsDeploy(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	at := func(mins int) *time.Time { t := time.Now().Add(-time.Duration(mins) * time.Minute); return &t }
	card := func(id string, pr int64, state string, finished int, status string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: status, Repository: "sf/code",
			PRNumber: pr, Objective: id + " change", FinishedAt: at(finished)}, ChainState: state}
	}
	// t-era finished before the first chain the set records; t-wait merged
	// after t-chain's run was recorded, so the era no longer holds it live
	r.tasks.tasks = []factoryd.BoardTask{
		card("t-era", 8, "", 90, "done"),
		card("t-chain", 9, "running", 40, "merged"),
		card("t-wait", 10, "", 30, "merged"),
		card("t-red", 11, "failure", 25, "merged"),
		card("t-gone", 12, "cancelled", 20, "merged"),
		card("t-live", 13, "success", 15, "done"),
		card("t-skip", 14, "skipped", 10, "done"),
		{Task: factoryd.Task{ID: "t-rollcard", Kind: "agent", Status: "done", Source: factoryd.SourceRoll,
			Repository: "sf/code", PRNumber: 15, Objective: "the roll itself", FinishedAt: at(35)}},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	progress, done := col(t, body, "In progress"), col(t, body, "Done")
	for id, stage := range map[string]string{
		"t-chain": ">chain running</span>",
		"t-wait":  ">waiting for the chain</span>",
		"t-red":   ">chain red</span>",
		"t-gone":  ">chain red</span>",
	} {
		if !strings.Contains(progress, `data-card="`+id+`"`) || !strings.Contains(progress, stage) {
			t.Errorf("In progress lacks %s wearing %q:\n%s", id, stage, progress)
		}
	}
	for _, id := range []string{"t-era", "t-live"} {
		if !strings.Contains(done, `data-card="`+id+`"`) {
			t.Errorf("Done lacks the live card %s:\n%s", id, done)
		}
		if strings.Contains(progress, `data-card="`+id+`"`) {
			t.Errorf("In progress holds the live card %s", id)
		}
	}
	for _, id := range []string{"t-chain", "t-wait", "t-red", "t-gone"} {
		if strings.Contains(done, `data-card="`+id+`"`) {
			t.Errorf("Done holds %s, whose change is not live", id)
		}
	}
	// a done card the chain holds at a skipped carry is on no surface,
	// invisible until covered
	if strings.Contains(body, `data-card="t-skip"`) {
		t.Error("t-skip rendered: a done card no green chain covers is on no surface")
	}
	if strings.Contains(body, `data-card="t-rollcard"`) {
		t.Error("the roll task rendered a card")
	}
}

// The board names the configuration version factoryd runs and links its
// commit to the tree in Forgejo, as /policy does; without a readable
// policy the line is left out, as the approver check degrades.
func TestBoardShowsTheConfigVersion(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	line := `<a class="lchip" href="https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory" target="_blank" rel="noopener" data-tip="version 4 · c9d98bb0aa11">config</a>`
	for _, path := range []string{"/", "/?view=wide", "/board"} {
		body := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
		if !strings.Contains(body, line) {
			t.Errorf("%s lacks the configuration version line", path)
		}
	}
	frag := r.do(httptest.NewRequest(http.MethodGet, "/board", nil), ck).Body.String()
	if !strings.HasPrefix(frag, `<div id="board"`) || strings.Contains(frag, "<html") {
		t.Fatalf("/board is not the bare board fragment: %.200s", frag)
	}
	r.c.cfg.Policies = noPolicy{r.ledger}
	if body := r.do(httptest.NewRequest(http.MethodGet, "/board", nil), ck).Body.String(); strings.Contains(body, "<dt>configuration</dt>") {
		t.Errorf("board shows a configuration version with no policy read: %.200s", body)
	}
}

func TestNewTaskFilesTheIssueAsTheHumanWithTheTrackLabel(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, s := r.loggedIn(t)
	form := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck).Body.String()
	if !strings.Contains(form, `<option value="priority/p1">`) || strings.Contains(form, "priority/p0") {
		t.Fatalf("form offers the existing priority labels only: %s", form)
	}
	// ruling 179: a picker over the projects agents work on, the first the
	// default; the infra project (agents none) is none of them
	picker := `<option value="ai-software-factory-code">ai-software-factory-code (sf/code)</option><option value="demo-app">demo-app (demo-app/demo-app)</option>`
	if !strings.Contains(form, picker) || strings.Contains(form, "infra") {
		t.Fatalf("form's projects: %s", form)
	}
	// roadmap 225: the track select offers direct, the default, plan where
	// the project's process has a plan track, and human
	want := `<select id="track" name="track">
<option value="direct" selected>Direct</option>
<option value="plan" data-projects="linkshort">Plan</option>
<option value="human">Human</option>
</select>`
	if !strings.Contains(form, want) {
		t.Errorf("the form lacks the track select %q:\n%s", want, form)
	}
	// each track files its label: direct agent, plan plan, human human
	for _, tc := range []struct {
		project, track, priority, body, size string
		labels                               []any
		filed                                string
	}{
		{"ai-software-factory-code", "direct", "", "details\n\nAcceptance:\n- It works", "small", []any{1.0}, "41"},
		{"ai-software-factory-code", "human", "priority/p1", "details", "", []any{2.0, 7.0}, "41"},
		{"linkshort", "plan", "", "details\n\nAcceptance:\n- It works", "small", []any{24.0}, "42"},
		{"linkshort", "direct", "", "details", "", []any{21.0}, "42"},
		{"linkshort", "human", "", "details", "", []any{22.0}, "42"},
	} {
		wantBody := tc.body
		if tc.size != "" {
			wantBody += "\n\nSize: " + tc.size
		}
		r.fg.issues, r.fg.calls = nil, nil
		rec := r.do(post("/tasks", url.Values{"csrf": {s.csrf}, "project": {tc.project}, "title": {" Fix the cache "}, "body": {tc.body},
			"track": {tc.track}, "priority": {tc.priority}, "size": {tc.size}}), ck)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?filed="+tc.filed+"&project="+tc.project {
			t.Fatalf("%s %s: POST /tasks = %d to %q: %s", tc.project, tc.track, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
		got := r.fg.issues[0]
		if got["title"] != "Fix the cache" || got["body"] != wantBody || fmt.Sprint(got["labels"]) != fmt.Sprint(tc.labels) {
			t.Errorf("%s %s: issue %v, want labels %v and body %q", tc.project, tc.track, got, tc.labels, wantBody)
		}
		for _, call := range r.fg.calls {
			if !strings.HasSuffix(call, " Bearer access-1") {
				t.Errorf("%s %s: call %q not made with the human's token", tc.project, tc.track, call)
			}
		}
	}
	noTrack := "track must be direct, plan or human\n"
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no title":  {url.Values{"title": {"  "}, "track": {"direct"}}, "a title of 1 to 255 characters is required\n"},
		"no track":  {url.Values{"title": {"x"}, "project": {"ai-software-factory-code"}}, noTrack},
		"bad track": {url.Values{"title": {"x"}, "track": {"robot"}, "project": {"ai-software-factory-code"}}, noTrack},
		"old lane":  {url.Values{"title": {"x"}, "track": {"ready"}, "project": {"ai-software-factory-code"}}, noTrack},
		"backlog":   {url.Values{"title": {"x"}, "track": {"backlog"}, "project": {"ai-software-factory-code"}}, noTrack},
		"label":     {url.Values{"title": {"x"}, "track": {"agent"}, "project": {"ai-software-factory-code"}}, noTrack},
		"no plan track": {url.Values{"title": {"x"}, "track": {"plan"}, "project": {"ai-software-factory-code"}},
			"Project ai-software-factory-code follows process maintenance, which has no plan track: file the card on track direct.\n"},
		"bad priority": {url.Values{"title": {"x"}, "track": {"direct"}, "priority": {"urgent"}, "project": {"ai-software-factory-code"}}, "unknown priority\n"},
		"no project":   {url.Values{"title": {"x"}, "track": {"direct"}}, "choose a project agents work on\n"},
		"agents none":  {url.Values{"title": {"x"}, "track": {"human"}, "project": {"ai-software-factory-infra"}}, "choose a project agents work on\n"},
	} {
		r.fg.issues = nil
		tc.form.Set("csrf", s.csrf)
		if rec := r.do(post("/tasks", tc.form), ck); rec.Code != http.StatusBadRequest || rec.Body.String() != tc.want || len(r.fg.issues) != 0 {
			t.Errorf("%s: %d %q, want 400 %q and no issue", name, rec.Code, rec.Body, tc.want)
		}
	}
}

// Stage 16 item 3.7: the form asks for a size; a sized card's issue ends
// with its line "Size: <size>", which intake reads for the budget, and its
// event names the size.
func TestNewTaskSize(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	form := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck).Body.String()
	want := `<select id="size" name="size">
<option value="">none</option>
<option value="small">small</option>
<option value="medium">medium</option>
<option value="large">large</option>
</select>`
	if !strings.Contains(form, want) {
		t.Fatalf("form lacks the size select %q: %s", want, form)
	}
	file := func(size, track, body string) *httptest.ResponseRecorder {
		r.fg.issues = nil
		return r.do(post("/tasks", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "title": {"Fix the cache"},
			"body": {body}, "track": {track}, "size": {size}}), ck)
	}
	for _, tt := range []struct{ size, track, body, wantBody string }{
		{"large", "direct", "details\n\nAcceptance:\n- It works", "details\n\nAcceptance:\n- It works\n\nSize: large"},
		{"", "human", "details", "details"},
	} {
		before := ev.count()
		if rec := file(tt.size, tt.track, tt.body); rec.Code != http.StatusSeeOther {
			t.Fatalf("size %q: POST /tasks = %d: %s", tt.size, rec.Code, rec.Body)
		}
		if len(r.fg.issues) != 1 || r.fg.issues[0]["body"] != tt.wantBody {
			t.Fatalf("size %q: issues %v, want one with body %q", tt.size, r.fg.issues, tt.wantBody)
		}
		detail := map[string]any{"track": tt.track, "title": "Fix the cache"}
		if tt.size != "" {
			detail["size"] = tt.size
		}
		checkRows(t, ev, before, wantRow{kind: factoryd.EventCardFiled, repo: "sf/code", detail: detail})
	}
	before := ev.count()
	if rec := file("giant", "direct", "details"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "size must be small, medium or large") {
		t.Fatalf("size giant: POST /tasks = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.issues) != 0 || ev.count() != before {
		t.Fatalf("size giant filed %v and left %d rows", r.fg.issues, ev.count()-before)
	}
}

func TestAnswerPostsTheCommentAsTheHuman(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("cannot reach the database", []factoryd.Task{
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13},
		{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 14},
	}...)
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "use the replica" {
		t.Fatalf("comment = %q", got)
	}
	if last := r.fg.calls[len(r.fg.calls)-1]; last != "POST /api/v1/repos/sf/code/issues/13/comments Bearer access-1" {
		t.Fatalf("last call %q: want the comment as the human", last)
	}
	for _, id := range []string{"t-r", "t-missing"} {
		rec := r.do(post("/tasks/"+id+"/answer", url.Values{"csrf": {s.csrf}, "answer": {"x"}}), ck)
		if rec.Code == http.StatusSeeOther {
			t.Errorf("answer to %s accepted", id)
		}
	}
	if len(r.fg.comments) != 1 {
		t.Fatal("a comment went to a task not waiting for a human")
	}
}

func TestAnswerPostsAnAgentChoiceAsTheMembersWords(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("The agent asks: Ship which?", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13})
	r.tasks.tasks[0].Question = &factoryd.HumanQuestion{Ask: "Ship which?", Choices: []factoryd.QuestionChoice{
		{Value: "say:Ship A", Label: "Ship A"}, {Value: factoryd.AnswerPark, Label: "Leave it waiting"}}}
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "value": {"say:Ship A"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "Ship A" {
		t.Fatalf("comment = %q, want the choice's label", got)
	}
}

// Roadmap 237: the answer box posts one comment, a whole answer, and
// names it to the card's workflow (AnswerWhole), which resumes without
// the quiet window an answer in parts waits out. A failed signal still
// answers: the comment is on the issue, and the window resumes the card.
func TestAnswerNamesItsCommentWhole(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("cannot reach the database", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13})
	var whole []string
	r.c.cfg.AnswerWhole = func(ctx context.Context, taskID string, comment int64, by string) error {
		whole = append(whole, fmt.Sprintf("%s %d %s", taskID, comment, by))
		return errors.New("temporal: unavailable")
	}
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	if !slices.Equal(whole, []string{"t-e 1 sebastian"}) {
		t.Fatalf("whole signals %q, want the posted comment 1 by sebastian", whole)
	}
}

// Roadmap 237: an answer reaches the sessions as at most
// factoryd.MaxAnswer bytes, so the answer box refuses a longer one, naming
// the bound and the answer's length, and posts nothing: 2000 two-byte "ä"
// are 4000 bytes and go, one more byte is refused.
func TestAnswerRefusedOverMaxAnswer(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("cannot reach the database", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13})
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {strings.Repeat("ä", 2000) + "a"}}), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "an answer of 1 to 4000 bytes is required; this one has 4001") {
		t.Fatalf("an answer of 4001 bytes = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.comments) != 0 {
		t.Fatal("the refused answer was posted")
	}
	if rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {strings.Repeat("ä", 2000)}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("an answer of 4000 bytes = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != strings.Repeat("ä", 2000) {
		t.Fatalf("comment = %d bytes, want the 4000-byte answer whole", len(got))
	}
}

// Ruling 322 (finding 215): the answer is refused before it is posted
// when the task's remaining budget cannot fund a call. No comment goes
// to the issue, and the refusal carries the message the card will carry:
// the outstanding holds and the two actions that fit (roadmap 243).
func TestAnswerRefusedWhenTheBudgetCannotFundACall(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("cannot reach the database", []factoryd.Task{
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, ModelAlias: "factory/small"},
	}...)
	refusal := "the remaining budget cannot fund a call: 0.170 EUR of the 5.000 EUR budget remains, outstanding holds take 0.170000 EUR of it (1, the earliest expiring 2026-10-14T19:35:03Z), so 0.000000 EUR is free, and the cheapest call needs more. " +
		"Wait for the holds to settle and answer (a released hold books its call's cost, an expired one is booked in full), or raise the budget (Fund on the board) and answer."
	var asked string
	r.c.cfg.SessionFunded = func(ctx context.Context, taskID, model string) (string, error) {
		asked = taskID + " " + model
		return refusal, nil
	}
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), refusal) {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	if asked != "t-e factory/small" {
		t.Fatalf("the floor read %q", asked)
	}
	if len(r.fg.comments) != 0 {
		t.Fatal("the refusal was commented to the issue: no answer reaches the card")
	}
	// a failed read refuses the answer with its own verdict: the answer
	// never dies mid-request, and nothing reaches the card
	r.c.cfg.SessionFunded = func(ctx context.Context, taskID, model string) (string, error) {
		return "", errors.New("the price book is down")
	}
	if rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "The card's budget could not be read; the answer was not posted. Try again.") {
		t.Fatalf("answer on a failed read = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.comments) != 0 {
		t.Fatal("the refused answer was posted")
	}
}

// An answer at a spent day cap is taken: the day cap resets on its own,
// and the attempt the answer buys waits for the day with no human.
func TestAnswerTakenAtTheDayCap(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("cannot reach the database", []factoryd.Task{
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, ModelAlias: "factory/small"},
	}...)
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) {
		return "the day cap cannot fund a call: Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit.\n" +
			"0.010000 EUR of the day's 20.00 EUR (budget.day_eur) is free", nil
	}
	if rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck); rec.Code != http.StatusSeeOther || len(r.fg.comments) != 1 {
		t.Fatalf("answer at the day cap = %d %s, %d comments; want it taken", rec.Code, rec.Body, len(r.fg.comments))
	}
}

func TestExpiredTokenIsRefreshedAndARefusedRefreshLogsOut(t *testing.T) {
	r := newRig(t)
	r.fg.expiresIn = 30 // under the one-minute margin
	ck, _ := r.loggedIn(t)
	r.fg.expiresIn = 3600 // the refreshed token lasts: one refresh serves every page read
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck); rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/new = %d", rec.Code)
	}
	g := r.fg.grants[len(r.fg.grants)-1]
	if g.Get("grant_type") != "refresh_token" || g.Get("refresh_token") != "refresh-1" || g.Get("client_secret") != "csecret" {
		t.Fatalf("refresh grant %v", g)
	}
	if last := r.fg.calls[len(r.fg.calls)-1]; !strings.HasSuffix(last, "Bearer access-2") {
		t.Fatalf("after refresh: call %q", last)
	}
	if _, refresh := r.tokens(t, ck); refresh != "refresh-2" {
		t.Fatalf("after refresh: the row's refresh token is %q, want refresh-2", refresh)
	}
	r.fg.refuse = true
	r.setTokenExpiry(t, ck, time.Now()) // the refreshed token runs out too
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("refused refresh: %d to %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if len(r.sessions.rows) != 0 {
		t.Fatal("session kept after the refresh was refused")
	}
}

// setTokenExpiry writes the session row's token expiry, as a token about
// to run out would have it.
func (r *rig) setTokenExpiry(t *testing.T, ck *http.Cookie, at time.Time) {
	t.Helper()
	hash := sessionIDHash(ck.Value)
	sealed, err := sealToken(r.c.cfg.SessionKey, "expired-access", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.cfg.Sessions.UpdateConsoleSessionTokens(context.Background(), hash, sealed, "", at); err != nil {
		t.Fatal(err)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/logout", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther || len(r.sessions.rows) != 0 {
		t.Fatalf("logout = %d, %d sessions", rec.Code, len(r.sessions.rows))
	}
	if gone := cookie(t, rec, sessionCookie); gone == nil || gone.MaxAge >= 0 {
		t.Fatalf("session cookie not deleted: %+v", gone)
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("old cookie after logout = %d, want 303", rec.Code)
	}
}

func TestTimelinePageAndStaticAssets(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_started","time":"2026-09-28T10:00:00Z","session":"s-<b>"}` + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if !strings.Contains(body, "session_started") || !strings.Contains(body, "s-\\u003cb\\u003e") && !strings.Contains(body, "s-&lt;b&gt;") {
		t.Fatalf("timeline page: %s", body)
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-none/timeline", nil), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task timeline = %d", rec.Code)
	}
	for _, name := range []string{"htmx.min.js", "console.js", "console.css"} {
		rec := r.do(httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("/static/%s = %d", name, rec.Code)
		}
	}
	if csp := r.do(httptest.NewRequest(http.MethodGet, "/login", nil)).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP %q", csp)
	}
	// img-src carries blob: beside 'self': the chat's previews of the
	// images a member has picked or pasted are object URLs the page
	// itself makes, before the message that stores them is sent
	if csp := r.do(httptest.NewRequest(http.MethodGet, "/login", nil)).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' blob:") {
		t.Errorf("CSP %q: the chat's blob previews stay broken", csp)
	}
}

func TestCostsShowTheLastFourteenDaysAndTodayAgainstTheCap(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	today := time.Now().UTC().Format(policy.DateLayout)
	r.ledger.costs = policy.Costs{From: "2026-09-15", To: today,
		Days: []policy.DayCost{
			{Day: "2026-09-20", ByKind: map[string]float64{"model": 0.5}, Total: 0.5},
			{Day: today, ByKind: map[string]float64{"model": 1.25, "settlement": 0.5, "resource": 2}, Total: 3.75},
		},
		Tasks: []policy.TaskCost{
			{TaskID: "t-big", EUR: 3.25, Lines: []policy.CostLine{{Kind: "model", Ref: "small", EUR: 3.125}, {Kind: "model", Ref: "review/big", EUR: 0.125}}},
			{TaskID: "t-small", EUR: 1}, {TaskID: "t-tiny", EUR: 0.000543}}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/costs", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("/costs = %d: %s", rec.Code, rec.Body)
	}
	if got := r.ledger.to.Sub(r.ledger.from); got != 13*24*time.Hour {
		t.Errorf("range %v to %v: want 14 days, today included", r.ledger.from, r.ledger.to)
	}
	if r.ledger.to.UTC().Format(policy.DateLayout) != today {
		t.Errorf("range ends %v, want today (UTC)", r.ledger.to)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<strong>€3.7500</strong> of the €20.0000 day cap",
		"<th scope=\"row\">" + today + "</th><td class=\"num\">1.2500</td><td class=\"num\">0.0000</td><td class=\"num\">2.0000</td><td class=\"num\">0.5000</td><td class=\"num\">3.7500</td>",
		"<th scope=\"row\">2026-09-20</th><td class=\"num\">0.5000</td>",
		`<a href="/tasks/t-big/timeline">t-big</a></th><td class="num">3.2500</td>`,
		`<tr class="line"><td>model small</td><td class="num">3.1250</td></tr>`,
		`<tr class="line"><td>model review/big</td><td class="num">0.1250</td></tr>`,
		`<a href="/tasks/t-tiny/timeline">t-tiny</a></th><td class="num">0.0005</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs lacks %q", want)
		}
	}
	if strings.Index(body, today) > strings.Index(body, "2026-09-20") {
		t.Error("days are not newest first")
	}
}

// The policy page shows the version in force read-only: its
// commit, the policy, the projects and the processes, each linked to its
// file at that commit.
func TestPolicyShowsTheCurrentVersionAndLinksToGit(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck).Body.String()
	at := "https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory"
	for _, want := range []string{"version 4 · commit ", "2026-09-27T22:50:07Z", "2026-09-27T22:56:38Z", `&#34;day_eur&#34;: 20`,
		`&#34;agent_time&#34;: &#34;20m0s&#34;`,
		`<a href="` + at + `" target="_blank" rel="noopener"><code>c9d98bb0aa11</code></a>`,
		`<a href="` + at + `/policy.yaml" target="_blank" rel="noopener">policy.yaml</a>`,
		`<h3>Process maintenance (<a href="` + at + `/processes/maintenance.yaml" target="_blank" rel="noopener">processes/maintenance.yaml</a>)</h3>`,
		`<a href="` + at + `/projects/demo-app.yaml" target="_blank" rel="noopener">demo-app</a>`,
		`<a href="` + at + `/projects/infra.yaml" target="_blank" rel="noopener">infra</a>`,
		"<td>sf/infra</td><td>human</td><td>seed</td><td>none</td><td></td>",
		"<td>demo-app/demo-app</td><td>auto</td><td>factory</td><td>coder</td><td>demo-staging, demo-prod</td>",
		"<td>screening</td><td>human</td><td><i class=\"lamp\" aria-hidden=\"true\"></i>no</td><td>outsider</td>",
		"<td>checks</td><td>agent</td><td><i class=\"lamp on\" aria-hidden=\"true\"></i>yes</td><td></td>",
		`<a href="https://git.example/sf/infra/src/branch/main/factory" target="_blank" rel="noopener">`} {
		if !strings.Contains(body, want) {
			t.Errorf("/policy lacks %q", want)
		}
	}
	if strings.Contains(body, "<form") && strings.Contains(body[strings.Index(body, "<h2>"):], "<form") {
		t.Error("/policy has a form: the configuration is read-only")
	}
}

func TestBoardShowsApprovalCardsOfBothKinds(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-res", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 21})
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 9}})
	// the human lane's approvals column is the wide view's, its cards as
	// brief as the narrow view's: each title opens its task's drawer,
	// which holds the facts in full
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	start := strings.Index(body, `id="col-approvals"`)
	end := strings.Index(body[start:], "</section>")
	lane := body[start : start+end]
	for _, want := range []string{
		// the resource request: type, purpose, lease, monthly cost, project, the task
		`<article class="card approval" data-card="t-res"`,
		`<a href="#card=t-res" class="open">Resource request</a>`,
		`<p class="need-line"><b>cx33</b> · load test</p>`,
		`<p class="need-line">5 h · 8.9900 EUR per month, this lease 0.0720 EUR · sf/code</p>`,
		"a lease of 300 minutes exceeds the envelope&#39;s 4 h", `href="https://git.example/sf/code/issues/21"`,
		`href="/tasks/t-res/timeline"`, `action="/approvals/3/approve"`, "Approves this task's request once; a wider envelope for the project is a change to its file in Git.",
		// the merge request: the PR, the chips and the forms; the
		// five-part ask lives in the drawer
		`<article class="card approval" data-card="t-m"`,
		`<a href="#card=t-m" class="open">Merge</a> <a href="https://git.example/sf/code/pulls/9" target="_blank" rel="noopener">PR #9</a>`,
		`<span class="chip risk">high risk</span>`, `<span class="chip migration">migration</span>`,
		`<a href="#card=t-m:reviews" class="details">Details</a>`,
		`action="/approvals/4/approve"`, `action="/approvals/4/deny"`, `name="action" value="request_changes" checked`, `name="action" value="reject"`,
		`name="csrf" value="` + s.csrf + `"`, `<label for="note-4">`, `<textarea id="note-4" name="note"`,
	} {
		if !strings.Contains(lane, want) {
			t.Errorf("approval lane lacks %q", want)
		}
	}
	// the drawer carries the merge request's five-part ask: why a
	// human, the model review's verdict and summary, the recommendation
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{`<p class="why">the project&#39;s merge rule is human</p>`, `<p class="why">risk class high is requires_approval</p>`, `<p class="info"><b>Model review</b> approve: adds the column safely</p>`, `<p class="recommendation">Recommendation: Adds the column; merge it.</p>`} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the merge drawer lacks %q", want)
		}
	}
	for _, wall := range []string{"<dl class=\"facts\">", "Risk class", "Earlier review", "Security audit", "Why a human", "Agent's recommendation"} {
		if strings.Contains(lane, wall) {
			t.Errorf("the wide approval card shows the facts wall %q", wall)
		}
	}
	if strings.Contains(lane, "/approvals/5/") || !strings.Contains(lane, `Approvals <span class="count">2</span>`) {
		t.Error("the lane shows a decided request")
	}
}

// Approve submits the human's APPROVED review at the gated head through
// their token, and only then signals the workflow.
func TestMergeApproveReviewsThenSignals(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.calls = nil
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	want := []string{"POST /api/v1/repos/sf/code/pulls/9/reviews Bearer access-1", "SIGNAL approved t-m #9 by sebastian"}
	if fmt.Sprint(r.fg.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %q, want %q", r.fg.calls, want)
	}
	if rv := r.fg.reviews[0]; rv["event"] != "APPROVED" || rv["commit_id"] != "abc123" {
		t.Fatalf("review = %v", rv)
	}
	if r.approvals.get(4).Status != "pending" {
		t.Fatal("the console decided the merge request; factoryd's Merge records the approval once merged")
	}
}

func TestMergeApproveSignalsNothingWhenTheReviewFails(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.calls, r.fg.failReview = nil, true
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("approve with a refused review = %d", rec.Code)
	}
	for _, c := range r.fg.calls {
		if strings.HasPrefix(c, "SIGNAL") {
			t.Fatalf("signalled after a failed review: %q", r.fg.calls)
		}
	}
	// the review went through but the signal did not: say so
	r.fg.failReview, r.signalErr = false, errors.New("temporal unavailable")
	rec = r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "review is on the pull request") {
		t.Fatalf("approve with a failed signal = %d: %s", rec.Code, rec.Body)
	}
}

func TestMergeDenyRequestsChangesOrRejectsAsTheHuman(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.calls = nil
	for name, f := range map[string]url.Values{
		"no reason":  {"action": {"request_changes"}, "note": {"  "}},
		"bad action": {"action": {"merge-anyway"}, "note": {"x"}},
	} {
		f.Set("csrf", s.csrf)
		if rec := r.do(post("/approvals/4/deny", f), ck); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
	if len(r.fg.calls) != 0 || r.approvals.get(4).Status != "pending" {
		t.Fatalf("a refused deny acted: %q", r.fg.calls)
	}
	rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"split the migration"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("request changes = %d: %s", rec.Code, rec.Body)
	}
	if rv := r.fg.reviews[0]; rv["event"] != "REQUEST_CHANGES" || rv["body"] != "split the migration" || rv["commit_id"] != "abc123" ||
		r.fg.calls[0] != "POST /api/v1/repos/sf/code/pulls/9/reviews Bearer access-1" {
		t.Fatalf("review = %v, calls %q", rv, r.fg.calls)
	}
	if a := r.approvals.get(4); a.Status != "denied" || a.DecidedBy != "sebastian" || a.DecisionNote != "changes requested: split the migration" {
		t.Fatalf("request = %+v", a)
	}
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"reject"}, "note": {"x"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("deny of a decided request = %d, want 409", rec.Code)
	}

	r.approvals.rows[1].Status = "pending"
	r.fg.calls = nil
	rec = r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"reject"}, "note": {"not wanted"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reject = %d: %s", rec.Code, rec.Body)
	}
	want := []string{"POST /api/v1/repos/sf/code/issues/9/comments Bearer access-1", "PATCH /api/v1/repos/sf/code/pulls/9 Bearer access-1"}
	if fmt.Sprint(r.fg.calls) != fmt.Sprint(want) || r.fg.closed[0] != "/api/v1/repos/sf/code/pulls/9 closed" ||
		r.fg.comments["/api/v1/repos/sf/code/issues/9/comments"] != "Rejected in the factory console: not wanted" {
		t.Fatalf("calls = %q, closed %q, comments %v", r.fg.calls, r.fg.closed, r.fg.comments)
	}
	if a := r.approvals.get(4); a.Status != "denied" || a.DecisionNote != "rejected: not wanted" {
		t.Fatalf("request = %+v", a)
	}
}

// A factory pull request card's approval offers what fits its head
// (roadmap 264): on red checks a re-run and a close, no Approve, and an
// approve refuses naming them; on green Approve and a close, and a re-run
// refuses. The close is the deny box without a choice of action: no agent
// works on the pull request, so the member's words go to its workflow,
// which closes the card and the pull request with them, whatever branch
// the pull request has; no review is posted.
func TestFactoryPullRequestApprovalFitsItsHead(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	factory := func(red bool) {
		r.approvals.mu.Lock()
		defer r.approvals.mu.Unlock()
		r.approvals.rows[1].Spec = json.RawMessage(fmt.Sprintf(`{"risk_class":"standard","head_sha":"abc123","factory":true,"red_checks":%t,`+
			`"why_human":["factoryd opened this pull request and no agent works on it: the findings meant for an agent go to a human"]}`, red))
	}
	item := func() string {
		t.Helper()
		body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
		start := strings.Index(body, `action="/approvals/4/`)
		if start < 0 {
			t.Fatalf("the board shows no form of request 4:\n%s", body)
		}
		start = strings.LastIndex(body[:start], "<article")
		return body[start : start+strings.Index(body[start:], "</article>")]
	}

	factory(true)
	got := item()
	for _, want := range []string{`action="/approvals/4/rerun"`, `aria-label="Re-run the checks of PR #9 of t-m">Re-run the checks</button>`,
		`aria-controls="deny-4">Close</button>`, `<label for="note-4">Why it closes, posted on its issue and pull request</label>`,
		`aria-label="Close PR #9 of t-m and its card">Close</button>`} {
		if !strings.Contains(got, want) {
			t.Errorf("the red factory request lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{`action="/approvals/4/approve"`, `name="action"`, ">Deny</button>", "Only the project&#39;s approvers may approve."} {
		if strings.Contains(got, not) {
			t.Errorf("the red factory request shows %q:\n%s", not, got)
		}
	}
	r.fg.calls = nil
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Pull request #9's checks were red when its request opened, so it cannot merge: run them again "+
		"(rerun_checks, the card's Re-run the checks) or close it with your words (deny_merge, the card's Close).") ||
		len(r.fg.reviews) != 0 || fmt.Sprint(r.fg.calls) != fmt.Sprint([]string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer access-1", "GET /api/v1/repos/sf/code/pulls/9 Bearer access-1"}) {
		t.Fatalf("approve on red = %d %q, reviews %v, calls %q: want the refusal, no review, the head's read and the pull's", rec.Code, rec.Body, r.fg.reviews, r.fg.calls)
	}
	if rec := r.do(post("/approvals/4/rerun", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("re-run = %d: %s", rec.Code, rec.Body)
	}
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "note": {"the template's CI file is not ours"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body)
	}
	equal(t, "the calls", r.fg.calls, []string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer access-1",
		"GET /api/v1/repos/sf/code/pulls/9 Bearer access-1",
		"SIGNAL rerun_checks t-m #9 by sebastian", "SIGNAL changes_requested t-m #9 by sebastian: the template's CI file is not ours"})

	factory(false)
	got = item()
	if !strings.Contains(got, `action="/approvals/4/approve"`) || !strings.Contains(got, `aria-controls="deny-4">Close</button>`) ||
		strings.Contains(got, `action="/approvals/4/rerun"`) {
		t.Errorf("the green factory request offers no Approve and Close, or a re-run:\n%s", got)
	}
	r.fg.calls = nil
	rec = r.do(post("/approvals/4/rerun", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Pull request #9's checks are green: approve it to merge (approve_merge) or close it (deny_merge).") ||
		len(r.fg.calls) != 0 {
		t.Fatalf("re-run on green = %d %q, calls %q", rec.Code, rec.Body, r.fg.calls)
	}
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"high","head_sha":"abc123"}`)
	rec = r.do(post("/approvals/4/rerun", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "approval request 4 is no merge request of a pull request factoryd opened") ||
		len(r.fg.calls) != 0 {
		t.Fatalf("re-run of an agent's pull request = %d %q, calls %q", rec.Code, rec.Body, r.fg.calls)
	}
}

// An agent card's merge request on red checks (#838: the cap reached, the sync
// with main turned the head red) cannot merge: its card offers the return to
// the agent and the close, no Approve, and an approve refuses naming both.
func TestAgentApprovalOnRedChecksOffersReturnAndClose(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()

	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	start := strings.Index(body, `action="/approvals/4/`)
	if start < 0 {
		t.Fatalf("the board shows no form of request 4:\n%s", body)
	}
	start = strings.LastIndex(body[:start], "<article")
	got := body[start : start+strings.Index(body[start:], "</article>")]
	for _, want := range []string{`action="/approvals/4/deny"`, `name="action" value="request_changes" checked`, "return it to the agent",
		`name="action" value="reject"`, "close it (closes the pull request)", `aria-label="Return or close PR #9 of t-m">Send</button>`} {
		if !strings.Contains(got, want) {
			t.Errorf("the red agent request lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{`action="/approvals/4/approve"`, `action="/approvals/4/rerun"`, ">Deny</button>", `aria-controls="deny-4"`, `id="deny-4" hidden`} {
		if strings.Contains(got, not) {
			t.Errorf("the red agent request shows %q:\n%s", not, got)
		}
	}
	r.fg.calls = nil
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "return it to the agent") || !strings.Contains(rec.Body.String(), "close it") ||
		len(r.fg.reviews) != 0 || fmt.Sprint(r.fg.calls) != fmt.Sprint([]string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer access-1", "GET /api/v1/repos/sf/code/pulls/9 Bearer access-1"}) {
		t.Fatalf("approve on red = %d %q, reviews %v, calls %q: want the refusal naming both ways, no review, the head's read and the pull's", rec.Code, rec.Body, r.fg.reviews, r.fg.calls)
	}
	rec = r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"make the stub match the interface"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("return = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.reviews) != 1 || r.fg.reviews[0]["event"] != "REQUEST_CHANGES" || r.fg.reviews[0]["body"] != "make the stub match the interface" {
		t.Fatalf("reviews = %v: want one REQUEST_CHANGES with the note", r.fg.reviews)
	}
}

// A merge request opened on red checks (#1013: the head healed after a
// re-run) approves once the recorded head's checks are green again — the
// approval re-reads the head's current verdicts, so no spec is written by
// hand — and a head still red keeps the refusal and its remedies.
func TestApproveOnRedChecksOnceTheHeadTurnedGreen(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()

	// the head still red: the refusal stands, and nothing was posted
	r.fg.statuses = map[string]string{"abc123": `{"state":"failure","statuses":[{"context":"ci / test","status":"failure","description":"exit 1"}]}`}
	r.fg.calls = nil
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(),
		"checks were red when its request opened, so it cannot merge: return it to the agent") || len(r.fg.reviews) != 0 {
		t.Fatalf("approve on a still-red head = %d %q, reviews %v", rec.Code, rec.Body, r.fg.reviews)
	}

	// the same head green after the re-run: the approve goes through
	r.fg.statuses = map[string]string{"abc123": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}
	rec = r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve on the healed head = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.reviews) != 1 || r.fg.reviews[0]["event"] != "APPROVED" || r.fg.reviews[0]["commit_id"] != "abc123" {
		t.Fatalf("reviews = %v: want one APPROVED at the recorded head", r.fg.reviews)
	}
	if !slices.Contains(r.fg.calls, "SIGNAL approved t-m #9 by sebastian") {
		t.Fatalf("calls = %q: want the approved signal", r.fg.calls)
	}
}

// A head's skipped checks count green as Forgejo ranks them (16 on: a
// skipped job reports skipped, not success); the worst check decides.
func TestHeadChecksGreenCountsSkippedAsGreen(t *testing.T) {
	r := newRig(t)
	_, s := r.loggedIn(t)
	for _, tc := range []struct {
		combined, test string
		green          bool
	}{
		{"success", "success", true},
		{"skipped", "skipped", true},
		{"failure", "failure", false},
		{"pending", "pending", false},
	} {
		r.fg.statuses = map[string]string{"abc123": fmt.Sprintf(`{"state":%q,"statuses":[{"context":"ci / test","status":%q},{"context":"ci / image-build","status":"skipped"}]}`, tc.combined, tc.test)}
		green, err := r.c.headChecksGreen(context.Background(), actor{s: s}, "sf/code", "abc123")
		if err != nil || green != tc.green {
			t.Fatalf("test %s beside a skipped job: green %t, %v; want %t", tc.test, green, err, tc.green)
		}
	}
}

// The card of a red-checks merge request whose head's checks turned green
// says so and offers the Approve again; still red keeps what fits a red
// head, the return and the close.
func TestHealedRedChecksCardOffersApprove(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()
	item := func() string {
		t.Helper()
		body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
		start := strings.Index(body, `action="/approvals/4/`)
		if start < 0 {
			t.Fatalf("the board shows no form of request 4:\n%s", body)
		}
		start = strings.LastIndex(body[:start], "<article")
		return body[start : start+strings.Index(body[start:], "</article>")]
	}

	r.fg.statuses = map[string]string{"abc123": `{"state":"failure","statuses":[{"context":"ci / test","status":"failure"}]}`}
	if got := item(); strings.Contains(got, `action="/approvals/4/approve"`) || strings.Contains(got, "checks healed after a re-run") {
		t.Errorf("the still-red card offers an approve or says the checks healed:\n%s", got)
	}

	r.fg.statuses = map[string]string{"abc123": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}
	r.fg.calls = nil
	got := item()
	for _, want := range []string{`action="/approvals/4/approve"`, "checks healed after a re-run"} {
		if !strings.Contains(got, want) {
			t.Errorf("the healed card lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{`action="/approvals/4/rerun"`, `aria-controls="deny-4">Return or close</button>`} {
		if strings.Contains(got, not) {
			t.Errorf("the healed card shows %q:\n%s", not, got)
		}
	}
	// the button path reads the head's current checks as the viewer who
	// asked for the board (#1013), the one forge call the read makes
	equal(t, "the head's read", r.fg.calls, []string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer access-1"})
}

// A red-checks merge request whose branch moved past the head it froze
// (#1091: the recorded head stays red, the branch head is green) approves
// on the current head — the head the merge takes, so the review pins it —
// and its card says which head healed it. A moved head still red keeps
// the return and the close.
func TestRedChecksHealOnTheMovedBranchHead(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()
	r.fg.pullHead = "def456"
	r.fg.statuses = map[string]string{
		"abc123": `{"state":"failure","statuses":[{"context":"ci / test","status":"failure"}]}`,
		"def456": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}

	// the card: the chip names the moved head, the Approve is back
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	for _, want := range []string{`action="/approvals/4/approve"`, "branch moved on, its checks green at def456"} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %q", want)
		}
	}

	// the approve: the review pins the moved head
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve on the moved green head = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.reviews) != 1 || r.fg.reviews[0]["event"] != "APPROVED" || r.fg.reviews[0]["commit_id"] != "def456" {
		t.Fatalf("reviews = %v: want one APPROVED at the moved head", r.fg.reviews)
	}

	// a moved head still red: no approve, the return and the close stand
	r2 := newRig(t)
	ck2, _ := r2.loggedIn(t)
	r2.approvals.mu.Lock()
	r2.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r2.approvals.mu.Unlock()
	r2.fg.pullHead = "def456"
	r2.fg.statuses = map[string]string{
		"abc123": `{"state":"failure","statuses":[{"context":"ci / test","status":"failure"}]}`,
		"def456": `{"state":"failure","statuses":[{"context":"ci / test","status":"failure"}]}`}
	body = r2.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck2).Body.String()
	if strings.Contains(body, `action="/approvals/4/approve"`) || strings.Contains(body, "checks green at def456") {
		t.Errorf("the still-red moved head offers an approve or a healed chip")
	}
	if !strings.Contains(body, `action="/approvals/4/deny"`) || strings.Contains(body, `id="deny-4" hidden`) {
		t.Errorf("the still-red moved head lost its return and close")
	}
}

// A red-checks card whose head's current checks cannot be read keeps the
// red face and says so (#1013): a failed read fails closed, and the card
// names it instead of failing silently.
func TestUnreadHealedHeadKeepsTheRedFaceAndSaysSo(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()
	r.fg.failAPI = true
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	if !strings.Contains(body, "The head's current checks could not be read") {
		t.Errorf("the card does not say the head's checks were unread:\n%s", body)
	}
	if strings.Contains(body, "checks healed after a re-run") || strings.Contains(body, `action="/approvals/4/approve"`) {
		t.Errorf("the unread card offers an approve or says the checks healed:\n%s", body)
	}
}

// The agent's board read renders through an actor that carries no
// session: a red-checks merge request's healed-head read
// (#1013) fails closed as HeadUnread instead of following the nil
// session, which crashed factoryd — a member session is not required to
// paint a card, and nothing is asked of Forgejo as a member.
func TestNilActorRedChecksBoard(t *testing.T) {
	r := newRig(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()
	r.fg.statuses = map[string]string{"abc123": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}
	r.fg.calls = nil
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(t.Context(), ctxKey{}, &session{}))
	d, err := r.c.boardOf(req, boardFilter{Project: "ai-software-factory-code", View: viewWide}, actor{})
	if err != nil {
		t.Fatalf("boardOf through a sessionless actor: %v", err)
	}
	i := slices.IndexFunc(d.Approvals, func(k approvalCard) bool { return k.ID == 4 })
	if i < 0 || !d.Approvals[i].HeadUnread || d.Approvals[i].Healed {
		t.Fatalf("the merge card = %+v: want the head unread, not healed", d.Approvals)
	}
	if len(r.fg.calls) != 0 {
		t.Fatalf("the sessionless read asked Forgejo as a member: %q", r.fg.calls)
	}
}

// A factory pull request's merge request on red checks fits its re-run and
// its close, not an approval, wherever a refusal names what fits a card; on
// green checks approve and deny decide it as any merge request.
func TestFittingAFactoryPullRequest(t *testing.T) {
	card := factoryd.Task{ID: "t-f", Status: "in-review", IssueNumber: 36}
	request := func(red bool) []factoryd.ApprovalRequest {
		return []factoryd.ApprovalRequest{{TaskID: "t-f", Kind: factoryd.ApprovalMerge,
			Spec: json.RawMessage(fmt.Sprintf(`{"head_sha":"c1","factory":true,"red_checks":%t}`, red))}}
	}
	equal(t, "on red checks", fitting(card, capFactsFrom(card, 0, "", request(true))),
		"rerun_checks runs its red checks again, or deny_merge closes it with your words")
	equal(t, "on green checks", fitting(card, capFactsFrom(card, 0, "", request(false))), "approve_merge or deny_merge decide its merge request")
}

// An agent pull request's merge request on red checks fits its return and
// its close, not an approval.
func TestFittingAnAgentPullRequestOnRedChecks(t *testing.T) {
	const want = "deny_merge with request_changes returns it to the agent, or with reject closes it"
	req := []factoryd.ApprovalRequest{{TaskID: "t-a", Kind: factoryd.ApprovalMerge, Spec: json.RawMessage(`{"head_sha":"c1","red_checks":true}`)}}
	for _, status := range []string{"in-review", "waiting-human"} {
		card := factoryd.Task{ID: "t-a", Status: status, IssueNumber: 37}
		equal(t, status, fitting(card, capFactsFrom(card, 0, "", req)), want)
	}
}

// A resource request is approved for its task, once; the project's
// envelope is its file's (ruling 183), no click widens it.
func TestResourceApproveForTheTaskAndDeny(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"project"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(3); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "approved for the task" {
		t.Fatalf("request = %+v", a)
	}
	if len(r.resources.calls) != 0 || len(r.fg.calls) != 1 { // the login's GET /user only
		t.Fatalf("a task approve touched %q / %q", r.resources.calls, r.fg.calls)
	}
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409", rec.Code)
	}
	r.approvals.rows[0].Status = "pending"
	rec = r.do(post("/approvals/3/deny", url.Values{"csrf": {s.csrf}, "note": {"use a cx23"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deny = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(3); a.Status != "denied" || a.DecidedBy != "sebastian" || a.DecisionNote != "use a cx23" {
		t.Fatalf("request = %+v", a)
	}
	if rec := r.do(post("/approvals/5/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("approve of a denied request = %d", rec.Code)
	}
}

func TestResourcesPageListsCommissionsAndEnvelopes(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	lease := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	at := lease.Format("2006-01-02 15:04 UTC")
	ended := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	atEnded := ended.Format("2006-01-02 15:04 UTC")
	r.resources.rows = []cloudres.Resource{
		{ID: 10, Kind: "firewall", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", LeaseExpires: lease},
		{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", Size: "cx33", Address: "203.0.113.9",
			LeaseExpires: lease, PricePerHourEUR: 0.0144, AccruedEUR: 0.0288, Purpose: "load test", MonthlyEUR: 8.99},
		{ID: 14, Kind: "volume", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", Size: "20GB",
			LeaseExpires: lease, PricePerHourEUR: 1.144 / 730, MonthlyEUR: 1.144},
		{ID: 12, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 8, State: "live", Size: "cx23", Lifetime: cloudres.LifetimeStanding, PromotedBy: "alice"},
		{ID: 13, Kind: "server", Repo: "sf/code", OwnerRef: "t-old", HoldID: 6, State: "deleted", Size: "cx23", LeaseExpires: ended, ReleasedBy: "sebastian"},
	}
	r.resources.use = map[string]cloudres.EnvelopeUse{"demo-app/demo-app": {OpenServers: 1, MonthEUR: 1.5}}
	r.resources.standing = map[string]cloudres.StandingUse{"demo-app/demo-app": {Count: 1, MonthlyEUR: 5.99}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /resources = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<h3 id="project-sf/code">sf/code</h3>`, `<h3 id="project-demo-app/demo-app">demo-app/demo-app</h3>`, `<h3 id="project-sf/infra">sf/infra</h3>`,
		"<td>cx33</td>", "<td>203.0.113.9</td>", `<td title="` + at + `">in 1 h</td>`, `<tr class="closed">`, `<tr class="standing">`,
		`<td title="` + atEnded + ` · released by sebastian">2 h ago</td>`, "<td class=\"num\">0.0144</td>", "<td class=\"num\">0.0288</td>", "<td><i class=\"lamp on\" aria-hidden=\"true\"></i>live</td>",
		"none: standing (made standing by alice)", "released by sebastian", "<td><i class=\"lamp\" aria-hidden=\"true\"></i>deleted</td>", `href="/tasks/t-res/timeline"`,
		// the commission's catalogue month: cx33 8.49 + IPv4 0.50 + 20 GB x 0.0572 = 10.134, the firewall free
		`<span id="promote-11" class="meta">standing, about €10.1340 a month against the standing block&#39;s €0.00 (€0.0000 standing now)</span>`,
		// sf/code is in no file of this configuration: the policy's default
		"No project file names this repository: the policy's envelope_default applies.",
		"task block €10.00 a month, types cx23, 2 servers at once, leases up to 4 h, volumes up to 20 GB · this month €0.0000 used or reserved, 0 servers open",
		// demo-app's file, read-only, linked at the version's commit
		`The project's file sets it: <a href="https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory/projects/demo-app.yaml" target="_blank" rel="noopener">change it in Git</a>.`,
		"task block €30.00 a month, types cx23, cx33, 3 servers at once, leases up to 8 h, volumes up to 0 GB",
		"this month €1.5000 used or reserved, 1 server open",
		// the standing monthly line (ruling 147)
		"standing block €12.00 a month, types cx23, 2 servers · standing now 1 server, €5.9900 a month",
		`name="csrf" value="` + s.csrf + `"`, `<a href="/resources" aria-current="page">Resources</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/resources lacks %q", want)
		}
	}
	if strings.Contains(body, "<form method=\"post\" action=\"/envelopes\"") || strings.Contains(body, "monthly_eur") {
		t.Error("/resources offers an envelope edit")
	}
	// one promote (the unpromoted open commission's server) and a release
	// per open commission; none for the closed one
	if strings.Count(body, `/promote"`) != 1 || !strings.Contains(body, `action="/resources/11/promote"`) ||
		strings.Count(body, `/release"`) != 2 || !strings.Contains(body, `action="/resources/12/release"`) || strings.Contains(body, "/resources/13/") {
		t.Errorf("buttons: %d promote, %d release", strings.Count(body, `/promote"`), strings.Count(body, `/release"`))
	}
}

// Promote and release record who; the promote is judged against the
// standing block of the project's file in force, the default's for a
// repository no file names; no envelope edit exists.
func TestPromoteAndReleaseRecordWho(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/resources/11/promote", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/resources" {
		t.Fatalf("promote = %d", rec.Code)
	}
	if rec := r.do(post("/resources/12/release", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("release = %d", rec.Code)
	}
	if rec := r.do(post("/resources/404/release", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("release of a closed resource = %d", rec.Code)
	}
	if rec := r.do(post("/resources/11/promote", url.Values{}), ck); rec.Code != http.StatusForbidden {
		t.Fatalf("promote without the form token = %d", rec.Code)
	}
	if want := "[promote 11 by sebastian under {0 [] 0} and {12 [cx23] 2} release 12 by sebastian release 404 by sebastian]"; fmt.Sprint(r.resources.calls) != want {
		t.Fatalf("calls = %q", r.resources.calls)
	}
	if rec := r.do(post("/envelopes", url.Values{"csrf": {s.csrf}, "project": {"sf/code"}}), ck); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /envelopes = %d, want no such route", rec.Code)
	}
}

// serverState counts the entries of every map the console holds.
func serverState(c *Console) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	v := reflect.ValueOf(c).Elem()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Map {
			n += f.Len()
		}
	}
	return n
}

// Ruling 128: only the policy's approvers may spend (approve a resource or
// a merge, edit an envelope, promote); other writers see the cards without
// those buttons, get 403, and may still deny and release.
func TestSpendActionsNeedAPolicyApprover(t *testing.T) {
	r := newRig(t)
	r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
	r.forgetWriters()
	ck, s := r.loggedIn(t)
	r.resources.rows = []cloudres.Resource{{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live",
		LeaseExpires: time.Now().Add(time.Hour)}}
	// alice writes the code project, no other: she asks for it
	board := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if strings.Contains(board, "/approve\"") || !strings.Contains(board, `action="/approvals/3/deny"`) ||
		!strings.Contains(board, "Only the project's approvers may approve.") {
		t.Fatalf("board for a non-approver: approve buttons %v, deny %v", strings.Contains(board, "/approve\""),
			strings.Contains(board, "/deny\""))
	}
	page := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck).Body.String()
	if strings.Contains(page, "/promote\"") || !strings.Contains(page, "/resources/11/release\"") {
		t.Fatal("resources for a non-approver offer promote, or lack release")
	}
	r.fg.calls = nil
	for _, req := range []*http.Request{
		post("/approvals/3/approve", url.Values{"csrf": {s.csrf}}),
		post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}),
		post("/resources/11/promote", url.Values{"csrf": {s.csrf}}),
	} {
		if rec := r.do(req, ck); rec.Code != http.StatusForbidden {
			t.Errorf("%s by a non-approver = %d, want 403", req.URL.Path, rec.Code)
		}
	}
	if len(r.fg.calls) != 0 || len(r.resources.calls) != 1 || r.approvals.get(3).Status != "pending" || r.approvals.get(4).Status != "pending" {
		t.Fatalf("a refused spend acted: forgejo %q, resources %q", r.fg.calls, r.resources.calls)
	}
	if rec := r.do(post("/resources/11/release", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("release by a writer = %d", rec.Code)
	}
	// a deny needs no approver, a writer of the request's project
	if rec := r.do(post("/approvals/3/deny", url.Values{"csrf": {s.csrf}, "note": {"no"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("deny by a writer = %d", rec.Code)
	}
}

// A merge Forgejo refused after the approve says so on its card.
func TestMergeCardShowsWhyItHasNotMerged(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.rows[1].DecisionNote = "waiting: Forgejo refused the merge after sebastian's approval: status 405"
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `<p class="problem" role="status">waiting: Forgejo refused the merge after sebastian&#39;s approval: status 405</p>`) {
		t.Fatal("the card does not show the refusal")
	}
}

// Pages say what failed in a fixed sentence; the details go to the log.
func TestErrorsShowNoInternals(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/resources/410/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "lease has ended or it was released") {
		t.Fatalf("promote after release = %d: %s", rec.Code, rec.Body)
	}
	// beyond the standing block: the human sees which bound and raises it first
	rec = r.do(post("/resources/420/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "server type cx53 is not in the standing types [cx23]; raise the standing block in the project's file first") {
		t.Fatalf("promote beyond the standing block = %d: %s", rec.Code, rec.Body)
	}
	rec = r.do(post("/resources/500/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("a store error = %d: %s", rec.Code, rec.Body)
	}
}

// Each repeated button names its row for screen readers.
func TestRepeatedButtonsNameTheirRow(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.resources.rows = []cloudres.Resource{{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live",
		Size: "cx33", LeaseExpires: time.Now().Add(time.Hour)}}
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	page := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck).Body.String()
	for _, want := range []string{
		`aria-label="Approve resource request 3 of t-res"`, `aria-label="Deny resource request 3 of t-res"`,
		`aria-label="Approve merge PR #9 of t-m"`, `aria-label="Deny merge PR #9 of t-m"`,
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %s", want)
		}
	}
	for _, want := range []string{`aria-label="Promote cx33 server 11 of t-res"`, `aria-label="Release cx33 server 11 of t-res"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/resources lacks %s", want)
		}
	}
}

// The writer list is fetched once for concurrent requests when it expires,
// and a Forgejo error serves the last good list for up to five minutes.
func TestWriterListRefreshIsSharedAndSurvivesABlip(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.writers.mu.Lock()
	r.writers.asked, r.writers.delay = 0, 50*time.Millisecond
	r.writers.mu.Unlock()
	r.c.mu.Lock()
	r.c.writersT = time.Time{}.Add(time.Hour) // expired, yet a list is known
	r.c.mu.Unlock()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck)
		}()
	}
	wg.Wait()
	if r.writers.asked != 1 {
		t.Fatalf("20 concurrent requests fetched the writers %d times", r.writers.asked)
	}
	r.writers.delay, r.writers.err = 0, errors.New("forgejo: 502")
	r.c.mu.Lock()
	r.c.writersT = time.Now().Add(-4 * time.Minute)
	r.c.mu.Unlock()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck); rec.Code != http.StatusOK {
		t.Fatalf("a blip with a 4-minute-old list = %d, want the stale list served", rec.Code)
	}
	r.c.mu.Lock()
	r.c.writersT = time.Now().Add(-6 * time.Minute)
	r.c.mu.Unlock()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a blip with a 6-minute-old list = %d, want 503", rec.Code)
	}
}

func TestResponsesCarryHSTS(t *testing.T) {
	r := newRig(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("Strict-Transport-Security = %q", got)
	}
}

// The board card shows how long the task has taken in all and the gate
// it is in with the gate's age; a finished task its total only.
func TestBoardCardShowsTotalAndCurrentGate(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	asked, since := now.Add(-(2*time.Hour + 5*time.Minute + 30*time.Second)), now.Add(-(25*time.Minute + 30*time.Second))
	finished := asked.Add(47*time.Minute + 10*time.Second)
	r.tasks.set("", factoryd.Task{ID: "t-c", Kind: "agent", Status: "in-review", Objective: "checking work", Repository: "sf/code",
		CreatedAt: asked, Gate: factoryd.GateChecks, GateSince: &since},
		factoryd.Task{ID: "t-d", Kind: "agent", Status: "done", Objective: "done work", Repository: "sf/code",
			CreatedAt: asked, FinishedAt: &finished})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<span class="elapsed">25 min</span>`, "47 min in all"} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c/drawer", nil), ck).Body.String(); !strings.Contains(drawer, "<dd>verifying, 25 min, 2 h 5 min in all</dd>") {
		t.Errorf("the drawer's Status lacks the stage's time and the total:\n%s", drawer)
	}
	if strings.Count(body, `class="gate"`) != 0 {
		t.Error("a card's face shows its gate")
	}
}

// A card in waiting-deps sits in the wide view's Queued column and reads
// the issues it waits on.
func TestBoardCardShowsWaitingOn(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-w", Kind: "agent", Status: "waiting-deps", Track: "direct", Objective: "links api",
		Repository: "sf/code", IssueNumber: 7}, WaitingOn: "#3, #5"}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	if c := col(t, body, "Queued"); !strings.Contains(c, "links api") || !strings.Contains(c, `<span class="chip waiting">waiting on #3, #5</span>`) {
		t.Fatalf("Queued column = %s, want the card waiting on #3, #5", c)
	}
}

// A card waiting in its project's merge queue reads how many entries are
// ahead of it and since when it waits.
func TestBoardCardShowsMergeQueuePlace(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	since := time.Now().Add(-(6*time.Minute + 30*time.Second))
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "in-review", Objective: "queued work",
		Repository: "linkshort/linkshort", CreatedAt: since, Gate: factoryd.GateMergeQueue, GateSince: &since}, QueueAhead: 2}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if strings.Contains(body, "merge queue") {
		t.Fatalf("the face names the merge queue:\n%s", faceOf(t, body, "t-q"))
	}
	if drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-q/drawer?tab=overview", nil), ck).Body.String(); !strings.Contains(drawer, "<dd>verifying, 6 min, merge queue, 2 ahead, 6 min in all</dd>") {
		t.Fatalf("the drawer's Status lacks the merge queue place:\n%s", drawer)
	}
}

// The timeline page opens with a table of the gates: each step that ended
// a gate is a row from its start, the task's open gate a row "open for N
// min", CI jobs rows under their checks, and every bar is scaled to the
// longest gate.
func TestTimelineDurationsTable(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Now().Add(-(12*time.Minute + 20*time.Second))
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work",
		Gate: factoryd.GateApproval, GateSince: &since})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(strings.Join([]string{
		`{"step":"intake","time":"2026-09-28T10:00:30Z","started":"2026-09-28T10:00:00Z","source":"webhook"}`,
		`{"step":"session_start","time":"2026-09-28T10:01:00Z","attempt":1}`,
		`{"step":"session_end","time":"2026-09-28T10:21:00Z","started":"2026-09-28T10:01:00Z","attempt":1,"outcome":"no_push"}`,
		`{"step":"session_end","time":"2026-09-28T10:30:00Z","started":"2026-09-28T10:22:00Z","attempt":2,"outcome":"pushed"}`,
		`{"step":"pushed","time":"2026-09-28T10:30:01Z","commit":"c1"}`,
		`{"step":"checks","time":"2026-09-28T10:37:00Z","started":"2026-09-28T10:30:05Z","state":"success",` +
			`"jobs":[{"name":"test","status":"success","queue_s":30,"run_s":258},{"name":"vet","status":"success","run_s":146}]}`,
		`{"step":"gate_reproduction","time":"2026-09-28T10:39:00Z","started":"2026-09-28T10:37:10Z","exit":1,"command":"make x"}`,
		`{"step":"signal","time":"2026-09-28T10:40:00Z","signal":"pr_merged","author":"x"}`,
		`{"step":"approval_requested","time":"2026-09-28T10:41:00Z","approval":3}`,
	}, "\n") + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	table := body[strings.Index(body, "<caption>Gates"):]
	table = table[:strings.Index(table, "</table>")]
	var rows []string
	for _, tr := range strings.Split(table, "<tr>")[2:] {
		cells := regexp.MustCompile(`<t[dh][^>]*>(.*?)</t[dh]>`).FindAllStringSubmatch(tr, -1)
		var row []string
		for _, c := range cells {
			row = append(row, c[1])
		}
		rows = append(rows, strings.Join(row, " | "))
	}
	// the longest is the first session: 20 min, 1200 s
	want := []string{
		`intake |  | <time>2026-09-28 10:00:00</time> | 30 s | webhook | <meter min="0" max="1200" value="30">30 s</meter>`,
		`session | 1 | <time>2026-09-28 10:01:00</time> | 20 min | no_push | <meter min="0" max="1200" value="1200">20 min</meter>`,
		`session | 2 | <time>2026-09-28 10:22:00</time> | 8 min | pushed | <meter min="0" max="1200" value="480">8 min</meter>`,
		`checks | 2 | <time>2026-09-28 10:30:05</time> | 6 min 55 s | success | <meter min="0" max="1200" value="415">6 min 55 s</meter>`,
		`job test | 2 |  | 4 min 18 s | success, queued 30 s | <meter min="0" max="1200" value="258">4 min 18 s</meter>`,
		`job vet | 2 |  | 2 min 26 s | success | <meter min="0" max="1200" value="146">2 min 26 s</meter>`,
		`reproduction | 2 | <time>2026-09-28 10:37:10</time> | 1 min 50 s | failed (exit 1) | <meter min="0" max="1200" value="110">1 min 50 s</meter>`,
		`approval | 2 | <time>` + since.UTC().Format("2006-01-02 15:04:05") + `</time> | open for 12 min | open | <meter min="0" max="1200" value="740">open for 12 min</meter>`,
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("gate rows:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(body, "approval_requested") || strings.Contains(body, "style=") {
		t.Fatal("the raw steps are gone, or an inline style crept in")
	}
}

// A gate that closed after the task's row entered it is not open.
func TestTimelineClosedGateIsNotOpen(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Date(2026, 9, 28, 10, 30, 5, 0, time.UTC)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work",
		Gate: factoryd.GateChecks, GateSince: &since})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"checks","time":"2026-09-28T10:37:00Z","started":"2026-09-28T10:30:05Z","state":"failure"}` + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if strings.Contains(body, "open for") || !strings.Contains(body, "<td>failure</td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// /alerts lists the alerts newest first; an open human alert takes a
// verdict from an approver, on the page and on the escalation's card.
// The alerts page is gone: the open human escalation's verdict buttons
// ride its board card, and POST /alerts/{id}/verdict keeps its rules.
func TestAlertVerdicts(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	raised := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	resolved := raised.Add(time.Minute)
	r.alerts.rows = []factoryd.Alert{
		{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e", RaisedAt: raised.Add(2 * time.Hour)},
		{ID: 2, Kind: factoryd.AlertSupervisorTrip, Source: "same_failure", TaskID: "t-e", RaisedAt: raised.Add(time.Hour),
			ResolvedAt: &resolved, Verdict: factoryd.VerdictNoise, ResolvedBy: "supervisor"},
		{ID: 1, Kind: factoryd.AlertGateBound, Source: "WaitChecks", TaskID: "t-c", RaisedAt: raised, BoundS: 600},
	}
	r.tasks.set("stuck", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", IssueNumber: 13})
	r.tasks.tasks[0].EscalationAlert = 3
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/alerts", nil), ck); rec.Code != http.StatusNotFound {
		t.Errorf("GET /alerts = %d, want 404: the page is gone", rec.Code)
	}
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Count(board, `action="/alerts/3/verdict"`) != 2 {
		t.Error("the escalation card lacks the two verdict buttons")
	}
	if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"maybe"}}), ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown verdict = %d, want 400", rec.Code)
	}
	if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"real"}, "back": {"/"}}), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("verdict = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"noise"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("a second verdict = %d, want 409", rec.Code)
	}
	if rec := r.do(post("/alerts/1/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"noise"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("a verdict on a machine alert = %d, want 409", rec.Code)
	}
	if !slices.Equal(r.alerts.verdicts, []string{"3 real sebastian"}) {
		t.Fatalf("verdicts = %q", r.alerts.verdicts)
	}
}

// Only the factory's approvers judge alerts: no buttons on the board,
// and a 403.
func TestAlertVerdictNeedsAnApprover(t *testing.T) {
	r := newRig(t)
	r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
	r.forgetWriters()
	ck, s := r.loggedIn(t)
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e", RaisedAt: time.Now()}}
	r.tasks.set("stuck", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", IssueNumber: 13})
	r.tasks.tasks[0].EscalationAlert = 3
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(board, "/verdict") {
		t.Fatal("a non-approver is offered the verdict buttons")
	}
	for _, v := range []string{"real", "maybe"} {
		if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {v}}), ck); rec.Code != http.StatusForbidden {
			t.Fatalf("verdict %q by a non-approver = %d, want 403", v, rec.Code)
		}
	}
	if len(r.alerts.verdicts) != 0 {
		t.Fatalf("verdicts = %q", r.alerts.verdicts)
	}
}

// A session_end from before attempts were recorded leaves the attempt
// empty, for its row and the rows after it.
func TestTimelineWithoutAttemptsShowsNone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_end","time":"2026-09-28T10:21:00Z","outcome":"pushed"}
{"step":"checks","time":"2026-09-28T10:37:00Z","started":"2026-09-28T10:30:05Z","state":"success"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if strings.Contains(body, "nil") || !strings.Contains(body, "<td>checks</td><td></td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// Commissioning ends when the server went live, not when the lease loop
// put the step on the timeline.
func TestTimelineCommissioningEndsAtLive(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"commissioned","time":"2026-09-28T10:01:44Z","started":"2026-09-28T10:00:00Z","live":"2026-09-28T10:01:35Z","server_type":"cx23"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if !strings.Contains(body, "<td>commissioning</td><td></td><td><time>2026-09-28 10:00:00</time></td><td>1 min 35 s</td><td>cx23</td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// A running session's open row names its attempt from session_start.
func TestTimelineOpenSessionShowsItsAttempt(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Now().Add(-3 * time.Minute)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work",
		Gate: factoryd.GateSession, GateSince: &since})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_start","time":"` + since.UTC().Format(time.RFC3339) + `","attempt":3}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if !strings.Contains(body, "<td>session</td><td>3</td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// A close, a merge in Forgejo or a change request ends the approval wait
// as well as an approval, each with its own outcome.
func TestTimelineApprovalWaitOutcomes(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "failed", Repository: "sf/code", Objective: "work"})
	var lines []string
	for _, s := range []string{`"signal":"approved","author":"sebastian"`, `"signal":"changes_requested","author":"alice"`,
		`"signal":"pr_merged","author":"bob"`, `"signal":"pr_closed","author":"bob"`, `"signal":"issue_closed","author":"sebastian"`} {
		lines = append(lines, `{"step":"signal","time":"2026-09-28T12:00:00Z","started":"2026-09-28T10:00:00Z",`+s+`}`)
	}
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(strings.Join(lines, "\n") + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	for _, want := range []string{"approved by sebastian", "changes requested by alice", "merged by bob", "closed"} {
		if !strings.Contains(body, "<td>2 h 0 min</td><td>"+want+"</td>") {
			t.Errorf("durations lack a 2 h approval wait ending %q", want)
		}
	}
	if n := strings.Count(body, "<td>2 h 0 min</td><td>closed</td>"); n != 2 {
		t.Errorf("%d closed approval waits, want 2 (the pull request's and the issue's)", n)
	}
}

// A machine alert shows what the supervisor did and why; a human
// escalation's detail stays off the page.
// The chat's alerts read carries the supervisor's action and diagnosis
// on resolved machine alerts; an escalation's detail stays private.
func TestAlertsReadTheSupervisorsActionAndDiagnosis(t *testing.T) {
	r, _, g := newChatRig(t)
	raised := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	r.alerts.rows = []factoryd.Alert{
		{ID: 3, Kind: factoryd.AlertEscalation, Source: "supervisor", TaskID: "t-e", RaisedAt: raised, Detail: map[string]any{"reason": "a private reason"}},
		{ID: 2, Kind: factoryd.AlertSupervisorTrip, Source: "no_write", TaskID: "t-e", RaisedAt: raised, Verdict: factoryd.VerdictReal, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "redirect", "diagnosis": "stop rereading the spec; write the handler"}},
		{ID: 1, Kind: factoryd.AlertGateBound, Source: "WaitChecks", TaskID: "t-c", RaisedAt: raised, BoundS: 30, Verdict: factoryd.VerdictReal, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "escalate", "diagnosis": "no runner serves the label", "recommendation": "register a runner"}},
	}
	text := strings.Join(reads(t, r, g, "alerts", `{"state":"all"}`), "")
	for _, want := range []string{
		"verdict real by supervisor, redirect: stop rereading the spec; write the handler",
		"verdict real by supervisor, escalate: no runner serves the label Recommendation: register a runner"} {
		if !strings.Contains(text, want) {
			t.Errorf("the alerts read lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "a private reason") {
		t.Error("the alerts read shows an escalation's detail")
	}
}

// The timeline names the supervisor's action and diagnosis on its steps:
// the session supervisor's decision and the gate supervisor's.
func TestTimelineShowsTheSupervisorsAction(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"supervisor","time":"2026-09-28T10:10:00Z","detector":"no_write","decision":"continue","reason":"it is compiling"}
{"step":"gate_supervisor","time":"2026-09-28T10:40:00Z","activity":"WaitChecks","action":"wait","diagnosis":"two checks run","recommendation":""}
{"step":"checks","time":"2026-09-28T10:45:00Z","state":"success"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	for _, want := range []string{`<th scope="col">Supervisor</th>`,
		"<td>supervisor</td><td><strong>continue</strong>: it is compiling</td>",
		"<td>gate_supervisor</td><td><strong>wait</strong>: two checks run</td>",
		"<td>checks</td><td></td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline lacks %q", want)
		}
	}
}

// The steps table reads its time as the gates table reads its start,
// seconds in UTC, with the raw stamp as the cell's title; a stamp that
// does not parse stays as it is on the wire.
func TestTimelineStepsTimeReadsLikeTheGates(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_start","time":"2026-10-17T12:41:47.048126987Z","attempt":1}
{"step":"session_end","time":"not a time","outcome":"pushed"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	for _, want := range []string{
		`<td title="2026-10-17T12:41:47.048126987Z"><time>2026-10-17 12:41:47</time></td>`,
		`<td>not a time</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the steps table lacks %s", want)
		}
	}
}

// A roll's card sits in the human lane only when its merge decision is
// the one remaining action (ruling 219): at a pending merge approval or
// in a waiting-human state; while its checks or review run the status
// mapping places it in the verifying column. A roll task is a card like
// any other (Sebastian, 2026-10-20); at its merge decision the approval
// card stands in needs you in its place.
func TestBoardShowsARollLikeAnyCard(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", []factoryd.Task{
		{ID: "t-checks", Kind: "agent", Source: factoryd.SourceRoll, Status: "in-review", Objective: "roll c1c1c1c: factoryd, gateway", Repository: "sf/code", PRNumber: 7},
		// the rig's pending merge approval (ID 4) cards t-m: its merge
		// decision is the one remaining action
		{ID: "t-m", Kind: "agent", Source: factoryd.SourceRoll, Status: "in-review", Objective: "roll c2c2c2c: worker", Repository: "sf/code", PRNumber: 9},
		{ID: "t-branch", Kind: "agent", Source: factoryd.SourceRoll, Status: "running", Objective: "roll c3c3c3c: console", Repository: "sf/code"},
	}...)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	progress := col(t, body, "In progress")
	for _, want := range []string{"roll c1c1c1c", "roll c3c3c3c"} {
		if !strings.Contains(progress, want) {
			t.Errorf("In progress lacks %s:\n%s", want, progress)
		}
	}
	needs := col(t, body, "Needs you")
	if !strings.Contains(needs, `id="approval-4"`) {
		t.Errorf("the pending merge approval of roll c2c2c2c is not in needs you:\n%s", needs)
	}
	if strings.Contains(progress, "roll c2c2c2c") {
		t.Errorf("the approval's roll task rendered a card in progress too:\n%s", progress)
	}
}

// interventions reads factory.human.intervention by kind, process and
// track.
func (r *rig) interventions(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "factory.human.intervention" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				kind, _ := dp.Attributes.Value(attribute.Key("kind"))
				process, _ := dp.Attributes.Value(attribute.Key("process"))
				track, _ := dp.Attributes.Value(attribute.Key("track"))
				out[kind.AsString()+" "+process.AsString()+" "+track.AsString()] = dp.Value
			}
		}
	}
	return out
}

// The console counts only the verdict click, once, when it took effect:
// its merge-card decisions act as the human in Forgejo and are counted by
// the intake, like the same decisions made in Forgejo.
func TestConsoleCountsInterventions(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.failReview = true
	r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	r.fg.failReview = false
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d", rec.Code)
	}
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"split it"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("request changes = %d", rec.Code)
	}
	r.approvals.rows[1].Status = "pending"
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"reject"}, "note": {"not wanted"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject = %d", rec.Code)
	}
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"task"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("resource approve = %d", rec.Code)
	}
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"}}
	for range 2 {
		r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"real"}}), ck)
	}
	want := map[string]int64{"alert_verdict_click none none": 1}
	if got := r.interventions(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("interventions = %v, want %v", got, want)
	}
}
