package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

var (
	testShape  = policy.PlanShape{MaxCards: 12, Foundation: true, Integration: true}
	testLimits = policy.Limits{AgentTime: 20 * time.Minute, Steps: 200}
)

// validatePlan is readPlan's findings alone.
func validatePlan(text string, shape policy.PlanShape, limits policy.Limits, pins map[string]policy.ImagePin) []string {
	_, findings := readPlan(text, shape, limits, pins)
	return findings
}

// testPlan is a valid plan of four cards (the foundation, two features
// with redirects on links-api, and the integration) carrying the spec
// (piece 5) and its three criteria: two tests and one invariant, every
// criterion cited by the card that works on it, the integration card one
// item per criterion.
func testPlan() Plan {
	return Plan{Spec: &Spec{Outcome: "A link shortener: links with slugs that redirect and count clicks.",
		Glossary: []string{"slug: the short key a link gets", "click: one redirect served"}},
		Summary: "The foundation first, then links and redirects, then the acceptance tests.", Criteria: []PlanCriterion{
			{ID: "links_crud", Kind: CriterionTest, Test: "TestCreateLink in internal/links", Inputs: "POST /api/links with a url", Expected: "201 and the row in the store"},
			{ID: "redirect_302", Kind: CriterionTest, Test: "TestRedirect in internal/links", Inputs: "GET /abc", Expected: "302"},
			{ID: "all_green", Kind: CriterionInvariant, Test: "go test ./... passes"},
		}, Cards: []PlanCard{
			{Key: "foundation", Role: RoleFoundation, Title: "Foundation: layout, contract, schema", Brief: "Lay out cmd/server, the store and one end-to-end test.",
				Directories: []string{"cmd/server", "internal/"}, Acceptance: []PlanItem{{Criterion: "all_green", Text: "go test ./... passes with TestEndToEndHealth"}}, Size: "medium"},
			{Key: "links-api", Role: RoleFeature, Title: "Links API", Brief: "Create, list and delete links.",
				Directories: []string{"internal/links"}, Acceptance: []PlanItem{{Criterion: "links_crud", Text: "POST /api/links answers 201"}}, Size: "small"},
			{Key: "redirects", Role: RoleFeature, Title: "Redirects and clicks", Brief: "GET /<slug> answers 302 and counts a click.",
				DependsOn: []string{"links-api"}, Acceptance: []PlanItem{{Criterion: "redirect_302", Text: "GET /abc answers 302"}}, Size: "small"},
			{Key: "integration", Role: RoleIntegration, Title: "Integration: acceptance tests", Brief: "Turn A1 to A9 into acceptance/ tests.",
				Directories: []string{"acceptance/"}, Acceptance: []PlanItem{
					{Criterion: "all_green", Text: "go test ./... passes"},
					{Criterion: "links_crud", Text: "TestCreateLink in internal/links: POST /api/links with a url: 201 and the row in the store"},
					{Criterion: "redirect_302", Text: "TestRedirect in internal/links: GET /abc: 302"},
				}, Size: "medium"},
		}}
}

func fencePlan(p Plan) string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return "Here is the plan.\n```json\n" + string(b) + "\n```\n"
}

// padded is testPlan's block padded with leading blanks to size bytes.
func padded(size int) string {
	b, _ := json.Marshal(testPlan())
	return "```json\n" + strings.Repeat(" ", size-len(b)) + string(b) + "\n```"
}

// The size table derives from the risk bound (roadmap 159, 184): large is
// large_lines, medium 60 percent of it, small 30 percent, rounded to whole
// lines; a bound of 1000 reproduces the old figures 300/600/1000, and no
// bound keeps them.
func TestSizeLinesDerivesFromTheBound(t *testing.T) {
	for _, tc := range []struct{ bound, small, medium, large int }{
		{1000, 300, 600, 1000},
		{400, 120, 240, 400},
		{333, 100, 200, 333},
		{0, 300, 600, 1000},
	} {
		got := sizeLines(tc.bound)
		for size, want := range map[string]int{"small": tc.small, "medium": tc.medium, "large": tc.large} {
			if got[size] != want {
				t.Fatalf("sizeLines(%d)[%s] = %d, want %d (all: %v)", tc.bound, size, got[size], want, got)
			}
		}
	}
}

// Every size of the ladder files at any bound: the ladder's lines are
// derived from the risk bound, so measuring them back against it judges
// nothing about the card. A size the ladder does not name is refused.
func TestValidatePlanTakesEveryCardSize(t *testing.T) {
	p := testPlan()
	p.Cards[1].Size = "large"
	if got := validatePlan(fencePlan(p), testShape, testLimits, nil); len(got) != 0 {
		t.Fatalf("a large card: validatePlan = %q, want no finding", got)
	}
	p.Cards[1].Size = "huge"
	if got := validatePlan(fencePlan(p), testShape, testLimits, nil); len(got) != 1 ||
		got[0] != "plan: card links-api: size huge: split it; a card is one package, one page or one set of endpoints" {
		t.Fatalf("a size the ladder does not name = %q, want the taxonomy line", got)
	}
}

func TestValidatePlan(t *testing.T) {
	change := func(edit func(p *Plan)) string {
		p := testPlan()
		edit(&p)
		return fencePlan(p)
	}
	fourteen := change(func(p *Plan) {
		for i := range 10 {
			p.Cards = slices.Insert(p.Cards, 3, PlanCard{Key: "extra-" + strconv.Itoa(i), Role: RoleFeature, Title: "x", Brief: "x", Acceptance: []PlanItem{{Criterion: "links_crud", Text: "x"}}, Size: "small"})
		}
	})
	dirs := make([]string, 11)
	for i := range dirs {
		dirs[i] = "d" + strconv.Itoa(i)
	}
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"valid":              {fencePlan(testPlan()), nil},
		"over the old bound": {padded(20 << 10), nil},
		"no block":           {"I could not plan this.", []string{"plan: no fenced json block in the last message"}},
		"unknown field":      {"```json\n{\"summary\": \"s\", \"cards\": [], \"extra\": 1}\n```", []string{`plan: json: unknown field "extra"`}},
		"cut off":            {"```json\n{\"summary\": \"s\",\n```", []string{"plan: json: unexpected EOF"}},
		"trailing data":      {"```json\n{\"summary\": \"s\", \"cards\": []} {}\n```", []string{"plan: json: data after the plan's object"}},
		"too many cards":     {fourteen, []string{"plan: 14 cards; this process allows 2 to 12"}},
		"key":                {change(func(p *Plan) { p.Cards[2].Key = "Links API" }), []string{`plan: card 3: key "Links API": lowercase letters, digits and dashes, 2 to 32`}},
		"key twice":          {change(func(p *Plan) { p.Cards[2].Key, p.Cards[2].DependsOn = "links-api", nil }), []string{"plan: card key links-api is used twice"}},
		"role":               {change(func(p *Plan) { p.Cards[2].Role = "extra" }), []string{`plan: card redirects: role "extra", want one of [foundation feature integration]`}},
		"no foundation":      {change(func(p *Plan) { p.Cards[0].Role = RoleFeature }), []string{"plan: 0 cards with role foundation; the process wants exactly one"}},
		"foundation waits":   {change(func(p *Plan) { p.Cards[0].DependsOn = []string{"links-api"} }), []string{"plan: card foundation: a foundation card depends on nothing"}},
		"no integration":     {change(func(p *Plan) { p.Cards[3].Role = RoleFeature }), []string{"plan: 0 cards with role integration; the process wants exactly one"}},
		"on integration":     {change(func(p *Plan) { p.Cards[2].DependsOn = []string{"links-api", "integration"} }), []string{"plan: card redirects: depends on the integration card, which comes last"}},
		"unknown card":       {change(func(p *Plan) { p.Cards[2].DependsOn = []string{"qr-codes"} }), []string{`plan: card redirects: depends on "qr-codes", which is no card of the plan`}},
		"itself":             {change(func(p *Plan) { p.Cards[2].DependsOn = []string{"redirects"} }), []string{"plan: card redirects: depends on itself"}},
		"cycle":              {change(func(p *Plan) { p.Cards[1].DependsOn = []string{"redirects"} }), []string{"plan: the dependencies form a cycle: links-api -> redirects -> links-api"}},
		"large":              {change(func(p *Plan) { p.Cards[2].Size = "large" }), nil},
		"size":               {change(func(p *Plan) { p.Cards[2].Size = "huge" }), []string{"plan: card redirects: size huge: split it; a card is one package, one page or one set of endpoints"}},
		"no size":            {change(func(p *Plan) { p.Cards[2].Size = "" }), []string{`plan: card redirects: size "": want small, medium or large`}},
		"no title":           {change(func(p *Plan) { p.Cards[2].Title = "" }), []string{"plan: card redirects: title empty"}},
		"no brief":           {change(func(p *Plan) { p.Cards[2].Brief = "" }), []string{"plan: card redirects: brief empty"}},
		"many directories":   {change(func(p *Plan) { p.Cards[2].Directories = dirs }), []string{"plan: card redirects: 11 directories; want 0 to 10"}},
		"outside":            {change(func(p *Plan) { p.Cards[2].Directories = []string{"../x", "/etc", "web/../../y"} }), []string{`plan: card redirects: directory "../x": a relative path inside the repository`, `plan: card redirects: directory "/etc": a relative path inside the repository`, `plan: card redirects: directory "web/../../y": a relative path inside the repository`}},
		"workflow dir":       {change(func(p *Plan) { p.Cards[2].Directories = []string{".forgejo/workflows", "./.github", ".Gitea"} }), []string{`plan: card redirects: directory ".forgejo/workflows": closed to agents, the template owns it`, `plan: card redirects: directory "./.github": closed to agents, the template owns it`, `plan: card redirects: directory ".Gitea": closed to agents, the template owns it`}},
		"no acceptance":      {change(func(p *Plan) { p.Cards[0].Acceptance = nil }), []string{"plan: card foundation: 0 acceptance tests; want 1 to 10"}},
		"empty acceptance":   {change(func(p *Plan) { p.Cards[0].Acceptance = []PlanItem{{}} }), []string{"plan: card foundation: acceptance test 1 is empty"}},
		"eleven acceptance":  {change(func(p *Plan) { p.Cards[0].Acceptance = slices.Repeat([]PlanItem{{Text: "a"}}, 11) }), []string{"plan: card foundation: 11 acceptance tests; want 1 to 10"}},
		// A spec plan (piece 5) drafts the spec the members confirm; the
		// finding is the same for none and for a blank outcome.
		"no spec":       {change(func(p *Plan) { p.Spec = nil }), []string{`plan: no spec: draft the spec first, {"spec": {"outcome": "the outcome in a few sentences", "glossary": ["term: what it is"]}}; the members confirm it before the criteria are fixed`}},
		"blank outcome": {change(func(p *Plan) { p.Spec.Outcome = "  " }), []string{`plan: no spec: draft the spec first, {"spec": {"outcome": "the outcome in a few sentences", "glossary": ["term: what it is"]}}; the members confirm it before the criteria are fixed`}},
		// The criteria cases edit one field of the criteria block or one
		// citation; a broken criterion stays properly cited and a broken
		// citation changes no criterion, so each case yields only the
		// findings listed.
		"no criteria": {change(func(p *Plan) { p.Criteria = nil }), []string{"plan: 0 criteria; want 1 to 10"}},
		"criterion id": {change(func(p *Plan) {
			p.Criteria = append(p.Criteria, PlanCriterion{ID: "Links API", Kind: CriterionInvariant, Test: "go test ./... passes"})
			p.Cards[1].Acceptance = append(p.Cards[1].Acceptance, PlanItem{Criterion: "Links API", Text: "x"})
			p.Cards[3].Acceptance = append(p.Cards[3].Acceptance, PlanItem{Criterion: "Links API", Text: "x"})
		}), []string{`plan: criterion 4: id "Links API": lowercase letters, digits and underscores, 2 to 32`}},
		"criterion id twice": {change(func(p *Plan) { p.Criteria = append(p.Criteria, p.Criteria[0]) }), []string{"plan: criterion id links_crud is used twice"}},
		"criterion kind": {change(func(p *Plan) {
			p.Criteria = append(p.Criteria, PlanCriterion{ID: "perf_test", Kind: "perf", Test: "t", Inputs: "i", Expected: "e"})
			p.Cards[1].Acceptance = append(p.Cards[1].Acceptance, PlanItem{Criterion: "perf_test", Text: "x"})
			p.Cards[3].Acceptance = append(p.Cards[3].Acceptance, PlanItem{Criterion: "perf_test", Text: "x"})
		}), []string{`plan: criterion perf_test: kind "perf", want one of [test invariant judgement human]`}},
		"test incomplete":    {change(func(p *Plan) { p.Criteria[0].Inputs = "" }), []string{"plan: criterion links_crud: kind test names its test, its inputs and its expected numbers"}},
		"test overfull":      {change(func(p *Plan) { p.Criteria[0].Claim = "x" }), []string{"plan: criterion links_crud: kind test carries no claim, prompt or evidence"}},
		"invariant overfull": {change(func(p *Plan) { p.Criteria[2].Expected = "no failures" }), []string{"plan: criterion all_green: kind invariant carries no inputs, expected numbers, claim, prompt or evidence"}},
		"judgement incomplete": {change(func(p *Plan) {
			p.Criteria = append(p.Criteria, PlanCriterion{ID: "reads_calm", Kind: CriterionJudgement, Claim: "the chat reads calm", Inputs: "the seeded issues"})
			p.Cards[2].Acceptance = append(p.Cards[2].Acceptance, PlanItem{Criterion: "reads_calm", Text: "the chat reads calm"})
			p.Cards[3].Acceptance = append(p.Cards[3].Acceptance, PlanItem{Criterion: "reads_calm", Text: "the chat reads calm"})
		}), []string{"plan: criterion reads_calm: kind judgement names its claim, its fixed inputs and its pinned prompt"}},
		"human incomplete": {change(func(p *Plan) {
			p.Criteria = append(p.Criteria, PlanCriterion{ID: "member_ok", Kind: CriterionHuman, Claim: "a member can file a card"})
			p.Cards[2].Acceptance = append(p.Cards[2].Acceptance, PlanItem{Criterion: "member_ok", Text: "a member can file a card"})
			p.Cards[3].Acceptance = append(p.Cards[3].Acceptance, PlanItem{Criterion: "member_ok", Text: "a member can file a card"})
		}), []string{"plan: criterion member_ok: kind human names its claim and the evidence the approver sees"}},
		"unknown citation": {change(func(p *Plan) { p.Cards[1].Acceptance[0].Criterion = "gone" }), []string{`plan: card links-api: acceptance item 1 cites "gone", which is no criterion of the plan`}},
		"criterion uncited": {change(func(p *Plan) {
			p.Criteria = append(p.Criteria, PlanCriterion{ID: "member_ok", Kind: CriterionHuman, Claim: "a member can file a card", Evidence: "the filed card"})
		}), []string{"plan: criterion member_ok is cited by no card", "plan: card integration: criterion member_ok is cited by no item"}},
		"card cites none": {change(func(p *Plan) { p.Cards[2].Acceptance[0].Criterion = "" }), []string{"plan: card redirects: no acceptance item cites a criterion"}},
		"misses one":      {change(func(p *Plan) { p.Cards[3].Acceptance = p.Cards[3].Acceptance[:2] }), []string{"plan: card integration: criterion redirect_302 is cited by no item"}},
		"cites twice":     {change(func(p *Plan) { p.Cards[3].Acceptance[2].Criterion = "links_crud" }), []string{"plan: card integration: criterion links_crud is cited by 2 items; want one", "plan: card integration: criterion redirect_302 is cited by no item"}},
		"item uncited":    {change(func(p *Plan) { p.Cards[3].Acceptance = append(p.Cards[3].Acceptance, PlanItem{Text: "an extra check"}) }), []string{"plan: card integration: acceptance item 4 cites no criterion"}},
		"empty item text": {change(func(p *Plan) { p.Cards[1].Acceptance[0].Text = "" }), []string{"plan: card links-api: acceptance test 1 is empty"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validatePlan(tc.text, testShape, testLimits, nil); !slices.Equal(got, tc.want) {
				t.Fatalf("validatePlan = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The criteria are capped at the acceptance cap, ten: the integration
// card carries one item per criterion inside its own cap, so a spec plan
// wants at least one criterion too; a shape without an integration card
// (a split's) refuses only the overrun. Every extra criterion is complete
// but uncited, so the count is the first finding either way.
func TestValidatePlanCapsCriteriaAtTen(t *testing.T) {
	at := func(n int) Plan {
		p := testPlan()
		for i := range n - 3 {
			p.Criteria = append(p.Criteria, PlanCriterion{ID: "extra_" + strconv.Itoa(i), Kind: CriterionTest, Test: "t", Inputs: "i", Expected: "e"})
		}
		return p
	}
	if got := validatePlan(fencePlan(at(11)), testShape, testLimits, nil); len(got) == 0 || got[0] != "plan: 11 criteria; want 1 to 10" {
		t.Fatalf("readPlan's first finding = %q, want the criteria count", got)
	}
	p := at(11)
	p.Cards = p.Cards[:2] // the foundation and links-api, no integration card
	p.Spec = nil          // a plan this process gives no integration card carries no spec to confirm
	if got := validatePlan(fencePlan(p), policy.PlanShape{MaxCards: 3}, testLimits, nil); len(got) == 0 || got[0] != "plan: 11 criteria; want at most 10" {
		t.Fatalf("readPlan's first finding = %q, want the criteria count", got)
	}
}

// The one-card minimum (roadmap 286): under min_cards 1 a plan of the
// integration card alone holds, the foundation rule waived because a
// single card is its own integration; a shape without min_cards keeps the
// two-card floor, and the one card still has to be the integration.
func TestValidatePlanOneCard(t *testing.T) {
	one := func(role string) Plan {
		p := testPlan()
		card := p.Cards[3]
		card.Role = role
		p.Cards = []PlanCard{card}
		return p
	}
	min1 := policy.PlanShape{MinCards: 1, MaxCards: 12, Foundation: true, Integration: true}
	if got := validatePlan(fencePlan(one(RoleIntegration)), min1, testLimits, nil); len(got) != 0 {
		t.Fatalf("validatePlan = %q, want no findings", got)
	}
	if got := validatePlan(fencePlan(one(RoleIntegration)), testShape, testLimits, nil); !slices.Equal(got, []string{"plan: 1 cards; this process allows 2 to 12"}) {
		t.Fatalf("validatePlan = %q, want the two-card floor", got)
	}
	if got := validatePlan(fencePlan(one(RoleFoundation)), min1, testLimits, nil); !slices.Equal(got, []string{"plan: 0 cards with role integration; the process wants exactly one"}) {
		t.Fatalf("validatePlan = %q, want the integration role finding", got)
	}
}

// The guide tells the planner a change that fits one card answers one
// card when the process allows it, and stays quiet when it does not.
func TestPlanGuideOneCard(t *testing.T) {
	want := "A change that fits one card answers one card, its own integration, citing every criterion."
	if got := planGuide(policy.PlanShape{MinCards: 1, MaxCards: 12, Foundation: true, Integration: true}, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
	if got := planGuide(testShape, testLimits, nil); strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want no one-card sentence under a two-card minimum", got)
	}
}

// A card names one of its project's pinned images, default or none: any
// other name is one finding naming the card, the image and the pinned
// names, and with no pins at hand (a split's check) every name but default
// is one.
func TestPlanCardNamesAPinnedImage(t *testing.T) {
	pins := map[string]policy.ImagePin{"video": {Tools: []string{"ffmpeg"}}, "browser": {Tools: []string{"chromium"}}}
	p := testPlan()
	p.Cards[1].Image = "browser"
	if got := validatePlan(fencePlan(p), testShape, testLimits, pins); len(got) != 0 {
		t.Fatalf("a pinned image: %q, want no finding", got)
	}
	p.Cards[2].Image = "gpu"
	want := []string{`plan: card redirects: image "gpu" is not one of this project's pinned images (browser, video)`}
	if got := validatePlan(fencePlan(p), testShape, testLimits, pins); !slices.Equal(got, want) {
		t.Fatalf("an unknown image: %q, want %q", got, want)
	}
	p.Cards[2].Image = "default" // pinned or not: an unpinned default runs the factory's image
	want = []string{`plan: card links-api: image "browser" is not one of this project's pinned images (none)`}
	if got := validatePlan(fencePlan(p), testShape, testLimits, nil); !slices.Equal(got, want) {
		t.Fatalf("no pins: %q, want %q", got, want)
	}
	// default is the card that names none: the plan read keeps it as ""
	read, findings := readPlan(fencePlan(p), testShape, testLimits, pins)
	if len(findings) != 0 || read.Cards[2].Image != "" || read.Cards[1].Image != "browser" {
		t.Fatalf("read plan's images %q, %q (%q); want \"\" for default, browser kept", read.Cards[2].Image, read.Cards[1].Image, findings)
	}
}

// The guide shows a card's image in the json shape and lists the images
// with their tools only when the project pins images.
func TestPlanGuideListsTheProjectsImages(t *testing.T) {
	pins := map[string]policy.ImagePin{"video": {Tools: []string{"ffmpeg"}}, "browser": {Tools: []string{"chromium", "fonts-liberation"}}}
	want := "Images: browser (chromium, fonts-liberation), video (ffmpeg); a card that needs one names it in image, any other card runs the default."
	got := planGuide(testShape, testLimits, pins)
	if !strings.Contains(got, want) || !strings.Contains(got, `"rung_reason": "one line", "image": "..."}`) {
		t.Fatalf("planGuide = %q, want the image field and %q", got, want)
	}
	if got := planGuide(testShape, testLimits, nil); strings.Contains(got, "Images:") || strings.Contains(got, `"image"`) || !strings.Contains(got, `"rung_reason": "one line"}]`) {
		t.Fatalf("planGuide without pins = %q, want no image field and no Images line", got)
	}
}

// stage12-design 5.2: a card is bounded by shape, not a session's time or
// step budget (ruling 221: the guide gives no time estimate any more).
func TestPlanGuideCardSizeSentence(t *testing.T) {
	want := "A card is one pull request by one agent: one package, one page, or one set of endpoints, with its tests, about ten files at most; a larger piece of work is two cards."
	if got := planGuide(testShape, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// Stage 16 item 3.7: the guide names the three sizes in the json shape
// and in the field rules. The line figures are readPlan's size
// findings (roadmap 186).
func TestPlanGuideSizes(t *testing.T) {
	got := planGuide(testShape, testLimits, nil)
	if !strings.Contains(got, `"size": "small|medium|large"`) ||
		!strings.Contains(got, "size: small, medium or large; anything bigger is two cards.") {
		t.Fatalf("planGuide = %q, want the three sizes", got)
	}
}

// stage12-design 5.2: acceptance items are named tests, not free text
// readPlan cannot check for. The count bound is readPlan's
// finding, not the prose (roadmap 186). The item cites the criterion it
// works on (the spec-intake concept, piece 2).
func TestPlanGuideAcceptanceRule(t *testing.T) {
	want := "acceptance: each item a test by name: TestXxx in <package>, its inputs and the numbers it expects, citing the criterion it works on; the foundation's items need cite none."
	if got := planGuide(testShape, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// The spec-intake concept (pieces 1 and 2, roadmap 285): the guide
// dictates the criteria block's json shape, the kinds' field rules, the
// citation rule, and the integration card's one item per criterion.
func TestPlanGuideCriteria(t *testing.T) {
	got := planGuide(testShape, testLimits, nil)
	for _, want := range []string{
		`"criteria": [{"id": "...", "kind": "test|invariant|judgement|human"`,
		`"acceptance": [{"criterion": "...", "text": "..."}]`,
		"criteria: the spec's proof, one object per criterion: the issue's criteria list when it carries one, else drafted by you from the spec's acceptance tests. " +
			"Each criterion has an id in snake_case and a kind naming the fields it uses: " +
			"kind test names its test as TestXxx in <package>, a test bash check runs in-process, against in-process fakes: " +
			"no browser, no external database, no device; a want beyond the sandbox becomes a human criterion or waits as a note; " +
			"kind invariant names its suite as go test <suite> passes, for example go test ./..., and carries no inputs; " +
			"kind judgement names the claim a model judges, its fixed inputs and the pinned prompt, and no test; " +
			"kind human names a claim only a person settles, with the evidence the approver will see.",
		"Every card but the foundation cites the criterion its acceptance item works on, and every criterion is cited by some card's item.",
		"An amendment of a filed revision adds a criterion, removes one (its citing card goes with it), or changes one under a new id that names the old one in replaces; an id never changes its definition.",
		"Its acceptance items are the criteria, one item per criterion by id; a judgement or human criterion arrives as an item that names its claim.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("planGuide = %q, want it to contain %q", got, want)
		}
	}
}

// The spec-intake concept (piece 5, roadmap 288): the guide names the
// spec block first, the skeleton the template ships, the members'
// confirmation that fixes the criteria, and the planner's one attack on
// its own draft.
func TestPlanGuideSpec(t *testing.T) {
	got := planGuide(testShape, testLimits, nil)
	for _, want := range []string{
		`{"spec": {"outcome": "...", "glossary": ["term: what it is"]}, "summary": "a paragraph: what gets built and in which order"`,
		"Draft the spec before the cards: the outcome in a few sentences, a glossary that names each component once, then the criteria; " +
			"when the repository carries a spec skeleton at docs/spec.md, draft into its shape and carry its standing criteria.",
		"spec: the outcome in a few sentences and the glossary that names each component once; the members confirm the spec in Needs you, and the criteria are fixed only when they answer.",
		"Attack your own draft once before you file: reread the spec, the criteria and the cards for contradictions, ambiguity and missing edge cases, " +
			"and turn each finding into a question with origin self_attack.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("planGuide = %q, want it to contain %q", got, want)
		}
	}
}

// stage12-design 5.2 (plan.go:502): the foundation's brief names the
// contracts every other card builds on.
func TestPlanGuideFoundationContractsSentence(t *testing.T) {
	want := "The foundation's brief names the contracts every card builds on: how a handler gets the logged-in user (the function and its package), " +
		"the store (its interface, its implementation's package, the methods stubbed), and the server constructor tests import (internal/server.New unless the template says otherwise)."
	if got := planGuide(testShape, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
	if strings.Contains(planGuide(policy.PlanShape{MaxCards: 2, Foundation: false, Integration: true}, testLimits, nil), want) {
		t.Fatal("a shape with no foundation card still gets the foundation's contracts sentence")
	}
}

// stage12-design 5.2: the planner is told every card's agent and CI run
// bash check, so it never asks a card to verify some other way.
func TestPlanGuideBashCheckLine(t *testing.T) {
	want := "Every card's agent and CI run bash check; do not ask a card to verify with a tool the check does not run."
	if got := planGuide(testShape, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// Roadmap 201: the harness reads AGENTS.md into every session itself, so
// the planner's guide does not ask for it.
func TestPlanGuideLeavesAGENTSmdToTheHarness(t *testing.T) {
	for _, shape := range []policy.PlanShape{testShape, {MaxCards: 12}} {
		if got := planGuide(shape, testLimits, nil); strings.Contains(got, "AGENTS.md") {
			t.Fatalf("planGuide = %q, want no AGENTS.md", got)
		}
	}
}

// The route-file rule stays in the prose: round2-design section 8 planned
// the template's AGENTS.md line, but no template's AGENTS.md carries it,
// so this sentence is the rule's only carrier.
func TestPlanGuideKeepsOneRouteFilePerCard(t *testing.T) {
	want := "Every card adds its routes in its own internal/server/routes_<key>.go, so no two cards edit one route list."
	if got := planGuide(testShape, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// The plan arrives by the file_plan tool, never as the last message
// (defect 178): RunPlan refuses a session that ends without filing one.
// The split guide keeps its last-message sentence: RunSplit still reads it.
func TestPlanGuideFilesThroughTheTool(t *testing.T) {
	want := "You file the whole result with the file_plan tool: its plan argument is the complete plan json, one object holding "
	if got := planGuide(testShape, testLimits, nil); !strings.Contains(got, want) {
		t.Fatalf("planGuide = %q, want it to contain %q", got, want)
	}
}

// factoryd, not the model, orders the foundation first and the integration
// last: every card depends on the foundation, the integration on every
// card, besides the edges the planner listed among the features.
func TestFactoryAddsFoundationAndIntegrationEdges(t *testing.T) {
	p := testPlan()
	p.Cards[1].DependsOn = []string{"foundation"} // listed as well: once
	got := planEdges(p)
	want := map[string][]string{
		"links-api":   {"foundation"},
		"redirects":   {"foundation", "links-api"},
		"integration": {"foundation", "links-api", "redirects"},
	}
	if len(got) != len(want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Fatalf("edges of %s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	// the integration listed first still files last
	p.Cards = append([]PlanCard{p.Cards[3]}, p.Cards[:3]...)
	var keys []string
	for _, c := range planOrder(p, planEdges(p)) {
		keys = append(keys, c.Key)
	}
	if !slices.Equal(keys, []string{"foundation", "links-api", "redirects", "integration"}) {
		t.Fatalf("order = %v", keys)
	}
}

// planForgejo is Forgejo's issue API for one repository as fileChildren
// and the plan's comment use it.
type planForgejo struct {
	mu        sync.Mutex
	issues    map[int64]*planIssue
	next      int64
	comments  []*stubComment
	calls     []string
	failLabel int // the nth labels PUT answers 500
	labelPuts int
	branches  map[string]string // the bot's fork: branch → head
	failCall  string            // the call answered failStatus once
	// failStatus is failCall's answer, 500 when 0
	failStatus int
	// onIssue runs as an issue is created, with its title
	onIssue func(title string)
}

type planIssue struct {
	title, body, poster, state string
	labels                     []string
	deps                       []int64
}

type stubComment struct {
	id     int64
	issue  int64
	body   string
	author string
}

func (f *planForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/linkshort/linkshort/")
		parts := strings.Split(rest, "/")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		call := r.Method + " " + rest
		if call == f.failCall {
			f.failCall = ""
			f.calls = append(f.calls, call)
			http.Error(w, "forgejo down", cmp.Or(f.failStatus, http.StatusInternalServerError))
			return
		}
		switch {
		case r.Method == "POST" && rest == "issues":
			if f.onIssue != nil {
				f.onIssue(body["title"].(string))
			}
			f.next++
			f.issues[f.next] = &planIssue{title: body["title"].(string), body: body["body"].(string), poster: "factory-agent"}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/linkshort/linkshort/issues/%d"}`, f.next, f.next)
		case r.Method == "GET" && rest == "issues":
			var list []map[string]any
			for n := int64(1); n <= f.next; n++ {
				if is := f.issues[n]; is != nil {
					list = append(list, map[string]any{"number": n, "title": is.title, "body": is.body, "user": map[string]string{"login": is.poster}})
				}
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "GET" && len(parts) == 3 && parts[2] == "labels":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			var list []map[string]any
			for _, l := range f.issues[n].labels {
				list = append(list, map[string]any{"name": l})
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "PUT" && len(parts) == 3 && parts[2] == "labels":
			f.labelPuts++
			if f.labelPuts == f.failLabel {
				w.WriteHeader(http.StatusInternalServerError)
				break
			}
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			f.issues[n].labels = nil
			for _, l := range body["labels"].([]any) {
				f.issues[n].labels = append(f.issues[n].labels, l.(string))
			}
			w.Write([]byte(`[]`))
		case len(parts) == 3 && parts[2] == "dependencies":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			if r.Method == "GET" {
				var list []map[string]any
				for _, d := range f.issues[n].deps {
					list = append(list, map[string]any{"number": d})
				}
				json.NewEncoder(w).Encode(list)
				break
			}
			if body["owner"] != "linkshort" || body["repo"] != "linkshort" {
				t.Errorf("dependency body %v", body)
			}
			f.issues[n].deps = append(f.issues[n].deps, int64(body["index"].(float64)))
			call = fmt.Sprintf("%s %v", call, body["index"])
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && len(parts) == 3 && parts[2] == "comments":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			var list []map[string]any
			for _, c := range f.comments {
				if c.issue == n {
					list = append(list, map[string]any{"id": c.id, "body": c.body, "user": map[string]string{"login": c.author}})
				}
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "POST" && len(parts) == 3 && parts[2] == "comments":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			c := &stubComment{id: int64(700 + len(f.comments)), issue: n, body: body["body"].(string), author: "factory-agent"}
			f.comments = append(f.comments, c)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, c.id)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/users/") && strings.HasSuffix(r.URL.Path, "/tokens"):
			json.NewEncoder(w).Encode([]any{})
		case r.Method == "GET" && len(parts) == 2 && parts[0] == "issues":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			fmt.Fprintf(w, `{"number":%d,"state":%q}`, n, f.issues[n].state)
		case r.Method == "GET" && strings.HasPrefix(rest, "pulls"):
			json.NewEncoder(w).Encode([]any{})
		case r.Method == "PATCH" && len(parts) == 2 && parts[0] == "issues":
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			f.issues[n].state = body["state"].(string)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/linkshort/branches/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/factory-agent/linkshort/branches/")
			call = "GET fork branch " + name
			head, ok := f.branches[name]
			if !ok {
				http.NotFound(w, r)
				break
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
		case r.Method == "POST" && r.URL.Path == "/api/v1/repos/factory-agent/linkshort/branches":
			call = fmt.Sprintf("POST fork branches %s", body["new_branch_name"])
			head, ok := f.branches[body["old_branch_name"].(string)]
			if !ok {
				http.Error(w, `{"message":"The old branch does not exist"}`, http.StatusNotFound)
				break
			}
			if _, exists := f.branches[body["new_branch_name"].(string)]; exists {
				http.Error(w, `{"message":"The branch already exists."}`, http.StatusConflict)
				break
			}
			f.branches[body["new_branch_name"].(string)] = head
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/linkshort/linkshort":
			w.Write([]byte(`{"default_branch":"main"}`))
		case r.Method == "GET" && rest == "branches/main":
			w.Write([]byte(`{"commit":{"id":"m1"}}`))
		case r.Method == "PATCH" && len(parts) == 3 && parts[1] == "comments":
			id, _ := strconv.ParseInt(parts[2], 10, 64)
			for _, c := range f.comments {
				if c.id == id {
					c.body = body["body"].(string)
				}
			}
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls = append(f.calls, call)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newPlanRunner is a runner on the fixture tree (version 7: linkshort
// follows spec, 20m of agent time, 200 steps) with plan task t-plan on
// issue #1, the planner's revision 1 of p stored.
func newPlanRunner(t *testing.T, p Plan) (*Runner, *fakeStore, *fakeObjectStore, *planForgejo) {
	t.Helper()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		IssueNumber: 1, Author: "alice", AuthorWriter: true, Priority: 2, Objective: "Link shortener\n\nBuild a link shortener."}
	fj := &planForgejo{issues: map[int64]*planIssue{1: {title: "Link shortener", poster: "alice"}}, next: 1}
	runner, _, store := newTestRunner(t, plan, newFakeKube(), &fakeObjectStore{})
	runner.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	runner.completion.Forgejo = runner.forgejo
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	raw, sha, err := planSHA(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(context.Background(), "t-plan", raw, sha); err != nil {
		t.Fatal(err)
	}
	return runner, store, runner.objects.(*fakeObjectStore), fj
}

// confirmSpec is the members' okay to the spec's confirmation (piece 5)
// on an earlier revision of t-plan: asked until answered once, so the
// revision under test asks no question.
func confirmSpec(t *testing.T, store *fakeStore) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", 0, PlanQuestion{Text: "Confirmation of the spec", Choices: []string{"okay"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AnswerPlanQuestion(ctx, "t-plan", 0, 1, "okay", "alice"); err != nil {
		t.Fatal(err)
	}
}

// fileRig is newPlanRunner's plan card running with its spec confirmed, so
// revision 1 asks no question, and workflows that record the starts.
func fileRig(t *testing.T, p Plan) (*Runner, *fakeStore, *fakeObjectStore, *planForgejo, *fakeWorkflows) {
	t.Helper()
	r, store, objects, fj := newPlanRunner(t, p)
	plan := store.tasks["t-plan"]
	plan.Status = "running"
	store.tasks["t-plan"] = plan
	confirmSpec(t, store)
	wf := &fakeWorkflows{}
	r.cfg.Workflows = wf
	return r, store, objects, fj, wf
}

// requestPlan runs RequestPlan of revision 1 under version 7.
func requestPlan(t *testing.T, r *Runner) PlanRef {
	t.Helper()
	val, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1, int64(7))
	return getInto[PlanRef](t, val, err)
}

// cardsByKey are the plan's cards filed, by key.
func cardsByKey(t *testing.T, store *fakeStore) map[string]Task {
	t.Helper()
	cards, err := store.PlanCards(context.Background(), "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Task{}
	for _, c := range cards {
		out[c.PlanKey] = c
	}
	return out
}

// browserInForce is linkshort's fixture with its browser image pinned, as
// the policy in force: a pin that landed after the plan task's version.
func browserInForce(t *testing.T) policy.Version {
	t.Helper()
	v := fixtureVersion(t)
	name, c, _ := v.ProjectOf("linkshort/linkshort")
	c.Images = map[string]policy.ImagePin{"browser": {Digest: "sha256:" + strings.Repeat("b", 64), Key: keyBrowser, Tools: []string{"chromium"}}}
	v.Projects[name] = c
	return v
}

// A card's image reaches its task row when the policy in force pins it for
// the project, the version taskImage resolves with, even when the pin
// landed after the plan task's version; an image no version in force pins
// refuses the filing.
func TestChildrenCarryTheirImage(t *testing.T) {
	p := testPlan()
	p.Cards[1].Image = "browser"
	r, _, _, _, _ := fileRig(t, p)
	r.cfg.Policy = func() policy.Version { return fixtureVersion(t) }
	if ref := requestPlan(t, r); len(ref.Filed) != 0 ||
		!slices.Equal(ref.Refused, []string{`plan: card links-api: image "browser" is not one of this project's pinned images (none)`}) {
		t.Fatalf("no pin: %+v, want the refusal", ref)
	}

	r, store, _, _, _ := fileRig(t, p)
	inForce := browserInForce(t)
	r.cfg.Policy = func() policy.Version { return inForce }
	if ref := requestPlan(t, r); len(ref.Filed) != 4 || len(ref.Refused) != 0 {
		t.Fatalf("pinned: %+v, want four cards", ref)
	}
	for key, want := range map[string]string{"foundation": "", "links-api": "browser", "redirects": "", "integration": ""} {
		if got := cardsByKey(t, store)[key].Image; got != want {
			t.Errorf("card %s image %q, want %q", key, got, want)
		}
	}
}

// A revision that asks no question files its cards so that every card's
// dependencies exist first: the foundation, the features, the integration;
// each queued on the direct track with its class at main's head, authored
// by the plan card's author (plans do not nest), labelled agent, with its
// Forgejo dependencies and task_deps, then started in that order; the
// plan's comment names their issues.
// A plan card an outside account filed (its plan label a writer's
// vouching) files cards with the poster's standing: no approver read the
// plan, so each child keeps its spec's author and runs the outsider's
// stages, the screening included.
func TestChildrenKeepTheirPlanAuthorsStanding(t *testing.T) {
	r, store, _, _, _ := fileRig(t, testPlan())
	plan := store.tasks["t-plan"]
	plan.Author, plan.AuthorWriter = "outsider", false
	store.tasks["t-plan"] = plan
	if ref := requestPlan(t, r); len(ref.Filed) != 4 {
		t.Fatalf("ref = %+v", ref)
	}
	for key, c := range cardsByKey(t, store) {
		if c.Author != "outsider" || c.AuthorWriter {
			t.Errorf("card %s: author %q, writer %v; want the outsider without standing", key, c.Author, c.AuthorWriter)
		}
	}
}

func TestChildrenFiledInTopologicalOrder(t *testing.T) {
	p := testPlan()
	p.Cards = []PlanCard{p.Cards[3], p.Cards[2], p.Cards[1], p.Cards[0]} // the model's order is no filing order
	r, store, objects, fj, wf := fileRig(t, p)
	ref := requestPlan(t, r)
	if !slices.Equal(ref.Filed, []int64{2, 3, 4, 5}) || len(ref.Refused) != 0 || ref.Revision != 1 || ref.Answered {
		t.Fatalf("ref = %+v", ref)
	}
	var titles []string
	for n := int64(2); n <= 5; n++ {
		titles = append(titles, fj.issues[n].title)
	}
	if !slices.Equal(titles, []string{"Foundation: layout, contract, schema", "Links API", "Redirects and clicks", "Integration: acceptance tests"}) {
		t.Fatalf("issues filed in order %q", titles)
	}
	cards := cardsByKey(t, store)
	var ids []string
	for _, key := range []string{"foundation", "links-api", "redirects", "integration"} {
		ids = append(ids, cards[key].ID)
	}
	if !slices.Equal(wf.started, ids) {
		t.Fatalf("started %v, want the cards in filing order %v", wf.started, ids)
	}
	for key, issue := range map[string]int64{"foundation": 2, "links-api": 3, "redirects": 4, "integration": 5} {
		c := cards[key]
		if c.IssueNumber != issue || c.Status != "queued" || c.Track != policy.TrackDirect || !slices.Equal(c.RequiredCapabilities, []string{policy.ClassCoder}) ||
			c.SHA != "m1" || c.Process != "spec" || c.Source != SourcePlan || c.Parent != "t-plan" || c.Author != "alice" || !c.AuthorWriter ||
			c.Kind != "agent" || c.Priority != 2 {
			t.Fatalf("card %s = %+v", key, c)
		}
		if !slices.Equal(fj.issues[issue].labels, []string{"agent", "state/queued"}) {
			t.Fatalf("issue #%d labels %v", issue, fj.issues[issue].labels)
		}
		if got := events(t, objects, c.ID); !slices.Equal(got, []string{"card_created direct alice"}) {
			t.Fatalf("card %s events %v", key, got)
		}
	}
	for key, want := range map[string][]int64{"foundation": nil, "links-api": {2}, "redirects": {2, 3}, "integration": {2, 3, 4}} {
		c := cards[key]
		if !slices.Equal(fj.issues[c.IssueNumber].deps, want) {
			t.Fatalf("issue #%d depends on %v in Forgejo, want %v", c.IssueNumber, fj.issues[c.IssueNumber].deps, want)
		}
		var ids []string
		for _, n := range want {
			for _, d := range cards {
				if d.IssueNumber == n {
					ids = append(ids, d.ID)
				}
			}
		}
		if !slices.Equal(store.deps[c.ID], ids) {
			t.Fatalf("task_deps of %s = %v, want %v", key, store.deps[c.ID], ids)
		}
	}
	redirects := fj.issues[4].body
	want := "Card redirects of plan #1 (3 of 4), filed for alice. Written by the planner.\n\nGET /<slug> answers 302 and counts a click.\n\n" +
		"Directories (guidance, not a boundary): none\nDepends on: #2 (foundation), #3 (links-api)\nAcceptance:\n- GET /abc answers 302 [redirect_302]\nSize: small\n\n" +
		"<!-- factoryd plan t-plan card redirects -->"
	if redirects != want {
		t.Fatalf("body of #4 =\n%s\nwant\n%s", redirects, want)
	}
	// only the integration card carries the spec (piece 7): the whole
	// feature's contract, its outcome and glossary
	if got := fj.issues[5].body; !strings.Contains(got, "\n\nThe spec of the whole feature:\nSpec: A link shortener: links with slugs that redirect and count clicks.\n"+
		"Glossary:\n- slug: the short key a link gets\n- click: one redirect served\n") || strings.Contains(fj.issues[2].body, "The spec of the whole feature") {
		t.Fatalf("the integration card's spec section:\n%s", got)
	}
	var onPlan []string
	for _, c := range fj.comments {
		if c.issue == 1 {
			onPlan = append(onPlan, c.body)
		}
	}
	if len(onPlan) != 1 || !strings.HasPrefix(onPlan[0], "factoryd: plan revision 1 of task t-plan, written by the planner. It asks no question, so its cards are filed") ||
		!strings.Contains(onPlan[0], "| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api | #4 |") {
		t.Fatalf("the plan's comments %q, want one naming the issues", onPlan)
	}
	if got, _, _ := store.Task(context.Background(), "t-plan"); got.Status != "running" || len(store.approvals) != 0 {
		t.Fatalf("plan task %s, approvals %+v; want it running, no request: nothing waits", got.Status, store.approvals)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"children_created [2 3 4 5]"}) {
		t.Fatalf("plan steps %v", steps)
	}
}

// A card's budget follows its size (policy budget 5 by default, 15 at
// most): small 2, medium 5, large 10, a large one at most the project's
// card maximum.
func TestChildrenBudgetBySize(t *testing.T) {
	p := testPlan()
	p.Cards[1].Size = "large"
	for _, tc := range []struct {
		budget *policy.ProjectBudget
		large  float64
	}{{nil, 10}, {&policy.ProjectBudget{MonthEUR: 50, TaskMaxEUR: 3}, 3}} {
		r, store, _, _, _ := fileRig(t, p)
		pinned := fixtureVersion(t)
		pinned.Document.Budget.TaskDefaultEUR, pinned.Document.Budget.TaskMaxEUR = 5, 15
		project := pinned.Projects["linkshort"]
		project.Budget = tc.budget
		pinned.Projects["linkshort"] = project
		r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
		if ref := requestPlan(t, r); len(ref.Filed) != 4 {
			t.Fatalf("ref %+v, want four cards", ref)
		}
		cards := cardsByKey(t, store)
		for key, want := range map[string]float64{"foundation": min(5, tc.large), "links-api": tc.large, "redirects": 2} {
			if got := cards[key].BudgetEUR; got != want {
				t.Errorf("project budget %+v: card %s budget %g, want %g", tc.budget, key, got, want)
			}
		}
	}
}

// A small card the plan files on the frontier rung gets a medium card's
// budget, 5 under the test policy; a small cheap card keeps a small one's 2.
func TestChildrenBudgetOfAFrontierCard(t *testing.T) {
	p := testPlan()
	p.Cards[1].Rung = RungFrontier
	r, store, _, _, _ := fileRig(t, p)
	if ref := requestPlan(t, r); len(ref.Filed) != 4 {
		t.Fatalf("ref %+v, want four cards", ref)
	}
	cards := cardsByKey(t, store)
	for key, want := range map[string]float64{"links-api": 5, "redirects": 2} {
		if got := cards[key].BudgetEUR; got != want {
			t.Errorf("card %s budget %g, want %g", key, got, want)
		}
	}
}

// planSteps are the plan task's plan steps on its timeline.
func planSteps(t *testing.T, objects *fakeObjectStore, id string) []string {
	t.Helper()
	b, _ := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e["step"] {
		case "children_created":
			out = append(out, fmt.Sprintf("children_created %v", e["issues"]))
		case "plan_revision":
			out = append(out, fmt.Sprintf("plan_revision %v %v", e["revision"], e["cards"]))
		case "plan_invalid", "plan_refused":
			out = append(out, fmt.Sprintf("%s %v", e["step"], e["findings"]))
		case "approval_requested":
			out = append(out, fmt.Sprintf("approval_requested %v", e["revision"]))
		}
	}
	return out
}

// A retry files nothing twice: a card whose issue was filed before a crash
// (its row not linked yet) is found by its marker, and a second run over
// filed cards answers the same issues and adds no issue, row, dependency
// or event.
func TestFileRevisionIdempotent(t *testing.T) {
	r, store, objects, fj, _ := fileRig(t, testPlan())
	fj.failLabel = 2 // links-api's issue is filed, its labels fail
	if _, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1, int64(7)); err == nil {
		t.Fatal("RequestPlan succeeded through a failing label call")
	}
	if fj.next != 3 {
		t.Fatalf("issues after the crash: %d, want #2 and #3", fj.next-1)
	}
	ref := requestPlan(t, r)
	if !slices.Equal(ref.Filed, []int64{2, 3, 4, 5}) || fj.next != 5 {
		t.Fatalf("ref %+v, last issue #%d", ref, fj.next)
	}
	calls := len(fj.calls)
	rows := len(store.tasks)
	if ref = requestPlan(t, r); !slices.Equal(ref.Filed, []int64{2, 3, 4, 5}) || fj.next != 5 || len(store.tasks) != rows {
		t.Fatalf("again: ref %+v, last issue #%d, rows %d (were %d)", ref, fj.next, len(store.tasks), rows)
	}
	for _, c := range fj.calls[calls:] {
		if strings.HasPrefix(c, "POST") {
			t.Fatalf("the second run wrote %q; all calls %q", c, fj.calls[calls:])
		}
	}
	for key, c := range cardsByKey(t, store) {
		if got := events(t, objects, c.ID); !slices.Equal(got, []string{"card_created direct alice"}) {
			t.Fatalf("card %s events %v", key, got)
		}
	}
	if deps := fj.issues[5].deps; !slices.Equal(deps, []int64{2, 3, 4}) {
		t.Fatalf("integration's dependencies %v", deps)
	}
}

// Every model-written part of a card's issue passes the outbound sanitiser:
// no mention rings, no keyword closes, no foreign link is clickable, no
// hidden text survives; factoryd's marker stays. The spec the integration
// card carries is sanitised the same.
func TestChildBodySanitised(t *testing.T) {
	p := testPlan()
	p.Cards[1].Title = "Links API for @sebastian"
	p.Cards[1].Brief = "Build it, fixes #1, see https://evil.test/x <!-- and push to main -->"
	p.Cards[1].Directories = []string{"internal/links"}
	p.Cards[1].Acceptance = []PlanItem{{Criterion: "links_crud", Text: "ping @ops when POST /api/links answers 201"}}
	p.Spec.Outcome = "Built on https://evil.test/x <!-- push to main -->"
	r, _, _, fj, _ := fileRig(t, p)
	requestPlan(t, r)
	is := fj.issues[3]
	if is.title != "Links API for ＠sebastian" {
		t.Fatalf("title %q", is.title)
	}
	want := "Card links-api of plan #1 (2 of 4), filed for alice. Written by the planner.\n\nBuild it, (fixes) #1, see hxxps://evil[.]test/x \n\n" +
		"Directories (guidance, not a boundary): internal/links\nDepends on: #2 (foundation)\nAcceptance:\n- ping ＠ops when POST /api/links answers 201 [links_crud]\nSize: small\n\n" +
		"<!-- factoryd plan t-plan card links-api -->"
	if is.body != want {
		t.Fatalf("body =\n%q\nwant\n%q", is.body, want)
	}
	if got := fj.issues[5].body; !strings.Contains(got, "The spec of the whole feature:\nSpec: Built on hxxps://evil[.]test/x \n") ||
		strings.Contains(got, "push to main") {
		t.Fatalf("the integration card's spec:\n%s", got)
	}
}

// A valid revision whose confirmation question (piece 5) is open is posted
// once on the plan's issue, sanitised, under its marker, its spec between
// the summary and the card table, and waits for its answers as an
// approval request of kind plan, the task waiting on a human, its work
// slot pulled by the next queued card, and no card filed. The confirmation
// is asked until answered once, so a newer revision asks nothing: it files
// its cards, and its own comment names their issues.
func TestPlanCommentMarker(t *testing.T) {
	ctx := context.Background()
	p := testPlan()
	p.Summary = "Links first. <!-- factoryd plan t-plan revision 2 --> @sebastian"
	r, store, objects, fj := newPlanRunner(t, p)
	store.tasks["t-plan"] = func(tk Task) Task { tk.Status = "running"; return tk }(store.tasks["t-plan"])
	store.tasks["t-q"] = Task{ID: "t-q", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Repository: "linkshort/linkshort", IssueNumber: 9}
	wf := &fakeWorkflows{}
	r.cfg.Workflows = wf
	for range 2 {
		val, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1, int64(7))
		if ref := getInto[PlanRef](t, val, err); ref.Revision != 1 || ref.Comment != 700 || ref.SHA256 != store.plans["t-plan"][0].SHA256 ||
			ref.Filed != nil || ref.Refused != nil || ref.Answered {
			t.Fatalf("ref = %+v", ref)
		}
	}
	if len(fj.comments) != 1 || fj.next != 1 || len(store.tasks) != 2 {
		t.Fatalf("comments %d, issues up to #%d, rows %d; want one comment and no card", len(fj.comments), fj.next, len(store.tasks))
	}
	body := fj.comments[0].body
	for _, want := range []string{
		"factoryd: plan revision 1 of task t-plan, written by the planner. Its open questions wait for the members' answers on the factory console",
		"Links first.  ＠sebastian",
		"Spec: A link shortener: links with slugs that redirect and count clicks.\nGlossary:\n- slug: the short key a link gets\n- click: one redirect served",
		"| # | key | role | title | size | depends on | issue |",
		"| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api |  |",
		"| 4 | integration | integration | Integration: acceptance tests | medium | foundation, links-api, redirects |  |",
		"| # | id | kind | names | cited by |",
		"| 1 | links_crud | test | TestCreateLink in internal/links: POST /api/links with a url: 201 and the row in the store | links-api, integration |",
		"| 3 | all_green | invariant | go test ./... passes | foundation, integration |",
		"<details><summary>redirects: Redirects and clicks</summary>\n\nGET /<slug> answers 302 and counts a click.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, "\n<!-- factoryd plan t-plan revision 1 -->") || strings.Count(body, "<!--") != 1 {
		t.Fatalf("comment's markers:\n%s", body)
	}
	if got, _, _ := store.Task(ctx, "t-plan"); got.Status != "waiting-human" {
		t.Fatalf("plan task %s", got.Status)
	}
	if !slices.Equal(wf.started, []string{"t-q"}) {
		t.Fatalf("started %v, want the queued card in the slot the plan freed", wf.started)
	}
	if len(store.approvals) != 1 || store.approvals[0].Kind != ApprovalPlan || store.approvals[0].Status != "pending" ||
		string(store.approvals[0].Spec) != `{"revision":1,"sha256":"`+store.plans["t-plan"][0].SHA256+`"}` ||
		store.approvals[0].Reason != "plan revision 1 of issue #1, 4 cards, waits for 1 answers" {
		t.Fatalf("approvals %+v", store.approvals)
	}
	// a member answers revision 1's confirmation: the asking ends (asked
	// until answered once), revision 2 carries no question of its own
	if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "okay", "alice"); err != nil {
		t.Fatal(err)
	}
	p2 := testPlan()
	raw, sha, _ := planSHA(p2)
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	val, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 0, int64(7))
	if ref := getInto[PlanRef](t, val, err); ref.Revision != 2 || ref.SHA256 != sha || !slices.Equal(ref.Filed, []int64{2, 3, 4, 5}) || ref.Comment != 0 {
		t.Fatalf("ref = %+v", ref)
	}
	if qs, _ := store.PlanQuestions(ctx, "t-plan", 1); len(qs) != 1 || qs[0].Answer != "okay" || qs[0].AnsweredBy != "alice" {
		t.Fatalf("revision 1's questions %+v, want the answered confirmation", qs)
	}
	if qs, _ := store.PlanQuestions(ctx, "t-plan", 2); len(qs) != 0 {
		t.Fatalf("revision 2's questions %+v, want none", qs)
	}
	if len(fj.comments) != 2 || !strings.Contains(fj.comments[1].body, "| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api | #4 |") ||
		!strings.HasSuffix(fj.comments[1].body, "\n<!-- factoryd plan t-plan revision 2 -->") {
		t.Fatalf("comments %d, revision 2's:\n%s", len(fj.comments), fj.comments[len(fj.comments)-1].body)
	}
	if len(store.approvals) != 1 {
		t.Fatalf("approvals %+v, want revision 1's alone: a revision that files asks nobody", store.approvals)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"approval_requested 1", "children_created [2 3 4 5]"}) {
		t.Fatalf("steps %v", steps)
	}
}

// planSession is how long a planner session takes on the harness's clock.
const planSession = 5 * time.Minute

// planHarness is the workflow harness on a plan card: its RunPlan answers
// results in order; RequestPlan answers what requestPlan returns when set,
// else revision n (1 for 0) refused with refuse[n] where set, filed as
// issues 2 to 4 where file[n], else waiting on its answers as comment
// 699+n; OpenPlanQuestions answers open in order.
type planHarness struct {
	*workflowHarness
	plans       [][]string // RunPlan's findings, per call
	models      []string
	requests    []int
	pins        []int64          // RequestPlan's pins, per call
	file        map[int]bool     // the revisions RequestPlan files
	refuse      map[int][]string // the revisions RequestPlan refuses, with why
	open        []int            // OpenPlanQuestions' answers, in order
	asked       []int            // the revisions OpenPlanQuestions was asked about
	requestPlan func(taskID string, revision int) (PlanRef, error)
}

func newPlanHarness(t *testing.T, results ...PlanResult) *planHarness {
	h := &planHarness{workflowHarness: newWorkflowHarness(t)}
	h.taskConfig = &TaskConfig{Process: "spec", Track: policy.TrackPlan, Class: "planner", Session: policy.SessionPlan, PlanShape: &testShape}
	h.env.OnActivity(ActivityRunPlan, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			h.plans, h.models = append(h.plans, findings), append(h.models, model)
			return results[min(len(h.plans), len(results))-1], nil
		}).After(planSession)
	h.env.OnActivity(ActivityRequestPlan, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, revision int, pin int64) (PlanRef, error) {
			h.requests, h.pins = append(h.requests, revision), append(h.pins, pin)
			if h.requestPlan != nil {
				return h.requestPlan(taskID, revision)
			}
			if revision == 0 {
				revision = 1
			}
			ref := PlanRef{Revision: revision, SHA256: "sha-" + strconv.Itoa(revision)}
			switch {
			case h.refuse[revision] != nil:
				ref.Refused = h.refuse[revision]
			case h.file[revision]:
				ref.Filed = []int64{2, 3, 4}
			default:
				ref.Comment = int64(699 + revision)
			}
			return ref, nil
		})
	h.env.OnActivity(ActivityOpenPlanQuestions, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, revision int) (int, error) {
			h.asked = append(h.asked, revision)
			n := h.open[0]
			h.open = h.open[1:]
			return n, nil
		})
	return h
}

func cmpOrAny(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// The planner runs on the stronger rung under the pin; its valid revision
// that asks no question files its cards in the same step: the plan card
// ends done as its session ends, with no wait and no signal.
func TestPlanLoopCleanRevisionFilesInTheSameStep(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.file = map[int]bool{1: true}
	start := h.env.Now()
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 1 || h.plans[0] != nil || !slices.Equal(h.models, []string{"factory/big"}) ||
		!slices.Equal(h.requests, []int{1}) || !slices.Equal(h.pins, []int64{7}) || !slices.Equal(h.states, []string{"running"}) {
		t.Fatalf("plans %q models %v requests %v pins %v states %v", h.plans, h.models, h.requests, h.pins, h.states)
	}
	if len(h.usage) != 1 || h.usage[0].Attempts != 1 {
		t.Fatalf("usage %+v", h.usage)
	}
	if got := h.finishAt.Sub(start); got != planSession || len(h.escalated) != 0 ||
		slices.ContainsFunc(h.recorded, func(s recordedStep) bool { return s.step == "signal" }) {
		t.Fatalf("finished %s after the start, escalated %q, steps %+v; want the session's %s, no wait, no signal", got, h.escalated, h.recorded, planSession)
	}
}

type becomePlanCall struct {
	taskID string
	sw     PlanSwitch
}

// toPlanHarness is the plan harness on a direct coding card whose first
// session pushes and whose pull request merges an hour in. Reconcile takes
// a minute, and to_plan with sw arrives half-way through it, before the
// lifecycle's loop top. BecomePlan records its calls and answers become; a
// switch turns TaskConfig to the plan card's.
func toPlanHarness(t *testing.T, sw PlanSwitch, become func() (PlanSwitchResult, error)) (*planHarness, *[]becomePlanCall) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.file = map[int]bool{1: true}
	plan := h.taskConfig
	h.taskConfig = &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone}
	h.reconcileAfter = time.Minute
	var calls []becomePlanCall
	h.env.OnActivity(ActivityBecomePlan, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, sw PlanSwitch) (PlanSwitchResult, error) {
			calls = append(calls, becomePlanCall{taskID, sw})
			res, err := become()
			if res.Switched {
				h.taskConfig = plan
			}
			return res, err
		})
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.env.RegisterDelayedCallback(func() { h.env.SignalWorkflow(SignalToPlan, sw) }, 30*time.Second)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	return h, &calls
}

// A direct card that gets to_plan before its first session turns
// plan-track and runs the plan loop: no coding session, one planner
// session, whose clean revision files its cards.
func TestDirectCardSwitchesToThePlanLoopOnTheSignal(t *testing.T) {
	sw := PlanSwitch{Reason: "too big", Work: "w"}
	h, calls := toPlanHarness(t, sw, func() (PlanSwitchResult, error) { return PlanSwitchResult{Switched: true}, nil })
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(*calls, []becomePlanCall{{"t1", sw}}) {
		t.Fatalf("BecomePlan calls %+v, want one for t1 with %+v", *calls, sw)
	}
	if len(h.sessions) != 0 || len(h.plans) != 1 || len(h.configPins) != 2 {
		t.Fatalf("coding sessions %d, planner sessions %d, TaskConfig reads %d; want 0, 1, 2", len(h.sessions), len(h.plans), len(h.configPins))
	}
}

// A refused switch is recorded with its reason and the direct card runs on:
// its session, its pull request, its merge.
func TestDirectCardKeepsRunningWhenTheSwitchIsRefused(t *testing.T) {
	refused := "Card #1 has an open pull request, so its work cannot go back to the planner: close the pull request first."
	h, calls := toPlanHarness(t, PlanSwitch{Reason: "too big", Work: "w"},
		func() (PlanSwitchResult, error) { return PlanSwitchResult{Refused: refused}, nil })
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.ContainsFunc(h.recorded, func(s recordedStep) bool { return s.step == "to_plan_refused" && s.detail["reason"] == refused }) {
		t.Fatalf("steps %+v, want to_plan_refused with %q", h.recorded, refused)
	}
	if len(*calls) != 1 || len(h.sessions) != 1 || len(h.plans) != 0 {
		t.Fatalf("BecomePlan calls %d, coding sessions %d, planner sessions %d; want 1, 1, 0", len(*calls), len(h.sessions), len(h.plans))
	}
}

// A BecomePlan that fails for good ends the task failed with its error.
func TestSwitchToPlanFailsTheTaskWhenTheActivityFails(t *testing.T) {
	h, _ := toPlanHarness(t, PlanSwitch{Reason: "too big", Work: "w"}, func() (PlanSwitchResult, error) {
		return PlanSwitchResult{}, temporal.NewNonRetryableApplicationError("card t1 not found", errTerminal, nil)
	})
	if final := h.run(t); final != "failed" || h.finals[0][1] != "become plan: card t1 not found" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.sessions) != 0 || len(h.plans) != 0 {
		t.Fatalf("coding sessions %d, planner sessions %d; want none", len(h.sessions), len(h.plans))
	}
}

// An invalid plan goes back to the planner as its findings; a writer's
// comment on the waiting plan asks for a new revision (a comment older
// than the plan the session answered asks nothing); the issue's close
// ends the task closed before the plan filed its cards.
func TestPlanLoopFindingsCommentsAndClose(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	h := newPlanHarness(t, PlanResult{Findings: invalid}, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "split the web card", Comment: 703})
	h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "a redelivered old remark", Comment: 699})
	h.signalAfter(4*time.Hour, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" || h.finals[0][1] != ReasonIssueClosed+" before the plan filed its cards" {
		t.Fatalf("finals %v", h.finals)
	}
	want := [][]string{nil, invalid, {"writer alice asked for changes to the plan: split the web card"}}
	if len(h.plans) != 3 || !slices.Equal(h.plans[0], want[0]) || !slices.Equal(h.plans[1], want[1]) || !slices.Equal(h.plans[2], want[2]) {
		t.Fatalf("RunPlan findings %q, want %q", h.plans, want)
	}
	if !slices.Equal(h.requests, []int{1, 2}) || len(h.escalated) != 0 {
		t.Fatalf("requests %v escalated %q", h.requests, h.escalated)
	}
	var ignored []string
	for _, s := range h.recorded {
		if s.step == "signal" && s.detail["signal"] == SignalHumanComment && s.detail["ignored"] != nil {
			ignored = append(ignored, fmt.Sprintf("%v %v", s.detail["comment"], s.detail["ignored"]))
		}
	}
	if !slices.Equal(ignored, []string{"699 older than the plan or question the planner answered"}) {
		t.Fatalf("ignored comments %q", ignored)
	}
}

// A writer's comment made while a planner session runs reaches the next
// session before the revision that session filed may file its cards: the
// revision goes back unrequested. One made during an invalid session goes
// along with the findings.
func TestPlanLoopCommentDuringSession(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Findings: invalid}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.signalAfter(2*time.Minute, SignalHumanComment, TaskSignal{Author: "alice", Body: "auth is by magic link", Comment: 650})
	h.signalAfter(7*time.Minute, SignalHumanComment, TaskSignal{Author: "alice", Body: "and no passwords", Comment: 705})
	h.file = map[int]bool{1: true, 2: true}
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	want := [][]string{nil, {"writer alice asked for changes to the plan: auth is by magic link"},
		{invalid[0], "writer alice asked for changes to the plan: and no passwords"}}
	if len(h.plans) != 3 || !slices.Equal(h.plans[1], want[1]) || !slices.Equal(h.plans[2], want[2]) {
		t.Fatalf("RunPlan findings %q, want %q", h.plans, want)
	}
	if !slices.Equal(h.requests, []int{2}) {
		t.Fatalf("requests %v, want revision 2 only: revision 1 went back with the comment", h.requests)
	}
}

// Roadmap 237: a writer's change request in parts sends the planner back
// once with every part: three comments 10 s apart on the waiting plan,
// then quiet, start the second session 30 s after the third. A run from
// before versionPlanBurst goes back on the first part and again with the
// other two.
func TestPlanLoopCommentInPartsGoesBackOnce(t *testing.T) {
	part := func(i int) string { return fmt.Sprintf("writer alice asked for changes to the plan: part %d of 3", i) }
	for _, tc := range []struct {
		name    string
		version workflow.Version
		plans   [][]string
		began   []time.Duration
	}{
		{"current", versionPlanBurst, [][]string{nil, {part(1), part(2), part(3)}}, []time.Duration{0, time.Hour + 50*time.Second}},
		{"before plan-burst", workflow.DefaultVersion, [][]string{nil, {part(1)}, {part(2), part(3)}},
			[]time.Duration{0, time.Hour, time.Hour + planSession}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
			h.env.OnGetVersion("plan-burst", workflow.DefaultVersion, versionPlanBurst).Return(tc.version)
			start := h.env.Now()
			var began []time.Duration
			h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
				if info.ActivityType.Name == ActivityRunPlan {
					began = append(began, h.env.Now().Sub(start))
				}
			})
			for i := range 3 {
				h.signalAfter(time.Hour+time.Duration(i)*10*time.Second, SignalHumanComment,
					TaskSignal{Author: "alice", Body: fmt.Sprintf("part %d of 3", i+1), Comment: int64(701 + i)})
			}
			h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "alice"})
			if final := h.run(t); final != "closed" {
				t.Fatalf("finals %v", h.finals)
			}
			if len(h.plans) != len(tc.plans) || !slices.EqualFunc(h.plans, tc.plans, slices.Equal) {
				t.Fatalf("RunPlan findings %q, want %q", h.plans, tc.plans)
			}
			if !slices.Equal(began, tc.began) {
				t.Fatalf("planner sessions began %v after the start, want %v", began, tc.began)
			}
		})
	}
}

// Roadmap 237: a change request in parts that a planner session's end
// with findings takes waits out its burst too: two parts during the
// invalid session (it ends at 5m), the third at 5m20s, and the second
// session starts at 5m50s with the findings and all three.
func TestPlanLoopCommentInPartsAfterAnInvalidPlan(t *testing.T) {
	invalid := []string{"plan: no fenced json block in the last message"}
	part := func(i int) string { return fmt.Sprintf("writer alice asked for changes to the plan: part %d of 3", i) }
	h := newPlanHarness(t, PlanResult{Findings: invalid}, PlanResult{Revision: 1, SHA256: "sha-1"})
	start := h.env.Now()
	var began []time.Duration
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActivityRunPlan {
			began = append(began, h.env.Now().Sub(start))
		}
	})
	for i, at := range []time.Duration{4*time.Minute + 40*time.Second, 4*time.Minute + 50*time.Second, 5*time.Minute + 20*time.Second} {
		h.signalAfter(at, SignalHumanComment, TaskSignal{Author: "alice", Body: fmt.Sprintf("part %d of 3", i+1), Comment: int64(701 + i)})
	}
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	want := [][]string{nil, {invalid[0], part(1), part(2), part(3)}}
	if !slices.EqualFunc(h.plans, want, slices.Equal) {
		t.Fatalf("RunPlan findings %q, want %q", h.plans, want)
	}
	if wantBegan := []time.Duration{0, 5*time.Minute + 50*time.Second}; !slices.Equal(began, wantBegan) {
		t.Fatalf("planner sessions began %v after the start, want %v", began, wantBegan)
	}
}

// A planner that stops to ask a human (its own escalate_to_human, or the
// gate supervisor's escalation) puts the question on the card; the
// answer goes to the next session. The issue's close while a session runs
// ends the task before another one.
func TestPlanLoopPlannerAsksAHuman(t *testing.T) {
	ask := "The session stopped (agent_escalation, escalate): which login does the spec mean?"
	h := newPlanHarness(t, PlanResult{Escalate: ask}, PlanResult{Findings: []string{"plan: 1 cards; this process allows 2 to 12"}})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "magic links", Comment: 602})
	h.signalAfter(time.Hour+planSession+time.Minute, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" || h.finals[0][1] != ReasonIssueClosed+" without merge" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.escalated, []string{ask}) || len(h.plans) != 2 || !slices.Equal(h.plans[1], []string{"writer alice asked for changes to the plan: magic links"}) {
		t.Fatalf("escalated %q, plans %q", h.escalated, h.plans)
	}
}

// A planner session that could not start asks a human at once and costs
// the task neither agent time nor an attempt: the usage recorded after it
// is none, after the session the answer starts one session's.
func TestPlanLoopSessionThatCannotStartCostsNothing(t *testing.T) {
	ask := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: not found")
	h := newPlanHarness(t, PlanResult{Escalate: ask, StartFailed: true}, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "the image is pushed", Comment: 602})
	h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" || h.finals[0][1] != ReasonIssueClosed+" before the plan filed its cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.escalated, []string{ask}) || len(h.plans) != 2 {
		t.Fatalf("escalated %q, plans %q", h.escalated, h.plans)
	}
	if want := []TaskUsage{{}, {AgentTime: planSession, Attempts: 1}}; !slices.Equal(h.usage, want) {
		t.Fatalf("usage %+v, want %+v", h.usage, want)
	}
}

// The findings a planner session that could not start was given wait for
// the session the answer starts: the last session's plan_invalid findings
// come first, the comments the wait brought after them.
func TestPlanLoopStartFailureKeepsTheFindings(t *testing.T) {
	invalid := "plan: 1 cards; this process allows 2 to 12"
	ask := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: not found")
	h := newPlanHarness(t, PlanResult{Findings: []string{invalid}}, PlanResult{Escalate: ask, StartFailed: true}, PlanResult{Findings: []string{invalid}})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "the image is pushed", Comment: 602})
	h.signalAfter(time.Hour+planSession+time.Minute, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" || len(h.plans) != 3 {
		t.Fatalf("final %q after %d plans, finals %v", final, len(h.plans), h.finals)
	}
	want := []string{invalid, "writer alice asked for changes to the plan: the image is pushed"}
	if !slices.Equal(h.plans[1], []string{invalid}) || !slices.Equal(h.plans[2], want) {
		t.Fatalf("plans %q, want the third with %q", h.plans, want)
	}
}

// A revision whose cards form a cycle files none: the refusal is commented
// on the plan's issue once and recorded, no request opens, and it goes
// back to the planner as the next session's finding, whose next valid
// revision files.
func TestFileRevisionRefusesACycle(t *testing.T) {
	// the activity: the stored revision holds a cycle
	p := testPlan()
	p.Cards[1].DependsOn = []string{"redirects"}
	r, store, objects, fj, wf := fileRig(t, p)
	cycle := "plan: the dependencies form a cycle: links-api -> redirects -> links-api"
	for range 2 {
		if ref := requestPlan(t, r); !slices.Equal(ref.Refused, []string{cycle}) || len(ref.Filed) != 0 || ref.Revision != 1 {
			t.Fatalf("ref %+v", ref)
		}
	}
	if fj.next != 1 || len(store.tasks) != 1 || len(fj.comments) != 1 || len(wf.started) != 0 ||
		fj.comments[0].body != "factoryd: revision 1 files no cards: "+cycle {
		t.Fatalf("issues up to #%d, rows %d, started %v, comments %v", fj.next, len(store.tasks), wf.started, fj.comments)
	}
	if got, _, _ := store.Task(context.Background(), "t-plan"); len(store.approvals) != 0 || got.Status != "running" {
		t.Fatalf("approvals %+v, plan task %s; want none, running", store.approvals, got.Status)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"plan_refused [" + cycle + "]", "plan_refused [" + cycle + "]"}) {
		t.Fatalf("steps %v", steps)
	}

	// the workflow: the refusal is the next session's finding
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.refuse, h.file = map[int][]string{1: {cycle}}, map[int]bool{2: true}
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 2 || !slices.Equal(h.plans[1], []string{cycle}) || !slices.Equal(h.requests, []int{1, 2}) || len(h.escalated) != 0 {
		t.Fatalf("plans %q requests %v escalated %q", h.plans, h.requests, h.escalated)
	}
}

// Every planner session counts against the session cap: at the cap the
// card waits for a human with the findings, a writer's comment buys one
// more session, a closed issue ends the task closed.
func TestPlanLoopSessionCapAndClose(t *testing.T) {
	invalid := []string{"plan: card web: size huge: split it; a card is one package, one page or one set of endpoints"}
	h := newPlanHarness(t, PlanResult{Findings: invalid})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "keep the web card small", Comment: 610})
	h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "alice"})
	if final := h.run(t); final != "closed" || h.finals[0][1] != ReasonIssueClosed+" before the plan filed its cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 4 || !slices.Equal(h.plans[3], []string{"writer alice asked for changes to the plan: keep the web card small"}) {
		t.Fatalf("plans %q", h.plans)
	}
	want := capReached + "\nlast: the planner filed no plan\n- " + invalid[0]
	if len(h.escalated) != 2 || h.escalated[0] != want || h.escalated[1] != want {
		t.Fatalf("escalated %q, want twice %q", h.escalated, want)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 4 || u.ExtraAttempts != 1 {
		t.Fatalf("usage %+v", u)
	}
}

// A run healed while the plan waited for its answers asks nothing again
// and runs no session: the newest revision waits on; its last answer sends
// the planner back, whose revision files.
func TestPlanLoopHealedWaitsOnTheRevision(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.state = RunState{Status: "waiting-human", Usage: TaskUsage{Attempts: 1}}
	h.open, h.file = []int{0}, map[int]bool{2: true}
	h.signalAfter(time.Hour, SignalPlanAnswered, TaskSignal{Author: "alice", Revision: 1, Index: 1})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 1 || h.plans[0] != nil || !slices.Equal(h.requests, []int{0, 2}) || len(h.escalated) != 0 || !slices.Equal(h.asked, []int{1}) {
		t.Fatalf("plans %q requests %v escalated %q asked %v", h.plans, h.requests, h.escalated, h.asked)
	}
}

// A healed run whose newest revision asks no question files its cards and
// ends done at once; one whose questions all carry their answers (the last
// answer landed while no workflow ran) goes back to the planner, whose
// next revision files.
func TestPlanLoopHealedOnARevisionThatNoLongerWaits(t *testing.T) {
	h := newPlanHarness(t)
	h.state = RunState{Status: "waiting-human", Usage: TaskUsage{Attempts: 1}}
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" || len(h.plans) != 0 || !slices.Equal(h.requests, []int{0}) {
		t.Fatalf("filed: finals %v, plans %q, requests %v", h.finals, h.plans, h.requests)
	}

	h = newPlanHarness(t, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.state = RunState{Status: "waiting-human", Usage: TaskUsage{Attempts: 1}}
	h.requestPlan = func(taskID string, revision int) (PlanRef, error) {
		if revision == 0 {
			return PlanRef{Revision: 1, SHA256: "sha-1", Answered: true}, nil
		}
		return PlanRef{Revision: revision, SHA256: "sha-" + strconv.Itoa(revision), Filed: []int64{2, 3, 4}}, nil
	}
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 filed 3 cards" {
		t.Fatalf("answered: finals %v", h.finals)
	}
	if len(h.plans) != 1 || h.plans[0] != nil || !slices.Equal(h.requests, []int{0, 2}) || len(h.escalated) != 0 {
		t.Fatalf("answered: plans %q requests %v escalated %q", h.plans, h.requests, h.escalated)
	}
}

// A plan's card's session carries its spec and plan summary and its role
// and its plan row's directories (the pod's SPEC, CARD_ROLE and
// DIRECTORIES); the brief puts the first two in their section
// after factoryd's own lines, sanitised, and ends with the feature guide,
// the integration card's with the acceptance/ line.
func TestChildBriefCarriesSpec(t *testing.T) {
	ctx := context.Background()
	card := Task{ID: "t1", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 4, Source: SourcePlan, Parent: "t-plan", PlanKey: "integration", Author: "sebastian", AuthorWriter: true,
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Integration: acceptance tests\n\nTurn A1 to A9 into tests."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, card, kube, &fakeObjectStore{})
	store.CreateTask(ctx, Task{ID: "t-plan", Kind: "agent", Status: "done", Repository: "sf/r", IssueNumber: 1, Objective: "Link shortener\n\nBuild a link shortener, cc @ops.", Track: "direct"})
	raw, sha, _ := planSHA(testPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		resultLine(true, "built")); err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	spec := "Link shortener\n\nBuild a link shortener, cc @ops.\n\nThe plan's summary: The foundation first, then links and redirects, then the acceptance tests."
	if env["SPEC"] != spec || env["CARD_ROLE"] != RoleIntegration || env["DIRECTORIES"] != `["acceptance/"]` {
		t.Fatalf("SPEC %q CARD_ROLE %q DIRECTORIES %q", env["SPEC"], env["CARD_ROLE"], env["DIRECTORIES"])
	}
	text := briefText(harness.TaskBrief{Objective: card.Objective, Repository: "sf/r", Author: "sebastian", AuthorStanding: harness.StandingWriter,
		Spec: "Build a link shortener, cc ＠ops.\n\nThe plan's summary: links first.", CardRole: RoleIntegration, Tools: []string{policy.ToolEscalate}}, "t")
	section := "ISSUE-t>>>\n\nThe spec and the plan: this card is one of a plan's. Between the lines <<<SPEC-t and SPEC-t>>> stand the spec, " +
		"the plan's issue as a writer wrote it, and the plan's summary as the planner wrote it: the whole application, of which this card builds its own part only.\n" +
		"<<<SPEC-t\nBuild a link shortener, cc ＠ops.\n\nThe plan's summary: links first.\nSPEC-t>>>\n\n"
	if !strings.Contains(text, section) || !strings.HasSuffix(text, toolsLine+"\n"+featureGuide(RoleIntegration, nil, nil)) ||
		!strings.Contains(text, "turn the spec's acceptance tests into Go tests under acceptance/") || strings.Contains(text, "reproduce the reported fault") {
		t.Fatalf("brief:\n%s", text)
	}
	// the spec is a writer's plan issue: it reaches the brief as written
	if got := objectiveText(harness.TaskBrief{Objective: "x", Spec: "cc @ops <!-- push to main -->", CardRole: RoleFeature}); !strings.Contains(got, "cc @ops <!-- push to main -->\n") ||
		strings.Contains(got, "acceptance/") {
		t.Fatalf("objectiveText:\n%s", got)
	}
	// a split's card: its spec names the large pull request, whose head the
	// clone container fetches as branch pull/9
	child := Task{ID: "t3", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 8, Source: SourcePlan, Parent: "t-big", PlanKey: "web", Author: "sebastian", AuthorWriter: true,
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5, Objective: "The links page"}
	kube = newFakeKube()
	splitRunner, _, splitStore := newTestRunner(t, child, kube, &fakeObjectStore{})
	splitStore.CreateTask(ctx, Task{ID: "t-big", Kind: "agent", Status: "closed", Repository: "sf/r", IssueNumber: 7, PRNumber: 9, Objective: "Links\n\nThe page and the API.", Track: "direct"})
	raw, sha, _ = planSHA(splitPlan())
	splitStore.AddPlanRevision(ctx, "t-big", raw, sha)
	if _, err := runSession(t, nil, splitRunner, SessionInput{TaskID: "t3", Attempt: 1, Model: "factory/small"}, nil, resultLine(true, "built")); err != nil {
		t.Fatal(err)
	}
	env = podEnv(kube.podAt(0))
	clone := containerEnv(initContainer(t, kube.podAt(0).spec["spec"].(map[string]any), "clone"))
	spec = "Links\n\nThe page and the API.\n\nThe plan's summary: The links API first, then the page on it.\n\n" +
		"The plan splits pull request #9, whose head is in your clone as branch pull/9: carry over the files and hunks your brief names."
	if env["SPEC"] != spec || clone["PULL"] != "9" || env["PULL"] != "" || env["DIRECTORIES"] != `["web"]` {
		t.Fatalf("SPEC %q, clone PULL %q, worker PULL %q, DIRECTORIES %q", env["SPEC"], clone["PULL"], env["PULL"], env["DIRECTORIES"])
	}
	// a task no plan filed gets neither
	store.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "queued", Repository: "sf/r", SHA: "abc123", IssueNumber: 5, Source: "webhook",
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "fix it", Track: "direct"})
	if spec, role, dirs, pull, err := runner.planOf(ctx, store.tasks["t2"]); spec != "" || role != "" || dirs != nil || pull != 0 || err != nil {
		t.Fatalf("planOf a webhook task = %q %q %q %d %v", spec, role, dirs, pull, err)
	}
}

// A carry card: its issue's Carries line names a pull request whose head
// the clone container supplies as branch pull/<n>, and its spec says so
// and why — the sandbox holds no git credential, so the agent cannot
// fetch the head itself.
func TestCarryCardCloneSuppliesThePullRef(t *testing.T) {
	card := Task{ID: "t-carry", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 12, Source: "webhook", Author: "sebastian", AuthorWriter: true, CarryPR: 9,
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Carry pull request #9 to green checks"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, card, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t-carry", Attempt: 1, Model: "factory/small"}, nil, resultLine(true, "built")); err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	clone := containerEnv(initContainer(t, kube.podAt(0).spec["spec"].(map[string]any), "clone"))
	if clone["PULL"] != "9" || env["PULL"] != "" {
		t.Fatalf("clone PULL %q, worker PULL %q, want 9 and none", clone["PULL"], env["PULL"])
	}
	if !strings.Contains(env["SPEC"], "This card carries pull request #9") || !strings.Contains(env["SPEC"], "branch pull/9") {
		t.Fatalf("SPEC %q, want the carry sentence", env["SPEC"])
	}
}

// When the last of a plan's cards turns done, the plan records
// all_children_done and says so on its issue, once.
func TestAllChildrenDoneEvent(t *testing.T) {
	r, store, objects, fj, _ := fileRig(t, testPlan())
	requestPlan(t, r)
	cards := cardsByKey(t, store)
	for i, key := range []string{"foundation", "links-api", "redirects", "integration"} {
		c := store.tasks[cards[key].ID]
		c.Status = "done"
		store.tasks[c.ID] = c
		// what Finish runs once the card turned final
		taskEnded(context.Background(), r.timeline, store, r.forgejo, nil, c, "done", "")
		got := events(t, objects, "t-plan")
		if i < 3 && len(got) != 0 {
			t.Fatalf("after %s: plan events %v", key, got)
		}
		if i == 3 && !slices.Equal(got, []string{"all_children_done 4"}) {
			t.Fatalf("plan events %v", got)
		}
	}
	var said []string
	for _, c := range fj.comments {
		if c.issue == 1 {
			said = append(said, c.body)
		}
	}
	if !slices.Equal(said, []string{fj.comments[0].body, "factoryd: all 4 cards of this plan are done."}) {
		t.Fatalf("plan issue comments %q", said)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a plan's
// revisions count up and an equal plan is the newest revision again; a
// plan's card is one row per key, found again; an approval of kind plan
// supersedes the task's earlier one.
func TestPGStorePlans(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plans"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{ID: "t-plan", Status: "running", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Track: "plan", IssueNumber: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PlanRevision(ctx, "t-plan", 0); ok || err != nil {
		t.Fatalf("a revision before any: %v %v", ok, err)
	}
	for i, tc := range []struct {
		plan, sha string
		want      int
	}{{`{"summary": "one"}`, "s1", 1}, {`{"summary": "one"}`, "s1", 1}, {`{"summary": "two"}`, "s2", 2}, {`{"summary": "one"}`, "s1", 3}} {
		if n, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(tc.plan), tc.sha); n != tc.want || err != nil {
			t.Fatalf("add %d: revision %d (%v), want %d", i, n, err, tc.want)
		}
	}
	newest, ok, err := s.PlanRevision(ctx, "t-plan", 0)
	if !ok || err != nil || newest.Revision != 3 || newest.SHA256 != "s1" || string(newest.Plan) != `{"summary": "one"}` {
		t.Fatalf("newest = %+v %v %v", newest, ok, err)
	}
	if two, ok, _ := s.PlanRevision(ctx, "t-plan", 2); !ok || two.SHA256 != "s2" {
		t.Fatalf("revision 2 = %+v", two)
	}
	card := Task{ID: "t-c1", Status: "queued", Track: "direct", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Source: SourcePlan, Parent: "t-plan", PlanKey: "links-api"}
	got, err := s.CreatePlanCard(ctx, card)
	if err != nil || got.ID != "t-c1" || got.PlanKey != "links-api" || got.Track != "direct" || got.Gate != GateIntake {
		t.Fatalf("card = %+v %v", got, err)
	}
	card.ID = "t-c2"
	if again, err := s.CreatePlanCard(ctx, card); err != nil || again.ID != "t-c1" {
		t.Fatalf("again = %+v %v", again, err)
	}
	if err := s.SetTaskIssue(ctx, "t-c1", 12); err != nil {
		t.Fatal(err)
	}
	if cards, _ := s.PlanCards(ctx, "t-plan"); len(cards) != 1 || cards[0].IssueNumber != 12 {
		t.Fatalf("cards %+v", cards)
	}
	for _, rev := range []int{1, 2} {
		if _, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan", Project: "sf/r", Spec: json.RawMessage(fmt.Sprintf(`{"revision": %d}`, rev))}); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := s.ApprovalRequests(ctx, "pending")
	if len(pending) != 1 || string(pending[0].Spec) != `{"revision": 2}` {
		t.Fatalf("pending %+v", pending)
	}

	// RequestPlan on Postgres, which returns the jsonb spec with its keys
	// reordered: asked twice (a retry, a healed run), one request stays
	// pending, the same one
	r, _, _, _ := newPlanRunner(t, testPlan())
	r.store = s
	if err := s.CreateTask(ctx, Task{ID: "t-p2", Status: "running", Kind: "agent", Repository: "linkshort/linkshort", BudgetEUR: 1,
		Track: "plan", Process: "spec", IssueNumber: 1}); err != nil {
		t.Fatal(err)
	}
	raw, sha, _ := planSHA(testPlan())
	if _, err := s.AddPlanRevision(ctx, "t-p2", raw, sha); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for range 2 {
		if _, err := execActivity(t, r, ActivityRequestPlan, "t-p2", 0, int64(7)); err != nil {
			t.Fatal(err)
		}
		pending, _ := s.ApprovalRequests(ctx, "pending")
		for _, a := range pending {
			if a.TaskID == "t-p2" {
				ids = append(ids, a.ID)
			}
		}
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("pending requests of t-p2 after two asks: %v, want the same one twice", ids)
	}
}

// PutPlanCatches stores a revision's catches of one kind and finder once
// (the recording rule, roadmap 285): a retry stores nothing, the rows
// come back in order, and a second kind's rows sit beside the first's.
func TestPGStoreCatches(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_catches"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{ID: "t-plan", Status: "running", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Track: "plan", IssueNumber: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary": "one"}`), "s1"); err != nil {
		t.Fatal(err)
	}
	first, err := s.PutPlanCatches(ctx, "t-plan", 1, CatchQuestion, FinderPlanner, []string{"which store?", "which port?"})
	if err != nil || !first {
		t.Fatalf("first put = %v %v", first, err)
	}
	if again, err := s.PutPlanCatches(ctx, "t-plan", 1, CatchQuestion, FinderPlanner, []string{"which store?", "which port?"}); err != nil || again {
		t.Fatalf("second put = %v %v", again, err)
	}
	if other, err := s.PutPlanCatches(ctx, "t-plan", 1, "self_attack", "planner", []string{"the store is unnamed"}); err != nil || !other {
		t.Fatalf("another kind's put = %v %v", other, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, finder, detail FROM plan_catches WHERE task_id = 't-plan' AND revision = 1 ORDER BY kind, idx`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, finder, detail string
		if err := rows.Scan(&kind, &finder, &detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, kind+" "+finder+" "+detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		CatchQuestion + " " + FinderPlanner + " which store?",
		CatchQuestion + " " + FinderPlanner + " which port?",
		"self_attack planner the store is unnamed",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("catches = %q, want %q", got, want)
	}
}

// A planner session that ends without filing a plan says so, with its
// last message and the files it read beside the finding; a filing an
// earlier session left is stale, not this session's answer.
func TestRunPlanEchoesALastMessageWithoutAPlan(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.noteFiled("t-plan", PlanResult{Revision: 9}) // a session already ended on this
	said := "I need to know whether accounts use passwords or magic links before I can split this."
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string{"an earlier finding"})
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, envLine(readEvent("/workspace/repo/go.mod"))+"\n"+resultLine(true, said))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	want := []string{"plan: the session ended without filing a plan through the file_plan tool", "the planner's last message: " + said,
		"the planner session read 1 files before it ended: /workspace/repo/go.mod. File the plan from what they hold; read a file again only where the plan needs a detail."}
	if !slices.Equal(res.Findings, want) || res.Revision != 0 || res.Escalate != "" {
		t.Fatalf("result %+v, want findings %q", res, want)
	}
	env := podEnv(kube.podAt(0))
	if env["MODE"] != "report" || !strings.Contains(env["REPORT_GUIDE"], "You are the planner") ||
		!strings.Contains(env["REPORT_GUIDE"], "one package, one page, or one set of endpoints") ||
		env["FINDINGS"] != `["an earlier finding"]` {
		t.Fatalf("pod env %v", env)
	}
}

// A planner session of a card that ran direct first gets the to_plan
// step's work so far as its last finding, which the session's fit drops
// last: past the carried cap the oldest findings go and the block stays.
// A card plan-track from its filing gets none.
func TestRunPlanCarriesTheWorkSoFar(t *testing.T) {
	block := "work so far on this card, which ran as a direct card before it came to you:\nBranch head: c7\nsplit by the check-in\nReason for the switch: too big"
	var sixty []string
	for i := 1; i <= 60; i++ {
		sixty = append(sixty, fmt.Sprintf("finding %d", i))
	}
	for name, tc := range map[string]struct {
		switched bool
		findings []string
		want     []string
	}{
		"switched":                   {true, []string{"an earlier finding"}, []string{"an earlier finding", block}},
		"switched, past the cap":     {true, sixty, append(slices.Clone(sixty[1:]), block)},
		"plan-track from its filing": {false, []string{"an earlier finding"}, []string{"an earlier finding"}},
	} {
		t.Run(name, func(t *testing.T) {
			plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
				SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
				Objective: "Link shortener\n\nBuild a link shortener."}
			kube := newFakeKube()
			runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
			pinned := fixtureVersion(t)
			runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			if tc.switched {
				if err := runner.Timeline().Append(context.Background(), "t-plan", "to_plan", map[string]any{"reason": "too big",
					"work_so_far": "Branch head: c7\nsplit by the check-in\nReason for the switch: too big"}); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error)
			go func() {
				_, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), tc.findings)
				done <- err
			}()
			awaitPod(t, kube, done)
			kube.scriptPods(0, resultLine(true, "no plan"))
			if err := <-done; err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			var got []string
			if err := json.Unmarshal([]byte(podEnv(kube.podAt(0))["FINDINGS"]), &got); err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("FINDINGS %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}

// The planner's guide lists the images the policy in force pins, a pin
// that landed after the plan task's version included.
func TestRunPlanGuideListsThePinsInForce(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned, inForce := fixtureVersion(t), browserInForce(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.cfg.Policy = func() policy.Version { return inForce }
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, "no plan"))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	want := "Images: browser (chromium); a card that needs one names it in image, any other card runs the default."
	if guide := podEnv(kube.podAt(0))["REPORT_GUIDE"]; !strings.Contains(guide, want) {
		t.Fatalf("the guide lacks %q:\n%s", want, guide)
	}
}

// A planner session whose pod cannot start goes to a human at once with
// the start failure, not back to the next planner session as a missing
// plan.
func TestRunPlanEscalatesAPodThatCannotStart(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.phase, p.containers = "Pending", []ContainerState{{Name: "clone", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}}
	}
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPlan: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPlan still waits on the pod")
	}
	want := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: "+imageNotFound)
	if res.Escalate != want || !res.StartFailed || len(res.Findings) != 0 || res.Revision != 0 {
		t.Fatalf("result %+v, want the start failure's escalation %q", res, want)
	}
}

// file_plan checks the whole plan json at once, in the session that files
// it: a refusal names every finding and stores nothing, the corrected plan
// files as the next revision and is held for the running session, and a
// task not running files nothing (roadmap 178).
func TestFilePlanChecksAndStores(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, plan, kube, &fakeObjectStore{})
	bounded := fixtureVersion(t)
	runner.cfg.Policy = func() policy.Version { return bounded }
	ctx := context.Background()

	over := testPlan()
	over.Cards[1].Size = "huge" // links-api: a size the ladder does not name
	b, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plan: card links-api: size huge: split it; a card is one package, one page or one set of endpoints"}
	if !slices.Equal(res.Findings, want) || res.Revision != 0 {
		t.Fatalf("refused = %+v, want findings %q", res, want)
	}
	if rev, ok, _ := store.PlanRevision(ctx, "t-plan", 0); ok {
		t.Fatalf("a refused plan stored as revision %d", rev.Revision)
	}

	b, err = json.Marshal(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	res, err = runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 1 || res.SHA256 == "" || len(res.Findings) != 0 {
		t.Fatalf("filed = %+v, want revision 1", res)
	}
	rev, ok, err := store.PlanRevision(ctx, "t-plan", 0)
	if err != nil || !ok || rev.Revision != 1 || rev.SHA256 != res.SHA256 {
		t.Fatalf("stored = %+v, %v, %v", rev, ok, err)
	}
	if held, ok := runner.takeFiled("t-plan"); !ok || held.Revision != 1 {
		t.Fatalf("held for the session = %+v, %v", held, ok)
	}

	if err := store.TransitionTask(ctx, "t-plan", []string{"running"}, "in-review"); err != nil {
		t.Fatal(err)
	}
	res, err = runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Findings, []string{"plan: the task is in-review, not running"}) || res.Revision != 0 {
		t.Fatalf("not running = %+v", res)
	}
	if _, ok := runner.takeFiled("t-plan"); ok {
		t.Fatal("a refused filing held for the session")
	}
}

// PlanFileHandler answers the gateway's file_plan with the shape the
// gateway reads: a refused plan 200 with its findings, a valid one its
// revision, a body that is not the pair 400.
func TestPlanFileHandler(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	bounded := fixtureVersion(t)
	runner.cfg.Policy = func() policy.Version { return bounded }
	h := PlanFileHandler(runner)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/plans", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	marshal := func(taskID string, p Plan) string {
		b, err := json.Marshal(map[string]string{"task_id": taskID, "plan": string(mustJSON(p))})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	over := testPlan()
	over.Cards[1].Size = "huge"
	if rec := post(marshal("t-plan", over)); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"revision":0,"sha256":"","findings":["plan: card links-api: size huge: split it; a card is one package, one page or one set of endpoints"]}` {
		t.Fatalf("refused plan: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(marshal("t-plan", testPlan()))
	if rec.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), `{"revision":1,"sha256":"`) {
		t.Fatalf("filed plan: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(marshal("no-such-task", testPlan())); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"plan":"{}"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no task_id: %d", rec.Code)
	}
	if rec := post(`not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("not json: %d", rec.Code)
	}
}

// The plan a session files through the tool is the session's answer: the
// tool's refusal reaches the planner inside the session, the corrected
// filing is what RunPlan returns when the session ends, and both filings
// cost one session, not two (roadmap 178).
func TestRunPlanTakesAToolFiling(t *testing.T) {
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.cfg.Policy = func() policy.Version { return pinned }

	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	ctx := context.Background()
	over := testPlan()
	over.Cards[1].Size = "huge"
	b, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || len(f.Findings) != 1 ||
		f.Findings[0] != "plan: card links-api: size huge: split it; a card is one package, one page or one set of endpoints" {
		t.Fatalf("a filing the plan rules refuse = %+v, %v", f, err)
	}
	b, err = json.Marshal(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	if f, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || f.Revision != 1 {
		t.Fatalf("corrected filing = %+v, %v", f, err)
	}
	kube.scriptPods(0, resultLine(true, "done"))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if res.Revision != 1 || res.SHA256 == "" || len(res.Findings) != 0 {
		t.Fatalf("result = %+v, want the filed revision 1", res)
	}
	if n := len(kube.podOrder); n != 1 {
		t.Fatalf("%d pods, want one session", n)
	}
}

// RunPlan takes its time from the task's class, not the policy's raw
// limits (review-task110.md I-1): reportSession resolves through
// TaskConfig, the planner's own agent_time and check_in, not
// limits.agent_time and limits.check_in. A class override much smaller
// than the limits, as the planner's real shape is (2h/45m against
// 30m/0), proves the wiring: the test activity environment fixes
// activity.GetInfo(ctx).Deadline about 10 minutes out (testRunnerConfig),
// so a session that anchors on the raw limits' 20 minutes starts its
// wall clock already deep in the past and trips at once, while one that
// anchors on the planner's 5 minutes starts on time. Reverting
// reportSession to s.policy.Limits.AgentTime/CheckIn turns this red: the
// runner's supervisor is the srv that fails the test on any call.
func TestRunPlanTakesItsTimeFromThePlannerClassNotTheLimits(t *testing.T) {
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	edit := func(file, old, new string) {
		t.Helper()
		s := string(files[file])
		if !strings.Contains(s, old) {
			t.Fatalf("%s lacks %q", file, old)
		}
		files[file] = []byte(strings.Replace(s, old, new, 1))
	}
	edit("policy.yaml", "  agent_time: 20m\n", "  agent_time: 20m\n  check_in: 15m\n")
	edit("policy.yaml", "  planner:\n    session: plan\n    models: [small, big]\n    supervisor_model: supervisor\n",
		"  planner:\n    session: plan\n    models: [small, big]\n    supervisor_model: supervisor\n    time: {agent_time: 5m, check_in: 0s}\n")
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	pinned := policy.Version{ID: 7, Commit: "c7", Tree: tree}

	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }

	if tc, err := runner.TaskConfig(context.Background(), "t-plan", 7); err != nil || tc.AgentTime != 5*time.Minute || tc.CheckIn != 0 {
		t.Fatalf("TaskConfig = %+v, %v, want agent_time 5m, check_in 0", tc, err)
	}

	logs := envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n" + resultLine(true, "no plan yet")
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string{})
		if err == nil {
			var res PlanResult
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, logs)
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
}

// The planner class's session_model (ruling 330) is the model its plan
// session runs on, whatever rung the plan loop passes.
func TestRunPlanRunsOnTheClassSessionModel(t *testing.T) {
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	edit := func(file, old, new string) {
		t.Helper()
		s := string(files[file])
		if !strings.Contains(s, old) {
			t.Fatalf("%s lacks %q", file, old)
		}
		files[file] = []byte(strings.Replace(s, old, new, 1))
	}
	edit("policy.yaml", "  planner:\n    session: plan\n    models: [small, big]\n",
		"  planner:\n    session: plan\n    models: [small, big, premium]\n    session_model: premium\n")
	edit("policy.yaml", "max_output_tokens: {", "max_output_tokens: {premium: 24576, ")
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	pinned := policy.Version{ID: 7, Commit: "c7", Tree: tree}
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, plan, kube, &fakeObjectStore{})
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }

	logs := envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n" + resultLine(true, "no plan yet")
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string{})
		if err == nil {
			var res PlanResult
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, logs)
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if env := podEnv(kube.podAt(0)); env["MODEL"] != "factory/premium" {
		t.Fatalf("the plan session's MODEL = %q, want factory/premium", env["MODEL"])
	}
}

// deploySpec is a spec with a Deploy line; noDeployOwner the refusal of a
// plan without a card owning deploy/ against it.
const (
	deploySpec    = "Uptime\n\nDeploy: one Deployment with one replica, a Service on 8080. ADMIN_TOKEN comes from a Secret."
	noDeployOwner = "the spec has a Deploy line and no card owns deploy/: add a card whose directories include deploy/ and whose brief names " +
		"the manifests' changes (the Secret, the environment, the volume, the service)"
)

// deployPlan is testPlan whose third card, redirects, owns deploy/.
func deployPlan() Plan {
	p := testPlan()
	p.Cards[2].Directories = []string{"deploy/staging", "internal/x"}
	return p
}

// Stage 16 item 3.6: a spec's Deploy line ("Deploy:" or a Deploy
// heading, any case, leading blanks) needs a card owning deploy/: the
// first card listing deploy or a path under it, cleaned.
func TestDeployOwner(t *testing.T) {
	dot := testPlan()
	dot.Cards[3].Directories = []string{"./deploy"}
	both := deployPlan()
	both.Cards[3].Directories = []string{"deploy"}
	near := testPlan()
	near.Cards[1].Directories = []string{"deployment", "internal/deploy"}
	for _, c := range []struct {
		name, spec string
		p          Plan
		needed     bool
		owner      string
	}{
		{"third card", deploySpec, deployPlan(), true, "redirects"},
		{"no card", deploySpec, testPlan(), true, ""},
		{"no deploy line", "Link shortener\n\nBuild a link shortener.", deployPlan(), false, ""},
		{"heading", "Uptime\n\n## Deploy\n\nOne Deployment.", dot, true, "integration"},
		{"lower case, indented", "Uptime\n\n  deploy: staging only.", both, true, "redirects"},
		{"other paths", deploySpec, near, true, ""},
		{"a sentence", "Uptime\n\nSee the deployment notes. We deploy: later.\nDeployment notes: none.", deployPlan(), false, ""},
		{"Deployment heading", "Uptime\n\n## Deployment\n\nOne replica.", deployPlan(), true, "redirects"},
		{"Deployment line", "Uptime\n\nDeployment: one replica", testPlan(), true, ""},
		{"blank before the colon", "Uptime\n\nDeploy : x", deployPlan(), true, "redirects"},
	} {
		if needed, owner := deployOwner(c.spec, c.p); needed != c.needed || owner != c.owner {
			t.Errorf("%s: deployOwner = %v %q, want %v %q", c.name, needed, owner, c.needed, c.owner)
		}
	}
}

// A plan without a deploy owner against a Deploy spec files no cards, as a
// cycle does; with one the cards are filed and the plan comment marks the
// owner's row.
func TestFileRevisionRefusesAPlanWithoutDeployOwner(t *testing.T) {
	r, store, objects, fj, _ := fileRig(t, testPlan())
	store.tasks["t-plan"] = func(tk Task) Task { tk.Objective = deploySpec; return tk }(store.tasks["t-plan"])
	if ref := requestPlan(t, r); !slices.Equal(ref.Refused, []string{noDeployOwner}) || len(ref.Filed) != 0 {
		t.Fatalf("ref %+v", ref)
	}
	if fj.next != 1 || len(store.tasks) != 1 || len(fj.comments) != 1 || len(store.approvals) != 0 ||
		fj.comments[0].body != "factoryd: revision 1 files no cards: "+noDeployOwner {
		t.Fatalf("issues up to #%d, rows %d, comments %v, approvals %+v", fj.next, len(store.tasks), fj.comments, store.approvals)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"plan_refused [" + noDeployOwner + "]"}) {
		t.Fatalf("steps %v", steps)
	}

	r, store, _, fj, _ = fileRig(t, deployPlan())
	store.tasks["t-plan"] = func(tk Task) Task { tk.Objective = deploySpec; return tk }(store.tasks["t-plan"])
	if ref := requestPlan(t, r); !slices.Equal(ref.Filed, []int64{2, 3, 4, 5}) || len(ref.Refused) != 0 {
		t.Fatalf("ref %+v", ref)
	}
	if len(fj.comments) != 1 {
		t.Fatalf("comments %v", fj.comments)
	}
	for _, want := range []string{
		"| # | key | role | title | size | depends on | issue | deploy |\n|---|---|---|---|---|---|---|---|\n",
		"| 1 | foundation | foundation | Foundation: layout, contract, schema | medium |  | #2 |  |\n",
		"| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api | #4 | deploy |\n",
		"| 4 | integration | integration | Integration: acceptance tests | medium | foundation, links-api, redirects | #5 |  |\n",
	} {
		if !strings.Contains(fj.comments[0].body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, fj.comments[0].body)
		}
	}
	if body := planComment(Task{ID: "t-plan", Objective: "Link shortener"}, 1, deployPlan(), nil); !strings.Contains(body,
		"| # | key | role | title | size | depends on | issue |\n|---|---|---|---|---|---|---|\n") ||
		!strings.Contains(body, "| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api |  |\n") {
		t.Fatalf("a spec without a Deploy line has a deploy column:\n%s", body)
	}
}

// A plan without the deploy owner its Deploy spec needs goes back to the
// planner through the real RequestPlan: no card is filed, and the planner
// revises in exactly one new session with the finding, whose revision with
// an owner files the cards and ends the plan done.
func TestPlanWithoutDeployOwnerThePlannerRevises(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := fileRig(t, testPlan())
	store.tasks["t-plan"] = func(tk Task) Task { tk.Objective = deploySpec; return tk }(store.tasks["t-plan"])
	raw, sha, _ := planSHA(deployPlan())
	if n, err := store.AddPlanRevision(ctx, "t-plan", raw, sha); n != 2 || err != nil {
		t.Fatalf("revision 2: %d, %v", n, err)
	}
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: store.plans["t-plan"][0].SHA256}, PlanResult{Revision: 2, SHA256: sha})
	h.requestPlan = func(taskID string, revision int) (PlanRef, error) {
		return r.RequestPlan(ctx, "t-plan", revision, 7)
	}
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 filed 4 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 2 || h.plans[0] != nil || !slices.Equal(h.plans[1], []string{noDeployOwner}) || !slices.Equal(h.requests, []int{1, 2}) {
		t.Fatalf("plans %q requests %v", h.plans, h.requests)
	}
	if fj.next != 5 || len(cardsByKey(t, store)) != 4 || !strings.HasPrefix(fj.comments[0].body, "factoryd: revision 1 files no cards: "+noDeployOwner) {
		t.Fatalf("issues up to #%d, cards %v, comments %v", fj.next, cardsByKey(t, store), fj.comments)
	}
}

// RunPlan answers a provider's refusal, not an invalid plan (roadmap 155).
func TestRunPlanNamesAProviderRefusal(t *testing.T) {
	const said = "DashscopeException: budget exhausted (status 429)"
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, plan, kube, &fakeObjectStore{})
	store.providerRefusals = map[string]string{"t-plan": "upstream 503"}
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(false, said))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if res.Refused != (ProviderWait{Status: 503, Reason: said}) || len(res.Findings) != 0 || res.Revision != 0 || res.Escalate != "" {
		t.Fatalf("result %+v, want the provider's refusal alone", res)
	}
}

// A planner session the gateway refused at a cap answers the refusal
// beside the findings a run from before the change reads (roadmap 240).
func TestRunPlanNamesACap(t *testing.T) {
	const said = "the model call failed: refused_day_cap"
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, plan, kube, &fakeObjectStore{})
	store.capRefusals = map[string]CapRefusal{"t-plan": dayRefusal}
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(false, said))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	want := []string{"plan: the session ended without filing a plan through the file_plan tool", "plan: the session ended without a last message: " + said}
	if res.Capped == nil || res.Capped.Refusal != dayRefusal.Refusal || !res.Capped.At.Equal(dayRefusal.At) || !slices.Equal(res.Findings, want) {
		t.Fatalf("result %+v, want the day cap's refusal and the findings %q", res, want)
	}
}

// A planner session whose model call crossed its cost hold names the
// call's state in its finding (roadmap 241): the tokens, the cost, the
// hold and the fitting action reach the next planner session.
func TestRunPlanNamesACostHoldCrossing(t *testing.T) {
	const said = `the agent's last model step finished on "length" after 24576 output and 0 reasoning tokens, not on "stop"`
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2,
		Objective: "Link shortener\n\nBuild a link shortener."}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, plan, kube, &fakeObjectStore{})
	store.overHolds = map[string]OverHoldCall{"t-plan": {Alias: "big", PromptTokens: 22680, CachedTokens: 8896, OutputTokens: 24576,
		OutputLimit: 24576, HoldEUR: 0.122078, CostEUR: 0.129745}}
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(false, said))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	want := []string{"plan: the session ended without filing a plan through the file_plan tool",
		"plan: the session ended without a last message: " + said + "; a model call on big crossed its cost hold: " +
			"22680 prompt tokens (8896 cached) and 24576 output tokens cost 0.129745 EUR against a hold of 0.122078 EUR; " +
			"its output ran to the limit of 24576 tokens, so a shorter output fits: write the change in smaller pieces"}
	if !slices.Equal(res.Findings, want) {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
}

// A planner session the day cap refused at its first call is no attempt
// (roadmap 240): the plan escalates with the refusal, waits for the day's
// turn on its own and runs the planner again with the same findings; no
// "no plan" round. A month's cap waits on a writer, whose comment the
// planner gets, and the refused session counts.
func TestPlanLoopWaitsOnACap(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Findings: []string{"plan: the session ended without a last message: refused"}, Capped: &dayRefusal},
		PlanResult{Revision: 1, SHA256: "sha-1"})
	h.env.SetStartTime(oct(14, 20, 43))
	var turns []time.Time
	h.env.OnActivity(ActivityDayResume, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, turned bool) (bool, error) {
			if turned {
				turns = append(turns, h.env.Now())
			}
			return turned, nil
		})
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{dayReason}) || !sameTimes(turns, []time.Time{oct(15, 0, 0)}) {
		t.Fatalf("final %q, escalations %q, turns %v; want the day cap asked and the turn at 00:00", final, h.escalated, turns)
	}
	if len(h.plans) != 2 || h.plans[0] != nil || h.plans[1] != nil {
		t.Fatalf("plans %q, want two sessions with no findings", h.plans)
	}
	if u := h.usage[len(h.usage)-1]; h.usage[0].Attempts != 0 || u.Attempts != 1 || u.AgentTime != planSession {
		t.Fatalf("usage %+v, want the refused session no attempt, the second one", h.usage)
	}

	month := CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "spent"}}
	h = newPlanHarness(t, PlanResult{Findings: []string{"plan: the session ended without a last message: refused"}, Capped: &month},
		PlanResult{Revision: 1, SHA256: "sha-1"})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 700})
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthCapReached + "spent"}) {
		t.Fatalf("final %q, escalations %q; want the month cap asked", final, h.escalated)
	}
	if len(h.plans) != 2 || !slices.Equal(h.plans[1], []string{"writer sebastian asked for changes to the plan: go on"}) || h.usage[0].Attempts != 1 {
		t.Fatalf("plans %q, usage %+v; want the writer's comment for the second session, the first counted", h.plans, h.usage)
	}
}

// A planner session whose pod never left Pending ran no planner
// (versionPodWait): no attempt and no agent time, and the planner runs
// again at once with the same findings, no "no plan" round. One planner
// session's time is planSession.
func TestPlanLoopRunsAgainWhenItsPodNeverStarted(t *testing.T) {
	why := podPendingWhy("s1")
	waited := PlanResult{Findings: []string{"plan: the session ended without a last message: " + why}, NotStarted: why}
	h := newPlanHarness(t, waited, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" || len(h.escalated) != 0 || len(h.plans) != 2 || !slices.Equal(h.plans[1], h.plans[0]) {
		t.Fatalf("final %q, escalations %q, plans %q; want done after a second session with the first's findings", final, h.escalated, h.plans)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 1 || u.AgentTime != planSession {
		t.Fatalf("usage %+v, want one attempt and one session's time", h.usage)
	}
}

var planRefused = PlanResult{Refused: ProviderWait{Status: 429, Reason: refusedSession.Reason}}

// A planner session the provider refused is no attempt (roadmap 155): the
// plan waits on the provider and the planner runs again, same findings.
func TestPlanLoopWaitsOnTheProvider(t *testing.T) {
	h := newPlanHarness(t, planRefused, planRefused, PlanResult{Revision: 1, SHA256: "sha-1"})
	backoffs := providerBackoffs(h.workflowHarness)
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 3 || slices.ContainsFunc(h.plans, func(f []string) bool { return f != nil }) || !slices.Equal(*backoffs, []int{120, 240}) ||
		!slices.Equal(h.states, []string{"running", "running", "running"}) || len(h.usage) != 1 || h.usage[0].Attempts != 1 || h.usage[0].AgentTime != planSession {
		t.Fatalf("plans %q, backoffs %v, states %v, usage %+v; want three sessions with no findings after waits of 120 and 240 s, one attempt", h.plans, *backoffs, h.states, h.usage)
	}
}

// Six refused planner sessions in a row ask a human with source provider;
// no attempt is counted.
func TestPlanLoopSixProviderRefusalsAskAHuman(t *testing.T) {
	h := newPlanHarness(t, planRefused)
	backoffs := providerBackoffs(h.workflowHarness)
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	want := "the model provider refused the first call of 6 sessions in a row (status 429): " + refusedSession.Reason
	if final := h.run(t); final != "closed" || len(h.plans) != 6 || !slices.Equal(*backoffs, []int{120, 240, 480, 960, 1800}) ||
		!slices.Equal(h.escalated, []string{want}) || escalationSource(want) != "provider" || len(h.usage) != 0 {
		t.Fatalf("final %q after %d sessions, backoffs %v, escalations %q, usage %+v; want %q, no attempt", final, len(h.plans), *backoffs, h.escalated, h.usage, want)
	}
}

// A plan run from before versionSessionRefusals takes a refusal as a plan
// without a last message: an attempt, its finding to the next session.
func TestPlanLoopRefusalBeforeTheChange(t *testing.T) {
	h := newPlanHarness(t, planRefused, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.env.OnGetVersion("provider-wait-sessions", workflow.DefaultVersion, versionSessionRefusals).Return(workflow.DefaultVersion)
	backoffs := providerBackoffs(h.workflowHarness)
	h.file = map[int]bool{1: true}
	want := []string{"plan: the session ended without a last message: " + refusedSession.Reason}
	if final := h.run(t); final != "done" || len(*backoffs) != 0 || len(h.usage) != 2 || h.usage[1].Attempts != 2 || len(h.plans) != 2 || !slices.Equal(h.plans[1], want) {
		t.Fatalf("final %q, backoffs %v, usage %+v, plans %q; want the second with %q", final, *backoffs, h.usage, h.plans, want)
	}
}

// A card's rung and reason reach its issue as a Rung line intake reads
// (roadmap 222, item 5); a card without one carries none.
func TestCardFactsRungLine(t *testing.T) {
	facts := cardFacts(PlanCard{Size: "small", Rung: RungFrontier, RungReason: "touches the queue"})
	if !strings.HasSuffix(facts, "Size: small\nRung: frontier, touches the queue\n") {
		t.Fatalf("facts = %q", facts)
	}
	if facts := cardFacts(PlanCard{Size: "small"}); strings.Contains(facts, "Rung") {
		t.Fatalf("facts = %q, want no rung line", facts)
	}
	if got := cardFindings(PlanCard{Key: "a", Title: "t", Brief: "b", Size: "small", Rung: "huge"}, testLimits); !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, "rung huge") }) {
		t.Fatalf("findings = %q, want the rung refused", got)
	}
}

// Roadmap 201: a plan card's brief carries its directories once, in the
// repository map, and its size and rung not at all: its objective keeps
// the card's text, its dependencies and its acceptance items. Without a
// map the directories line stays.
func TestPlanCardObjectiveLeavesItsFieldsToTheCard(t *testing.T) {
	c := PlanCard{Key: "redirects", Title: "Redirects", Brief: "GET /<slug> answers 302.", Directories: []string{"internal/links"},
		Acceptance: []PlanItem{{Text: "GET /abc answers 302"}}, Size: "small", Rung: RungCheap, RungReason: "routine"}
	objective := "Redirects\n\n" + cardBody(c, "plan #1", 3, 4, "sebastian", []Dependency{{Issue: 2, Key: "foundation"}}, "")
	want := "Redirects\n\nCard redirects of plan #1 (3 of 4), filed for sebastian. Written by the planner.\n\nGET /<slug> answers 302.\n\n" +
		"Depends on: #2 (foundation)\nAcceptance:\n- GET /abc answers 302"
	if got := briefObjective(objective, true); got != want {
		t.Fatalf("brief objective =\n%s\nwant\n%s", got, want)
	}
	if got := briefObjective(objective, false); !strings.Contains(got, "Directories (guidance, not a boundary): internal/links\nDepends on") || strings.Contains(got, "Size") || strings.Contains(got, "Rung") {
		t.Fatalf("brief objective without a map =\n%s", got)
	}
}

func TestReadsFindingNamesThePathsAndTheWayOn(t *testing.T) {
	got := readsFinding([]string{"/workspace/repo/a.go", "/workspace/docs/b.md"})
	want := "the planner session read 2 files before it ended: /workspace/repo/a.go, /workspace/docs/b.md. File the plan from what they hold; read a file again only where the plan needs a detail."
	if got != want {
		t.Fatalf("readsFinding = %q, want %q", got, want)
	}
}

// A planner that used its time gets one fresh session with its findings
// instead of a fail. The cap is 20m and a human's answer granted 5m: 20m
// used leaves 5m, the first session's whole time (5m + 2m grace), and it
// uses all of it. 0 left is under the 5m a session needs, so 10m of retry
// time top the next session up to half the cap (10m + 2m grace); the
// human's 5m neither block that nor grow.
func TestPlanLoopTimeLimitStartsAFreshPlanner(t *testing.T) {
	found := []string{"plan: the session ended without filing a plan through the file_plan tool", "the planner session read 1 files before it ended: /w/a.go. File the plan from what they hold; read a file again only where the plan needs a detail."}
	h := newPlanHarness(t, PlanResult{Findings: found}, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 20 * time.Minute, ExtraTime: 5 * time.Minute, Attempts: 1}}
	var budgets []time.Duration
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActivityRunPlan {
			budgets = append(budgets, info.StartToCloseTimeout)
		}
	})
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" {
		t.Fatalf("final %q, finals %v, want done after a second planner", final, h.finals)
	}
	if len(h.plans) != 2 || h.plans[0] != nil || !slices.Equal(h.plans[1], found) {
		t.Fatalf("plans %q, want a second session with the first's findings", h.plans)
	}
	if !slices.Equal(budgets, []time.Duration{7 * time.Minute, 12 * time.Minute}) {
		t.Fatalf("RunPlan budgets %v, want 7m and 12m", budgets)
	}
	if want := (TaskUsage{AgentTime: 25 * time.Minute, ExtraTime: 5 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 2}); len(h.usage) < 2 || h.usage[1] != want {
		t.Fatalf("usage %+v, want the grant %+v second", h.usage, want)
	}
}

// Once only: a planner out of time with no automatic session to come asks
// a human, its retry used (20m cap + 10m retry - 30m used = 0 left) or its
// cap too short for one (half of 8m, 4m, is under the 5m a session needs).
// The answer buys what an answer leaves a session, 5m (a quarter of 20m;
// for 8m the 5m minimum), and starts a session with them (5m + 2m grace)
// and the answer. A run from before the gate fails at once.
func TestPlanLoopOutOfTimeAsksAHuman(t *testing.T) {
	retryUsed := TaskUsage{AgentTime: 30 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 1}
	shortCap := TaskUsage{AgentTime: 8 * time.Minute, Attempts: 1}
	for _, tc := range []struct {
		name     string
		cap      time.Duration
		usage    TaskUsage
		version  workflow.Version
		question string // "" for the old fail
	}{
		{"retry used", 0, retryUsed, versionPlannerRetry,
			"the planner used its agent time and its automatic second session without filing a plan: an answer starts one more planner session with 5 minutes of agent time and the answer among its findings"},
		{"cap too short", 8 * time.Minute, shortCap, versionPlannerRetry,
			"the planner used its agent time without filing a plan, and its 8-minute cap is too short for an automatic second session: an answer starts one more planner session with 5 minutes of agent time and the answer among its findings"},
		{"retry used, before planner-retry", 0, retryUsed, workflow.DefaultVersion, ""},
		{"cap too short, before planner-retry", 8 * time.Minute, shortCap, workflow.DefaultVersion, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
			if tc.cap != 0 {
				h.taskConfig.AgentTime = tc.cap
			}
			h.state = RunState{Status: "running", Usage: tc.usage}
			h.env.OnGetVersion("planner-retry", workflow.DefaultVersion, versionPlannerRetry).Return(tc.version)
			var budgets []time.Duration
			h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
				if info.ActivityType.Name == ActivityRunPlan {
					budgets = append(budgets, info.StartToCloseTimeout)
				}
			})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "plan the API alone", Comment: 801})
			h.file = map[int]bool{1: true}
			final := h.run(t)
			if tc.question == "" {
				if final != "failed" || h.finals[0][1] != "wall-clock cap reached" || len(h.plans) != 0 || len(h.escalated) != 0 || len(h.usage) != 0 {
					t.Fatalf("final %q, finals %v, plans %q, escalated %q, usage %+v, want the old fail", final, h.finals, h.plans, h.escalated, h.usage)
				}
				return
			}
			if final != "done" || !slices.Equal(h.escalated, []string{tc.question}) || escalationSource(tc.question) != "cap" {
				t.Fatalf("final %q, finals %v, escalated %q, want done after the question", final, h.finals, h.escalated)
			}
			if len(h.plans) != 1 || !slices.Equal(h.plans[0], []string{"writer sebastian asked for changes to the plan: plan the API alone"}) ||
				!slices.Equal(budgets, []time.Duration{7 * time.Minute}) {
				t.Fatalf("plans %q, budgets %v, want one session of 7m with the answer", h.plans, budgets)
			}
			want := tc.usage
			want.ExtraTime = 5 * time.Minute
			if len(h.usage) == 0 || h.usage[0] != want {
				t.Fatalf("usage %+v, want the answer's grant %+v first", h.usage, want)
			}
		})
	}
}

// An answer to the planner's own question that finds its time used, the
// retry with it, buys agent time and asks nothing more: 25m used of the
// 20m cap + 10m retry leaves 5m, a session (5m + 2m grace) that asks and
// uses them; the answer buys 5m for the next (7m again).
func TestPlanLoopAnswerAtTheTimeLimitBuysTime(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Escalate: "which login do accounts use?"}, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 25 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 1}}
	var budgets []time.Duration
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActivityRunPlan {
			budgets = append(budgets, info.StartToCloseTimeout)
		}
	})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "magic links", Comment: 801})
	h.file = map[int]bool{1: true}
	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{"which login do accounts use?"}) {
		t.Fatalf("final %q, finals %v, escalated %q, want done after the planner's own question alone", final, h.finals, h.escalated)
	}
	if len(h.plans) != 2 || !slices.Equal(h.plans[1], []string{"writer sebastian asked for changes to the plan: magic links"}) ||
		!slices.Equal(budgets, []time.Duration{7 * time.Minute, 7 * time.Minute}) {
		t.Fatalf("plans %q, budgets %v", h.plans, budgets)
	}
	if want := (TaskUsage{AgentTime: 30 * time.Minute, ExtraTime: 5 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 2}); len(h.usage) < 2 || h.usage[1] != want {
		t.Fatalf("usage %+v, want the answer's grant %+v second", h.usage, want)
	}
}

// A run healed while it waited on the time question asks it again
// (healedReason, which opens no second alert) and its answer still buys
// the time: 30m used of the 20m cap + 10m retry leaves none, the answer
// buys 5m, and the session (5m + 2m grace) files the plan.
func TestPlanLoopHealedOnTheTimeQuestionAsksAgain(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.state = RunState{Status: "waiting-human", Usage: TaskUsage{AgentTime: 30 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 1}}
	h.requestPlan = func(taskID string, revision int) (PlanRef, error) {
		if revision == 0 {
			return PlanRef{}, nil // the planner filed no plan: the wait was the time question's
		}
		return PlanRef{Revision: revision, SHA256: "sha-" + strconv.Itoa(revision), Filed: []int64{2, 3, 4}}, nil
	}
	var budgets []time.Duration
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActivityRunPlan {
			budgets = append(budgets, info.StartToCloseTimeout)
		}
	})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "plan the API alone", Comment: 801})
	if final := h.run(t); final != "done" {
		t.Fatalf("final %q, finals %v, want done after the re-ask", final, h.finals)
	}
	if !slices.Equal(h.escalated, []string{healedReason}) {
		t.Fatalf("escalated %q, want the healed re-ask alone: Escalate opens an alert for every reason but it", h.escalated)
	}
	if len(h.plans) != 1 || !slices.Equal(h.plans[0], []string{"writer sebastian asked for changes to the plan: plan the API alone"}) ||
		!slices.Equal(budgets, []time.Duration{7 * time.Minute}) {
		t.Fatalf("plans %q, budgets %v, want one session of 7m with the answer", h.plans, budgets)
	}
	if want := (TaskUsage{AgentTime: 30 * time.Minute, ExtraTime: 5 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 1}); len(h.usage) == 0 || h.usage[0] != want {
		t.Fatalf("usage %+v, want the answer's grant %+v first", h.usage, want)
	}
}

// A run from before the change fails at the limit (20m cap, 20m used).
func TestPlanLoopBeforePlannerRetryFailsAtTheLimit(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Findings: []string{"plan: no plan"}})
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
	h.env.OnGetVersion("planner-retry", workflow.DefaultVersion, versionPlannerRetry).Return(workflow.DefaultVersion)
	if final := h.run(t); final != "failed" || h.finals[0][1] != "wall-clock cap reached" || len(h.plans) != 0 {
		t.Fatalf("final %q, finals %v, plans %q", final, h.finals, h.plans)
	}
}

// A plan card's dependencies are stored before its issue exists: the
// issue's label event can start the card through intake, and a start
// that found no dependency would run it ahead of them.
func TestPlanCardsHoldTheirDependenciesBeforeTheirIssues(t *testing.T) {
	r, store, _, fj, _ := fileRig(t, testPlan())
	atIssue := map[string][]string{}
	fj.onIssue = func(title string) {
		for id, c := range store.tasks {
			if c.Parent == "t-plan" && strings.HasPrefix(c.Objective, title+"\n") {
				atIssue[c.PlanKey] = slices.Clone(store.deps[id])
			}
		}
	}
	if ref := requestPlan(t, r); len(ref.Filed) != 4 {
		t.Fatalf("ref = %+v", ref)
	}
	for key, c := range cardsByKey(t, store) {
		got, ok := atIssue[key]
		if want := store.deps[c.ID]; !ok || !slices.Equal(got, want) {
			t.Errorf("%s: dependencies at its issue %v, want its own %v", key, got, want)
		}
	}
	if n := len(store.deps); n == 0 {
		t.Fatal("the plan filed no dependency")
	}
}
