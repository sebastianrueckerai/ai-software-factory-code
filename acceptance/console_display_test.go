package acceptance

// Package acceptance holds the plan's acceptance tests. This file is the
// two display fixes (roadmap 317) end to end, read through the rig's
// console as sebastian, the project's writer: a running card whose
// session supervisor redirected it shows the decision in its drawer's
// Supervisor column, and a plan child without a writer's summary is
// titled by its objective's headline on the board and in the drawer
// while its key stays on the plan chip.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A running card's drawer reads the session supervisor's check-in step:
// the one checkin its timeline holds, written with the detector that
// tripped and the guidance, renders its redirect decision in the steps
// table's Supervisor column on the Activity tab.
func TestAcceptanceCheckinDecisionInDrawer(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	ck, _ := r.Login(t)
	// the supervisor redirected the running card's session: the runner
	// recorded one check-in step beside the detector and the guidance
	r.timelines["sessions/t-r/timeline.jsonl"] = []byte(
		`{"time":"2026-10-29T09:12:00Z","step":"checkin","session":"s-1","detector":"no_write","trip":"no write in 8 min","digest":"read console.css twice","decision":"redirect","guidance":"run the failing acceptance test","reason":"","called":true}` + "\n")
	rec := r.request(http.MethodGet, "/cards/t-r/drawer?tab=activity", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the running card's activity = %d:\n%s", rec.Code, rec.Body)
	}
	activity := rec.Body.String()
	if got := strings.Count(activity, "<td>checkin</td>"); got != 1 {
		t.Errorf("%d checkin rows on the activity, want the one supervisor step:\n%s", got, activity)
	}
	if want := `<td>checkin</td><td><strong>redirect</strong>: run the failing acceptance test</td>`; !strings.Contains(activity, want) {
		t.Errorf("the Supervisor column lacks the decision %s:\n%s", want, activity)
	}
}

// A plan child without a writer's summary is titled by its objective's
// headline: the board's face carries it as the card's title, the drawer
// heads with it, and the plan chip keeps the key after the plan's number.
func TestAcceptancePlanChildHeadlineAndChip(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	ck, _ := r.Login(t)
	// the plan child among the rig's tasks: filed from the accepted plan
	// #531 with its key, no summary, its objective's first line the title
	// the issue was filed under
	r.mu.Lock()
	r.tasks = append(r.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-plan531", Kind: "agent", Status: "running", Track: policy.TrackPlan,
			Process: "spec", Repository: "sf/code", IssueNumber: 531, Objective: "Console: two display defects from the demo"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-drawer", Kind: "agent", Status: "queued", Track: policy.TrackDirect,
			Process: "spec", Repository: "sf/code", IssueNumber: 535, Source: factoryd.SourcePlan, Parent: "t-plan531",
			PlanKey:   "drawer-files",
			Objective: "Drawer: the Activity tab shows the redirect\n\nCard drawer-files of plan #531 (3 of 3), filed for sebastian. Written by the planner.\n\nShow the supervisor's check-in decision."}})
	r.mu.Unlock()
	body := r.request(http.MethodGet, "/", nil, ck).Body.String()
	headline := "Drawer: the Activity tab shows the redirect"
	if want := `<strong class="title" title="` + headline + `"><a href="#card=t-drawer" class="open">` + headline + `</a></strong>`; !strings.Contains(body, want) {
		t.Errorf("the plan child's face is not titled by its headline %s:\n%s", want, body)
	}
	if n := strings.Count(body, ">drawer-files</a>") + strings.Count(body, ">drawer-files</strong>"); n != 0 {
		t.Errorf("%d faces title the plan child by its bare key", n)
	}
	if got := strings.Count(body, `<span class="chip plan">from plan #531 · drawer-files</span>`); got != 1 {
		t.Errorf("%d plan chips read from plan #531 · drawer-files, want the child's one:\n%s", got, body)
	}
	drawer := r.request(http.MethodGet, "/cards/t-drawer/drawer", nil, ck).Body.String()
	if want := `<h2 tabindex="-1">` + headline + `</h2>`; !strings.Contains(drawer, want) {
		t.Errorf("the plan child's drawer does not head with its headline %s:\n%s", want, drawer)
	}
}
