package acceptance

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The configuration repository's side of the acceptance harness (the
// pool-policy rig): sf/infra's fake as Forgejo's contents, pull request
// and merge endpoints answer it, seeded with the project files a policy
// PR merged, every guarded commit, pull request and merge recorded, and
// the seams the pool's write path calls — the change check that judges
// each candidate and the reload the merge requests — under the test's
// control. The merged policy (the fake's files) and the loaded one (the
// rig's CurrentPolicy overlay) are independent: only the test's
// activation installs a merged change into the policy in force.

// accInfra is the configuration repository's API path: sf/infra.
const accInfra = "/api/v1/repos/sf/infra"

// AccFileChange is one file of a commit the fake landed: its operation,
// its path under the configuration directory, its base64 content and the
// blob sha the guard held.
type AccFileChange struct {
	Operation, Path, Content, SHA string
}

// AccInfraCommit is one guarded commit the fake landed on its branch:
// its message, the branch, the main it branches from, the Authorization
// that made it and its files.
type AccInfraCommit struct {
	Message, Branch, From, Bearer string
	Files                         []AccFileChange
}

// AccInfraPull is one pull request the fake opened: its head branch, its
// base, its title and the Authorization that opened it.
type AccInfraPull struct {
	Head, Base, Title, Bearer string
}

// AccInfraMerge is one merge the fake answered: the pull request's
// counted number and the Authorization that merged it.
type AccInfraMerge struct {
	Number int64
	Bearer string
}

// accBlob is text's blob sha as the fake answers it.
func accBlob(text string) string { return factoryd.ContentHash(text)[:12] }

// serveInfra answers the configuration repository's API: a file of main
// by path with its blob sha, a change of the files as one commit on a
// new branch off the ref the request names — main alone, a change that
// names another refused 404 as Forgejo refuses a ref it does not hold,
// and the sha guard held — an update of a file whose blob is another
// refused 409 — the pull request the change opens with a counted number,
// and the member's merge of it, which lands the branch's files on main;
// a refused merge leaves main as it was. Every call is recorded with its
// Authorization. a.mu held.
func (a *AcceptanceRig) serveInfra(w http.ResponseWriter, r *http.Request) {
	a.infraCalls = append(a.infraCalls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, accInfra+"/contents/"):
		key := strings.TrimPrefix(r.URL.Path, accInfra+"/contents/")
		text, ok := a.infra[key]
		if !ok {
			http.Error(w, `{"message":"object does not exist"}`, http.StatusNotFound)
			return
		}
		if a.switchPath == key {
			// another writer landed between this read and the next: the
			// answer holds the file as it was, the next read sees the move
			a.infra[key], a.switchPath, a.switchText = a.switchText, "", ""
		}
		_ = json.NewEncoder(w).Encode(map[string]string{ // #nosec G104 -- test harness stub response
			"content": base64.StdEncoding.EncodeToString([]byte(text)), "sha": accBlob(text)})
	case r.Method == http.MethodPost && r.URL.Path == accInfra+"/contents":
		var body struct {
			Branch    string `json:"branch"`
			NewBranch string `json:"new_branch"`
			Message   string `json:"message"`
			Files     []struct {
				Operation, Path, Content, SHA string
			} `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // #nosec G104 -- test harness stub request
		// the fake holds main alone: a change that names another ref —
		// wrong or nonexistent — branches from nothing Forgejo has, and
		// the answer says so rather than landing the commit anyway
		if body.Branch != "main" {
			http.Error(w, `{"message":"object does not exist"}`, http.StatusNotFound)
			return
		}
		if a.commitStat != 0 {
			http.Error(w, `{"message":`+strconv.Quote(cmp.Or(a.commitMsg, "the branch is protected"))+`}`, a.commitStat)
			return
		}
		c := AccInfraCommit{Message: body.Message, Branch: body.NewBranch, From: body.Branch, Bearer: r.Header.Get("Authorization")}
		for _, f := range body.Files {
			current, present := a.infra[f.Path]
			switch {
			case f.Operation == "update" && (!present || f.SHA != accBlob(current)):
				http.Error(w, `{"message":"sha does not match"}`, http.StatusConflict)
				return
			case f.Operation == "create" && present:
				http.Error(w, `{"message":"file exists"}`, http.StatusUnprocessableEntity)
				return
			}
			c.Files = append(c.Files, AccFileChange{Operation: f.Operation, Path: f.Path, Content: f.Content, SHA: f.SHA})
		}
		if a.branches == nil {
			a.branches = map[string][]AccFileChange{}
		}
		a.branches[body.NewBranch] = c.Files
		a.commits = append(a.commits, c)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": fmt.Sprintf("acc0ffee%d", len(a.commits))}}) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodPost && r.URL.Path == accInfra+"/pulls":
		var body struct{ Head, Base, Title string }
		_ = json.NewDecoder(r.Body).Decode(&body) // #nosec G104 -- test harness stub request
		a.prNext++
		if a.prBranch == nil {
			a.prBranch = map[int64]string{}
		}
		a.prBranch[a.prNext] = body.Head
		a.pulls = append(a.pulls, AccInfraPull{Head: body.Head, Base: body.Base, Title: body.Title,
			Bearer: r.Header.Get("Authorization")})
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": a.prNext}) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, accInfra+"/pulls/") && strings.HasSuffix(r.URL.Path, "/merge"):
		number, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, accInfra+"/pulls/"), "/merge"), 10, 64)
		a.merges = append(a.merges, AccInfraMerge{Number: number, Bearer: r.Header.Get("Authorization")})
		if a.mergeStat != 0 {
			http.Error(w, `{"message":`+strconv.Quote(cmp.Or(a.mergeMsg, "the pull request is not mergeable"))+`}`, a.mergeStat)
			return
		}
		for _, f := range a.branches[a.prBranch[number]] {
			text, _ := base64.StdEncoding.DecodeString(f.Content) // #nosec G104 -- test harness stub request
			if f.Operation == "delete" {
				delete(a.infra, f.Path)
				continue
			}
			a.infra[f.Path] = string(text)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{}) // #nosec G104 -- test harness stub response
	default:
		http.NotFound(w, r)
	}
}

// PutInfraFile seeds or rewrites path on the configuration repository's
// main, as a policy PR merged outside the rig: the blob a proposal
// captures reads it after.
func (a *AcceptanceRig) PutInfraFile(path, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.infra == nil {
		a.infra = map[string]string{}
	}
	a.infra[path] = text
}

// InfraFiles answers a copy of main's files by path.
func (a *AcceptanceRig) InfraFiles() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.infra)
}

// InfraCalls answers every call the fake saw, "METHOD path Bearer token"
// each, in order.
func (a *AcceptanceRig) InfraCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.infraCalls)
}

// InfraAPI answers the configuration repository fake's own API root, for
// the tests that call its guarded endpoints directly; the fake's guards
// answer them as they answer the console's own writes.
func (a *AcceptanceRig) InfraAPI() string { return a.forgejo + accInfra }

// InfraCommits answers the guarded commits the fake landed, oldest
// first.
func (a *AcceptanceRig) InfraCommits() []AccInfraCommit {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.commits)
}

// InfraPulls answers the pull requests the fake opened, oldest first.
func (a *AcceptanceRig) InfraPulls() []AccInfraPull {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.pulls)
}

// InfraMerges answers the merges the fake answered, oldest first.
func (a *AcceptanceRig) InfraMerges() []AccInfraMerge {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.merges)
}

// Checked answers the candidates the change check judged, each its files
// by path with their text, oldest first.
func (a *AcceptanceRig) Checked() []map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.checked)
}

// Reloads answers the reload requests the merges made, acknowledged or
// not.
func (a *AcceptanceRig) Reloads() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reloads
}

// OnReload sets what a reload request runs: the loader's activation, the
// test's alone — an acknowledgement without it changes no policy.
func (a *AcceptanceRig) OnReload(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onReload = fn
}

// SetLoadedPool installs the escalation pool the policy in force holds for
// the code project, its money at eur, beside a month cap of EUR 600 and a
// task maximum of EUR 5. A reload's activation is its only honest caller.
func (a *AcceptanceRig) SetLoadedPool(eur float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pool = &policy.Escalation{Pool: policy.Draw{EUR: eur}}
}

// SetLoadedAutonomy installs the autonomy the policy in force holds for
// the code project — autonomous takes fund off the confirm list, so the
// agent raises from the pool without a click; manual puts it back. A
// reload's activation is its only honest caller.
func (a *AcceptanceRig) SetLoadedAutonomy(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autonomy = level
}

// LoadedPool answers the pool money the policy in force holds for the code
// project's own file, false for none.
func (a *AcceptanceRig) LoadedPool() (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pool == nil {
		return 0, false
	}
	return a.pool.Pool.EUR, true
}

// SwitchOnRead makes the fake's next read of path answer the file it
// holds and then land text on it, as another writer did between the
// change's own read and its guarded commit: the read after it sees the
// moved blob, and the guard names it.
func (a *AcceptanceRig) SwitchOnRead(path, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.switchPath, a.switchText = path, text
}

// RefuseCheck makes the change check refuse every next candidate with
// the lines; none named, the candidates load. With fail set, the check
// itself cannot read.
func (a *AcceptanceRig) RefuseCheck(lines []string, fail error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checkRefus, a.checkFail = lines, fail
}

// RefuseCommit makes the fake refuse every next commit with status and
// Forgejo's message; status 0 lands them.
func (a *AcceptanceRig) RefuseCommit(status int, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commitStat, a.commitMsg = status, message
}

// RefuseMerge makes the fake refuse every next merge with status and
// Forgejo's message; status 0 lands them.
func (a *AcceptanceRig) RefuseMerge(status int, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mergeStat, a.mergeMsg = status, message
}

// FailReload makes every next reload request answer err.
func (a *AcceptanceRig) FailReload(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadFail = err
}

// FailRecords makes the event spine refuse every next row with err.
func (a *AcceptanceRig) FailRecords(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recordFail = err
}
