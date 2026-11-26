package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	runnerImage  = "git.example/sf/ci-runner@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	storeHost    = "store.example"
	storeToken   = "store-token-123456"
	forgeAdminPw = "forge-admin-password-987654"
	forgeAdmin   = "forge-admin"
	goLine       = "docker.io/library/golang:1.26.11-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	nodeLine     = "node v24.22.3 node-v24.22.3-linux-x64.tar.xz https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// headFiles are a head's six files the copy reads.
func headFiles() map[string][]byte {
	return map[string][]byte{
		"upstream-images.txt":               []byte(goLine + "\n"),
		"upstream-files.txt":                []byte(nodeLine + "\n"),
		"go.mod":                            []byte("module example.com/m\n"),
		"go.sum":                            []byte("example.com/x v1.0.0 h1:x=\n"),
		"opencode-plugin/package.json":      []byte(`{"dependencies":{}}` + "\n"),
		"opencode-plugin/package-lock.json": []byte(`{"lockfileVersion":3}` + "\n"),
	}
}

// copyFiles are what the activity hands the pod: the head's files and
// main's two scripts.
func copyFiles() map[string][]byte {
	files := headFiles()
	files["scripts/upstream-copy"] = []byte("# main's upstream-copy\n")
	files["scripts/upstream-files"] = []byte("# main's upstream-files\n")
	return files
}

// tokenForgejo is the site admin's API for the copy's user, its team and its
// tokens: ensure, mint, list and revoke, with the admin's login checked.
type tokenForgejo struct {
	mu      sync.Mutex
	live    []tokenRec
	minted  []tokenRec
	revoked []string
	badAuth int
	next    int64
	users   map[string]string         // user -> email, as created
	teams   map[string]map[string]any // team name -> the body it was created with
	members map[string]bool           // user -> in the team
	srv     *httptest.Server
	// namespaceOf says whether the run named by a token's suffix has its
	// namespace: a token is minted into a run that exists
	namespaceOf func(suffix string) bool
}

type tokenRec struct {
	ID     int64
	Name   string
	Scopes []string
	User   string `json:"-"`
}

func newTokenForgejo(t *testing.T) *tokenForgejo {
	t.Helper()
	f := &tokenForgejo{users: map[string]string{}, teams: map[string]map[string]any{}, members: map[string]bool{}}
	f.srv = httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if u, p, _ := r.BasicAuth(); u != forgeAdmin || p != forgeAdminPw {
			f.badAuth++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/users/") && !strings.Contains(path[len("/api/v1/users/"):], "/"):
			email, ok := f.users[path[len("/api/v1/users/"):]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"email":%q}`, email)
		case r.Method == http.MethodPost && path == "/api/v1/admin/users":
			var body struct{ Username, Email string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.users[body.Username] = body.Email
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && path == "/api/v1/orgs/upstream/teams":
			var list []map[string]any
			for name, body := range f.teams {
				list = append(list, map[string]any{"id": 7, "name": name, "includes_all_repositories": body["includes_all_repositories"], "units_map": body["units_map"]})
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodPost && path == "/api/v1/orgs/upstream/teams":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.teams[body["name"].(string)] = body
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":7}`)
		case strings.HasPrefix(path, "/api/v1/teams/7/members/"):
			user := path[strings.LastIndex(path, "/")+1:]
			switch r.Method {
			case http.MethodGet:
				if !f.members[user] {
					http.NotFound(w, r)
				}
			case http.MethodPut:
				f.members[user] = true
				w.WriteHeader(http.StatusNoContent)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/tokens"):
			var body tokenRec
			_ = json.NewDecoder(r.Body).Decode(&body)
			if suffix := strings.TrimPrefix(body.Name, upstreamTokenPrefix); f.namespaceOf != nil && !f.namespaceOf(suffix) {
				http.Error(w, "no namespace of this run", http.StatusConflict)
				return
			}
			f.next++
			body.ID = f.next
			body.User = strings.Split(path, "/")[4]
			f.live = append(f.live, body)
			f.minted = append(f.minted, body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"sha1":%q}`, body.ID, storeToken)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/tokens"):
			user := strings.Split(path, "/")[4]
			if _, ok := f.users[user]; !ok {
				http.NotFound(w, r)
				return
			}
			list := []tokenRec{}
			for _, t := range f.live {
				if t.User == user {
					list = append(list, t)
				}
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodDelete:
			// a token is the path's user's: another user's is not found
			id, _ := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
			user := strings.Split(path, "/")[4]
			n := len(f.live)
			f.live = slices.DeleteFunc(f.live, func(t tokenRec) bool {
				if t.ID == id && t.User == user {
					f.revoked = append(f.revoked, t.Name)
					return true
				}
				return false
			})
			if len(f.live) == n {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *tokenForgejo) liveNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.live {
		out = append(out, t.Name)
	}
	return out
}

// ensureUser makes the copy's user, as the first run's start would.
func (f *tokenForgejo) ensureUser() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[UpstreamUser] = UpstreamUser + "@factory.local"
}

// copyPodKube runs the copy pod in the test when its pod is created: run
// reads the pod's Secret as the pod's mount would and answers its log and
// exit.
type copyPodKube struct {
	*fakeKube
	run       func(secret map[string]string) (log string, exit int32)
	created   []string
	status    map[string]int32
	logs      map[string]string
	pending   bool // the pod never starts
	deleteErr error
	event     string // the pod's newest event
	diagnosis PodDiagnosis
}

func (k *copyPodKube) CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error {
	if err := k.fakeKube.CreatePod(ctx, namespace, name, pod); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.created = append(k.created, namespace)
	if k.pending {
		return nil
	}
	log, exit := k.run(k.secrets[namespace+"/"+upstreamSecretName])
	k.logs[namespace], k.status[namespace] = log, exit
	return nil
}

// DeleteNamespace refuses a context that has ended, as the API client does.
func (k *copyPodKube) DeleteNamespace(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if k.deleteErr != nil {
		return k.deleteErr
	}
	return k.fakeKube.DeleteNamespace(ctx, name)
}

func (k *copyPodKube) PodStatus(ctx context.Context, namespace, name string) (string, int32, PodWait, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending {
		return "Pending", -1, PodWait{}, nil
	}
	exit, ok := k.status[namespace]
	if !ok {
		return "", -1, PodWait{}, ErrNotFound
	}
	if exit != 0 {
		return "Failed", exit, PodWait{}, nil
	}
	return "Succeeded", 0, PodWait{}, nil
}

func (k *copyPodKube) PodDiagnosis(ctx context.Context, namespace, name string) (PodDiagnosis, error) {
	if err := ctx.Err(); err != nil {
		return PodDiagnosis{}, err
	}
	return k.diagnosis, nil
}

func (k *copyPodKube) NewestPodEvent(ctx context.Context, namespace, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return k.event, nil
}

func (k *copyPodKube) PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return io.NopCloser(strings.NewReader(k.logs[namespace])), nil
}

func (k *copyPodKube) PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	all := strings.SplitAfter(k.logs[namespace], "\n")
	return strings.Join(all[max(0, len(all)-lines):], ""), nil
}

// newUpstreamPod is an UpstreamPod whose pod prints nothing and succeeds,
// with the site admin's login on a token API of its own.
func newUpstreamPod(t *testing.T) *UpstreamPod {
	t.Helper()
	k := &copyPodKube{fakeKube: newFakeKube(), status: map[string]int32{}, logs: map[string]string{}}
	k.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	k.run = func(map[string]string) (string, int32) { return "", 0 }
	tf := newTokenForgejo(t)
	tokenServers[tf.srv.URL] = tf
	tf.namespaceOf = func(suffix string) bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		_, ok := k.namespaces[upstreamNamespacePrefix+suffix]
		return ok
	}
	return &UpstreamPod{
		Kube: k, Namespace: "factory", PullSecret: "forgejo-registry", RunnerImage: runnerImage, StoreHost: storeHost,
		Forgejo: &Forgejo{BaseURL: tf.srv.URL},
		OpenBao: &fakeKV{data: map[string]string{"username": forgeAdmin, "password": forgeAdminPw}},
		Poll:    time.Millisecond,
	}
}

func podKube(u *UpstreamPod) *copyPodKube { return u.Kube.(*copyPodKube) }

// tokens is the token API behind u.Forgejo.
func tokens(t *testing.T, u *UpstreamPod) *tokenForgejo {
	t.Helper()
	return tokenServers[u.Forgejo.BaseURL]
}

var tokenServers = map[string]*tokenForgejo{}

func shortRuns(t *testing.T) {
	t.Helper()
	d, g, s := upstreamDeadline, upstreamStartGrace, upstreamSweepSlack
	t.Cleanup(func() { upstreamDeadline, upstreamStartGrace, upstreamSweepSlack = d, g, s })
}

// The pod runs the CI runner image at factoryd's pin over a Secret that holds
// the run's minted token and the files the activity read, main's two scripts
// among them: its program runs those scripts and no file of the head, and
// its one env is the store the scripts write.
func TestUpstreamPodRunsMainsScriptsOverTheHeadsFilesInTheRunnerImage(t *testing.T) {
	u := newUpstreamPod(t)
	k := podKube(u)
	var spec map[string]any
	k.run = func(map[string]string) (string, int32) {
		// the namespace is live while the pod runs
		for ns := range k.namespaces {
			spec = k.pods[ns+"/"+upstreamPodName].spec
			want := map[string]string{
				"user": UpstreamUser, "password": storeToken,
				"upstream-images.txt": goLine + "\n", "upstream-files.txt": nodeLine + "\n",
				"go.mod": "module example.com/m\n", "go.sum": "example.com/x v1.0.0 h1:x=\n",
				"package.json": `{"dependencies":{}}` + "\n", "package-lock.json": `{"lockfileVersion":3}` + "\n",
				"upstream-copy": "# main's upstream-copy\n", "upstream-files": "# main's upstream-files\n",
			}
			if got := k.secrets[ns+"/"+upstreamSecretName]; !maps.Equal(got, want) {
				t.Errorf("the pod's Secret = %v, want %v", got, want)
			}
			if raw, _ := json.Marshal(k.secrets); strings.Contains(string(raw), forgeAdminPw) {
				t.Error("the admin's password reached a Secret")
			}
			if !strings.HasPrefix(ns, "factory-agents-upstream-") {
				t.Errorf("namespace %q lacks the prefix", ns)
			}
			labels := k.namespaces[ns]
			if labels["pod-security.kubernetes.io/enforce"] != "restricted" || !strings.HasPrefix(labels["factory.ai/task"], "upstream-") || labels[UpstreamLabel] != "true" {
				t.Errorf("labels = %v", labels)
			}
			// the policy the run created, not the builder's output
			requirePolicy(t, k.cnps[ns+"/upstream-egress"])
		}
		return "", 0
	}
	if _, err := u.Copy(t.Context(), copyFiles()); err != nil {
		t.Fatal(err)
	}
	if spec == nil {
		t.Fatal("no pod was created")
	}
	pod := spec["spec"].(map[string]any)
	if pod["runtimeClassName"] != "gvisor" || pod["automountServiceAccountToken"] != false {
		t.Errorf("runtimeClassName %v, automount %v", pod["runtimeClassName"], pod["automountServiceAccountToken"])
	}
	// the pod ends at its own deadline before the wait's bound
	if got, want := pod["activeDeadlineSeconds"], int64(upstreamDeadline/time.Second); got != want || time.Duration(want)*time.Second >= upstreamDeadline+upstreamStartGrace {
		t.Errorf("activeDeadlineSeconds = %v, want %d, under the wait's bound", got, want)
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["image"] != runnerImage {
		t.Errorf("image = %v, want the CI runner's %s", c["image"], runnerImage)
	}
	if !reflect.DeepEqual(c["command"], []any{"bash", "-c", upstreamProgram}) || c["args"] != nil {
		t.Errorf("command %v, args %v, want bash running the program", c["command"], c["args"])
	}
	for _, want := range []string{
		`cp "$in/upstream-images.txt" upstream-images.txt`, `cp "$in/upstream-files.txt" upstream-files.txt`,
		`cp "$in/go.mod" go.mod`, `cp "$in/go.sum" go.sum`,
		`cp "$in/package.json" opencode-plugin/package.json`, `cp "$in/package-lock.json" opencode-plugin/package-lock.json`,
		`cp "$in/upstream-copy" scripts/upstream-copy`, `cp "$in/upstream-files" scripts/upstream-files`,
		"bash scripts/upstream-copy ", "bash scripts/upstream-files ",
	} {
		if !strings.Contains(upstreamProgram, want) {
			t.Errorf("the program lacks %q:\n%s", want, upstreamProgram)
		}
	}
	// the program runs main's two scripts and nothing else the Secret holds
	if runs := regexp.MustCompile(`(?m)^(bash|sh|make|\./)\S*\s+\S+`).FindAllString(upstreamProgram, -1); !slices.Equal(runs, []string{"bash scripts/upstream-copy", "bash scripts/upstream-files"}) {
		t.Errorf("the program runs %q, want main's two scripts alone", runs)
	}
	if want := []any{map[string]any{"name": "UPSTREAM_STORE", "value": storeHost + "/upstream"}}; !reflect.DeepEqual(c["env"], want) {
		t.Errorf("env = %v, want %v alone: the token is a file", c["env"], want)
	}
	res := c["resources"].(map[string]any)["limits"].(map[string]any)
	if res["cpu"] == nil || res["memory"] == nil {
		t.Errorf("limits = %v, want cpu and memory", res)
	}
	sc := c["securityContext"].(map[string]any)
	if sc["runAsNonRoot"] != true || sc["runAsUser"] == 0 || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext = %v", sc)
	}
	raw, _ := json.Marshal(spec)
	if strings.Contains(string(raw), storeToken) || strings.Contains(string(raw), forgeAdminPw) {
		t.Error("the stored pod spec carries a password")
	}
	volumes := map[string]map[string]any{}
	for _, v := range pod["volumes"].([]any) {
		volumes[v.(map[string]any)["name"].(string)] = v.(map[string]any)
	}
	mounts := map[string]map[string]any{}
	for _, m := range c["volumeMounts"].([]any) {
		mounts[m.(map[string]any)["mountPath"].(string)] = m.(map[string]any)
	}
	login := mounts[upstreamSecretDir]
	if login == nil || login["readOnly"] != true || volumes[login["name"].(string)]["secret"].(map[string]any)["secretName"] != upstreamSecretName {
		t.Errorf("the Secret's mount %v of volumes %v, want %s read-only from %s", login, volumes, upstreamSecretDir, upstreamSecretName)
	}
	// the root is read-only: the work directory, crane's login, go's module
	// cache and npm's logs live on these
	for _, dir := range []string{"/tmp", "/home/runner"} {
		if m := mounts[dir]; m == nil || volumes[m["name"].(string)]["emptyDir"] == nil {
			t.Errorf("no emptyDir at %s in %v", dir, mounts)
		}
	}
}

// The pod's program, run with stub scripts and crane: the files land at
// their repository paths in a directory of its own, crane logs into the
// store's host with the run's login, both scripts run there whatever the
// first one ended with, and each line a script could not copy comes again
// at the end, led by the finding mark, with a failed exit.
func TestUpstreamPodProgramRunsBothScriptsAndRepeatsWhatTheyCouldNotCopy(t *testing.T) {
	for name, c := range map[string]struct {
		copyExit, filesExit string
		findings            []string
		exit                int
	}{
		"both copied":  {"exit 0", "exit 0", nil, 0},
		"both refused": {"exit 1", "exit 1", []string{"finding: upstream-copy: docker.io/x:1@sha256:ab: the copy failed", "finding: upstream-files: vulndb.zip: its URL does not serve the pinned sha256 abc"}, 1},
		"one refused":  {"exit 0", "exit 1", []string{"finding: upstream-files: vulndb.zip: its URL does not serve the pinned sha256 abc"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			in, bin := t.TempDir(), t.TempDir()
			files := copyFiles()
			copyErr := ""
			if c.copyExit != "exit 0" {
				copyErr = `echo "upstream-copy: docker.io/x:1@sha256:ab: the copy failed" >&2` + "\n"
			}
			files["scripts/upstream-copy"] = []byte(`echo "copy ran in $PWD as $REGISTRY_USER"` + "\n" + copyErr + c.copyExit + "\n")
			filesErr := ""
			if c.filesExit != "exit 0" {
				filesErr = `echo "upstream-files: vulndb.zip: its URL does not serve the pinned sha256 abc" >&2` + "\n"
			}
			files["scripts/upstream-files"] = []byte(`for f in upstream-images.txt upstream-files.txt go.mod go.sum opencode-plugin/package.json opencode-plugin/package-lock.json; do test -f "$f" || { echo "no $f"; exit 3; }; done` + "\n" +
				`echo "files ran with $REGISTRY_USER:$REGISTRY_PASSWORD"` + "\n" + filesErr + c.filesExit + "\n")
			write := func(dir, name, body string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			for p, b := range files {
				write(in, filepath.Base(p), string(b), 0o644)
			}
			write(in, "user", UpstreamUser, 0o644)
			write(in, "password", storeToken, 0o644)
			write(bin, "crane", "#!/bin/sh\necho \"crane $* $(cat)\" >> "+bin+"/calls\n", 0o755)
			cmd := exec.Command("bash", "-c", strings.ReplaceAll(upstreamProgram, upstreamSecretDir, in))
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "UPSTREAM_STORE=" + storeHost + "/upstream"}
			out, err := cmd.CombinedOutput()
			exit := 0
			if err != nil {
				exit = cmd.ProcessState.ExitCode()
			}
			if exit != c.exit {
				t.Fatalf("exit %d, want %d: %s", exit, c.exit, out)
			}
			for _, want := range []string{"copy ran in ", "as " + UpstreamUser, "files ran with " + UpstreamUser + ":" + storeToken} {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			var marked []string
			for _, l := range lines {
				if strings.HasPrefix(l, upstreamFinding) {
					marked = append(marked, l)
				}
			}
			if !slices.Equal(marked, c.findings) || len(c.findings) > 0 && !slices.Equal(lines[len(lines)-len(c.findings):], c.findings) {
				t.Errorf("marked lines %q, want %q at the end:\n%s", marked, c.findings, out)
			}
			calls, err := os.ReadFile(filepath.Join(bin, "calls"))
			if want := "crane auth login " + storeHost + " -u " + UpstreamUser + " --password-stdin " + storeToken + "\n"; err != nil || string(calls) != want {
				t.Errorf("crane calls %q (%v), want %q", calls, err, want)
			}
		})
	}
}

func TestUpstreamPodRunsAsADedicatedUserOfAPackageWriteTeamOfOrgUpstream(t *testing.T) {
	u := newUpstreamPod(t)
	f := tokens(t, u)
	if _, err := u.Copy(t.Context(), copyFiles()); err != nil {
		t.Fatal(err)
	}
	if len(f.minted) != 1 || f.minted[0].User != UpstreamUser || f.minted[0].User == forgeAdmin {
		t.Fatalf("minted %+v, want one token of %s, not the site admin's", f.minted, UpstreamUser)
	}
	if email := f.users[UpstreamUser]; email != UpstreamUser+"@factory.local" {
		t.Errorf("users = %v, want %s made by registration's rule", f.users, UpstreamUser)
	}
	if len(f.teams) != 1 {
		t.Fatalf("teams = %v, want one in org upstream", f.teams)
	}
	team := f.teams[upstreamTeam]
	units, _ := team["units_map"].(map[string]any)
	if team == nil || team["includes_all_repositories"] != false || len(units) != 1 || units["repo.packages"] != "write" || team["permission"] != "write" || team["can_create_org_repo"] != false {
		t.Errorf("team %s = %v, want package write on no repository and nothing else", upstreamTeam, team)
	}
	if !f.members[UpstreamUser] || len(f.members) != 1 {
		t.Errorf("members = %v, want %s alone", f.members, UpstreamUser)
	}
}

func TestUpstreamPodMintsAPackageTokenAndRevokesItAtEachEnd(t *testing.T) {
	for name, c := range map[string]struct {
		exit    int32
		pending bool
	}{"success": {}, "failure": {exit: 1}, "timeout": {pending: true}} {
		t.Run(name, func(t *testing.T) {
			shortRuns(t)
			upstreamDeadline, upstreamStartGrace = 30*time.Millisecond, 30*time.Millisecond
			u := newUpstreamPod(t)
			f := tokens(t, u)
			k := podKube(u)
			k.pending = c.pending
			k.run = func(map[string]string) (string, int32) { return "", c.exit }
			_, err := u.Copy(t.Context(), copyFiles())
			if (err != nil) != (c.exit != 0 || c.pending) {
				t.Fatalf("err = %v", err)
			}
			if len(f.minted) != 1 || !strings.HasPrefix(f.minted[0].Name, upstreamTokenPrefix) || !slices.Equal(f.minted[0].Scopes, []string{"write:package"}) {
				t.Fatalf("minted %+v, want one write:package token named %s...", f.minted, upstreamTokenPrefix)
			}
			if live := f.liveNames(); len(live) != 0 || !slices.Equal(f.revoked, []string{f.minted[0].Name}) {
				t.Errorf("live %v, revoked %v: want the minted token revoked", live, f.revoked)
			}
			if f.badAuth != 0 {
				t.Errorf("%d token calls without the admin's login", f.badAuth)
			}
		})
	}
}

func TestUpstreamPodRevokesATokenWhoseRunIsGoneAtOnce(t *testing.T) {
	u := newUpstreamPod(t)
	f := tokens(t, u)
	f.ensureUser()
	// no namespace of the orphan's run exists, however young it is; the
	// other run is live
	orphan := fmt.Sprintf("%s%d-aaaa", upstreamTokenPrefix, time.Now().Unix())
	running := fmt.Sprintf("%s%d-bbbb", upstreamTokenPrefix, time.Now().Unix())
	podKube(u).namespaces[upstreamNamespacePrefix+strings.TrimPrefix(running, upstreamTokenPrefix)] = map[string]string{UpstreamLabel: "true"}
	f.live = []tokenRec{{ID: 90, Name: orphan, User: UpstreamUser}, {ID: 91, Name: running, User: UpstreamUser}, {ID: 92, Name: "registry", User: UpstreamUser}}
	if _, err := u.Copy(t.Context(), copyFiles()); err != nil {
		t.Fatal(err)
	}
	if live := f.liveNames(); !slices.Equal(live, []string{running, "registry"}) {
		t.Errorf("live tokens %v, want the orphan revoked and the live run's and the other kept", live)
	}
}

// A pass lists the tokens at most every 10 minutes: factoryd's boot and a
// run's start force it.
func TestUpstreamPodSweepListsTokensWhenDueOrForced(t *testing.T) {
	u := newUpstreamPod(t)
	f := tokens(t, u)
	f.ensureUser()
	u.Sweep(t.Context(), true)
	f.mu.Lock()
	f.live = []tokenRec{{ID: 90, Name: upstreamTokenPrefix + "1-aaaa", User: UpstreamUser}}
	f.mu.Unlock()
	u.Sweep(t.Context(), false)
	if len(f.liveNames()) != 1 {
		t.Fatal("a pass inside the interval listed the tokens")
	}
	u.Sweep(t.Context(), true)
	if len(f.liveNames()) != 0 {
		t.Error("a forced sweep left the orphan")
	}
}

// Before the first run, the copy's user does not exist: the sweep logs
// nothing for that normal state and revokes nothing.
func TestUpstreamPodSweepTreatsAMissingUserAsNoTokens(t *testing.T) {
	u := newUpstreamPod(t)
	f := tokens(t, u)
	logs := captureLog(t)
	u.Sweep(t.Context(), true)
	if got := logs.String(); strings.Contains(got, "list the copy's tokens") || strings.Contains(got, "not found") {
		t.Errorf("a sweep without the copy's user logged %q", got)
	}
	if len(f.revoked) != 0 || len(f.liveNames()) != 0 {
		t.Errorf("revoked %v, live %v: want nothing to revoke", f.revoked, f.liveNames())
	}
}

// factoryd's boot: BootSweep's namespaces are still Terminating when the
// forced sweep runs, and the tokens of those runs go.
func TestUpstreamPodBootSweepRevokesTheTokenOfATerminatingRun(t *testing.T) {
	u := newUpstreamPod(t)
	k, f := podKube(u), tokens(t, u)
	f.ensureUser()
	suffix := fmt.Sprintf("%d-aaaa", time.Now().Unix())
	ns := upstreamNamespacePrefix + suffix
	k.namespaces[ns] = map[string]string{UpstreamLabel: "true"}
	k.terminating[ns] = true
	f.live = []tokenRec{{ID: 90, Name: upstreamTokenPrefix + suffix, User: UpstreamUser}}
	u.Sweep(t.Context(), true)
	if live := f.liveNames(); len(live) != 0 {
		t.Errorf("live tokens %v, want the Terminating run's token revoked", live)
	}
}

// The run's context ends, as an activity's deadline or a worker's stop ends
// it: the token and the namespace go on contexts of their own, and a
// namespace that will not delete leaves the revoked token revoked.
func TestUpstreamPodRevokesAndDeletesWhenTheRunsContextEnds(t *testing.T) {
	for name, deleteErr := range map[string]error{"deletes": nil, "delete fails": fmt.Errorf("api down")} {
		t.Run(name, func(t *testing.T) {
			u := newUpstreamPod(t)
			k := podKube(u)
			k.pending = true
			k.deleteErr = deleteErr
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			k.onCreatePod = cancel
			k.diagnosis = PodDiagnosis{Phase: "Pending"}
			k.event = "Pulling: image"
			_, err := u.Copy(ctx, copyFiles())
			if err == nil || !strings.Contains(err.Error(), "waiting for the copy pod") || strings.Contains(err.Error(), "did not end within") || !strings.Contains(err.Error(), "phase Pending") || !strings.Contains(err.Error(), "newest event Pulling: image") {
				t.Fatalf("err = %v, want the cancelled run's error with the pod's state", err)
			}
			f := tokens(t, u)
			if live := f.liveNames(); len(live) != 0 || len(f.revoked) != 1 {
				t.Errorf("live tokens %v, revoked %v: want the run's token revoked", live, f.revoked)
			}
			if _, ok := k.namespaces[k.created[0]]; ok != (deleteErr != nil) {
				t.Errorf("namespace live = %v with delete error %v", ok, deleteErr)
			}
		})
	}
}

func TestUpstreamPodNeverStartingPodEndsWithItsState(t *testing.T) {
	shortRuns(t)
	upstreamDeadline, upstreamStartGrace = 20*time.Millisecond, 20*time.Millisecond
	u := newUpstreamPod(t)
	k := podKube(u)
	k.pending = true
	k.diagnosis = PodDiagnosis{Phase: "Pending", Conditions: []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available"}}}
	k.event = "FailedCreatePodSandBox: gvisor: bearer abcdefgh12345678abcdefgh12345678abcd"
	_, err := u.Copy(t.Context(), copyFiles())
	if err == nil {
		t.Fatal("a pod that never started ended the copy without an error")
	}
	if strings.Contains(err.Error(), "abcdefgh12345678abcdefgh12345678abcd") {
		t.Errorf("error %q carries the event's secret", err)
	}
	for _, want := range []string{"did not end within", "phase Pending", "Unschedulable: 0/3 nodes are available", "newest event FailedCreatePodSandBox: gvisor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if len(k.namespaces) != 0 {
		t.Errorf("namespaces left: %v", k.namespaces)
	}
}

// A pod that fails with no line marked as a finding failed itself, not a
// line: the caller gets its exit and its last log lines, scrubbed.
func TestUpstreamPodFailedCopyReachesTheCallerWithItsScrubbedReason(t *testing.T) {
	u := newUpstreamPod(t)
	k := podKube(u)
	k.diagnosis = PodDiagnosis{Phase: "Failed", Reason: "DeadlineExceeded"}
	k.event = "Killing: Stopping container copy"
	k.run = func(map[string]string) (string, int32) {
		return "copying\nError: put https://root:hunter2hunter2@store/v2 Authorization: Bearer abcdefgh12345678 " + storeToken + "\n", 3
	}
	_, err := u.Copy(t.Context(), copyFiles())
	if err == nil {
		t.Fatal("a failed copy returned no error")
	}
	for _, want := range []string{"exit 3", "failed", "Error: put", "phase Failed", "DeadlineExceeded", "newest event Killing: Stopping container copy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for _, leak := range []string{"hunter2hunter2", "abcdefgh12345678", storeToken} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q", err, leak)
		}
	}
}

func TestUpstreamPodNamespaceIsRemovedAfterSuccessAndAfterFailure(t *testing.T) {
	for name, exit := range map[string]int32{"success": 0, "failure": 1} {
		t.Run(name, func(t *testing.T) {
			u := newUpstreamPod(t)
			k := podKube(u)
			k.run = func(map[string]string) (string, int32) { return "", exit }
			_, err := u.Copy(t.Context(), copyFiles())
			if (err != nil) != (exit != 0) {
				t.Fatalf("err = %v, exit %d", err, exit)
			}
			if len(k.created) != 1 || len(k.namespaces) != 0 || len(k.deletedNs) != 1 || k.deletedNs[0] != k.created[0] {
				t.Errorf("created %v, live %v, deleted %v: want the one namespace gone", k.created, k.namespaces, k.deletedNs)
			}
		})
	}
}

// The scripts exit 1 when a line fails, and the program repeats each such
// line at its end: a failed pod whose log holds them returns them as the
// findings, what the script printed, and no error.
func TestUpstreamPodFindingsComeBackFromThePodsLog(t *testing.T) {
	u := newUpstreamPod(t)
	podKube(u).run = func(map[string]string) (string, int32) {
		return "copied docker.io/library/golang:1.26.11\n" +
			"upstream-files: vulndb.zip: its URL does not serve the pinned sha256 0a54\n" +
			"uploaded node v24.22.3 node-v24.22.3-linux-x64.tar.xz\n" +
			"finding: upstream-files: vulndb.zip: its URL does not serve the pinned sha256 0a54\n" +
			"finding: upstream-copy: https://github.com/x/y.tgz@sha256:ab helm: the upload failed\n", 1
	}
	findings, err := u.Copy(t.Context(), copyFiles())
	want := []string{"upstream-files: vulndb.zip: its URL does not serve the pinned sha256 0a54", "upstream-copy: https://github.com/x/y.tgz@sha256:ab helm: the upload failed"}
	if err != nil || !slices.Equal(findings, want) {
		t.Fatalf("findings %q, err %v; want %q", findings, err, want)
	}
}

func TestUpstreamPodScrubsTheFindingsItReadsBack(t *testing.T) {
	u := newUpstreamPod(t)
	podKube(u).run = func(map[string]string) (string, int32) {
		return upstreamFinding + "upstream-files: get https://root:hunter2hunter2@store/v2 Authorization: Bearer abcdefgh12345678 " + storeToken + "\n", 1
	}
	findings, err := u.Copy(t.Context(), copyFiles())
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings %q, err %v", findings, err)
	}
	for _, leak := range []string{"hunter2hunter2", "abcdefgh12345678", storeToken} {
		if strings.Contains(findings[0], leak) {
			t.Errorf("finding %q carries %q", findings[0], leak)
		}
	}
}

func TestUpstreamPodRefusesARunnerImageWithoutADigest(t *testing.T) {
	u := newUpstreamPod(t)
	u.RunnerImage = "git.example/sf/ci-runner:main"
	k := podKube(u)
	_, err := u.Copy(t.Context(), copyFiles())
	if err == nil || !strings.Contains(err.Error(), "git.example/sf/ci-runner:main") || !strings.Contains(err.Error(), "@sha256:") || !strings.Contains(err.Error(), "CI_RUNNER_IMAGE") {
		t.Fatalf("err = %v, want a refusal naming the image, CI_RUNNER_IMAGE and the digest pin", err)
	}
	if len(k.created) != 0 || len(tokens(t, u).minted) != 0 {
		t.Errorf("a run began: pods %v, tokens %v", k.created, tokens(t, u).minted)
	}
}

// A line whose host the pod's egress does not admit is a finding that names
// the host, beside what the pod copies: an https line by its URL's host, an
// image line by its registry, docker.io's being index.docker.io as crane
// names it.
func TestUpstreamPodNamesAHostOutsideItsEgress(t *testing.T) {
	u := newUpstreamPod(t)
	files := copyFiles()
	mcr := "mcr.microsoft.com/dotnet/runtime:9.0@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	chart := "https://charts.example.org/x-1.0.0.tgz@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	files["upstream-images.txt"] = []byte("# images\n" + goLine + "\n" + mcr + "\n" + chart + " helm\n" +
		"https://github.com/SigNoz/charts/releases/download/signoz-0.151.0/signoz-0.151.0.tgz@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff helm\n" +
		"# https://comment.example.org/x.tgz@sha256:ab helm\n\n")
	files["upstream-files.txt"] = []byte(nodeLine + "\n" +
		"tool v1 tool.tar.gz https://downloads.example.net/tool.tar.gz 1111111111111111111111111111111111111111111111111111111111111111\n")
	findings, err := u.Copy(t.Context(), files)
	if err != nil {
		t.Fatal(err)
	}
	const fix = ", outside the copy's egress: a commit adds the host to upstreamFQDNs in internal/factoryd/upstream_pod.go"
	want := []string{
		"upstream-images.txt: " + mcr + " names mcr.microsoft.com" + fix,
		"upstream-images.txt: " + chart + " names charts.example.org" + fix,
		"upstream-files.txt: https://downloads.example.net/tool.tar.gz names downloads.example.net" + fix,
	}
	if !slices.Equal(findings, want) {
		t.Errorf("findings\n%q\nwant\n%q", findings, want)
	}
	if len(podKube(u).created) != 1 {
		t.Errorf("pods %v, want the copy of the other lines", podKube(u).created)
	}
}

func TestUpstreamPodSweepsOnlyNamespacesOlderThanTheLongestRun(t *testing.T) {
	u := newUpstreamPod(t)
	k := podKube(u)
	ago := func(d time.Duration, tag string) string {
		return fmt.Sprintf("factory-agents-upstream-%d-%s", time.Now().Add(-d).Unix(), tag)
	}
	// the longest run is 27 minutes; its sweep age is 32
	old, justUnder, young := ago(40*time.Minute, "aaaa"), ago(30*time.Minute, "bbbb"), ago(0, "cccc")
	for _, ns := range []string{old, justUnder, young} {
		k.namespaces[ns] = map[string]string{UpstreamLabel: "true"}
	}
	u.Sweep(t.Context(), false)
	for ns, want := range map[string]bool{old: false, justUnder: true, young: true} {
		if _, ok := k.namespaces[ns]; ok != want {
			t.Errorf("namespace %s live = %v, want %v", ns, ok, want)
		}
	}
}

// The CI spawner's reconcile pass runs the upstream sweep: a leftover goes
// in the next pass.
func TestCISpawnerReconcileSweepsALeftoverUpstreamNamespace(t *testing.T) {
	kube := newFakeKube()
	spawner := newTestCISpawner(t, kube, &ciHub{})
	u := newUpstreamPod(t)
	k := podKube(u)
	old := fmt.Sprintf("factory-agents-upstream-%d-aaaa", time.Now().Add(-time.Hour).Unix())
	k.namespaces[old] = map[string]string{UpstreamLabel: "true"}
	spawner.SweepEachPass(func(ctx context.Context) { u.Sweep(ctx, false) })
	spawner.reconcile(t.Context(), codeCIRepos, &runnerStarts{})
	if _, ok := k.namespaces[old]; ok {
		t.Error("the leftover namespace survived a reconcile pass")
	}
}

func TestAdmitLeasesSkipsAnUpstreamCopyNamespace(t *testing.T) {
	kube := newFakeKube()
	kube.namespaces["factory-agents-upstream-1700000000-abcd"] = map[string]string{"factory.ai/task": "upstream-1700000000-abcd"}
	kube.namespaces["factory-agents-t1"] = map[string]string{"factory.ai/task": "t1"}
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{}}
	r := &Runner{cfg: cfg, kube: kube, store: newFakeStore()}
	if err := r.AdmitLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kube.applies, ","); got != "factory-agents-t1/worker-egress" {
		t.Errorf("applies = %s, want the task's alone", got)
	}
}

// upstreamSecondHops are the hosts a first host of the scripts' tools sends
// them on to, which no dial from here shows: a token realm a registry's 401
// names, or a redirect of a registry API, a blob or a release download.
// Read with crane --verbose (each image line's manifest, a child's and its
// config blob) and curl -sSL -v (each https line of both lists) on
// 2026-11-25; Docker Hub's other two blob hosts and us-west1 come from
// #1698's runs of the Go copier.
var upstreamSecondHops = map[string]string{
	"auth.docker.io":                   "Docker Hub's token realm, which index.docker.io's 401 names",
	"production.cloudfront.docker.com": "where index.docker.io redirects a blob",
	"production.cloudflare.docker.com": "where index.docker.io redirects a blob",
	"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com": "where index.docker.io redirects a blob",
	"pkg-containers.githubusercontent.com":                                         "where ghcr.io redirects a blob",
	"cdn*.quay.io":                                                                 "where quay.io redirects a blob (cdn01 seen)",
	"cdn.registry.k8s.io":                                                          "where registry.k8s.io redirects a blob",
	"*-docker.pkg.dev":                                                             "where registry.k8s.io redirects its API (europe-west3 seen, us-west1 in #1698)",
	"cdn.factory.talos.dev":                                                        "where factory.talos.dev redirects a blob",
	"release-assets.githubusercontent.com":                                         "where github.com redirects a release download",
	"dl.google.com":                                                                "where go.dev redirects a Go tarball (seen 2026-11-25)",
}

// podEgressFor is whether the pod's DNS rule resolves host and an FQDN rule
// admits it on 443/TCP alone, by matchName or matchPattern (Cilium's `*` is
// the characters of one DNS label).
func podEgressFor(t *testing.T, host string) (resolves, admits bool) {
	t.Helper()
	raw, err := json.Marshal(upstreamEgressSpec(storeHost))
	if err != nil {
		t.Fatal(err)
	}
	type name struct{ MatchName, MatchPattern string }
	type ports []struct {
		Ports []struct{ Port, Protocol string }
		Rules struct{ DNS []name }
	}
	var spec struct {
		Egress []struct {
			ToFQDNs []name
			ToPorts ports
		}
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	has := func(ns []name) bool {
		return slices.ContainsFunc(ns, func(n name) bool {
			if n.MatchPattern == "" {
				return n.MatchName == host
			}
			return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(n.MatchPattern), `\*`, "[-a-zA-Z0-9_]*") + "$").MatchString(host)
		})
	}
	for _, rule := range spec.Egress {
		for _, p := range rule.ToPorts {
			resolves = resolves || has(p.Rules.DNS)
		}
		if has(rule.ToFQDNs) {
			admits = len(rule.ToPorts) == 1 && len(rule.ToPorts[0].Ports) == 1 &&
				rule.ToPorts[0].Ports[0].Port == "443" && rule.ToPorts[0].Ports[0].Protocol == "TCP"
		}
	}
	return resolves, admits
}

// The real scripts run over the real lists, go.mod, go.sum and plugin
// lockfile, crane, curl and go sent through a proxy that records each host
// a tool dials and refuses it. npm reconnects to a refusing proxy without
// end, so its hosts are read where npm ci reads them: each resolved URL of
// the lockfile, and its registry. Each host is admitted by name, or is the
// store, which the pod resolves and reaches through the edge gateway; and
// each entry of upstreamFQDNs is such a host or one of upstreamSecondHops,
// so the list names the hosts the tools use and no other.
func TestUpstreamPodEgressAdmitsEveryHostTheScriptsDial(t *testing.T) {
	for _, tool := range []string{"crane", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH: the CI runner image, where the copy pod and the checks run, has it", tool)
		}
	}
	var mu sync.Mutex
	dialled := map[string]bool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		mu.Lock()
		dialled[host] = true
		mu.Unlock()
		http.Error(w, "refused", http.StatusForbidden)
	}))
	t.Cleanup(proxy.Close)
	work := t.TempDir()
	for _, p := range upstreamHeadFiles {
		raw, err := os.ReadFile(filepath.Join("..", "..", p))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, p), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{
		// the go that built this test, which go.mod's go line admits
		"PATH=" + bin + string(os.PathListSeparator) + filepath.Join(runtime.GOROOT(), "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home, "TMPDIR=" + t.TempDir(), "GOMODCACHE=" + filepath.Join(home, "mod"),
		"GOPROXY=https://proxy.golang.org,direct", "GOSUMDB=sum.golang.org",
		"UPSTREAM_STORE=" + storeHost + "/upstream", "REGISTRY_USER=u", "REGISTRY_PASSWORD=p",
		"HTTPS_PROXY=" + proxy.URL, "https_proxy=" + proxy.URL, "HTTP_PROXY=" + proxy.URL, "http_proxy=" + proxy.URL,
		"NO_PROXY=", "no_proxy=",
	}
	for _, script := range upstreamScripts {
		path, err := filepath.Abs(filepath.Join("..", "..", script))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", path)
		cmd.Dir, cmd.Env = work, env
		// every line fails at the proxy: the hosts are the result
		out, _ := cmd.CombinedOutput()
		t.Logf("%s:\n%s", script, out)
	}
	var lock struct {
		Packages map[string]struct{ Resolved string } `json:"packages"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "opencode-plugin", "package-lock.json"))
	if err != nil || json.Unmarshal(raw, &lock) != nil {
		t.Fatalf("the plugin lockfile: %v", err)
	}
	dialled["registry.npmjs.org"] = true
	for _, p := range lock.Packages {
		if u, err := url.Parse(p.Resolved); err == nil && u.Host != "" {
			dialled[u.Hostname()] = true
		}
	}
	hosts := slices.Sorted(maps.Keys(dialled))
	for _, h := range hosts {
		resolves, admits := podEgressFor(t, h)
		if h == storeHost {
			admits = true
		}
		if !resolves || !admits {
			t.Errorf("the scripts dial %s, which the pod's policy resolves %v, admits on 443 %v", h, resolves, admits)
		}
	}
	for _, e := range upstreamFQDNs {
		m := e.(map[string]any)
		name, _ := m["matchName"].(string)
		if name == "" {
			name, _ = m["matchPattern"].(string)
		}
		if _, hop := upstreamSecondHops[name]; !dialled[name] && !hop {
			t.Errorf("upstreamFQDNs admits %s, which the scripts do not dial and no second hop names: %v", name, hosts)
		}
	}
}

// The edge gateway's Envoy is an L7 load balancer that enforces a pod's own
// egress policy on its upstream leg too (enforce_policy_on_l7lb in its
// bpf_metadata, and cilium.l7policy on the forgejo-http cluster): a pod that
// reaches the store's public host needs entity ingress on 443 for the first
// leg and Forgejo's HTTP port for the second, or the Envoy answers 403
// "Access denied". The copy pod reaches the store as a build runner does.
func TestUpstreamPodReachesTheStoreAsTheBuildRunnerDoes(t *testing.T) {
	runner := (&CISpawner{cfg: CIConfig{GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000", RegistryHost: storeHost}}).
		ciEgressSpec(&BuildVM{IPv4: "10.0.0.5"}, CIRepo{}, "")
	pod := upstreamEgressSpec(storeHost)
	for _, rule := range []map[string]any{forgejoHTTPEgress(), edgeGatewayEgress()} {
		if !slices.ContainsFunc(runner["egress"].([]any), func(r any) bool { return reflect.DeepEqual(r, rule) }) {
			t.Fatalf("the build runner's policy lacks %v: the store's route is stated wrong here", rule)
		}
		if !slices.ContainsFunc(pod["egress"].([]any), func(r any) bool { return reflect.DeepEqual(r, rule) }) {
			t.Errorf("the copy pod's policy lacks %v, which the build runner reaches the store by", rule)
		}
	}
	if resolves, _ := podEgressFor(t, storeHost); !resolves {
		t.Errorf("the copy pod does not resolve the store %s", storeHost)
	}
}

// The patterns admit names under them, in the DNS rule and the 443 rule
// alike, and no name outside them.
func TestUpstreamPodEgressPatternsAdmitTheirCDNsAlone(t *testing.T) {
	for name, want := range map[string]bool{
		"cdn01.quay.io": true, "cdn02.quay.io": true, "europe-west3-docker.pkg.dev": true, "us-west1-docker.pkg.dev": true,
		"evil.quay.io": false, "cdn01.quay.io.evil.example": false, "docker.pkg.dev": false, "x.europe-west3-docker.pkg.dev": false, "evil.dev": false,
	} {
		if resolves, admits := podEgressFor(t, name); resolves != want || admits != want {
			t.Errorf("%s: resolves %v, admits %v, want both %v", name, resolves, admits, want)
		}
	}
}

// The pod's whole policy, written out: an extra rule on any port, a
// reordered rule or a top-level key such as enableDefaultDeny fails it. The
// store is its public host, which in-cluster DNS answers with the edge
// gateway: the pod resolves it, reaches the gateway, entity ingress, on 443,
// and Forgejo's HTTP port, the gateway's upstream leg, which the gateway's
// Envoy holds to the pod's own policy.
const upstreamPodPolicy = `{
  "endpointSelector": {"matchLabels": {"app.kubernetes.io/name": "upstream-copy"}},
  "egress": [
    {"toEndpoints": [{"matchLabels": {"k8s:io.kubernetes.pod.namespace": "forgejo", "k8s:app.kubernetes.io/name": "forgejo"}}],
     "toPorts": [{"ports": [{"port": "3000", "protocol": "TCP"}]}]},
    {"toFQDNs": [
       {"matchName": "index.docker.io"}, {"matchName": "auth.docker.io"},
       {"matchName": "production.cloudfront.docker.com"}, {"matchName": "production.cloudflare.docker.com"},
       {"matchName": "docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com"},
       {"matchName": "ghcr.io"}, {"matchName": "pkg-containers.githubusercontent.com"},
       {"matchName": "quay.io"}, {"matchName": "registry.k8s.io"}, {"matchName": "cdn.registry.k8s.io"},
       {"matchName": "code.forgejo.org"}, {"matchName": "factory.talos.dev"}, {"matchName": "cdn.factory.talos.dev"},
       {"matchPattern": "cdn*.quay.io"}, {"matchPattern": "*-docker.pkg.dev"},
       {"matchName": "github.com"}, {"matchName": "release-assets.githubusercontent.com"},
       {"matchName": "raw.githubusercontent.com"}, {"matchName": "go.dev"}, {"matchName": "dl.google.com"},
       {"matchName": "nodejs.org"}, {"matchName": "vuln.go.dev"}, {"matchName": "www.postgresql.org"},
       {"matchName": "proxy.golang.org"}, {"matchName": "registry.npmjs.org"}],
     "toPorts": [{"ports": [{"port": "443", "protocol": "TCP"}]}]},
    {"toEntities": ["ingress"],
     "toPorts": [{"ports": [{"port": "443", "protocol": "TCP"}]}]},
    {"toEndpoints": [{"matchLabels": {"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}],
     "toPorts": [{"ports": [{"port": "53", "protocol": "ANY"}],
       "rules": {"dns": [
         {"matchName": "index.docker.io"}, {"matchName": "auth.docker.io"},
         {"matchName": "production.cloudfront.docker.com"}, {"matchName": "production.cloudflare.docker.com"},
         {"matchName": "docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com"},
         {"matchName": "ghcr.io"}, {"matchName": "pkg-containers.githubusercontent.com"},
         {"matchName": "quay.io"}, {"matchName": "registry.k8s.io"}, {"matchName": "cdn.registry.k8s.io"},
         {"matchName": "code.forgejo.org"}, {"matchName": "factory.talos.dev"}, {"matchName": "cdn.factory.talos.dev"},
         {"matchPattern": "cdn*.quay.io"}, {"matchPattern": "*-docker.pkg.dev"},
         {"matchName": "github.com"}, {"matchName": "release-assets.githubusercontent.com"},
         {"matchName": "raw.githubusercontent.com"}, {"matchName": "go.dev"}, {"matchName": "dl.google.com"},
         {"matchName": "nodejs.org"}, {"matchName": "vuln.go.dev"}, {"matchName": "www.postgresql.org"},
         {"matchName": "proxy.golang.org"}, {"matchName": "registry.npmjs.org"},
         {"matchName": "store.example"}]}}]}
  ]
}`

// requirePolicy fails unless spec is the written policy, as JSON.
func requirePolicy(t *testing.T, spec map[string]any) {
	t.Helper()
	var want, got any
	if err := json.Unmarshal([]byte(upstreamPodPolicy), &want); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the pod's policy\n%s\nis not the written one\n%s", raw, upstreamPodPolicy)
	}
}

func TestUpstreamPodPolicyIsExactlyTheWrittenOne(t *testing.T) {
	requirePolicy(t, upstreamEgressSpec(storeHost))
}

// pr.yml's changes job, which image-build needs, runs .forgejo/store-holds.sh
// over the images it selected, so a missing copy fails before a build VM is
// made; the script waits out the longest copy, since an open pull request's
// checks start at the push, beside the copy of the pushed head.
func TestTheStoreCheckRunsBeforeTheBuildVMAndWaitsOutTheLongestCopy(t *testing.T) {
	script, err := os.ReadFile("../../.forgejo/store-holds.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`STORE_WAIT:-([0-9]+)`).FindSubmatch(script)
	if m == nil {
		t.Fatal("store-holds.sh has no STORE_WAIT default")
	}
	if secs, _ := strconv.Atoi(string(m[1])); time.Duration(secs)*time.Second < upstreamDeadline+upstreamStartGrace {
		t.Errorf("the check waits %d s, less than the copy's %s", secs, upstreamDeadline+upstreamStartGrace)
	}
	data, err := os.ReadFile("../../.forgejo/workflows/pr.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs  any    `yaml:"needs"`
			RunsOn string `yaml:"runs-on"`
			Steps  []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	changes := wf.Jobs["changes"]
	var runs bool
	for _, s := range changes.Steps {
		runs = runs || strings.Contains(s.Run, "bash .forgejo/store-holds.sh ${{ steps.select.outputs.images }}")
	}
	if !runs || changes.RunsOn != "pr-untrusted" || fmt.Sprint(wf.Jobs["image-build"].Needs) != "changes" {
		t.Errorf("changes runs the check %v on %q, image-build needs %v: want the check on a runner without a VM, before image-build", runs, changes.RunsOn, wf.Jobs["image-build"].Needs)
	}
}

func copyRunner(t *testing.T, task Task, fj *prForgejo, u *UpstreamPod) *Runner {
	t.Helper()
	r, _, _ := newActivityRunner(t, task, fj)
	r.Upstream = u
	return r
}

// rawAt adds files to raw as prForgejo serves them at repoAt.
func rawAt(raw map[string]string, repoAt string, files map[string][]byte) map[string]string {
	for p, b := range files {
		raw[repoAt+":"+p] = string(b)
	}
	return raw
}

func copyFindings(t *testing.T, r *Runner) []string {
	t.Helper()
	val, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	if err := val.Get(&findings); err != nil {
		t.Fatal(err)
	}
	return findings
}

// The activity reads the head's files on the fork, the base's on the
// repository and the two scripts on main: the pod gets the head's files
// with main's scripts, never the head's own.
func TestCopyUpstreamImagesActivityHandsThePodTheHeadsFilesAndMainsScripts(t *testing.T) {
	u := newUpstreamPod(t)
	var secret map[string]string
	podKube(u).run = func(s map[string]string) (string, int32) { secret = s; return "", 0 }
	head := headFiles()
	head["upstream-files.txt"] = []byte(nodeLine + "\nvulndb 2 vulndb.zip https://vuln.go.dev/vulndb.zip 2222222222222222222222222222222222222222222222222222222222222222\n")
	raw := rawAt(rawAt(map[string]string{}, "sf/repo@base", headFiles()), "factory-agent/repo@c1", head)
	raw["factory-agent/repo@c1:scripts/upstream-copy"] = "the head's own upstream-copy\n"
	raw["factory-agent/repo@c1:scripts/upstream-files"] = "the head's own upstream-files\n"
	raw["sf/repo@main:scripts/upstream-copy"] = "main's upstream-copy\n"
	raw["sf/repo@main:scripts/upstream-files"] = "main's upstream-files\n"
	r := copyRunner(t, prTask, &prForgejo{raw: raw}, u)
	if findings := copyFindings(t, r); len(findings) != 0 {
		t.Errorf("findings %q, want none", findings)
	}
	if secret == nil {
		t.Fatal("no pod ran")
	}
	for key, want := range map[string]string{
		"upstream-copy": "main's upstream-copy\n", "upstream-files": "main's upstream-files\n",
		"upstream-files.txt": string(head["upstream-files.txt"]), "upstream-images.txt": goLine + "\n",
		"package-lock.json": `{"lockfileVersion":3}` + "\n",
	} {
		if secret[key] != want {
			t.Errorf("the pod's %s = %q, want %q", key, secret[key], want)
		}
	}
}

func TestCopyUpstreamImagesActivityTakesMainsHeadForAnEmptyBase(t *testing.T) {
	u := newUpstreamPod(t)
	task := prTask
	task.SHA = ""
	raw := rawAt(rawAt(map[string]string{}, "sf/repo@m9", headFiles()), "factory-agent/repo@c1", headFiles())
	r := copyRunner(t, task, &prForgejo{head: "m9", raw: raw}, u)
	if findings := copyFindings(t, r); len(findings) != 0 {
		t.Errorf("findings %q, want none", findings)
	}
	if got := podKube(u).created; len(got) != 0 {
		t.Errorf("main's own files ran a pod: %v", got)
	}
}

// A head whose files equal its base's, or without upstream-images.txt
// (another project's repository, or a head that drops the list), runs no
// pod and asks no login.
func TestCopyUpstreamImagesActivityUnchangedOrAbsentListRunsNoPod(t *testing.T) {
	for name, raw := range map[string]map[string]string{
		"equal":   rawAt(rawAt(map[string]string{}, "sf/repo@base", headFiles()), "factory-agent/repo@c1", headFiles()),
		"no file": {},
		"removed": rawAt(map[string]string{}, "sf/repo@base", headFiles()),
	} {
		t.Run(name, func(t *testing.T) {
			u := newUpstreamPod(t)
			u.OpenBao = &fakeKV{err: fmt.Errorf("no login wanted")}
			r := copyRunner(t, prTask, &prForgejo{raw: raw}, u)
			if findings := copyFindings(t, r); len(findings) != 0 {
				t.Errorf("findings %q, want none", findings)
			}
			if got := podKube(u).created; len(got) != 0 {
				t.Errorf("a pod for nothing new: %v", got)
			}
		})
	}
}

// A head that holds the list but lacks another file the scripts read gets
// a finding naming it, and no pod runs.
func TestCopyUpstreamImagesActivityNamesAFileTheHeadLacks(t *testing.T) {
	u := newUpstreamPod(t)
	head := headFiles()
	delete(head, "go.sum")
	delete(head, "opencode-plugin/package-lock.json")
	raw := rawAt(rawAt(map[string]string{}, "sf/repo@base", headFiles()), "factory-agent/repo@c1", head)
	r := copyRunner(t, prTask, &prForgejo{raw: raw}, u)
	want := []string{"the head holds no go.sum, opencode-plugin/package-lock.json, which the copy reads: nothing was copied"}
	if findings := copyFindings(t, r); !slices.Equal(findings, want) {
		t.Errorf("findings %q, want %q", findings, want)
	}
	if got := podKube(u).created; len(got) != 0 {
		t.Errorf("a pod ran: %v", got)
	}
}

func TestCopyUpstreamImagesActivityWithoutCopierDoesNothing(t *testing.T) {
	r := copyRunner(t, prTask, &prForgejo{}, nil)
	if findings := copyFindings(t, r); len(findings) != 0 {
		t.Fatalf("findings %q", findings)
	}
}

func TestCopyUpstreamImagesActivityErrorIsScrubbed(t *testing.T) {
	u := newUpstreamPod(t)
	u.OpenBao = &fakeKV{err: fmt.Errorf("get https://user:pass@bao.example/v1/x: Authorization: Bearer abcdefgh12345678")}
	head := headFiles()
	head["go.sum"] = []byte("example.com/y v1.0.0 h1:y=\n")
	raw := rawAt(rawAt(rawAt(map[string]string{}, "sf/repo@base", headFiles()), "factory-agent/repo@c1", head), "sf/repo@main", copyFiles())
	r := copyRunner(t, prTask, &prForgejo{raw: raw}, u)
	_, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1")
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{"user:pass", "abcdefgh12345678"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q", err, leak)
		}
	}
}

func TestCopyUpstreamImagesActivityReadsAnOpenPullRequestsHeadRepository(t *testing.T) {
	u := newUpstreamPod(t)
	var secret map[string]string
	podKube(u).run = func(s map[string]string) (string, int32) { secret = s; return "", 0 }
	head := headFiles()
	head["upstream-images.txt"] = []byte(goLine + "\n# the fork's line\n")
	raw := rawAt(rawAt(rawAt(map[string]string{}, "sf/repo@base", headFiles()), "someone/fork@c1", head), "sf/repo@main", copyFiles())
	fj := &prForgejo{
		pulls: map[int64]string{12: `{"number":12,"state":"open","html_url":"https://git.example/sf/repo/pulls/12","head":{"sha":"c1","ref":"factory/schema-abc","repo":{"full_name":"someone/fork"}},"base":{"ref_name":"main"}}`},
		raw:   raw,
	}
	task := prTask
	task.PRNumber = 12
	r := copyRunner(t, task, fj, u)
	copyFindings(t, r)
	if secret["upstream-images.txt"] != goLine+"\n# the fork's line\n" {
		t.Errorf("the pod's list = %q, want the pull request's head repository's", secret["upstream-images.txt"])
	}
}
