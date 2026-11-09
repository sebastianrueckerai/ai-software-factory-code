package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestLabScenarioOpensAPlan proves lab_scenario_opens_plan of the
// show-doc card (roadmap 280): the lab holds a scenario whose chat
// opens the build plan — a turn of the seeded conversation carrying a
// show_doc call of docs/plans/2026-09-24-build-plan.md — and the
// scenario seeds the rig, whose own stream for that call rides the
// view event right after the tool's block, the plan reading as the
// lab's own document.
func TestLabScenarioOpensAPlan(t *testing.T) {
	r, _, g := newChatRig(t)
	withDocs(t, r, labDocs)
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}

	// a scenario whose conversation opens the build plan
	scene, names := "", make([]string, 0, len(labScenes))
	for name, scenario := range labScenes {
		names = append(names, name)
		for _, reply := range scenario(time.Now()).replies {
			for _, chunk := range reply {
				for _, c := range chunk.Call {
					if c[0] == "show_doc" && strings.Contains(c[1], "docs/plans/2026-09-24-build-plan.md") {
						scene = name
					}
				}
			}
		}
	}
	if scene == "" {
		t.Errorf("no lab scenario's chat opens docs/plans/2026-09-24-build-plan.md; the scenarios are %s",
			strings.Join(slices.Sorted(slices.Values(names)), ", "))
	}
	if _, err := r.labSeed(scene); err != nil {
		t.Fatalf("seed the scenario: %v", err)
	}

	// the scenario's open rides the rig's own stream
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(0, "show_doc", `{"path":"docs/plans/2026-09-24-build-plan.md"}`)}, {say("The plan is open.")}}
	_, ev, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"show the build plan"}})
	at := slices.IndexFunc(ev, func(e string) bool {
		return strings.HasPrefix(e, "tool ") && strings.Contains(e, `"name":"show_doc"`)
	})
	if at < 0 {
		t.Fatalf("the turn holds no show_doc tool block:\n%s", strings.Join(ev, "\n"))
	}
	if !strings.Contains(ev[at], "docs/plans/2026-09-24-build-plan.md is open in your document view.") {
		t.Errorf("the block's result does not name the open:\n%s", ev[at])
	}
	if want := `view {"doc":"docs/plans/2026-09-24-build-plan.md"}`; at+1 >= len(ev) || ev[at+1] != want {
		t.Errorf("the open does not ride the tool's block:\n%s", strings.Join(ev, "\n"))
	}

	// the plan the scenario opens reads as the lab's own document
	res := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/docs/docs/plans/2026-09-24-build-plan.md", nil), ck)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "The build plan") {
		t.Errorf("the lab's plan reads %d:\n%s", res.Code, res.Body.String())
	}
}
