package factoryd

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Each poke class's row, hand-derived from the event and its card
// (roadmap 239, C14's inbox): an ask is the card, its why line and its
// suggestion, a notice one line.
func TestPokeOf(t *testing.T) {
	card := Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Objective: "Session store: pick the table's store\n\nAcceptance:\n- one store", SpentEUR: 0.214, Track: "direct"}
	child := card
	child.Parent = "t-plan"
	plan := Task{ID: "t-plan", Kind: "agent", IssueNumber: 558, Objective: "Members page", Track: "plan"}
	held := Task{ID: "t-h", Kind: "human", Status: "waiting-agent", Objective: "main is still red", Source: SourceStillRed}
	faced := held
	faced.Status, faced.IssueNumber = "waiting-human", 70
	agentDid := func(kind, level string, more map[string]any) Event {
		d := map[string]any{"autonomy": level, "merge_rule": "human", "decider": "agent"}
		for k, v := range more {
			d[k] = v
		}
		return Event{Kind: kind, TaskID: "t-1", Actor: "sebastian", Detail: d}
	}
	const named = "#603 (Session store: pick the table's store)"
	stranded := Event{Kind: EventCardHoldStranded, TaskID: "t-1", Detail: map[string]any{"release_task": "t-9", "release": "#31 (plan #30)", "release_issue": float64(31), "status": "closed"}}
	failedRelease := Event{Kind: EventCardHoldStranded, TaskID: "t-1", Detail: map[string]any{"release_task": "t-9", "release": "#31 (plan #30)", "release_issue": float64(31), "status": "failed"}}
	heldOn := func(release string) Task {
		c := card
		c.Status, c.Hold = "queued", &CardHold{Reason: "after the pg split", Release: release, By: "sebastian"}
		return c
	}
	for _, tt := range []struct {
		name string
		e    Event
		t    Task
		want ChatMessage
		ok   bool
	}{
		{"a question with no why names the question", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "source": "agent",
			"choices": []any{"Postgres", "SQLite"}}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Which store?", Choices: []string{"Postgres", "SQLite"}}, true},
		{"a question with its why", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "why": "Both stores pass the load test", "source": "agent"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Both stores pass the load test."}, true},
		{"a session cap names its why, not the budget", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "The card used all 3 sessions. How should it go on?",
			"why": "A card runs only the sessions it was given, and this one has used all 3.", "source": "cap",
			"offers":      []any{map[string]any{"value": "extend", "label": "Give more room", "effect": "Gives the card more sessions or agent time, and it goes on."}},
			"preselected": map[string]any{"action": "extend"}, "recommended_by": "the factory agent"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. A card runs only the sessions it was given, and this one has used all 3. The factory agent suggests “Give more room”."}, true},
		// a row after the card's escalation, so the inbox's answer row
		// offers it no more
		{"a withdrawn question", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "reason": "the checks failed"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " needs you no more: the checks failed."}, true},
		// a monitor's case sits beside the card's own ask: its row names the flag
		{"a monitor case's withdrawn question", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"case_id": float64(12), "case_kind": string(CaseMonitorFlag), "reason": "the wait ended", "problem": "The card waits past its bound."}}, card,
			ChatMessage{Kind: MessagePoke, Content: "The flag on " + named + " needs you no more: the wait ended."}, true},
		{"a case's withdrawn face is the card's own", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"case_id": float64(14), "case_kind": string(CaseSessionCap), "question": "Which store?", "reason": "reviewer sebastian requested changes"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " needs you no more: reviewer sebastian requested changes."}, true},
		// a withdrawal's line past a notice's length names the card by its
		// number alone: 182 runes with the title, 142 without
		{"a withdrawal past a notice's length drops the card's title", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"reason": "factoryd closed pull request #12: the branch it came from was deleted, and no other branch holds its commits any more"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 needs you no more: factoryd closed pull request #12: the branch it came from was deleted, and no other branch holds its commits any more."}, true},
		// a roll's 58-rune title takes the line to 163 runes, its number alone to 102
		{"rung 0's withdrawal on a roll card keeps its reason", Event{Kind: EventQuestionWithdrawn, TaskID: "t-r", Detail: map[string]any{"case_kind": string(CaseGateStop),
			"reason": "every check on head 1a2b3c4 is green, so the factory resumed the checks wait"}},
			Task{ID: "t-r", Kind: "agent", IssueNumber: 2255, Source: SourceRoll, Objective: "Deploy the new images factoryd, gateway, worker, ci-runner"},
			ChatMessage{Kind: MessagePoke, Content: "#2255 needs you no more: every check on head 1a2b3c4 is green, so the factory resumed the checks wait."}, true},
		// a reason of 188 runes passes a notice's length with the number alone (213)
		{"a withdrawal past a notice's length even by number drops its reason", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"reason": "factoryd closed pull request #12: the branch it came from was deleted on the fork, no other branch of the fork or of the repository holds its commits any more, and none can be pushed again"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " needs you no more."}, true},
		// a merge request's case climbs first: its face's ask is the poke
		{"a merge", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "high"}}, card, ChatMessage{}, false},
		{"an agent's merge on red checks", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "normal", "factory": false, "red_checks": true, "checks": "red"}}, card, ChatMessage{}, false},
		{"a factory merge on red checks", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "normal", "factory": true, "red_checks": true, "checks": "red"}}, card, ChatMessage{}, false},
		{"a merge's face", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: mergeAskRow(card, MergeSpec{RiskClass: "high", HeadSHA: "c1"})}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Merge “Session store: pick the table's store”? The change is high risk, and the factory's rules let only a human merge it.", Choices: []string{"Merge it"}}, true},
		{"the face of a merge the review approved suggests it", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: mergeAskRow(card, MergeSpec{RiskClass: "high", HeadSHA: "c1",
			Review: Review{Verdict: "approve", Summary: "The change is small and tested."}})}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Merge “Session store: pick the table's store”? The change is high risk, and the factory's rules let only a human merge it. The model review suggests “Merge it”.", Choices: []string{"Merge it"}}, true},
		{"the face of an agent's merge on red checks offers no merge", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: mergeAskRow(card, MergeSpec{RiskClass: "normal", HeadSHA: "c1", RedChecks: true})}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. The checks of “Session store: pick the table's store” are red. Send it back to its agent? It cannot merge until its checks pass."}, true},
		{"the face of a factory merge on red checks offers the rerun", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: mergeAskRow(card, MergeSpec{RiskClass: "normal", HeadSHA: "c1", RedChecks: true, Factory: true})}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. The checks of “Session store: pick the table's store” are red. Run them again, or close it? It cannot merge until its checks pass, and no agent works on a pull request the factory opened.",
				Choices: []string{"Run the failed checks again"}}, true},
		// a plan's questions climb first: the face of each asks, never its
		// request nor the planner's row
		{"a plan pokes nobody", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "plan", "revision": float64(2), "cards": float64(6)}}, plan, ChatMessage{}, false},
		{"a planner's question pokes nobody", Event{Kind: EventQuestionAsked, TaskID: "t-plan", Detail: map[string]any{"question": "Which database?", "revision": float64(2),
			"index": float64(1), "case_id": float64(31), "choices": []any{"Postgres", "SQLite"}}}, plan, ChatMessage{}, false},
		{"a planner's confirmation pokes nobody", Event{Kind: EventQuestionAsked, TaskID: "t-plan", Detail: ConfirmationRow(PlanQuestion{Kind: QuestionConfirmation,
			Text: "Confirmation of the spec and the criteria of plan revision 2:\nConfirm the plan fixes them."}, 2, 1)}, plan, ChatMessage{}, false},
		{"a plan question's face", Event{Kind: EventQuestionAsked, TaskID: "t-plan", Detail: faceAskRow(plan, Case{ID: 31, TaskID: plan.ID, Kind: CasePlanQuestion, Status: CaseAtHuman,
			Problem: "Which database?", Facts: map[string]any{"question": "Which database?", "revision": 2, "index": 1, "choices": []any{"Postgres", "SQLite"}}}, "Postgres", "SQLite")}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. Which database? The planner needs your answer before it can finish the plan.",
				Choices: []string{"Postgres", "SQLite"}}, true},
		{"a plan confirmation's face says why", Event{Kind: EventQuestionAsked, TaskID: "t-plan", Detail: faceAskRow(plan, Case{ID: 32, TaskID: plan.ID, Kind: CasePlanConfirmation, Status: CaseAtHuman,
			Problem: "Confirmation of the spec and the criteria of plan revision 2:\nConfirm the plan fixes them.", Facts: map[string]any{"revision": 2, "index": 2, "kind": QuestionConfirmation}})}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. Confirm plan “Members page” so its cards can start? The plan's cards and promises are fixed only once a human confirms them."}, true},
		// factoryd's own human card climbs first: its held row pokes nobody,
		// its face's row asks
		{"a held human card's ask", Event{Kind: EventQuestionAsked, TaskID: "t-h", Detail: map[string]any{"question": "Main is still red at abc1234. Fix it, or revert?",
			"source": SourceStillRed, "held": true, "case_id": float64(40)}}, held, ChatMessage{}, false},
		{"a human card's face", Event{Kind: EventQuestionAsked, TaskID: "t-h", Detail: faceAskRow(faced, Case{ID: 40, TaskID: "t-h", Kind: CaseRedMain, Status: CaseAtHuman,
			Problem: "Main is still red at abc1234. Fix it, or revert?", Facts: map[string]any{"ask": "Main is still red at abc1234. Fix it, or revert?",
				"why": "The remedy merged and main stayed red.", "source": SourceStillRed}})}, faced,
			ChatMessage{Kind: MessageEscalation, Content: "#70 (main is still red) needs you. The remedy merged and main stayed red."}, true},
		// a held card's withdrawn ask reached nobody, so its withdrawal
		// tells nobody either
		{"a held card's withdrawal", Event{Kind: EventQuestionWithdrawn, TaskID: "t-h", Detail: map[string]any{"reason": "the factory closed pull request #1: x",
			"held": true}}, held, ChatMessage{}, false},
		// a criteria request's case climbs first: its face's ask is the poke
		{"a criteria gap", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "criteria", "open": []any{"c1", "c2"}, "of": float64(3),
			"problem": CriteriaGapReason([]string{"c1", "c2"})}}, plan, ChatMessage{}, false},
		{"criteria never judged", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "criteria_rerun", "open": []any{"a", "b", "c"},
			"unchecked": []any{"a", "b", "c"}, "of": float64(3), "problem": "criteria never judged: the criteria gate has not settled a, b. c is yours to settle on this card"}}, plan, ChatMessage{}, false},
		{"a criteria gap's face", Event{Kind: EventQuestionAsked, TaskID: "t-plan", Detail: criteriaAskRow(plan, ApprovalCriteria, []string{"c1", "c2"}, nil, 3)}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. Plan “Members page” finished, but 2 of 3 promises are not met. Settle each one. " +
				"Until both are settled, the plan stays open and holds production deploys."}, true},
		{"the face of criteria never judged", Event{Kind: EventQuestionAsked, TaskID: "t-plan", Detail: criteriaAskRow(plan, ApprovalCriteriaRerun, []string{"a", "b", "c"}, []string{"a", "b", "c"}, 3)}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. Plan “Members page” finished, but no test checked its 3 promises. Settle each one. " +
				"Until all 3 are settled, the plan stays open and holds production deploys."}, true},
		{"the face of a merge whose promise a human judges first opens on its page", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: mergeAskRow(card, MergeSpec{RiskClass: "high", HeadSHA: "c1",
			Review: Review{Verdict: "approve"}, HumanOpen: []string{"pages_read"}})}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Is this promise met by “Session store: pick the table's store”? The pull request merges only once the promise a human judges has a verdict."}, true},
		{"a resource request pokes nobody", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "resource"}}, card, ChatMessage{}, false},
		{"a failure", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed",
			"reason": "policy: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " failed.", Choices: []string{"Try the failed card again", "Not now"}}, true},
		{"a failure that fits names its reason's first line", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed",
			"reason": "checks: the checks failed on c1.\noutput tail:\nFAIL TestStore"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " failed: the checks failed on c1.", Choices: []string{"Try the failed card again", "Not now"}}, true},
		// a title past 70 characters is cut at a word: "Board export: keep the nightly snapshots in Postgres and SQLite" is 63
		{"a long title is cut at a word", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}},
			Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Objective: "Board export: keep the nightly snapshots in Postgres and SQLite beside the export", SpentEUR: 0.214},
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. #603 (Board export: keep the nightly snapshots in Postgres and SQLite…) is done and live. It cost 0.21 EUR."}, true},
		{"a card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. " + named + " is done and live. It cost 0.21 EUR."}, true},
		{"a plan's card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, child, ChatMessage{}, false},
		// a merged card informs once at its merge; the done poke comes at
		// live, so a card already done by then (live at once) is quiet here
		{"a merge waits for the chain", Event{Kind: EventPullRequestMerged, TaskID: "t-1", Detail: map[string]any{"pr": float64(9), "by": MergeUser}}, func() Task { c := card; c.Status = "merged"; return c }(),
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. " + named + " is merged. The checks on main run now."}, true},
		{"a merge already done informs nobody", Event{Kind: EventPullRequestMerged, TaskID: "t-1", Detail: map[string]any{"pr": float64(9), "by": MergeUser}}, func() Task { c := card; c.Status = "done"; return c }(),
			ChatMessage{}, false},
		{"a closed card", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "closed"}}, card, ChatMessage{}, false},
		{"a chat task", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed"}}, Task{ID: "t-1", Kind: KindChat}, ChatMessage{}, false},
		{"a plan done", Event{Kind: EventAllChildrenDone, TaskID: "t-plan", Detail: map[string]any{"cards": float64(6)}}, plan,
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. The plan #558 (Members page) is done: all 6 cards."}, true},
		{"a chat budget spent", Event{Kind: EventChatBudgetSpent, TaskID: "t-chat-linkshort-2000-01", Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"budget_eur": float64(15), "spent_eur": 14.96, "suggest_eur": float64(3)}}, Task{ID: "t-chat-linkshort-2000-01", Kind: KindChat},
			ChatMessage{Kind: MessagePoke, Content: "The chat's budget for this month is used up: 14.96 of 15.00 EUR spent. Its turns wait until it is funded or the month turns."}, true},
		{"a level change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "manual", "to": "assisted"}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. sebastian set the autonomy from manual to assisted."}, true},
		{"a cap change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "assisted", "to": "assisted", "queue_cap": float64(5)}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. sebastian set the queue cap to 5."}, true},
		{"the agent's retry", agentDid(EventCardRetried, "assisted", map[string]any{}), card,
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. The agent tried " + named + " again.", Decider: "assisted level · retried by the agent"}, true},
		{"the agent's filing at full auto", agentDid(EventCardFiled, "autonomous", map[string]any{"merge_rule": "auto"}), card,
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. The agent filed " + named + ".", Decider: "full auto level · filed by the agent"}, true},
		{"the agent's grant", agentDid(EventSessionGranted, "assisted", map[string]any{}), card,
			ChatMessage{Kind: MessagePoke, Content: "Nothing for you. The agent gave " + named + " one more session.", Decider: "assisted level · granted by the agent"}, true},
		{"the agent's filing at manual", agentDid(EventCardFiled, "manual", map[string]any{}), card, ChatMessage{}, false},
		{"the member's retry", Event{Kind: EventCardRetried, TaskID: "t-1", Detail: map[string]any{"autonomy": "assisted", "decider": "member"}}, card, ChatMessage{}, false},
		{"a stranded hold", stranded, heldOn("t-9"),
			ChatMessage{Kind: MessagePoke, Content: "#603 (Session store: pick the table's store) stays on hold: its release card #31 (plan #30) was closed. " +
				"Release the hold or hold it again."}, true},
		{"a hold whose release card failed", failedRelease, heldOn("t-9"),
			ChatMessage{Kind: MessagePoke, Content: "#603 (Session store: pick the table's store) stays on hold: its release card #31 (plan #30) failed. " +
				"Retry #31 or release the hold."}, true},
		{"a hold released before the pass", stranded, func() Task { c := card; c.Status = "queued"; return c }(), ChatMessage{}, false},
		{"a hold a retry's card took over", stranded, heldOn("t-9-r1"), ChatMessage{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PokeOf(tt.e, tt.t, Task{})
			if ok != tt.ok {
				t.Fatalf("pokeOf ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if !slices.Contains(pokeKinds, tt.e.Kind) {
				t.Fatalf("the writer reads no %s event", tt.e.Kind)
			}
			tt.want.Role = "assistant"
			tt.want.BirthKind = tt.e.Kind
			if tt.e.Kind != EventAutonomyChanged && tt.e.Kind != EventChatBudgetSpent {
				tt.want.TaskID = tt.e.TaskID
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("pokeOf = %+v,\nwant %+v", got, tt.want)
			}
		})
	}
}

// A helper's poke names its step on the parent, the card the step stands
// on: a withdrawn ask, a flag's and a failure say "the sizing of #2003",
// never the parent alone, and a failed helper offers no retry, which
// files no report again. Its ask names the parent and says which step
// asks; its end done is the parent's to tell.
func TestAHelpersPokesNameItsStep(t *testing.T) {
	parent := Task{ID: "t-c", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 2003, Objective: "Board export: import a snapshot back"}
	sizing := Task{ID: "t-size", Kind: "agent", Status: "failed", Repository: "sf/code", Source: SourceSizing, Parent: "t-c"}
	const of = "#2003 (Board export: import a snapshot back)"
	event := func(kind string, detail map[string]any) Event {
		return Event{Kind: kind, TaskID: "t-size", IssueNumber: 2003, Detail: detail}
	}
	for _, tt := range []struct {
		name    string
		e       Event
		want    string
		choices []string
	}{
		{"its ask", event(EventQuestionAsked, map[string]any{"ask": "No machine could start the sizing's session. Try again, or end the sizing?",
			"why": "Several starts in a row waited for a machine with room, and no agent ran."}),
			of + " needs you. The sizing asks. Several starts in a row waited for a machine with room, and no agent ran.", nil},
		{"its withdrawn ask", event(EventQuestionWithdrawn, map[string]any{"reason": "the sizing ended"}),
			"The sizing of " + of + " needs you no more: the sizing ended.", nil},
		{"its withdrawn flag", event(EventQuestionWithdrawn, map[string]any{"reason": "the issue is closed", "case_kind": string(CaseMonitorFlag)}),
			"The flag on the sizing of " + of + " needs you no more: the issue is closed.", nil},
		{"its failure", event(EventTaskEnded, map[string]any{"state": "failed", "reason": "checks: the session found no plan."}),
			"The sizing of " + of + " failed: the session found no plan.", nil},
		{"the agent's grant", Event{Kind: EventSessionGranted, TaskID: "t-size", IssueNumber: 2003, Detail: map[string]any{"autonomy": "assisted", "merge_rule": "human", "decider": "agent"}},
			"Nothing for you. The agent gave the sizing of " + of + " one more session.", nil},
	} {
		m, ok := PokeOf(tt.e, sizing, parent)
		if !ok || m.Content != tt.want || !slices.Equal(m.Choices, tt.choices) {
			t.Errorf("%s: the poke = %q, choices %v (ok %v); want %q, choices %v", tt.name, m.Content, m.Choices, ok, tt.want, tt.choices)
		}
	}
	done := sizing
	done.Status = "done"
	if m, ok := PokeOf(event(EventTaskEnded, map[string]any{"state": "done"}), done, parent); ok {
		t.Errorf("the sizing's done end pokes %q, want no poke", m.Content)
	}
}

// The merged inform is one poke at the merge move, its words the chain's
// facts as the pass reads them (detail's chain, repair and covered —
// Pass's late read): running tells the chain, a red chain names the
// repair on it, and a chain that already covers the card — the poller
// concluded between merge and poke — informs nobody, its done poke its
// only tell.
func TestMergedPokeInformsOnce(t *testing.T) {
	merged := Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Status: "merged", FinishedAt: justNow(), Track: "direct"}
	merge := func(chain string, repair float64, covered bool) Event {
		return Event{Kind: EventPullRequestMerged, TaskID: "t-1",
			Detail: map[string]any{"pr": float64(9), "by": MergeUser, "chain": chain, "repair": repair, "covered": covered}}
	}
	for _, tt := range []struct {
		name string
		e    Event
		want string
		ok   bool
	}{
		{"the chain running", merge("running", 0, false), "Nothing for you. #603 is merged. The checks on main run now.", true},
		{"the chain red with its repair", merge("failure", 1139, false), "Nothing for you. #603 is merged, and the checks on main failed. #1139 repairs them.", true},
		{"the chain red before its repair card files", merge("failure", 0, false), "Nothing for you. #603 is merged, and the checks on main failed.", true},
		{"another head's red, its repair on it", merge("running", 1139, false), "Nothing for you. #603 is merged, and the checks on main failed. #1139 repairs them.", true},
		{"a chain that already covers the card", merge("failure", 0, true), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := PokeOf(tt.e, merged, Task{})
			if ok != tt.ok {
				t.Fatalf("pokeOf ok = %v, want %v", ok, tt.ok)
			}
			if ok && m.Content != tt.want {
				t.Fatalf("poke = %q, want %q", m.Content, tt.want)
			}
		})
	}
}

// Done pokes at live and its words state it, the whole card's cost beside
// them.
func TestDonePokeSaysLive(t *testing.T) {
	m, ok := PokeOf(Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}},
		Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Status: "done", SpentEUR: 0.214,
			Objective: "Session store: pick the table's store\n\nAcceptance:\n- one store", Track: "direct"}, Task{})
	if !ok {
		t.Fatal("pokeOf ok = false, want the done poke")
	}
	want := "Nothing for you. #603 (Session store: pick the table's store) is done and live. It cost 0.21 EUR."
	if m.Content != want {
		t.Fatalf("poke = %q, want %q", m.Content, want)
	}
}

// A chat budget's poke asks at the needs-you level: the messenger's copy
// goes to every member whose plan carries the level, an inform though it
// is, for the chat stands until someone funds it.
func TestChatBudgetPokeDeliversAtNeedsYou(t *testing.T) {
	e := Event{Kind: EventChatBudgetSpent, TaskID: "t-chat-linkshort-2000-01", Detail: map[string]any{"budget_eur": float64(5)}}
	m, ok := PokeOf(e, Task{ID: "t-chat-linkshort-2000-01", Kind: KindChat}, Task{})
	if !ok || m.Kind != MessagePoke {
		t.Fatalf("pokeOf = %+v, %v", m, ok)
	}
	if got := deliveryLevel(m); got != LevelNeedsYou {
		t.Fatalf("deliveryLevel = %q, want %q", got, LevelNeedsYou)
	}
}

// The writer on a real database: the first pass starts at the newest
// event, so no history pokes; each poke lands in the inbox thread of the
// project's notify list, else its approvers; a repeat of the newest poke
// about a card counts on its row; an event that pokes nobody moves the
// cursor only.
func TestPGStorePokes(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pokes"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, chain_runs CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, task := range []Task{
		{ID: "t-1", Kind: "agent", Repository: "sf/code", IssueNumber: 603, Status: "waiting-human", Objective: "Session store", Track: "direct"},
		{ID: "t-2", Kind: "agent", Repository: "sf/other", IssueNumber: 7, Status: "failed", Objective: "Feed page", Track: "direct"},
	} {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	record := func(e Event) {
		t.Helper()
		if _, err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	tree := policy.Tree{Document: policy.Document{Approvers: []string{"anna"}},
		Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian", "bob"}}}}
	line := &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: line}
	asked := Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which store?", "choices": []string{"Postgres", "SQLite"}}}

	record(asked) // before the first pass: history
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if threads, _ := s.Conversations(ctx, "sf/code", "sebastian"); len(threads) != 0 {
		t.Fatalf("the first pass poked the history: %+v", threads)
	}

	record(asked)
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventCardCommented, Actor: "sebastian", Via: ViaButton})
	record(asked)
	record(Event{Repository: "sf/other", TaskID: "t-2", Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"state": "failed"}})
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"state": "closed"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"sebastian", "bob"} {
		// the inbox first, then the empty current thread 1 stored with it
		threads, err := s.Conversations(ctx, "sf/code", login)
		if err != nil || len(threads) != 2 || !threads[0].Inbox || threads[0].Number != 2 || threads[0].Title != InboxTitle || threads[0].Unread != 1 ||
			threads[1].Number != 1 || !threads[1].Current || threads[1].Messages != 0 {
			t.Fatalf("%s's threads = %+v, %v, want the inbox 2 with one unread poke and the empty current thread 1", login, threads, err)
		}
		rows, err := s.ChatMessages(ctx, "sf/code", login, threads[0].Number, 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s's inbox = %+v, %v, want one row", login, rows, err)
		}
		got := rows[0]
		want := ChatMessage{ID: got.ID, At: got.At, Conversation: threads[0].Number, Role: "assistant", Kind: MessageEscalation,
			BirthKind: asked.Kind, Content: "#603 (Session store) needs you. Which store?", Choices: []string{"Postgres", "SQLite"}, TaskID: "t-1", Repeats: 1}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s's poke = %+v,\nwant %+v", login, got, want)
		}
	}
	threads, _ := s.Conversations(ctx, "sf/other", "anna")
	if len(threads) != 2 || threads[0].Messages != 1 {
		t.Fatalf("the approver's threads on sf/other = %+v, want the inbox with the failure", threads)
	}
	var cursor, newest int64
	if err := s.db.QueryRow(`SELECT (SELECT event_id FROM chat_poke_cursor), (SELECT max(id) FROM events)`).Scan(&cursor, &newest); err != nil || cursor != newest {
		t.Fatalf("the cursor = %d, the newest event %d (%v): want them equal", cursor, newest, err)
	}

	// a later question on the same card is a new poke, not a repeat
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which port?"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, _ := s.EnsureInbox(ctx, "sf/code", "bob")
	rows, _ := s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 2 || rows[1].Content != "#603 (Session store) needs you. Which port?" || rows[1].Repeats != 0 {
		t.Fatalf("bob's inbox = %+v, want the second question as its own row", rows)
	}

	// its withdrawal is the inbox's newest row: the answer row is gone
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionWithdrawn, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which port?", "reason": "the checks failed"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 3 || rows[2].Kind != MessagePoke || rows[2].Content != "#603 (Session store) needs you no more: the checks failed." {
		t.Fatalf("bob's inbox = %+v, want the withdrawal as its newest row", rows)
	}

	// a turn runs in bob's inbox: his poke waits queued behind it and goes
	// to the line to land; sebastian's lands at once (roadmap 263)
	line.busy["bob"] = true
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"state": "done"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 4 || !rows[3].Queued || rows[3].Content != "Nothing for you. #603 (Session store) is done and live. It cost 0.00 EUR." || !slices.Equal(line.landed["bob"], []int64{rows[3].ID}) {
		t.Fatalf("bob's inbox = %+v, landed %v: want the poke queued as its newest row and its id handed to the line", rows, line.landed["bob"])
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	rows, _ = s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10)
	if last := rows[len(rows)-1]; last.Queued || last.Content != "Nothing for you. #603 (Session store) is done and live. It cost 0.00 EUR." || line.landed["sebastian"] != nil {
		t.Fatalf("sebastian's newest row = %+v, landed %v: want the poke at once", last, line.landed["sebastian"])
	}

	// a merged card informs once at its merge, its words the chain's facts
	// as the pass reads them: a red chain names the repair card on it
	merged := Task{ID: "t-3", Kind: "agent", Repository: "sf/code", IssueNumber: 604, PRNumber: 9, Status: "merged", FinishedAt: justNow(), Objective: "Feed page", Track: "direct"}
	if err := s.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	// an earlier repair card, merged, stands beside the acting one: its fix
	// is in main, so it is no longer on the red
	old := merged
	old.ID, old.Source, old.IssueNumber, old.PRNumber = "t-r0", SourceChainRepair, 1100, 0
	if err := s.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	repair := merged
	repair.ID, repair.Status, repair.Source, repair.IssueNumber, repair.PRNumber = "t-r", "queued", SourceChainRepair, 1139, 0
	if err := s.CreateTask(ctx, repair); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChainRun(ctx, "sf/code", 9, "ffeeddccb00112233445566778899aabbccddeeff", 21, 0, "failure"); err != nil {
		t.Fatal(err)
	}
	record(Event{Repository: "sf/code", TaskID: "t-3", Kind: EventPullRequestMerged, Actor: "sebastian", Via: ViaButton,
		Detail: map[string]any{"pr": float64(9), "by": MergeUser}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 20)
	if len(rows) != 5 || rows[4].Content != "Nothing for you. #604 (Feed page) is merged, and the checks on main failed. #1139 repairs them." {
		t.Fatalf("bob's inbox = %+v, want the merged inform naming the repair as its newest row", rows)
	}
}

// A failed card's poke is made from the reason its issue comment carries:
// Finish records it on the task_ended event, and the poke names it without
// the gate's code word, "policy", within a notice's 160 characters (155).
func TestFinishRecordsTheIssueCommentsReason(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}}
	task := prTask
	task.Kind = "agent"
	r, _, objects := newActivityRunner(t, task, fj)
	reason := "policy: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml"
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", reason); err != nil {
		t.Fatal(err)
	}
	if want := "This card failed: " + reason; len(fj.comments) != 1 || fj.comments[0] != want {
		t.Fatalf("the issue's comments = %q, want %q", fj.comments, want)
	}
	b, err := objects.Get(t.Context(), "sessions/t1/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	for line := range strings.Lines(string(b)) {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err == nil && e["event"] == EventTaskEnded {
			detail = e
		}
	}
	if detail["reason"] != reason {
		t.Fatalf("the task_ended row's reason = %v, want %q", detail["reason"], reason)
	}
	m, ok := PokeOf(Event{Kind: EventTaskEnded, TaskID: "t1", Detail: detail}, task, Task{})
	if want := "#4 (Add a make check target) failed: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml."; !ok || m.Content != want {
		t.Fatalf("the poke = %+v, %t, want %q", m, ok, want)
	}
}

// Finish lands a merged card in merged after its cleanup, so a pass can
// read the merge while the card is on its way there: one in review or
// closing in a project whose chain runs informs as a merged one does; a
// roll, a card of a project without a chain and one of a repository no
// project file names end done at the merge and inform nobody.
func TestTheMergedInformReachesACardOnItsWayToMerged(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pokes"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, chain_runs CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tasks := []Task{
		{ID: "t-rev", Kind: "agent", Repository: "sf/code", IssueNumber: 601, PRNumber: 9, Status: "in-review", Objective: "Feed page", Track: "direct"},
		{ID: "t-cls", Kind: "agent", Repository: "sf/code", IssueNumber: 602, PRNumber: 10, Status: "closing", Objective: "Feed page", Track: "direct"},
		{ID: "t-roll", Kind: "agent", Repository: "sf/code", IssueNumber: 603, PRNumber: 11, Status: "closing", Source: SourceRoll, Objective: "roll", Track: "direct"},
		{ID: "t-none", Kind: "agent", Repository: "sf/demo", IssueNumber: 604, PRNumber: 3, Status: "closing", Objective: "Feed page", Track: "direct"},
		{ID: "t-gone", Kind: "agent", Repository: "sf/gone", IssueNumber: 605, PRNumber: 4, Status: "closing", Objective: "Feed page", Track: "direct"},
	}
	for _, task := range tasks {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	tree := policy.Tree{Document: policy.Document{Approvers: []string{"bob"}}, Projects: map[string]policy.ProjectConfig{
		"code": {Repository: "sf/code", Notify: []string{"bob"}},
		"demo": {Repository: "sf/demo", Notify: []string{"bob"}, Chain: policy.ChainConfig{Mode: chainModeNone}},
	}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if _, err := s.Record(ctx, Event{Repository: task.Repository, TaskID: task.ID, Kind: EventPullRequestMerged, Actor: MergeUser, Via: ViaFactoryd,
			Detail: map[string]any{"pr": float64(task.PRNumber), "by": MergeUser}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, repository := range []string{"sf/code", "sf/demo", "sf/gone"} {
		inbox, _ := s.EnsureInbox(ctx, repository, "bob")
		rows, _ := s.ChatMessages(ctx, repository, "bob", inbox, 10)
		for _, r := range rows {
			got = append(got, r.Content)
		}
	}
	if want := []string{"Nothing for you. #601 (Feed page) is merged. The checks on main run now.", "Nothing for you. #602 (Feed page) is merged. The checks on main run now."}; !slices.Equal(got, want) {
		t.Fatalf("bob's inboxes = %q, want %q", got, want)
	}
}

// busyLine is the chat's line as the poker sees it: a turn runs in the
// inbox of each login busy marks, and landed holds the queued rows handed
// to land there.
type busyLine struct {
	busy   map[string]bool
	landed map[string][]int64
}

func (l *busyLine) Behind(_, login string, _ int64) (bool, func(...int64)) {
	return l.busy[login], func(ids ...int64) {
		if l.busy[login] {
			l.landed[login] = append(l.landed[login], ids...)
		}
	}
}

// A level change pokes the project's other members and not the member who
// set it: they know what they set (Sebastian, 2026-10-21).
func TestPGStorePokeLevelChangeTellsTheOthers(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pokes_level"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	ctx := t.Context()
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian", "bob"}}}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil { // the first pass: history
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/code", Kind: EventAutonomyChanged, Actor: "sebastian", Via: ViaButton,
		Detail: map[string]any{"from": "manual", "to": "assisted"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, _ := s.EnsureInbox(ctx, "sf/code", "bob")
	rows, _ := s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 1 || rows[0].Content != "Nothing for you. sebastian set the autonomy from manual to assisted." {
		t.Fatalf("bob's inbox = %+v, want the level change", rows)
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if rows, _ := s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10); len(rows) != 0 {
		t.Fatalf("sebastian's inbox = %+v, want his own level change kept out", rows)
	}
}

// The card's own ask and a monitor's reach the inbox in one pass, both
// rows written after both asks: the monitor's withdrawal names its case,
// so it settles that row alone, read through either inbox read and the
// messenger's copy, and its own row names the case. On the card once it
// ended, the monitor's ask that asks its issue closed stays open until a
// row names its case, and another monitor's ask moves on.
func TestTheInboxSettlesAMonitorsAskByItsCase(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_poke_cases"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	ctx := t.Context()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, cases CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-1", Kind: "agent", Repository: "sf/code", IssueNumber: 603, Status: "waiting-human",
		Objective: "Session store", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian"}}}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	const flagged = "This card's run has not moved or answered for more than 30 minutes. Rescue the work into a new card?"
	flag, _, err := s.OpenCase(ctx, Case{TaskID: "t-1", Repository: "sf/code", Kind: CaseMonitorFlag, StateKey: "wait_over_bound|x",
		Problem: flagged})
	if err != nil {
		t.Fatal(err)
	}
	// the flag's face follows rung 2's hand-up, which passes it through the door
	if _, err := s.AppendCaseStep(ctx, CaseStep{CaseID: flag.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp}); err != nil {
		t.Fatal(err)
	}
	record := func(kind string, detail map[string]any) {
		t.Helper()
		if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-1", IssueNumber: 603, Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	record(EventQuestionAsked, map[string]any{"question": "Use a replica?"})
	record(EventQuestionAsked, map[string]any{"question": flagged, "case_id": flag.ID, "case_kind": string(CaseMonitorFlag)})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	record(EventQuestionWithdrawn, map[string]any{"case_id": flag.ID, "case_kind": string(CaseMonitorFlag), "reason": "the wait ended"})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := s.TaskEvents(ctx, "t-1", 500)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	if err != nil {
		t.Fatal(err)
	}
	asks, err := s.ChatAsks(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if want := "The flag on #603 (Session store) needs you no more: the wait ended."; len(rows) != 3 || len(asks) != 3 || rows[2].Content != want {
		t.Fatalf("the inbox = %+v, asks %+v; want the two asks and the withdrawal %q", rows, asks, want)
	}
	askRows := make([]ChatMessage, len(asks))
	for i, a := range asks {
		askRows[i] = a.ChatMessage
	}
	for read, got := range map[string][]ChatMessage{"ChatMessages": rows[:2], "ChatAsks": askRows[:2]} {
		var words []string
		for _, m := range got {
			words = append(words, SettleOf(m, events, "waiting-human", false).Word)
		}
		if !slices.Equal(words, []string{"", "withdrawn"}) {
			t.Fatalf("%s: the asks settle as %q, want the card's own open and the monitor's withdrawn", read, words)
		}
	}
	// the messenger's copy reads its row as the inbox does
	var copies []string
	for _, m := range rows[:2] {
		st, err := (&Sender{Store: s}).settleState(ctx, Delivery{Kind: m.Kind, BirthKind: m.BirthKind, TaskID: m.TaskID, At: m.At, Content: m.Content})
		if err != nil {
			t.Fatal(err)
		}
		copies = append(copies, st.Word)
	}
	if !slices.Equal(copies, []string{"", "withdrawn"}) {
		t.Fatalf("the messenger's copies settle as %q, want the card's own open and the monitor's withdrawn", copies)
	}

	// on the card once it ended, the monitor's ask that asks its issue
	// closed stays open until a row names its case, and another monitor's
	// ask moves on
	if _, err := s.db.Exec(`UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 't-1'`); err != nil {
		t.Fatal(err)
	}
	var faced []int64
	flagAsks := map[string]string{"issue_open_after_end": "This card ended, and its issue is still open. Close the issue?", "checkin_cut": "This card's last session went unjudged because its check ended early."}
	for _, flag := range []string{"issue_open_after_end", "checkin_cut"} {
		c, _, err := s.OpenCase(ctx, Case{TaskID: "t-1", Repository: "sf/code", Kind: CaseMonitorFlag, StateKey: flag + "|x", Problem: flagAsks[flag]})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp}); err != nil {
			t.Fatal(err)
		}
		faced = append(faced, c.ID)
		record(EventQuestionAsked, map[string]any{"question": flagAsks[flag],
			"case_id": c.ID, "case_kind": string(CaseMonitorFlag), "flag": flag})
	}
	copyWords := func() []string {
		t.Helper()
		if err := p.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		if rows, err = s.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10); err != nil || len(rows) < 5 {
			t.Fatalf("the inbox = %+v, %v; want the two asks as its fourth and fifth rows", rows, err)
		}
		var words []string
		for _, m := range rows[3:5] {
			st, err := (&Sender{Store: s}).settleState(ctx, Delivery{Kind: m.Kind, BirthKind: m.BirthKind, TaskID: m.TaskID, At: m.At, Content: m.Content})
			if err != nil {
				t.Fatal(err)
			}
			words = append(words, st.Word)
		}
		return words
	}
	if words := copyWords(); !slices.Equal(words, []string{"", "moved on"}) {
		t.Fatalf("the messenger's copies of the ended card's monitor asks settle as %q, want the issue's open and the other moved on", words)
	}
	record(EventQuestionWithdrawn, map[string]any{"case_id": faced[0], "reason": "the issue is closed"})
	if words := copyWords(); !slices.Equal(words, []string{"withdrawn", "moved on"}) {
		t.Fatalf("after the issue's withdrawal the copies settle as %q, want the issue's withdrawn", words)
	}
}

// Rows written before the inbox's buttons took the vocabulary's labels
// offer the old words, which no press resolves: the migration renames a
// request's and a failure's buttons to the labels and leaves an ask's
// choices, which are its asker's own words, as they are.
func TestTheChoiceLabelsMigrationRenamesTheOldButtons(t *testing.T) {
	s := newAskStore(t)
	ctx := t.Context()
	conv, err := s.EnsureInbox(ctx, "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []ChatMessage{
		{Kind: MessageEscalation, BirthKind: EventApprovalRequested, Choices: []string{"Approve"}},
		{Kind: MessageEscalation, BirthKind: EventApprovalRequested, Choices: []string{"Re-run the checks"}},
		{Kind: MessageEscalation, BirthKind: EventApprovalRequested, Choices: []string{"Approve", "Deny"}},
		{Kind: MessageEscalation, BirthKind: EventApprovalRequested, Choices: []string{"Accept", "Request changes"}},
		{Kind: MessagePoke, BirthKind: EventTaskEnded, Choices: []string{"Retry", "Leave it"}},
		{Kind: MessageEscalation, BirthKind: EventQuestionAsked, Choices: []string{"Approve", "Retry"}},
	} {
		// a request's own row is written no more: the store holds it from
		// before a request asked through its case's face
		birth := m.BirthKind
		m.Role, m.Conversation, m.TaskID, m.Content, m.BirthKind = "assistant", conv, "t-1", "a row", ""
		id, err := s.AddChatMessage(ctx, "sf/wake", "sebastian", m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE chat_messages SET birth_kind = $1 WHERE id = $2`, birth, id); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.ReadFile("../schema/migrations/factoryd/20261125100000_chat_choice_labels.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(file)); err != nil {
		t.Fatalf("the migration: %v", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT choices FROM chat_messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][]string
	for rows.Next() {
		var raw []byte
		var choices []string
		if err := rows.Scan(&raw); err != nil || json.Unmarshal(raw, &choices) != nil {
			t.Fatalf("a row's choices: %v %s", err, raw)
		}
		got = append(got, choices)
	}
	want := [][]string{{"Merge it"}, {"Run the failed checks again"}, {"Merge it", "Send it back"}, {"Confirm the plan", "Ask for changes"},
		{"Try the failed card again", "Not now"}, {"Approve", "Retry"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the choices = %q, want %q", got, want)
	}
}

// An ask's message holds at most 280 characters: a why that would take it
// past stays on the card, and the name, the ask and the suggestion stand.
func TestAnAsksMessageDropsAWhyPastItsBound(t *testing.T) {
	why := strings.Repeat("a long reason ", 18) // 252 characters
	if got := askMessage("#7 (Pages)", "Merge “Pages”?", "it fits", ""); got != "#7 (Pages) needs you. Merge “Pages”? It fits." {
		t.Errorf("a short why: %q", got)
	}
	if got := askMessage("#7 (Pages)", "Merge “Pages”?", why, "The model review suggests “Merge it”."); got != "#7 (Pages) needs you. Merge “Pages”? The model review suggests “Merge it”." {
		t.Errorf("a why past the bound: %q", got)
	}
}

// A refusal names each answer by the key an agent types, and a human
// reads the answer's label: a key with its variant reads the variant's
// label, a key no answer has stays as it is, and the name of a field an
// agent passes, in brackets, goes.
func TestForHumanNamesTheAnswersByTheirLabels(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"the human may take `extend`, `close` and `note`", "the human may take “Give more room”, “End the card” and “Not now”"},
		{"send it back (`deny_merge` with `request_changes`) or close it (`deny_merge` with `reject`)", "send it back (“Send it back”) or close it (“Reject and close it”)"},
		{"`withdraw` ends a monitor flag's ask", "`withdraw` ends a monitor flag's ask"},
		{"`set_dependency` names the card to wait for (`depends_on`)", "“Make it wait for a card” names the card to wait for"},
		{"Choose “It is met” or “It is not met” (`met` or `not_met`).", "Choose “It is met” or “It is not met”."},
		{"no key here", "no key here"},
	} {
		if got := ForHuman(tc.in); got != tc.want {
			t.Errorf("ForHuman(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A hold whose release card ends failed or closed releases nothing, and the
// human hears it once: #31 of plan #30 fails, ending twice as a retried end
// may, and each inbox the project tells gets one poke about held card #6,
// naming #31, its plan, how it ended and the two ways on; #8, held on
// another card, and the done end of #33, a release card too, poke nobody,
// and no case opens. The poke is an inform, delivered at needs-you.
func TestAStrandedHoldPokesTheHumanOnce(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_stranded"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, cases CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	card := func(id string, n int64, status string) Task {
		return Task{ID: id, Kind: "agent", Repository: "sf/code", IssueNumber: n, Status: status, Objective: "Card " + id, Track: "direct"}
	}
	plan := card("t-pl", 30, "done")
	plan.Track = policy.TrackPlan
	rel := card("t-rel", 31, "running")
	rel.Source, rel.Parent, rel.PlanKey = SourcePlan, "t-pl", "split"
	for _, k := range []Task{plan, rel, card("t-rel2", 33, "running"), card("t-held", 6, "queued"), card("t-held2", 8, "queued")} {
		if err := s.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	for id, release := range map[string]string{"t-held": "t-rel", "t-held2": "t-rel2"} {
		if ok, err := s.SetCardHold(ctx, id, CardHold{Reason: "after the pg split", Release: release, By: "sebastian", At: at}); err != nil || !ok {
			t.Fatalf("hold %s: %v, %v", id, ok, err)
		}
	}
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian"}}}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	tl := &Timeline{Objects: &fakeObjectStore{}, Events: s, Store: s}
	for id, final := range map[string]string{"t-rel": "failed", "t-rel2": "done"} {
		if err := s.TransitionTask(ctx, id, []string{"running"}, final); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		taskEnded(ctx, tl, s, nil, nil, rel, "failed", "the checks stayed red")
	}
	rel2, _, _ := s.Task(ctx, "t-rel2")
	taskEnded(ctx, tl, s, nil, nil, rel2, "done", "")
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	if err != nil {
		t.Fatal(err)
	}
	var held []ChatMessage
	for _, m := range rows {
		if m.BirthKind == EventCardHoldStranded {
			held = append(held, m)
		}
	}
	const want = "#6 (Card t-held) stays on hold: its release card #31 (plan #30) failed. Retry #31 or release the hold."
	if len(held) != 1 || held[0].Kind != MessagePoke || held[0].TaskID != "t-held" || held[0].Content != want || held[0].Repeats != 0 || len(held[0].Choices) != 0 {
		t.Fatalf("the hold's pokes = %+v, want one inform about #6: %q", held, want)
	}
	if level := deliveryLevel(held[0]); level != LevelNeedsYou {
		t.Errorf("the poke's delivery level = %q, want needs-you", level)
	}
	for _, id := range []string{"t-held", "t-held2"} {
		if open, err := s.OpenCasesOf(ctx, id); err != nil || len(open) != 0 {
			t.Errorf("%s's open cases = %+v, %v, want none", id, open, err)
		}
	}
}

// criteriaAskRow is the ask row the face of plan's criteria case of kind
// writes as it reaches the human: open its open promises, unchecked those
// nothing judged, of the plan's count.
func criteriaAskRow(plan Task, kind string, open, unchecked []string, of int) map[string]any {
	return faceAskRow(plan, OnOpen(Case{ID: 7, TaskID: plan.ID, Kind: CasePlanCriteria, Status: CaseAtHuman, Facts: map[string]any{"approval": 30, "approval_kind": kind}}, open, unchecked, of))
}

// mergeAskRow is the ask row the face of card's merge case writes as it
// reaches the human, the case as the request of spec for pull request 612
// opens it.
func mergeAskRow(card Task, spec MergeSpec) map[string]any {
	b, _ := json.Marshal(spec)
	c, err := RequestCase(ApprovalRequest{ID: 31, Kind: ApprovalMerge, TaskID: card.ID, PRNumber: 612, Spec: b})
	if err != nil {
		panic(err)
	}
	c.ID, c.Status = 8, CaseAtHuman
	return faceAskRow(card, c)
}

// faceAskRow is the ask row the face of case c on card writes as it reaches
// the human, as AskHuman writes it and the store reads it back, choices
// the case's own.
func faceAskRow(card Task, c Case, choices ...string) map[string]any {
	same := func(s string) string { return s }
	face := FaceRow(c, card, FactsOf(c), "", nil, same, same)
	ask, _ := face["ask"].(string)
	b, _ := json.Marshal(AskRow(c, card, c.Problem, ask, 0, face, choices, nil, same))
	var read map[string]any
	_ = json.Unmarshal(b, &read)
	return read
}
