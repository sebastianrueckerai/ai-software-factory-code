package acceptance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The acceptance proof of the escalation pool's write integrity (the
// pool-policy rig card of plan #1399): the set_pool change a confirmed
// proposal carries, end-to-end through the console's HTTP surface on the
// configuration repository's fake: one guarded branch commit off main,
// one pull request, one merge, one reload request and one pool_changed
// row, the file's escalation.pool.eur the only key that moves. A refused press is
// its typed outcome's real HTTP status, answered before any stream: a
// stale captured source 409 whose spent token replays 404, a candidate
// the change check refuses 422, a malformed correction 400 that leaves
// the proposal usable for its owner. A refusal the offer itself answers,
// a malformed amount or a file whose pool the write cannot take, is the
// streamed refusal sentence with the typed status its tool row keeps,
// before any token exists. None of them writes.

// accInfraFile is the code project's file on the configuration
// repository's main.
const accInfraFile = "factory/projects/" + accProjectName + ".yaml"

// accPoolBase is the project file a policy PR wrote, commented, with an
// escalation pool that sets its sessions and leaves its money to the
// policy.
const accPoolBase = "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n  month_eur: 600\n  task_max_eur: 5\n" +
	"escalation:\n  # the pool every extend and fund draws from, which a policy PR writes\n  pool:\n    sessions: 30\n"

// accPoolHead is accPoolBase with the pool's money at EUR 10.
const accPoolHead = accPoolBase + "    eur: 10\n"

// accPoolLanded is accPoolHead after a set of EUR 12: only the pool's
// money moved, the comment and every other key stand.
const accPoolLanded = accPoolBase + "    eur: 12\n"

// accPoolTopped is accPoolLanded after a top-up of EUR 1: only the pool's
// money moved again, on the source the top-up captured.
const accPoolTopped = accPoolBase + "    eur: 13\n"

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

// pressPoolRaw presses the proposal's Do it with body as the request's
// whole body; it answers the press's status and body, so a refusal is
// read as the typed outcome the press answered, not as a stream.
func pressPoolRaw(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, token, body string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path+"/proposals/"+token, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(ck)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// refusedPress presses the proposal's Do it and asserts the typed
// refusal it answered before any stream: status want and body literal,
// whole.
func refusedPress(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, token string, want int, literal string) {
	t.Helper()
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != want || strings.TrimSpace(body) != literal {
		t.Fatalf("Do it = %d:\n%s\nwant %d:\n%s", code, body, want, literal)
	}
}

// poolRowStatus answers the HTTP status the last set_pool call answered,
// from the tool row that stored it — the machine fact behind the
// refusal's sentence, 0 for a call that ran.
func poolRowStatus(t *testing.T, r *AcceptanceRig) int {
	t.Helper()
	rows := r.ToolCalls("sf/code", "sebastian")
	if len(rows) == 0 {
		t.Fatalf("no set_pool tool row was stored")
	}
	return rows[len(rows)-1].Status
}

// offerRefusal proposes set_pool with args the proposal itself refuses
// — before any token exists — and asserts the streamed refusal sentence,
// the typed status its tool row keeps and that the stream carries no
// proposal. corrective is the agent's reply to the refusal.
func offerRefusal(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, args, corrective, refusal string, status int) {
	t.Helper()
	r.Reply([]string{Call(1, "set_pool", args)}, []string{Say(corrective)})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"please change the pool"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	events := eventsOf(t, body)
	for _, e := range events {
		if strings.HasPrefix(e, "proposal ") {
			t.Fatalf("the refused offer %s still proposed:\n%v", args, events)
		}
	}
	equal(t, "the offer's refusal "+args, events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_pool","result":"Refused: ` + refusal + `"}`,
		`text {"text":"` + corrective + `"}`, `done {}`})
	equal(t, "the offer's refusal status "+args, poolRowStatus(t, r), status)
}

// landedPress asserts the successful press's stream structurally: the
// tool block of the write that happened before the stream opened, the
// applied line and done — the tool's name, arguments and result whole,
// its duration a number the test never pins to zero.
func landedPress(t *testing.T, what, body, args, words string) {
	t.Helper()
	var kinds []string
	var tool struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Result     string          `json:"result"`
		DurationMS *int64          `json:"duration_ms"`
	}
	var applied map[string]string
	for _, msg := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		line := strings.Split(msg, "\n")
		if len(line) < 2 || !strings.HasPrefix(line[0], "event: ") {
			t.Fatalf("%s holds a stream block of no event: %q", what, msg)
		}
		data := strings.TrimPrefix(line[1], "data: ")
		switch kind := strings.TrimPrefix(line[0], "event: "); kind {
		case "tool":
			if err := json.Unmarshal([]byte(data), &tool); err != nil {
				t.Fatalf("%s's tool block %q: %v", what, data, err)
			}
		case "applied":
			if err := json.Unmarshal([]byte(data), &applied); err != nil {
				t.Fatalf("%s's applied line %q: %v", what, data, err)
			}
		}
		kinds = append(kinds, strings.TrimPrefix(line[0], "event: "))
	}
	equal(t, what+"'s events", kinds, []string{"tool", "applied", "done"})
	var want, got bytes.Buffer
	if err := json.Compact(&want, []byte(args)); err != nil {
		t.Fatalf("the expected arguments %s: %v", args, err)
	}
	if err := json.Compact(&got, tool.Arguments); err != nil {
		t.Fatalf("the tool block's arguments %s: %v", tool.Arguments, err)
	}
	if tool.Name != "set_pool" || tool.Result != words || tool.DurationMS == nil || *tool.DurationMS < 0 || want.String() != got.String() {
		t.Fatalf("%s's tool block = %s %s %v (arguments %s), want set_pool on %s answering %s",
			what, tool.Name, tool.Result, tool.DurationMS, got.String(), args, words)
	}
	equal(t, what+"'s applied line", applied, map[string]string{"tool": "set_pool", "result": words})
}

// poolWrites answers the writes the fake saw: commits, pull requests and
// merges together.
func poolWrites(r *AcceptanceRig) int {
	return len(r.InfraCommits()) + len(r.InfraPulls()) + len(r.InfraMerges())
}

// TestPoolPolicyPR is the live check of the pool write's integrity: the
// confirmed Set EUR 12 lands as one guarded commit, one pull request and
// one merge that changes only escalation.pool.eur, asks one reload and
// records one pool_changed row; a stale captured source answers the real
// 409 before any stream and its spent token replays 404, a candidate the
// change check refuses answers 422, a malformed amount or a file whose
// pool the write cannot take refuses at the offer (the streamed sentence
// with the typed status its row keeps), and a malformed correction of a
// valid proposal answers 400 that leaves the proposal usable. None of
// the refusals writes.
func TestPoolPolicyPR(t *testing.T) {
	r, path, ck, csrf := poolRig(t)

	// the valid Set EUR 12: proposed, then confirmed by the member's Do it
	args := `{"action":"set","eur":12,"reason":"the findings cost money"}`
	token := proposePool(t, r, path, ck, csrf, args)
	equal(t, "the writes the proposal alone made", poolWrites(r), 0)
	r.OnReload(func() { r.SetLoadedPool(12) })
	answer := "The pool of " + accProjectName + " is €12.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it."
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the Do it", body, args, answer)
	equal(t, "the landed call's status", poolRowStatus(t, r), 0)

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
	if !strings.HasPrefix(c.Message, "Project "+accProjectName+": escalation pool 12.00 EUR\n\n") ||
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
		[]string{"main", "Project " + accProjectName + ": escalation pool 12.00 EUR"})
	if !strings.HasPrefix(pulls[0].Head, "config-"+accProjectName+"-") {
		t.Errorf("the pull request's head = %q", pulls[0].Head)
	}
	merges := r.InfraMerges()
	if len(merges) != 1 || merges[0].Number != 101 {
		t.Fatalf("the merges = %+v, want one of pull request 101", merges)
	}

	// the change the check judged is the candidate that landed, only
	// escalation.pool.eur moved, and the reload was asked once
	equal(t, "the checked candidate", r.Checked(),
		[]map[string]string{{"projects/" + accProjectName + ".yaml": accPoolLanded}})
	equal(t, "main's file", r.InfraFiles()[accInfraFile], accPoolLanded)
	equal(t, "the reload requests", r.Reloads(), 1)

	// one pool_changed row naming the member, and the policy in force
	// holds the new pool: the reload's activation, alone, did it
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
	// its guarded commit, and the press answers the real 409 before any
	// stream, naming the blob that moved; the attempt was valid, so the
	// token is spent and its replay 404 — and nothing wrote
	moved := strings.Replace(accPoolLanded, "eur: 12\n", "eur: 14\n", 1)
	stale := `{"action":"top_up","eur":1,"reason":"a later finding"}`
	token = proposePool(t, r, path, ck, csrf, stale)
	r.SwitchOnRead(accInfraFile, moved)
	refusedPress(t, r, path, ck, csrf, token, http.StatusConflict,
		"projects/"+accProjectName+".yaml changed since the change was proposed (its blob is now "+accBlob(moved)+
			"): read it again and propose the change anew")
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusNotFound {
		t.Fatalf("the stale change's replay = %d:\n%s", code, body)
	}
	equal(t, "the other writer's file", r.InfraFiles()[accInfraFile], moved)
	equal(t, "the writes the stale change made", poolWrites(r), 3)
	r.PutInfraFile(accInfraFile, accPoolLanded)

	// a candidate the change check refuses: 422 before any stream, its
	// line whole
	r.RefuseCheck([]string{"projects/" + accProjectName + ".yaml: budget.month_eur must not be negative"}, nil)
	rejected := `{"action":"set","eur":20,"reason":"over a broken bound"}`
	token = proposePool(t, r, path, ck, csrf, rejected)
	refusedPress(t, r, path, ck, csrf, token, http.StatusUnprocessableEntity,
		"The project file would not load: projects/"+accProjectName+".yaml: budget.month_eur must not be negative")
	equal(t, "the rejected candidate", r.Checked()[len(r.Checked())-1],
		map[string]string{"projects/" + accProjectName + ".yaml": strings.Replace(accPoolLanded, "eur: 12\n", "eur: 20\n", 1)})
	equal(t, "the writes the rejected candidate made", poolWrites(r), 3)
	r.RefuseCheck(nil, nil)

	// a malformed amount refuses at the offer, before any token exists:
	// the streamed sentence, the typed 400 its row keeps, no proposal
	for _, tc := range []struct{ args, refusal string }{
		{`{"action":"set","eur":-5,"reason":"below zero"}`, "a set takes an amount above 0 EUR: a pool of 0 takes the policy's figure"},
		{`{"action":"set","eur":0,"reason":"the policy's figure"}`, "a set takes an amount above 0 EUR: a pool of 0 takes the policy's figure"},
		{`{"action":"top_up","eur":0,"reason":"nothing"}`, "a top-up takes a positive amount in EUR"},
		{`{"action":"set","eur":12.005,"reason":"sub-cent"}`, "a pool change's amount is whole cents"},
	} {
		offerRefusal(t, r, path, ck, csrf, tc.args, "Understood.", tc.refusal, http.StatusBadRequest)
		equal(t, "the writes the malformed amount "+tc.args+" made", poolWrites(r), 3)
	}

	// a file whose pool the write cannot take refuses at the offer too:
	// 409, each refusal naming what a policy PR corrects, no proposal
	const head = "# the code project\nrepository: sf/code\nmerge_rule: human\n"
	file := "projects/" + accProjectName + ".yaml"
	for _, tc := range []struct{ name, file, args, refusal string }{
		{"a top-up of the policy's money", head, `{"action":"top_up","eur":2,"reason":"no money of its own"}`,
			file + " sets no escalation.pool.eur, so the pool's money is the policy's: set the pool instead of a top-up"},
		{"an aliased pool", head + "escalation:\n  pool: &pool\n    eur: 12\n  card_draw: *pool\n", `{"action":"set","eur":20,"reason":"shared"}`,
			file + " aliases its escalation block or its pool: a policy PR removes the YAML alias before a pool change"},
		{"an escalation block no mapping", head + "escalation: 12\n", `{"action":"set","eur":20,"reason":"no block"}`,
			file + " holds an escalation block the pool cannot read: a policy PR corrects it"},
		{"a money the pool cannot take", head + "escalation:\n  pool:\n    eur: lots\n", `{"action":"set","eur":20,"reason":"no amount"}`,
			file + " holds an escalation.pool.eur the pool cannot take: a policy PR corrects it"},
	} {
		r.PutInfraFile(accInfraFile, tc.file)
		offerRefusal(t, r, path, ck, csrf, tc.args, "Understood.", tc.refusal, http.StatusConflict)
		equal(t, "the writes the file with "+tc.name+" made", poolWrites(r), 3)
	}
	r.PutInfraFile(accInfraFile, accPoolLanded)

	// a malformed correction of a valid proposal answers the real 400
	// before the token is spent, so the proposal stays its owner's to
	// use — and the plain press after it lands the captured change
	topUp := `{"action":"top_up","eur":1,"reason":"the last finding"}`
	token = proposePool(t, r, path, ck, csrf, topUp)
	for _, correction := range []struct{ body, refusal string }{
		{`{"correction":{"eur":0}}`, "a top-up takes a positive amount in EUR"},
		{`{"correction":{"eur":2.005}}`, "a pool change's amount is whole cents"},
	} {
		code, body := pressPoolRaw(t, r, path, ck, csrf, token, correction.body)
		if code != http.StatusBadRequest || strings.TrimSpace(body) != correction.refusal {
			t.Fatalf("the corrected Do it %s = %d:\n%s\nwant 400:\n%s", correction.body, code, body, correction.refusal)
		}
		equal(t, "the writes the malformed correction "+correction.body+" made", poolWrites(r), 3)
	}
	r.OnReload(func() { r.SetLoadedPool(13) })
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("the usable proposal's Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the top-up's Do it", body, topUp,
		"The pool of "+accProjectName+" is €13.00 now, was €12.00: sf/infra#102 carries the change and the policy in force holds it.")

	// the refusals aside: one more validated branch commit, pull request,
	// merge, reload and row — every refused change wrote nothing
	commits = r.InfraCommits()
	if len(commits) != 2 {
		t.Fatalf("%d commits, want the set's and the top-up's: %+v", len(commits), commits)
	}
	equal(t, "the top-up's source branch", commits[1].From, "main")
	if !strings.HasPrefix(commits[1].Message, "Project "+accProjectName+": escalation pool +1.00 EUR\n\n") {
		t.Errorf("the top-up's message = %q", commits[1].Message)
	}
	equal(t, "the top-up's change", []string{commits[1].Files[0].Operation, commits[1].Files[0].Content, commits[1].Files[0].SHA},
		[]string{"update", accB64(accPoolTopped), accBlob(accPoolLanded)})
	if len(r.InfraPulls()) != 2 || len(r.InfraMerges()) != 2 || r.InfraMerges()[1].Number != 102 {
		t.Fatalf("the pull requests = %+v, the merges = %+v, want one more of pull request 102", r.InfraPulls(), r.InfraMerges())
	}
	equal(t, "the reload requests", r.Reloads(), 2)
	rows = r.Events()
	if len(rows) != 2 {
		t.Fatalf("%d event rows, want the set's and the top-up's: %+v", len(rows), rows)
	}
	equal(t, "the top-up's row", []any{rows[1].Kind, rows[1].Actor, rows[1].Detail},
		[]any{factoryd.EventPoolChanged, "sebastian", map[string]any{"action": "top_up", "from_eur": 12.0,
			"confirmed_eur": 1.0, "to_eur": 13.0, "reason": "the last finding", "pr": "sf/infra#102"}})
	loaded, ok = r.LoadedPool()
	equal(t, "the loaded pool", []any{ok, loaded}, []any{true, 13.0})
	equal(t, "main's file", r.InfraFiles()[accInfraFile], accPoolTopped)
}

// TestPoolSourceBranch is the fake's own guard on the source a pool
// change branches from: a POST /contents that names a ref other than
// main — wrong or nonexistent — lands nothing, and a change that names
// main but not the blob the file holds refuses on the sha. The
// successful change's recorded source is main (TestPoolPolicyPR's
// commit); nothing here reaches a commit, pull request or merge.
func TestPoolSourceBranch(t *testing.T) {
	r, _, _, _ := poolRig(t)
	for _, tc := range []struct {
		name, branch, sha string
		want              int
		refusal           string
	}{
		{"a wrong source branch", "develop", accBlob(accPoolHead), http.StatusNotFound, `{"message":"object does not exist"}`},
		{"a nonexistent source branch", "no-such-ref", accBlob(accPoolHead), http.StatusNotFound, `{"message":"object does not exist"}`},
		{"a stale sha on main", "main", "0000000000000", http.StatusConflict, `{"message":"sha does not match"}`},
	} {
		b, err := json.Marshal(map[string]any{"branch": tc.branch, "new_branch": "config-direct",
			"message": "Project " + accProjectName + ": escalation pool 12.00 EUR",
			"files": []any{map[string]any{"operation": "update", "path": accInfraFile,
				"content": accB64(accPoolLanded), "sha": tc.sha}}})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, r.InfraAPI()+"/contents", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.want || strings.TrimSpace(string(body)) != tc.refusal {
			t.Errorf("%s = %d:\n%s\nwant %d:\n%s", tc.name, resp.StatusCode, body, tc.want, tc.refusal)
		}
		if writes := poolWrites(r); writes != 0 {
			t.Errorf("%s wrote %d changes", tc.name, writes)
		}
	}
	equal(t, "main's file", r.InfraFiles()[accInfraFile], accPoolHead)
	equal(t, "the change checks", len(r.Checked()), 0)
}

// accB64 is text as the console posts it in a file change.
func accB64(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
