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
	"log"
	"maps"
	"math"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The plan (stage11-design.md section 1.2, roadmap 225): a plan-track
// card's planner answers a plan; a revision with no open question files
// its cards on the direct track and the plan card ends done in the same
// step, and one with open questions waits for the writers' answers, which
// the planner revises it with.
const (
	// SignalPlanAnswered is the console's answer to a plan's question
	// (AnswerPlan): Author the member, Revision and Index the question.
	SignalPlanAnswered = "plan_answered"

	ActivityRunPlan     = "RunPlan"
	ActivityRequestPlan = "RequestPlan"
	// ActivityOpenPlanQuestions counts a revision's open questions.
	ActivityOpenPlanQuestions = "OpenPlanQuestions"
)

// The roles of a plan's cards.
const (
	RoleFoundation  = "foundation"
	RoleFeature     = "feature"
	RoleIntegration = "integration"
)

// The kinds of a plan's criteria (the spec-intake concept, piece 1): how
// a criterion is proven.
const (
	CriterionTest      = "test"
	CriterionInvariant = "invariant"
	CriterionJudgement = "judgement"
	CriterionHuman     = "human"
)

// The recording rule's first catch kinds (the spec-intake concept,
// roadmap 285): a question's finder is the planner. A self-attack
// question's catch kind is its origin (piece 5).
const (
	CatchQuestion   = "question"
	CatchSelfAttack = "self_attack"
	CatchCriterion  = "criterion"
	FinderPlanner   = "planner"
)

// The plan's caps are counts, never lengths: the transports that carry a
// plan (the file-plan body cap, maxReport) are its only ceiling.
const (
	maxCardDirs       = 10
	MaxCardAcceptance = 10
	maxQuestions      = 5
)

// Plan is the planner's answer: one fenced json block of its last message.
type Plan struct {
	Spec      *Spec           `json:"spec,omitempty"`
	Summary   string          `json:"summary"`
	Criteria  []PlanCriterion `json:"criteria,omitempty"`
	Cards     []PlanCard      `json:"cards"`
	Questions []PlanQuestion  `json:"questions,omitempty"`
}

// Spec is the spec the planning session drafts (the spec-intake
// concept, piece 5): the outcome in a few sentences and a glossary that
// names each component once, the members' confirmation question carries
// it and the criteria beside it.
type Spec struct {
	Outcome  string   `json:"outcome"`
	Glossary []string `json:"glossary,omitempty"`
}

// PlanCriterion is one criterion of the spec, a piece of its proof (the
// spec-intake concept, piece 1): kind test names its test, its inputs and
// the numbers it expects, a test bash check runs; invariant names a suite
// and carries nothing else; judgement names the claim a model judges, its
// fixed inputs and the pinned prompt, and no test; human names the claim
// only a person settles, with the evidence the approver will see. The
// kind names the fields it uses, the rest stay empty.
type PlanCriterion struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Test     string `json:"test,omitempty"`
	Inputs   string `json:"inputs,omitempty"`
	Expected string `json:"expected,omitempty"`
	Claim    string `json:"claim,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Replaces names the id this criterion revises (piece 10): a changed
	// criterion files under a new id that carries the old one here, so the
	// revision record holds the pair and no id ever changes meaning.
	Replaces string `json:"replaces,omitempty"`
}

// PlanItem is one acceptance item of a card: the test it names and the
// criterion it works on, cited by id in the item itself, so which cards
// work on a criterion is the sum of their items' citations and no second
// list can drift out of step (the spec-intake concept, piece 2). The
// criterion is empty where none is cited: a foundation card's contracts.
type PlanItem struct {
	Criterion string `json:"criterion,omitempty"`
	Text      string `json:"text"`
}

// PlanQuestion is an open point the spec leaves to the member, with the
// choices the planner offers; the store adds the member's answer, "" until
// it is given. Origin self_attack marks a question the planner's own
// attack on its draft found (piece 5); the catch ledger carries it as the
// self-attack's kind.
type PlanQuestion struct {
	Text    string   `json:"text"`
	Choices []string `json:"choices"`
	// Kind marks the confirmation question (kind confirmation), whose
	// answer fixes the spec and the criteria; every other question's kind
	// is question.
	Kind       string    `json:"kind,omitempty"`
	Origin     string    `json:"origin,omitempty"`
	Answer     string    `json:"-"`
	AnsweredBy string    `json:"-"`
	AnsweredAt time.Time `json:"-"`
}

// QuestionConfirmation is the confirmation question's kind.
const QuestionConfirmation = "confirmation"

// PlanCard is one card of a plan: one pull request, one session.
type PlanCard struct {
	Key         string     `json:"key"`
	Role        string     `json:"role"`
	Title       string     `json:"title"`
	Brief       string     `json:"brief"`
	Directories []string   `json:"directories"`
	DependsOn   []string   `json:"depends_on"`
	Acceptance  []PlanItem `json:"acceptance"`
	Size        string     `json:"size"`
	// Rung and RungReason: the model rung the card starts on and why,
	// cheap or frontier; "" lets intake derive it (roadmap 222, item 5)
	Rung       string `json:"rung,omitempty"`
	RungReason string `json:"rung_reason,omitempty"`
	// Image is the project's pinned image the card runs on; "" for the
	// project's default
	Image string `json:"image,omitempty"`
	// From is the old card whose work the card starts from: its key, or
	// #<issue> of a card the findings name without a key (the plan card's
	// own work after a direct card's cut or replan)
	From string `json:"from,omitempty"`
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
	// Capped: the gateway refused a model call of the session at a cap
	// (versionDayCap); Findings say it as a run from before reads them.
	Capped *CapRefusal `json:",omitempty"`
	// NotStarted: the session's pod never left Pending, and why
	// (versionPodWait); Findings say it as a run from before reads them.
	NotStarted string `json:",omitempty"`
}

// maxPlanEcho bounds the last message a planner answered without a plan,
// as its next session's finding.
const maxPlanEcho = 2000

// PlanRef is the plan revision a plan task waits on, and its comment on
// the issue: only later comments ask for changes. Zero: no plan yet.
// Filed are the issues of the cards a revision with no open question filed:
// the plan card ends done. Refused is why such a revision filed none, the
// next planner session's findings. Answered: every question of the
// revision has its answer, which the planner revises it with.
type PlanRef struct {
	Revision int
	SHA256   string
	Comment  int64
	Filed    []int64  `json:",omitempty"`
	Refused  []string `json:",omitempty"`
	Answered bool     `json:",omitempty"`
}

// ChildrenResult is the cards a plan's revision filed, or why it filed
// none.
type ChildrenResult struct {
	Issues  []int64
	Refused []string
}

var (
	planBlock = regexp.MustCompile("(?s)```json[ \t]*\r?\n(.*?)\r?\n[ \t]*```")
	cardKey   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	// criterionID is a criterion id's shape: the concept's examples are
	// snake_case (schedule_test), where card keys use dashes.
	criterionID    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	roles          = []string{RoleFoundation, RoleFeature, RoleIntegration}
	criterionKinds = []string{CriterionTest, CriterionInvariant, CriterionJudgement, CriterionHuman}
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

// readPlan decodes the planner's last message and judges it against the
// process's plan shape, the pinned limits and the project's image pins
// (nil: none): the plan, and one line per finding, none for a valid plan.
func readPlan(text string, shape policy.PlanShape, limits policy.Limits, pins map[string]policy.ImagePin) (Plan, []string) {
	blocks := planBlock.FindAllStringSubmatch(text, -1)
	if len(blocks) == 0 {
		return Plan{}, []string{"plan: no fenced json block in the last message"}
	}
	block := blocks[len(blocks)-1][1]
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
	if n := len(p.Cards); n < shape.Minimum() || n > shape.MaxCards {
		add("%d cards; this process allows %d to %d", n, shape.Minimum(), shape.MaxCards)
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
	if shape.Foundation && len(p.Cards) > 1 && len(foundation) != 1 {
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
		out = append(out, cardFindings(c, limits)...)
		if _, ok := pins[c.Image]; c.Image != "" && c.Image != "default" && !ok {
			add("card %s: image %q is not one of this project's pinned images (%s)", c.Key, c.Image,
				cmp.Or(strings.Join(slices.Sorted(maps.Keys(pins)), ", "), "none"))
		}
	}
	for i := range p.Cards {
		p.Cards[i].Image = cardImage(p.Cards[i].Image)
	}
	out = append(out, specFindings(p, shape)...)
	out = append(out, criteriaFindings(p, shape)...)
	return p, append(out, questionFindings(p.Questions)...)
}

// questionFindings are the findings on a plan's questions: 0 to 5, each
// with a text, 2 to 5 distinct choices of 1 to 60 and an origin of
// self_attack or none.
func questionFindings(qs []PlanQuestion) []string {
	var out []string
	if len(qs) > maxQuestions {
		out = append(out, fmt.Sprintf("plan: %d questions; want 0 to %d", len(qs), maxQuestions))
	}
	for i, q := range qs {
		add := func(format string, args ...any) {
			out = append(out, fmt.Sprintf("plan: question %d: "+format, append([]any{i + 1}, args...)...))
		}
		if q.Origin != "" && q.Origin != CatchSelfAttack {
			add("origin %q, want self_attack or none", q.Origin)
		}
		if utf8.RuneCountInString(q.Text) == 0 {
			add("text empty")
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
// check's alike: one of the card sizes.
func sizeFindings(size string) []string {
	var out []string
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
// plan card's and the ready check's alike, on the item lines as cardFacts
// writes them and the body carries them back.
func acceptanceFindings(acceptance []string) []string {
	var out []string
	if n := len(acceptance); n < 1 || n > MaxCardAcceptance {
		out = append(out, fmt.Sprintf("%d acceptance tests; want 1 to %d", n, MaxCardAcceptance))
	}
	for i, a := range acceptance {
		if utf8.RuneCountInString(a) == 0 {
			out = append(out, fmt.Sprintf("acceptance test %d is empty", i+1))
		}
	}
	return out
}

// itemText is acceptance item it as cardFacts writes its line and the
// ready check reads it back: the text alone, the criterion's id behind it
// in brackets when the item cites one.
func itemText(it PlanItem) string {
	if it.Criterion == "" || it.Text == "" {
		return it.Text
	}
	return it.Text + " [" + it.Criterion + "]"
}

// citations keys each criterion id of p to the keys of the cards whose
// items cite it, in card order (the spec-intake concept, piece 2).
func citations(p Plan) map[string][]string {
	out := map[string][]string{}
	for _, c := range p.Cards {
		for _, it := range c.Acceptance {
			if it.Criterion != "" && !slices.Contains(out[it.Criterion], c.Key) {
				out[it.Criterion] = append(out[it.Criterion], c.Key)
			}
		}
	}
	return out
}

// specFindings are the spec block's findings: a spec plan (a plan this
// process gives an integration card) drafts the spec the members confirm
// (piece 5), the outcome in a few sentences and a glossary that names
// each component once. Any other plan asks for none.
func specFindings(p Plan, shape policy.PlanShape) []string {
	if !shape.Integration || (p.Spec != nil && strings.TrimSpace(p.Spec.Outcome) != "") {
		return nil
	}
	return []string{`plan: no spec: draft the spec first, {"spec": {"outcome": "the outcome in a few sentences", "glossary": ["term: what it is"]}}; the members confirm it before the criteria are fixed`}
}

// criteriaFindings are a plan's criteria's findings (the spec-intake
// concept, pieces 1 and 3, roadmap 285): a spec plan wants the spec's
// criteria, 1 to MaxCardAcceptance (the integration card carries one item
// per criterion inside its own cap); each criterion is well formed for
// its kind, every field the kind uses present and within the item cap,
// every other field empty; and, when the plan carries criteria, every
// citation names a criterion of the plan, every criterion is cited by a
// card, every card but the foundation cites one, and the integration
// card's items are the criteria, one item per criterion by id. A plan
// without criteria passes the citation rules; a shape without an
// integration card does not ask for criteria.
func criteriaFindings(p Plan, shape policy.PlanShape) []string {
	var out []string
	if n := len(p.Criteria); n > MaxCardAcceptance || (shape.Integration && n < 1) {
		want := "at most " + strconv.Itoa(MaxCardAcceptance)
		if shape.Integration {
			want = "1 to " + strconv.Itoa(MaxCardAcceptance)
		}
		out = append(out, fmt.Sprintf("plan: %d criteria; want %s", n, want))
	}
	ids := map[string]int{}
	for i, c := range p.Criteria {
		if !criterionID.MatchString(c.ID) {
			out = append(out, fmt.Sprintf("plan: criterion %d: id %q: lowercase letters, digits and underscores, 2 to 32", i+1, c.ID))
		} else if ids[c.ID]++; ids[c.ID] == 2 {
			out = append(out, fmt.Sprintf("plan: criterion id %s is used twice", c.ID))
		}
		if !slices.Contains(criterionKinds, c.Kind) {
			out = append(out, fmt.Sprintf("plan: criterion %s: kind %q, want one of %v", c.ID, c.Kind, criterionKinds))
			continue
		}
		add := func(format string, args ...any) {
			out = append(out, fmt.Sprintf("plan: criterion "+c.ID+": "+format, args...))
		}
		switch c.Kind {
		case CriterionTest:
			if c.Test == "" || c.Inputs == "" || c.Expected == "" {
				add("kind test names its test, its inputs and its expected numbers")
			}
			if c.Claim != "" || c.Prompt != "" || c.Evidence != "" {
				add("kind test carries no claim, prompt or evidence")
			}
		case CriterionInvariant:
			if c.Test == "" {
				add("kind invariant names its suite")
			}
			if c.Inputs != "" || c.Expected != "" || c.Claim != "" || c.Prompt != "" || c.Evidence != "" {
				add("kind invariant carries no inputs, expected numbers, claim, prompt or evidence")
			}
		case CriterionJudgement:
			if c.Claim == "" || c.Inputs == "" || c.Prompt == "" {
				add("kind judgement names its claim, its fixed inputs and its pinned prompt")
			}
			if c.Test != "" || c.Expected != "" || c.Evidence != "" {
				add("kind judgement carries no test, expected numbers or evidence")
			}
		case CriterionHuman:
			if c.Claim == "" || c.Evidence == "" {
				add("kind human names its claim and the evidence the approver sees")
			}
			if c.Test != "" || c.Inputs != "" || c.Expected != "" || c.Prompt != "" {
				add("kind human carries no test, inputs, expected numbers or prompt")
			}
		}
	}
	if len(p.Criteria) == 0 {
		return out
	}
	for _, c := range p.Cards {
		for i, it := range c.Acceptance {
			if it.Criterion != "" && !slices.ContainsFunc(p.Criteria, func(k PlanCriterion) bool { return k.ID == it.Criterion }) {
				out = append(out, fmt.Sprintf("plan: card %s: acceptance item %d cites %q, which is no criterion of the plan", c.Key, i+1, it.Criterion))
			}
		}
	}
	cited := citations(p)
	for _, k := range p.Criteria {
		if len(cited[k.ID]) == 0 {
			out = append(out, fmt.Sprintf("plan: criterion %s is cited by no card", k.ID))
		}
	}
	for _, c := range p.Cards {
		if c.Role == RoleFoundation {
			continue
		}
		if !slices.ContainsFunc(c.Acceptance, func(it PlanItem) bool { return it.Criterion != "" }) {
			out = append(out, fmt.Sprintf("plan: card %s: no acceptance item cites a criterion", c.Key))
		}
	}
	for _, c := range p.Cards {
		if c.Role != RoleIntegration {
			continue
		}
		counts := map[string]int{}
		for i, it := range c.Acceptance {
			switch {
			case it.Criterion == "":
				out = append(out, fmt.Sprintf("plan: card %s: acceptance item %d cites no criterion", c.Key, i+1))
			default:
				counts[it.Criterion]++
			}
		}
		for _, k := range p.Criteria {
			switch n := counts[k.ID]; {
			case n == 0:
				out = append(out, fmt.Sprintf("plan: card %s: criterion %s is cited by no item", c.Key, k.ID))
			case n > 1:
				out = append(out, fmt.Sprintf("plan: card %s: criterion %s is cited by %d items; want one", c.Key, k.ID, n))
			}
		}
	}
	return out
}

// cardFindings are one card's findings on its own fields.
func cardFindings(c PlanCard, limits policy.Limits) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("plan: card %s: "+format, append([]any{c.Key}, args...)...))
	}
	for _, f := range sizeFindings(c.Size) {
		add("%s", f)
	}
	if c.Rung != "" && c.Rung != RungCheap && c.Rung != RungFrontier {
		add("rung %s: want cheap, frontier or none", c.Rung)
	}
	if utf8.RuneCountInString(c.Title) == 0 {
		add("title empty")
	}
	if utf8.RuneCountInString(c.Brief) == 0 {
		add("brief empty")
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
	lines := make([]string, len(c.Acceptance))
	for i, it := range c.Acceptance {
		lines[i] = itemText(it)
	}
	for _, f := range acceptanceFindings(lines) {
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
// readPlan refuses on their own (out of the foundation, into the
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
			panic("planOrder: a cycle readPlan let through")
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
// specBlock is the plan's spec as the plan's comment and the integration
// card's body carry it: the outcome, then the glossary line by line; ""
// when the plan carries no spec.
func specBlock(p Plan) string {
	if p.Spec == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Spec: %s", strings.TrimSpace(p.Spec.Outcome))
	if len(p.Spec.Glossary) > 0 {
		b.WriteString("\nGlossary:")
		for _, g := range p.Spec.Glossary {
			fmt.Fprintf(&b, "\n- %s", g)
		}
	}
	return b.String()
}

func planComment(t Task, revision int, p Plan, issues map[string]int64) string {
	edges := planEdges(p)
	needed, owner := deployOwner(t.Objective, p)
	var b strings.Builder
	next := "Its open questions wait for the members' answers on the factory console, which send it back to the planner; " +
		"a comment here asks the planner for a new revision; closing the issue ends the plan."
	if issues != nil {
		next = "It asks no question, so its cards are filed: the table names their issues."
	}
	fmt.Fprintf(&b, "factoryd: plan revision %d of task %s, written by the planner. %s\n\n%s\n\n", revision, t.ID, next, strings.TrimSpace(p.Summary))
	if s := specBlock(p); s != "" {
		b.WriteString(s + "\n\n")
	}
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
	if len(p.Criteria) > 0 {
		cited := citations(p)
		b.WriteString("\n| # | id | kind | names | cited by |\n|---|---|---|---|---|\n")
		for i, k := range p.Criteria {
			id := cell(k.ID)
			if k.Replaces != "" {
				id += " (replaces " + cell(k.Replaces) + ")"
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", i+1, id, cell(k.Kind), cell(criterionNames(k)), cell(strings.Join(cited[k.ID], ", ")))
		}
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

// criterionNames is what criterion c names, the fields its kind uses
// joined with ": ": the test, the inputs and the expected numbers for a
// test, the suite for an invariant, the claim for a judgement or a human
// criterion.
func criterionNames(c PlanCriterion) string {
	join := func(fields ...string) string {
		var out []string
		for _, f := range fields {
			if f != "" {
				out = append(out, f)
			}
		}
		return strings.Join(out, ": ")
	}
	switch c.Kind {
	case CriterionTest:
		return join(c.Test, c.Inputs, c.Expected)
	case CriterionInvariant:
		return c.Test
	case CriterionJudgement, CriterionHuman:
		return c.Claim
	}
	return join(c.Test, c.Inputs, c.Expected, c.Claim, c.Prompt, c.Evidence)
}

// cardDirectories keys a plan card's directories line.
const cardDirectories = "Directories (guidance, not a boundary)"

// cardFacts are a card's directories, acceptance tests and size as its
// issue and the plan comment list them.
func cardFacts(c PlanCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", cardDirectories, cmp.Or(strings.Join(c.Directories, ", "), "none"))
	b.WriteString("Acceptance:\n")
	for _, it := range c.Acceptance {
		fmt.Fprintf(&b, "- %s\n", itemText(it))
	}
	fmt.Fprintf(&b, "Size: %s\n", c.Size)
	if c.Rung != "" {
		fmt.Fprintf(&b, "Rung: %s, %s\n", c.Rung, cmp.Or(strings.TrimSpace(c.RungReason), "the planner's choice"))
	}
	return b.String()
}

// cardBody is card c's issue body before its marker: card i of n of of
// (plan #n), filed for author, with the issues of the cards it depends on
// (deps, their keys by issue; a card not filed yet by its key alone), and
// start, the line on the old card's work it starts from, after the facts
// ("" for none). The integration card carries the plan's spec (piece 7:
// its brief already does), spec the rendered spec block or "".
func cardBody(c PlanCard, of string, i, n int, author string, deps []Dependency, spec, start string) string {
	on := make([]string, 0, len(deps))
	for _, d := range deps {
		if d.Issue == 0 {
			on = append(on, d.Key)
			continue
		}
		on = append(on, fmt.Sprintf("#%d (%s)", d.Issue, d.Key))
	}
	directories, facts, _ := strings.Cut(cardFacts(c), "\n")
	body := fmt.Sprintf("Card %s of %s (%d of %d), filed for %s. Written by the planner.\n\n%s\n\n%s\nDepends on: %s\n%s",
		c.Key, of, i, n, author, strings.TrimSpace(c.Brief), directories, cmp.Or(strings.Join(on, ", "), "none"), strings.TrimSuffix(facts, "\n"))
	if start != "" {
		body += "\n" + start
	}
	if spec != "" {
		body += "\n\nThe spec of the whole feature:\n" + spec
	}
	return body
}

// workLine is the start line of a card that starts from old card k's
// work; goneLine its line when k's branch is gone.
func workLine(k Task) string {
	work := fmt.Sprintf("#%d", k.IssueNumber)
	if k.PlanKey != "" {
		work += " (" + k.PlanKey + ")"
	}
	return "Starts from the work of " + work + ", on this card's branch: finish what it holds, make it build green, and land it."
}

func goneLine(k Task) string {
	return fmt.Sprintf("The branch of #%d is gone: start from main.", k.IssueNumber)
}

// planGuide is the planner's guide: the spec is the issue, the answer a
// plan filed with the file_plan tool, a card one session of the pinned
// limits. The shape's mechanical rules (counts, keys, bounds, cycles) are
// readPlan's findings, not this prose (roadmap 186). pins are the
// project's images a card may name.
func planGuide(shape policy.PlanShape, limits policy.Limits, pins map[string]policy.ImagePin) string {
	g := "You are the planner. " +
		"The issue's text tells what to build. Read it and the repository: " +
		"every card follows the repository's conventions. " +
		"Draft the spec before the cards: the outcome in a few sentences, a glossary that names each component once, then the criteria; " +
		"when the repository carries a spec skeleton at docs/spec.md, draft into its shape and carry its standing criteria. " +
		"Split the work into the cards of a plan. " +
		"A card is one pull request by one agent: one package, one page, or one set of endpoints, with its tests, about ten files at most; a larger piece of work is two cards. " +
		"You file the whole result with the file_plan tool: its plan argument is the complete plan json, one object holding " +
		`{"spec": {"outcome": "...", "glossary": ["term: what it is"]}, "summary": "a paragraph: what gets built and in which order", "criteria": [{"id": "...", "kind": "test|invariant|judgement|human", "test": "...", "inputs": "...", "expected": "...", "claim": "...", "prompt": "...", "evidence": "..."}], "cards": [{"key": "...", "role": "...", "title": "...", "brief": "...", ` +
		`"directories": ["..."], "depends_on": ["..."], "acceptance": [{"criterion": "...", "text": "..."}], "size": "small|medium|large", "rung": "cheap|frontier", "rung_reason": "one line"` + imageField(pins) + `}], "questions": [{"text": "...", "choices": ["...", "..."]}]}. ` +
		"spec: the outcome in a few sentences and the glossary that names each component once; the members confirm the spec in Needs you, and the criteria are fixed only when they answer. " +
		"criteria: the spec's proof, one object per criterion: the issue's criteria list when it carries one, else drafted by you from the spec's acceptance tests. " +
		"Each criterion has an id in snake_case and a kind naming the fields it uses: " +
		"kind test names its test as TestXxx in <package>, a test bash check runs in-process, against in-process fakes: no browser, no external database, no device; a want beyond the sandbox becomes a human criterion or waits as a note; " +
		"kind invariant names its suite as go test <suite> passes, for example go test ./..., and carries no inputs; " +
		"kind judgement names the claim a model judges, its fixed inputs and the pinned prompt, and no test; " +
		"kind human names a claim only a person settles, with the evidence the approver will see. " +
		"Every card but the foundation cites the criterion its acceptance item works on, and every criterion is cited by some card's item. " +
		"An amendment of a filed revision adds a criterion, removes one (its citing card goes with it, unless it merged), or changes one under a new id that names the old one in replaces; an id never changes its definition. " +
		"A replan lists the plan's old cards in the findings. Keep a card by keeping its key. " +
		`Change a card by adding a new card with "from": "<its key>" (or "#<issue>" for a card the findings name without a key): ` +
		"the new card starts from that card's branch and lands what it holds first. Drop a card by leaving its key out: an open card stops, its branch stays. " +
		"role: foundation, feature or integration. rung: the model rung the card starts on: cheap for routine, well-specified work, frontier for a large change or one in factoryd's own package; leave it out to derive it from the size. "
	g += imagesLine(pins)
	if shape.Foundation {
		g += "The foundation card holds the layout, the API contract, the schema and one end-to-end test, which every other card builds on. " +
			"The foundation's brief names the contracts every card builds on: how a handler gets the logged-in user (the function and its package), " +
			"the store (its interface, its implementation's package, the methods stubbed), and the server constructor tests import (internal/server.New unless the template says otherwise). " +
			"Every card adds its routes in its own internal/server/routes_<key>.go, so no two cards edit one route list. "
	}
	if shape.Integration {
		g += "The integration card turns the spec's acceptance tests into tests under acceptance/ and makes them pass, after every other card. " +
			"Its acceptance items are the criteria, one item per criterion by id; a judgement or human criterion arrives as an item that names its claim. "
	}
	if shape.Minimum() == 1 {
		g += "A change that fits one card answers one card, its own integration, citing every criterion. "
	}
	if shape.Foundation || shape.Integration {
		g += "factoryd adds the foundation and integration edges itself: " +
			"list in depends_on only the dependencies among feature cards. "
	} else {
		g += "depends_on lists the keys of the cards a card needs merged first. "
	}
	return g + "title and brief: markdown, what to build and how it is checked. " +
		"directories: paths relative to the repository's root, guidance for the card's agent; " +
		"acceptance: each item a test by name: TestXxx in <package>, its inputs and the numbers it expects, citing the criterion it works on; the foundation's items need cite none. " +
		"size: small, medium or large; anything bigger is two cards. " +
		"Attack your own draft once before you file: reread the spec, the criteria and the cards for contradictions, ambiguity and missing edge cases, " +
		"and turn each finding into a question with origin self_attack. " +
		"questions: open points the spec leaves to the member; their answers come back to you with your last plan, " +
		"and a plan with open questions waits for their answers, while a plan with none files its cards at once. " +
		"Every card's agent and CI run bash check; do not ask a card to verify with a tool the check does not run. " +
		"A writer reads your plan on the issue; a comment asking for changes comes back to you as a finding, with your last plan: " +
		"answer the whole plan again."
}

// imageField is a card's image field in the plan's shape where the
// project pins images, "" where it pins none.
func imageField(pins map[string]policy.ImagePin) string {
	if len(pins) == 0 {
		return ""
	}
	return `, "image": "..."`
}

// imagesLine is the planner's line of the project's pinned images with
// their tools, "" without pins.
func imagesLine(pins map[string]policy.ImagePin) string {
	if len(pins) == 0 {
		return ""
	}
	return "Images: " + ImageList(pins) + "; a card that needs one names it in image, any other card runs the default. "
}

// ImageList is pins as the planner and the project agent read them: each
// image by name with its tools.
func ImageList(pins map[string]policy.ImagePin) string {
	var images []string
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		images = append(images, name+" ("+strings.Join(pins[name].Tools, ", ")+")")
	}
	return strings.Join(images, ", ")
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

// cardLineBound is the changed lines a card of the given size plans at
// most, and whether the size is one of small, medium, large. An unsized
// card is bound by large.
func cardLineBound(size string, largeLines int) (lines int, sized bool) {
	bounds := sizeLines(largeLines)
	lines, sized = bounds[size]
	if !sized {
		lines = bounds["large"]
	}
	return lines, sized
}

// PlanAnswerComment is the console's comment, as the member, on the plan's
// issue when they answer question idx: intake knows it for no change
// request by the answer stored before it.
func PlanAnswerComment(idx int, answer string) string {
	return fmt.Sprintf("Answer %d: %s", idx, answer)
}

// ErrStalePlan refuses an answer to a revision that is no longer the
// newest; its message is the card's.
type ErrStalePlan struct{ Newest int }

func (e ErrStalePlan) Error() string {
	return fmt.Sprintf("the plan changed to revision %d; reload", e.Newest)
}

// ErrAnswerUnsignalled is a plan's answer stored whose workflow the
// console could not signal.
var ErrAnswerUnsignalled = errors.New("the answer is recorded, but the plan's workflow was not signalled")

// AnswerPlan is the console's answer by a member to question idx of plan
// revision revision, which must be the newest (ErrStalePlan): the answer
// is stored (ErrAnswered for a second one), then the plan task's workflow
// signalled, which sends the planner back once none is open. The last
// answer resolves the plan's request as superseded: the planner revises
// the revision with the answers.
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
	confirming, err := confirmationAnswer(ctx, store, taskID, revision, []int{idx})
	if err != nil {
		return err
	}
	if confirming {
		if open, err := store.UndecidedHoldouts(ctx, taskID, revision); err != nil || open > 0 {
			if err == nil {
				err = ErrHoldoutsOpen
			}
			return err
		}
	}
	if err := store.AnswerPlanQuestion(ctx, taskID, revision, idx, answer, by); err != nil {
		return err
	}
	if confirming {
		if err := store.SweepPlanHoldouts(ctx, taskID); err != nil {
			return err
		}
	}
	if err := wf.Signal(ctx, taskID, SignalPlanAnswered, TaskSignal{Author: by, Revision: revision, Index: idx}); err != nil {
		return fmt.Errorf("%w: %v", ErrAnswerUnsignalled, err)
	}
	if open, err := store.OpenPlanQuestions(ctx, taskID, revision); err != nil || open > 0 {
		return err
	}
	return store.ResolveApprovals(ctx, taskID, ApprovalPlan, "denied", FactoryDecider, fmt.Sprintf("superseded: the planner revises revision %d with the answers", revision))
}

// confirmationAnswer tells whether any of idxs names revision's
// confirmation question: its answer fixes the criteria, so the round's
// hold-outs must carry their clicks first and sweep after.
func confirmationAnswer(ctx context.Context, store Store, taskID string, revision int, idxs []int) (bool, error) {
	ci, ok, err := store.ConfirmationIndex(ctx, taskID, revision)
	if err != nil || !ok {
		return false, err
	}
	for _, i := range idxs {
		if i == ci {
			return true, nil
		}
	}
	return false, nil
}

// AnswerPlanQuestions is the console's batch answer by a member to every
// question of plan revision revision it names at once, which must be the
// newest (ErrStalePlan): all answers are stored in one statement (any bad
// entry — ErrAnswered for an answered question, ErrNotFound for an unknown
// one — stores none), then the plan task's workflow signalled once per
// answer, which sends the planner back once none is open. When no question
// of the revision stays open the plan's request resolves as superseded:
// the planner revises the revision with the answers.
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
	idxs := make([]int, len(answers))
	for i, a := range answers {
		idxs[i] = a.Idx
	}
	confirming, err := confirmationAnswer(ctx, store, taskID, revision, idxs)
	if err != nil {
		return err
	}
	if confirming {
		if open, err := store.UndecidedHoldouts(ctx, taskID, revision); err != nil || open > 0 {
			if err == nil {
				err = ErrHoldoutsOpen
			}
			return err
		}
	}
	if err := store.AnswerPlanQuestions(ctx, taskID, revision, answers, by); err != nil {
		return err
	}
	if confirming {
		if err := store.SweepPlanHoldouts(ctx, taskID); err != nil {
			return err
		}
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
	return ApprovalRequest{}, fmt.Errorf("task %s has no plan waiting for answers", taskID)
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO plan_questions (task_id, revision, idx, text, choices, kind)
		SELECT $1, $2, i, q->>'text', q->'choices', 'question' FROM jsonb_array_elements($3::jsonb) WITH ORDINALITY AS a(q, i) ON CONFLICT DO NOTHING`, id, revision, b)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PutPlanConfirmation appends the confirmation question to revision's
// questions, after the planner's: kind confirmation, the index after the
// last question the revision holds, nothing when it has it already (an
// activity retry).
func (s *PGStore) PutPlanConfirmation(ctx context.Context, id string, revision int, q PlanQuestion) (int, bool, error) {
	choices, err := json.Marshal(q.Choices)
	if err != nil {
		return 0, false, err
	}
	var idx int
	err = s.db.QueryRowContext(ctx, `INSERT INTO plan_questions (task_id, revision, idx, text, choices, kind)
		SELECT $1, $2, COALESCE(MAX(idx), 0) + 1, $3, $4, 'confirmation' FROM plan_questions WHERE task_id = $1 AND revision = $2
		HAVING NOT EXISTS (SELECT 1 FROM plan_questions WHERE task_id = $1 AND revision = $2 AND kind = 'confirmation')
		RETURNING idx`, id, revision, q.Text, choices).Scan(&idx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return idx, true, nil
}

// PlanConfirmationAnswered is the newest of plan task id's revisions whose
// confirmation question is answered, and whether there is one: a later
// revision is judged against it.
func (s *PGStore) PlanConfirmationAnswered(ctx context.Context, id string) (int, bool, error) {
	var revision sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(revision) FROM plan_questions WHERE task_id = $1 AND kind = 'confirmation' AND answer IS NOT NULL`, id).Scan(&revision)
	return int(revision.Int64), revision.Valid, err
}

// ConfirmationIndex is revision's confirmation question's index, and
// whether it has one.
func (s *PGStore) ConfirmationIndex(ctx context.Context, id string, revision int) (int, bool, error) {
	var idx int
	err := s.db.QueryRowContext(ctx, `SELECT idx FROM plan_questions WHERE task_id = $1 AND revision = $2 AND kind = 'confirmation'`, id, revision).Scan(&idx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return idx, true, nil
}

// PutPlanCatches stores revision's hole catches of one kind and finder,
// the recording rule's ledger (the spec-intake concept, roadmap 285):
// false when the revision holds them already, so an activity retry adds
// nothing.
func (s *PGStore) PutPlanCatches(ctx context.Context, id string, revision int, kind, finder string, details []string) (bool, error) {
	b, err := json.Marshal(details)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO plan_catches (task_id, revision, kind, idx, finder, detail)
		SELECT $1, $2, $3, i, $4, d FROM jsonb_array_elements_text($5::jsonb) WITH ORDINALITY AS a(d, i) ON CONFLICT DO NOTHING`,
		id, revision, kind, finder, b)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *PGStore) PlanQuestions(ctx context.Context, id string, revision int) ([]PlanQuestion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text, choices, COALESCE(kind, 'question'), COALESCE(answer, ''), COALESCE(answered_by, ''), answered_at FROM plan_questions
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
		if err := rows.Scan(&q.Text, &choices, &q.Kind, &q.Answer, &q.AnsweredBy, &at); err != nil {
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

// switchToPlan sends a direct card's work to the planner
// (versionDirectToPlan): BecomePlan turns it plan-track, without the pull
// request it had, and the program runs again, which TaskConfig now gives
// session mode plan. The writers' comments the direct run left unread are
// recorded as ignored, so the plan loop does not take them as written
// while the planner ran, and the gates beside the checks stop, their
// questions withdrawn (versionReplan). A refusal is recorded on the
// timeline, kept in t.planRefused, and the direct lifecycle goes on
// (switched false), its gates still running. The end of a switched run is
// kept in t.switched for a wait's caller.
func (t *taskRun) switchToPlan(sw PlanSwitch) (final string, switched bool, err error) {
	if t.directToPlan < versionDirectToPlan {
		return "", false, nil
	}
	var res PlanSwitchResult
	if err := workflow.ExecuteActivity(t.short, ActivityBecomePlan, t.taskID, sw).Get(t.ctx, &res); err != nil {
		final, err := t.finish("failed", "become plan: "+errMessage(err))
		t.switched = &programEnd{final, err}
		return final, true, err
	}
	if !res.Switched {
		t.planRefused = res.Refused
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "to_plan_refused", map[string]any{"reason": res.Refused}).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record to_plan_refused", "task", t.taskID, "error", err)
		}
		return "", false, nil
	}
	t.pr = PullRequest{}
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalHumanComment).ReceiveAsync(&sig) {
		detail := map[string]any{"signal": SignalHumanComment, "author": sig.Author, "comment": sig.Comment, "ignored": "written before the card went to the planner"}
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
		}
	}
	if t.replan >= versionReplan {
		t.cancelBeside(toPlanner)
	}
	final, err = t.program()
	t.switched = &programEnd{final, err}
	return final, true, err
}

// programEnd is how a run's program ended.
type programEnd struct {
	final string
	err   error
}

// replans tells a run whose waits take a person's replan (versionReplan):
// a direct agent card's coding run, not a roll's, a factory pull request
// card's or a plan child's, once its screening passed.
func (t *taskRun) replans() bool {
	return t.replan >= versionReplan && t.cfg.Task.Session == policy.SessionCode && t.cfg.Task.Track == policy.TrackDirect &&
		t.source != SourcePlan && !slices.Contains(unboundSources, t.source) && !t.screening
}

// takeToPlan takes a pending to_plan, a person's replan of the card: the
// switch it asks for and the replan step it records.
func (t *taskRun) takeToPlan() (sw PlanSwitch, step ReplanStep, ok bool) {
	var sig TaskSignal
	if !workflow.GetSignalChannel(t.ctx, SignalToPlan).ReceiveAsync(&sig) {
		return PlanSwitch{}, ReplanStep{}, false
	}
	return PlanSwitch{Reason: sig.Body, Work: "Asked by " + sig.Author + "."},
		ReplanStep{Origin: ReplanPerson, By: sig.Author, Comment: sig.Body, Card: t.taskID}, true
}

// replanToPlan is switchToPlan of a replan: the replan step, when there is
// one, goes on the card's own timeline first (versionReplan), for the
// planner's record.
func (t *taskRun) replanToPlan(step ReplanStep, sw PlanSwitch) (final string, switched bool, err error) {
	if t.replan >= versionReplan && step.Origin != "" {
		detail := map[string]any{"origin": step.Origin, "card": step.Card}
		if step.By != "" {
			detail["by"] = step.By
		}
		if step.Comment != "" {
			detail["comment"] = step.Comment
		}
		if step.Work != "" {
			detail["work"] = step.Work
		}
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, stepReplan, detail).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record replan", "task", t.taskID, "error", err)
		}
	}
	return t.switchToPlan(sw)
}

// switchFromWait sends a card from a wait to the planner: a card in review
// has its approval superseded, the card runs, a status BecomePlan accepts,
// and switches. A switched card's final ends the wait (done); a refused
// switch moves the card back to back and the wait goes on. The plan
// program's error comes back through the caller's finish.
func (t *taskRun) switchFromWait(sw PlanSwitch, step ReplanStep, back string) (final, why string, done bool) {
	if back == "in-review" {
		reason := "superseded: " + cmp.Or(step.By, "a person") + " asked to send the card back to the planner"
		if err := workflow.ExecuteActivity(t.short, ActivitySupersedeApproval, t.taskID, reason).Get(t.ctx, nil); err != nil {
			return "failed", "supersede the approval: " + errMessage(err), true
		}
	}
	if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
		return "failed", "set running: " + errMessage(err), true
	}
	if final, switched, _ := t.replanToPlan(step, sw); switched {
		return cmp.Or(final, "failed"), "the card went to the planner", true
	}
	if back != "running" {
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, back).Get(t.ctx, nil); err != nil {
			return "failed", "set " + back + ": " + errMessage(err), true
		}
	}
	return "", "", false
}

// replanCut sends a card whose session stopped for a replan
// (OutcomeReplan) to the planner: a direct card, cut by the check-in or
// stopped by a person, turns plan-track and the plan loop runs on; a
// plan's child goes back to its plan and ends closed. done with the run's
// end, else refused says why not, for a person.
func (t *taskRun) replanCut(res SessionResult) (final, refused string, done bool, err error) {
	if t.source == SourcePlan {
		var rc ReplanChildResult
		if err := workflow.ExecuteActivity(t.short, ActivityReplanChild, t.taskID, res.Replan).Get(t.ctx, &rc); err != nil {
			final, err := t.finish("failed", "replan child: "+errMessage(err))
			return final, "", true, err
		}
		if rc.Refused != "" {
			return "", rc.Refused, false, nil
		}
		final, err := t.finish("closed", rc.Reason)
		return final, "", true, err
	}
	reason, work := sanitize.Inbound(res.Reason), sanitize.Inbound(res.Replan)
	sw, step := PlanSwitch{Reason: "cut: " + reason, Work: work}, ReplanStep{Origin: ReplanCut, Card: t.taskID, Work: work}
	if res.Trip == string(TripReplan) {
		var ok bool
		if sw, step, ok = t.takeToPlan(); !ok {
			sw, step = PlanSwitch{Reason: reason, Work: work}, ReplanStep{Origin: ReplanPerson, Comment: reason, Card: t.taskID}
		}
	}
	if final, switched, err := t.replanToPlan(step, sw); switched {
		return final, "", true, err
	}
	return "", t.planRefused, false, nil
}

// planLoop is the program of a task of session mode plan: planner sessions
// until one answers a valid plan, which RequestPlan files when it asks no
// question (a refusal sends the planner back) and which otherwise waits for
// its answers; the last answer or a writer's comment sends the planner
// back with the last plan, a closed issue ends the task closed. Every
// session counts against the session cap, where the card waits for a
// human and a writer's comment buys one more.
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
		return t.finish("closed", ReasonIssueClosed+" before the plan filed its cards")
	}
	u := st.Usage
	var ref PlanRef
	var findings []string
	healed := st.Status == "waiting-human"
	if healed {
		if err := t.awaitFilingStops(); err != nil {
			return t.finish("failed", "replan stops: "+errMessage(err))
		}
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(t.ctx, requestPlanOptions()), ActivityRequestPlan, t.taskID, 0, t.cfg.PolicyVersion).Get(t.ctx, &ref); err != nil {
			return t.finish("failed", "request plan: "+errMessage(err))
		}
		if final, why, next, stop := planRequested(&ref); stop {
			if final != "" {
				return t.finish(final, why)
			}
			findings = next
		}
		t.escalation = ref.Comment
	}
	wait := ref.Revision != 0
	var ask string // a question for a human: the cap card, or a stop's
	// carried are the findings of a session that could not start: the
	// next one starts with them and what the wait brings
	var carried []string
	reason := "no session answered a plan"
	refusals := 0 // refused sessions in a row (versionSessionRefusals)
	// day is the wait on the day cap the next ask holds (versionDayCap)
	var day *dayWait
	// a healed run with no plan pending whose planner is out of time with
	// no retry due re-asks as healedReason, so no second alert posts; the
	// answer buys the time at the wait's branch below
	if healed && !wait && t.plannerRetry >= versionPlannerRetry {
		if short, retry := t.plannerTime(u); short && !retry {
			ask = healedReason
		}
	}
	if healed && t.dayCap >= versionDayCap && st.DayCap != "" && workflow.Now(t.ctx).Before(st.DayCapUntil) {
		ask, day = st.DayCap, &dayWait{until: st.DayCapUntil, model: t.cfg.Models.Stronger}
	}
	// the comment the running or next session answers: writers' comments
	// newer than it ask for changes
	floor := t.escalation
	stopsAwaited := t.replan < versionReplan
	for {
		if !wait && ask == "" && u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts {
			ask = t.sessionCapCard(reason, findings)
			if healed {
				ask = healedReason
			}
		}
		if ask != "" {
			if err := t.ask(ask, escalationQuestion(ask), nil); err != nil {
				return t.finish("failed", escalateFailed+errMessage(err))
			}
			ask, wait = "", true
		}
		healed = false
		if wait {
			final, why, next := t.awaitPlan(&ref, floor, day)
			if final != "" {
				return t.finish(final, why)
			}
			findings, wait, carried = append(carried, next...), false, nil
			// an answer at the session cap buys a session; one to a planner
			// out of time, its automatic session used or the cap too short
			// for one, buys agent time (versionPlannerRetry); an answer on
			// the day's turn buys nothing
			if u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts && !day.over() {
				t.grant(&u)
			} else if short, retry := t.plannerTime(u); t.plannerRetry >= versionPlannerRetry && short && !retry && !day.over() {
				t.grantTime(&u)
			}
			day = nil
			continue
		}
		remaining := t.cfg.WallClockCap + u.ExtraTime + u.RetryTime - u.AgentTime
		if t.plannerRetry >= versionPlannerRetry {
			switch short, retry := t.plannerTime(u); {
			case short && retry:
				u.RetryTime = t.cfg.WallClockCap/2 - remaining
				t.recordUsage(u)
				remaining = t.cfg.WallClockCap + u.ExtraTime + u.RetryTime - u.AgentTime
			case short:
				ask = t.plannerTimeCard(u)
				continue
			}
		}
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
		if !stopsAwaited {
			if err := t.awaitReplanStops(); err != nil {
				return t.finish("failed", "replan stops: "+errMessage(err))
			}
			stopsAwaited = true
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
			continue
		}
		if t.podWait >= versionPodWait && err == nil && !decided && res.NotStarted != "" {
			// no planner ran: no attempt, no agent time, the same findings
			if ask = t.notStarted(res.NotStarted); ask != "" {
				reason, carried = "no node started the planner session's pod", findings
			}
			if final, why, ok := t.decided(); ok {
				final, why, _ = closedBeforeStart(final, why)
				return t.finish(final, why)
			}
			continue
		}
		t.pendingPods = 0
		capped := t.dayCap >= versionDayCap && err == nil && !decided && res.Capped != nil
		// a session the day cap refused at its first call is no attempt
		if !res.StartFailed && !(capped && res.Capped.Decision == policy.RefusedDayCap && !res.Capped.Allowed) {
			t.providerAnswered(&refusals, err)
			u.AgentTime += workflow.Now(t.ctx).Sub(began)
			u.Attempts++
		}
		t.recordUsage(u)
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
		case capped:
			// the next session starts with the same findings, once a human
			// answers or, at the day cap, the wait ends on its own
			reason = "the gateway refused the planner session at a cap"
			if until := res.Capped.until(); until.IsZero() || workflow.Now(t.ctx).Before(until) {
				ask, carried = res.Capped.reason(), findings
				if !until.IsZero() {
					day = &dayWait{until: until, model: t.cfg.Models.Stronger}
				}
			}
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
		if err := t.awaitFilingStops(); err != nil {
			return t.finish("failed", "replan stops: "+errMessage(err))
		}
		// a writer's comment made while the session ran, or while the
		// filing waited, asks for changes before the revision may file
		if final, why, comments := t.planCommentsWhole(floor); final != "" {
			return t.finish(final, why)
		} else if len(comments) > 0 {
			reason, findings = "a writer asked for changes while the planner ran", comments
			continue
		}
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(t.ctx, requestPlanOptions()), ActivityRequestPlan, t.taskID, res.Revision, t.cfg.PolicyVersion).Get(t.ctx, &ref); err != nil {
			return t.finish("failed", "request plan: "+errMessage(err))
		}
		if final, why, next, stop := planRequested(&ref); stop {
			if final != "" {
				return t.finish(final, why)
			}
			reason, findings = "the plan's revision filed no cards", next
			continue
		}
		t.escalation, findings, wait = ref.Comment, nil, true
		t.approvalRequested = workflow.Now(t.ctx)
		reason = fmt.Sprintf("plan revision %d waits for its answers", ref.Revision)
	}
}

// replanStopsWait bounds the wait of a replan's planner for the cards the
// replan stops; replanStopsPoll is how often it asks.
const (
	replanStopsWait = 10 * time.Minute
	replanStopsPoll = 15 * time.Second
)

// awaitReplanStops holds the run's first planner session (versionReplan)
// until the cards a pending replan stops, and the started cards the
// newest revision drops, have ended, so the planner reads what they left.
// Past replanStopsWait it records the cards still open and lets the
// planner run. It keeps the person's name and note of the replan for
// planComments.
func (t *taskRun) awaitReplanStops() error {
	start := workflow.Now(t.ctx)
	for {
		var w ReplanWait
		if err := workflow.ExecuteActivity(t.short, ActivityReplanStopped, t.taskID).Get(t.ctx, &w); err != nil {
			return err
		}
		t.replanBy, t.replanNote = w.By, w.Comment
		if w.Stopped {
			return nil
		}
		if workflow.Now(t.ctx).Sub(start) >= replanStopsWait {
			if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "replan_stops_late", map[string]any{"open": w.Open}).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("record replan_stops_late", "task", t.taskID, "error", err)
			}
			return nil
		}
		if err := workflow.Sleep(t.ctx, replanStopsPoll); err != nil {
			return err
		}
	}
}

// awaitFilingStops is awaitReplanStops before a revision files
// (versionReplan): the cards it drops stop meanwhile, and a card that
// starts from one's work copies its branch once its last push is in.
func (t *taskRun) awaitFilingStops() error {
	if t.replan < versionReplan {
		return nil
	}
	return t.awaitReplanStops()
}

// plannerTime tells a plan card's agent time (versionPlannerRetry): short
// when less is left than a session needs, retry when its one automatic
// top-up to half the cap is still to come and half the cap is enough for a
// session.
func (t *taskRun) plannerTime(u TaskUsage) (short, retry bool) {
	least := min(minSession, t.cfg.WallClockCap)
	return t.cfg.WallClockCap+u.ExtraTime+u.RetryTime-u.AgentTime < least, u.RetryTime == 0 && t.cfg.WallClockCap/2 >= least
}

// plannerOutOfTime opens the question of a plan card whose planner is out
// of time with no automatic session to come.
const plannerOutOfTime = "the planner used its agent time"

// plannerTimeCard is that question: the automatic session used, or the cap
// too short for one, and what an answer buys.
func (t *taskRun) plannerTimeCard(u TaskUsage) string {
	what := " and its automatic second session without filing a plan"
	if u.RetryTime == 0 {
		what = fmt.Sprintf(" without filing a plan, and its %g-minute cap is too short for an automatic second session", t.cfg.WallClockCap.Minutes())
	}
	return fmt.Sprintf("%s%s: an answer starts one more planner session with %g minutes of agent time and the answer among its findings",
		plannerOutOfTime, what, t.answerTime().Minutes())
}

// planRequested reads what RequestPlan made of revision ref: filed cards
// end the plan card done (final); a refusal or a revision answered whole
// clears ref and goes back to the planner with findings; either way stop.
// A revision with open questions waits.
func planRequested(ref *PlanRef) (final, why string, findings []string, stop bool) {
	switch {
	case len(ref.Filed) > 0:
		return "done", fmt.Sprintf("plan revision %d filed %d cards", ref.Revision, len(ref.Filed)), nil, true
	case len(ref.Refused) > 0 || ref.Answered:
		findings = ref.Refused
		*ref = PlanRef{}
		return "", "", findings, true
	}
	return "", "", nil, false
}

// planComments takes the writers' comments signalled so far: each newer
// than floor, the comment the session answered, asks for changes, except
// the note of the replan that reopened the plan, posted as its person's
// (versionReplan). heard are the comments that ask.
func (t *taskRun) planComments(floor int64) (out []string, heard []int64) {
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalHumanComment).ReceiveAsync(&sig) {
		detail := map[string]any{"signal": SignalHumanComment, "author": sig.Author, "comment": sig.Comment}
		switch {
		case sig.Comment <= floor:
			detail["ignored"] = "older than the plan or question the planner answered"
		case t.replan >= versionReplan && t.replanBy != "" && sig.Author == t.replanBy && strings.TrimSpace(sig.Body) == strings.TrimSpace(t.replanNote):
			detail["ignored"] = "the replan's own note"
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
	t.awaitBurst([]string{SignalIssueClosed, SignalHumanComment}, func() ([]int64, bool) {
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

// planEnded takes a close of the issue signalled so far: the task's end
// before the plan filed its cards.
func (t *taskRun) planEnded() (final, why string, ok bool) {
	var sig TaskSignal
	if workflow.GetSignalChannel(t.ctx, SignalIssueClosed).ReceiveAsync(&sig) {
		t.counted(SignalIssueClosed, sig)
		return "closed", ReasonIssueClosed + " before the plan filed its cards", true
	}
	return "", "", false
}

// awaitPlan waits, at no cost, for the writers on revision ref (none
// yet: only a writer's comment or an end): the task's end, or the
// findings the next planner session starts with. A writer's comment and
// the rest of its burst (versionPlanBurst), or the answer that leaves no
// question of the revision open (versionPlanAnswers), goes back to the
// planner, as does the end of day, a wait on the day cap, on its own
// (versionDayCap).
func (t *taskRun) awaitPlan(ref *PlanRef, floor int64, day *dayWait) (final, why string, findings []string) {
	day.start(t)
	defer day.stop()
	wake := []string{SignalIssueClosed, SignalHumanComment}
	if t.planAnswers >= versionPlanAnswers {
		wake = append(wake, SignalPlanAnswered)
	}
	answered := false
	pass := func() (heard []int64, stop bool) {
		if final, why, stop = t.planEnded(); stop {
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
		day.add(sel)
		sel.Select(t.ctx)
		heard, stop := pass()
		if stop || len(heard) > 0 && t.planBurst >= versionPlanBurst && t.awaitBurst(wake, pass, heard) {
			if final != "" {
				return final, why, nil
			}
			return "", "", findings
		}
		if answered || len(findings) > 0 || day.over() {
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
		switch {
		case sig.Revision != ref.Revision:
			detail["ignored"] = fmt.Sprintf("the plan changed to revision %d", ref.Revision)
		case !t.approvalRequested.IsZero():
			// the writers' wait ends: its step carries the wait's start
			detail["started"] = rfc3339(t.approvalRequested)
			t.approvalRequested = time.Time{}
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

// OpenPlanQuestions is how many of plan revision's questions have no
// answer.
func (r *Runner) OpenPlanQuestions(ctx context.Context, taskID string, revision int) (int, error) {
	return r.store.OpenPlanQuestions(ctx, taskID, revision)
}

// projectPins are the image pins of repository's project in v: a plan's
// cards are held to the version in force's, which taskImage resolves with.
func projectPins(v policy.Version, repository string) map[string]policy.ImagePin {
	_, p, _ := v.ProjectOf(repository)
	return p.Images
}

// planShape is the plan track's shape of the process task t follows in
// version v, and the process's name.
func planShape(v policy.Version, t Task) (policy.PlanShape, string, error) {
	name := t.Process
	if name == "" {
		_, project, _ := v.ProjectOf(t.Repository)
		name = project.Process
	}
	plan, ok := v.Processes[name].Tracks.Track(policy.TrackPlan)
	if !ok || plan.Plan == nil {
		return policy.PlanShape{}, "", terminalError("task %s: process %q has no plan track in configuration version %d", t.ID, name, v.ID)
	}
	return *plan.Plan, name, nil
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
	extra, findings, err := r.planRecord(ctx, t, findings)
	if err != nil {
		return PlanResult{}, err
	}
	r.takeFiled(taskID) // a filing an earlier session left is stale here
	began := time.Now()
	end, err := r.reportSession(ctx, t, repo, model, t.Standing(), true, pin, planGuide(shape, v.Document.Limits, projectPins(r.cfg.Policy(), t.Repository)), findings, answers, extra)
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
	if len(end.reads) > 0 {
		found = append(found, readsFinding(end.reads))
	}
	r.record(ctx, taskID, "plan_invalid", map[string]any{"findings": found})
	res := PlanResult{Findings: found, Capped: end.capped}
	if end.pending != nil {
		res.NotStarted = why
	}
	return res, nil
}

// planRecord is what plan t's planner session gets of the work before
// it. A pending replan's findings go after findings as one, since the
// session's fit drops the oldest first, and its old cards' branches go to
// the clone. The agent's words, the cut's opener and the work so far of a
// card that ran direct, stand in the brief's fence beside the old cards'
// files, and in no finding.
func (r *Runner) planRecord(ctx context.Context, t Task, findings []string) (reportExtra, []string, error) {
	rec, replan, err := r.replanRecord(ctx, t)
	if err != nil {
		return reportExtra{}, nil, err
	}
	tl, err := r.objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
	if err != nil {
		return reportExtra{}, nil, fmt.Errorf("the timeline of %s: %w", t.ID, err)
	}
	var extra reportExtra
	var fenced []string
	prior := harness.PriorState{Session: stepToPlan, Heading: "The work so far on this card, as factoryd copied it when this session started."}
	if replan {
		// one finding, so the fit drops it last and the last plan before it
		findings = append(slices.Clone(findings), strings.Join(rec.findings, "\n"))
		extra.branches = rec.branches
		prior.Session = stepReplan
		prior.Heading = "The records of the plan's old cards, as factoryd copied them when this session started."
		fenced = append([]string{"One file per card: its review findings, its criteria verdicts, the check-in's notes and the working state of its last session."}, rec.work...)
	}
	if step := lastStep(tl, stepToPlan); step != nil {
		work, _ := step["work_so_far"].(string)
		fenced = append(fenced, workSoFarHead+sanitize.Inbound(work))
	}
	if len(fenced) > 0 {
		prior.Body = strings.Join(fenced, "\n\n")
		extra.record = &carryState{Prior: prior, Files: rec.files}
	}
	return extra, findings, nil
}

// readsFinding tells the next planner session what the last one read.
func readsFinding(paths []string) string {
	return fmt.Sprintf("the planner session read %d files before it ended: %s. File the plan from what they hold; read a file again only where the plan needs a detail.",
		len(paths), textutil.Truncate(strings.Join(paths, ", "), maxPlanEcho))
}

// FilePlan is the file_plan tool's hand-over: the complete plan json of
// the planner session that runs, checked at once under the version in
// force. A refusal carries every finding back into the session; a valid
// plan is stored as the task's next revision and held as the session's
// answer, which RunPlan returns when the session ends.
func (r *Runner) FilePlan(ctx context.Context, taskID, plan string) (PlanResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	if t.Status != "running" {
		return PlanResult{Findings: []string{"plan: the task is " + t.Status + ", not running"}}, nil
	}
	if t.Track != policy.TrackPlan {
		return PlanResult{Findings: []string{"plan: the task is not on the plan track"}}, nil
	}
	v := r.cfg.Policy()
	shape, _, err := planShape(v, t)
	if err != nil {
		return PlanResult{}, err
	}
	p, found := readPlan("```json\n"+plan+"\n```", shape, v.Document.Limits, projectPins(v, t.Repository))
	if len(found) > 0 {
		return PlanResult{Findings: found}, nil
	}
	// the revision this one changes, when there is one: the amendment
	// rules judge the pair (piece 10), the replan rules the revision
	// against the plan's record of cards
	st, err := r.replanState(ctx, t)
	if err != nil {
		return PlanResult{}, err
	}
	if st.prev != nil {
		found = revisionFindings(*st.prev, p)
	}
	if found = append(found, replanFindings(st, p)...); len(found) > 0 {
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
	if st.prev != nil && n > st.revision {
		r.recordCriteria(ctx, taskID, n, *st.prev, p)
		r.restartChildren(ctx, t, repo, n, p)
	}
	r.record(ctx, taskID, "plan_revision", map[string]any{"revision": n, "cards": len(p.Cards), "sha256": sha})
	r.noteFiled(taskID, PlanResult{Revision: n, SHA256: sha})
	return PlanResult{Revision: n, SHA256: sha}, nil
}

// revisionFindings judges an amendment against the revision it changes
// (piece 10): an id never changes meaning, so a changed criterion files
// under a new id that names the old one in replaces. A removed
// criterion's citation goes with it; the coverage rules carry that.
func revisionFindings(old, next Plan) []string {
	prev := map[string]PlanCriterion{}
	for _, c := range old.Criteria {
		prev[c.ID] = c
	}
	var found []string
	for _, c := range next.Criteria {
		if o, ok := prev[c.ID]; ok && c != o {
			found = append(found, "criterion "+c.ID+": the previous revision holds this id with another definition; a changed criterion files under a new id")
		}
		if c.Replaces == "" {
			continue
		}
		if _, ok := prev[c.Replaces]; !ok {
			found = append(found, "criterion "+c.ID+": replaces "+c.Replaces+", an id the previous revision does not hold")
		}
	}
	return found
}

// recordCriteria writes the revision's records (the recording rule): one
// catch per criterion the revision adds, removes or revises under a new
// id, the churn join's old id, new id and time (piece 10).
func (r *Runner) recordCriteria(ctx context.Context, taskID string, revision int, old, next Plan) {
	details := criteriaDelta(old, next)
	if len(details) == 0 {
		return
	}
	if _, err := r.store.PutPlanCatches(ctx, taskID, revision, CatchCriterion, FinderPlanner, details); err != nil {
		log.Printf("plan %s revision %d: its criteria records: %v", taskID, revision, err)
	}
}

// criteriaDelta is what next does to old's criteria, sorted: "<id>
// added", "<id> replaces <old>" and "<id> removed".
func criteriaDelta(old, next Plan) []string {
	prev := map[string]bool{}
	for _, c := range old.Criteria {
		prev[c.ID] = true
	}
	held := map[string]bool{}
	for _, c := range next.Criteria {
		held[c.ID] = true
	}
	var details []string
	for _, c := range next.Criteria {
		switch {
		case prev[c.ID]:
		case c.Replaces != "":
			details = append(details, c.ID+" replaces "+c.Replaces)
		default:
			details = append(details, c.ID+" added")
		}
	}
	for id := range prev {
		if !held[id] && replacesNothing(next, id) {
			details = append(details, id+" removed")
		}
	}
	slices.Sort(details)
	return details
}

func replacesNothing(p Plan, id string) bool {
	for _, c := range p.Criteria {
		if c.Replaces == id {
			return false
		}
	}
	return true
}

// restartChildren runs the restart rules a new revision carries (piece
// 10): the checks re-ran with the filing, every card that never started
// takes the new revision's brief, image, rung and size, with the line on
// the old card's work it starts from kept, one the new plan drops ends
// closed with its issue unless it merged (Finish leaves a merged card to
// its chain, which ends it done), a started open card the new plan drops
// (dropped) stops as for a replan, its work kept on its branch, a started
// card it keeps runs on, and a final card never reopens.
func (r *Runner) restartChildren(ctx context.Context, t Task, repo Repo, revision int, p Plan) {
	cards, err := r.store.PlanCards(ctx, t.ID)
	if err != nil {
		log.Printf("plan %s revision %d: restart its cards: %v", t.ID, revision, err)
		return
	}
	fork := r.forgejo.Fork(repo)
	byRef := map[string]Task{fromRef(t): t}
	for _, k := range cards {
		byRef[fromRef(k)] = k
	}
	revisions := map[int]*Plan{}
	edges := planEdges(p)
	order := planOrder(p, edges)
	place := map[string]int{}
	card := map[string]PlanCard{}
	for i, c := range order {
		place[c.Key] = i + 1
		card[c.Key] = c
	}
	issue := map[string]int64{}
	for _, k := range cards {
		issue[k.PlanKey] = k.IssueNumber
	}
	of := fmt.Sprintf("plan #%d", t.IssueNumber)
	for _, k := range cards {
		if Final(k.Status) {
			continue
		}
		c, ok := card[k.PlanKey]
		if k.PlanRevision > 0 {
			if drop, err := r.dropped(ctx, t.ID, k, p, revisions); err != nil {
				log.Printf("card %s: the plan's revision %d: %v", k.ID, revision, err)
			} else if drop {
				if _, err := r.stopForReplan(ctx, k, t, droppedReason(revision)); err != nil {
					log.Printf("card %s: the plan's revision %d drops it: %v", k.ID, revision, err)
				}
			}
			continue
		}
		if !ok {
			if err := r.Finish(ctx, k.ID, "closed", fmt.Sprintf("the plan's revision %d drops the card %s", revision, k.PlanKey)); err != nil {
				log.Printf("card %s: the plan's revision %d drops it: %v", k.ID, revision, err)
			}
			continue
		}
		var deps []Dependency
		for _, d := range dependsOn(order, edges, c.Key) {
			deps = append(deps, Dependency{Issue: issue[d], Key: d})
		}
		spec := ""
		if c.Role == RoleIntegration {
			spec = specBlock(p)
		}
		// the card's own branch is the old card's work it was filed on; a
		// read that fails rebriefs it without the line
		start := ""
		if from, named := byRef[c.From]; c.From != "" && named {
			switch _, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(k)); {
			case errors.Is(err, ErrNotFound):
				start = goneLine(from)
			case err != nil:
				log.Printf("card %s: the plan's revision %d rebriefs it without its start line: %v", k.ID, revision, err)
			default:
				start = workLine(from)
			}
		}
		body := r.untrusted(t, cardBody(c, of, place[c.Key], len(order), k.Author, deps, spec, start))
		if err := r.store.UpdatePlanCardBrief(ctx, k.ID, Task{Objective: r.untrusted(t, c.Title) + "\n\n" + body, Image: c.Image, Rung: c.Rung, Size: c.Size}); err != nil {
			log.Printf("card %s: the plan's revision %d rebriefs it: %v", k.ID, revision, err)
		}
	}
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
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

// RequestPlan asks revision's questions of plan task taskID (the newest
// revision for 0; none yet answers PlanRef{}). A revision that asks none
// files its cards (fileRevision) and the plan card ends done in the same
// step; one whose questions all carry their answers goes back to the
// planner; one with open questions posts the plan and waits for the
// writers' answers in Needs you: the plan's request names the revision,
// and the task waits on a human, its work slot freed. pin is the version
// the cards are filed under (0: the one in force).
func (r *Runner) RequestPlan(ctx context.Context, taskID string, revision int, pin int64) (PlanRef, error) {
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
	// the round's hold-outs are drafted before a question is stored, a
	// review model call and dry runs of minutes: the questions, the
	// confirmation and the zone then reach Needs you as the plan waits
	if confirm, err := r.confirms(ctx, t, p); err != nil {
		return PlanRef{}, err
	} else if confirm {
		if err := r.askHoldouts(ctx, t, rev.Revision, p); err != nil {
			return PlanRef{}, err
		}
	}
	if err := r.askQuestions(ctx, t, rev.Revision, p.Questions); err != nil {
		return PlanRef{}, err
	}
	if err := r.askConfirmation(ctx, t, rev.Revision, p); err != nil {
		return PlanRef{}, err
	}
	ref := PlanRef{Revision: rev.Revision, SHA256: rev.SHA256}
	asked, err := r.store.PlanQuestions(ctx, taskID, rev.Revision)
	if err != nil {
		return PlanRef{}, err
	}
	open, err := r.store.OpenPlanQuestions(ctx, taskID, rev.Revision)
	switch {
	case err != nil:
		return PlanRef{}, err
	case len(asked) == 0:
		v, err := r.pinnedVersion(ctx, pin)
		if err != nil {
			return PlanRef{}, err
		}
		res, err := r.fileRevision(ctx, t, repo, v, rev)
		ref.Filed, ref.Refused = res.Issues, res.Refused
		return ref, err
	case open == 0:
		ref.Answered = true
		return ref, nil
	}
	if ref.Comment, err = r.postPlan(ctx, t, repo, rev.Revision, p, nil); err != nil {
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
			Reason: fmt.Sprintf("plan revision %d of issue #%d, %d cards, waits for %d answers", rev.Revision, t.IssueNumber, len(p.Cards), open)})
		if err != nil {
			return PlanRef{}, err
		}
		r.record(ctx, taskID, "approval_requested", map[string]any{"approval": id, "revision": rev.Revision, "sha256": rev.SHA256})
		r.timeline.RecordRow(ctx, taskID, EventApprovalRequested, FactoryDecider, ViaFactoryd,
			map[string]any{"approval": id, "kind": ApprovalPlan, "revision": rev.Revision, "cards": len(p.Cards)})
	}
	if t.Status != "waiting-human" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"queued", "running"}, "waiting-human"); err != nil {
			return PlanRef{}, terminalError("task %s is %s, cannot wait for its answers: %v", taskID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
		pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	return ref, nil
}

// askQuestions stores revision's questions of plan task t, sanitised as
// the plan's comment is (a clicked choice is posted as the member's
// comment), records one question_asked row each when it stored them, and
// writes them to the recording rule's ledger as the planner's catches: a
// self-attack question's catch carries the attack's kind (piece 5).
func (r *Runner) askQuestions(ctx context.Context, t Task, revision int, qs []PlanQuestion) error {
	if len(qs) == 0 {
		return nil
	}
	asked := make([]PlanQuestion, len(qs))
	for i, q := range qs {
		asked[i] = PlanQuestion{Text: r.untrusted(t, q.Text), Choices: make([]string, len(q.Choices)), Origin: q.Origin}
		for j, c := range q.Choices {
			asked[i].Choices[j] = r.untrusted(t, c)
		}
	}
	if inserted, err := r.store.PutPlanQuestions(ctx, t.ID, revision, asked); err != nil || !inserted {
		return err
	}
	var plain, attacks []string
	for _, q := range asked {
		if q.Origin == CatchSelfAttack {
			attacks = append(attacks, q.Text)
		} else {
			plain = append(plain, q.Text)
		}
	}
	if len(plain) > 0 {
		if _, err := r.store.PutPlanCatches(ctx, t.ID, revision, CatchQuestion, FinderPlanner, plain); err != nil {
			return err
		}
	}
	if len(attacks) > 0 {
		if _, err := r.store.PutPlanCatches(ctx, t.ID, revision, CatchSelfAttack, FinderPlanner, attacks); err != nil {
			return err
		}
	}
	for i, q := range asked {
		r.timeline.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd,
			map[string]any{"question": q.Text, "choices": q.Choices, "revision": revision, "index": i + 1})
	}
	return nil
}

// askConfirmation appends the confirmation question to revision's stored
// questions (the spec-intake concept, piece 5): the spec and the criteria
// shown to the members in Needs you, an okay fixes the criteria, an
// answer with changes carries them to the next planner round, a comment
// on the issue starts one too. Asked until answered once, and again for a
// revision that changes the spec or a criterion (confirms). A plan
// without criteria carries no spec to confirm.
func (r *Runner) askConfirmation(ctx context.Context, t Task, revision int, p Plan) error {
	if confirm, err := r.confirms(ctx, t, p); err != nil || !confirm {
		return err
	}
	q := PlanQuestion{Text: r.untrusted(t, confirmationQuestion(revision, p)), Choices: []string{"okay"}}
	idx, inserted, err := r.store.PutPlanConfirmation(ctx, t.ID, revision, q)
	if err != nil || !inserted {
		return err
	}
	r.timeline.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd,
		map[string]any{"question": q.Text, "choices": q.Choices, "revision": revision, "index": idx, "kind": "confirmation"})
	return nil
}

// confirms reports whether plan p of task t asks the confirmation: it
// carries criteria, and no revision's confirmation was answered yet or p
// changes the spec or a criterion of the newest revision whose
// confirmation was.
func (r *Runner) confirms(ctx context.Context, t Task, p Plan) (bool, error) {
	if len(p.Criteria) == 0 {
		return false, nil
	}
	confirmed, answered, err := r.store.PlanConfirmationAnswered(ctx, t.ID)
	if err != nil || !answered || confirmed < 1 {
		return err == nil && !answered, err
	}
	prev, _, err := r.planAt(ctx, t.ID, confirmed)
	return err == nil && prev != nil && specChanged(*prev, p), err
}

// specChanged tells whether next changes prev's spec, its outcome or its
// glossary, or a criterion, compared by value: revisions apart may hold
// one id with two definitions.
func specChanged(prev, next Plan) bool {
	a, b := cmp.Or(prev.Spec, &Spec{}), cmp.Or(next.Spec, &Spec{})
	if a.Outcome != b.Outcome || !slices.Equal(a.Glossary, b.Glossary) || len(prev.Criteria) != len(next.Criteria) {
		return true
	}
	held := map[string]PlanCriterion{}
	for _, c := range prev.Criteria {
		held[c.ID] = c
	}
	for _, c := range next.Criteria {
		if was, ok := held[c.ID]; !ok || was != c {
			return true
		}
	}
	return false
}

// confirmationQuestion is the confirmation question's text: what an
// answer does, the spec inline, the criteria by id and kind, and where
// the whole plan reads.
func confirmationQuestion(revision int, p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Confirmation of the spec and the criteria of plan revision %d: answer okay to confirm them, or answer with what changes; the criteria are fixed only when this is answered.",
		revision)
	if p.Spec != nil {
		if o := strings.TrimSpace(p.Spec.Outcome); o != "" {
			fmt.Fprintf(&b, "\nOutcome: %s", o)
		}
		if len(p.Spec.Glossary) > 0 {
			fmt.Fprintf(&b, "\nGlossary: %s", strings.Join(p.Spec.Glossary, "; "))
		}
	}
	ids := make([]string, len(p.Criteria))
	for i, c := range p.Criteria {
		ids[i] = c.ID + " (" + c.Kind + ")"
	}
	fmt.Fprintf(&b, "\nCriteria: %s.\nThe whole plan is in the plan's comment on this issue.", strings.Join(ids, ", "))
	return b.String()
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

// fileRevision files the cards of plan task t's revision rev, which asks
// no question, under version v, once however often it runs: it judges the revision again, the
// deploy owner its spec needs included (a finding refuses the filing, the
// planner gets it); files each card in topological order on the direct
// track at main's head, with the card's author as the cards' (plans do not
// nest: the children are direct cards); records the edges in Forgejo and
// task_deps; puts the issues into the plan's comment; and, for a replan,
// records it, which ends the replan.
func (r *Runner) fileRevision(ctx context.Context, t Task, repo Repo, v policy.Version, rev PlanRevision) (ChildrenResult, error) {
	revision := rev.Revision
	shape, process, err := planShape(v, t)
	if err != nil {
		return ChildrenResult{}, err
	}
	p, findings := readPlan("```json\n"+string(rev.Plan)+"\n```", shape, v.Document.Limits, projectPins(r.cfg.Policy(), t.Repository))
	if needed, owner := deployOwner(t.Objective, p); needed && owner == "" {
		findings = append(findings, ErrNoDeployOwner.Error())
	}
	if len(findings) > 0 {
		note := fmt.Sprintf("revision %d files no cards: %s", revision, strings.Join(findings, "; "))
		r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: "+r.untrusted(t, note))
		r.record(ctx, t.ID, "plan_refused", map[string]any{"revision": revision, "findings": findings})
		return ChildrenResult{Refused: findings}, nil
	}
	sha, err := r.forgejo.DefaultBranchHeadSHA(ctx, repo.Owner, repo.Name, "")
	if err != nil {
		return ChildrenResult{}, err
	}
	step, pending, err := r.pendingReplan(ctx, t)
	if err != nil {
		return ChildrenResult{}, err
	}
	old, err := r.oldCardsOf(ctx, t, step, pending)
	if err != nil {
		return ChildrenResult{}, err
	}
	author := cmp.Or(t.Author, r.forgejo.BotUser)
	cards, err := r.fileChildren(ctx, t, repo, v, process, fmt.Sprintf("plan #%d", t.IssueNumber), p, author, t.AuthorWriter, sha, old)
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
	r.record(ctx, t.ID, "children_created", map[string]any{"revision": revision, "by": author, "issues": issues})
	if pending {
		if err := r.recordReplan(ctx, t, repo, revision, p, step, byKey); err != nil {
			return ChildrenResult{}, err
		}
	}
	return ChildrenResult{Issues: issues}, nil
}

// fileChildren files p's cards of task t, its process's, the cards of
// of (plan #n), in topological order as task rows queued on the direct
// track at base sha and bot issues found by their markers or filed,
// records the edges in Forgejo and task_deps, then starts each under the
// work bound, where a card waits for the cards it depends on (plans do not
// nest: a plan's card is never a plan). approver authors the cards, with
// writer as its standing: a plan's are its card's, whose spec an outside
// account may have written; the cards in filing order. A card whose from
// names one of old, the plan's record (oldCards), starts from its work,
// unless a row of its key started or ended: it files at the old card's
// base, and its branch is the old card's before its issue exists; an old
// card whose branch is gone leaves it to start from main.
func (r *Runner) fileChildren(ctx context.Context, t Task, repo Repo, v policy.Version, process, of string, p Plan, approver string, writer bool, sha string, old []Task) ([]Task, error) {
	edges := planEdges(p)
	order := planOrder(p, edges)
	_, project, _ := v.ProjectOf(t.Repository)
	direct, ok := v.Processes[process].Tracks.Track(policy.TrackDirect)
	if !ok || direct.Class == "" {
		return nil, terminalError("task %s: process %q has no direct track in configuration version %d", t.ID, process, v.ID)
	}
	byRef, rows := map[string]Task{}, map[string]Task{}
	for _, k := range old {
		byRef[fromRef(k)] = k
		if k.PlanKey != "" {
			rows[k.PlanKey] = k
		}
	}
	fork := r.forgejo.Fork(repo)
	cards := map[string]Task{}
	var open []webhookIssue
	for i, c := range order {
		var deps []Dependency
		for _, d := range dependsOn(order, edges, c.Key) {
			deps = append(deps, Dependency{TaskID: cards[d].ID, Issue: cards[d].IssueNumber, Key: d})
		}
		title := r.untrusted(t, c.Title)
		spec := ""
		if c.Role == RoleIntegration {
			spec = specBlock(p)
		}
		// a row of the key that started or ended runs on its own work; one
		// that never started is copied to again, which keeps a branch a
		// filing already put there
		base, start := sha, ""
		from, continues := byRef[c.From]
		if row, filed := rows[c.Key]; c.From == "" || filed && (row.PlanRevision > 0 || Final(row.Status)) {
			continues = false
		}
		if continues {
			switch _, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(from)); {
			case errors.Is(err, ErrNotFound):
				start, continues = goneLine(from), false
			case err != nil:
				return nil, fmt.Errorf("branch head of %s: %w", from.ID, err)
			default:
				base, start = from.SHA, workLine(from)
			}
		}
		body := r.untrusted(t, cardBody(c, of, i+1, len(order), approver, deps, spec, start))
		card, err := r.store.CreatePlanCard(ctx, Task{ID: NewTaskID(), Priority: t.Priority, RiskClass: "standard",
			BudgetEUR: policy.CardBudget(v.Document.Budget, project.Budget, c.Size, c.Rung == RungFrontier, 0), Status: "queued",
			Track: policy.TrackDirect, RequiredCapabilities: []string{direct.Class}, SHA: base, Process: process,
			Objective: title + "\n\n" + body, Repository: t.Repository, Kind: "agent", Source: SourcePlan, Parent: t.ID, PlanKey: c.Key,
			Rung: c.Rung, Size: c.Size, Image: cardImage(c.Image), Author: approver, AuthorWriter: writer})
		if err != nil {
			return nil, err
		}
		// the branch is in place before the issue, whose label event can
		// start the card
		if continues {
			if _, err := r.ContinueBranch(ctx, from, card); err != nil {
				return nil, fmt.Errorf("card %s: continue the branch of %s: %w", c.Key, from.ID, err)
			}
		}
		// the card's dependencies are stored before its issue exists: the
		// issue's label event can start it, and the start must find them
		if len(deps) > 0 {
			ids := make([]string, len(deps))
			for j, d := range deps {
				ids[j] = d.TaskID
			}
			if err := r.store.AddTaskDeps(ctx, card.ID, ids); err != nil {
				return nil, err
			}
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
			if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, card.IssueNumber, []string{LabelAgent, "state/queued"}); err != nil {
				return nil, err
			}
			if err := r.store.SetTaskIssue(ctx, card.ID, card.IssueNumber); err != nil {
				return nil, err
			}
			r.timeline.recordEvent(ctx, card.ID, EventCardCreated, map[string]any{"by": approver, "issue": card.IssueNumber, "plan": t.ID, "track": policy.TrackDirect})
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
		for _, d := range dependsOn(order, edges, c.Key) {
			if dep := cards[d]; !slices.Contains(have, dep.IssueNumber) {
				if err := r.forgejo.AddIssueDependency(ctx, repo, child.IssueNumber, dep.IssueNumber); err != nil {
					return nil, err
				}
			}
		}
	}
	out := make([]Task, len(order))
	for i, c := range order {
		out[i] = cards[c.Key]
		if out[i].Status != "queued" {
			continue // a card an earlier filing of the plan left
		}
		if err := r.cfg.Workflows.Start(ctx, out[i].ID); err != nil {
			// the heal pass's pull starts it
			log.Printf("task %s: start card %s of %s: %v", t.ID, out[i].ID, of, err)
		}
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

// filedPlan is plan task parent's newest plan revision, the one that filed
// its cards, as stored;
// child names the card the revision's findings name. A read that fails is
// a terminal one.
func (r *Runner) filedPlan(ctx context.Context, parent, child string) (Plan, error) {
	rev, ok, err := r.store.PlanRevision(ctx, parent, 0)
	if err != nil || !ok {
		return Plan{}, cmp.Or(err, error(terminalError("plan %s of card %s has no revision", parent, child)))
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return Plan{}, terminalError("plan %s revision %d: %v", parent, rev.Revision, err)
	}
	return p, nil
}

// planOf is plan card t's section "The spec and the plan" (its plan's
// issue text and the plan's summary), its role and its plan row's
// directories; "" and none for a task no plan filed.
func (r *Runner) planOf(ctx context.Context, t Task) (spec, role string, dirs []string, err error) {
	if t.Source != SourcePlan || t.Parent == "" {
		return "", "", nil, nil
	}
	plan, _, err := r.task(ctx, t.Parent)
	if err != nil {
		return "", "", nil, err
	}
	p, err := r.filedPlan(ctx, t.Parent, t.ID)
	if err != nil {
		return "", "", nil, err
	}
	role = RoleFeature
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == t.PlanKey }); i >= 0 {
		role, dirs = p.Cards[i].Role, p.Cards[i].Directories
	}
	spec = plan.Objective + "\n\nThe plan's summary: " + strings.TrimSpace(p.Summary)
	return spec, role, dirs, nil
}
