package pg

import (
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The helpers the rung 2, monitor and cases_events files share with
// factoryd's own test files and cannot reach across the package line:
// copies, kept verbatim except where they reach the surface.

// spentStop is a check-in's spend stop that names neither the call cap nor the
// time cap: its card asks, and a human's answer verifies an open head.
const spentStop = "the card's spend is above the policy's bound"

func testSupervisorModel(url string) *SupervisorModel {
	return &SupervisorModel{
		GatewayURL: url,
		Token:      "gw-token",
		Budget:     TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000},
	}
}

// wakeAsk records one ask of task on repository, via chat when the agent
// itself filed it, and returns the event's id.
func wakeAsk(t *testing.T, s *factoryd.PGStore, repository, task, ask, via string) int64 {
	t.Helper()
	id, err := s.Record(t.Context(), Event{Repository: repository, TaskID: task, Kind: EventQuestionAsked, Via: via,
		Detail: map[string]any{"ask": ask}})
	if err != nil {
		t.Fatalf("record the ask: %v", err)
	}
	return id
}

func (g rungRig) infoOf(t *testing.T, id int64) string { return g.askedOf(t, id, "info") }

func (g rungRig) askedOf(t *testing.T, id int64, key string) string {
	t.Helper()
	for _, e := range g.asks(t) {
		if n, _ := toInt(e.Detail["case_id"]); int64(n) == id {
			s, _ := e.Detail[key].(string)
			return s
		}
	}
	t.Fatalf("no question asked on case %d", id)
	return ""
}

// testPlan is a valid plan of four cards (the foundation, two features
// with redirects on links-api, and the integration) carrying the spec
// (piece 5) and its three criteria: two tests and one invariant, every
// criterion cited by the card that works on it, the integration card one
// item per criterion. Each test criterion names its suite and its test's
// name, and a card whose directories hold the suite carries it: links-api
// links_crud, redirects redirect_302, both in internal/links.
func testPlan() Plan {
	return Plan{Spec: &Spec{Outcome: "A link shortener: links with slugs that redirect and count clicks.",
		Glossary: []string{"slug: the short key a link gets", "click: one redirect served"}},
		Summary: "The foundation first, then links and redirects, then the acceptance tests.", Criteria: []PlanCriterion{
			{ID: "links_crud", Kind: CriterionTest, Claim: "a link created through the API is stored", Inputs: "POST /api/links with a url", Expected: "201 and the row in the store", Suite: "internal/links", Name: "TestCreateLink"},
			{ID: "redirect_302", Kind: CriterionTest, Claim: "a slug redirects", Inputs: "GET /abc", Expected: "302", Suite: "internal/links", Name: "TestRedirect"},
			{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."},
		}, Cards: []PlanCard{
			{Key: "foundation", Role: RoleFoundation, Title: "Foundation: layout, contract, schema", Brief: "Lay out cmd/server, the store and one end-to-end test.",
				Directories: []string{"cmd/server", "internal/"}, Acceptance: []PlanItem{{Criterion: "all_green", Text: "go test ./... passes with TestEndToEndHealth"}}, Size: "medium"},
			{Key: "links-api", Role: RoleFeature, Title: "Links API", Brief: "Create, list and delete links.",
				Directories: []string{"internal/links"}, Acceptance: []PlanItem{{Criterion: "links_crud", Text: "POST /api/links answers 201"}}, Size: "small"},
			{Key: "redirects", Role: RoleFeature, Title: "Redirects and clicks", Brief: "GET /<slug> answers 302 and counts a click.",
				DependsOn: []string{"links-api"}, Directories: []string{"internal/links"}, Acceptance: []PlanItem{{Criterion: "redirect_302", Text: "GET /abc answers 302"}}, Size: "small"},
			{Key: "integration", Role: RoleIntegration, Title: "Integration: acceptance tests", Brief: "Turn A1 to A9 into acceptance/ tests.",
				Directories: []string{"acceptance/", "internal/links"}, Acceptance: []PlanItem{
					{Criterion: "all_green", Text: "go test ./... passes"},
					{Criterion: "links_crud", Text: "TestCreateLink in internal/links: POST /api/links with a url: 201 and the row in the store"},
					{Criterion: "redirect_302", Text: "TestRedirect in internal/links: GET /abc: 302"},
				}, Size: "medium"},
		}}
}
