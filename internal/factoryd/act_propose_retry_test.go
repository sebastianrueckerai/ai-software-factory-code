package factoryd

import (
	"context"
	"strings"
	"testing"

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
	for _, status := range []string{CaseOpen, CaseAtHuman} {
		for _, level := range []string{policy.AutonomyAssisted, policy.AutonomyManual, policy.AutonomyAutonomous, policy.AutonomyFullAuto} {
			t.Run(status+"/"+level, func(t *testing.T) {
				ctx := context.Background()
				r, store, _, fj, _ := newRetryRig(t)
				atLevel(t, r, level)
				store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: status, StateKey: "t-a", Facts: map[string]any{}}}
				if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-b", Kind: IntakeEscalation, Author: "factoryd", Body: "ask",
					Question: &HumanQuestion{Ask: "A dependency failed", Why: "w", Info: "i"}}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.AppendCaseStep(ctx, CaseStep{CaseID: 3, Rung: 2, Actor: ChatUser, Outcome: StepInProgress}); err != nil {
					t.Fatal(err)
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
					if status == CaseOpen && len(asked) != 1 {
						t.Fatalf("%d question_asked rows, want the one face", len(asked))
					}
					if status == CaseOpen {
						pre, _ := asked[0].Detail["preselected"].(map[string]any)
						if pre["action"] != ActRetry || !strings.HasPrefix(asked[0].Detail["recommendation"].(string), "retry: ") {
							t.Errorf("face %+v, want retry preselected and recommended", asked[0].Detail)
						}
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
					Question: &HumanQuestion{Ask: "what now", Choices: []QuestionChoice{{Value: AnswerKeepWaiting, Label: "Keep waiting"}}}}); err != nil {
					t.Fatal(err)
				}
				by := "sebastian"
				if tc.rung == 3 {
					by = "alice"
				}
				got, err := r.Act(ctx, ActInput{TaskID: "t-h", CaseID: 7, By: by, Via: ViaChat, Rung: tc.rung, Res: Resolution{Action: AnswerKeepWaiting, Note: "Keep waiting"}})
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
