package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The plan (stage11-design.md section 1.2): a plan card's planner answers
// a plan, an approver accepts one revision, and factoryd files its cards.
const (
	// SignalPlanAccepted is the console's Accept plan (AcceptPlan): Author
	// the approver, Revision and SHA256 the revision they read.
	SignalPlanAccepted = "plan_accepted"
	// SignalPlanRejected is the console's Reject (RejectPlan): Author the
	// writer, Body why. The plan task ends failed.
	SignalPlanRejected = "plan_rejected"
	// SignalPlanAnswered is the console's answer to a plan's question
	// (AnswerPlan): Author the member, Revision and Index the question.
	SignalPlanAnswered = "plan_answered"

	ActivityRunPlan        = "RunPlan"
	ActivityRequestPlan    = "RequestPlan"
	ActivityCreateChildren = "CreateChildren"
	// ActivityOpenPlanQuestions counts a revision's open questions.
	ActivityOpenPlanQuestions = "OpenPlanQuestions"
)

// The roles of a plan's cards.
const (
	RoleFoundation  = "foundation"
	RoleFeature     = "feature"
	RoleIntegration = "integration"
)

// The plan's own caps: the report's transport cap (maxReport) never cuts a
// valid plan.
const (
	maxPlanBytes      = 12 << 10
	maxCardBrief      = 1500
	maxCardTitle      = 120
	maxCardDirs       = 10
	maxCardAcceptance = 10
	maxAcceptanceItem = 300
	maxQuestions      = 5
	maxQuestionText   = 300
)

// Plan is the planner's answer: one fenced json block of its last message.
type Plan struct {
	Summary   string         `json:"summary"`
	Cards     []PlanCard     `json:"cards"`
	Questions []PlanQuestion `json:"questions,omitempty"`
}

// PlanQuestion is an open point the spec leaves to the member, with the
// choices the planner offers; the store adds the member's answer, "" until
// it is given.
type PlanQuestion struct {
	Text       string    `json:"text"`
	Choices    []string  `json:"choices"`
	Answer     string    `json:"-"`
	AnsweredBy string    `json:"-"`
	AnsweredAt time.Time `json:"-"`
}

// PlanCard is one card of a plan: one pull request, one session.
type PlanCard struct {
	Key         string   `json:"key"`
	Role        string   `json:"role"`
	Title       string   `json:"title"`
	Brief       string   `json:"brief"`
	Directories []string `json:"directories"`
	DependsOn   []string `json:"depends_on"`
	Acceptance  []string `json:"acceptance"`
	Size        string   `json:"size"`
	// Rung and RungReason: the model rung the card starts on and why,
	// cheap or frontier; "" lets intake derive it (roadmap 222, item 5)
	Rung       string `json:"rung,omitempty"`
	RungReason string `json:"rung_reason,omitempty"`
}

// PlanRevision is one valid plan of a plan task, as stored.
type PlanRevision struct {
	TaskID    string
	Revision  int
	Plan      json.RawMessage
	SHA256    string
	CreatedAt time.Time
}

// PlanSpec is the spec of an approval request of kind plan.
type PlanSpec struct {
	Revision int    `json:"revision"`
	SHA256   string `json:"sha256"`
}

// PlanResult is one planner session: the revision its valid plan became,
// the findings of an invalid one, or the stop that asks a human.
type PlanResult struct {
	Revision int
	SHA256   string
	Findings []string
	Escalate string
	// Refused: the provider refused the session's first call (Status 0:
	// it did not).
	Refused ProviderWait
	// StartFailed: the session could not start (Escalate says why). No
	// planner ran, so it took no agent time and is no attempt.
	StartFailed bool `json:",omitempty"`
}

// maxPlanEcho bounds the last message a planner answered without a plan,
// as its next session's finding.
const maxPlanEcho = 2000

// PlanRef is the plan revision a plan task waits on, and its comment on
// the issue: only later comments ask for changes. Zero: no plan yet.
type PlanRef struct {
	Revision int
	SHA256   string
	Comment  int64
}

// ChildrenResult is CreateChildren's: the cards filed, or why the
// acceptance was refused.
type ChildrenResult struct {
	Issues  []int64
	Refused []string
}

var (
	planBlock = regexp.MustCompile("(?s)```json[ \t]*\r?\n(.*?)\r?\n[ \t]*```")
	cardKey   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	roles     = []string{RoleFoundation, RoleFeature, RoleIntegration}
	// acceptedComment is PlanAcceptedComment of any revision
	acceptedComment = regexp.MustCompile(`^Plan revision [0-9]+ accepted\.$`)
	// deployLine is a spec's line asking for a deployment: "Deploy:" or
	// "Deployment:", or a Deploy or Deployment heading
	deployLine = regexp.MustCompile(`(?im)^[ \t]*(deploy(ment)?[ \t]*:|#+[ \t]+deploy(ment)?\b)`)
)

// ErrNoDeployOwner refuses a plan whose spec has a Deploy line and no card
// owning the deploy manifests; its text is the planner's finding.
var ErrNoDeployOwner = errors.New("the spec has a Deploy line and no card owns deploy/: add a card whose directories include deploy/ " +
	"and whose brief names the manifests' changes (the Secret, the environment, the volume, the service)")

// deployOwner is whether spec has a Deploy line, and the key of p's first
// card whose directories hold deploy/ or a path under it.
func deployOwner(spec string, p Plan) (needed bool, owner string) {
	if !deployLine.MatchString(spec) {
		return false, ""
	}
	for _, c := range p.Cards {
		for _, d := range c.Directories {
			if d = path.Clean("/" + d); d == "/deploy" || strings.HasPrefix(d, "/deploy/") {
				return true, c.Key
			}
		}
	}
	return true, ""
}

// ValidatePlan judges the planner's last message against the process's plan
// shape and the pinned limits: one line per finding, none for a valid plan.
func ValidatePlan(text string, shape policy.PlanShape, limits policy.Limits, bound int) []string {
	_, findings := readPlan(text, shape, limits, bound)
	return findings
}

// readPlan is ValidatePlan with the plan it decoded. Bound is the risk
// bound the sizes derive from (0: none).
func readPlan(text string, shape policy.PlanShape, limits policy.Limits, bound int) (Plan, []string) {
	blocks := planBlock.FindAllStringSubmatch(text, -1)
	if len(blocks) == 0 {
		return Plan{}, []string{"plan: no fenced json block in the last message"}
	}
	block := blocks[len(blocks)-1][1]
	if len(block) > maxPlanBytes {
		size := fmt.Sprintf("%.1f KiB", float64(len(block))/1024)
		if size == "12.0 KiB" {
			size = fmt.Sprintf("%d bytes", len(block))
		}
		return Plan{}, []string{"plan: the plan is " + size + "; at most 12 KiB: shorten the briefs"}
	}
	var p Plan
	dec := json.NewDecoder(strings.NewReader(block))
	dec.DisallowUnknownFields()
	err := dec.Decode(&p)
	if err == nil && dec.Decode(new(json.RawMessage)) != io.EOF {
		err = errors.New("data after the plan's object")
	}
	if err != nil {
		return Plan{}, []string{"plan: json: " + strings.TrimPrefix(err.Error(), "json: ")}
	}
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf("plan: "+format, args...)) }
	if n := len(p.Cards); n < 2 || n > shape.MaxCards {
		add("%d cards; this process allows 2 to %d", n, shape.MaxCards)
	}
	keys := map[string]int{}
	for i, c := range p.Cards {
		if !cardKey.MatchString(c.Key) {
			add("card %d: key %q: lowercase letters, digits and dashes, 2 to 32", i+1, c.Key)
		}
		if keys[c.Key]++; keys[c.Key] == 2 {
			add("card key %s is used twice", c.Key)
		}
	}
	for _, c := range p.Cards {
		if !slices.Contains(roles, c.Role) {
			add("card %s: role %q, want one of %v", c.Key, c.Role, roles)
		}
	}
	foundation, integration := cardsOf(p, RoleFoundation), cardsOf(p, RoleIntegration)
	if shape.Foundation && len(foundation) != 1 {
		add("%d cards with role foundation; the process wants exactly one", len(foundation))
	}
	if shape.Integration && len(integration) != 1 {
		add("%d cards with role integration; the process wants exactly one", len(integration))
	}
	for _, c := range p.Cards {
		if c.Role == RoleFoundation && len(c.DependsOn) > 0 {
			add("card %s: a foundation card depends on nothing", c.Key)
		}
		for _, d := range c.DependsOn {
			switch {
			case d == c.Key:
				add("card %s: depends on itself", c.Key)
			case keys[d] == 0:
				add("card %s: depends on %q, which is no card of the plan", c.Key, d)
			case slices.Contains(integration, d):
				add("card %s: depends on the integration card, which comes last", c.Key)
			}
		}
	}
	if cycle := planCycle(p, planEdges(p)); cycle != nil {
		add("the dependencies form a cycle: %s", strings.Join(cycle, " -> "))
	}
	for _, c := range p.Cards {
		out = append(out, cardFindings(c, limits, bound)...)
	}
	return p, append(out, questionFindings(p.Questions)...)
}

// questionFindings are the findings on a plan's questions: 0 to 5, each
// with a text of 1 to 300 characters and 2 to 5 distinct choices of 1 to
// 60.
func questionFindings(qs []PlanQuestion) []string {
	var out []string
	if len(qs) > maxQuestions {
		out = append(out, fmt.Sprintf("plan: %d questions; want 0 to %d", len(qs), maxQuestions))
	}
	for i, q := range qs {
		add := func(format string, args ...any) {
			out = append(out, fmt.Sprintf("plan: question %d: "+format, append([]any{i + 1}, args...)...))
		}
		switch n := utf8.RuneCountInString(q.Text); {
		case n == 0:
			add("text empty")
		case n > maxQuestionText:
			add("text over %d characters", maxQuestionText)
		}
		if n := len(q.Choices); n < 2 || n > MaxChoices {
			add("%d choices; want 2 to %d", n, MaxChoices)
		}
		seen := map[string]int{}
		for j, c := range q.Choices {
			seen[c]++
			switch n := utf8.RuneCountInString(c); {
			case n == 0:
				add("choice %d is empty", j+1)
			case n > MaxChoice:
				add("choice %d is over %d characters", j+1, MaxChoice)
			case seen[c] == 2:
				add("choice %q is offered twice", c)
			}
		}
	}
	return out
}

// sizeFindings are a card size's findings, the plan card's and the ready
// check's alike: one of the card sizes, whose lines stay under the risk
// bound.
func sizeFindings(size string, bound int) []string {
	var out []string
	if n, ok := sizeLines(bound)[size]; ok && bound > 0 && n >= bound {
		out = append(out, fmt.Sprintf("size %s plans up to %d changed lines and the bound is %d: split the card", size, n, bound))
	}
	switch {
	case slices.Contains(policy.CardSizes, size):
	case size == "":
		out = append(out, `size "": want small, medium or large`)
	default:
		out = append(out, fmt.Sprintf("size %s: split it; a card is one package, one page or one set of endpoints", size))
	}
	return out
}

// acceptanceFindings are a card's acceptance criteria's findings, the
// plan card's and the ready check's alike.
func acceptanceFindings(acceptance []string) []string {
	var out []string
	if n := len(acceptance); n < 1 || n > maxCardAcceptance {
		out = append(out, fmt.Sprintf("%d acceptance tests; want 1 to %d", n, maxCardAcceptance))
	}
	for i, a := range acceptance {
		switch n := utf8.RuneCountInString(a); {
		case n == 0:
			out = append(out, fmt.Sprintf("acceptance test %d is empty", i+1))
		case n > maxAcceptanceItem:
			out = append(out, fmt.Sprintf("acceptance test %d is over %d characters", i+1, maxAcceptanceItem))
		}
	}
	return out
}

// cardFindings are one card's findings on its own fields: a size whose
// derived lines reach the risk bound does not fit under it.
func cardFindings(c PlanCard, limits policy.Limits, bound int) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("plan: card %s: "+format, append([]any{c.Key}, args...)...))
	}
	for _, f := range sizeFindings(c.Size, bound) {
		add("%s", f)
	}
	if c.Rung != "" && c.Rung != RungCheap && c.Rung != RungFrontier {
		add("rung %s: want cheap, frontier or none", c.Rung)
	}
	switch n := utf8.RuneCountInString(c.Title); {
	case n == 0:
		add("title empty")
	case n > maxCardTitle:
		add("title over %d characters", maxCardTitle)
	}
	switch n := utf8.RuneCountInString(c.Brief); {
	case n == 0:
		add("brief empty")
	case n > maxCardBrief:
		add("brief over %d characters", maxCardBrief)
	}
	if len(c.Directories) > maxCardDirs {
		add("%d directories; want 0 to %d", len(c.Directories), maxCardDirs)
	}
	for _, d := range c.Directories {
		clean := path.Clean(d)
		first, _, _ := strings.Cut(clean, "/")
		_, workflow := WorkflowDir(first)
		switch {
		case d == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../"):
			add("directory %q: a relative path inside the repository", d)
		case workflow:
			add("directory %q: closed to agents, the template owns it", d)
		}
	}
	for _, f := range acceptanceFindings(c.Acceptance) {
		add("%s", f)
	}
	return out
}

// cardsOf are the keys of p's cards of role.
func cardsOf(p Plan, role string) []string {
	var keys []string
	for _, c := range p.Cards {
		if c.Role == role {
			keys = append(keys, c.Key)
		}
	}
	return keys
}

// planEdges are each card's dependencies as factoryd files them: the ones
// the planner listed among the plan's cards, every card's on the
// foundation card, the integration card's on every other card. Edges
// ValidatePlan refuses on their own (out of the foundation, into the
// integration card, onto itself or no card) are left out, so a cycle is
// only ever one of the feature cards.
func planEdges(p Plan) map[string][]string {
	foundation, integration := cardsOf(p, RoleFoundation), cardsOf(p, RoleIntegration)
	keys := map[string]bool{}
	for _, c := range p.Cards {
		keys[c.Key] = true
	}
	edges := map[string][]string{}
	add := func(from, to string) {
		if from != to && !slices.Contains(edges[from], to) {
			edges[from] = append(edges[from], to)
		}
	}
	for _, c := range p.Cards {
		if c.Role == RoleFoundation {
			continue
		}
		for _, f := range foundation {
			add(c.Key, f)
		}
		for _, d := range c.DependsOn {
			if keys[d] && !slices.Contains(integration, d) {
				add(c.Key, d)
			}
		}
		if c.Role == RoleIntegration {
			for _, o := range p.Cards {
				if o.Role != RoleIntegration {
					add(c.Key, o.Key)
				}
			}
		}
	}
	return edges
}

// planCycle is the first cycle of edges in the plan's order, as the keys
// along it with the first repeated; nil when there is none.
func planCycle(p Plan, edges map[string][]string) []string {
	const (
		unseen = iota
		onPath
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(k string) []string
	visit = func(k string) []string {
		state[k] = onPath
		stack = append(stack, k)
		for _, d := range edges[k] {
			switch state[d] {
			case onPath:
				i := slices.Index(stack, d)
				return append(slices.Clone(stack[i:]), d)
			case unseen:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[k] = done
		return nil
	}
	for _, c := range p.Cards {
		if state[c.Key] == unseen {
			if cycle := visit(c.Key); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// planOrder is the plan's cards in topological order of edges: each after
// every card it depends on, ties in the plan's order.
func planOrder(p Plan, edges map[string][]string) []PlanCard {
	var out []PlanCard
	placed := map[string]bool{}
	for len(out) < len(p.Cards) {
		progress := false
		for _, c := range p.Cards {
			if placed[c.Key] || slices.ContainsFunc(edges[c.Key], func(d string) bool { return !placed[d] }) {
				continue
			}
			out, placed[c.Key], progress = append(out, c), true, true
			break
		}
		if !progress {
			panic("planOrder: a cycle ValidatePlan let through")
		}
	}
	return out
}

// dependsOn are the keys card key depends on (edges) in filing order.
func dependsOn(order []PlanCard, edges map[string][]string, key string) []string {
	var out []string
	for _, c := range order {
		if slices.Contains(edges[key], c.Key) {
			out = append(out, c.Key)
		}
	}
	return out
}

// planSHA is the sha256 of the plan's canonical JSON, which is what a
// revision stores.
func planSHA(p Plan) (json.RawMessage, string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// planMarker marks a plan revision's comment on the plan's issue.
func planMarker(taskID string, revision int) string {
	return fmt.Sprintf("<!-- factoryd plan %s revision %d -->", taskID, revision)
}

// cardMarker marks a plan card's issue.
func cardMarker(taskID, key string) string {
	return fmt.Sprintf("<!-- factoryd plan %s card %s -->", taskID, key)
}

// PlanRow is one card of a plan as the console's plan card shows it.
type PlanRow struct {
	Key, Role, Title, Size string
	DependsOn              []string
}

// PlanRows are p's cards in filing order, each with its dependencies, the
// edges factoryd adds included, as the plan comment lists them.
func PlanRows(p Plan) []PlanRow {
	edges := planEdges(p)
	order := planOrder(p, edges)
	rows := make([]PlanRow, len(order))
	for i, c := range order {
		rows[i] = PlanRow{Key: c.Key, Role: c.Role, Title: c.Title, Size: c.Size, DependsOn: dependsOn(order, edges, c.Key)}
	}
	return rows
}

// cell is s as one Markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", `\|`)
}

// planComment is revision's comment on plan task t's issue before its
// marker: the summary and one row per card in filing order, with its issue
// once filed (issues by key) and, when t's spec has a Deploy line, the
// deploy owner's mark, and each brief in a details block.
func planComment(t Task, revision int, p Plan, issues map[string]int64) string {
	edges := planEdges(p)
	needed, owner := deployOwner(t.Objective, p)
	var b strings.Builder
	fmt.Fprintf(&b, "factoryd: plan revision %d of task %s, written by the planner. An approver accepts it on the factory console; "+
		"a comment here asks the planner for a new revision; closing the issue ends the plan.\n\n%s\n\n", revision, t.ID, strings.TrimSpace(p.Summary))
	head, rule := "| # | key | role | title | size | depends on | issue |", "|---|---|---|---|---|---|---|"
	if needed {
		head, rule = head+" deploy |", rule+"---|"
	}
	b.WriteString(head + "\n" + rule + "\n")
	order := planOrder(p, edges)
	for i, c := range order {
		issue := ""
		if n := issues[c.Key]; n != 0 {
			issue = fmt.Sprintf("#%d", n)
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |", i+1, cell(c.Key), cell(c.Role), cell(c.Title), cell(c.Size),
			cell(strings.Join(dependsOn(order, edges, c.Key), ", ")), issue)
		switch {
		case needed && c.Key == owner:
			b.WriteString(" deploy |")
		case needed:
			b.WriteString("  |")
		}
		b.WriteString("\n")
	}
	if len(p.Questions) > 0 {
		b.WriteString("\nQuestions:\n")
		for i, q := range p.Questions {
			b.WriteString(QuestionLine(i+1, q) + "\n")
		}
	}
	for _, c := range order {
		fmt.Fprintf(&b, "\n<details><summary>%s: %s</summary>\n\n%s\n\n%s</details>\n", cell(c.Key), cell(c.Title), strings.TrimSpace(c.Brief), cardFacts(c))
	}
	return b.String()
}

// QuestionLine is question n of a plan as its comment and the project
// agent list it.
func QuestionLine(n int, q PlanQuestion) string {
	return fmt.Sprintf("%d. %s (choices: %s)", n, q.Text, strings.Join(q.Choices, " / "))
}

// cardDirectories keys a plan card's directories line.
const cardDirectories = "Directories (guidance, not a boundary)"

// cardFacts are a card's directories, acceptance tests and size as its
// issue and the plan comment list them.
func cardFacts(c PlanCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", cardDirectories, cmp.Or(strings.Join(c.Directories, ", "), "none"))
	b.WriteString("Acceptance:\n")
	for _, a := range c.Acceptance {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	fmt.Fprintf(&b, "Size: %s\n", c.Size)
	if c.Rung != "" {
		fmt.Fprintf(&b, "Rung: %s, %s\n", c.Rung, cmp.Or(strings.TrimSpace(c.RungReason), "the planner's choice"))
	}
	return b.String()
}

// cardBody is card c's issue body before its marker: card i of n of of
// (plan #n), accepted by approver, with the issues of the cards it
// depends on (deps, their keys by issue).
func cardBody(c PlanCard, of string, i, n int, approver string, deps []Dependency) string {
	on := make([]string, 0, len(deps))
	for _, d := range deps {
		on = append(on, fmt.Sprintf("#%d (%s)", d.Issue, d.Key))
	}
	directories, facts, _ := strings.Cut(cardFacts(c), "\n")
	return fmt.Sprintf("Card %s of %s (%d of %d), accepted by %s. Written by the planner.\n\n%s\n\n%s\nDepends on: %s\n%s",
		c.Key, of, i, n, approver, strings.TrimSpace(c.Brief), directories, cmp.Or(strings.Join(on, ", "), "none"), strings.TrimSuffix(facts, "\n"))
}

// planGuide is the planner's guide: the spec is the issue, the answer a
// plan filed with the file_plan tool, a card one session of the pinned
// limits. The shape's mechanical rules (counts, keys, bounds, cycles) are
// ValidatePlan's findings, not this prose (roadmap 186).
func planGuide(shape policy.PlanShape, limits policy.Limits) string {
	g := "You are the planner. " +
		"The issue's text is the spec of an application to build in this repository. Read it, and the repository, its AGENTS.md first: " +
		"every card follows its conventions. Split the work into the cards of a plan. " +
		"A card is one pull request by one agent: one package, one page, or one set of endpoints, with its tests, about ten files at most; a larger piece of work is two cards. " +
		"You file the whole result with the file_plan tool: its plan argument is the complete plan json, one object holding " +
		`{"summary": "a paragraph: what gets built and in which order", "cards": [{"key": "...", "role": "...", "title": "...", "brief": "...", ` +
		`"directories": ["..."], "depends_on": ["..."], "acceptance": ["..."], "size": "small|medium|large", "rung": "cheap|frontier", "rung_reason": "one line"}], "questions": [{"text": "...", "choices": ["...", "..."]}]}. ` +
		"role: foundation, feature or integration. rung: the model rung the card starts on: cheap for routine, well-specified work, frontier for a large change or one in factoryd's own package; leave it out to derive it from the size. "
	if shape.Foundation {
		g += "The foundation card holds the layout, the API contract, the schema and one end-to-end test, which every other card builds on. " +
			"The foundation's brief names the contracts every card builds on: how a handler gets the logged-in user (the function and its package), " +
			"the store (its interface, its implementation's package, the methods stubbed), and the server constructor tests import (internal/server.New unless the template says otherwise). " +
			"Every card adds its routes in its own internal/server/routes_<key>.go, so no two cards edit one route list. "
	}
	if shape.Integration {
		g += "The integration card turns the spec's acceptance tests into tests under acceptance/ and makes them pass, after every other card. "
	}
	if shape.Foundation || shape.Integration {
		g += "factoryd adds the foundation and integration edges itself: " +
			"list in depends_on only the dependencies among feature cards. "
	} else {
		g += "depends_on lists the keys of the cards a card needs merged first. "
	}
	return g + "title and brief: markdown, what to build and how it is checked. " +
		"directories: paths relative to the repository's root, guidance for the card's agent; " +
		"acceptance: each item a test by name: TestXxx in <package>, its inputs and the numbers it expects. " +
		"size: small, medium or large; anything bigger is two cards. " +
		"questions: open points the spec leaves to the member; their answers come back to you with your last plan, " +
		"and a plan with open questions cannot be accepted until they are answered. " +
		"Every card's agent and CI run bash check; do not ask a card to verify with a tool the check does not run. " +
		"A writer reads your plan on the issue; a comment asking for changes comes back to you as a finding, with your last plan: " +
		"answer the whole plan again."
}

// sizeLines are the changed lines a card of each size plans at most,
// derived from the risk bound (roadmap 159, 184): large is the bound
// itself, medium 60 percent of it, small 30 percent, rounded to whole
// lines. Without a bound (0) the figures are 300/600/1000.
func sizeLines(bound int) map[string]int {
	if bound <= 0 {
		bound = 1000
	}
	return map[string]int{"small": int(math.Round(0.3 * float64(bound))), "medium": int(math.Round(0.6 * float64(bound))), "large": bound}
}

// PlanAcceptedComment is the console's comment, as the approver, on the
// plan's issue when they accept revision: the plan's wait does not read it
// as a change request.
func PlanAcceptedComment(revision int) string {
	return fmt.Sprintf("Plan revision %d accepted.", revision)
}

// PlanAnswerComment is the console's comment, as the member, on the plan's
// issue when they answer question idx: intake knows it for no change
// request by the answer stored before it.
func PlanAnswerComment(idx int, answer string) string {
	return fmt.Sprintf("Answer %d: %s", idx, answer)
}

// ErrStalePlan refuses an acceptance of a revision that is no longer the
// newest; its message is the card's.
type ErrStalePlan struct{ Newest int }

func (e ErrStalePlan) Error() string {
	return fmt.Sprintf("the plan changed to revision %d; reload", e.Newest)
}

// ErrOpenQuestions refuses an acceptance of a revision whose questions
// are not all answered; its message is the card's.
type ErrOpenQuestions struct{ Issue, Revision, Open int }

func (e ErrOpenQuestions) Error() string {
	return fmt.Sprintf("plan #%d revision %d has %d unanswered questions; answer them first", e.Issue, e.Revision, e.Open)
}

// openQuestions refuses revision of plan task t while any of its
// questions is open.
func openQuestions(ctx context.Context, store Store, t Task, revision int) error {
	n, err := store.OpenPlanQuestions(ctx, t.ID, revision)
	if err == nil && n > 0 {
		err = ErrOpenQuestions{Issue: int(t.IssueNumber), Revision: revision, Open: n}
	}
	return err
}

// ErrAnswerUnsignalled is a plan's answer stored whose workflow the
// console could not signal.
var ErrAnswerUnsignalled = errors.New("the answer is recorded, but the plan's workflow was not signalled")

// AnswerPlan is the console's answer by a member to question idx of plan
// revision revision, which must be the newest (ErrStalePlan): the answer
// is stored (ErrAnswered for a second one), then the plan task's workflow
// signalled, which sends the planner back once none is open. The last
// answer resolves the plan's approval as superseded: Accept cannot file
// the revision the planner revises with the answers.
func AnswerPlan(ctx context.Context, store Store, wf Workflows, taskID string, revision, idx int, answer, by string) error {
	if _, err := pendingPlan(ctx, store, taskID); err != nil {
		return err
	}
	newest, ok, err := store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return err
	}
	if !ok || newest.Revision != revision {
		return ErrStalePlan{Newest: newest.Revision}
	}
	if err := store.AnswerPlanQuestion(ctx, taskID, revision, idx, answer, by); err != nil {
		return err
	}
	if err := wf.Signal(ctx, taskID, SignalPlanAnswered, TaskSignal{Author: by, Revision: revision, Index: idx}); err != nil {
		return fmt.Errorf("%w: %v", ErrAnswerUnsignalled, err)
	}
	if open, err := store.OpenPlanQuestions(ctx, taskID, revision); err != nil || open > 0 {
		return err
	}
	return store.ResolveApprovals(ctx, taskID, ApprovalPlan, "denied", FactoryDecider, fmt.Sprintf("superseded: the planner revises revision %d with the answers", revision))
}

// AnswerPlanQuestions is the console's batch answer by a member to every
// question of plan revision revision it names at once, which must be the
// newest (ErrStalePlan): all answers are stored in one statement (any bad
// entry — ErrAnswered for an answered question, ErrNotFound for an unknown
// one — stores none), then the plan task's workflow signalled once per
// answer, which sends the planner back once none is open. When no question
// of the revision stays open the plan's approval resolves as superseded:
// Accept cannot file the revision the planner revises with the answers.
func AnswerPlanQuestions(ctx context.Context, store Store, wf Workflows, taskID string, revision int, answers []PlanAnswer, by string) error {
	if _, err := pendingPlan(ctx, store, taskID); err != nil {
		return err
	}
	newest, ok, err := store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return err
	}
	if !ok || newest.Revision != revision {
		return ErrStalePlan{Newest: newest.Revision}
	}
	if err := store.AnswerPlanQuestions(ctx, taskID, revision, answers, by); err != nil {
		return err
	}
	for _, a := range answers {
		if err := wf.Signal(ctx, taskID, SignalPlanAnswered, TaskSignal{Author: by, Revision: revision, Index: a.Idx}); err != nil {
			return fmt.Errorf("%w: %v", ErrAnswerUnsignalled, err)
		}
	}
	if open, err := store.OpenPlanQuestions(ctx, taskID, revision); err != nil || open > 0 {
		return err
	}
	return store.ResolveApprovals(ctx, taskID, ApprovalPlan, "denied", FactoryDecider, fmt.Sprintf("superseded: the planner revises revision %d with the answers", revision))
}

// AcceptPlan is the console's Accept plan, by an approver: it signals the
// plan task's workflow when revision (at sha) is the newest and awaits
// acceptance; a newer revision refuses it with ErrStalePlan, an open
// question with ErrOpenQuestions. A plan
// without the deploy owner its spec needs is signalled all the same, so
// CreateChildren refuses it back to the planner, and answers
// ErrNoDeployOwner.
func AcceptPlan(ctx context.Context, store Store, wf Workflows, taskID string, revision int, sha, by string) error {
	q, err := pendingPlan(ctx, store, taskID)
	if err != nil {
		return err
	}
	newest, ok, err := store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return err
	}
	if !ok || newest.Revision != revision || newest.SHA256 != sha {
		return ErrStalePlan{Newest: newest.Revision}
	}
	t, _, err := store.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if err := openQuestions(ctx, store, t, revision); err != nil {
		return err
	}
	var p Plan
	if err := json.Unmarshal(newest.Plan, &p); err != nil {
		return err
	}
	sig := TaskSignal{Author: by, Revision: revision, SHA256: sha}
	if needed, owner := deployOwner(t.Objective, p); needed && owner == "" {
		// CreateChildren refuses it to the planner and denies the request
		if err := wf.Signal(ctx, taskID, SignalPlanAccepted, sig); err != nil {
			return err
		}
		return ErrNoDeployOwner
	}
	return decideAndSignal(ctx, store, wf, q, "approved", by, fmt.Sprintf("revision %d accepted", revision), SignalPlanAccepted, sig)
}

// RejectPlan is the console's Reject, by a writer: the plan task ends
// failed with why.
func RejectPlan(ctx context.Context, store Store, wf Workflows, taskID, by, why string) error {
	q, err := pendingPlan(ctx, store, taskID)
	if err != nil {
		return err
	}
	return decideAndSignal(ctx, store, wf, q, "denied", by, "rejected: "+why, SignalPlanRejected, TaskSignal{Author: by, Body: why})
}

// pendingPlan is the task's pending approval request of kind plan.
func pendingPlan(ctx context.Context, store Store, taskID string) (ApprovalRequest, error) {
	pending, err := store.ApprovalRequests(ctx, "pending")
	if err != nil {
		return ApprovalRequest{}, err
	}
	for _, a := range pending {
		if a.Kind == ApprovalPlan && a.TaskID == taskID {
			return a, nil
		}
	}
	return ApprovalRequest{}, fmt.Errorf("task %s has no plan waiting for acceptance", taskID)
}

func (s *PGStore) AddPlanRevision(ctx context.Context, id string, plan json.RawMessage, sha string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `WITH newest AS (SELECT revision, sha256 FROM plan_revisions WHERE task_id = $1 ORDER BY revision DESC LIMIT 1),
		added AS (INSERT INTO plan_revisions (task_id, revision, plan, sha256)
			SELECT $1, COALESCE((SELECT revision FROM newest), 0) + 1, $2::jsonb, $3
			WHERE NOT EXISTS (SELECT 1 FROM newest WHERE sha256 = $3) RETURNING revision)
		SELECT revision FROM added UNION ALL SELECT revision FROM newest WHERE sha256 = $3`, id, []byte(plan), sha).Scan(&n)
	return n, err
}

func (s *PGStore) PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error) {
	var p PlanRevision
	var plan []byte
	err := s.db.QueryRowContext(ctx, `SELECT task_id, revision, plan, sha256, created_at FROM plan_revisions
		WHERE task_id = $1 AND ($2 = 0 OR revision = $2) ORDER BY revision DESC LIMIT 1`, id, n).Scan(&p.TaskID, &p.Revision, &plan, &p.SHA256, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanRevision{}, false, nil
	}
	p.Plan = plan
	return p, err == nil, err
}

// PlanAnswer is one member's answer to question Idx (from 1) of a plan
// revision, as the batch store method and AnswerPlanQuestions take them.
// The tags name the keys the store's one statement reads back.
type PlanAnswer struct {
	Idx  int    `json:"idx"`
	Text string `json:"text"`
}

// ErrAnswered refuses a second answer to a plan's question.
var ErrAnswered = errors.New("the question is answered already")

func (s *PGStore) PutPlanQuestions(ctx context.Context, id string, revision int, qs []PlanQuestion) (bool, error) {
	b, err := json.Marshal(qs)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO plan_questions (task_id, revision, idx, text, choices)
		SELECT $1, $2, i, q->>'text', q->'choices' FROM jsonb_array_elements($3::jsonb) WITH ORDINALITY AS a(q, i) ON CONFLICT DO NOTHING`, id, revision, b)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *PGStore) PlanQuestions(ctx context.Context, id string, revision int) ([]PlanQuestion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text, choices, COALESCE(answer, ''), COALESCE(answered_by, ''), answered_at FROM plan_questions
		WHERE task_id = $1 AND revision = $2 ORDER BY idx`, id, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanQuestion
	for rows.Next() {
		var q PlanQuestion
		var choices []byte
		var at sql.NullTime
		if err := rows.Scan(&q.Text, &choices, &q.Answer, &q.AnsweredBy, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(choices, &q.Choices); err != nil {
			return nil, err
		}
		q.AnsweredAt = at.Time
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *PGStore) AnswerPlanQuestion(ctx context.Context, id string, revision, idx int, answer, by string) error {
	var answered, asked bool
	err := s.db.QueryRowContext(ctx, `WITH done AS (UPDATE plan_questions SET answer = $4, answered_by = $5, answered_at = now()
			WHERE task_id = $1 AND revision = $2 AND idx = $3 AND answer IS NULL RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM done), EXISTS (SELECT 1 FROM plan_questions WHERE task_id = $1 AND revision = $2 AND idx = $3)`,
		id, revision, idx, answer, by).Scan(&answered, &asked)
	switch {
	case err != nil || answered:
		return err
	case asked:
		return ErrAnswered
	}
	return ErrNotFound
}

// AnswerPlanQuestions stores every answer in one statement: any bad entry
// — an idx with an answer (ErrAnswered) or without a question
// (ErrNotFound) — refuses the whole batch, so none is stored. Duplicate
// entries count once.
func (s *PGStore) AnswerPlanQuestions(ctx context.Context, id string, revision int, answers []PlanAnswer, by string) error {
	if len(answers) == 0 {
		return nil
	}
	b, err := json.Marshal(answers)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	var missing, answered, open int
	if err := tx.QueryRowContext(ctx, `WITH want (idx) AS (SELECT DISTINCT (e->>'idx')::int FROM jsonb_array_elements($3::jsonb) AS a(e)),
 		have AS (SELECT w.idx, q.answer IS NOT NULL AS done FROM want w
 			JOIN plan_questions q ON q.task_id = $1 AND q.revision = $2 AND q.idx = w.idx),
 		miss AS (SELECT count(*) AS n FROM want w WHERE NOT EXISTS (SELECT 1 FROM have h WHERE h.idx = w.idx)),
 		got AS (SELECT count(*) AS n FROM have h WHERE h.done),
 		opn AS (SELECT count(*) AS n FROM have h WHERE NOT h.done)
 		SELECT (SELECT n FROM miss), (SELECT n FROM got), (SELECT n FROM opn)`,
		id, revision, string(b)).Scan(&missing, &answered, &open); err != nil {
		return err
	}
	switch {
	case missing > 0 && open == 0:
		return ErrNotFound
	case answered > 0:
		return ErrAnswered
	case missing > 0:
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plan_questions q SET answer = w.text, answered_by = $3, answered_at = now()
 		FROM (SELECT (e->>'idx')::int AS idx, e->>'text' AS text FROM jsonb_array_elements($4::jsonb) AS a(e)) AS w
 		WHERE q.task_id = $1 AND q.revision = $2 AND q.idx = w.idx`, id, revision, by, string(b)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PGStore) OpenPlanQuestions(ctx context.Context, id string, revision int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM plan_questions WHERE task_id = $1 AND revision = $2 AND answer IS NULL`, id, revision).Scan(&n)
	return n, err
}

func (s *PGStore) CreatePlanCard(ctx context.Context, t Task) (Task, error) {
	if err := s.insertTask(ctx, t, `ON CONFLICT (parent_task, plan_key) WHERE source = 'plan' DO NOTHING`); err != nil {
		return Task{}, err
	}
	got, ok, err := s.oneTask(ctx, `parent_task = $1 AND source = $2 AND plan_key = $3`, t.Parent, SourcePlan, t.PlanKey)
	if err == nil && !ok {
		err = fmt.Errorf("card %s of plan %s not found after its insert", t.PlanKey, t.Parent)
	}
	return got, err
}

func (s *PGStore) SetTaskIssue(ctx context.Context, id string, issue int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET issue_number = $2 WHERE id = $1`, id, issue)
	return oneRow(res, err, "task "+id)
}

// planLoop is the program of a task of session mode plan: planner sessions
// until one answers a valid plan, then the approvers' wait. A writer's
// comment sends the planner back with the last plan; Accept plan files the
// cards (a refusal sends the planner back); Reject ends the task failed, a
// closed issue closed. Every session counts against the session cap, where
// the card waits for a human and a writer's comment buys one more.
func (t *taskRun) planLoop() (string, error) {
	if err := workflow.ExecuteActivity(t.short, ActivityEnsureIssue, t.taskID).Get(t.ctx, nil); err != nil {
		return t.finish("failed", "ensure issue: "+errMessage(err))
	}
	var st RunState
	if err := workflow.ExecuteActivity(t.short, ActivityReconcile, t.taskID).Get(t.ctx, &st); err != nil {
		return t.finish("failed", "reconcile: "+errMessage(err))
	}
	switch {
	case st.Skip:
		return st.Status, nil
	case st.IssueClosed:
		return t.finish("closed", ReasonIssueClosed+" before the plan was accepted")
	}
	u := st.Usage
	var ref PlanRef
	healed := st.Status == "waiting-human"
	if healed {
		if err := workflow.ExecuteActivity(t.short, ActivityRequestPlan, t.taskID, 0).Get(t.ctx, &ref); err != nil {
			return t.finish("failed", "request plan: "+errMessage(err))
		}
		t.escalation = ref.Comment
	}
	wait := ref.Revision != 0
	var findings []string
	var ask string // a question for a human: the cap card, or a stop's
	// carried are the findings of a session that could not start: the
	// next one starts with them and what the wait brings
	var carried []string
	reason := "no session answered a plan"
	refusals := 0 // refused sessions in a row (versionSessionRefusals)
	// the comment the running or next session answers: writers' comments
	// newer than it ask for changes
	floor := t.escalation
	for {
		if !wait && ask == "" && u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts {
			ask = t.sessionCapCard(reason, findings)
			if healed {
				ask = healedReason
			}
		}
		if ask != "" {
			if err := workflow.ExecuteActivity(t.short, ActivityEscalate, t.taskID, ask, []string(nil)).Get(t.ctx, &t.escalation); err != nil {
				return t.finish("failed", escalateFailed+errMessage(err))
			}
			ask, wait = "", true
		}
		healed = false
		if wait {
			final, why, next := t.awaitPlan(&ref, floor)
			if final != "" {
				return t.finish(final, why)
			}
			findings, wait, carried = append(carried, next...), false, nil
			if u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts {
				t.grant(&u)
			}
			continue
		}
		remaining := t.cfg.WallClockCap + u.ExtraTime - u.AgentTime
		if remaining <= 0 {
			return t.finish("failed", "wall-clock cap reached")
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set running: "+errMessage(err))
		}
		floor = t.escalation
		if v, decided := t.awaitRollHold(); decided {
			final, why, _ := closedBeforeStart(v.final, v.reason)
			return t.finish(final, why)
		}
		began := workflow.Now(t.ctx)
		var res PlanResult
		v, decided, err := t.race(t.sessionOptions(remaining), &res, ActivityRunPlan, t.taskID, t.cfg.Models.Stronger, t.cfg.PolicyVersion, findings)
		if res.Refused.Status != 0 && t.sessionRefusals < versionSessionRefusals {
			res.Findings = []string{"plan: the session ended without a last message: " + res.Refused.Reason}
		} else if err == nil && !decided && res.Refused.Status != 0 {
			ask = t.providerRefused(&refusals, res.Refused)
			if final, why, ok := t.decided(); ok {
				final, why, _ = closedBeforeStart(final, why)
				return t.finish(final, why)
			}
			if final, why, ok := t.planRejected(); ok {
				return t.finish(final, why)
			}
			continue
		}
		if !res.StartFailed {
			t.providerAnswered(&refusals, err)
			u.AgentTime += workflow.Now(t.ctx).Sub(began)
			u.Attempts++
		}
		t.recordUsage(u)
		if final, why, ok := t.planRejected(); ok {
			return t.finish(final, why)
		}
		switch {
		case decided && v.final != "":
			final, why, _ := closedBeforeStart(v.final, v.reason)
			return t.finish(final, why)
		case decided:
			reason, findings, ask = "the gate supervisor stopped the planner session", nil, v.escalate
			continue
		case isTerminal(err):
			return t.finish("failed", "planner session: "+errMessage(err))
		case err != nil:
			reason = "planner session failed: " + errMessage(err)
			final, why, comments := t.planCommentsWhole(floor)
			if final != "" {
				return t.finish(final, why)
			}
			findings = append([]string{"the last planner session failed: " + errMessage(err)}, comments...)
			continue
		case res.StartFailed:
			reason, ask, carried = "the planner session could not start", res.Escalate, findings
			continue
		case res.Escalate != "":
			reason, findings, ask = "the planner asked a human", nil, res.Escalate
			continue
		case len(res.Findings) > 0:
			final, why, comments := t.planCommentsWhole(floor)
			if final != "" {
				return t.finish(final, why)
			}
			reason, findings = "the planner filed no plan", append(res.Findings, comments...)
			continue
		}
		if err := workflow.ExecuteActivity(t.short, ActivityRequestPlan, t.taskID, res.Revision).Get(t.ctx, &ref); err != nil {
			return t.finish("failed", "request plan: "+errMessage(err))
		}
		t.escalation, findings, wait = ref.Comment, nil, true
		t.approvalRequested = workflow.Now(t.ctx)
		reason = fmt.Sprintf("plan revision %d waits for an approver", ref.Revision)
	}
}

// planRejected takes a Reject sent while a session ran: the task ends
// before the next session spends.
func (t *taskRun) planRejected() (final, why string, ok bool) {
	var sig TaskSignal
	if workflow.GetSignalChannel(t.ctx, SignalPlanRejected).ReceiveAsync(&sig) {
		t.recordPlanSignal(SignalPlanRejected, sig, "")
		return "failed", fmt.Sprintf("plan rejected by %s: %s", sig.Author, sig.Body), true
	}
	return "", "", false
}

// planComments takes the writers' comments signalled so far: each newer
// than floor, the comment the session answered, asks for changes; the
// console's acceptance comment asks for nothing. heard are the comments
// that ask.
func (t *taskRun) planComments(floor int64) (out []string, heard []int64) {
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalHumanComment).ReceiveAsync(&sig) {
		detail := map[string]any{"signal": SignalHumanComment, "author": sig.Author, "comment": sig.Comment}
		switch {
		case sig.Comment <= floor:
			detail["ignored"] = "older than the plan or question the planner answered"
		case acceptedComment.MatchString(strings.TrimSpace(sig.Body)):
			detail["ignored"] = "the console's acceptance comment"
		default:
			out = append(out, "writer "+sig.Author+" asked for changes to the plan: "+sig.Body)
			heard = append(heard, sig.Comment)
		}
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
		}
	}
	return out, heard
}

// planCommentsWhole is planComments with the rest of the burst a comment
// opens (versionPlanBurst): a writer's change request in parts reaches the
// next planner session whole. final is the task's end meanwhile.
func (t *taskRun) planCommentsWhole(floor int64) (final, why string, out []string) {
	out, heard := t.planComments(floor)
	if len(heard) == 0 || t.planBurst < versionPlanBurst {
		return "", "", out
	}
	t.awaitBurst([]string{SignalIssueClosed, SignalHumanComment, SignalPlanRejected}, func() ([]int64, bool) {
		var ended bool
		if final, why, ended = t.planEnded(); ended {
			return nil, true
		}
		more, heard := t.planComments(floor)
		out = append(out, more...)
		return heard, false
	}, heard)
	return final, why, out
}

// planEnded takes a close of the issue or a Reject signalled so far: the
// task's end before a plan was accepted.
func (t *taskRun) planEnded() (final, why string, ok bool) {
	var sig TaskSignal
	if workflow.GetSignalChannel(t.ctx, SignalIssueClosed).ReceiveAsync(&sig) {
		t.counted(SignalIssueClosed, sig)
		return "closed", ReasonIssueClosed + " before the plan was accepted", true
	}
	return t.planRejected()
}

// awaitPlan waits, at no cost, for the approvers on revision ref (none
// yet: only a writer's comment or an end): the task's end, or the
// findings the next planner session starts with. An accepted revision
// files its cards and ends the task done; a refused acceptance, a
// writer's comment and the rest of its burst (versionPlanBurst), or the
// answer that leaves no question of the revision open
// (versionPlanAnswers) goes back to the planner.
func (t *taskRun) awaitPlan(ref *PlanRef, floor int64) (final, why string, findings []string) {
	wake := []string{SignalIssueClosed, SignalHumanComment, SignalPlanAccepted, SignalPlanRejected}
	if t.planAnswers >= versionPlanAnswers {
		wake = append(wake, SignalPlanAnswered)
	}
	answered := false
	pass := func() (heard []int64, stop bool) {
		if final, why, stop = t.planEnded(); stop {
			return nil, true
		}
		var sig TaskSignal
		for workflow.GetSignalChannel(t.ctx, SignalPlanAccepted).ReceiveAsync(&sig) {
			if ref.Revision == 0 {
				t.recordPlanSignal(SignalPlanAccepted, sig, "no plan waits for acceptance")
				continue
			}
			if sig.Revision != ref.Revision || sig.SHA256 != ref.SHA256 {
				t.recordPlanSignal(SignalPlanAccepted, sig, fmt.Sprintf("the plan changed to revision %d", ref.Revision))
				continue
			}
			t.recordPlanSignal(SignalPlanAccepted, sig, "")
			var res ChildrenResult
			if err := workflow.ExecuteActivity(t.short, ActivityCreateChildren, t.taskID, ref.Revision, sig.Author, t.cfg.PolicyVersion).Get(t.ctx, &res); err != nil {
				final, why = "failed", "create the plan's cards: "+errMessage(err)
				return nil, true
			}
			if len(res.Refused) > 0 {
				*ref = PlanRef{}
				findings = append(findings, res.Refused...)
				return nil, true
			}
			final, why = "done", fmt.Sprintf("plan revision %d accepted: %d cards filed", sig.Revision, len(res.Issues))
			return nil, true
		}
		ok, err := t.planAnswered(ref)
		if err != nil {
			final, why = "failed", "count the plan's open questions: "+errMessage(err)
			return nil, true
		}
		answered = answered || ok
		out, heard := t.planComments(floor)
		findings = append(findings, out...)
		return heard, false
	}
	for {
		sel := workflow.NewSelector(t.ctx)
		for _, n := range wake {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.Select(t.ctx)
		heard, stop := pass()
		if stop || len(heard) > 0 && t.planBurst >= versionPlanBurst && t.awaitBurst(wake, pass, heard) {
			if final != "" {
				return final, why, nil
			}
			return "", "", findings
		}
		if answered || len(findings) > 0 {
			return "", "", findings
		}
	}
}

// planAnswered takes the members' answers signalled so far: whether one
// answered a question of revision ref and none of its questions is open
// any more. An answer to another revision is recorded and ignored.
func (t *taskRun) planAnswered(ref *PlanRef) (bool, error) {
	if t.planAnswers < versionPlanAnswers {
		return false, nil
	}
	current := false
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalPlanAnswered).ReceiveAsync(&sig) {
		detail := map[string]any{"signal": SignalPlanAnswered, "author": sig.Author, "revision": sig.Revision, "index": sig.Index}
		if sig.Revision != ref.Revision {
			detail["ignored"] = fmt.Sprintf("the plan changed to revision %d", ref.Revision)
		}
		current = current || sig.Revision == ref.Revision
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
		}
	}
	if !current {
		return false, nil
	}
	var open int
	err := workflow.ExecuteActivity(t.short, ActivityOpenPlanQuestions, t.taskID, ref.Revision).Get(t.ctx, &open)
	return err == nil && open == 0, err
}

// recordPlanSignal records an approver's or writer's plan signal on the
// timeline, with why it does not count when it does not.
func (t *taskRun) recordPlanSignal(name string, sig TaskSignal, ignored string) {
	detail := map[string]any{"signal": name, "author": sig.Author, "revision": sig.Revision}
	switch {
	case ignored != "":
		detail["ignored"] = ignored
	case !t.approvalRequested.IsZero():
		// the approvers' wait ends: its step carries the wait's start
		detail["started"] = rfc3339(t.approvalRequested)
		t.approvalRequested = time.Time{}
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
}

// OpenPlanQuestions is how many of plan revision's questions have no
// answer.
func (r *Runner) OpenPlanQuestions(ctx context.Context, taskID string, revision int) (int, error) {
	return r.store.OpenPlanQuestions(ctx, taskID, revision)
}

// planShape is the plan lane's shape of the process task t follows in
// version v, and the process's name.
func planShape(v policy.Version, t Task) (policy.PlanShape, string, error) {
	name := t.Process
	if name == "" {
		_, project, _ := v.ProjectOf(t.Repository)
		name = project.Process
	}
	lane, ok := v.Processes[name].Lane(policy.LanePlan)
	if !ok || lane.Plan == nil {
		return policy.PlanShape{}, "", terminalError("task %s: process %q has no lane plan in configuration version %d", t.ID, name, v.ID)
	}
	return *lane.Plan, name, nil
}

// RunPlan is one planner session under the pinned version, with findings,
// the last plan and the members' answers to its questions. The session
// answers by the file_plan tool, which checks the whole plan json while it
// runs: RunPlan takes the filing the tool stored (FilePlan) and returns
// it; a session that ends without filing one says so, with its last
// message beside the finding.
func (r *Runner) RunPlan(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return PlanResult{}, err
	}
	shape, _, err := planShape(v, t)
	if err != nil {
		return PlanResult{}, err
	}
	last, ok, err := r.store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return PlanResult{}, err
	}
	var answers []string
	if ok {
		findings = append(slices.Clone(findings), fmt.Sprintf("your last plan, revision %d: answer the whole plan again, changed as the findings ask:\n```json\n%s\n```", last.Revision, last.Plan))
		qs, err := r.store.PlanQuestions(ctx, taskID, last.Revision)
		if err != nil {
			return PlanResult{}, err
		}
		for _, q := range qs {
			if q.Answer != "" {
				answers = append(answers, q.Text+": "+q.Answer)
			}
		}
	}
	r.takeFiled(taskID) // a filing an earlier session left is stale here
	began := time.Now()
	end, err := r.reportSession(ctx, t, repo, model, t.Standing(), pin, planGuide(shape, v.Document.Limits), findings, answers)
	if err != nil {
		return PlanResult{}, err
	}
	text, why := end.report, end.why
	if end.start != nil {
		reason := end.start.escalation(thenAnswer)
		r.record(ctx, taskID, "plan_escalated", map[string]any{"reason": reason})
		return PlanResult{Escalate: reason, StartFailed: true}, nil
	}
	if text == "" {
		if refused, err := r.providerRefusal(ctx, t.ID, began, why); err != nil || refused.Status != 0 {
			return PlanResult{Refused: refused}, err
		}
	}
	if end.escalate != "" {
		r.record(ctx, taskID, "plan_escalated", map[string]any{"reason": end.escalate})
		return PlanResult{Escalate: end.escalate}, nil
	}
	if res, ok := r.takeFiled(taskID); ok {
		return res, nil
	}
	found := []string{"plan: the session ended without filing a plan through the " + policy.ToolFilePlan + " tool"}
	if text == "" {
		found = append(found, "plan: the session ended without a last message: "+why)
	} else {
		found = append(found, "the planner's last message: "+textutil.Truncate(strings.TrimSpace(text), maxPlanEcho))
	}
	r.record(ctx, taskID, "plan_invalid", map[string]any{"findings": found})
	return PlanResult{Findings: found}, nil
}

// FilePlan is the file_plan tool's hand-over: the complete plan json of
// the planner session that runs, checked at once under the version in
// force. A refusal carries every finding back into the session; a valid
// plan is stored as the task's next revision and held as the session's
// answer, which RunPlan returns when the session ends.
func (r *Runner) FilePlan(ctx context.Context, taskID, plan string) (PlanResult, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	if t.Status != "running" {
		return PlanResult{Findings: []string{"plan: the task is " + t.Status + ", not running"}}, nil
	}
	if t.Lane != policy.LanePlan {
		return PlanResult{Findings: []string{"plan: the task is not in the plan lane"}}, nil
	}
	v := r.cfg.Policy()
	shape, _, err := planShape(v, t)
	if err != nil {
		return PlanResult{}, err
	}
	p, found := readPlan("```json\n"+plan+"\n```", shape, v.Document.Limits, v.Document.Risk.LargeLines)
	if len(found) > 0 {
		return PlanResult{Findings: found}, nil
	}
	raw, sha, err := planSHA(p)
	if err != nil {
		return PlanResult{}, err
	}
	n, err := r.store.AddPlanRevision(ctx, taskID, raw, sha)
	if err != nil {
		return PlanResult{}, err
	}
	r.record(ctx, taskID, "plan_revision", map[string]any{"revision": n, "cards": len(p.Cards), "sha256": sha})
	r.noteFiled(taskID, PlanResult{Revision: n, SHA256: sha})
	return PlanResult{Revision: n, SHA256: sha}, nil
}

// PlanFileHandler answers the gateway's file_plan tool: the running
// planner session's complete plan json, checked and stored as the
// session's answer. A plan the check refuses answers 200 with its
// findings — the refusal belongs in the session, not in a status.
func PlanFileHandler(runner *Runner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TaskID string `json:"task_id"`
			Plan   string `json:"plan"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		in.Plan = strings.TrimSpace(in.Plan)
		if in.TaskID == "" || in.Plan == "" {
			http.Error(w, "task_id and plan are required", http.StatusBadRequest)
			return
		}
		res, err := runner.FilePlan(r.Context(), in.TaskID, in.Plan)
		if err != nil {
			status := http.StatusInternalServerError
			if isTerminal(err) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Revision int      `json:"revision"`
			SHA256   string   `json:"sha256"`
			Findings []string `json:"findings"`
		}{res.Revision, res.SHA256, res.Findings}) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
	}
}

// RequestPlan puts revision (0: the newest, none: a zero PlanRef) before
// the approvers: its comment on the plan's issue, once; an approval
// request of kind plan, which supersedes the one of an earlier revision;
// the task waiting on a human.
func (r *Runner) RequestPlan(ctx context.Context, taskID string, revision int) (PlanRef, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanRef{}, err
	}
	rev, ok, err := r.store.PlanRevision(ctx, taskID, revision)
	switch {
	case err != nil:
		return PlanRef{}, err
	case !ok && revision == 0:
		return PlanRef{}, nil
	case !ok:
		return PlanRef{}, terminalError("task %s has no plan revision %d", taskID, revision)
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return PlanRef{}, terminalError("task %s: plan revision %d: %v", taskID, rev.Revision, err)
	}
	if err := r.askQuestions(ctx, t, rev.Revision, p.Questions); err != nil {
		return PlanRef{}, err
	}
	comment, err := r.postPlan(ctx, t, repo, rev.Revision, p, nil)
	if err != nil {
		return PlanRef{}, err
	}
	pending, err := r.store.ApprovalRequests(ctx, "pending")
	if err != nil {
		return PlanRef{}, err
	}
	want := PlanSpec{Revision: rev.Revision, SHA256: rev.SHA256}
	spec, err := json.Marshal(want)
	if err != nil {
		return PlanRef{}, err
	}
	if !slices.ContainsFunc(pending, func(a ApprovalRequest) bool {
		var have PlanSpec
		return a.Kind == ApprovalPlan && a.TaskID == taskID && json.Unmarshal(a.Spec, &have) == nil && have == want
	}) {
		id, err := r.store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: taskID, Project: t.Repository, Spec: spec,
			Reason: fmt.Sprintf("plan revision %d of issue #%d, %d cards, waits for an approver", rev.Revision, t.IssueNumber, len(p.Cards))})
		if err != nil {
			return PlanRef{}, err
		}
		r.record(ctx, taskID, "approval_requested", map[string]any{"approval": id, "revision": rev.Revision, "sha256": rev.SHA256})
		r.timeline.RecordRow(ctx, taskID, EventApprovalRequested, FactoryDecider, ViaFactoryd,
			map[string]any{"approval": id, "kind": ApprovalPlan, "revision": rev.Revision, "cards": len(p.Cards)})
	}
	if t.Status != "waiting-human" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"queued", "running"}, "waiting-human"); err != nil {
			return PlanRef{}, terminalError("task %s is %s, cannot wait for its approvers: %v", taskID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	}
	return PlanRef{Revision: rev.Revision, SHA256: rev.SHA256, Comment: comment}, nil
}

// askQuestions stores revision's questions of plan task t, sanitised as
// the plan's comment is (a clicked choice is posted as the member's
// comment), and records one question_asked row each when it stored them.
func (r *Runner) askQuestions(ctx context.Context, t Task, revision int, qs []PlanQuestion) error {
	if len(qs) == 0 {
		return nil
	}
	asked := make([]PlanQuestion, len(qs))
	for i, q := range qs {
		asked[i] = PlanQuestion{Text: r.untrusted(t, q.Text), Choices: make([]string, len(q.Choices))}
		for j, c := range q.Choices {
			asked[i].Choices[j] = r.untrusted(t, c)
		}
	}
	if inserted, err := r.store.PutPlanQuestions(ctx, t.ID, revision, asked); err != nil || !inserted {
		return err
	}
	for i, q := range asked {
		r.timeline.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd,
			map[string]any{"question": q.Text, "choices": q.Choices, "revision": revision, "index": i + 1})
	}
	return nil
}

// postPlan puts revision's comment on the plan's issue, sanitised, with
// the cards' issues once filed: it edits the comment its marker finds, or
// posts it.
func (r *Runner) postPlan(ctx context.Context, t Task, repo Repo, revision int, p Plan, issues map[string]int64) (int64, error) {
	marker := planMarker(t.ID, revision)
	body := r.untrusted(t, planComment(t, revision, p, issues)) + "\n" + marker
	have, err := r.forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil {
		return 0, err
	}
	for _, c := range have {
		if c.Author == r.forgejo.BotUser && strings.Contains(c.Body, marker) {
			if c.Body == body {
				return c.ID, nil
			}
			return c.ID, r.forgejo.EditIssueComment(ctx, repo.Owner, repo.Name, c.ID, body)
		}
	}
	return r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body)
}

// CreateChildren files the cards of the plan task's accepted revision,
// once however often it runs: it judges the revision again, the deploy
// owner its spec needs and its answered questions included (a finding
// refuses the acceptance: the
// approval ends, the planner gets it); files
// each card in topological order as a backlog task row and a bot issue
// found by its marker or filed; records the edges in Forgejo and
// task_deps; puts the issues into the plan's comment.
func (r *Runner) CreateChildren(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChildrenResult{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return ChildrenResult{}, err
	}
	shape, process, err := planShape(v, t)
	if err != nil {
		return ChildrenResult{}, err
	}
	rev, ok, err := r.store.PlanRevision(ctx, taskID, revision)
	if err != nil {
		return ChildrenResult{}, err
	}
	if !ok {
		return ChildrenResult{}, terminalError("task %s has no plan revision %d", taskID, revision)
	}
	p, findings := readPlan("```json\n"+string(rev.Plan)+"\n```", shape, v.Document.Limits, v.Document.Risk.LargeLines)
	if needed, owner := deployOwner(t.Objective, p); needed && owner == "" {
		findings = append(findings, ErrNoDeployOwner.Error())
	}
	var unanswered ErrOpenQuestions
	switch err := openQuestions(ctx, r.store, t, revision); {
	case errors.As(err, &unanswered):
		findings = append(findings, unanswered.Error())
	case err != nil:
		return ChildrenResult{}, err
	}
	if len(findings) > 0 {
		note := fmt.Sprintf("accepting revision %d was refused: %s", revision, strings.Join(findings, "; "))
		if err := r.store.ResolveApprovals(ctx, taskID, ApprovalPlan, "denied", FactoryDecider, note); err != nil {
			return ChildrenResult{}, err
		}
		r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: "+r.untrusted(t, note))
		r.record(ctx, taskID, "plan_refused", map[string]any{"revision": revision, "by": approver, "findings": findings})
		return ChildrenResult{Refused: findings}, nil
	}
	cards, err := r.fileChildren(ctx, t, repo, v, process, fmt.Sprintf("plan #%d", t.IssueNumber), p, approver)
	if err != nil {
		return ChildrenResult{}, err
	}
	byKey := map[string]int64{}
	var issues []int64
	for _, c := range cards {
		byKey[c.PlanKey] = c.IssueNumber
		issues = append(issues, c.IssueNumber)
	}
	if _, err := r.postPlan(ctx, t, repo, revision, p, byKey); err != nil {
		return ChildrenResult{}, err
	}
	if err := r.store.ResolveApprovals(ctx, taskID, ApprovalPlan, "approved", approver, fmt.Sprintf("revision %d accepted: %d cards filed", revision, len(issues))); err != nil {
		return ChildrenResult{}, err
	}
	r.record(ctx, taskID, "children_created", map[string]any{"revision": revision, "by": approver, "issues": issues})
	return ChildrenResult{Issues: issues}, nil
}

// fileChildren files p's cards of task t, its process's, the cards of
// of (plan #n), in topological order as backlog task rows and bot issues
// found by their markers or filed, and records the edges in Forgejo and
// task_deps; the cards in filing order.
func (r *Runner) fileChildren(ctx context.Context, t Task, repo Repo, v policy.Version, process, of string, p Plan, approver string) ([]Task, error) {
	edges := planEdges(p)
	order := planOrder(p, edges)
	_, project, _ := v.ProjectOf(t.Repository)
	cards := map[string]Task{}
	var open []webhookIssue
	for i, c := range order {
		var deps []Dependency
		for _, d := range dependsOn(order, edges, c.Key) {
			deps = append(deps, Dependency{TaskID: cards[d].ID, Issue: cards[d].IssueNumber, Key: d})
		}
		title := r.untrusted(t, c.Title)
		body := r.untrusted(t, cardBody(c, of, i+1, len(order), approver, deps))
		card, err := r.store.CreatePlanCard(ctx, Task{ID: NewTaskID(), Priority: t.Priority, RiskClass: "standard",
			BudgetEUR: policy.CardBudget(v.Document.Budget, project.Budget, c.Size, 0), Status: "backlog", Lane: policy.LaneBacklog, Process: process,
			Objective: title + "\n\n" + body, Repository: t.Repository, Kind: "agent", Source: SourcePlan, Parent: t.ID, PlanKey: c.Key,
			Rung: c.Rung, Size: c.Size, Author: approver, AuthorWriter: true})
		if err != nil {
			return nil, err
		}
		if card.IssueNumber == 0 {
			marker := cardMarker(t.ID, c.Key)
			if open == nil {
				if open, err = r.forgejo.OpenIssues(ctx, repo.Owner, repo.Name); err != nil {
					return nil, err
				}
			}
			i := slices.IndexFunc(open, func(is webhookIssue) bool {
				return is.Poster.UserName == r.forgejo.BotUser && strings.Contains(is.Body, marker)
			})
			if i >= 0 {
				card.IssueNumber = open[i].Index
			} else {
				ref, err := r.forgejo.CreateIssue(ctx, repo.Owner, repo.Name, title, body+"\n\n"+marker)
				if err != nil {
					return nil, err
				}
				card.IssueNumber = ref.Number
			}
			if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, card.IssueNumber, []string{LabelBacklog, "state/backlog"}); err != nil {
				return nil, err
			}
			if err := r.store.SetTaskIssue(ctx, card.ID, card.IssueNumber); err != nil {
				return nil, err
			}
			r.timeline.recordEvent(ctx, card.ID, EventCardCreated, map[string]any{"by": approver, "issue": card.IssueNumber, "plan": t.ID})
		}
		cards[c.Key] = card
	}
	for _, c := range order {
		if len(edges[c.Key]) == 0 {
			continue
		}
		child := cards[c.Key]
		have, err := r.forgejo.IssueDependencies(ctx, repo.Owner, repo.Name, child.IssueNumber)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, d := range dependsOn(order, edges, c.Key) {
			dep := cards[d]
			ids = append(ids, dep.ID)
			if !slices.Contains(have, dep.IssueNumber) {
				if err := r.forgejo.AddIssueDependency(ctx, repo, child.IssueNumber, dep.IssueNumber); err != nil {
					return nil, err
				}
			}
		}
		if err := r.store.AddTaskDeps(ctx, child.ID, ids); err != nil {
			return nil, err
		}
	}
	out := make([]Task, len(order))
	for i, c := range order {
		out[i] = cards[c.Key]
	}
	return out, nil
}

// EditIssueComment replaces comment id's body.
func (f *Forgejo) EditIssueComment(ctx context.Context, owner, repo string, id int64, body string) error {
	payload, err := json.Marshal(map[string]any{"body": body})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/%s/issues/comments/%d", owner, repo, id), payload, http.StatusOK)
	return err
}

// IssueDependencies are the numbers of the issues issue number depends on
// (Forgejo's blocking dependencies), all pages.
func (f *Forgejo) IssueDependencies(ctx context.Context, owner, repo string, number int64) ([]int64, error) {
	deps, err := GetPages[struct {
		Number int64 `json:"number"`
	}](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies?", owner, repo, number))
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.Number)
	}
	return out, nil
}

// AddIssueDependency makes issue number of repo depend on issue dependency
// of the same repository (swagger issueCreateIssueDependencies).
func (f *Forgejo) AddIssueDependency(ctx context.Context, repo Repo, number, dependency int64) error {
	payload, err := json.Marshal(map[string]any{"owner": repo.Owner, "repo": repo.Name, "index": dependency})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies", repo.Owner, repo.Name, number), payload, http.StatusCreated)
	return err
}

// SetIssueDependency makes issue number of repo depend on issue
// dependency, or with remove no longer, as the bot; an issue already as
// asked is left alone, so a retry or a change made in Forgejo fails
// nothing. The delete takes any 2xx (swagger issueRemoveIssueDependencies
// says 200, where the add answers 201).
func (f *Forgejo) SetIssueDependency(ctx context.Context, repo Repo, number, dependency int64, remove bool) error {
	have, err := f.IssueDependencies(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return err
	}
	has := slices.Contains(have, dependency)
	switch {
	case has && !remove, !has && remove:
		return nil
	case !remove:
		return f.AddIssueDependency(ctx, repo, number, dependency)
	}
	payload, err := json.Marshal(map[string]any{"owner": repo.Owner, "repo": repo.Name, "index": dependency})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies", repo.Owner, repo.Name, number), payload, anySuccess)
	return err
}

// planOf is plan card t's section "The spec and the plan" (its plan's
// issue text and the accepted plan's summary), its role and its plan
// row's directories, and the pull request of a split's card; "" and none
// for a task no plan filed.
func (r *Runner) planOf(ctx context.Context, t Task) (spec, role string, dirs []string, pull int64, err error) {
	if t.Source != SourcePlan || t.Parent == "" {
		return "", "", nil, 0, nil
	}
	plan, _, err := r.task(ctx, t.Parent)
	if err != nil {
		return "", "", nil, 0, err
	}
	rev, ok, err := r.store.PlanRevision(ctx, t.Parent, 0)
	if err != nil || !ok {
		return "", "", nil, 0, cmp.Or(err, error(terminalError("plan %s of card %s has no revision", t.Parent, t.ID)))
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return "", "", nil, 0, terminalError("plan %s revision %d: %v", t.Parent, rev.Revision, err)
	}
	role = RoleFeature
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == t.PlanKey }); i >= 0 {
		role, dirs = p.Cards[i].Role, p.Cards[i].Directories
	}
	spec = plan.Objective + "\n\nThe plan's summary: " + strings.TrimSpace(p.Summary)
	if plan.PRNumber != 0 {
		spec += fmt.Sprintf("\n\nThe plan splits pull request #%[1]d, whose head is in your clone as branch pull/%[1]d: carry over the files and hunks your brief names.", plan.PRNumber)
	}
	return spec, role, dirs, plan.PRNumber, nil
}
