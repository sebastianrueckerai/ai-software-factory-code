package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const agentDiff = `diff --git a/slug.go b/slug.go
index 1111111..2222222 100644
--- a/slug.go
+++ b/slug.go
@@ -1,3 +1,3 @@
 package slug
-func Max() int { return 10 }
+func Max() int { return 64 }
diff --git a/slug_test.go b/slug_test.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/slug_test.go
@@ -0,0 +1,2 @@
+package slug
+func TestMax(t *testing.T) {}
`

const revertSlug = `diff --git a/slug.go b/slug.go
index 2222222..4444444 100644
--- a/slug.go
+++ b/slug.go
@@ -1,4 +1,4 @@
 package slug
-func Max() int { return 64 }
+func Max() int { return 10 }
 // a later line moved the context
`

const revertTest = `diff --git a/slug_test.go b/slug_test.go
deleted file mode 100644
index 3333333..0000000
--- a/slug_test.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package slug
-func TestMax(t *testing.T) {}
`

const readmeDiff = `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1,2 @@
 # slug
+A second line.
`

// A later pull request reverts an agent's when, for every file the
// agent's changed, it adds exactly the lines the agent's removed and
// removes exactly the lines it added (context and line numbers aside); a
// file it leaves alone, a change in the same direction or a different
// edit of the same file is no revert. More files besides are allowed.
func TestRevertsRule(t *testing.T) {
	for name, tc := range map[string]struct {
		later string
		want  bool
	}{
		"the whole revert":            {revertSlug + revertTest, true},
		"the revert and another file": {readmeDiff + revertTest + revertSlug, true},
		"partial overlap: one file":   {revertSlug, false},
		"the same change again":       {agentDiff, false},
		"another edit of the file": {revertTest + `diff --git a/slug.go b/slug.go
--- a/slug.go
+++ b/slug.go
@@ -1,3 +1,3 @@
-func Max() int { return 64 }
+func Max() int { return 32 }
`, false},
		"another file only": {readmeDiff, false},
		"nothing":           {"", false},
	} {
		if got := reverts(agentDiff, tc.later); got != tc.want {
			t.Errorf("%s: reverts = %v, want %v", name, got, tc.want)
		}
	}
	if reverts("", revertSlug) {
		t.Error("an empty agent diff is reverted by anything")
	}
}

// fakeRevertBook is the pass's store: candidates by finish, marks once.
type fakeRevertBook struct {
	tasks    []Task
	refiled  []Task
	projects []Project
	marked   []string
}

func (b *fakeRevertBook) RevertCandidates(_ context.Context, since time.Time) ([]Task, error) {
	var out []Task
	for _, t := range b.tasks {
		if t.RevertedAt == nil && !t.FinishedAt.Before(since) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (b *fakeRevertBook) MarkReverted(_ context.Context, id string, at time.Time) (bool, error) {
	for i := range b.tasks {
		if b.tasks[i].ID == id && b.tasks[i].RevertedAt == nil {
			b.tasks[i].RevertedAt = &at
			b.marked = append(b.marked, fmt.Sprintf("%s %s", id, at.Format(time.RFC3339)))
			return true, nil
		}
	}
	return false, nil
}

func (b *fakeRevertBook) MarkRefiledFixes(context.Context, time.Duration) ([]Task, error) {
	out := b.refiled
	b.refiled = nil
	return out, nil
}

func (b *fakeRevertBook) ProjectByRepository(_ context.Context, repository string) (Project, bool, error) {
	for _, p := range b.projects {
		if p.Repository == repository {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

// revertForgejo serves a repository's closed pull requests, newest update
// first, 50 a page, and their diffs.
func revertForgejo(t *testing.T, prs []map[string]any, diffs map[int64]string) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/repos/sf/repo/pulls":
			q := r.URL.Query()
			if q.Get("state") != "closed" || q.Get("sort") != "recentupdate" {
				t.Errorf("pulls query %q", r.URL.RawQuery)
			}
			page := prs
			if q.Get("page") != "1" {
				page = nil
			}
			json.NewEncoder(w).Encode(page) // #nosec G104 -- test stub
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/repo/pulls/") && strings.HasSuffix(r.URL.Path, ".diff"):
			var n int64
			fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/repo/pulls/"), "%d.diff", &n)
			d, ok := diffs[n]
			if !ok {
				t.Errorf("diff of #%d asked", n)
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, d)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

func mergedPR(number int64, mergedAt time.Time) map[string]any {
	return map[string]any{"number": number, "merged": true, "merged_at": mergedAt.Format(time.RFC3339),
		"updated_at": mergedAt.Format(time.RFC3339), "base": map[string]any{"ref": "main"}, "head": map[string]any{"sha": fmt.Sprint("h", number)}}
}

// change is a one-file diff turning line from into line to.
func change(file, from, to string) string {
	return fmt.Sprintf("diff --git a/%[1]s b/%[1]s\n--- a/%[1]s\n+++ b/%[1]s\n@@ -1,2 +1,2 @@\n package x\n-%[2]s\n+%[3]s\n", file, from, to)
}

// The daily pass marks a merged agent pull request reverted by a pull
// request merged on the same repository after the task finished and
// within 14 days of it, at the revert's merge, and each task whose alert
// refiled after its fix; each mark counts factory.task.reverted{project}
// once, however often the pass runs.
func TestRevertPass(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	day := 24 * time.Hour
	book := &fakeRevertBook{
		projects: []Project{{Name: "demo", Repository: "sf/repo"}},
		tasks: []Task{
			{ID: "t-rev", Repository: "sf/repo", PRNumber: 7, Status: "done", FinishedAt: at(-3 * day)},
			{ID: "t-partial", Repository: "sf/repo", PRNumber: 8, Status: "done", FinishedAt: at(-3 * day)},
			{ID: "t-late", Repository: "sf/repo", PRNumber: 9, Status: "done", FinishedAt: at(-14*day - 20*time.Hour)},
			{ID: "t-before", Repository: "sf/repo", PRNumber: 10, Status: "done", FinishedAt: at(-2 * day)},
		},
		refiled: []Task{{ID: "t-alert", Repository: "sf/other"}},
	}
	prs := []map[string]any{
		mergedPR(20, now.Add(-day)),
		mergedPR(21, now.Add(-day)),
		mergedPR(22, now.Add(-10*time.Hour)),
		mergedPR(23, now.Add(-2*day-time.Hour)),
		{"number": 24, "merged": false, "updated_at": now.Add(-time.Hour).Format(time.RFC3339), "base": map[string]any{"ref": "main"}},
	}
	diffs := map[int64]string{
		7: agentDiff, 20: revertSlug + revertTest,
		8: change("cut.go", "a", "b") + change("cut_test.go", "c", "d"), 21: change("cut.go", "b", "a"),
		9: change("late.go", "a", "b"), 22: change("late.go", "b", "a"),
		10: change("early.go", "a", "b"), 23: change("early.go", "b", "a"),
	}
	m, reader := testMetrics(t)
	pass := &RevertPass{Book: book, Forgejo: revertForgejo(t, prs, diffs), Metrics: m, Now: func() time.Time { return now }}
	for range 2 {
		if err := pass.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"t-rev " + now.Add(-day).Format(time.RFC3339)}; !slices.Equal(book.marked, want) {
		t.Fatalf("marked = %q, want %q", book.marked, want)
	}
	wantSums(t, sums(t, reader, "factory.task.reverted"), map[string]float64{"project=demo": 1, "project=sf/other": 1})
}

func TestPGStoreRevertMarks(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, alerts CASCADE`); err != nil {
		t.Fatal(err)
	}
	mk := func(id, status, source string, pr int64, finishedAgo time.Duration) {
		t.Helper()
		if err := s.CreateTask(ctx, Task{ID: id, RiskClass: "standard", BudgetEUR: 1, Status: "queued", Objective: "o", Repository: "sf/r", Source: source}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = $2, pr_number = $3, finished_at = now() - $4 * interval '1 second' WHERE id = $1`,
			id, status, pr, finishedAgo.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	day := 24 * time.Hour
	mk("t-a", "done", "webhook", 7, 2*day)
	mk("t-nopr", "done", "webhook", 0, 2*day)
	mk("t-failed", "failed", "webhook", 5, 2*day)
	mk("t-roll", "done", SourceRoll, 6, 2*day)
	mk("t-old", "done", "webhook", 9, 20*day)
	cands, err := s.RevertCandidates(ctx, time.Now().Add(-15*day))
	if err != nil || len(cands) != 1 || cands[0].ID != "t-a" {
		t.Fatalf("candidates = %+v, %v; want t-a only", cands, err)
	}
	when := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for i, want := range []bool{true, false} {
		if ok, err := s.MarkReverted(ctx, "t-a", when); err != nil || ok != want {
			t.Fatalf("mark %d = %v %v, want %v", i, ok, err, want)
		}
	}
	if got, _, _ := s.Task(ctx, "t-a"); got.RevertedAt == nil || !got.RevertedAt.Equal(when) {
		t.Fatalf("reverted_at = %v, want %v", got.RevertedAt, when)
	}
	if cands, _ := s.RevertCandidates(ctx, time.Now().Add(-15*day)); len(cands) != 0 {
		t.Fatalf("a marked task is still a candidate: %+v", cands)
	}

	// the refile rule: a later alerts row of the fingerprint raised after
	// the fixing task finished, within 14 days
	mk("t-fix", "done", SourceAlert, 11, 3*day)
	mk("t-refiled", "queued", SourceAlert, 0, 0)
	mk("t-fix-old", "done", SourceAlert, 12, 20*day)
	mk("t-refiled-late", "queued", SourceAlert, 0, 0)
	mk("t-fix-open", "running", SourceAlert, 13, 0)
	mk("t-again", "queued", SourceAlert, 0, 0)
	alert := func(task, fp string, raisedAgo time.Duration) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO alerts (kind, source, task_id, fingerprint, raised_at) VALUES ('alert', 'e', $1, $2, now() - $3 * interval '1 second')`,
			task, fp, raisedAgo.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	alert("t-fix", "fp1", 4*day)
	alert("t-refiled", "fp1", day)
	alert("t-fix-old", "fp2", 21*day)
	alert("t-refiled-late", "fp2", day)
	alert("t-fix-open", "fp3", 2*day)
	alert("t-again", "fp3", day)
	marked, err := s.MarkRefiledFixes(ctx, 14*day)
	if err != nil || len(marked) != 1 || marked[0].ID != "t-fix" || marked[0].Repository != "sf/r" {
		t.Fatalf("refiled = %+v, %v; want t-fix", marked, err)
	}
	if got, _, _ := s.Task(ctx, "t-fix"); got.RevertedAt == nil || time.Since(*got.RevertedAt) < day-time.Minute || time.Since(*got.RevertedAt) > day+time.Minute {
		t.Fatalf("t-fix reverted_at = %v, want the refiled alert's raise a day ago", got.RevertedAt)
	}
	if again, err := s.MarkRefiledFixes(ctx, 14*day); err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}

	// a session keeps its model alias on the task
	for _, sess := range []Session{{ID: "s1", TaskID: "t-a", Model: "coding-cheap"}, {ID: "s2", TaskID: "t-a", Model: "coding-frontier"}} {
		if err := s.InsertSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := s.Task(ctx, "t-a"); got.ModelAlias != "coding-frontier" {
		t.Fatalf("model alias = %q, want the last session's", got.ModelAlias)
	}
}
