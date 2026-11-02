package acceptance

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The acceptance proof of the month pool's write integrity (the
// pool-policy rig card of plan #1399): the set_pool change a confirmed
// proposal carries, end-to-end through the console's HTTP surface on the
// configuration repository's fake — one guarded branch commit off main,
// one pull request, one merge, one reload request and one pool_changed
// row, the file's own pool the only key that moves — and the refusals a
// stale, rejected, malformed or unbounded change answers, each its exact
// status, read from the tool row the press stored beside the sentence,
// and none of them writing.

// accInfraFile is the code project's file on the configuration
// repository's main.
const accInfraFile = "factory/projects/" + accProjectName + ".yaml"

// accPoolHead is the project file a policy PR wrote: commented, with the
// bounds the pool needs, the pool at EUR 10.
const accPoolHead = "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n" +
	"  # the bounds the pool needs, which a policy PR writes\n  month_eur: 600\n  task_max_eur: 5\n" +
	"  raise_max_eur: 1\n  discretionary_eur: 10\n"

// accPoolLanded is accPoolHead after a set of EUR 12: only the pool
// moved; the comment and every other key stand.
const accPoolLanded = "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n" +
	"  # the bounds the pool needs, which a policy PR writes\n  month_eur: 600\n  task_max_eur: 5\n" +
	"  raise_max_eur: 1\n  discretionary_eur: 12\n"

// poolRig answers the rig with the member logged in and the code
// project's file seeded on infra's main.
func poolRig(t *testing.T) (*AcceptanceRig, string, *http.Cookie, string) {
	t.Helper()
	r, _, path := NewAcceptanceRig(t, false, nil)
	ck, csrf := r.Login(t)
	r.PutInfraFile(accInfraFile, accPoolHead)
	return r, path, ck, csrf
}

// poolToken answers the set_pool proposal the turn's stream carries.
func poolToken(t *testing.T, events []string) string {
	t.Helper()
	for _, e := range events {
		if !strings.HasPrefix(e, "proposal ") {
			continue
		}
		var p struct {
			ID   string `json:"id"`
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "proposal ")), &p); err != nil {
			t.Fatalf("the proposal event %q: %v", e, err)
		}
		if p.Tool == "set_pool" {
			return p.ID
		}
	}
	t.Fatalf("the stream holds no set_pool proposal:\n%v", events)
	return ""
}

// proposePool has the member ask and the agent call set_pool with args;
// it answers the proposal's token.
func proposePool(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, args string) string {
	t.Helper()
	r.Reply([]string{Call(1, "set_pool", args)})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"please change the pool"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	return poolToken(t, eventsOf(t, body))
}

// pressPool presses the proposal's Do it; it answers the press's stream.
func pressPool(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, token string) []string {
	t.Helper()
	code, body := postChat(t, r, path+"/proposals/"+token, ck, url.Values{"csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	return eventsOf(t, body)
}

// pressStatus answers the HTTP status the press's set_pool call answered,
// from the tool row its Do it stored — the machine fact behind the
// refusal's sentence, 0 for a call that ran.
func pressStatus(t *testing.T, r *AcceptanceRig) int {
	t.Helper()
	rows := r.ToolCalls("sf/code", "sebastian")
	if len(rows) == 0 {
		t.Fatalf("the press stored no tool row")
	}
	return rows[len(rows)-1].Status
}

// refusedPool proposes set_pool with args and presses Do it; it answers
// the press's stream and the status the refused call answered.
// corrective is the agent's reply to the refusal.
func refusedPool(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, args, corrective string) ([]string, int) {
	t.Helper()
	r.Reply([]string{Call(1, "set_pool", args)}, []string{Say(corrective)})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"please change the pool"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	return pressPool(t, r, path, ck, csrf, poolToken(t, eventsOf(t, body))), pressStatus(t, r)
}

// TestPoolPolicyPR is the live check of the pool write's integrity: the
// confirmed Set EUR 12 lands as one guarded commit, one pull request and
// one merge that changes only discretionary_eur, asks one reload and
// records one pool_changed row; a change whose source moved answers the
// 409 guard, a candidate the check refuses answers 422, a malformed
// amount answers 400 and a file without the bounds the pool needs
// answers 409 — and none of them writes.
func TestPoolPolicyPR(t *testing.T) {
	r, path, ck, csrf := poolRig(t)

	// the valid Set EUR 12: proposed, then confirmed by the member's Do it
	args := `{"action":"set","eur":12,"reason":"the findings cost money"}`
	token := proposePool(t, r, path, ck, csrf, args)
	if writes := len(r.InfraCommits()) + len(r.InfraPulls()) + len(r.InfraMerges()); writes != 0 {
		t.Fatalf("the proposal alone wrote %d changes", writes)
	}
	r.OnReload(func() { r.SetLoadedPool(12) })
	answer := "The pool of " + accProjectName + " is €12.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it."
	equal(t, "the Do it's stream", pressPool(t, r, path, ck, csrf, token), []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_pool","result":"` + answer + `"}`,
		`applied {"result":"` + answer + `","tool":"set_pool"}`, `done {}`})
	equal(t, "the landed call's status", pressStatus(t, r), 0)

	// one guarded commit on a branch of main: the file read with the
	// config token, the change made with the member's own token, the
	// blob the guard held the one the proposal read
	commits := r.InfraCommits()
	if len(commits) != 1 {
		t.Fatalf("%d commits, want one: %+v", len(commits), commits)
	}
	c := commits[0]
	if !strings.HasPrefix(c.Branch, "config-"+accProjectName+"-") {
		t.Errorf("the commit's branch = %q", c.Branch)
	}
	equal(t, "the commit's source branch", c.From, "main")
	if !strings.HasPrefix(c.Message, "Project "+accProjectName+": month pool 12.00 EUR\n\n") ||
		!strings.Contains(c.Message, "Factory-Role: chat\n") ||
		!strings.Contains(c.Message, "Requested-by: sebastian\n") {
		t.Errorf("the commit's message = %q", c.Message)
	}
	if len(c.Files) != 1 {
		t.Fatalf("the commit changes %d files, want the project's one: %+v", len(c.Files), c.Files)
	}
	f := c.Files[0]
	equal(t, "the change", []string{f.Operation, f.Path, f.Content, f.SHA},
		[]string{"update", accInfraFile, accB64(accPoolLanded), accBlob(accPoolHead)})
	if !strings.Contains(c.Bearer, "access-") {
		t.Errorf("the commit's token = %q, want the member's", c.Bearer)
	}
	for _, want := range []string{
		"GET /api/v1/repos/sf/infra/contents/factory/projects/" + accProjectName + ".yaml Bearer config-token",
		"POST /api/v1/repos/sf/infra/contents " + c.Bearer,
	} {
		if !slices.Contains(r.InfraCalls(), want) {
			t.Errorf("the write's calls lack %q:\n%v", want, r.InfraCalls())
		}
	}

	// one pull request and one merge: the counted identity the answer and
	// the event name
	pulls := r.InfraPulls()
	if len(pulls) != 1 {
		t.Fatalf("%d pull requests, want one: %+v", len(pulls), pulls)
	}
	equal(t, "the pull request", []string{pulls[0].Base, pulls[0].Title},
		[]string{"main", "Project " + accProjectName + ": month pool 12.00 EUR"})
	if !strings.HasPrefix(pulls[0].Head, "config-"+accProjectName+"-") {
		t.Errorf("the pull request's head = %q", pulls[0].Head)
	}
	merges := r.InfraMerges()
	if len(merges) != 1 || merges[0].Number != 101 {
		t.Fatalf("the merges = %+v, want one of pull request 101", merges)
	}

	// the change the check judged is the candidate that landed, only
	// discretionary_eur moved, and the reload was asked once
	equal(t, "the checked candidate", r.Checked(),
		[]map[string]string{{"projects/" + accProjectName + ".yaml": accPoolLanded}})
	equal(t, "main's file", r.InfraFiles()[accInfraFile], accPoolLanded)
	equal(t, "the reload requests", r.Reloads(), 1)

	// one pool_changed row naming the member, and the policy in force
	// holds the new ceiling: the reload's activation, alone, did it
	rows := r.Events()
	if len(rows) != 1 {
		t.Fatalf("%d event rows, want the one pool_changed: %+v", len(rows), rows)
	}
	e := rows[0]
	equal(t, "the row", []any{e.Kind, e.Actor, e.Via, e.Repository},
		[]any{factoryd.EventPoolChanged, "sebastian", factoryd.ViaChat, "sf/code"})
	equal(t, "the row's detail", e.Detail, map[string]any{"action": "set", "from_eur": 10.0,
		"confirmed_eur": 12.0, "to_eur": 12.0, "reason": "the findings cost money", "pr": "sf/infra#101"})
	loaded, ok := r.LoadedPool()
	equal(t, "the loaded pool", []any{ok, loaded}, []any{true, 12.0})

	// a stale change: another writer landed between the change's read and
	// its guarded commit, and the 409 guard names the blob that moved
	moved := strings.Replace(accPoolLanded, "discretionary_eur: 12", "discretionary_eur: 14", 1)
	stale := `{"action":"top_up","eur":1,"reason":"a later finding"}`
	r.Reply([]string{Call(1, "set_pool", stale)}, []string{Say("I will read the file again.")})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"top the pool up"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	token = poolToken(t, eventsOf(t, body))
	r.SwitchOnRead(accInfraFile, moved)
	equal(t, "the stale press", pressPool(t, r, path, ck, csrf, token), []string{
		`tool {"arguments":` + stale + `,"duration_ms":0,"name":"set_pool","result":"Refused: projects/` + accProjectName +
			`.yaml changed since the change was proposed (its blob is now ` + accBlob(moved) + `): read it again and propose the change anew"}`,
		`text {"text":"I will read the file again."}`, `done {}`})
	equal(t, "the stale refusal's status", pressStatus(t, r), http.StatusConflict)
	equal(t, "the other writer's file", r.InfraFiles()[accInfraFile], moved)
	r.PutInfraFile(accInfraFile, accPoolLanded)

	// a candidate the change check refuses: 422, its line whole
	r.RefuseCheck([]string{"projects/" + accProjectName + ".yaml: budget.month_eur must not be negative"}, nil)
	rejected := `{"action":"set","eur":20,"reason":"over a broken bound"}`
	stream, status := refusedPool(t, r, path, ck, csrf, rejected, "I will write a policy PR.")
	equal(t, "the rejected candidate", stream, []string{
		`tool {"arguments":` + rejected + `,"duration_ms":0,"name":"set_pool","result":"Refused: The project file would not load: projects/` +
			accProjectName + `.yaml: budget.month_eur must not be negative"}`,
		`text {"text":"I will write a policy PR."}`, `done {}`})
	equal(t, "the rejected candidate's status", status, http.StatusUnprocessableEntity)
	r.RefuseCheck(nil, nil)

	// a malformed amount: 400, before any file is read for a write
	for _, tc := range []struct{ args, refusal string }{
		{`{"action":"set","eur":-5,"reason":"below zero"}`, "a set takes a non-negative amount in EUR"},
		{`{"action":"top_up","eur":0,"reason":"nothing"}`, "a top-up takes a positive amount in EUR"},
		{`{"action":"set","eur":12.005,"reason":"sub-cent"}`, "a pool change's amount is whole cents"},
	} {
		stream, status := refusedPool(t, r, path, ck, csrf, tc.args, "Understood.")
		equal(t, "the malformed amount "+tc.args, stream, []string{
			`tool {"arguments":` + tc.args + `,"duration_ms":0,"name":"set_pool","result":"Refused: ` + tc.refusal + `"}`,
			`text {"text":"Understood."}`, `done {}`})
		equal(t, "the malformed amount "+tc.args+" answers", status, http.StatusBadRequest)
	}

	// a file without the bounds the pool needs: 409, each named
	for _, tc := range []struct{ name, file, refusal string }{
		{"no budget block", "# the code project\nrepository: sf/code\nmerge_rule: human\n",
			"projects/" + accProjectName + ".yaml sets no budget block: a policy PR writes one, with month_eur and raise_max_eur, before a pool can change"},
		{"no month cap", "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n  raise_max_eur: 1\n  discretionary_eur: 12\n",
			"projects/" + accProjectName + ".yaml sets no budget.month_eur: a policy PR writes the project's month cap first (an intentional month of zero writes month_eur: 0)"},
		{"no per-raise bound", "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n  month_eur: 600\n  raise_max_eur: 0\n  discretionary_eur: 12\n",
			"projects/" + accProjectName + ".yaml sets no budget.raise_max_eur above 0: a policy PR writes the per-raise bound first (every raise of the pool would refuse)"},
	} {
		r.PutInfraFile(accInfraFile, tc.file)
		args20 := `{"action":"set","eur":20,"reason":"unbounded"}`
		stream, status := refusedPool(t, r, path, ck, csrf, args20, "Understood.")
		equal(t, "the file with "+tc.name, stream, []string{
			`tool {"arguments":` + args20 + `,"duration_ms":0,"name":"set_pool","result":"Refused: ` + tc.refusal + `"}`,
			`text {"text":"Understood."}`, `done {}`})
		equal(t, "the file with "+tc.name+" answers", status, http.StatusConflict)
	}
	r.PutInfraFile(accInfraFile, accPoolLanded)

	// the refusals aside: no second commit, pull request, merge, reload
	// or row — a refused change writes nothing
	equal(t, "the commits", len(r.InfraCommits()), 1)
	equal(t, "the pull requests", len(r.InfraPulls()), 1)
	equal(t, "the merges", len(r.InfraMerges()), 1)
	equal(t, "the reload requests", r.Reloads(), 1)
	equal(t, "the event rows", len(r.Events()), 1)
	equal(t, "main's file", r.InfraFiles()[accInfraFile], accPoolLanded)
}

// accB64 is text as the console posts it in a file change.
func accB64(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
