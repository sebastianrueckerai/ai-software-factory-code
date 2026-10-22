package factoryd

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A rule that fires when its condition held at every step of the window
// (matchType 2, "all the times") must see the whole window: a series born
// inside it (a roll's new ReplicaSet) holds for its few steps and fired
// "factory-gateway roll not live for 10 minutes" three minutes after the
// roll was live (#215).
func TestAllTheTimesRulesRequireAFullWindow(t *testing.T) {
	var files []string
	for _, dir := range []string{"../../k8s/alerts", "../../k8s/platform/signoz/project-rules"} {
		more, err := filepath.Glob(dir + "/*.json")
		if err != nil {
			t.Fatalf("rules in %s: %v", dir, err)
		}
		files = append(files, more...)
	}
	if len(files) == 0 {
		t.Fatalf("no rules found")
	}
	seen := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Condition struct {
				RequireMinPoints  bool `json:"requireMinPoints"`
				RequiredNumPoints int  `json:"requiredNumPoints"`
				CompositeQuery    struct {
					Queries []struct {
						Spec struct {
							StepInterval int `json:"stepInterval"`
						} `json:"spec"`
					} `json:"queries"`
				} `json:"compositeQuery"`
				Thresholds struct {
					Spec []struct {
						MatchType string `json:"matchType"`
					} `json:"spec"`
				} `json:"thresholds"`
			} `json:"condition"`
			Evaluation struct {
				Spec struct {
					EvalWindow string `json:"evalWindow"`
				} `json:"spec"`
			} `json:"evaluation"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(r.Condition.Thresholds.Spec) == 0 || r.Condition.Thresholds.Spec[0].MatchType != "2" {
			continue
		}
		seen++
		window, err := time.ParseDuration(r.Evaluation.Spec.EvalWindow)
		if err != nil {
			t.Fatalf("%s: evalWindow: %v", f, err)
		}
		step := r.Condition.CompositeQuery.Queries[0].Spec.StepInterval
		if want := int(window.Seconds()) / step; !r.Condition.RequireMinPoints || r.Condition.RequiredNumPoints != want {
			t.Errorf("%s: requireMinPoints %v, requiredNumPoints %d, want true and %d (%s in steps of %d s)",
				filepath.Base(f), r.Condition.RequireMinPoints, r.Condition.RequiredNumPoints, want, window, step)
		}
	}
	if seen != 3 {
		t.Fatalf("%d all-the-times rules, want the two roll rules and ci-queue-p95", seen)
	}
}

// The 5xx-rate rule measures a share, and a share alone has no floor:
// one failed request among the eighteen a Saturday-evening factory
// serves is 5.5555... %, above the target, and the rule filed a
// human-work card for a blip every handler is built to ride out
// (2026-10-17, 1 of 18). A storm is not one request: the rule counts
// at least three failures in the window, folded into its formula as a
// gate that is 0 below the floor and 1 above it — the sign of
// A - 2.5, a half a count never lands on — because a formula knows
// arithmetic and abs but no ternary. The evaluation is rolling, so
// the formula's A and B are the window's totals in one division (the
// storm of 2026-10-16 fired at 8 of 141, 5.6737... %), and the floor
// sees every failure the window saw.
func Test5xxRateRuleRequiresThreeFailures(t *testing.T) {
	data, err := os.ReadFile("../../k8s/alerts/5xx-rate.json")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Condition struct {
			CompositeQuery struct {
				Queries []struct {
					Type string `json:"type"`
					Spec struct {
						Name       string `json:"name"`
						Expression string `json:"expression"`
					} `json:"spec"`
				} `json:"queries"`
			} `json:"compositeQuery"`
			SelectedQueryName string `json:"selectedQueryName"`
			Thresholds        struct {
				Spec []struct {
					Target    float64 `json:"target"`
					Op        string  `json:"op"`
					MatchType string  `json:"matchType"`
				} `json:"spec"`
			} `json:"thresholds"`
		} `json:"condition"`
		Evaluation struct {
			Kind string `json:"kind"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("k8s/alerts/5xx-rate.json: %v", err)
	}

	// The formula as provisioned, pinned: editing it is a decision this
	// test exists to force.
	const want = "(A * 100 / B) * ((A - 2.5) / abs(A - 2.5) + 1) / 2"
	var formula string
	for _, q := range r.Condition.CompositeQuery.Queries {
		if q.Type == "builder_formula" {
			formula = q.Spec.Expression
		}
	}
	if formula != want {
		t.Errorf("F1 = %q, want %q (a share that is 0 below three failures in the window)", formula, want)
	}

	// What the pinned formula computes: the share, times a gate that is
	// 0 while A < 3 and 1 once A >= 3 (A is a count, never 2.5).
	gatedShare := func(a, b float64) float64 {
		return (a * 100 / b) * (((a - 2.5) / math.Abs(a-2.5)) + 1) / 2
	}
	// The firing of 2026-10-17 and its neighbour, both below the floor.
	for _, c := range [][2]float64{{1, 18}, {2, 18}} {
		if share := gatedShare(c[0], c[1]); share > 5 {
			t.Errorf("share of %v failures among %v = %v, want the floor to hold it at 0", c[0], c[1], share)
		}
	}
	// The storm of 2026-10-16, 8 of 141, still fires.
	if share := gatedShare(8, 141); share <= 5 {
		t.Errorf("share of 8 failures among 141 = %v, want the 5.67 %% of the storm, above the target", share)
	}
	// Three failures on a quiet service is a storm a share may name.
	if share := gatedShare(3, 18); share <= 5 {
		t.Errorf("share of 3 failures among 18 = %v, want 16.7 %%, above the target", share)
	}

	// The threshold itself is unchanged: the floor edits the share's
	// reach, not what a storm means.
	th := r.Condition.Thresholds.Spec[0]
	if r.Condition.SelectedQueryName != "F1" || th.Target != 5 || th.Op != "1" || th.MatchType != "3" {
		t.Errorf("selectedQueryName %q, target %v, op %q, matchType %q, want F1, 5, op \"1\", matchType \"3\"",
			r.Condition.SelectedQueryName, th.Target, th.Op, th.MatchType)
	}

	// The floor rides on the evaluation rolling the window into one
	// sample (the formula's A is the window's total); a timewise rule
	// would gate each minute alone, and a drip of two failures a minute
	// would never reach the floor.
	if r.Evaluation.Kind != "rolling" {
		t.Errorf("evaluation.kind = %q, want \"rolling\"", r.Evaluation.Kind)
	}
}

// A restart rule counts each container start once (card #1225): SigNoz
// stored roll #1224's one start of factoryd three times, the copies
// differing only in their arrival, and count() read three starts in ten
// minutes. A copy keeps the event's own time; a crash restart in the same
// pod moves it (Forgejo's crash loop of 2026-10-18: one event, counts 2 to
// 10, nine times). Both Deployments run one replica, so no two starts share
// a second.
func TestRestartRulesCountEachStartOnce(t *testing.T) {
	for _, f := range []string{"../../k8s/alerts/restarts-factoryd.json", "../../k8s/alerts/restarts-gateway.json"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Condition struct {
				CompositeQuery struct {
					Queries []struct {
						Spec struct {
							Aggregations []struct {
								Expression string `json:"expression"`
							} `json:"aggregations"`
						} `json:"spec"`
					} `json:"queries"`
				} `json:"compositeQuery"`
			} `json:"condition"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		q := r.Condition.CompositeQuery.Queries
		if len(q) != 1 || len(q[0].Spec.Aggregations) != 1 || q[0].Spec.Aggregations[0].Expression != "count_distinct(timestamp)" {
			t.Errorf("%s aggregates %+v, want count_distinct(timestamp): one count per start, not per stored copy", f, q)
		}
	}
}
