package acceptance

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The stream contract of every write chat tool, one table. A write that
// the chat's confirm list names proposes first: its stream holds the
// proposal and nothing ran. Its Do it, or the agent's own call for a tool
// off the list, runs it: the tool block's result is the action's own
// sentence, with no sentence about undoing; a write that took effect sends
// the applied event, which carries an undo token exactly when the action
// has an inverse; the sentence about undoing rides the result the
// conversation stores for the model. The expectations are derived from
// internal/console (chat.go's turn.call and runTool, chat_tools.go, and
// the do* function of each tool), never from a run.

const (
	mayUndo    = " The human may undo it."
	noUndo     = " It cannot be undone."
	taskMissed = "Refused: Project " + accProjectName + " has no card #99: the board read lists its cards."
)

// writeCase is one write tool's call.
type writeCase struct {
	name, tool, args string
	// confirm: the default chat policy names the tool, so the call
	// proposes instead of running.
	confirm bool
	docs    map[string]string
	setup   func(r *AcceptanceRig)
	// result is the tool block's result, applied.result too.
	result string
	// undo: the applied event carries an undo token; undoResult is the
	// undo's own result.
	undo       bool
	undoResult string
	// event is a kind of event row the write records.
	event string
	// calls are the port calls and the Forgejo writes it makes, undoCalls
	// the ones its undo makes.
	calls, undoCalls []string
	// leaks are texts no stream may carry.
	leaks []string
	check func(t *testing.T, r *AcceptanceRig, docs *AccDocs)
}

const docPlan = "# The links plan\n\nThe first draft.\n"

var writeCases = []writeCase{
	{name: "alert_verdict", tool: "alert_verdict", args: `{"id":7,"verdict":"noise"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.ws.seatAlert(factoryd.Alert{ID: 7, Kind: "escalation", TaskID: "t-m"})
		},
		result: "Alert 7 is judged noise.", event: factoryd.EventAlertVerdict, calls: []string{"alert 7 noise by sebastian"}},
	{name: "promote_resource", tool: "promote_resource", args: `{"id":3}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.ws.seatResource(cloudres.Resource{ID: 3, Repo: "sf/code", OwnerRef: "t-m", Kind: "server"})
		},
		result: "Resource 3 is standing now.", event: factoryd.EventResourcePromoted, calls: []string{"promote 3 by sebastian"}},
	{name: "release_resource", tool: "release_resource", args: `{"id":3}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.ws.seatResource(cloudres.Resource{ID: 3, Repo: "sf/code", OwnerRef: "t-m", Kind: "server"})
		},
		result: "Resource 3 is released; the collector deletes it.", event: factoryd.EventResourceReleased, calls: []string{"release 3 by sebastian"}},
	{name: "new_card", tool: "new_card",
		args:    `{"title":"Cache the board","track":"direct","track_reason":"one file","acceptance":["The board is cached"]}`,
		confirm: true,
		result:  "Filed sf/code#51 on track direct.", event: factoryd.EventCardFiled, calls: []string{"POST /api/v1/repos/sf/code/issues"}},
	{name: "request_image", tool: "request_image", args: `{"tool":"jq","need":"the cards parse JSON"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.ws.seatImageCard(factoryd.Task{ID: "t-img", Repository: "sf/code", IssueNumber: 60, HumanSummary: "Add jq to the default image"})
		},
		result: `Filed tool card #60 t-img "Add jq to the default image". After it merges, the factory builds, signs and pins the image; cards can then name it in image.`,
		event:  factoryd.EventCardFiled, calls: []string{"image sf/code jq for the cards parse JSON by sebastian"}},
	{name: "answer", tool: "answer", args: `{"task_id":"t-e","text":"Use a replica."}`, confirm: true,
		result: "Case 1 took " + factoryd.ActContinue + ".", event: factoryd.EventQuestionAnswered,
		calls: []string{"POST /api/v1/repos/sf/code/issues/13/comments"}},
	{name: "comment", tool: "comment", args: `{"task_id":"t-r","text":"Looks right."}`,
		result: "Commented on #11.", undo: true, undoResult: "Deleted the comment on #11.", event: factoryd.EventCardCommented,
		calls: []string{"POST /api/v1/repos/sf/code/issues/11/comments"}, undoCalls: []string{"DELETE /api/v1/repos/sf/code/issues/comments/1"}},
	{name: "tell_card", tool: "tell_card", args: `{"task_id":"t-r","text":"Rebase on main first."}`, confirm: true,
		result: "Told #11 (its session reads the message): The session of #11 has your message.", event: factoryd.EventCardSteered},
	{name: "steer", tool: "steer", args: `{"task_id":"t-r","text":"Rebase on main first."}`, confirm: true,
		result: "The session of #11 has your message.", event: factoryd.EventCardSteered,
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			equal(t, "the text the session was handed", r.Steered(), []string{"t-r sebastian: Rebase on main first."})
		}},
	{name: "retry", tool: "retry", args: `{"task_id":"t-f"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.AddCard(factoryd.Task{ID: "t-f", Kind: "agent", Status: "failed", IssueNumber: 31, Objective: "Add the cache."})
		},
		result: "Retried #31 as #61; it starts once a work slot is free.", event: factoryd.EventCardRetried, calls: []string{"retry t-f by sebastian"}},
	{name: "request_changes", tool: "request_changes", args: `{"task_id":"t-pl","text":"Split it in two."}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.AddCard(planCard()) },
		result: "Your change request is on plan #30; the planner answers a new revision.", event: factoryd.EventPlanChangesRequested,
		calls: []string{"POST /api/v1/repos/sf/code/issues/30/comments"}},
	{name: "approve_merge", tool: "approve_merge", args: `{"task_id":"t-g"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.TellCard("t-g", 15, 12, 6, "Cache the board.", "c0ffee1") },
		result: "Approved pull request #12; factoryd merges it.", event: factoryd.EventMergeApproved,
		calls: []string{"POST /api/v1/repos/sf/code/pulls/12/reviews", "approve t-g pr 12 by sebastian"},
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			rv := r.Reviews()
			if len(rv) != 1 || rv[0]["event"] != "APPROVED" || rv[0]["commit_id"] != "c0ffee1" {
				t.Errorf("the reviews = %v, want one APPROVED at c0ffee1", rv)
			}
		}},
	{name: "deny_merge", tool: "deny_merge", args: `{"task_id":"t-g","note":"Drop the binary.","action":"request_changes"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.TellCard("t-g", 15, 12, 6, "Cache the board.", "c0ffee1") },
		result: "Denied pull request #12 (request changes).", event: factoryd.EventMergeDenied,
		calls: []string{"POST /api/v1/repos/sf/code/pulls/12/reviews"},
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			if q := r.Approval(6); q.Status != "denied" || q.DecisionNote != "changes requested: Drop the binary." {
				t.Errorf("approval request 6 = %+v, want it denied with the change request", q)
			}
		}},
	{name: "deny_merge reject", tool: "deny_merge", args: `{"task_id":"t-g","note":"Not wanted.","action":"reject"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.TellCard("t-g", 15, 12, 6, "Cache the board.", "c0ffee1") },
		result: "Denied pull request #12 (reject).", event: factoryd.EventMergeDenied,
		calls: []string{"POST /api/v1/repos/sf/code/issues/12/comments", "PATCH /api/v1/repos/sf/code/pulls/12"},
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			if q := r.Approval(6); q.Status != "denied" || q.DecisionNote != "rejected: Not wanted." {
				t.Errorf("approval request 6 = %+v, want it denied as rejected", q)
			}
		}},
	{name: "rerun_checks", tool: "rerun_checks", args: `{"task_id":"t-g"}`,
		setup: func(r *AcceptanceRig) {
			r.AddCard(factoryd.Task{ID: "t-g", Kind: "agent", Status: "in-review", Gate: factoryd.GateApproval, IssueNumber: 15, PRNumber: 12})
			r.PendRequest(6, factoryd.ApprovalMerge, "t-g", 12, `{"head_sha":"c0ffee1","factory":true,"red_checks":true}`)
		},
		result: "Running the checks of pull request #12 again; the card asks again once they settle.", event: factoryd.EventMergeDenied,
		calls: []string{"rerun t-g pr 12 by " + factoryd.ChatUser}},
	{name: "answer_criteria", tool: "answer_criteria", args: `{"task_id":"t-pl","action":"grant"}`,
		setup: func(r *AcceptanceRig) {
			r.AddCard(planCard())
			r.PendRequest(8, factoryd.ApprovalCriteria, "t-pl", 0, `{}`)
		},
		result: "Granted one more bounded card for 2 criteria; the planner drafts it and the board shows it.", event: factoryd.EventCriterionDecided,
		calls: []string{"grant t-pl approval 8 by sebastian"}},
	{name: "set_summary", tool: "set_summary", args: `{"task_id":"t-m","summary":"Fix the cache"}`,
		result: "The summary of #5 is set.", undo: true, undoResult: `The summary of #5 is back to "".`, event: factoryd.EventSummarySet},
	{name: "set_accent", tool: "set_accent", args: `{"task_id":"t-m","accent":"blue"}`,
		result: "The accent of #5 is set.", undo: true, undoResult: `The accent of #5 is back to "".`, event: factoryd.EventAccentSet},
	{name: "invite", tool: "invite", args: `{"login":"anna-b"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.SeatFactoryObjects() },
		result: "anna-b has write access; the console admits them within a minute.", event: factoryd.EventMemberInvited,
		calls: []string{"collaborator sf/code anna-b write"}},
	{name: "github_export", tool: "github_export", args: `{"remote":"acme/code"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.SeatFactoryObjects()
			r.ws.seatMirror(factoryd.Mirror{Project: accProjectName, Direction: factoryd.MirrorExport, Username: "gh-user", Token: "ghp_secret1234"})
		},
		result: "GitHub export of " + accProjectName + " to acme/code is set: Forgejo pushes every branch and tag on each commit and every 8 h.",
		event:  factoryd.EventGithubExportSet, leaks: []string{"ghp_secret1234"},
		calls: []string{"push mirror sf/code to " + factoryd.GitHubAddress("acme/code") + " as gh-user",
			"mirror " + accProjectName + " export acme/code mode mirror"}},
	{name: "github_sync", tool: "github_sync", args: `{}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.SeatFactoryObjects()
			r.ws.seatMirror(factoryd.Mirror{Project: accProjectName, Direction: factoryd.MirrorExport, Remote: "acme/code", Username: "gh-user", Token: "ghp_secret1234"})
		},
		result: "Sync from GitHub of " + accProjectName + " started: its result arrives as an event and on the projects page's export row.",
		event:  factoryd.EventGithubSynced, calls: []string{"sync " + accProjectName + " by sebastian"}, leaks: []string{"ghp_secret1234"}},
	{name: "close", tool: "close", args: `{"task_id":"t-m","note":"Not needed."}`, confirm: true,
		result: "Closed #5; factoryd ends the card.", event: factoryd.EventCardClosed,
		calls: []string{"POST /api/v1/repos/sf/code/issues/5/comments", "PATCH /api/v1/repos/sf/code/issues/5"}},
	{name: "edit", tool: "edit", args: `{"task_id":"t-m","title":"Fix the cache and the TTL"}`, confirm: true,
		result: "Edited #5; the card's next session reads the edit.", event: factoryd.EventCardEdited,
		calls: []string{"PATCH /api/v1/repos/sf/code/issues/5"}},
	{name: "grant_session", tool: "grant_session", args: `{"task_id":"t-e","text":"One more session."}`, confirm: true,
		result: "Case 1 took " + factoryd.ActExtend + ".", event: factoryd.EventCardExtended,
		calls: []string{"POST /api/v1/repos/sf/code/issues/13/comments"}},
	{name: "fund", tool: "fund", args: `{"task_id":"t-e","eur":0.5,"text":"Go on."}`, confirm: true,
		result: "Case 1 took " + factoryd.ActFund + ".", event: factoryd.EventCardFunded,
		calls: []string{"POST /api/v1/repos/sf/code/issues/13/comments"}},
	{name: "set_dependency", tool: "set_dependency", args: `{"task_id":"t-a","depends_on":"t-b"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.SeatDependencies()
			r.AddCard(planChild("t-a", 21))
			r.AddCard(planChild("t-b", 22))
		},
		result: "#21 depends on #22 now.", undo: true, undoResult: "#21 no longer depends on #22.", event: factoryd.EventDependencySet,
		calls: []string{"issue dependency sf/code#21 on #22 remove false"}, undoCalls: []string{"issue dependency sf/code#21 on #22 remove true"}},
	{name: "replan", tool: "replan", args: `{"task_id":"t-f","note":"Plan it again."}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.AddCard(factoryd.Task{ID: "t-f", Kind: "agent", Status: "failed", IssueNumber: 31, Objective: "Add the cache."})
		},
		result: "Sent t-f back to the planner.", event: factoryd.EventCardReplanned,
		calls: []string{"replan t-f by sebastian off_main false", "POST /api/v1/repos/sf/code/issues/31/comments"}},
	{name: "set_pool", tool: "set_pool", args: `{"action":"set","eur":12,"reason":"Allow two small follow-ups"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.PutInfraFile(accInfraFile, accPoolHead)
			r.OnReload(func() { r.SetLoadedPool(12) })
		},
		result: "The pool of " + accProjectName + " is €12.00 now, was €10.00: sf/infra#101 carries the change and the policy in force holds it.",
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			equal(t, "the pool file after the change", r.InfraFiles()[accInfraFile], accPoolLanded)
		}},
	{name: "write_doc", tool: "write_doc", confirm: true, docs: map[string]string{"docs/plans/links.md": docPlan},
		args: `{"path":"docs/plans/links.md","old":"The first draft.","new":"The second draft.","base_sha256":"` + factoryd.ContentHash(docPlan) + `"}`,
		result: "Committed c0ffee0 to sf/docs@master: docs/plans/links.md, now base_sha256 " +
			factoryd.ContentHash(strings.Replace(docPlan, "first", "second", 1)) + ".",
		check: func(t *testing.T, _ *AcceptanceRig, docs *AccDocs) {
			equal(t, "the plan after the write", docs.Files()["docs/plans/links.md"], strings.Replace(docPlan, "first", "second", 1))
		}},
	{name: "set_autonomy", tool: "set_autonomy", args: `{"autonomy":"assisted"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.PutInfraFile(accInfraFile, accPoolHead) },
		result: "The project runs at assisted now.", event: factoryd.EventAutonomyChanged,
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			if !strings.Contains(r.InfraFiles()[accInfraFile], "autonomy: assisted") {
				t.Errorf("the project file after the change:\n%s", r.InfraFiles()[accInfraFile])
			}
		}},
	{name: "routine_write", tool: "routine_write", confirm: true,
		args:   `{"name":"nightly","title":"Nightly review","schedule":[{"cron":"0 6 * * *"}],"files":"human"}`,
		setup:  func(r *AcceptanceRig) { r.PutInfraFile(accInfraFile, accPoolHead) },
		result: "Pull request #101 added routine nightly to projects/" + accProjectName + ".yaml. The board shows it once the factory loads the new configuration.",
		event:  factoryd.EventRoutineWritten,
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			if !strings.Contains(r.InfraFiles()[accInfraFile], "name: nightly") {
				t.Errorf("the project file after the change:\n%s", r.InfraFiles()[accInfraFile])
			}
		}},
	{name: "routine_delete", tool: "routine_delete", args: `{"name":"nightly"}`, confirm: true,
		setup: func(r *AcceptanceRig) {
			r.PutInfraFile(accInfraFile, accPoolHead+nightlyYAML)
			r.SeatRoutine(nightly(), "")
		},
		result: "Pull request #101 deleted routine nightly from projects/" + accProjectName + ".yaml.", event: factoryd.EventRoutineDeleted,
		check: func(t *testing.T, r *AcceptanceRig, _ *AccDocs) {
			if strings.Contains(r.InfraFiles()[accInfraFile], "nightly") {
				t.Errorf("the project file after the change:\n%s", r.InfraFiles()[accInfraFile])
			}
		}},
	{name: "routine_run", tool: "routine_run", args: `{"name":"nightly"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.SeatRoutine(nightly(), "Mon 06:00") },
		result: "The last card is still open", event: factoryd.EventRoutineRunNow, calls: []string{"run now " + accProjectName + " nightly"}},
	{name: "routine_skip", tool: "routine_skip", args: `{"name":"nightly"}`, confirm: true,
		setup:  func(r *AcceptanceRig) { r.SeatRoutine(nightly(), "Mon 06:00") },
		result: "Skipped once · next Mon 06:00", event: factoryd.EventRoutineSkipped, calls: []string{"skip " + accProjectName + " nightly true by sebastian"}},
	{name: "notes_write", tool: "notes_write", args: `{"text":"Remember the cache."}`,
		result: "The scratchpad holds 19 of 16384 bytes now.", calls: []string{"notes Remember the cache."}},
	{name: "escalate", tool: "escalate",
		args:   `{"ask":"Move the cache to Redis?","why":"The card needs a person's call.","info":"It costs 20 EUR a month.","recommendation":"Keep the local cache.","choices":["Move it","Keep it"]}`,
		result: "Filed the escalation as #70 in Needs you; the inbox carries it to the project's humans with its choices.",
		calls:  []string{`escalate sf/code "Move the cache to Redis?" by sebastian`}},
	{name: "attach_analysis", tool: "attach_analysis", args: `{"task_id":"t-e","found":"The replica lags 2 s.","recommendation":"Use it."}`,
		setup:  func(r *AcceptanceRig) { r.SeatAsk("t-e", "Use a replica?") },
		result: "Recorded the analysis inside the ask of #13; the humans read it with the ask.", event: factoryd.EventAnalysisAttached},
}

func planCard() factoryd.Task {
	return factoryd.Task{ID: "t-pl", Kind: "agent", Status: "waiting-human", Track: policy.TrackPlan, Process: "maintenance",
		IssueNumber: 30, Objective: "Plan the links."}
}

func planChild(id string, issue int64) factoryd.Task {
	return factoryd.Task{ID: id, Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "maintenance",
		IssueNumber: issue, Objective: "Add " + id + ".", Source: factoryd.SourcePlan, Parent: "t-pl"}
}

const nightlyYAML = "routines:\n  - name: nightly\n    title: Nightly review\n    schedule:\n      - cron: \"0 6 * * *\"\n    files: human\n"

func nightly() policy.Routine {
	return policy.Routine{Name: "nightly", Title: "Nightly review", Schedule: []policy.Trigger{{Cron: "0 6 * * *"}}, Files: policy.FilesHuman}
}

// sseEvent is one event of a turn's stream.
type sseEvent struct {
	Kind string
	Data map[string]any
}

func streamOf(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, e := range eventsOf(t, body) {
		kind, data, _ := strings.Cut(e, " ")
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("the %s event %q: %v", kind, data, err)
		}
		out = append(out, sseEvent{kind, m})
	}
	return out
}

func kindsOf(events []sseEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// only answers the one event of kind, failing on none or a second.
func only(t *testing.T, events []sseEvent, kind string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, e := range events {
		if e.Kind == kind {
			found = append(found, e.Data)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the stream holds %d %s events, want one: %v", len(found), kind, kindsOf(events))
	}
	return found[0]
}

func effects(r *AcceptanceRig) []string { return append(r.Writes(), r.Calls()...) }

// within polls until ok, for a record an action makes after it answered.
func within(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
	}
}

// TestWriteToolsStream runs every write tool through the chat: a tool on
// the confirm list proposes and runs on its Do it, any other runs in the
// turn itself.
func TestWriteToolsStream(t *testing.T) {
	for _, c := range writeCases {
		t.Run(c.name, func(t *testing.T) {
			r, docs, path := NewAcceptanceRig(t, true, c.docs)
			ck, csrf := r.Login(t)
			if c.setup != nil {
				c.setup(r)
			}
			wroteBefore := len(effects(r))
			r.Reply([]string{Call(1, c.tool, c.args)}, []string{Say("Done.")})
			code, body := postChat(t, r, path, ck, url.Values{"message": {"do it"}, "csrf": {csrf}})
			if code != http.StatusOK {
				t.Fatalf("POST the chat = %d:\n%s", code, body)
			}
			stream := streamOf(t, body)
			var did []sseEvent
			var doneBody string
			if c.confirm {
				equal(t, "the offer's events", kindsOf(stream), []string{"proposal", "done"})
				p := stream[0].Data
				equal(t, "the proposal's tool", p["tool"], c.tool)
				equal(t, "what the offer wrote", len(effects(r)), wroteBefore)
				equal(t, "the event rows the offer wrote", len(r.Events()), 0)
				token, _ := p["id"].(string)
				code, doneBody = postChat(t, r, path+"/proposals/"+token, ck, url.Values{"csrf": {csrf}})
				if code != http.StatusOK {
					t.Fatalf("Do it = %d:\n%s", code, doneBody)
				}
				did = streamOf(t, doneBody)
				if got := kindsOf(did); !slices.Equal(got, []string{"tool", "applied", "done"}) {
					t.Fatalf("the Do it's events = %v, want a tool block, an applied event and done:\n%s", got, doneBody)
				}
			} else {
				doneBody = body
				did = stream
				if k := kindsOf(did); slices.Contains(k, "proposal") || !slices.Contains(k, "tool") || !slices.Contains(k, "applied") || k[len(k)-1] != "done" {
					t.Fatalf("the turn's events = %v, want a tool block and an applied event with no proposal", k)
				}
			}

			block := only(t, did, "tool")
			equal(t, "the tool block's name", block["name"], c.tool)
			equal(t, "the tool block's result", block["result"], c.result)
			applied := only(t, did, "applied")
			equal(t, "the applied event's tool", applied["tool"], c.tool)
			equal(t, "the applied event's result", applied["result"], c.result)
			token, hasUndo := applied["undo"].(string)
			if hasUndo != c.undo || c.undo && token == "" {
				t.Errorf("the applied event's undo = %q, present %t, want present %t", token, hasUndo, c.undo)
			}
			for _, sentence := range []string{mayUndo, noUndo} {
				if strings.Contains(block["result"].(string), strings.TrimSpace(sentence)) {
					t.Errorf("the tool block's result carries the undo sentence %q", sentence)
				}
			}
			for _, leak := range c.leaks {
				if strings.Contains(body+doneBody, leak) {
					t.Errorf("the stream carries %q", leak)
				}
			}

			wantStored := c.result + noUndo
			if c.undo {
				wantStored = c.result + mayUndo
			}
			var stored []string
			for _, call := range r.ToolCalls(repository, login) {
				if call.Name == c.tool {
					stored = append(stored, call.Result)
				}
			}
			if len(stored) == 0 || stored[len(stored)-1] != wantStored {
				t.Errorf("the conversation's stored results of %s = %q, want the last to be %q", c.tool, stored, wantStored)
			}

			for _, want := range c.calls {
				within(t, "the call "+want, func() bool { return slices.Contains(effects(r), want) })
			}
			if c.event != "" {
				within(t, "the event row "+c.event, func() bool {
					return slices.ContainsFunc(r.Events(), func(e factoryd.Event) bool { return e.Kind == c.event })
				})
			}
			if c.check != nil {
				c.check(t, r, docs)
			}

			if !c.undo {
				return
			}
			code, undone := postChat(t, r, path+"/undo/"+token, ck, url.Values{"csrf": {csrf}})
			if code != http.StatusOK {
				t.Fatalf("Undo = %d:\n%s", code, undone)
			}
			ustream := streamOf(t, undone)
			equal(t, "the undo's events", kindsOf(ustream), []string{"tool", "applied", "done"})
			ub := only(t, ustream, "tool")
			equal(t, "the undo block's name", ub["name"], "undo "+c.tool)
			equal(t, "the undo block's result", ub["result"], c.undoResult)
			ua := only(t, ustream, "applied")
			equal(t, "the undo's applied event", ua, map[string]any{"tool": c.tool, "result": c.undoResult})
			for _, want := range c.undoCalls {
				within(t, "the undo's call "+want, func() bool { return slices.Contains(effects(r), want) })
			}
		})
	}
}

// TestWriteToolsRefuse has each write tool that names a card meet a card
// that is not there: a confirm-listed tool does not propose, no tool sends
// an applied event, and the tool block carries the refusal.
func TestWriteToolsRefuse(t *testing.T) {
	for _, c := range writeCases {
		if !strings.Contains(c.args, `"task_id"`) {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			r, _, path := NewAcceptanceRig(t, true, nil)
			ck, csrf := r.Login(t)
			args := strings.Replace(c.args, `"task_id":"`+firstTask(c.args)+`"`, `"task_id":"#99"`, 1)
			r.Reply([]string{Call(1, c.tool, args)}, []string{Say("Done.")})
			code, body := postChat(t, r, path, ck, url.Values{"message": {"do it"}, "csrf": {csrf}})
			if code != http.StatusOK {
				t.Fatalf("POST the chat = %d:\n%s", code, body)
			}
			stream := streamOf(t, body)
			if k := kindsOf(stream); slices.Contains(k, "proposal") || slices.Contains(k, "applied") {
				t.Fatalf("the events = %v, want no proposal and no applied", k)
			}
			block := only(t, stream, "tool")
			equal(t, "the tool block's name", block["name"], c.tool)
			equal(t, "the tool block's result", block["result"], taskMissed)
			equal(t, "the event rows", len(r.Events()), 0)
			equal(t, "the writes", len(effects(r)), 0)
		})
	}
}

func firstTask(args string) string {
	var in struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		panic(err)
	}
	return in.TaskID
}

// writeTools are the console's write tools: every tool of its table that
// writes, but the undo's own delete_comment, which no agent is offered.
// The tools of the issue that asked for this suite that the console no
// longer has are moved apart.
var (
	writeTools = []string{"alert_verdict", "promote_resource", "release_resource", "new_card", "request_image", "answer", "comment",
		"tell_card", "steer", "retry", "request_changes", "approve_merge", "deny_merge", "rerun_checks", "answer_criteria",
		"set_summary", "set_accent", "invite", "github_export", "github_sync", "close", "edit", "grant_session", "fund",
		"set_dependency", "replan", "set_pool", "write_doc", "set_autonomy", "routine_write", "routine_delete", "routine_run",
		"routine_skip", "notes_write", "escalate", "attach_analysis"}
	// readTools are the tools that change nothing; with writeTools they
	// are every tool the policy names.
	readTools = []string{"system", "board", "card", "history", "card_history", "card_step", "board_digest", "costs", "diff", "plan", "pr",
		"file", "tree", "grep", "web_search", "web_scrape", "research", "session", "session_steps", "session_step", "session_search", "ci",
		"alerts", "deployment_health", "resources", "settings", "policy", "project_images", "read_doc", "show_doc", "routines",
		"notes_read", "active_threads", "chat_search", "inbox", "board_view", "board_view_reset", "watch_card", "unwatch_card",
		"set_timer", "analyze_image", "read_attachment", "summarize_attachment", "ask"}
	removedTools = []string{"move_card", "start_plan", "accept_plan", "reject_plan"}
)

// TestWriteToolsTable keeps the table whole: every write tool has a case,
// every case's tool is one the policy names, and the confirm flag of each
// is what the default chat policy says.
func TestWriteToolsTable(t *testing.T) {
	confirm := policy.ConfirmFor("", policy.MergeHuman, policy.DefaultChat.Confirm)
	cased := map[string]bool{}
	for _, c := range writeCases {
		cased[c.tool] = true
		if !slices.Contains(policy.ChatTools, c.tool) {
			t.Errorf("case %s: the policy names no tool %s", c.name, c.tool)
		}
		if got := slices.Contains(confirm, c.tool); got != c.confirm {
			t.Errorf("case %s: confirm = %t, the default policy says %t", c.name, c.confirm, got)
		}
	}
	for _, tool := range writeTools {
		if !cased[tool] {
			t.Errorf("write tool %s has no case", tool)
		}
	}
	classed := slices.Sorted(slices.Values(slices.Concat(writeTools, readTools)))
	if named := slices.Sorted(slices.Values(policy.ChatTools)); !slices.Equal(classed, named) {
		t.Errorf("the policy's tools and the suite's write and read lists differ: class each new tool, and give a write tool a case\nonly in the policy: %v\nonly in the lists: %v",
			slices.DeleteFunc(slices.Clone(named), func(n string) bool { return slices.Contains(classed, n) }),
			slices.DeleteFunc(slices.Clone(classed), func(n string) bool { return slices.Contains(named, n) }))
	}
	for _, tool := range removedTools {
		if slices.Contains(policy.ChatTools, tool) || cased[tool] {
			t.Errorf("tool %s is back: give it a case", tool)
		}
	}
}
