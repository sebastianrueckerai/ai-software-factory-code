package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// PolicyStore holds the loaded configuration versions, the holds and the
// cost ledger.
type PolicyStore interface {
	// RecordPolicy stores tree under commit unless known and activates it.
	RecordPolicy(ctx context.Context, commit string, tree policy.Tree) (policy.Version, bool, error)
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
	// PolicyVersion is version id, the one a task pinned.
	PolicyVersion(ctx context.Context, id int64) (policy.Version, bool, error)
	Costs(ctx context.Context, from, to time.Time) (policy.Costs, error)
	// SweepHolds books the holds whose caller never released them.
	SweepHolds(ctx context.Context) (int, error)
	// OpenCardsByLane are the open cards that sit in a lane.
	OpenCardsByLane(ctx context.Context) ([]OpenCard, error)
}

// OpenCard is an open card in a lane: the project its repository belongs
// to, its lane, the process it follows and its issue.
type OpenCard struct {
	Project, Lane, Process string
	Issue                  int64
}

// ConfigSource reads the configuration tree (ruling 183): the head commit
// of its branch, and the tree's files at a commit keyed by their path
// relative to the tree's directory, schema/ left out.
type ConfigSource interface {
	Head(ctx context.Context) (string, error)
	Files(ctx context.Context, commit string) (map[string][]byte, error)
}

// PolicyLoader turns commits of the configuration tree into versions: each
// new valid tree becomes an immutable row and the version in force; an
// invalid one is logged and raised (Rejected), and the previous version
// stays in force. A version must let the agent capability classes call
// every model of the ladder (LadderModels), else a task would be refused
// mid-run.
type PolicyLoader struct {
	Source       ConfigSource
	Store        PolicyStore
	LadderModels []string
	// Rejected raises a refused commit, once per commit: the loader asks
	// again at the next poll while it fails. nil: logged only.
	Rejected func(ctx context.Context, commit string, reason error, current policy.Version, loaded bool) error
	// Loaded settles a loaded head once (the open rejections close): the
	// loader asks again at the next poll while it fails. nil: nothing.
	Loaded func(ctx context.Context, v policy.Version) error
	// ReadFailing raises readFailureStreak failed reads in a row, once a
	// streak. nil: logged only.
	ReadFailing func(ctx context.Context, err error) error

	mu      sync.Mutex
	current policy.Version
	loaded  bool
	seen    string // the commit last handled, valid or not
	settled string // the loaded commit Loaded last succeeded for
	failed  int    // failed reads in a row
	// held is the valid head open cards refused, its tree and the lines it
	// was refused with: judged again at each call without a second read
	held      string
	heldTree  policy.Tree
	heldLines string
}

// readFailureStreak is how many failed reads in a row, polled every 30 s,
// raise an alert: about five minutes of a factory deaf to its
// configuration.
const readFailureStreak = 10

// readFailed counts a failed read and raises the streak once.
func (l *PolicyLoader) readFailed(ctx context.Context, what string, err error) error {
	log.Printf("config: %s: %v", what, err)
	l.mu.Lock()
	l.failed++
	raise := l.failed == readFailureStreak
	l.mu.Unlock()
	if raise && l.ReadFailing != nil {
		if rerr := l.ReadFailing(ctx, err); rerr != nil {
			log.Printf("config: raise %d failed reads: %v", readFailureStreak, rerr)
			l.mu.Lock()
			l.failed--
			l.mu.Unlock()
		}
	}
	return err
}

// Boot makes the stored current version current, then loads the head. It
// fails only when no valid version exists at all. The stored version is
// validated again: one written under an older schema is not a valid
// version for this binary.
func (l *PolicyLoader) Boot(ctx context.Context) error {
	v, ok, err := l.Store.CurrentPolicy(ctx)
	if err != nil {
		return fmt.Errorf("read current policy: %w", err)
	}
	if ok {
		err := v.Validate()
		if err == nil {
			err = fitsLadder(v.Tree, l.LadderModels)
		}
		if err != nil {
			log.Printf("CONFIG REJECTED: stored version %d (commit %s): %v", v.ID, v.Commit, err)
			ok = false
		} else {
			l.mu.Lock()
			l.current, l.loaded = v, true
			l.mu.Unlock()
		}
	}
	if err := l.Load(ctx); err != nil && !ok {
		return fmt.Errorf("no valid configuration version: %w", err)
	}
	return nil
}

// Load reads the head commit and records its tree if the commit changed
// since the last call. It logs every failure itself; a failure to read is
// tried again at the next call, a refused tree is not, unless open cards in
// a lane it drops refused it (CheckOpenLanes): that head's tree is kept and
// its cards read again at each call, logged and raised only when the lines
// change.
func (l *PolicyLoader) Load(ctx context.Context) error {
	head, err := l.Source.Head(ctx)
	if err != nil {
		return l.readFailed(ctx, "read the head", err)
	}
	l.mu.Lock()
	seen, cur, loaded := l.seen, l.current, l.loaded
	held, heldTree, heldLines := l.held, l.heldTree, l.heldLines
	l.failed = 0
	l.mu.Unlock()
	if head == seen {
		return l.settle(ctx, head)
	}
	var tree policy.Tree
	if head == held {
		tree = heldTree
	} else {
		files, err := l.Source.Files(ctx, head)
		if err != nil {
			return l.readFailed(ctx, "read commit "+head, err)
		}
		tree, err = CheckTree(files, l.LadderModels)
		if err != nil {
			if l.reject(ctx, head, err, cur, loaded) {
				l.mu.Lock()
				l.seen = head
				l.mu.Unlock()
			}
			return err
		}
	}
	cards, err := l.Store.OpenCardsByLane(ctx)
	if err != nil {
		log.Printf("config: commit %s: open cards per lane: %v", head, err)
		return fmt.Errorf("open cards per lane: %w", err)
	}
	if lines := CheckOpenLanes(tree, cards); len(lines) > 0 {
		err := errors.New(strings.Join(lines, "\n"))
		if head == held && err.Error() == heldLines {
			return err
		}
		if l.reject(ctx, head, err, cur, loaded) {
			l.mu.Lock()
			l.held, l.heldTree, l.heldLines = head, tree, err.Error()
			l.mu.Unlock()
		}
		return err
	}
	v, inserted, err := l.Store.RecordPolicy(ctx, head, tree)
	if err != nil {
		log.Printf("config: record commit %s: %v", head, err)
		return fmt.Errorf("record policy: %w", err)
	}
	l.mu.Lock()
	l.current, l.loaded, l.seen = v, true, head
	l.held, l.heldTree, l.heldLines = "", policy.Tree{}, ""
	l.mu.Unlock()
	how := "loaded"
	if !inserted {
		how = "current (commit known)"
	}
	log.Printf("config: version %d %s, commit %s", v.ID, how, v.Commit)
	return l.settle(ctx, head)
}

// reject logs head refused for reason and raises it, and reports whether
// the raise took. A tree refused on its own is not judged again; one open
// cards refused is (Load keeps it as held).
func (l *PolicyLoader) reject(ctx context.Context, head string, reason error, cur policy.Version, loaded bool) bool {
	stays := "no version is current"
	if loaded {
		stays = fmt.Sprintf("version %d (commit %s) stays current", cur.ID, cur.Commit)
	}
	log.Printf("CONFIG REJECTED: commit %s: %v; %s", head, reason, stays)
	if l.Rejected != nil {
		if err := l.Rejected(ctx, head, reason, cur, loaded); err != nil {
			log.Printf("config: raise the rejection of commit %s: %v", head, err)
			return false
		}
	}
	return true
}

// settle runs Loaded once for head when head is the version in force.
func (l *PolicyLoader) settle(ctx context.Context, head string) error {
	l.mu.Lock()
	cur, done := l.current, l.settled == head
	l.mu.Unlock()
	if l.Loaded == nil || done || cur.Commit != head {
		return nil
	}
	if err := l.Loaded(ctx, cur); err != nil {
		log.Printf("config: settle commit %s: %v", head, err)
		return err
	}
	l.mu.Lock()
	l.settled = head
	l.mu.Unlock()
	return nil
}

// Watch loads the head every interval.
func (l *PolicyLoader) Watch(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			l.Load(ctx) // #nosec G104 -- Load logs every failure itself
		}
	}
}

// Current is the version in force; Boot guarantees there is one.
func (l *PolicyLoader) Current() policy.Version {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

func (l *PolicyLoader) Budget() policy.Budget { return l.Current().Document.Budget }

// CheckTree is the loader's whole judgement of a tree's files, which
// `factoryd config check` runs too: policy.ParseTree, and every class a
// starting lane names must allow every model of the ladder, else a task
// would be refused mid-run.
func CheckTree(files map[string][]byte, ladder []string) (policy.Tree, error) {
	tree, err := policy.ParseTree(files)
	if err != nil {
		return policy.Tree{}, err
	}
	if err := fitsLadder(tree, ladder); err != nil {
		return policy.Tree{}, err
	}
	return tree, nil
}

// CheckProjectFile judges a new project's file the loader's way before the
// console files it (stage11-design.md section 5): files is the tree at
// infra's head, file becomes projects/<name>.yaml, and CheckTree judges.
func CheckProjectFile(files map[string][]byte, name string, file []byte, ladder []string) error {
	path := policy.ProjectFile(name)
	if _, ok := files[path]; ok {
		return fmt.Errorf("%s: the file exists at infra's head: the project is there already", path)
	}
	tree := maps.Clone(files)
	tree[path] = file
	_, err := CheckTree(tree, ladder)
	return err
}

// CheckOpenLanes is the loader's one judgement the tree alone cannot make
// (D1, D1b): no open card may sit in a lane that the process its project
// follows in tree lacks. One line per project and lane, on the process file
// when the card's own process lost the lane, else on the project file.
func CheckOpenLanes(tree policy.Tree, cards []OpenCard) []string {
	type key struct {
		project, lane, process string
		moved                  bool
	}
	held := map[key][]int64{}
	for _, c := range cards {
		pc, ok := tree.Projects[c.Project]
		if !ok || pc.Process == "" {
			continue
		}
		if _, ok := laneOf(tree.Processes[pc.Process].Lanes, c.Lane); ok {
			continue
		}
		k := key{c.Project, c.Lane, pc.Process, c.Process != "" && c.Process != pc.Process}
		held[k] = append(held[k], c.Issue)
	}
	var lines []string
	for k, issues := range held {
		slices.Sort(issues)
		refs := make([]string, len(issues))
		for i, n := range issues {
			refs[i] = fmt.Sprintf("#%d", n)
		}
		count := fmt.Sprintf("%d open card", len(issues))
		if len(issues) > 1 {
			count += "s"
		}
		if k.moved {
			lines = append(lines, fmt.Sprintf("%s: process %s has no lane %s, which holds %s of project %s (%s); move or close them first",
				policy.ProjectFile(k.project), k.process, k.lane, count, k.project, strings.Join(refs, ", ")))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: lanes: lane %s is gone from process %s, which project %s follows: it holds %s (%s); move or close them first",
			policy.ProcessFile(k.process), k.lane, k.process, k.project, count, strings.Join(refs, ", ")))
	}
	slices.Sort(lines)
	return lines
}

// fitsLadder holds every class a starting lane of any process names
// against the ladder, one line per class and missing model.
func fitsLadder(t policy.Tree, ladder []string) error {
	var errs []error
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(t.Processes)) {
		for _, l := range t.Processes[name].Lanes {
			for _, m := range ladder {
				if l.Class == "" || seen[l.Class+" "+m] || t.Document.AllowsModel([]string{l.Class}, m) {
					continue
				}
				seen[l.Class+" "+m] = true
				errs = append(errs, fmt.Errorf("%s: capability class %q does not allow the ladder model %q (lane %s of %s starts it)",
					policy.FilePolicy, l.Class, m, l.Name, policy.ProcessFile(name)))
			}
		}
	}
	return errors.Join(errs...)
}

// ForgejoConfig is the configuration tree in Forgejo: directory Dir of
// Repo's Branch, read with Token, a read-only token that reaches Repo only.
type ForgejoConfig struct {
	Forgejo *Forgejo
	Token   string
	Repo    Repo
	Branch  string
	Dir     string
}

func (c *ForgejoConfig) call(ctx context.Context, path string) ([]byte, error) {
	return c.Forgejo.tokenCall(c.Token)(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/%s", c.Repo.Owner, c.Repo.Name, path), nil, http.StatusOK)
}

func (c *ForgejoConfig) Head(ctx context.Context) (string, error) {
	body, err := c.call(ctx, "branches/"+url.PathEscape(c.Branch))
	if err != nil {
		return "", err
	}
	var res struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Commit.ID == "" {
		return "", fmt.Errorf("forgejo branch %s of %s: no head commit (%v)", c.Branch, c.Repo, err)
	}
	return res.Commit.ID, nil
}

// Files lists Dir's tree at commit and reads each file below it, schema/
// left out.
func (c *ForgejoConfig) Files(ctx context.Context, commit string) (map[string][]byte, error) {
	dir, err := c.tree(ctx, commit, false)
	if err != nil {
		return nil, err
	}
	sha := ""
	for _, e := range dir {
		if e.Path == c.Dir && e.Type == "tree" {
			sha = e.SHA
		}
	}
	if sha == "" {
		return nil, fmt.Errorf("commit %s of %s has no directory %s/", commit, c.Repo, c.Dir)
	}
	entries, err := c.tree(ctx, sha, true)
	if err != nil {
		return nil, err
	}
	var blobs []treeEntry
	for _, e := range entries {
		switch {
		case e.Type == "tree" || strings.HasPrefix(e.Path, policy.DirSchema+"/"):
		case e.Type != "blob" || e.Mode != "100644" && e.Mode != "100755":
			return nil, fmt.Errorf("%s: the tree holds regular files only", e.Path)
		default:
			blobs = append(blobs, e)
		}
	}
	files := map[string][]byte{}
	for _, e := range blobs {
		b, err := c.File(ctx, commit, e.Path)
		if err != nil {
			return nil, err
		}
		files[e.Path] = b
	}
	return files, nil
}

// File reads name, relative to Dir, at commit.
func (c *ForgejoConfig) File(ctx context.Context, commit, name string) ([]byte, error) {
	var segs []string
	for _, seg := range strings.Split(c.Dir+"/"+name, "/") {
		segs = append(segs, url.PathEscape(seg))
	}
	return c.call(ctx, "raw/"+strings.Join(segs, "/")+"?ref="+url.QueryEscape(commit))
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
	SHA  string `json:"sha"`
}

// tree is one page of up to 1000 entries of tree sha; a bigger tree is an
// error, the configuration is a handful of files.
func (c *ForgejoConfig) tree(ctx context.Context, sha string, recursive bool) ([]treeEntry, error) {
	q := url.Values{"per_page": {"1000"}, "recursive": {fmt.Sprint(recursive)}}
	body, err := c.call(ctx, "git/trees/"+url.PathEscape(sha)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var res struct {
		TotalCount int         `json:"total_count"`
		Truncated  bool        `json:"truncated"`
		Tree       []treeEntry `json:"tree"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("forgejo tree %s of %s: %w", sha, c.Repo, err)
	}
	if res.Truncated || res.TotalCount > len(res.Tree) {
		return nil, fmt.Errorf("forgejo tree %s of %s: %d entries, more than one page", sha, c.Repo, res.TotalCount)
	}
	return res.Tree, nil
}

// SweepLoop books expired holds every interval until ctx ends.
func SweepLoop(ctx context.Context, store PolicyStore, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := store.SweepHolds(ctx)
			if err != nil {
				log.Printf("sweep holds: %v", err)
			}
			if n > 0 {
				log.Printf("sweep holds: booked %d expired holds at their full amount", n)
			}
		}
	}
}

// PolicyHandler serves GET /policy: the version in force, its whole tree.
func PolicyHandler(store PolicyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok, err := store.CurrentPolicy(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no policy version loaded", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, v)
	}
}

// maxCostDays bounds one GET /costs range.
const maxCostDays = 366

// CostsHandler serves GET /costs?from=YYYY-MM-DD&to=YYYY-MM-DD (UTC days,
// both included, each defaulting to today): per-day totals by kind and
// per-task totals with their lines (kind and ref) from the ledger.
func CostsHandler(store PolicyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		today := time.Now().UTC().Format(policy.DateLayout)
		day := func(name string) (time.Time, error) {
			v := r.URL.Query().Get(name)
			if v == "" {
				v = today
			}
			t, err := time.Parse(policy.DateLayout, v)
			if err != nil {
				return t, fmt.Errorf("%s: want YYYY-MM-DD", name)
			}
			return t, nil
		}
		from, err := day("from")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, err := day("to")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if to.Before(from) || to.Sub(from) > maxCostDays*24*time.Hour {
			http.Error(w, fmt.Sprintf("want from <= to, at most %d days apart", maxCostDays), http.StatusBadRequest)
			return
		}
		c, err := store.Costs(r.Context(), from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, c)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) // #nosec G104 -- best-effort response write
}

// the PGStore side of PolicyStore

func (s *PGStore) RecordPolicy(ctx context.Context, commit string, tree policy.Tree) (policy.Version, bool, error) {
	return policy.Record(ctx, s.db, commit, tree)
}

// OpenCardsByLane reads every open card with a lane; its project is the
// one whose file in force names its repository, a card of no project is
// left out, and a card the loader has not given a process yet follows its
// project's in force.
func (s *PGStore) OpenCardsByLane(ctx context.Context) ([]OpenCard, error) {
	ps, err := s.Projects(ctx)
	if err != nil {
		return nil, err
	}
	project := map[string]Project{}
	for _, p := range ps {
		if p.File {
			project[p.Repository] = p
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT repository, lane, COALESCE(process, ''), issue_number FROM tasks
		WHERE lane IS NOT NULL AND status NOT IN ('done', 'failed', 'closed') ORDER BY issue_number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cards []OpenCard
	for rows.Next() {
		var repository string
		var c OpenCard
		if err := rows.Scan(&repository, &c.Lane, &c.Process, &c.Issue); err != nil {
			return nil, err
		}
		p, ok := project[repository]
		if !ok {
			continue
		}
		c.Project, c.Process = p.Name, cmp.Or(c.Process, p.Process)
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

func (s *PGStore) PolicyVersion(ctx context.Context, id int64) (policy.Version, bool, error) {
	return policy.VersionByID(ctx, s.db, id)
}

func (s *PGStore) CurrentPolicy(ctx context.Context) (policy.Version, bool, error) {
	return policy.Current(ctx, s.db)
}

func (s *PGStore) SweepHolds(ctx context.Context) (int, error) {
	return policy.SweepHolds(ctx, s.db)
}

func (s *PGStore) Costs(ctx context.Context, from, to time.Time) (policy.Costs, error) {
	return policy.LedgerCosts(ctx, s.db, from, to)
}

func (s *PGStore) DaySpend(ctx context.Context, day time.Time) (float64, error) {
	return policy.LedgerDaySpend(ctx, s.db, day)
}

func (s *PGStore) ModelCalls(ctx context.Context, taskID string) ([]policy.ModelCall, error) {
	return policy.ModelCalls(ctx, s.db, taskID)
}

func (s *PGStore) TaskFree(ctx context.Context, taskID string) (policy.TaskFree, error) {
	return policy.ReadTaskFree(ctx, s.db, taskID)
}

func (s *PGStore) DayFree(ctx context.Context, dayEUR float64) (float64, error) {
	return policy.DayFree(ctx, s.db, dayEUR, time.Now())
}

// CallTotals sums the tasks' audit call rows — their tokens and modeled
// pay-as-you-go cost to date, and the credits the GLM Coding Plan derives
// from the tokens of each deployment that answered — in one query, with
// no stored aggregate (ruling 198). Credits are computed here, at read
// time, never stored (roadmap 206); a deployment the plan holds no rate
// for contributes its tokens and cost but no credits. A task without
// call rows is absent from the answer.
func (s *PGStore) CallTotals(ctx context.Context, tasks []string) (map[string]policy.CallTotals, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, deployment, sum(prompt_tokens), sum(cached_tokens), sum(output_tokens), COALESCE(sum(call_cost_eur), 0)::float8
		 FROM audit WHERE task_id = ANY($1) GROUP BY task_id, deployment`, tasks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	totals := map[string]policy.CallTotals{}
	for rows.Next() {
		var id, deployment string
		var t policy.CallTotals
		if err := rows.Scan(&id, &deployment, &t.Prompt, &t.Cached, &t.Output, &t.ModeledEUR); err != nil {
			return nil, err
		}
		if rate, ok := policy.CreditRateOf(deployment); ok {
			t.Credits = rate.Credits(t.Prompt, t.Cached, t.Output)
		}
		sum := totals[id]
		sum.Prompt, sum.Cached, sum.Output = sum.Prompt+t.Prompt, sum.Cached+t.Cached, sum.Output+t.Output
		sum.ModeledEUR, sum.Credits = sum.ModeledEUR+t.ModeledEUR, sum.Credits+t.Credits
		totals[id] = sum
	}
	return totals, rows.Err()
}

var _ PolicyStore = (*PGStore)(nil)

// Aliases are the ladder's models as the gateway sees them: without the
// opencode provider prefix ("factory/coding-cheap" is "coding-cheap").
func (l ModelLadder) Aliases() []string {
	return []string{aliasOf(l.Start), aliasOf(l.Stronger)}
}

// aliasOf is model without its opencode provider prefix.
func aliasOf(model string) string {
	if _, alias, ok := strings.Cut(model, "/"); ok {
		return alias
	}
	return model
}

// SourceConfig is the source of a task filed for a refused configuration
// commit.
const SourceConfig = "config"

// ConfigBook is what the configuration alarm keeps: the human task and
// one alerts row per refused commit.
type ConfigBook interface {
	CreateTask(ctx context.Context, t Task) error
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	InsertAlert(ctx context.Context, a Alert) (int64, error)
	AlertByFingerprint(ctx context.Context, kind, fingerprint string) (Alert, bool, error)
	SetAlertTask(ctx context.Context, id int64, taskID string, detail map[string]any) error
	ResolveAlert(ctx context.Context, id int64, kind, verdict, by string, detail map[string]any) error
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	// BackfillProcess sets process on repository's open cards that have a
	// lane and no process yet.
	BackfillProcess(ctx context.Context, repository, process string) error
	projectLookup
	taskReader
}

// ConfigAlarm raises a refused commit of the configuration tree (ruling
// 183): a human-lane issue on the tree's repository and an alerts row of
// kind config_rejected, once per commit.
type ConfigAlarm struct {
	Store   ConfigBook
	Forgejo *Forgejo
	// Repo holds the tree in directory Dir.
	Repo Repo
	Dir  string
	// Metrics counts the outcome of the tasks Resolve ends; nil counts none.
	Metrics *telemetry.Metrics
}

// Raise is PolicyLoader.Rejected. The alerts row, the dedupe key, is
// written first and linked to the task once the issue is filed, so a
// failed filing is tried again without a second issue.
func (a *ConfigAlarm) Raise(ctx context.Context, commit string, reason error, current policy.Version, loaded bool) error {
	row, ok, err := a.Store.AlertByFingerprint(ctx, AlertConfigRejected, commit)
	if err != nil || row.TaskID != "" {
		return err
	}
	if !ok {
		if row.ID, err = a.Store.InsertAlert(ctx, Alert{Kind: AlertConfigRejected, Source: "loader", Fingerprint: commit,
			Detail: map[string]any{"commit": commit, "repository": a.Repo.String(), "reason": reason.Error(), "current": current.ID}}); err != nil {
			return err
		}
	}
	short := commit[:min(10, len(commit))]
	stays := "No version is current: factoryd cannot run tasks until a valid commit lands."
	if loaded {
		stays = fmt.Sprintf("Version %d (commit %s) stays current.", current.ID, current.Commit)
	}
	link := fmt.Sprintf("%s/%s/src/commit/%s/%s", a.Forgejo.PublicURL, a.Repo, commit, a.Dir)
	title := fmt.Sprintf("config: %s/ at %s rejected", a.Dir, short)
	body := fmt.Sprintf("factoryd refused the configuration tree %s/ at commit %s (%s). %s\n\nReasons (`factoryd config check %s` shows the same):\n\n%s\n\n"+
		"Fix the files and push; the next valid commit on main becomes current within 30 s and closes this issue.\n\nfiled by factoryd",
		a.Dir, commit, link, stays, a.Dir, fenced(reason.Error()))
	t, ref, err := FileTask(ctx, a.Store, a.Forgejo, a.Repo, title, body, []string{"human"}, Task{
		ID: NewTaskID(), Kind: "human", RiskClass: "standard", Objective: title + "\n\n" + body, SHA: commit,
		Source: SourceConfig, Author: a.Forgejo.BotUser,
	})
	if err != nil {
		return err
	}
	return a.Store.SetAlertTask(ctx, row.ID, t.ID, map[string]any{"issue": ref.Number})
}

// ReadFailing is PolicyLoader.ReadFailing: an alerts row, judged real,
// without an issue, since what fails may be the way to Forgejo.
func (a *ConfigAlarm) ReadFailing(ctx context.Context, err error) error {
	_, ierr := a.Store.InsertAlert(ctx, Alert{Kind: AlertConfigRejected, Source: "read", Verdict: VerdictReal, ResolvedBy: "factoryd",
		Detail: map[string]any{"repository": a.Repo.String(), "error": err.Error(), "failed_reads": readFailureStreak}})
	return ierr
}

// Resolve is PolicyLoader.Loaded: a valid commit loaded. The open cards
// the lanes migration left without a process take their project's, and
// every open rejection is fixed: each issue gets a comment naming the
// commit and is closed as the bot, its task ends done, and its alert is
// judged real.
func (a *ConfigAlarm) Resolve(ctx context.Context, v policy.Version) error {
	for _, c := range v.Projects {
		if c.Process == "" {
			continue
		}
		if err := a.Store.BackfillProcess(ctx, c.Repository, c.Process); err != nil {
			return fmt.Errorf("process of the open cards of %s: %w", c.Repository, err)
		}
	}
	open, err := a.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return err
	}
	for _, t := range open {
		if t.Source != SourceConfig || t.Repository != a.Repo.String() {
			continue
		}
		msg := fmt.Sprintf("factoryd loaded commit %s (version %d); this rejection is resolved.", v.Commit, v.ID)
		if _, err := a.Forgejo.CreateIssueComment(ctx, a.Repo.Owner, a.Repo.Name, t.IssueNumber, msg); err != nil {
			return err
		}
		if err := recordFactoryClose(ctx, a.Store, t); err != nil {
			return err
		}
		if err := a.Forgejo.CloseIssue(ctx, a.Repo.Owner, a.Repo.Name, t.IssueNumber); err != nil {
			return err
		}
		if err := a.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
			return err
		}
		countOutcome(ctx, a.Metrics, a.Store, t, "done")
		row, ok, err := a.Store.AlertByFingerprint(ctx, AlertConfigRejected, t.SHA)
		if err != nil {
			return err
		}
		if ok && row.Verdict == "" {
			if err := a.Store.ResolveAlert(ctx, row.ID, AlertConfigRejected, VerdictReal, "factoryd", map[string]any{"fixed_by": v.Commit}); err != nil {
				return err
			}
		}
		log.Printf("config: commit %s closed issue #%d of rejected commit %s, task %s done", v.Commit, t.IssueNumber, t.SHA, t.ID)
	}
	return nil
}
