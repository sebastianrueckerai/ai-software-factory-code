package factoryd

import (
	"encoding/json"
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
	files, err := filepath.Glob("../../k8s/alerts/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("rules: %v %v", files, err)
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
