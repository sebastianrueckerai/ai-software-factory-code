package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// The commit reads that replace the compare (CommitsBetween, ChangedBetween,
// CommitChangedFiles, holds) against Forgejo 17.0.1: the commits list with
// sha and not pages a known range past one page, files=false and stat=false
// leave the file lists out, path with not and limit=1 answers one commit
// for a path the range changed and none for one it did not, and holds
// tells a commit the head has from one it lacks.
func TestCommitReadsOnForgejo17(t *testing.T) {
	base, a := startForgejo(t)
	ctx := context.Background()
	f := &Forgejo{BaseURL: base, BotUser: a.User, BotPassword: a.Password}
	repo := Repo{Owner: a.User, Name: "range"}
	must := func(_ []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.call(ctx, http.MethodPost, "/api/v1/user/repos", mustJSON(map[string]any{"name": repo.Name, "auto_init": true, "default_branch": "main"}), http.StatusCreated))
	commit := func(branch, path, text string) string {
		t.Helper()
		sha, err := postContents(ctx, f.call, repo, map[string]any{"branch": branch, "message": "add " + path + "\n\nbody",
			"files": []map[string]string{{"operation": "create", "path": path, "content": base64.StdEncoding.EncodeToString([]byte(text))}}})
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	start, err := f.BranchHeadSHA(ctx, repo.Owner, repo.Name, "main")
	if err != nil {
		t.Fatal(err)
	}
	const n = 120
	var made []string // oldest first
	for i := 1; i <= n; i++ {
		path := fmt.Sprintf("src/f%03d.go", i)
		if i == 70 {
			path = "docs/changed.md"
		}
		made = append(made, commit("main", path, "x\n"))
	}
	head := made[n-1]
	must(f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/branches", repo), mustJSON(map[string]string{"new_branch_name": "side", "old_branch_name": "main"}), http.StatusCreated))
	side := commit("side", "side.go", "y\n")
	later := commit("main", "later.go", "z\n")

	t.Run("the list pages a range and leaves files out", func(t *testing.T) {
		q := url.Values{"sha": {head}, "not": {start}, "stat": {"false"}, "verification": {"false"}, "files": {"false"}, "limit": {"50"}}
		for page, want := range map[int]int{1: 50, 2: 50, 3: 20, 4: 0} {
			q.Set("page", fmt.Sprint(page))
			body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/commits?%s", repo, q.Encode()), nil, http.StatusOK)
			if err != nil {
				t.Fatal(err)
			}
			var got []map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil || len(got) != want {
				t.Fatalf("page %d: %d commits, %v; want %d", page, len(got), err, want)
			}
			for _, c := range got {
				if v, ok := c["files"]; ok && string(v) != "null" {
					t.Fatalf("page %d: a commit carries files %s with files=false", page, v)
				}
				if v, ok := c["stats"]; ok && string(v) != "null" {
					t.Fatalf("page %d: a commit carries stats %s with stat=false", page, v)
				}
			}
		}
		q.Set("page", "1")
		q.Set("files", "true")
		body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/commits?%s", repo, q.Encode()), nil, http.StatusOK)
		if err != nil {
			t.Fatal(err)
		}
		var withFiles []struct {
			Files []struct{} `json:"files"`
		}
		if err := json.Unmarshal(body, &withFiles); err != nil || len(withFiles) == 0 || len(withFiles[0].Files) == 0 {
			t.Fatalf("files=true answered no files (%v): the parameter would then prove nothing", err)
		}
	})

	t.Run("CommitsBetween", func(t *testing.T) {
		got, err := f.CommitsBetween(ctx, repo.Owner, repo.Name, start, head)
		if err != nil || len(got) != n {
			t.Fatalf("%d commits, %v; want %d", len(got), err, n)
		}
		for i, c := range got {
			if want := made[n-1-i]; c.SHA != want || c.Parents != 1 {
				t.Fatalf("commit %d = %+v, want %s with one parent, newest first", i, c, want)
			}
		}
		if got[0].Subject != "add src/f120.go" {
			t.Fatalf("subject %q, want the message's first line", got[0].Subject)
		}
		if got, err := f.CommitsBetween(ctx, repo.Owner, repo.Name, head, head); err != nil || len(got) != 0 {
			t.Fatalf("an empty range = %d commits, %v", len(got), err)
		}
		if got, err := f.CommitsBetween(ctx, repo.Owner, repo.Name, made[n-3], head); err != nil || len(got) != 2 {
			t.Fatalf("the last two = %d commits, %v", len(got), err)
		}
	})

	t.Run("ChangedBetween", func(t *testing.T) {
		for path, want := range map[string]bool{"docs/changed.md": true, "src/f005.go": true, "never.go": false, "side.go": false} {
			if got, err := f.ChangedBetween(ctx, repo.Owner, repo.Name, start, head, path); err != nil || got != want {
				t.Errorf("%s changed in the range = %v, %v; want %v", path, got, err, want)
			}
		}
		if got, err := f.ChangedBetween(ctx, repo.Owner, repo.Name, made[69], head, "docs/changed.md"); err != nil || got {
			t.Errorf("docs/changed.md after the commit that made it = %v, %v; want false", got, err)
		}
		body, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/commits?%s", repo,
			commitsQuery(head, start, url.Values{"path": {"src"}, "limit": {"1"}})), nil, http.StatusOK)
		var one []struct{}
		if err != nil || json.Unmarshal(body, &one) != nil || len(one) != 1 {
			t.Fatalf("path src with limit=1 answered %d commits (%v), want 1", len(one), err)
		}
	})

	t.Run("the raw answer for a path the range did not change", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+fmt.Sprintf("/api/v1/repos/%s/commits?%s", repo,
			commitsQuery(head, start, url.Values{"path": {"never.go"}, "limit": {"1"}})), nil)
		req.SetBasicAuth(a.User, a.Password)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b [300]byte
		k, _ := res.Body.Read(b[:])
		if res.StatusCode != http.StatusNotFound || !strings.Contains(string(b[:k]), `"message":"FileCommitsCount"`) {
			t.Fatalf("status %d body %s; want 404 FileCommitsCount", res.StatusCode, b[:k])
		}
	})

	t.Run("CommitChangedFiles", func(t *testing.T) {
		files, err := f.CommitChangedFiles(ctx, repo.Owner, repo.Name, made[69])
		if err != nil || !slices.Equal(files, []string{"docs/changed.md"}) {
			t.Fatalf("files %v, %v; want docs/changed.md", files, err)
		}
	})

	t.Run("holds", func(t *testing.T) {
		for name, tc := range map[string]struct {
			head, sha string
			want      bool
		}{
			"an ancestor":      {head, made[10], true},
			"the head":         {head, head, true},
			"the start":        {head, start, true},
			"a side commit":    {head, side, false},
			"head behind":      {made[10], head, false},
			"side's own":       {side, made[10], true},
			"side lacks later": {side, later, false},
		} {
			if got, err := holds(ctx, f.call, repo, tc.head, tc.sha); err != nil || got != tc.want {
				t.Errorf("%s: holds = %v, %v; want %v", name, got, err, tc.want)
			}
		}
	})
}
