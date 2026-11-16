package console

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
func (anyRef) Heading(string, string) bool      { return true }

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

// agentCalls has the agent call tool with args in one turn of path's
// chat: the result of each call as the agent read it, "" for a call that
// proposed, and the proposals' tokens.
func agentCalls(t *testing.T, r *rig, g *chatGateway, path string, calls ...string) ([]string, []string) {
	t.Helper()
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, calls...)}, {say("ok")}}
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
				if ok, lines := factoryd.FileBrief(factoryd.BriefText{Title: q.Ask, Why: q.Why}, factoryd.BriefAsk, anyRef{}, factoryd.BriefTry(ctx)); !ok {
					return 0, &factoryd.BriefRefusal{Lines: lines}
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

// An agent the door refuses three times in one turn files its last text
// on the third try, its card_filed row marked unchecked; the next turn's
// filing counts from one again.
func TestTheThirdRefusedNewCardFilesMarkedUnchecked(t *testing.T) {
	r, g := newWritesRig(t)
	r.ledger.chat = &policy.ChatConfig{Confirm: []string{}}
	args := jsonOf(t, map[string]any{"title": liveBrief.Title, "why": liveBrief.Why, "summary": liveBrief.Summary, "body": "Count the refusals.",
		"acceptance": []string{"A plan with none closes"}, "track": "direct", "track_reason": "one fix", "size": "small"})
	before := r.events.count()
	results, _ := agentCalls(t, r, g, chatPath, "new_card", args, "new_card", args, "new_card", args)
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
