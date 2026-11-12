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
		byAgent                      bool
		autonomy                     string
		confirming, off              bool
		refuses, wantWord, wantPress string
	}{
		{"a human at manual", false, policy.AutonomyManual, true, true, "", "", ""},
		{"a human at full auto, taking work off main", false, policy.AutonomyFullAuto, true, true, "", "", ""},
		{"the agent's confirmation at full auto", true, policy.AutonomyFullAuto, true, false, "", "", ""},
		{"the agent's question at full auto", true, policy.AutonomyFullAuto, false, false, "", "", ""},
		{"the agent's Request changes at full auto, taking work off main", true, policy.AutonomyFullAuto, false, true, "", "", ""},
		{"the agent's confirmation at full auto, taking work off main", true, policy.AutonomyFullAuto, true, true, "x", offMainWord, humanConfirmButton},
		{"the agent's confirmation at autonomous", true, policy.AutonomyAutonomous, true, false, "x", belowFullAutoWord, humanConfirmButton},
		{"the agent's confirmation at assisted", true, policy.AutonomyAssisted, true, false, "x", belowFullAutoWord, humanConfirmButton},
		{"the agent's question at autonomous", true, policy.AutonomyAutonomous, false, false, "x", belowFullAutoWord, "on the plan's card"},
		{"the agent's answer with no level read", true, "", false, false, "x", belowFullAutoWord, "on the plan's card"},
	} {
		got := PlanAnswerRefusal(c.byAgent, c.autonomy, c.confirming, c.off)
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
		has, not []string
	}{
		{"confirmation at full auto", confirm, policy.AutonomyFullAuto, false, []string{"You may confirm this plan with the answer tool (revision 3, question 2, decision confirm)"}, nil},
		{"confirmation taking work off main", confirm, policy.AutonomyFullAuto, true, []string{offMainWord, humanConfirmButton, "decision revise"}, []string{"You may confirm"}},
		{"confirmation at autonomous", confirm, policy.AutonomyAutonomous, false, []string{belowFullAutoWord}, []string{"You may"}},
		{"question at full auto", question, policy.AutonomyFullAuto, false, []string{"You may answer this question with the answer tool (revision 3, question 1)"}, nil},
		{"question at assisted", question, policy.AutonomyAssisted, false, []string{belowFullAutoWord}, []string{"You may"}},
	} {
		got := planOffer(c.c, c.autonomy, c.off)
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

// Act asks the guard for a plan's ask at rung 2: refused below full auto
// and on a revision that takes work off main, each naming the human's
// press; at full auto on a revision that takes nothing off main the guard
// passes it (the action table then judges the action itself). A plan
// question follows the same one rule.
func TestActAsksThePlanAnswerGuard(t *testing.T) {
	for _, c := range []struct {
		name, autonomy, kind string
		takeOff              []string
		want                 string
	}{
		{"confirmation at full auto", policy.AutonomyFullAuto, string(CasePlanConfirmation), nil, ""},
		{"confirmation taking work off main", policy.AutonomyFullAuto, string(CasePlanConfirmation), []string{"api"}, offMainWord},
		{"confirmation at autonomous", policy.AutonomyAutonomous, string(CasePlanConfirmation), nil, humanConfirmButton},
		{"confirmation at assisted", policy.AutonomyAssisted, string(CasePlanConfirmation), nil, humanConfirmButton},
		{"question at full auto", policy.AutonomyFullAuto, string(CasePlanQuestion), nil, ""},
		{"question at autonomous", policy.AutonomyAutonomous, string(CasePlanQuestion), nil, belowFullAutoWord},
	} {
		g, k := planAskRig(t, c.autonomy, c.takeOff)
		g.store.cases[len(g.store.cases)-1].Kind = CaseKind(c.kind)
		_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: k.ID, By: "factory-agent", Rung: 2, Res: Resolution{Action: ActNote, Note: "x"}})
		got := ""
		if err != nil {
			got = err.Error()
		}
		guarded := strings.Contains(got, belowFullAutoWord) || strings.Contains(got, offMainWord)
		if c.want == "" && guarded || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: Act = %q, want the guard's refusal with %q: %v", c.name, got, c.want, c.want != "")
		}
	}
}
