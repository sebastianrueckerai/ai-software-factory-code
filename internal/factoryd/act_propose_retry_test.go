package factoryd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// atLevel puts linkshort at autonomy state: full auto is autonomous with
// merge rule auto.
func atLevel(t *testing.T, r *Runner, state string) {
	t.Helper()
	v := fixtureVersion(t)
	name, p, ok := v.ProjectOf("linkshort/linkshort")
	if !ok {
		t.Fatal("the policy names no linkshort project")
	}
	p.Autonomy, p.MergeRule = state, policy.MergeHuman
	if state == policy.AutonomyFullAuto {
		p.Autonomy, p.MergeRule = policy.AutonomyAutonomous, policy.MergeAuto
	}
	projects := map[string]policy.ProjectConfig{}
	for k, v := range v.Projects {
		projects[k] = v
	}
	projects[name] = p
	v.Projects = projects
	r.cfg.Policy = func() policy.Version { return v }
}

// Rung 2's retry of the failed dependency on the retry rig. By hand: at
// assisted nothing is filed (no issue #9, no replacement task), rung 2's
// step ends handed up with the retry proposed, and the human is asked once
// with it preselected, whether the case was open or already at the human
// (a new comment names the proposal, the question carries it). At
// autonomous and full auto the same call files #9 and resolves the case.
func TestRung2RetryProposesAtAssistedAndRunsAbove(t *testing.T) {
	for _, mode := range []string{CaseOpen, CaseAtHuman, "at human, step expired"} {
		status := CaseAtHuman
		if mode == CaseOpen {
			status = CaseOpen
		}
		for _, level := range []string{policy.AutonomyAssisted, policy.AutonomyManual, policy.AutonomyAutonomous, policy.AutonomyFullAuto} {
			t.Run(mode+"/"+level, func(t *testing.T) {
				ctx := context.Background()
				r, store, _, fj, _ := newRetryRig(t)
				atLevel(t, r, level)
				store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: status, StateKey: "t-a", Facts: map[string]any{}}}
				if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-b", Kind: IntakeEscalation, Author: "factoryd", Body: "ask",
					Question: &HumanQuestion{Ask: "A dependency failed", Why: "w", Info: "i"}}); err != nil {
					t.Fatal(err)
				}
				if mode != "at human, step expired" {
					if _, err := store.AppendCaseStep(ctx, CaseStep{CaseID: 3, Rung: 2, Actor: ChatUser, Outcome: StepInProgress}); err != nil {
						t.Fatal(err)
					}
				}
				if status == CaseAtHuman {
					store.events = append(store.events, Event{TaskID: "t-b", Kind: EventQuestionAsked, Detail: map[string]any{"case_id": int64(3), "ask": "A dependency failed"}})
				}
				note := "the dependency failed on a cap " + leaky
				got, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Via: ViaChat, Rung: 2,
					Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Note: note}})
				if err != nil {
					t.Fatal(err)
				}
				filed := fj.issues[9] != nil
				if level == policy.AutonomyAssisted || level == policy.AutonomyManual {
					if filed || !got.Proposed || got.Text != "Proposed retry on #6: the human confirms it on the card." {
						t.Fatalf("filed %v, result %+v; want nothing filed and the proposal's text", filed, got)
					}
					steps := store.caseSteps[3]
					last := steps[len(steps)-1]
					if last.Rung != 2 || last.Outcome != StepHandedUp || len(last.Did) != 1 || last.Did[0].Action != ActRetry || last.Did[0].Result != "proposed" {
						t.Fatalf("steps %+v, want rung 2's handed up with the retry proposed", steps)
					}
					if strings.Contains(last.Grounds, "user:pass") || strings.Contains(last.Grounds, "abc123def456") {
						t.Errorf("grounds %q hold a secret", last.Grounds)
					}
					var asked []Event
					for _, e := range store.events {
						if e.Kind == EventQuestionAsked && e.TaskID == "t-b" {
							asked = append(asked, e)
						}
					}
					q := store.intakeEvents[len(store.intakeEvents)-1].Question
					if len(asked) != 1 {
						t.Fatalf("%d question_asked rows, want the one face", len(asked))
					}
					pre, _ := asked[0].Detail["preselected"].(map[string]any)
					if pre["action"] != ActRetry || !strings.HasPrefix(asked[0].Detail["recommendation"].(string), "retry: ") {
						t.Errorf("face %+v, want retry preselected and recommended", asked[0].Detail)
					}
					if !strings.HasPrefix(q.Recommendation, "retry: ") || strings.Contains(q.Recommendation, "user:pass") || strings.Contains(q.Recommendation, "abc123def456") {
						t.Errorf("recommendation %q, want retry's, masked", q.Recommendation)
					}
					var on6 []string
					for _, c := range fj.comments {
						if c.issue == 6 {
							on6 = append(on6, c.body)
						}
					}
					if len(on6) != 1 || (status == CaseAtHuman && !strings.Contains(on6[0], "The factory agent proposes retry: ")) ||
						strings.Contains(on6[0], "user:pass") || strings.Contains(on6[0], "abc123def456") {
						t.Errorf("comments on #6 %q, want one", on6)
					}
					return
				}
				if !filed || got.Proposed || got.Case.Status != CaseResolved {
					t.Fatalf("filed %v, result %+v; want #9 filed and the case resolved", filed, got)
				}
			})
		}
	}
}

// The cards a held roll, a refused configuration and a red main file are
// human cards: their choices answer their case through Act, as a human
// card's do. By hand: the human's pick resolves the case; rung 2's pick
// resolves it at full auto and is proposed at assisted, leaving the case
// open at the human.
func TestActTakesTheChoicesOfTheBoardKinds(t *testing.T) {
	for _, kind := range []CaseKind{CaseRedMain, CaseRollHeld, CaseConfigRefused} {
		for _, tc := range []struct {
			name  string
			rung  int
			level string
			want  string
		}{
			{"human", 3, policy.AutonomyManual, CaseResolved},
			{"rung 2 at full auto", 2, policy.AutonomyFullAuto, CaseResolved},
			{"rung 2 at assisted", 2, policy.AutonomyAssisted, CaseAtHuman},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				r, store, _, _, _ := newRetryRig(t)
				atLevel(t, r, tc.level)
				if err := store.CreateTask(ctx, Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "linkshort/linkshort", IssueNumber: 7}); err != nil {
					t.Fatal(err)
				}
				store.cases = []Case{{ID: 7, TaskID: "t-h", Kind: kind, Status: CaseAtHuman, Facts: map[string]any{}}}
				if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-h", Kind: IntakeEscalation, Author: "factoryd",
					Question: &HumanQuestion{Ask: "what now", Choices: []QuestionChoice{{Value: AnswerClose, Label: "Done"}}}}); err != nil {
					t.Fatal(err)
				}
				by := "sebastian"
				if tc.rung == 3 {
					by = "alice"
				}
				got, err := r.Act(ctx, ActInput{TaskID: "t-h", CaseID: 7, By: by, Via: ViaChat, Rung: tc.rung, Res: Resolution{Action: AnswerClose, Note: "Done"}})
				if err != nil {
					t.Fatalf("Act: %v", err)
				}
				if got.Case.Status != tc.want || got.Proposed != (tc.want == CaseAtHuman) {
					t.Fatalf("case %s, proposed %v; want %s", got.Case.Status, got.Proposed, tc.want)
				}
			})
		}
	}
}

// A second proposal on a case whose face already holds one is refused, with
// the standing proposal and the case named; nothing more is posted.
func TestASecondProposalOnTheSameCaseIsRefused(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	atLevel(t, r, policy.AutonomyAssisted)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: CaseAtHuman, StateKey: "t-a", Facts: map[string]any{}}}
	propose := func() error {
		_, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Via: ViaChat, Rung: 2,
			Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Note: "again"}})
		return err
	}
	if err := propose(); err != nil {
		t.Fatal(err)
	}
	err := propose()
	want := "case 3 already carries your proposal of retry: the human decides it on the card"
	if err == nil || err.Error() != want {
		t.Fatalf("second proposal = %v, want %q", err, want)
	}
	var on6 int
	for _, c := range fj.comments {
		if c.issue == 6 {
			on6++
		}
	}
	if on6 != 1 || len(store.caseSteps[3]) != 1 {
		t.Fatalf("%d comments on #6, steps %+v; want one of each", on6, store.caseSteps[3])
	}
}

// A human card's work runs inside Act, once Act accepts the answer. By
// hand: the human's pick and rung 2's at full auto run it once, and its
// comment is the answer's; rung 2's at assisted is proposed and a choice
// the question does not offer or a case that ended is refused, and none of
// those runs it. The level is read when Act answers, so one lowered after
// the turn began proposes.
func TestActRunsAHumanCardsWorkOnlyOnceItAccepts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rung  int
		level string
		value string
		ended bool
		runs  bool
	}{
		{"human", 3, policy.AutonomyManual, AnswerKeepWaiting, false, true},
		{"rung 2 at full auto", 2, policy.AutonomyFullAuto, AnswerKeepWaiting, false, true},
		{"rung 2 at assisted", 2, policy.AutonomyAssisted, AnswerKeepWaiting, false, false},
		{"a choice the question does not offer", 3, policy.AutonomyManual, "deploy_anyway", false, false},
		{"the case ended", 3, policy.AutonomyManual, AnswerKeepWaiting, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, store, _, _, _ := newRetryRig(t)
			atLevel(t, r, tc.level)
			if err := store.CreateTask(ctx, Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "linkshort/linkshort", IssueNumber: 7}); err != nil {
				t.Fatal(err)
			}
			status := CaseAtHuman
			if tc.ended {
				status = CaseResolved
			}
			store.cases = []Case{{ID: 7, TaskID: "t-h", Kind: CaseRollHeld, Status: status, Facts: map[string]any{}}}
			if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-h", Kind: IntakeEscalation, Author: "factoryd",
				Question: &HumanQuestion{Ask: "what now", Choices: []QuestionChoice{{Value: AnswerKeepWaiting, Label: "Keep waiting"}}}}); err != nil {
				t.Fatal(err)
			}
			by := "sebastian"
			if tc.rung == 3 {
				by = "alice"
			}
			ran := 0
			got, err := r.Act(ctx, ActInput{TaskID: "t-h", CaseID: 7, By: by, Via: ViaChat, Rung: tc.rung, Res: Resolution{Action: tc.value, Note: "Keep waiting"},
				Work: func(context.Context) (int64, error) { ran++; return 99, nil }})
			if ran != map[bool]int{true: 1, false: 0}[tc.runs] {
				t.Fatalf("work ran %d times (err %v), want runs %v", ran, err, tc.runs)
			}
			if tc.runs && (err != nil || got.Comment != 99) {
				t.Fatalf("err %v, comment %d; want the work's comment 99 as the answer's", err, got.Comment)
			}
		})
	}
}

// A case holds one standing recommendation: a proposal on a case whose face
// already holds rung 2's attach_analysis recommendation is refused, naming it.
func TestAProposalAfterAnAnalysisIsRefused(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	atLevel(t, r, policy.AutonomyAssisted)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: CaseAtHuman, StateKey: "t-a", Facts: map[string]any{}}}
	if _, err := store.AppendCaseStep(ctx, CaseStep{CaseID: 3, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp, Decision: ActReplan,
		Did: []CaseAction{{Action: ActReplan, Result: resultRecommended}}}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Via: ViaChat, Rung: 2,
		Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}})
	want := "case 3 already carries your recommendation of replan: the human decides it on the card"
	if err == nil || err.Error() != want {
		t.Fatalf("proposal after an analysis = %v, want %q", err, want)
	}
	for _, c := range fj.comments {
		if c.issue == 6 {
			t.Fatalf("a comment on #6 after the refusal: %q", c.body)
		}
	}
}

// The wait for a case's lock ends with the answer's context.
func TestActGivesUpTheWaitForACasesLockWhenItsContextEnds(t *testing.T) {
	r, store, _, _, _ := newRetryRig(t)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: CaseAtHuman, StateKey: "t-a", Facts: map[string]any{}}}
	lock, _ := r.caseLocks.LoadOrStore(int64(3), &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3, Res: Resolution{Action: ActClose}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Act on a locked case = %v, want the context's deadline", err)
	}
}

// A monitor flag faces beside the card's own ask: its proposal updates its
// own face alone, and the card's question keeps no recommendation.
func TestAFlagsProposalLeavesTheCardsQuestionAlone(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := newRetryRig(t)
	atLevel(t, r, policy.AutonomyAssisted)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseMonitorFlag, Status: CaseAtHuman, StateKey: "k", Facts: map[string]any{"flag": "session_no_push"}}}
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-b", Kind: IntakeEscalation, Author: "factoryd", Question: &HumanQuestion{Ask: "the card's own ask"}}); err != nil {
		t.Fatal(err)
	}
	store.events = append(store.events, Event{TaskID: "t-b", Kind: EventQuestionAsked, Detail: map[string]any{"case_id": int64(3), "flag": "session_no_push"}})
	if _, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Note: "x"}}); err != nil {
		t.Fatal(err)
	}
	if q := store.intakeEvents[len(store.intakeEvents)-1].Question; q.Recommendation != "" {
		t.Errorf("the card's question holds %q, want none from a flag's proposal", q.Recommendation)
	}
	face := store.events[len(store.events)-1]
	if pre, _ := face.Detail["preselected"].(map[string]any); pre["action"] != ActRetry {
		t.Errorf("the flag's face %+v, want retry preselected", face.Detail)
	}
}
