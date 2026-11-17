package console

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// acceptedBrief and liveBrief are factoryd's: the brief the door accepts
// and card #2100's live title under the accepted why line and summary.
var (
	acceptedBrief = factoryd.BriefText{Title: "Narrow the close refusal to plans that hold criteria", Why: "So a plan with no criteria closes without a waiver.",
		Summary: "The console refuses to close a plan while criteria are open. This card limits the refusal to plans that hold criteria. A test closes a plan with none and one with two."}
	liveBrief = factoryd.BriefText{Title: "The doClose refusal narrowed to plans that hold criteria: the review finding answered on branch card/2084",
		Why: acceptedBrief.Why, Summary: acceptedBrief.Summary}
)

// anyRef resolves every reference: the briefs here name none.
type anyRef struct{}

func (anyRef) Issue(int) (string, string, bool) { return "", "", true }
func (anyRef) Files(path string) []string       { return []string{path} }
func (anyRef) Heading(string, string) bool      { return true }
func (anyRef) Err() error                       { return nil }

// carriesCheck fails t unless refusal carries every line CheckBrief writes
// for b as job.
func carriesCheck(t *testing.T, refusal string, b factoryd.BriefText, job factoryd.BriefJob) {
	t.Helper()
	lines := factoryd.CheckBrief(b, job, anyRef{})
	if len(lines) == 0 {
		t.Fatalf("CheckBrief passes %+v", b)
	}
	for _, l := range lines {
		if !strings.Contains(refusal, l) {
			t.Errorf("the refusal %q lacks %q", refusal, l)
		}
	}
}

// agentCalls has the agent make calls, each tool and its args, in one
// model response of a turn of path's chat (agentRounds).
func agentCalls(t *testing.T, r *rig, g *chatGateway, path string, calls ...string) ([]string, []string) {
	t.Helper()
	return agentRounds(t, r, g, path, calls)
}

// agentRounds has the agent answer each round's calls in a model response
// of its own, in one turn of path's chat: the result of each call as the
// agent read it, "" for a call that proposed, and the proposals' tokens.
func agentRounds(t *testing.T, r *rig, g *chatGateway, path string, rounds ...[]string) ([]string, []string) {
	t.Helper()
	ck, s := r.loggedIn(t)
	g.replies = nil
	for first, round := 1, 0; round < len(rounds); first, round = first+len(rounds[round])/2, round+1 {
		g.replies = append(g.replies, []string{call(first, rounds[round]...)})
	}
	g.replies = append(g.replies, []string{say("ok")})
	_, events, tokens := chatPost(t, r, ck, s, path, url.Values{"message": {"file it"}})
	var results []string
	for _, e := range events {
		if rest, ok := strings.CutPrefix(e, "tool "); ok {
			var v struct{ Result string }
			if err := json.Unmarshal([]byte(rest), &v); err != nil {
				t.Fatal(err)
			}
			results = append(results, v.Result)
		}
		if strings.HasPrefix(e, "proposal ") {
			results = append(results, "")
		}
	}
	return results, tokens
}

// doIt presses Do it on the proposal of token in path's chat.
func doIt(t *testing.T, r *rig, path, token string) []string {
	t.Helper()
	ck, s := r.loggedIn(t)
	_, done, _ := chatPost(t, r, ck, s, path+"/proposals/"+token, nil)
	return done
}

// jsonOf is v as JSON.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// filedBrief is the why line and the summary of the issue the rig filed
// last, as the intake reads them back.
func filedBrief(r *rig) (string, string) {
	body, _ := r.fg.issues[len(r.fg.issues)-1]["body"].(string)
	why, summary, _ := factoryd.ParseIssueText(body)
	return why, summary
}

// Every filing door of the console checks the brief its writer wrote: the
// live title of card #2100 comes back in the door's own channel with
// CheckBrief's words, and the accepted brief files, its issue opening
// with the why line and the summary the intake stores.
func TestEveryFilingDoorChecksTheBrief(t *testing.T) {
	type door struct {
		name string
		job  factoryd.BriefJob
		// file answers the refusal the writer read, else the why line
		// and the summary the filing stored
		file func(t *testing.T, b factoryd.BriefText) (refusal, why, summary string)
	}
	chatTool := func(tool string, args func(factoryd.BriefText) map[string]any) func(t *testing.T, b factoryd.BriefText) (string, string, string) {
		return func(t *testing.T, b factoryd.BriefText) (string, string, string) {
			r, g := newWritesRig(t)
			results, tokens := agentCalls(t, r, g, chatPath, tool, jsonOf(t, args(b)))
			if results[0] != "" {
				return results[0], "", ""
			}
			if done := doIt(t, r, chatPath, tokens[0]); len(r.fg.issues) == 0 {
				t.Fatalf("Do it filed no issue: %q", done)
			}
			why, summary := filedBrief(r)
			return "", why, summary
		}
	}
	newCard := func(b factoryd.BriefText) map[string]any {
		return map[string]any{"title": b.Title, "why": b.Why, "summary": b.Summary, "body": "Count the refusals.", "acceptance": []string{"A plan with none closes"},
			"track": "direct", "track_reason": "one fix", "size": "small"}
	}
	for _, d := range []door{
		{"new_card", factoryd.BriefWork, chatTool("new_card", newCard)},
		{"new_card on a case, its file_card", factoryd.BriefWork, chatTool("new_card", func(b factoryd.BriefText) map[string]any {
			args := newCard(b)
			args["task_id"] = "t-cap"
			return args
		})},
		{"answer's file_card", factoryd.BriefWork, chatTool("answer", func(b factoryd.BriefText) map[string]any {
			return map[string]any{"task_id": "t-cap", "action": factoryd.ActFileCard, "text": "the cause is elsewhere",
				"args": map[string]any{"title": b.Title, "why": b.Why, "summary": b.Summary, "body": "Count the refusals.\n\nAcceptance:\n- A plan with none closes"}}
		})},
		{"chat escalate", factoryd.BriefAsk, func(t *testing.T, b factoryd.BriefText) (string, string, string) {
			r, g := newWritesRig(t)
			var filed []factoryd.HumanQuestion
			r.c.cfg.Escalate = func(ctx context.Context, _ string, q factoryd.HumanQuestion, _ string) (int64, error) {
				if _, refused, _ := factoryd.AdmitBrief(factoryd.BriefText{Title: q.Ask, Why: q.Why}, factoryd.BriefAsk, anyRef{}, factoryd.BriefTry(ctx)); refused != nil {
					return 0, &factoryd.BriefRefusal{Lines: refused}
				}
				filed = append(filed, q)
				return 42, nil
			}
			results, _ := agentCalls(t, r, g, chatPath, "escalate", jsonOf(t, map[string]any{"ask": b.Title, "why": b.Why, "info": "i", "recommendation": "r",
				"choices": []string{"Narrow it", "Leave it"}}))
			if len(filed) == 0 {
				return results[0], "", ""
			}
			return "", filed[0].Why, ""
		}},
		{"routine file", factoryd.BriefWork, func(t *testing.T, b factoryd.BriefText) (string, string, string) {
			r, g, _, _ := routineRig(t)
			results, _ := agentCalls(t, r, g, "/projects/demo-app/chat", "routine_write", jsonOf(t, map[string]any{"name": "close-check", "title": b.Title,
				"schedule": []map[string]string{{"cron": "0 6 * * 4"}}, "files": "agent", "skill": "deps"}))
			routine := factoryd.SourceBriefs[factoryd.SourceRoutine](factoryd.BriefFacts{Name: b.Title, Of: "close-check"})
			if results[0] != "" {
				return results[0], "", ""
			}
			return "", routine.Why, routine.Summary
		}},
		{"New task form", factoryd.BriefHumanIssue, func(t *testing.T, b factoryd.BriefText) (string, string, string) {
			r, _ := newWritesRig(t)
			ck, s := r.loggedIn(t)
			rec := r.do(post("/tasks", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "title": {b.Title}, "why": {b.Why},
				"summary": {b.Summary}, "body": {"Count the refusals."}, "track": {"direct"}}), ck)
			if rec.Code == http.StatusBadRequest {
				return html.UnescapeString(rec.Body.String()), "", ""
			}
			why, summary := filedBrief(r)
			return "", why, summary
		}},
	} {
		t.Run(d.name, func(t *testing.T) {
			live := liveBrief
			if d.name == "routine file" {
				live = factoryd.SourceBriefs[factoryd.SourceRoutine](factoryd.BriefFacts{Name: liveBrief.Title, Of: "close-check"})
			}
			refusal, _, _ := d.file(t, liveBrief)
			carriesCheck(t, refusal, live, d.job)

			refusal, why, summary := d.file(t, acceptedBrief)
			want := [2]string{acceptedBrief.Why, acceptedBrief.Summary}
			switch d.name {
			case "chat escalate":
				want[1] = ""
			case "routine file":
				routine := factoryd.SourceBriefs[factoryd.SourceRoutine](factoryd.BriefFacts{Name: acceptedBrief.Title, Of: "close-check"})
				want = [2]string{routine.Why, routine.Summary}
			}
			if refusal != "" || [2]string{why, summary} != want {
				t.Fatalf("the accepted brief: refusal %q, stored %q, %q; want %q", refusal, why, summary, want)
			}
		})
	}
}

// liveCard is new_card's args of the live title, with body as its text.
func liveCard(t *testing.T, body string) string {
	return jsonOf(t, map[string]any{"title": liveBrief.Title, "why": liveBrief.Why, "summary": liveBrief.Summary, "body": body,
		"acceptance": []string{"A plan with none closes"}, "track": "direct", "track_reason": "one fix", "size": "small"})
}

// An agent the door refuses in three model responses of one turn files
// its last text on the third, its card_filed row marked unchecked; the
// next turn's filing counts from one again.
func TestTheThirdRefusedNewCardFilesMarkedUnchecked(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.chat = &policy.ChatConfig{Confirm: []string{}}
	args := liveCard(t, "Count the refusals.")
	before := r.events.count()
	results, _ := agentRounds(t, r, g, chatPath, []string{"new_card", args}, []string{"new_card", args}, []string{"new_card", args})
	if len(results) != 3 || !strings.HasPrefix(results[0], "Refused: ") || !strings.HasPrefix(results[1], "Refused: ") || results[2] != "Filed sf/code#41 on track direct." {
		t.Fatalf("the three tries read %q", results)
	}
	rows := r.events.all()[before:]
	if len(rows) != 1 || rows[0].Kind != factoryd.EventCardFiled || rows[0].Detail["brief_unchecked"] != true {
		t.Fatalf("rows %+v, want one card_filed row marked unchecked", rows)
	}
	if body, _ := r.fg.issues[0]["body"].(string); !strings.HasPrefix(body, "Why: "+liveBrief.Why+"\n\nSummary: ") {
		t.Fatalf("the issue = %q, want it opening with the why line", body)
	}
	if results, _ = agentCalls(t, r, g, chatPath, "new_card", args); !strings.HasPrefix(results[0], "Refused: ") {
		t.Fatalf("the next turn's first try read %q", results[0])
	}
}

// A restarted plan files its plan anew under the old plan's why line and
// summary, its text opening with the line it continues.
func TestRestartedPlanCarriesTheBrief(t *testing.T) {
	r, _ := newWritesRig(t)
	withLinkshort(r)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-br", Kind: "agent", Status: "done", Track: policy.TrackPlan,
		Repository: "linkshort/linkshort", IssueNumber: 30, Objective: "Build the stats\n\nCount the hits.", Why: acceptedBrief.Why, Summary: acceptedBrief.Summary}})
	r.tasks.mu.Unlock()
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, bool) error { return nil }
	r.c.cfg.CloseHeldPlan = func(_ context.Context, _, by, note string, _ bool) (int, string, bool, error) {
		return 0, factoryd.PlanCloseWords(by, note, false), false, nil
	}
	_, s := r.loggedIn(t)
	plan, _, _ := r.tasks.Task(t.Context(), "t-br")
	if _, err := r.c.restartPlan(t.Context(), actor{s: s, via: factoryd.ViaButton}, plan, "split it in two"); err != nil {
		t.Fatal(err)
	}
	body, _ := r.fg.issues[0]["body"].(string)
	why, summary, text := factoryd.ParseIssueText(body)
	if r.fg.issues[0]["title"] != "Build the stats" || why != acceptedBrief.Why || summary != acceptedBrief.Summary || !strings.HasPrefix(text, "Continues #30.\n\nCount the hits.") {
		t.Fatalf("the new plan's issue = %v", r.fg.issues[0])
	}
}

// The calls of one model response share a try: three different cards the
// door refuses in one response are each refused, and none files.
func TestOneResponsesFilingsShareATry(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.chat = &policy.ChatConfig{Confirm: []string{}}
	results, _ := agentCalls(t, r, g, chatPath, "new_card", liveCard(t, "Count the refusals."), "new_card", liveCard(t, "Count the closes."),
		"new_card", liveCard(t, "Count the waivers."))
	for i, res := range results {
		if !strings.HasPrefix(res, "Refused: ") {
			t.Errorf("card %d read %q, want the refusal", i+1, res)
		}
	}
	if len(results) != 3 || len(r.fg.issues) != 0 {
		t.Fatalf("results %q, issues %v; want three refusals and no issue", results, r.fg.issues)
	}
}

// A human's file_card on the case form files as the New task form does: a
// face that offers it renders its file-as-written box, a brief the door
// refuses comes back with the way to file it as written,
// and with file as written ticked it files, marked unchecked. A title no
// issue can carry is refused for what it is, ticked or not, with no word
// of the box.
func TestCaseFormFilesAsWritten(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("the findings repeat", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseFindingsRepeated, roomyFacts(nil))
	fileCard, _ := factoryd.AnswerOf(factoryd.ActFileCard)
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"ask": "Go on?", "story": factoryd.Story{},
		"offers": []factoryd.AskOffer{{Value: factoryd.ActFileCard, Label: fileCard.Label, Effect: fileCard.Effect}}}}}
	if !strings.Contains(escalationOf(t, r, ck, "t-g"), `<input type="checkbox" name="file_card.as_written" value="yes">`) {
		t.Fatalf("the case form offers no file as written:\n%s", escalationOf(t, r, ck, "t-g"))
	}
	form := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "case_id": {"1"}, "action": {"file_card"},
		"file_card.title": {liveBrief.Title}, "file_card.why": {liveBrief.Why}, "file_card.summary": {liveBrief.Summary}, "file_card.body": {"Count the refusals."}}
	rec := r.do(post("/tasks/t-g/resolve", form), ck)
	refusal := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(refusal, "tick file as written and press File card again") || len(r.fg.issues) != 0 {
		t.Fatalf("a refused brief = %d %q, issues %v", rec.Code, refusal, r.fg.issues)
	}
	carriesCheck(t, refusal, factoryd.BriefText{Title: liveBrief.Title, Why: liveBrief.Why, Summary: liveBrief.Summary,
		Body: "Count the refusals.\n\nFiled from #21, case 1 (" + string(factoryd.CaseFindingsRepeated) + ")."}, factoryd.BriefHumanIssue)
	for _, ticked := range []string{"", "yes"} {
		long := url.Values{}
		for k, v := range form {
			long[k] = v
		}
		long.Set("file_card.title", strings.Repeat("a", 256))
		long.Set("file_card.as_written", ticked)
		rec := r.do(post("/tasks/t-g/resolve", long), ck)
		if body := rec.Body.String(); rec.Code != http.StatusBadRequest || !strings.Contains(body, "a title of 1 to 255 characters is required") ||
			strings.Contains(body, "file as written") || len(r.fg.issues) != 0 {
			t.Fatalf("a 256-character title, ticked %q = %d %q, issues %v", ticked, rec.Code, body, r.fg.issues)
		}
	}
	form.Set("file_card.as_written", "yes")
	before := r.events.count()
	if rec := r.do(post("/tasks/t-g/resolve", form), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("file as written = %d %q", rec.Code, rec.Body)
	}
	rows := r.events.all()[before:]
	if len(r.fg.issues) != 1 || !slices.ContainsFunc(rows, func(e factoryd.Event) bool {
		return e.Kind == factoryd.EventCardFiled && e.Detail["brief_unchecked"] == true
	}) {
		t.Fatalf("issues %v, rows %+v; want the card filed marked unchecked", r.fg.issues, rows)
	}
	// the agent's file_card passes the door on its own tries, ticked or not
	args := jsonOf(t, map[string]any{"title": liveBrief.Title, "why": liveBrief.Why, "summary": liveBrief.Summary, "body": "Count the refusals.",
		factoryd.AsWritten: "yes"})
	if code, msg := refused(t, r, "answer", toolInput{TaskID: "t-g", Action: factoryd.ActFileCard, Text: "the cause", Args: json.RawMessage(args)}); code != http.StatusBadRequest ||
		!strings.Contains(msg, "The title has 105 characters.") {
		t.Fatalf("the agent's file_card with as_written = %d %q, want the door's refusal", code, msg)
	}
}

// A human's Do it of the project agent's proposal posts the card under the
// human's own account, yet the card stays the agent's: its card_filed row
// names the chat as the writer and carries the door's mark (the documents
// could not be read at the Do it, so it files unchecked), and the card the
// intake may have made before the row is marked from it.
func TestAConfirmedChatCardStaysTheAgents(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
	f := withDocs(t, r, map[string]string{"docs/plan.md": "# The plan\n"})
	ck, s := r.loggedIn(t)
	args := `{"title":"Collapse the lanes","why":"The board needs it.","summary":"It builds what the title names. A test checks it.","body":"Builds [the plan](docs/plan.md).","acceptance":["The board shows no lanes"],"track":"direct","track_reason":"the plan is written","size":"small"}`
	g.replies = [][]string{{call(1, "new_card", args)}}
	_, proposed, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"collapse the lanes"}})
	if len(tokens) != 1 {
		t.Fatalf("new_card proposed %v", proposed)
	}
	f.failTree = true
	before := r.events.count()
	_, done, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	equal(t, "the human's Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	var filed []factoryd.Event
	for _, e := range r.events.all()[before:] {
		if e.Kind == factoryd.EventCardFiled {
			filed = append(filed, e)
		}
	}
	if len(filed) != 1 || filed[0].Via != factoryd.ViaChat || filed[0].Actor != s.user || filed[0].Detail["brief_unchecked"] != true {
		t.Fatalf("card_filed rows %+v, want one of the chat for %s, marked unchecked", filed, s.user)
	}
	r.fg.mu.Lock()
	calls := slices.Clone(r.fg.calls)
	r.fg.mu.Unlock()
	if !slices.Contains(calls, "POST /api/v1/repos/sf/code/issues Bearer "+s.access) {
		t.Errorf("Forgejo calls %v, want the issue posted with the human's own session", calls)
	}
	equal(t, "the cards marked", r.tasks.marked, []string{"sf/code#41 The board needs it."})
}

// The New task form mirrors each shape of its brief on the issue, and the
// mirror reads back as the form's why line, summary and text: a summary
// without a why line included. The intake stores what it reads
// (factoryd's TestIntakeStoresEveryBriefShape).
func TestNewTaskMirrorsEveryBriefShape(t *testing.T) {
	const text = "The cache drops entries on reload."
	for _, c := range []struct{ why, summary string }{
		{"So the board loads.", "It caches. A test reads it."}, {"So the board loads.", ""}, {"", "It caches. A test reads it."}, {"", ""},
	} {
		r, _ := newRigWithEvents(t)
		ck, s := r.loggedIn(t)
		form := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "title": {"Keep the cache"}, "why": {c.why},
			"summary": {c.summary}, "body": {text}, "track": {"human"}}
		if rec := r.do(post("/tasks", form), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("why %q, summary %q: the form = %d: %s", c.why, c.summary, rec.Code, rec.Body)
		}
		if len(r.fg.issues) != 1 {
			t.Fatalf("issues %v, want one", r.fg.issues)
		}
		body, _ := r.fg.issues[0]["body"].(string)
		if why, summary, got := factoryd.ParseIssueText(body); why != c.why || summary != c.summary || got != text {
			t.Errorf("the issue %q reads back as %q, %q, %q; want %q, %q, %q", body, why, summary, got, c.why, c.summary, text)
		}
	}
}

// A human's text stays as written at every door a human files or edits
// through, as at the webhook and the task API: the New task form, the
// case form's file_card and the edit of a human's issue take a text that
// names no card (#999) at the first try, unrefused and unmarked.
func TestAHumansTextPassesEveryHumanDoor(t *testing.T) {
	const text = "Like #999 did."
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	form := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "title": {"Keep the cache"}, "body": {text}, "track": {"human"}}
	if rec := r.do(post("/tasks", form), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the New task form = %d: %s", rec.Code, rec.Body)
	}
	r.tasks.set("the findings repeat", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseFindingsRepeated, roomyFacts(nil))
	cased := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "case_id": {"1"}, "action": {"file_card"},
		"file_card.title": {"Keep the cache"}, "file_card.body": {text}}
	if rec := r.do(post("/tasks/t-g/resolve", cased), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the case form's file_card = %d: %s", rec.Code, rec.Body)
	}
	for _, e := range ev.all() {
		if e.Kind == factoryd.EventCardFiled && e.Detail["brief_unchecked"] != nil {
			t.Errorf("a human's filing marked unchecked: %+v", e)
		}
	}

	w, _ := newWritesRig(t)
	w.tasks.set("", factoryd.Task{ID: "t-h", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Repository: "sf/code", IssueNumber: 5,
		Source: factoryd.SourceWebhook, Objective: "Fix the cache\n\nIt drops entries."})
	_, ws := w.loggedIn(t)
	if note, err := w.c.doEdit(t.Context(), actor{s: ws, via: factoryd.ViaButton}, "t-h", factoryd.BriefText{Body: text}); err != nil {
		t.Fatalf("the edit of a human's text = %q, %v", note, err)
	}
}
