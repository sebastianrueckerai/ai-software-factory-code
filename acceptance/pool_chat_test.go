package acceptance

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The escalation pool's chat acceptance (the pool-chat-http card of plan
// #1399): the set_pool proposal through the real console surface — the
// offer the stream carries with the change it captured, the human's
// unchanged or corrected confirmation, and the access the confirmation
// itself re-judges — beside the top-up and the lowered pool the readout
// reads whole, and the partial failures a merged change can still leave:
// a merge Forgejo refused, a pool the loader has not activated and an
// audit row the spine refused, each answered with its typed HTTP status
// before any stream, its pull request named and the old pool alone in
// force.

// accPoolNoPool is accPoolHead without the pool's money: the policy's
// figure stands.
const accPoolNoPool = accPoolBase

// accPoolTopped4 is accPoolHead after a top-up of EUR 4: only the pool's
// money moved.
const accPoolTopped4 = accPoolBase + "    eur: 14\n"

// accPoolSet2 is accPoolHead after a set of EUR 2.
const accPoolSet2 = accPoolBase + "    eur: 2\n"

// accPool18 is accPoolHead after a top-up of EUR 4 onto a pool of 14.
const accPool18 = accPoolBase + "    eur: 18\n"

// poolOffer has the member ask and the agent call set_pool with args,
// and answers the proposal whole: its token, the description it carries
// and the preview of the change it captured — nothing but the proposal
// on the stream, nothing written.
func poolOffer(t *testing.T, r *AcceptanceRig, path string, ck *http.Cookie, csrf, args string) (string, string, factoryd.PoolPreview) {
	t.Helper()
	r.Reply([]string{Call(1, "set_pool", args)})
	code, body := postChat(t, r, path, ck, url.Values{"message": {"please change the pool"}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	events := eventsOf(t, body)
	var kinds []string
	for _, e := range events {
		k, _, _ := strings.Cut(e, " ")
		kinds = append(kinds, k)
	}
	equal(t, "the offer's events "+args, kinds, []string{"proposal", "done"})
	for _, e := range events {
		if !strings.HasPrefix(e, "proposal ") {
			continue
		}
		var p struct {
			ID          string                `json:"id"`
			Tool        string                `json:"tool"`
			Description string                `json:"description"`
			Pool        *factoryd.PoolPreview `json:"pool"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "proposal ")), &p); err != nil {
			t.Fatalf("the proposal event %q: %v", e, err)
		}
		if p.Tool != "set_pool" || p.Pool == nil {
			t.Fatalf("the proposal event %q is no set_pool offer with a preview", e)
		}
		return p.ID, p.Description, *p.Pool
	}
	t.Fatalf("the stream holds no set_pool proposal:\n%v", events)
	return "", "", factoryd.PoolPreview{}
}

// poolChangedRows answers the pool_changed rows the spine holds.
func poolChangedRows(r *AcceptanceRig) []factoryd.Event {
	var out []factoryd.Event
	for _, e := range r.Events() {
		if e.Kind == factoryd.EventPoolChanged {
			out = append(out, e)
		}
	}
	return out
}

// poolLineOf is the pool line the policy in force reads at now: the
// pool's money and the money its cards drew, whether the file sets the
// money, whether a part is overdrawn, and its words.
func poolLineOf(t *testing.T, r *AcceptanceRig, now time.Time) []any {
	t.Helper()
	v, ok, err := r.CurrentPolicy(t.Context())
	if err != nil || !ok {
		t.Fatalf("the policy in force = %v, %v", ok, err)
	}
	pl := r.c.ProjectPool(t.Context(), v, factoryd.Project{Name: accProjectName, Repository: "sf/code"}, now)
	return []any{pl.Pool.EUR, pl.Drawn.EUR, pl.Present, pl.Overdrawn, pl.Words}
}

// TestPoolChatProposal is the offer and its two confirmations through
// the chat's HTTP surface: the model's Set EUR 10 of a file that leaves
// the pool's money to the policy proposes with the change it captured — the operation, the
// suggestion, the reason and the ceiling it would stand at — and writes
// nothing; the unchanged confirmation lands 10 with the answer naming
// the pull request, and the corrected confirmation lands 12 in the
// merged file while the policy still holds 10: the answer names the
// pull request, distinguishes the merged change from the loaded one and
// leaves the activation to the loader's next poll.
func TestPoolChatProposal(t *testing.T) {
	r, path, ck, csrf := poolRig(t)
	r.PutInfraFile(accInfraFile, accPoolNoPool)
	args := `{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"}`

	// the offer: the captured change beside the arguments, nothing run
	token, desc, preview := poolOffer(t, r, path, ck, csrf, args)
	equal(t, "the offer's description", desc,
		"Set the pool, the pool's money is the policy's, set it at 10.00 EUR, reason Allow two small verified follow-ups, it would stand at 10.00 EUR")
	equal(t, "the offer's preview", []any{preview.Action, preview.EUR, preview.FromEUR, preview.ToEUR, preview.Reason},
		[]any{"set", 10.0, 0.0, 10.0, "Allow two small verified follow-ups"})
	equal(t, "the writes the offer made", poolWrites(r), 0)

	// the unchanged confirmation lands 10
	r.OnReload(func() { r.SetLoadedPool(10) })
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("the unchanged Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the unchanged Do it", body, args,
		"The pool of "+accProjectName+" is €10.00 now, was the policy's: sf/infra#101 carries the change and the policy in force holds it.")
	equal(t, "main's file after the set", r.InfraFiles()[accInfraFile], accPoolHead)
	loaded, ok := r.LoadedPool()
	equal(t, "the loaded pool", []any{ok, loaded}, []any{true, 10.0})
	equal(t, "the reload requests", r.Reloads(), 1)
	rows := poolChangedRows(r)
	equal(t, "the rows", len(rows), 1)
	equal(t, "the set's row", []any{rows[0].Actor, rows[0].Via, rows[0].Detail},
		[]any{"sebastian", factoryd.ViaChat, map[string]any{"action": "set", "from_eur": 0.0,
			"confirmed_eur": 10.0, "to_eur": 10.0, "reason": "Allow two small verified follow-ups", "pr": "sf/infra#101"}})

	// the corrected confirmation merges 12 while the policy still holds
	// 10: the answer distinguishes them, names the pull request and
	// leaves the activation to the loader
	token, _, _ = poolOffer(t, r, path, ck, csrf, args)
	r.OnReload(nil)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, `{"correction":{"eur":12}}`)
	if code != http.StatusServiceUnavailable || strings.TrimSpace(body) !=
		"The pool change is merged as sf/infra#102, but the policy in force still holds the pool of "+accProjectName+
			" at EUR 10.00: the loader picks the merge up on its next poll. The pool_changed row is written; the new ceiling counts only once the policy holds it." {
		t.Fatalf("the corrected Do it = %d:\n%s", code, body)
	}
	equal(t, "main's file after the corrected confirmation", r.InfraFiles()[accInfraFile], accPoolLanded)
	loaded, ok = r.LoadedPool()
	equal(t, "the policy still holds 10", []any{ok, loaded}, []any{true, 10.0})
	rows = poolChangedRows(r)
	equal(t, "the rows", len(rows), 2)
	equal(t, "the corrected change's row", rows[1].Detail, map[string]any{"action": "set", "from_eur": 10.0,
		"confirmed_eur": 12.0, "to_eur": 12.0, "reason": "Allow two small verified follow-ups", "pr": "sf/infra#102"})
	equal(t, "the writes both confirmations made", poolWrites(r), 6)
	equal(t, "the reload requests", r.Reloads(), 2)

	// the loader's next poll is what activates the ceiling
	r.SetLoadedPool(12)
	loaded, ok = r.LoadedPool()
	equal(t, "the loaded pool after the activation", []any{ok, loaded}, []any{true, 12.0})

	// the spent token replays 404 and writes nothing further
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusNotFound {
		t.Fatalf("the spent token's replay = %d:\n%s", code, body)
	}
	equal(t, "the writes after the replay", poolWrites(r), 6)
}

// TestPoolTopUp is the increment the human corrects and the pool line
// that reads it whole: the model's top-up of EUR 5 proposes with the pool
// it would stand at, the human's +4 lands 14 with the month's draws
// standing at 3, one row with the raise it confirmed, and the spent token
// replays 404. A set of 2 keeps the draws whole and reads the pool
// overdrawn in money, and a set of 0 refuses at the offer: the policy's
// figure is a pool without the project's money, which a set never
// writes.
func TestPoolTopUp(t *testing.T) {
	r, path, ck, csrf := poolRig(t)
	r.SetLoadedPool(10)
	now := time.Now().UTC()
	for _, eur := range []float64{1, 1, 1} { // the month's draws stand at 3
		seedFundRow(t, r, eur, now)
	}

	// the offer reads the standing pool and the pool the top-up would
	// stand at
	args := `{"action":"top_up","eur":5,"reason":"a second finding"}`
	token, desc, preview := poolOffer(t, r, path, ck, csrf, args)
	equal(t, "the offer's description", desc,
		"Set the pool, the pool stands at 10.00 EUR, top it up by 5.00 EUR, reason a second finding, it would stand at 15.00 EUR")
	equal(t, "the offer's preview", []any{preview.Action, preview.EUR, preview.FromEUR, preview.ToEUR},
		[]any{"top_up", 5.0, 10.0, 15.0})
	equal(t, "the writes the offer made", poolWrites(r), 0)

	// the human corrects +5 to +4: the raise alone changed, the pool
	// lands at 14 with the draws standing at 3
	r.OnReload(func() { r.SetLoadedPool(14) })
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, `{"correction":{"eur":4}}`)
	if code != http.StatusOK {
		t.Fatalf("the corrected Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the corrected Do it", body, `{"action":"top_up","eur":4,"reason":"a second finding"}`,
		"The pool of "+accProjectName+" is €14.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it.")
	rows := poolChangedRows(r)
	equal(t, "the rows", len(rows), 1)
	equal(t, "the top-up's row", rows[0].Detail, map[string]any{"action": "top_up", "from_eur": 10.0,
		"confirmed_eur": 4.0, "to_eur": 14.0, "reason": "a second finding", "pr": "sf/infra#101"})
	equal(t, "main's file after the top-up", r.InfraFiles()[accInfraFile], accPoolTopped4)
	equal(t, "the pool line", poolLineOf(t, r, now), []any{14.0, 3.0, true, false,
		"0 of 30 sessions, 0 h of 30 h agent time and €3.00 of €14.00 drawn this UTC month"})
	if drawn, err := r.PoolDrawn(t.Context(), "sf/code", now); err != nil || drawn != (policy.Draw{EUR: 3}) {
		t.Fatalf("the month's draws = %+v, %v; want 3 EUR still", drawn, err)
	}

	// the spent token replays 404 and writes nothing further
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusNotFound {
		t.Fatalf("the spent token's replay = %d:\n%s", code, body)
	}
	equal(t, "the writes after the replay", poolWrites(r), 3)

	// set 2: the draws stand whole and the pool reads overdrawn in money,
	// where only the human draws more
	set := `{"action":"set","eur":2,"reason":"the month is nearly spent"}`
	token, _, _ = poolOffer(t, r, path, ck, csrf, set)
	r.OnReload(func() { r.SetLoadedPool(2) })
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusOK {
		t.Fatalf("the lowered set's Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the lowered set's Do it", body, set,
		"The pool of "+accProjectName+" is €2.00 now, was €14.00: sf/infra#102 carries the change and the policy in force holds it.")
	equal(t, "main's file after the lowered set", r.InfraFiles()[accInfraFile], accPoolSet2)
	equal(t, "the pool line the lowered set leaves", poolLineOf(t, r, now), []any{2.0, 3.0, true, true,
		"0 of 30 sessions, 0 h of 30 h agent time and €3.00 of €2.00 drawn this UTC month, overdrawn in money: only the human draws more until the month turns"})

	// set 0 refuses at the offer, before any token: a pool of 0 is the
	// policy's figure, which the project file leaves unset
	offerRefusal(t, r, path, ck, csrf, `{"action":"set","eur":0,"reason":"stop the spending"}`, "Understood.",
		"a set takes an amount above 0 EUR: a pool of 0 takes the policy's figure", http.StatusBadRequest)
	equal(t, "main's file after the refused zero", r.InfraFiles()[accInfraFile], accPoolSet2)
	equal(t, "the writes the two confirmations made", poolWrites(r), 6)
}

// TestPoolConfirmationAccess is the human press the pool change always
// needs and the access the press itself re-judges: a writer without the
// approvers' authority offers nothing, full autonomy and a custom
// confirm list still only propose, another member's press refuses
// without spending the owner's token, a correction may override none of
// the captured fields but the amount, the approvers are re-read at the
// press and the expired or consumed proposal answers 404 — while the
// owner's press lands the corrected change with the actor from the
// session alone.
func TestPoolConfirmationAccess(t *testing.T) {
	// the writer seated before any login: the console reads the writers
	// list once per login, so bob is a member of the repository from the
	// first read — a member without the approvers' authority
	r, _, path := NewAcceptanceRig(t, false, nil)
	r.PutInfraFile(accInfraFile, accPoolHead)
	r.AddProjectWriter("bob")
	ck, csrf := r.Login(t)
	r.SetLoadedPool(10)
	args := `{"action":"set","eur":10,"reason":"Allow two small verified follow-ups"}`

	// a writer of the repository without the approvers' authority offers
	// nothing: the streamed refusal with the typed status its row keeps
	bk, bcsrf := r.LoginAs(t, "bob")
	equal(t, "the non-approver's offer", agentCall(t, r, path, bk, bcsrf, "set_pool", args, "Understood."), []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"set_pool","result":"Refused: Only the approvers of project ` + accProjectName + ` may change the escalation pool."}`,
		`text {"text":"Understood."}`, `done {}`})
	equal(t, "the non-approver's offer status", lastToolRow(t, r, "bob", "set_pool").Status, http.StatusForbidden)
	equal(t, "the writes the non-approver's offer made", poolWrites(r), 0)
	ck, csrf = r.LoginAs(t, "sebastian")

	// full autonomy still proposes: the member's press alone runs a pool
	// change
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	r.SetLoadedMergeRule(policy.MergeAuto)
	full, _, _ := poolOffer(t, r, path, ck, csrf, args)
	equal(t, "the writes the full-autonomy offer made", poolWrites(r), 0)

	// a custom confirm list that leaves the pool off it still proposes
	r.SetLoadedAutonomy(policy.AutonomyManual)
	r.SetLoadedMergeRule(policy.MergeHuman)
	r.SetLoadedChat(&policy.ChatConfig{Confirm: []string{"fund"}})
	held, _, _ := poolOffer(t, r, path, ck, csrf, args)
	equal(t, "the writes the custom-confirm offer made", poolWrites(r), 0)

	// full autonomy under that same custom list still only proposes: the
	// one combination that could most plausibly skip the human's press
	r.SetLoadedAutonomy(policy.AutonomyAutonomous)
	r.SetLoadedMergeRule(policy.MergeAuto)
	_, _, _ = poolOffer(t, r, path, ck, csrf, args)
	equal(t, "the writes the full-auto custom-confirm offer made", poolWrites(r), 0)

	// the wrong member's press refuses and spends nothing: the owner's
	// token survives it
	ak, acsrf := r.LoginAs(t, "anna")
	code, body := pressPoolRaw(t, r, path, ak, acsrf, held, "")
	if code != http.StatusForbidden || strings.TrimSpace(body) != "It is another human's." {
		t.Fatalf("anna's press of sebastian's proposal = %d:\n%s", code, body)
	}
	equal(t, "the writes the wrong member's press made", poolWrites(r), 0)
	ck, csrf = r.LoginAs(t, "sebastian")

	// the correction is the amount alone: an override of the actor, the
	// operation, the reason, the repository or the source baseline
	// answers 400 and leaves the proposal its owner's
	for _, correction := range []string{
		`{"correction":{"eur":12,"actor":"eve"}}`,
		`{"correction":{"eur":12,"action":"top_up"}}`,
		`{"correction":{"eur":12,"reason":"override"}}`,
		`{"correction":{"eur":12,"repository":"sf/other"}}`,
		`{"correction":{"eur":12,"source":"` + accBlob(accPoolHead) + `"}}`,
	} {
		code, body := pressPoolRaw(t, r, path, ck, csrf, held, correction)
		if code != http.StatusBadRequest || strings.TrimSpace(body) != `The correction is one JSON object {"correction":{"eur":12}} and nothing else.` {
			t.Fatalf("the overriding correction %s = %d:\n%s", correction, code, body)
		}
		equal(t, "the writes the correction "+correction+" made", poolWrites(r), 0)
	}

	// the authority is re-read at the press: a revoked approver answers
	// the real 403 before any stream, writes nothing and spends the
	// token the attempt was valid enough to take
	r.SetLoadedApprovers([]string{"anna"})
	code, body = pressPoolRaw(t, r, path, ck, csrf, full, "")
	if code != http.StatusForbidden || strings.TrimSpace(body) != "Only the approvers of project "+accProjectName+" may change the escalation pool." {
		t.Fatalf("the revoked approver's press = %d:\n%s", code, body)
	}
	equal(t, "the writes the revoked approver's press made", poolWrites(r), 0)
	code, body = pressPoolRaw(t, r, path, ck, csrf, full, "")
	if code != http.StatusNotFound {
		t.Fatalf("the revoked approver's replay = %d:\n%s", code, body)
	}
	r.SetLoadedApprovers(nil)

	// the expired proposal answers 404 whatever it held
	gone, _, _ := poolOffer(t, r, path, ck, csrf, args)
	r.ExpireProposal(gone)
	code, body = pressPoolRaw(t, r, path, ck, csrf, gone, "")
	if code != http.StatusNotFound {
		t.Fatalf("the expired proposal's press = %d:\n%s", code, body)
	}
	equal(t, "the writes the expired proposal made", poolWrites(r), 0)

	// the owner's press lands the corrected change: the actor from the
	// session alone, the captured operation, reason and source intact
	r.OnReload(func() { r.SetLoadedPool(12) })
	code, body = pressPoolRaw(t, r, path, ck, csrf, held, `{"correction":{"eur":12}}`)
	if code != http.StatusOK {
		t.Fatalf("the owner's corrected Do it = %d:\n%s", code, body)
	}
	landedPress(t, "the owner's corrected Do it", body, `{"action":"set","eur":12,"reason":"Allow two small verified follow-ups"}`,
		"The pool of "+accProjectName+" is €12.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it.")
	commits := r.InfraCommits()
	equal(t, "the commits", len(commits), 1)
	if !strings.Contains(commits[0].Message, "Requested-by: sebastian\n") {
		t.Errorf("the commit's message = %q", commits[0].Message)
	}
	equal(t, "the commit's change", []string{commits[0].Files[0].Operation, commits[0].Files[0].Content, commits[0].Files[0].SHA},
		[]string{"update", accB64(accPoolLanded), accBlob(accPoolHead)})
	rows := poolChangedRows(r)
	equal(t, "the rows", len(rows), 1)
	equal(t, "the row", []any{rows[0].Actor, rows[0].Detail}, []any{"sebastian", map[string]any{"action": "set", "from_eur": 10.0,
		"confirmed_eur": 12.0, "to_eur": 12.0, "reason": "Allow two small verified follow-ups", "pr": "sf/infra#101"}})
	equal(t, "the writes the owner's press made", poolWrites(r), 3)

	// the consumed proposal replays 404
	code, body = pressPoolRaw(t, r, path, ck, csrf, held, "")
	if code != http.StatusNotFound {
		t.Fatalf("the consumed proposal's replay = %d:\n%s", code, body)
	}
	equal(t, "the writes after the replay", poolWrites(r), 3)
}

// TestPoolPendingAndFailures is what a merged change can still leave:
// a merge Forgejo refused answers 502 with the file and the policy
// standing and the attempt's token spent; a change the loader has not
// activated answers 503 naming the pull request and the pool the policy
// still holds, its row written and the pool line reading the old pool
// until the activation; a change whose pool_changed row the spine refused
// answers the repair that names the pull request and the missing row,
// and neither repeats the change nor undoes its activation.
func TestPoolPendingAndFailures(t *testing.T) {
	r, path, ck, csrf := poolRig(t)
	r.SetLoadedPool(10)
	now := time.Now().UTC()
	seedFundRow(t, r, 8, now) // the month's draws stand at 8
	args := `{"action":"top_up","eur":4,"reason":"a verified finding"}`

	// a merge Forgejo refused: the press answers the real 502, the file
	// and the policy stand, no row was written and the attempt spent its
	// token
	token, _, _ := poolOffer(t, r, path, ck, csrf, args)
	r.RefuseMerge(409, "the branch is protected")
	code, body := pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusBadGateway || strings.TrimSpace(body) != "Forgejo refused to merge the pull request #101; see the factoryd log" {
		t.Fatalf("the refused merge's press = %d:\n%s", code, body)
	}
	equal(t, "main's file after the refused merge", r.InfraFiles()[accInfraFile], accPoolHead)
	loaded, ok := r.LoadedPool()
	equal(t, "the pool in force", []any{ok, loaded}, []any{true, 10.0})
	equal(t, "the rows after the refused merge", len(poolChangedRows(r)), 0)
	equal(t, "the writes the refused merge made", poolWrites(r), 3)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusNotFound {
		t.Fatalf("the refused merge's replay = %d:\n%s", code, body)
	}
	equal(t, "the writes after the replay", poolWrites(r), 3)
	r.RefuseMerge(0, "")

	// a merged change the loader has not activated: the answer names the
	// pull request and the pool the policy still holds, the row is
	// written and the old pool alone stands
	token, _, _ = poolOffer(t, r, path, ck, csrf, args)
	r.OnReload(nil)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusServiceUnavailable || strings.TrimSpace(body) !=
		"The pool change is merged as sf/infra#102, but the policy in force still holds the pool of "+accProjectName+
			" at EUR 10.00: the loader picks the merge up on its next poll. The pool_changed row is written; the new ceiling counts only once the policy holds it." {
		t.Fatalf("the unloaded merge's press = %d:\n%s", code, body)
	}
	equal(t, "main's file after the unloaded merge", r.InfraFiles()[accInfraFile], accPoolTopped4)
	loaded, ok = r.LoadedPool()
	equal(t, "the pool in force still", []any{ok, loaded}, []any{true, 10.0})
	rows := poolChangedRows(r)
	equal(t, "the rows", len(rows), 1)
	equal(t, "the unloaded change's row", []any{rows[0].Actor, rows[0].Detail}, []any{"sebastian", map[string]any{"action": "top_up",
		"from_eur": 10.0, "confirmed_eur": 4.0, "to_eur": 14.0, "reason": "a verified finding", "pr": "sf/infra#102"}})
	equal(t, "the writes so far", poolWrites(r), 6)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusNotFound {
		t.Fatalf("the unloaded merge's replay = %d:\n%s", code, body)
	}
	equal(t, "the writes after the replay", poolWrites(r), 6)

	// the pool line reads the old pool until the loader activates the
	// merged one
	equal(t, "the pool line before the activation", poolLineOf(t, r, now), []any{10.0, 8.0, true, false,
		"0 of 30 sessions, 0 h of 30 h agent time and €8.00 of €10.00 drawn this UTC month"})
	r.SetLoadedPool(14)
	equal(t, "the pool line the activation leaves", poolLineOf(t, r, now), []any{14.0, 8.0, true, false,
		"0 of 30 sessions, 0 h of 30 h agent time and €8.00 of €14.00 drawn this UTC month"})

	// a merged change whose audit row the spine refused: the repair
	// names the pull request and the missing row, the activation stands
	// and neither the row nor a second change is written
	r.FailRecords(errors.New("the spine is down"))
	r.OnReload(func() { r.SetLoadedPool(18) })
	token, _, _ = poolOffer(t, r, path, ck, csrf, args)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusInternalServerError || strings.TrimSpace(body) !=
		`The pool change is merged as sf/infra#103, but its pool_changed row could not be written: the spine is down. The change stands: write the missing row (action top_up, from_eur 14.00, confirmed_eur 4.00, to_eur 18.00, reason "a verified finding") and do not repeat the change.` {
		t.Fatalf("the unwritten row's press = %d:\n%s", code, body)
	}
	equal(t, "main's file after the repair", r.InfraFiles()[accInfraFile], accPool18)
	loaded, ok = r.LoadedPool()
	equal(t, "the activated pool", []any{ok, loaded}, []any{true, 18.0})
	equal(t, "the rows the spine refused", len(poolChangedRows(r)), 1)
	equal(t, "the writes the repair made", poolWrites(r), 9)
	code, body = pressPoolRaw(t, r, path, ck, csrf, token, "")
	if code != http.StatusNotFound {
		t.Fatalf("the repair's replay = %d:\n%s", code, body)
	}
	equal(t, "the writes after the replay", poolWrites(r), 9)
	equal(t, "the file after the replay", r.InfraFiles()[accInfraFile], accPool18)
	r.FailRecords(nil)
}
