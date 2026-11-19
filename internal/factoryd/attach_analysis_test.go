package factoryd

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// AttachAnalysis records the wake turn's findings on the card's open ask
// (the main agent concept): an analysis_attached row on the event spine,
// which the card's face shows beside the ask (CaseDesk). No inbox row
// repeats it, and the ask's own event stays what it was: the spine is
// append-only.

// newAskStore opens the store and empties the rows an ask touches.
func newAskStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "attach"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	for _, table := range []string{"events", "tasks", "chat_messages", "chat_conversations"} {
		if _, err := s.db.ExecContext(t.Context(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return s
}

// seedAsk seeds card issue of repository waiting on its question: the
// case the ask opened, its question_asked row naming the case, and the
// member's escalation row of it. It answers the case and the row's words.
func seedAsk(t *testing.T, s *PGStore, repository, taskID string, issue int64, question string) (Case, string) {
	t.Helper()
	card := Task{ID: taskID, Kind: "agent", Status: "waiting-human", Repository: repository, IssueNumber: issue, Objective: question, Track: "direct"}
	if err := s.CreateTask(t.Context(), card); err != nil {
		t.Fatal(err)
	}
	c, e := askOn(t, s, card, "ask", question)
	return c, seedRow(t, s, e, card)
}

// askOn records question on card as the ask that opens a case at the
// human under key, and answers the case and the ask's event.
func askOn(t *testing.T, s *PGStore, card Task, key, question string) (Case, Event) {
	t.Helper()
	c, _, err := s.OpenCase(t.Context(), Case{TaskID: card.ID, Repository: card.Repository, Kind: CaseAgentEscalate, StateKey: key,
		Status: CaseAtHuman, Problem: question})
	if err != nil {
		t.Fatal(err)
	}
	e := Event{Repository: card.Repository, TaskID: card.ID, IssueNumber: card.IssueNumber, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": question, "why": "the card cannot go on", "info": "the metrics say so", "case_id": c.ID}}
	if _, err := s.Record(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	return c, e
}

// seedRow writes event e's poke on card to the member's inbox, as the
// Poker writes it, and answers the row's words.
func seedRow(t *testing.T, s *PGStore, e Event, card Task) string {
	t.Helper()
	m, ok := PokeOf(e, card, Task{})
	if !ok {
		t.Fatalf("no poke of %s", e.Kind)
	}
	conv, err := s.EnsureInbox(t.Context(), e.Repository, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	m.Conversation = conv
	if _, err := s.AddChatMessage(t.Context(), e.Repository, "sebastian", m); err != nil {
		t.Fatal(err)
	}
	return m.Content
}

// askRow reads the task's escalation row content.
func askRow(t *testing.T, s *PGStore, repository, taskID string) string {
	t.Helper()
	var content string
	if err := s.db.QueryRowContext(t.Context(), `SELECT content FROM chat_messages
		WHERE repository = $1 AND task_id = $2 AND kind = $3`, repository, taskID, MessageEscalation).Scan(&content); err != nil {
		t.Fatal(err)
	}
	return content
}

// analyses counts the task's analysis_attached rows.
func analyses(t *testing.T, s *PGStore, taskID string) []Event {
	t.Helper()
	events, err := s.TaskEvents(t.Context(), taskID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range events {
		if e.Kind == EventAnalysisAttached {
			out = append(out, e)
		}
	}
	return out
}

// sebastiansAnalysis is the analysis_attached row sebastian's chat writes.
var sebastiansAnalysis = Event{Repository: "sf/wake", Actor: "sebastian", Via: ViaChat}

// The row keeps the detail its caller gave, a chat token's stamp, beside
// the findings the store adds.
func TestAttachAnalysisKeepsItsEventsDetail(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	e := sebastiansAnalysis
	e.Detail = map[string]any{"token": map[string]any{"id": 7, "name": "laptop"}}
	if err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", e); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	rows := analyses(t, s, "t-1")
	if len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	got := rows[0]
	if token, _ := got.Detail["token"].(map[string]any); got.Actor != "sebastian" || got.IssueNumber != 40 || got.Detail["found"] != "The replica adds 12 ms of lag." ||
		got.Detail["source"] != "agent" || token["name"] != "laptop" || token["id"] != float64(7) {
		t.Fatalf("the analysis row = %+v", got)
	}
}

// deskAnalysis is the analysis the board reads beside case id's ask.
func deskAnalysis(t *testing.T, s *PGStore, repository string, id int64) string {
	t.Helper()
	d, err := s.CaseDesk(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	return d.Asks[id].Analysis
}

// An open ask takes the analysis on its card: the spine holds an
// analysis_attached row with the findings, the board reads the newest one
// beside the card's case and its face shows it after the agent's view,
// and the inbox's row stays as the Poker wrote it. A second attach is the
// one the card shows, while the spine keeps both.
func TestAttachAnalysisShowsOnTheCard(t *testing.T) {
	s := newAskStore(t)
	c, seededRow := seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The replica adds 12 ms of lag, and the board reads it fine.", "Keep one replica", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	want := "The project agent found: The replica adds 12 ms of lag, and the board reads it fine.\n\nIt recommends: Keep one replica"
	if got := deskAnalysis(t, s, "sf/wake", c.ID); got != want {
		t.Fatalf("the card's analysis = %q, want %q", got, want)
	}
	ask := CaseAsk{Detail: map[string]any{"info": "the metrics say so"}, Analysis: want}
	if got := CaseFace(c, Task{ID: "t-1"}, ask).Info; got != "the metrics say so\n\n"+Quoted(want) {
		t.Fatalf("the face's info = %q", got)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); got != seededRow {
		t.Fatalf("the inbox's row = %q, want it as written: %q", got, seededRow)
	}
	rows := analyses(t, s, "t-1")
	if len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	e := rows[0]
	if e.Via != ViaChat || e.Actor != "sebastian" || e.Detail["source"] != "agent" ||
		e.Detail["found"] != "The replica adds 12 ms of lag, and the board reads it fine." ||
		e.Detail["recommendation"] != "Keep one replica" {
		t.Fatalf("the analysis row = %+v", e)
	}
	events, err := s.TaskEvents(t.Context(), "t-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if first := events[len(events)-1]; first.Kind != EventQuestionAsked || first.Detail["question"] != "Use a replica?" || first.Detail["found"] != nil {
		t.Fatalf("the ask's own event changed: %+v", first)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1",
		"The writes queue behind the replica at the measured peak.", "", sebastiansAnalysis); err != nil {
		t.Fatal(err)
	}
	if got := deskAnalysis(t, s, "sf/wake", c.ID); got != "The project agent found: The writes queue behind the replica at the measured peak." {
		t.Fatalf("the second attach left the card at %q", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 2 {
		t.Fatalf("%d analysis rows after the second attach, want 2", len(rows))
	}
}

// A monitor's ask sits beside the card's own and is answered by its
// case's number: the card's own ask takes the analysis, though the
// monitor's is newer and open, and no row of the inbox changes. The board
// shows the analysis beside the card's own case, not the monitor's.
func TestAttachAnalysisLeavesAMonitorsAskOut(t *testing.T) {
	s := newAskStore(t)
	own, seededRow := seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	flag, _, err := s.OpenCase(t.Context(), Case{TaskID: "t-1", Repository: "sf/wake", Kind: CaseMonitorFlag, StateKey: "flag", Status: CaseAtHuman,
		Problem: "This card's run has not moved or answered for more than 30 minutes. Rescue the work into a new card?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40, Kind: EventQuestionAsked, Actor: FactoryDecider,
		Via: ViaFactoryd, Detail: map[string]any{"question": "This card's run has not moved or answered for more than 30 minutes. Rescue the work into a new card?",
			"case_id": flag.ID, "case_kind": string(CaseMonitorFlag)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); got != seededRow {
		t.Fatalf("the card's own ask's row = %q, want it as written", got)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	if got := deskAnalysis(t, s, "sf/wake", own.ID); got != "The project agent found: The replica adds 12 ms of lag." {
		t.Fatalf("the card's own case shows %q", got)
	}
	if got := deskAnalysis(t, s, "sf/wake", flag.ID); got != "" {
		t.Fatalf("the monitor's case shows %q, want no analysis", got)
	}
}

// A second ask after the first was answered takes the analysis: the
// answer settled the first ask alone.
func TestAttachAnalysisTakesTheAskAfterAnAnsweredOne(t *testing.T) {
	s := newAskStore(t)
	seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40, Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat,
		Detail: map[string]any{"answer": "No"}}); err != nil {
		t.Fatal(err)
	}
	card, _, err := s.Task(t.Context(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := askOn(t, s, card, "port", "Which port?")
	if err := s.AttachAnalysis(t.Context(), "t-1", "Port 8080 is taken.", "", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 1 || rows[0].Detail["found"] != "Port 8080 is taken." {
		t.Fatalf("the analysis rows = %+v", rows)
	}
	if got := deskAnalysis(t, s, "sf/wake", second.ID); got != "The project agent found: Port 8080 is taken." {
		t.Fatalf("the second ask's case shows %q", got)
	}
}

// An ask a settling event answered takes no analysis: AskSettledError with
// the ask's kind, the word and the actor, the row unchanged, no analysis
// row on the spine.
func TestAttachAnalysisRefusesAnsweredAsk(t *testing.T) {
	s := newAskStore(t)
	_, seededRow := seedAsk(t, s, "sf/wake", "t-1", 40, "Use a replica?")
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaChat, Detail: map[string]any{"answer": "No"}}); err != nil {
		t.Fatal(err)
	}
	err := s.AttachAnalysis(t.Context(), "t-1", "The replica adds 12 ms of lag.", "", sebastiansAnalysis)
	var se *AskSettledError
	if !errors.As(err, &se) || se.Word != "answered" || se.By != "sebastian" {
		t.Fatalf("the refusal = %v, want the question answered by sebastian", err)
	}
	if got := askRow(t, s, "sf/wake", "t-1"); got != seededRow {
		t.Fatalf("the answered ask's row changed: %q, want %q", got, seededRow)
	}
	if rows := analyses(t, s, "t-1"); len(rows) != 0 {
		t.Fatalf("%d analysis rows on an answered ask, want 0", len(rows))
	}
}

// seatFace records the face of live case c of card as its climb writes it:
// the register's face row, which puts the case at the human.
func seatFace(t *testing.T, s *PGStore, card Task, c Case, reason string) {
	t.Helper()
	same := func(s string) string { return s }
	c.Status = CaseAtHuman
	face := FaceRow(c, card, FactsOf(c), "", nil, same, same)
	ask, _ := face["ask"].(string)
	if _, err := s.Record(t.Context(), Event{Repository: card.Repository, TaskID: card.ID, IssueNumber: card.IssueNumber, Kind: EventQuestionAsked,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: AskRow(c, card, reason, ask, 0, face, nil, nil, same)}); err != nil {
		t.Fatal(err)
	}
}

// A merge case faced beside the card's run, the card in review: its ask,
// the face's row, waits while the request pends and takes the analysis,
// which the merge's card shows, while the request's own row is no ask.
// Once a human approved the merge the ask is settled: the refusal names
// the question approved by that human, and nothing more is written.
func TestAttachAnalysisReachesAMergeCasesFace(t *testing.T) {
	s := newAskStore(t)
	ctx := t.Context()
	card := Task{ID: "t-merge", Kind: "agent", Status: "in-review", Repository: "sf/wake", IssueNumber: 41,
		PRNumber: 9, Objective: "Add the index.", Track: "direct"}
	if err := s.CreateTask(ctx, card); err != nil {
		t.Fatal(err)
	}
	reason := "pull request #9 at abc123 is on the card (risk class high): a human decides the merge"
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t-merge", Project: "sf/wake", PRNumber: 9,
		Spec: []byte(`{"risk_class":"high","head_sha":"abc123"}`), Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	request := Event{Repository: "sf/wake", TaskID: "t-merge", IssueNumber: 41, Kind: EventApprovalRequested, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"approval": id, "kind": ApprovalMerge, "pr": 9, "class": "high", "factory": false, "red_checks": false, "checks": "green"}}
	if _, err := s.Record(ctx, request); err != nil {
		t.Fatal(err)
	}
	if m, ok := PokeOf(request, card, Task{}); ok {
		t.Fatalf("the request's own row pokes with %q, want no poke", m.Content)
	}
	if err := s.AttachAnalysis(ctx, "t-merge", "The index covers the board's query.", "approve_merge", sebastiansAnalysis); !errors.Is(err, ErrNoAsk) {
		t.Fatalf("AttachAnalysis before the face = %v, want ErrNoAsk: the request's row is no ask", err)
	}
	cases, err := s.OpenCasesOf(ctx, "t-merge")
	if err != nil || len(cases) != 1 || cases[0].Kind != CaseMergeApproval || cases[0].Status != CaseOpen {
		t.Fatalf("the card's live cases = %+v, %v, want its merge case open at rung 0", cases, err)
	}
	seatFace(t, s, card, cases[0], reason)
	if err := s.AttachAnalysis(ctx, "t-merge", "The index covers the board's query.", "approve_merge", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	if got, want := deskAnalysis(t, s, "sf/wake", cases[0].ID), "The project agent found: The index covers the board's query.\n\nIt recommends: approve_merge"; got != want {
		t.Fatalf("the merge's case shows %q, want %q", got, want)
	}
	if rows := analyses(t, s, "t-merge"); len(rows) != 1 || rows[0].IssueNumber != 41 ||
		rows[0].Detail["found"] != "The index covers the board's query." || rows[0].Detail["recommendation"] != "approve_merge" {
		t.Fatalf("the analysis rows = %+v", rows)
	}

	if err := s.DecideApproval(ctx, id, ApprovalMerge, "approved", "sebastian", "Approved in the factory console."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/wake", TaskID: "t-merge", IssueNumber: 41, Kind: EventMergeApproved,
		Actor: "sebastian", Via: ViaConsole, Detail: map[string]any{"pr": 9}}); err != nil {
		t.Fatal(err)
	}
	err = s.AttachAnalysis(ctx, "t-merge", "The index is still fine.", "", sebastiansAnalysis)
	var se *AskSettledError
	if !errors.As(err, &se) || se.Word != "approved" || se.By != "sebastian" {
		t.Fatalf("the refusal = %v, want the question approved by sebastian", err)
	}
	if rows := analyses(t, s, "t-merge"); len(rows) != 1 {
		t.Fatalf("%d analysis rows after the settled attach, want 1", len(rows))
	}
}

// A plan's request stands beside its revision's two open questions and,
// asked after them, its confirmation when the plan has one: the request
// is the revision's bookkeeping, and the planner's rows ask nobody while
// their cases climb. Once each case has its face the card opens on the
// first question, so the analysis attaches to that question's case and
// shows there, on no other page.
func TestAttachAnalysisOnAPlanTakesItsOpenQuestion(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(fmt.Sprintf("confirmation %v", confirmed), func(t *testing.T) {
			s := newAskStore(t)
			ctx := t.Context()
			// a card of its own: the store keeps the cases of the run before
			card := Task{ID: fmt.Sprintf("t-plan-%v", confirmed), Kind: "agent", Status: "waiting-human", Repository: "sf/wake", IssueNumber: 42, Objective: "Members page", Track: "plan"}
			if err := s.CreateTask(ctx, card); err != nil {
				t.Fatal(err)
			}
			ask := func(detail map[string]any) {
				t.Helper()
				if _, err := s.Record(ctx, Event{Repository: "sf/wake", TaskID: card.ID, IssueNumber: 42, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
					Detail: detail}); err != nil {
					t.Fatal(err)
				}
			}
			ask(PlanQuestionRow(PlanQuestion{Text: "How far back does the feed page by default?", Choices: []string{"One day", "One week"},
				Changes: []string{"The feed shows the last day.", "The feed shows the last week."}}, 1, 1))
			ask(PlanQuestionRow(PlanQuestion{Text: "Who may read the feed?", Choices: []string{"Every human", "The project's humans"},
				Changes: []string{"The feed is open.", "The feed asks for a login."}}, 1, 2))
			want := []CaseKind{CasePlanQuestion, CasePlanQuestion}
			if confirmed {
				ask(ConfirmationRow(PlanQuestion{Kind: QuestionConfirmation,
					Text: ConfirmationQuestion(1, Plan{Criteria: []PlanCriterion{{ID: "feed_reads_fast", Kind: CriterionTest}}})}, 1, 3))
				want = append(want, CasePlanConfirmation)
			}
			id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: card.ID, Project: "sf/wake", Spec: []byte(`{"revision":1}`),
				Reason: PlanApprovalReason(1, 42, 4, len(want))})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Record(ctx, Event{Repository: "sf/wake", TaskID: card.ID, IssueNumber: 42, Kind: EventApprovalRequested, Actor: FactoryDecider, Via: ViaFactoryd,
				Detail: map[string]any{"approval": id, "kind": ApprovalPlan, "revision": 1, "cards": 4}}); err != nil {
				t.Fatal(err)
			}
			if err := s.AttachAnalysis(ctx, card.ID, "A week of rows fits one page on a busy day.", "", sebastiansAnalysis); !errors.Is(err, ErrNoAsk) {
				t.Fatalf("AttachAnalysis before a face = %v, want ErrNoAsk: the planner's rows and the request ask nobody", err)
			}
			climbing, err := s.OpenCasesOf(ctx, card.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range climbing {
				if c.Status != CaseOpen {
					t.Fatalf("case %d (%s) is %s, want it open at rung 0", c.ID, c.Kind, c.Status)
				}
				seatFace(t, s, card, c, c.Problem)
			}
			if err := s.AttachAnalysis(ctx, card.ID, "A week of rows fits one page on a busy day.", "", sebastiansAnalysis); err != nil {
				t.Fatalf("AttachAnalysis: %v", err)
			}
			cases, err := s.OpenCasesOf(ctx, card.ID)
			if err != nil {
				t.Fatal(err)
			}
			var kinds []CaseKind
			for _, c := range cases {
				kinds = append(kinds, c.Kind)
			}
			if !slices.Equal(kinds, want) || cases[0].Problem != "How far back does the feed page by default?" {
				t.Fatalf("the card's live cases = %+v, want %v, the first question first", cases, want)
			}
			if got := deskAnalysis(t, s, "sf/wake", cases[0].ID); got != "The project agent found: A week of rows fits one page on a busy day." {
				t.Fatalf("the first question's case shows %q", got)
			}
			for _, c := range cases[1:] {
				if got := deskAnalysis(t, s, "sf/wake", c.ID); got != "" {
					t.Errorf("case %d (%s) shows %q, want no analysis", c.ID, c.Kind, got)
				}
			}
		})
	}
}

// A plan's criteria case faced on the done plan: its ask, the face's row,
// waits while the request pends, and takes the analysis, which its card
// shows; the request's own row pokes nobody.
func TestAttachAnalysisReachesACriteriaCasesFace(t *testing.T) {
	s := newAskStore(t)
	ctx := t.Context()
	// a card of its own: the store keeps the cases of the run before
	card := Task{ID: "t-criteria", Kind: "agent", Status: "done", Repository: "sf/wake", IssueNumber: 43, Objective: "Members page", Track: "plan"}
	if err := s.CreateTask(ctx, card); err != nil {
		t.Fatal(err)
	}
	reason := CriteriaGapReason([]string{"c1"})
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalCriteria, TaskID: card.ID, Project: "sf/wake", Spec: []byte(`{"task":"t-int"}`), Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	request := Event{Repository: "sf/wake", TaskID: card.ID, IssueNumber: 43, Kind: EventApprovalRequested, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"approval": id, "kind": ApprovalCriteria, "criteria": []string{"c1"}, "problem": reason}}
	if _, err := s.Record(ctx, request); err != nil {
		t.Fatal(err)
	}
	if m, ok := PokeOf(request, card, Task{}); ok {
		t.Fatalf("the request's own row pokes with %q, want no poke", m.Content)
	}
	cases, err := s.OpenCasesOf(ctx, card.ID)
	if err != nil || len(cases) != 1 || cases[0].Kind != CasePlanCriteria || cases[0].Status != CaseOpen {
		t.Fatalf("the plan's live cases = %+v, %v, want its criteria case open at rung 0", cases, err)
	}
	c := cases[0]
	seatFace(t, s, card, c, reason)
	if err := s.AttachAnalysis(ctx, card.ID, "c1 fails on main's head.", "grant_criteria", sebastiansAnalysis); err != nil {
		t.Fatalf("AttachAnalysis: %v", err)
	}
	rows := analyses(t, s, card.ID)
	if len(rows) != 1 {
		t.Fatalf("%d analysis rows, want 1", len(rows))
	}
	if named, _ := toInt(rows[0].Detail["case_id"]); int64(named) != c.ID {
		t.Fatalf("the analysis names %v, want the face's case %d", rows[0].Detail, c.ID)
	}
	if got, want := deskAnalysis(t, s, "sf/wake", c.ID), "The project agent found: c1 fails on main's head.\n\nIt recommends: grant_criteria"; got != want {
		t.Fatalf("the criteria case shows %q, want %q", got, want)
	}
}
