package factoryd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	humanConfirmButton = "made with the plan's Confirm button."
	belowFullAutoWord  = "below full auto is the human's"
	offMainWord        = "takes merged work off main"
)

// By hand: a human passes at every level and on every revision. The agent
// passes a question only at full auto; its confirmation only at full auto
// and only when the revision takes nothing off main; its Request changes
// is an answer like a question's.
func TestPlanAnswerRefusal(t *testing.T) {
	for _, c := range []struct {
		name                         string
		rung                         int
		autonomy                     string
		confirming, off              bool
		refuses, wantWord, wantPress string
	}{
		{"a human at manual", 3, policy.AutonomyManual, true, true, "", "", ""},
		{"a human at full auto, taking work off main", 3, policy.AutonomyFullAuto, true, true, "", "", ""},
		{"the agent's confirmation at full auto", 2, policy.AutonomyFullAuto, true, false, "", "", ""},
		{"the agent's question at full auto", 2, policy.AutonomyFullAuto, false, false, "", "", ""},
		{"the agent's Request changes at full auto, taking work off main", 2, policy.AutonomyFullAuto, false, true, "", "", ""},
		{"the agent's confirmation at full auto, taking work off main", 2, policy.AutonomyFullAuto, true, true, "x", offMainWord, humanConfirmButton},
		{"the agent's confirmation at autonomous", 2, policy.AutonomyAutonomous, true, false, "x", belowFullAutoWord, humanConfirmButton},
		{"the agent's confirmation at assisted", 2, policy.AutonomyAssisted, true, false, "x", belowFullAutoWord, humanConfirmButton},
		{"the agent's question at autonomous", 2, policy.AutonomyAutonomous, false, false, "x", belowFullAutoWord, "on the plan's card"},
		{"the agent's answer with no level read", 2, "", false, false, "x", "autonomy level could not be read", "on the plan's card"},
		{"rung 1's confirmation at full auto", 1, policy.AutonomyFullAuto, true, false, "x", "Rung 1 decides no plan answer", "on the plan's card"},
		{"rung 1's question at full auto", 1, policy.AutonomyFullAuto, false, false, "x", "Rung 1 decides no plan answer", "on the plan's card"},
	} {
		got := PlanAnswerRefusal(c.rung, c.autonomy, c.confirming, c.off)
		if got != "" && c.rung != 3 && !strings.Contains(got, "attach_analysis") {
			t.Errorf("%s: refusal %q names no next step for the agent", c.name, got)
		}
		if (got != "") != (c.refuses != "") || !strings.Contains(got, c.wantWord) || !strings.Contains(got, c.wantPress) {
			t.Errorf("%s: refusal %q, want one with %q and %q: %v", c.name, got, c.wantWord, c.wantPress, c.refuses != "")
		}
	}
}

// Rung 2's statement offers the confirmation only where the guard passes
// it: at full auto on a revision that takes nothing off main; on one that
// takes work off main it names the human's press and offers Request
// changes; below full auto it offers nothing.
func TestPlanOfferFollowsTheGuard(t *testing.T) {
	confirm := Case{Kind: CasePlanConfirmation, Facts: map[string]any{"revision": float64(3), "index": float64(2)}}
	question := Case{Kind: CasePlanQuestion, Facts: map[string]any{"revision": float64(3), "index": float64(1)}}
	for _, c := range []struct {
		name     string
		c        Case
		autonomy string
		off      bool
		holdouts bool
		has, not []string
	}{
		{"confirmation at full auto", confirm, policy.AutonomyFullAuto, false, false, []string{"You may confirm this plan with the answer tool (revision 3, question 2, decision confirm)"}, nil},
		{"confirmation with hold-outs undecided", confirm, policy.AutonomyFullAuto, false, true, []string{"Hold-out candidates", "only the human okays or drops them", "decision revise"}, []string{"You may confirm"}},
		{"confirmation taking work off main", confirm, policy.AutonomyFullAuto, true, false, []string{offMainWord, humanConfirmButton, "decision revise"}, []string{"You may confirm"}},
		{"confirmation at autonomous", confirm, policy.AutonomyAutonomous, false, false, []string{belowFullAutoWord}, []string{"You may"}},
		{"question at full auto", question, policy.AutonomyFullAuto, false, false, []string{"You may answer this question with the answer tool (revision 3, question 1)"}, nil},
		{"question at assisted", question, policy.AutonomyAssisted, false, false, []string{belowFullAutoWord}, []string{"You may"}},
	} {
		got := planOffer(c.c, c.autonomy, c.off, c.holdouts)
		for _, w := range c.has {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q holds %q", c.name, got, w)
			}
		}
	}
}

func planAskRig(t *testing.T, autonomy string, takeOff []string) (factsRig, Case) {
	t.Helper()
	g := newFactsRig(t)
	g.card()
	files := testFiles(testPolicyYAML)
	demo := demoFile
	if autonomy != policy.AutonomyFullAuto {
		demo = strings.Replace(strings.Replace(demoFile, "autonomy: autonomous\n", "autonomy: "+autonomy+"\n", 1), "merge_rule: auto", "merge_rule: human", 1)
	}
	files["projects/demo-app.yaml"] = []byte(policy.Header("projects/demo-app.yaml") + "\n" + demo)
	files["projects/ai-software-factory-code.yaml"] = []byte(policy.Header("projects/ai-software-factory-code.yaml") + "\n" + codeFile)
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	v := policy.Version{ID: 7, Tree: tree}
	g.r.cfg.Policy = func() policy.Version { return v }
	raw, _ := json.Marshal(Plan{TakeOffMain: takeOff})
	if _, err := g.store.AddPlanRevision(context.Background(), "t1", raw, "r1"); err != nil {
		t.Fatal(err)
	}
	c := Case{ID: 7, TaskID: "t1", Kind: CasePlanConfirmation, Status: CaseAtHuman, Facts: map[string]any{"revision": float64(1), "index": float64(1)}}
	g.store.cases = append(g.store.cases, c)
	return g, c
}

// Act asks the guard for a plan's ask: refused for rung 1 at every level,
// and for rung 2 below full auto and on a revision that takes work off main
// or whose reverts cannot be read, each naming the human's press; at full
// auto on a revision that takes nothing off main rung 2 goes past the
// guard, and its note is recorded as a step on the case, which stays open.
// A plan question follows the same one rule.
func TestActAsksThePlanAnswerGuard(t *testing.T) {
	for _, c := range []struct {
		name, autonomy string
		kind           CaseKind
		rung           int
		takeOff        []string
		noRevision     bool
		want           string
	}{
		{"confirmation at full auto", policy.AutonomyFullAuto, CasePlanConfirmation, 2, nil, false, ""},
		{"confirmation taking work off main", policy.AutonomyFullAuto, CasePlanConfirmation, 2, []string{"api"}, false, offMainWord},
		{"confirmation whose revision cannot be read", policy.AutonomyFullAuto, CasePlanConfirmation, 2, nil, true, offMainWord},
		{"confirmation at autonomous", policy.AutonomyAutonomous, CasePlanConfirmation, 2, nil, false, humanConfirmButton},
		{"confirmation at assisted", policy.AutonomyAssisted, CasePlanConfirmation, 2, nil, false, humanConfirmButton},
		{"question at full auto", policy.AutonomyFullAuto, CasePlanQuestion, 2, nil, false, ""},
		{"question at autonomous", policy.AutonomyAutonomous, CasePlanQuestion, 2, nil, false, belowFullAutoWord},
		{"rung 1's confirmation at full auto", policy.AutonomyFullAuto, CasePlanConfirmation, 1, nil, false, "Rung 1 decides no plan answer"},
		{"rung 1's question at full auto", policy.AutonomyFullAuto, CasePlanQuestion, 1, nil, false, "Rung 1 decides no plan answer"},
	} {
		g, k := planAskRig(t, c.autonomy, c.takeOff)
		g.store.cases[len(g.store.cases)-1].Kind = c.kind
		if c.noRevision {
			delete(g.store.plans, "t1")
		}
		_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: k.ID, By: "factory-agent", Rung: c.rung, Res: Resolution{Action: ActNote, Note: "x"}})
		steps := g.store.caseSteps[k.ID]
		if c.want == "" {
			if err != nil || len(steps) != 1 || steps[0].Rung != 2 || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman {
				t.Errorf("%s: Act = %v, steps %+v; want the note recorded on the open case", c.name, err, steps)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || len(steps) != 0 {
			t.Errorf("%s: Act = %v, steps %+v; want the refusal with %q and nothing recorded", c.name, err, steps, c.want)
		}
	}
}

// A replan that takes merged work off main is the human's through Act too:
// rung 2's is refused with the human's press named and nothing is asked of
// the planner; a plain replan is not refused for it.
func TestActRefusesAnAgentsReplanOffMain(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.store.cases = append(g.store.cases, Case{ID: 7, TaskID: "t1", Kind: CaseSessionCap, Status: CaseAtHuman})
	_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: 7, By: "factory-agent", Rung: 2,
		Res: Resolution{Action: ActReplan, Note: "the plan is wrong", Args: map[string]any{"off_main": true}}})
	if err == nil || !strings.Contains(err.Error(), "A replan that takes merged work off main is the human's, made with the Replan button's off-main box.") {
		t.Errorf("rung 2's off-main replan = %v", err)
	}
	if got := OffMainReplanRefusal(3); got != "" {
		t.Errorf("the human's off-main replan refused: %q", got)
	}
}
