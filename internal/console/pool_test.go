package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// noDraws is the pool's words of a project with no draws this month under
// the default pool.
const noDraws = "0 of 30 sessions, 0 h of 30 h agent time and €0.00 of €60.00 drawn this UTC month"

// defaultPoolRead is the settings and policy reads' pool line over the
// fixtures' loaded policy for a project with no draws: the identity and
// this UTC month with the default pool's three parts.
func defaultPoolRead() string {
	return "Pool (policy version 4 (commit c9d98bb), " + time.Now().UTC().Format("2006-01") + " UTC): " + noDraws
}

// poolRig is a rig whose project ai-software-factory-code sets pool as its
// escalation block's pool (nil for none) and whose cards drew drawn this
// month, or a draw read that fails with fail.
func poolRig(t *testing.T, pool *policy.Draw, drawn policy.Draw, fail error) *rig {
	t.Helper()
	r := newRig(t)
	pc := policy.ProjectConfig{Repository: "sf/code", MergeRule: "human", Objects: "factory", Agents: "coder", Process: "maintenance"}
	if pool != nil {
		pc.Escalation = &policy.Escalation{Pool: *pool}
	}
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": pc}
	r.drawn, r.drawnErr = drawn, fail
	return r
}

// poolFace is the project's face as every reader reads it: the status
// line's pool, the board's text and HTML, and the chat's status line,
// one projection through them all.
func poolFace(t *testing.T, r *rig) (pool poolLine, board, html, snap string) {
	t.Helper()
	ck, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/board?project=ai-software-factory-code", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	d, err := r.c.boardOf(req, boardFilter{Project: "ai-software-factory-code", View: viewWide}, actor{})
	if err != nil {
		t.Fatalf("the board of ai-software-factory-code: %v", err)
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/board?project=ai-software-factory-code", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /board = %d: %s", rec.Code, rec.Body)
	}
	snap, err = (&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent"},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}).snapshot()
	if err != nil {
		t.Fatalf("the project's status line: %v", err)
	}
	// the template's escaping alone differs; the figures are the same words
	return d.Status.Pool, boardWords(d), strings.ReplaceAll(rec.Body.String(), "&#39;", "'"), snap
}

// The project face's pool is one projection the board's HTML, the board's
// text and the chat's status line read: the three parts drawn against the
// pool the tree sets, the default's where the file sets none and the
// file's own money with the default's other parts, overdrawn naming each
// part past its pool, and an unreadable draw never zero.
func TestProjectPoolStatus(t *testing.T) {
	for _, tc := range []struct {
		name              string
		pool              *policy.Draw
		drawn             policy.Draw
		fail              error
		present, readable bool
		overdrawn         bool
		want              string
	}{
		{name: "the default", drawn: policy.Draw{Sessions: 3, AgentTime: 90 * time.Minute, EUR: 4.5}, readable: true,
			want: "3 of 30 sessions, 1 h 30 min of 30 h agent time and €4.50 of €60.00 drawn this UTC month"},
		{name: "its own money", pool: &policy.Draw{EUR: 10}, drawn: policy.Draw{EUR: 3}, present: true, readable: true,
			want: "0 of 30 sessions, 0 h of 30 h agent time and €3.00 of €10.00 drawn this UTC month"},
		{name: "overdrawn", pool: &policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 2}, drawn: policy.Draw{Sessions: 3, AgentTime: 61 * time.Minute, EUR: 2},
			present: true, readable: true, overdrawn: true,
			want: "3 of 2 sessions, 1 h 1 min of 1 h agent time and €2.00 of €2.00 drawn this UTC month, overdrawn in sessions and agent time: only the human draws more until the month turns"},
		{name: "unreadable draws", pool: &policy.Draw{EUR: 10}, fail: errors.New("the store is down"), present: true,
			want: "the pool is 30 sessions, 30 h of agent time and €10.00 this UTC month, but what its cards drew could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, board, html, snap := poolFace(t, poolRig(t, tc.pool, tc.drawn, tc.fail))
			if pool.Present != tc.present || pool.Readable != tc.readable || pool.Overdrawn != tc.overdrawn || pool.Words != tc.want {
				t.Errorf("the pool = present %v, readable %v, overdrawn %v, %q; want %v, %v, %v, %q",
					pool.Present, pool.Readable, pool.Overdrawn, pool.Words, tc.present, tc.readable, tc.overdrawn, tc.want)
			}
			if len(pool.Month) != 7 || !strings.Contains(pool.Policy, "policy version") {
				t.Errorf("the pool's month %q or policy identity %q is blank", pool.Month, pool.Policy)
			}
			for name, got := range map[string]string{"the board's text": board, "the board's HTML": html, "the chat's status line": snap} {
				if !strings.Contains(got, tc.want) {
					t.Errorf("%s lacks the pool's words %q:\n%s", name, tc.want, got)
				}
			}
		})
	}
}

// poolTool runs the settings or policy page read as the rig's member and
// answers what the agent reads.
func poolTool(t *testing.T, r *rig, name string) string {
	t.Helper()
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, _ := r.c.chatTool(name)
	out, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, toolInput{})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// The settings and policy read-outs carry the project face's pool: 3
// sessions and EUR 4 drawn of a pool of EUR 10, identically in the
// settings and policy HTML and in the settings and policy chat reads,
// with the loaded policy's identity; an unreadable draw stays its own row.
func TestSettingsPolicyPool(t *testing.T) {
	r := poolRig(t, &policy.Draw{EUR: 10}, policy.Draw{Sessions: 3, EUR: 4}, nil)
	ck, _ := r.loggedIn(t)
	settings := r.do(httptest.NewRequest(http.MethodGet, "/settings?project=ai-software-factory-code", nil), ck).Body.String()
	policyPage := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck).Body.String()
	chatSettings, chatPolicy := poolTool(t, r, "settings"), poolTool(t, r, "policy")
	const figures = "3 of 30 sessions, 0 h of 30 h agent time and €4.00 of €10.00 drawn this UTC month"
	id := func(page, name string) string {
		t.Helper()
		m := poolIdentity.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("%s carries no pool identity:\n%s", name, page)
		}
		return m[0]
	}
	identities := map[string]string{
		"the settings page": id(settings, "the settings page"),
		"the policy page":   id(policyPage, "the policy page"),
		"the settings read": id(chatSettings, "the settings read"),
		"the policy read":   id(chatPolicy, "the policy read"),
	}
	for name, want := range identities {
		for other, got := range identities {
			if want != got {
				t.Errorf("%s's loaded identity %q differs from %s's %q", name, want, other, got)
			}
		}
	}
	for name, page := range map[string]string{
		"the settings page": settings, "the policy page": policyPage,
		"the settings read": chatSettings, "the policy read": chatPolicy,
	} {
		if !strings.Contains(page, figures) {
			t.Errorf("%s lacks the pool's figures %q:\n%s", name, figures, page)
		}
		if !strings.Contains(page, "policy version 4 (commit c9d98bb)") {
			t.Errorf("%s lacks the loaded policy's identity:\n%s", name, page)
		}
	}
	if !strings.Contains(chatSettings, "Pool (policy version 4 (commit c9d98bb), ") {
		t.Errorf("the settings read lacks the pool's identity line:\n%s", chatSettings)
	}

	unreadable := poolRig(t, &policy.Draw{EUR: 10}, policy.Draw{}, errors.New("the store is down"))
	uck, _ := unreadable.loggedIn(t)
	rr := unreadable.do(httptest.NewRequest(http.MethodGet, "/settings?project=ai-software-factory-code", nil), uck).Body.String()
	if !strings.Contains(rr, "but what its cards drew could not be read") || strings.Contains(rr, "drawn this UTC month") {
		t.Errorf("the unreadable settings row reads as a draw:\n%s", rr)
	}
}

// poolIdentity matches the pool line's loaded-policy identity wherever a
// read-out shows it.
var poolIdentity = regexp.MustCompile(`policy version \d+ \(commit [0-9a-f]{7,}\)`)

// The settings page stays read-only on the pool: it links the loaded
// version's project file, offers no pool input or write, and a read
// never moves the policy in force.
func TestSettingsPoolVersion(t *testing.T) {
	r := poolRig(t, &policy.Draw{EUR: 10}, policy.Draw{EUR: 3}, nil)
	ck, _ := r.loggedIn(t)
	page := r.do(httptest.NewRequest(http.MethodGet, "/settings?project=ai-software-factory-code", nil), ck).Body.String()
	const link = `href="https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory/projects/ai-software-factory-code.yaml"`
	if !strings.Contains(page, link) {
		t.Errorf("the settings page does not link the loaded version's project file:\n%s", page)
	}
	for _, editable := range []string{`name="escalation`, `name="pool`, "top-up", "Top up"} {
		if strings.Contains(page, editable) {
			t.Errorf("the settings page offers an editable pool (%s):\n%s", editable, page)
		}
	}
	for _, rt := range r.c.postRoutes() {
		if strings.Contains(rt.pattern, "settings") || strings.Contains(rt.pattern, "pool") {
			t.Errorf("%s would write from the settings page", rt.pattern)
		}
	}
	v, ok := r.c.inForce(context.Background())
	if !ok || v.ID != 4 || factoryd.Cents(v.Projects["ai-software-factory-code"].Escalation.Pool.EUR) != 1000 {
		t.Fatalf("the policy in force moved: %v, %v", ok, v.ID)
	}
	if rec := r.do(httptest.NewRequest(http.MethodPost, "/settings", nil), ck); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /settings = %d, want 405", rec.Code)
	}
	poolTool(t, r, "settings")
	poolTool(t, r, "policy")
	after, ok := r.c.inForce(context.Background())
	if !ok || after.ID != v.ID || after.Commit != v.Commit ||
		factoryd.Cents(after.Projects["ai-software-factory-code"].Escalation.Pool.EUR) != 1000 {
		t.Errorf("a pool read-out mutated the policy in force: version %d (%s)", after.ID, after.Commit)
	}
}

// poolHead is demo-app's project file above the pool's money; poolFile
// is the file with its escalation pool of pool EUR.
const poolHead = "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nautonomy: assisted\nbudget:\n" +
	"  month_eur: 10\n  task_max_eur: 4\nescalation:\n  # the pool every rung draws from\n  pool:\n    sessions: 10\n"

func poolFile(pool string) string { return poolHead + "    eur: " + pool + "\n" }

// poolLoaded is demo-app's loaded project with an escalation pool of eur.
func poolLoaded(eur float64) policy.ProjectConfig {
	return policy.ProjectConfig{Repository: "demo-app/demo-app", Budget: &policy.ProjectBudget{MonthEUR: 10},
		Escalation: &policy.Escalation{Pool: policy.Draw{Sessions: 10, EUR: eur}}}
}

// poolWritesRig is the writes rig whose infra's main holds demo-app's
// file with an escalation pool of pool EUR, and whose check loads every
// candidate. The pool in force stays what fakeLedger's built-in holds
// until a test seats the policy of its own.
func poolWritesRig(t *testing.T, pool string) (*rig, *infraFake) {
	t.Helper()
	r, _ := newWritesRig(t)
	infra := withInfra(t, r)
	infra.files = map[string]string{"factory/projects/demo-app.yaml": poolFile(pool)}
	r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) { return nil, nil }
	return r, infra
}

// TestPoolPolicyWrite covers the landed write on the writes rig: the set
// replaces only escalation.pool.eur of the project file, keeping the
// file's comments, the pool's other parts and every other line; a top-up
// corrected to the file as it now is grows the file's own money; a file
// with no escalation block gets one, its pool and its money; each change
// is one commit, one pull request, one merge, one reload after it, and
// the check sees the candidate. Every refusal writes nothing: a malformed
// amount (400), a candidate the loader refuses (422), a project the
// policy does not hold, a file the pool cannot read, a top-up of money
// the file does not set, a stale source and a change of nothing (409).
func TestPoolPolicyWrite(t *testing.T) {
	r, infra := poolWritesRig(t, "10")
	next := 12.0
	reloads := 0
	r.c.cfg.PolicyReload = func(context.Context) error {
		reloads++
		r.ledger.files["demo-app"] = poolLoaded(next)
		return nil
	}
	var checked []string
	r.c.cfg.CheckChange = func(_ context.Context, changes map[string][]byte) ([]string, error) {
		for path, file := range changes {
			checked = append(checked, path+": "+string(file))
		}
		return nil, nil
	}
	_, s := r.loggedIn(t)
	a := actor{s: s, via: factoryd.ViaChat}

	res, err := r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 12,
		reason: "the load test needs a bigger pool", source: "blob1"}, a)
	if err != nil {
		t.Fatalf("doSetPool(set 12) = %v", err)
	}
	equal(t, "the set", res, poolResult{from: 10, confirmed: 12, to: 12, pr: 12, loaded: true})
	if len(infra.contents) != 1 || len(infra.pulls) != 1 || len(infra.merges) != 1 {
		t.Fatalf("the set wrote %d commits, %d pull requests, %d merges", len(infra.contents), len(infra.pulls), len(infra.merges))
	}
	body, branch := infra.contents[0].body, infra.contents[0].body["new_branch"].(string)
	change := body["files"].([]any)[0].(map[string]any)
	content, _ := base64.StdEncoding.DecodeString(change["content"].(string))
	landed := poolFile("12")
	if body["branch"] != "main" || !strings.HasPrefix(branch, "config-demo-app-") ||
		body["message"] != "Project demo-app: escalation pool 12.00 EUR\n\nFactory-Role: chat\nRequested-by: sebastian\n" ||
		change["operation"] != "update" || change["path"] != "factory/projects/demo-app.yaml" || change["sha"] != "blob1" || string(content) != landed {
		t.Fatalf("the commit = %+v (%q)", body, content)
	}
	if infra.pulls[0].body["head"] != branch || infra.pulls[0].body["base"] != "main" ||
		infra.pulls[0].body["title"] != "Project demo-app: escalation pool 12.00 EUR" {
		t.Fatalf("pulls = %+v", infra.pulls)
	}
	equal(t, "the reload", reloads, 1)
	equal(t, "the check", checked, []string{"projects/demo-app.yaml: " + landed})

	// The file changed since the proposal read it: the change refuses
	// with the blob the file now is, before any edit is judged, and
	// nothing is written.
	infra.mu.Lock()
	infra.files["factory/projects/demo-app.yaml"] = poolFile("8")
	infra.shas = map[string]string{"factory/projects/demo-app.yaml": "blob2"}
	infra.mu.Unlock()
	var no *refusal
	if _, err := r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 12,
		reason: "proposed against the old file", source: "blob1"}, a); !errors.As(err, &no) ||
		no.status != http.StatusConflict || no.msg != "projects/demo-app.yaml changed since the change was proposed (its blob is now blob2): read it again and propose the change anew" {
		t.Fatalf("the stale source = %v", err)
	}
	equal(t, "writes after the refusal", []int{len(infra.contents), len(infra.pulls), len(infra.merges)}, []int{1, 1, 1})

	// A top-up corrected to the file as it now is grows the file's own
	// pool.
	res, err = r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolTopUp, eur: 4,
		reason: "corrected: the pool grows by the load test's need", source: "blob2"}, a)
	if err != nil {
		t.Fatalf("doSetPool(top up 4) = %v", err)
	}
	equal(t, "the top-up", res, poolResult{from: 8, confirmed: 4, to: 12, pr: 12, loaded: true})
	change = infra.contents[1].body["files"].([]any)[0].(map[string]any)
	content, _ = base64.StdEncoding.DecodeString(change["content"].(string))
	if infra.contents[1].body["message"] != "Project demo-app: escalation pool +4.00 EUR\n\nFactory-Role: chat\nRequested-by: sebastian\n" ||
		string(content) != poolFile("12") || change["sha"] != "blob2" {
		t.Fatalf("the top-up's commit = %+v (%q)", infra.contents[1].body, content)
	}
	equal(t, "two changes, two of each", []int{len(infra.contents), len(infra.pulls), len(infra.merges)}, []int{2, 2, 2})
	equal(t, "the reload after the top-up", reloads, 2)

	// A file with no escalation block gets one, with its pool and its
	// money, and nothing else moves.
	infra.mu.Lock()
	infra.files["factory/projects/demo-app.yaml"] = "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nautonomy: assisted\nbudget:\n  month_eur: 0\n"
	infra.shas["factory/projects/demo-app.yaml"] = "blob3"
	infra.mu.Unlock()
	next = 5
	res, err = r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 5,
		reason: "the project sets its own pool", source: "blob3"}, a)
	if err != nil {
		t.Fatalf("doSetPool(set 5 onto no pool) = %v", err)
	}
	equal(t, "the first pool", res, poolResult{from: 0, confirmed: 5, to: 5, pr: 12, loaded: true})
	equal(t, "the file with its first pool", committed(t, infra, 2),
		"# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nautonomy: assisted\nbudget:\n  month_eur: 0\nescalation:\n  pool:\n    eur: 5\n")

	// A two-place amount lands as its exact decimal, at a size a float's
	// own addition could not carry: the file holds the cents the member
	// confirmed, never a float's rendering.
	infra.mu.Lock()
	infra.files["factory/projects/demo-app.yaml"] = poolFile("0")
	infra.shas["factory/projects/demo-app.yaml"] = "blob4"
	infra.mu.Unlock()
	next = 123456789012.34
	res, err = r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 123456789012.34,
		reason: "an exact two-place amount", source: "blob4"}, a)
	if err != nil {
		t.Fatalf("doSetPool(set 123456789012.34) = %v", err)
	}
	equal(t, "the exact set", res, poolResult{from: 0, confirmed: 123456789012.34, to: 123456789012.34, pr: 12, loaded: true})
	change = infra.contents[3].body["files"].([]any)[0].(map[string]any)
	content, _ = base64.StdEncoding.DecodeString(change["content"].(string))
	equal(t, "the file with its exact pool", string(content), poolFile("123456789012.34"))

	// An anchor nothing aliases changes nothing but its own value: the
	// pool lands with the anchor kept.
	anchored := "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nautonomy: assisted\nescalation:\n  pool:\n    eur: &eur 10\n"
	infra.mu.Lock()
	infra.files["factory/projects/demo-app.yaml"] = anchored
	infra.shas["factory/projects/demo-app.yaml"] = "blob5"
	infra.mu.Unlock()
	next = 6
	res, err = r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 6,
		reason: "an anchored pool no alias shares", source: "blob5"}, a)
	if err != nil {
		t.Fatalf("doSetPool(set 6 onto an anchored pool) = %v", err)
	}
	equal(t, "the anchored set", res, poolResult{from: 10, confirmed: 6, to: 6, pr: 12, loaded: true})
	change = infra.contents[4].body["files"].([]any)[0].(map[string]any)
	content, _ = base64.StdEncoding.DecodeString(change["content"].(string))
	equal(t, "the file with its anchored pool", string(content), strings.Replace(anchored, "&eur 10", "&eur 6", 1))

	// The refusals write nothing: each seats the file the change meets
	// and answers its refusal before any commit, pull request, merge,
	// reload or row.
	why := poolChange{action: poolSet, eur: 5, reason: "why the pool moves", source: "blob1"}
	aliased := "projects/demo-app.yaml aliases its escalation block or its pool: a policy PR removes the YAML alias before a pool change"
	for _, row := range []struct {
		name    string
		project string
		file    string
		sha     string
		in      poolChange
		check   string
		status  int
		answer  string
	}{
		{"an unknown action", "demo-app", poolFile("10"), "blob1", poolChange{action: "double", eur: 5, reason: "why the pool moves", source: "blob1"}, "",
			400, "the pool's action is set or top_up"},
		{"a change without its reason", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: 5, source: "blob1"}, "",
			400, "a pool change names its reason"},
		{"a change without its source", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: 5, reason: "why the pool moves"}, "",
			400, "a pool change names the source of the project file it read"},
		{"a set of zero", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: 0, reason: "why the pool moves", source: "blob1"}, "",
			400, "a set takes an amount above 0 EUR: a pool of 0 takes the policy's figure"},
		{"a top-up of nothing", "demo-app", poolFile("10"), "blob1", poolChange{action: poolTopUp, eur: 0, reason: "why the pool moves", source: "blob1"}, "",
			400, "a top-up takes a positive amount in EUR"},
		{"an amount that is no number", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: math.Inf(1), reason: "why the pool moves", source: "blob1"}, "",
			400, "a pool change's amount is a finite number in EUR"},
		{"an amount too large", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: 1e18, reason: "why the pool moves", source: "blob1"}, "",
			400, "a pool change's amount is too large"},
		{"a project the policy does not hold", "elsewhere", poolFile("10"), "blob1", why, "",
			409, "the policy in force holds no project elsewhere"},
		{"a top-up of money the file does not set", "demo-app", "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nescalation:\n  pool:\n    sessions: 10\n", "blob1",
			poolChange{action: poolTopUp, eur: 5, reason: "why the pool moves", source: "blob1"}, "",
			409, "projects/demo-app.yaml sets no escalation.pool.eur, so the pool's money is the policy's: set the pool instead of a top-up"},
		{"an escalation block that is no mapping", "demo-app", "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nescalation: always\n", "blob1", why, "",
			409, "projects/demo-app.yaml holds an escalation block the pool cannot read: a policy PR corrects it"},
		{"a pool the file cannot hold", "demo-app", poolFile("1000000000000000000"), "blob1", why, "",
			409, "projects/demo-app.yaml holds an escalation.pool.eur the pool cannot take: a policy PR corrects it"},
		{"a stale source", "demo-app", poolFile("8"), "blob2", why, "",
			409, "projects/demo-app.yaml changed since the change was proposed (its blob is now blob2): read it again and propose the change anew"},
		{"a change of nothing", "demo-app", poolFile("5"), "blob1", poolChange{action: poolSet, eur: 5, reason: "why the pool moves", source: "blob1"}, "",
			409, "Nothing changes: projects/demo-app.yaml already holds this."},
		{"a sub-cent amount", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: 0.004, reason: "why the pool moves", source: "blob1"}, "",
			400, "a pool change's amount is whole cents"},
		{"a sub-cent top-up", "demo-app", poolFile("10"), "blob1", poolChange{action: poolTopUp, eur: 0.009, reason: "why the pool moves", source: "blob1"}, "",
			400, "a pool change's amount is whole cents"},
		{"an amount over the pool's bound", "demo-app", poolFile("10"), "blob1", poolChange{action: poolSet, eur: 900000000001, reason: "why the pool moves", source: "blob1"}, "",
			400, "a pool change's amount is too large"},
		{"a file pool over the bound", "demo-app", poolFile("2000000000000"), "blob1", why, "",
			409, "projects/demo-app.yaml holds an escalation.pool.eur the pool cannot take: a policy PR corrects it"},
		{"a file pool in sub-cents", "demo-app", poolFile("10.005"), "blob1", why, "",
			409, "projects/demo-app.yaml holds an escalation.pool.eur the pool cannot take: a policy PR corrects it"},
		{"an escalation block shared through a YAML alias", "demo-app", "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\ndefaults: &d\n  pool:\n    eur: 10\nescalation: *d\n", "blob1", why, "",
			409, aliased},
		{"a pool shared through a YAML alias", "demo-app", "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nescalation:\n  pool: &p\n    eur: 10\n  card_draw: *p\n", "blob1", why, "",
			409, aliased},
		{"money shared through a YAML alias", "demo-app", "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nescalation:\n  pool:\n    eur: &eur 10\n  card_draw:\n    eur: *eur\n", "blob1", why, "",
			409, aliased},
		{"a top-up over the pool's bound", "demo-app", poolFile("899999999999.99"), "blob1",
			poolChange{action: poolTopUp, eur: 0.02, reason: "why the pool moves", source: "blob1"}, "",
			409, "a top-up over the pool of projects/demo-app.yaml does not fit: propose a set of the target instead"},
		{"a candidate the loader refuses", "demo-app", poolFile("10"), "blob1", why,
			"projects/demo-app.yaml: escalation: card_draw.eur 4 is above pool.eur 3",
			422, "The project file would not load: projects/demo-app.yaml: escalation: card_draw.eur 4 is above pool.eur 3"},
	} {
		t.Run(row.name, func(t *testing.T) {
			infra.mu.Lock()
			infra.files = map[string]string{"factory/projects/demo-app.yaml": row.file}
			infra.shas = map[string]string{"factory/projects/demo-app.yaml": row.sha}
			infra.contents, infra.pulls, infra.merges = nil, nil, nil
			infra.mu.Unlock()
			reloads, rows := 0, r.events.count()
			r.c.cfg.PolicyReload = func(context.Context) error { reloads++; return nil }
			r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) {
				if row.check != "" {
					return []string{row.check}, nil
				}
				return nil, nil
			}
			var no *refusal
			if _, err := r.c.doSetPool(context.Background(), row.project, row.in, actor{s: s, via: factoryd.ViaChat}); !errors.As(err, &no) ||
				no.status != row.status || no.msg != row.answer {
				t.Fatalf("doSetPool(%s) = %v, want %d %q", row.name, err, row.status, row.answer)
			}
			infra.mu.Lock()
			writes := len(infra.contents) + len(infra.pulls) + len(infra.merges)
			infra.mu.Unlock()
			if writes != 0 || reloads != 0 || r.events.count() != rows {
				t.Fatalf("%s wrote %d, reloaded %d and recorded %d rows", row.name, writes, reloads, r.events.count()-rows)
			}
		})
	}
}

// TestPoolChangeAudit covers the pool_changed row: exactly one row per
// change, naming the member who confirmed it as the actor — a change
// carries no actor of its own, so none a model names is ever taken — the
// action, the pool in force and after it, the amount the member
// confirmed, the reason, and the pull request namespaced by the
// configuration repository, at the store's own time.
func TestPoolChangeAudit(t *testing.T) {
	r, infra := poolWritesRig(t, "10")
	next := 12.0
	r.c.cfg.PolicyReload = func(context.Context) error {
		r.ledger.files["demo-app"] = poolLoaded(next)
		return nil
	}
	_, s := r.loggedIn(t)
	a := actor{s: s, via: factoryd.ViaChat}
	before := r.events.count()

	res, err := r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 12,
		reason: "the load test needs a bigger pool", source: "blob1"}, a)
	if err != nil {
		t.Fatalf("doSetPool(set 12) = %v", err)
	}
	equal(t, "the set's answer", res, poolResult{from: 10, confirmed: 12, to: 12, pr: 12, loaded: true})

	infra.mu.Lock()
	infra.files["factory/projects/demo-app.yaml"] = poolFile("8")
	infra.shas = map[string]string{"factory/projects/demo-app.yaml": "blob2"}
	infra.mu.Unlock()
	if _, err := r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolTopUp, eur: 4,
		reason: "corrected: the pool grows by the load test's need", source: "blob2"}, a); err != nil {
		t.Fatalf("doSetPool(top up 4) = %v", err)
	}

	rows := r.events.all()[before:]
	equal(t, "the rows", len(rows), 2)
	equal(t, "the set's row", rows[0], factoryd.Event{ID: rows[0].ID, At: rows[0].At, Repository: "demo-app/demo-app",
		Kind: factoryd.EventPoolChanged, Actor: "sebastian", Via: factoryd.ViaChat,
		Detail: map[string]any{"action": "set", "from_eur": 10.0, "confirmed_eur": 12.0, "to_eur": 12.0,
			"reason": "the load test needs a bigger pool", "pr": "sf/infra#12"}})
	equal(t, "the top-up's row", rows[1], factoryd.Event{ID: rows[1].ID, At: rows[1].At, Repository: "demo-app/demo-app",
		Kind: factoryd.EventPoolChanged, Actor: "sebastian", Via: factoryd.ViaChat,
		Detail: map[string]any{"action": "top_up", "from_eur": 8.0, "confirmed_eur": 4.0, "to_eur": 12.0,
			"reason": "corrected: the pool grows by the load test's need", "pr": "sf/infra#12"}})
	if rows[0].At.IsZero() || rows[1].At.IsZero() {
		t.Errorf("the rows carry no time of their own")
	}
}

// TestPoolPolicyFailure covers the partial failures, one row each: a
// merge Forgejo refuses leaves the pull request open and nothing
// recorded; a reload whose acknowledgement is not activation — the
// loader answers late, the policy in force still holding the old pool,
// or it fails — answers that the ceiling is not yet in force, its row
// written and the merged pull request named, never active while
// unloaded; a pool_changed row the store refuses after the merge is the
// one partial failure that names the already-merged pull request and the
// missing row to write by hand, and the change is never repeated.
func TestPoolPolicyFailure(t *testing.T) {
	const unloaded = "The pool change is merged as sf/infra#12, but the policy in force still holds the pool of demo-app at EUR 10.00: " +
		"the loader picks the merge up on its next poll. The pool_changed row is written, and the pool stays EUR 10.00 until the policy holds EUR 12.00."
	for _, row := range []struct {
		name    string
		seat    func(*testing.T) (*rig, *infraFake, *session)
		forgejo string
		status  int
		answer  string
		res     poolResult
		rows    int
	}{
		{"a merge Forgejo refuses", func(t *testing.T) (*rig, *infraFake, *session) {
			r, infra := poolWritesRig(t, "10")
			infra.mu.Lock()
			infra.mergeStatus, infra.mergeBody = http.StatusConflict, `{"message":"the branch is protected"}`
			infra.mu.Unlock()
			_, s := r.loggedIn(t)
			return r, infra, s
		}, "Forgejo refused to merge the pull request #12: forgejo POST /api/v1/repos/sf/infra/pulls/12/merge: status 409: {\"message\":\"the branch is protected\"}",
			0, "", poolResult{}, 0},
		{"a reload that answers while the policy in force holds the old pool", func(t *testing.T) (*rig, *infraFake, *session) {
			r, infra := poolWritesRig(t, "10")
			r.ledger.files["demo-app"] = poolLoaded(10)
			r.c.cfg.PolicyReload = func(context.Context) error { return nil }
			_, s := r.loggedIn(t)
			return r, infra, s
		}, "", http.StatusServiceUnavailable, unloaded, poolResult{from: 10, confirmed: 12, to: 12, pr: 12}, 1},
		{"a reload that fails", func(t *testing.T) (*rig, *infraFake, *session) {
			r, infra := poolWritesRig(t, "10")
			r.ledger.files["demo-app"] = poolLoaded(10)
			r.c.cfg.PolicyReload = func(context.Context) error { return errors.New("the loader is down") }
			_, s := r.loggedIn(t)
			return r, infra, s
		}, "", http.StatusServiceUnavailable, unloaded, poolResult{from: 10, confirmed: 12, to: 12, pr: 12}, 1},
		{"a pool_changed row the store refuses", func(t *testing.T) (*rig, *infraFake, *session) {
			r, infra := poolWritesRig(t, "10")
			next := 12.0
			r.c.cfg.PolicyReload = func(context.Context) error {
				r.ledger.files["demo-app"] = poolLoaded(next)
				return nil
			}
			r.events.fail = errors.New("the events store is down")
			_, s := r.loggedIn(t)
			return r, infra, s
		}, "", http.StatusInternalServerError,
			"The pool change is merged as sf/infra#12, but its pool_changed row could not be written: the events store is down. " +
				"The change stands: write the missing row (action set, from_eur 10.00, confirmed_eur 12.00, to_eur 12.00, reason \"the load test needs a bigger pool\") and do not repeat the change.",
			poolResult{from: 10, confirmed: 12, to: 12, pr: 12, loaded: true}, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, infra, s := row.seat(t)
			before := r.events.count()
			res, err := r.c.doSetPool(context.Background(), "demo-app",
				poolChange{action: poolSet, eur: 12, reason: "the load test needs a bigger pool", source: "blob1"},
				actor{s: s, via: factoryd.ViaChat})
			if row.forgejo != "" {
				var fj *forgejoRefusal
				if !errors.As(err, &fj) || fj.Error() != row.forgejo {
					t.Fatalf("doSetPool(%s) = %v, want the Forgejo failure %q", row.name, err, row.forgejo)
				}
			} else {
				var no *refusal
				if !errors.As(err, &no) || no.status != row.status || no.msg != row.answer {
					t.Fatalf("doSetPool(%s) = %v, want %d %q", row.name, err, row.status, row.answer)
				}
			}
			equal(t, "the answer", res, row.res)
			infra.mu.Lock()
			writes := []int{len(infra.contents), len(infra.pulls), len(infra.merges)}
			infra.mu.Unlock()
			equal(t, "one change, never a retry", writes, []int{1, 1, 1})
			equal(t, "the rows", r.events.count()-before, row.rows)
		})
	}
}

// TestPoolProposalDisplay covers the pool proposal's typed preview, live
// and restored: the stream's proposal event and GET /chat's proposal
// state carry the same safe display fields — the action, the suggested
// replacement or increment, the captured allowance, the ceiling the
// change would stand at and the fixed reason, whole cents — and never
// the capture's source. A generic proposal's shape changes none, and
// the captured allowance stands across a reload's current-policy move:
// the member's corrected press previews its ceiling against what the
// row holds, not the policy in force.
func TestPoolProposalDisplay(t *testing.T) {
	r, g, _ := poolChatRig(t, 10, 3)
	poolChatReload(r, 14)
	ck, s := r.loggedIn(t)
	states := func() map[string]factoryd.ProposalState {
		t.Helper()
		var answer chatGetAnswer
		if err := json.Unmarshal(r.do(httptest.NewRequest(http.MethodGet, chatPath, nil), ck).Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		out := map[string]factoryd.ProposalState{}
		for _, st := range answer.Proposals {
			out[st.Token] = st
		}
		return out
	}
	live := func(event string) *factoryd.PoolPreview {
		t.Helper()
		var p struct {
			Pool *factoryd.PoolPreview `json:"pool"`
		}
		data, ok := strings.CutPrefix(event, "proposal ")
		if !ok || json.Unmarshal([]byte(data), &p) != nil {
			t.Fatalf("the proposal event does not read: %s", event)
		}
		return p.Pool
	}

	// a set of EUR 10 against the standing EUR 10, then a top-up of
	// +EUR 5 against the same pool: each live event and each restored
	// state carry the typed preview, the top-up's ceiling EUR 15
	for _, row := range []struct {
		name string
		args string
		pool string
		want factoryd.PoolPreview
	}{
		{"a set", `{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"}`,
			`"pool":{"action":"set","eur":10,"from_eur":10,"to_eur":10,"reason":"Allow two small verified follow-ups"}`,
			factoryd.PoolPreview{Action: "set", EUR: 10, FromEUR: 10, ToEUR: 10, Reason: "Allow two small verified follow-ups"}},
		{"a top-up", `{"action":"top_up","eur":5,"reason":"a second finding"}`,
			`"pool":{"action":"top_up","eur":5,"from_eur":10,"to_eur":15,"reason":"a second finding"}`,
			factoryd.PoolPreview{Action: "top_up", EUR: 5, FromEUR: 10, ToEUR: 15, Reason: "a second finding"}},
	} {
		g.replies = [][]string{{call(1, "set_pool", row.args)}}
		_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"a pool for the findings"}})
		if len(tokens) != 1 || len(events) != 2 {
			t.Fatalf("%s: %d tokens, events %v", row.name, len(tokens), events)
		}
		if !strings.Contains(events[0], row.pool) {
			t.Errorf("%s's live event lacks the preview %s:\n%s", row.name, row.pool, events[0])
		}
		if got := live(events[0]); got == nil || *got != row.want {
			t.Errorf("%s's live preview = %+v, want %+v", row.name, got, row.want)
		}
		restored := states()[tokens[0]]
		if restored.State != "pending" || restored.Pool == nil || *restored.Pool != row.want {
			t.Errorf("%s's restored state = %+v, want pending with the same preview %+v", row.name, restored, row.want)
		}
		if b, err := json.Marshal(restored); err != nil ||
			string(b) != fmt.Sprintf(`{"token":%q,"state":"pending","pool":%s}`, tokens[0], strings.TrimPrefix(row.pool, `"pool":`)) {
			t.Errorf("%s's restored state carries more than the safe fields: %s (%v)", row.name, b, err)
		}
	}

	// a generic proposal's shape changes none: no preview rides it
	cardArgs := `{"title":"Cache metrics","track":"direct","track_reason":"one count"}`
	g.replies = [][]string{{call(1, "new_card", cardArgs)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"file the count"}})
	equal(t, "the generic proposal", events, []string{
		`proposal {"arguments":` + cardArgs + `,"description":"File a card, title Cache metrics, track direct, track_reason one count","expires":"EXPIRES","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	if st := states()[tokens[0]]; st.Pool != nil {
		t.Errorf("a generic proposal carries a pool preview: %+v", st)
	}

	// the captured EUR 10 stands across a current-policy move: the
	// preview keeps reading the row's capture, and the corrected +EUR 4
	// previews its ceiling against it — EUR 14 — with no fresh read of
	// the policy in force
	g.replies = [][]string{{call(1, "set_pool", `{"action":"top_up","eur":5,"reason":"a second finding"}`)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"top the pool up"}})
	want := factoryd.PoolPreview{Action: "top_up", EUR: 5, FromEUR: 10, ToEUR: 15, Reason: "a second finding"}
	before := states()[tokens[0]]
	if before.State != "pending" || before.Pool == nil || *before.Pool != want {
		t.Fatalf("the top-up's restored state = %+v, want pending with %+v", before, want)
	}
	r.ledger.files["ai-software-factory-code"].Escalation.Pool.EUR = 25
	if moved := states()[tokens[0]]; moved.Pool == nil || *moved.Pool != want {
		t.Errorf("the preview after the current policy moved = %+v, want the captured %+v", moved.Pool, want)
	}
	_, done, _ := chatPostRaw(t, r, ck, s, chatPath+"/proposals/"+tokens[0], `{"correction":{"eur":4}}`)
	equal(t, "the corrected Do it", done, ran("set_pool", `{"action":"top_up","eur":4,"reason":"a second finding"}`,
		"The pool of ai-software-factory-code is €14.00 now, was €10.00: sf/infra#12 carries the change and the policy in force holds it."))
	if used := states()[tokens[0]]; used.State != "used" || used.Pool == nil || *used.Pool != want {
		t.Errorf("the used top-up's state = %+v, want used with the captured %+v", used, want)
	}
}
