package factoryd

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// Plan names: every plan carries a short name a person reads at a
// glance — the child card's plan chip, the approval header, the find
// box — written when the plan's first revision files. The small model
// proposes; code decides. The answer is normalized (one line, 3 to 24
// runes, single spaces) and checked against the project's open plans'
// names; a bad answer is asked again, at most three attempts, each
// carrying what the last one got wrong. When no answer holds, the plan
// stays unnamed and shows its title, and the next start names it again.
// Naming never fails and never blocks a filing.

// planNameMin and planNameMax bound a name after normalization: three
// runes at least, twenty-four at most — a chip, not a sentence.
const (
	planNameMin = 3
	planNameMax = 24
)

// nameAttempts bounds the naming asks: past the third bad answer the
// plan stays unnamed.
const nameAttempts = 3

// recordWindow bounds the write of a name.
const recordWindow = 10 * time.Second

// nameFactsBytes bounds each fact a naming ask carries.
const nameFactsBytes = 600

// namerPrompt is the naming ask's system prompt: the answer must work
// as a plan's name on a card, one JSON field like every supervisor
// answer.
var namerPrompt = "You name build plans. Answer with a JSON object with one field \"name\": short plain words that say what the plan does, fixes or enables, at most 24 characters, one line, no trailing period.\n\n" + writingguide.For(writingguide.JobTitle)

// NameStore is what the namer reads and writes.
type NameStore interface {
	// OpenPlanNames are the names of the project's open plans, every
	// one but the plan being named: what a new name must not repeat.
	OpenPlanNames(ctx context.Context, projectID, excludeID string) ([]string, error)
	// SetPlanName records the plan's short name.
	SetPlanName(ctx context.Context, id, name string) error
	// UnnamedOpenPlans are the open plans that hold no name yet.
	UnnamedOpenPlans(ctx context.Context) ([]Task, error)
	// PlanRevision reads revision n of plan id, the newest for 0.
	PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error)
}

// Namer names a project's plans.
type Namer struct {
	Store   NameStore
	Model   *SupervisorModel
	Current func() policy.Version
}

// Name names t's plan and records the name. The small model proposes,
// at most three asks, each told what the last answer got wrong; a
// failed ask is not retried, as a filing never waits on a broken
// gateway. The return is the name, "" when no answer held.
func (n *Namer) Name(ctx context.Context, t Task, title, outcome, summary string) string {
	taken, err := n.Store.OpenPlanNames(ctx, t.Repository, t.ID)
	if err != nil {
		log.Printf("names: open plans of %s: %v", t.Repository, err)
		taken = nil
	}
	v := n.Current()
	alias := v.Document.FactoryModel(t.RequiredCapabilities, policy.PurposeTitle)
	for attempt := 1; attempt <= nameAttempts; attempt++ {
		answer, _, err := n.Model.Chat(ctx, t.ID, alias, policy.PurposeTitle,
			v.Document.Limits.SupervisorTimeout, namerPrompt, nameFacts(title, outcome, summary, taken, attempt))
		if err != nil {
			break
		}
		if name, ok := PlanName(nameAnswer(answer), taken); ok {
			n.record(ctx, t, name)
			return name
		}
	}
	return ""
}

// NameOpenPlans names the open plans this build inherits, once at
// startup, from each plan's newest revision. A plan with no revision yet
// is skipped: it is named when its first revision files. A revision that
// does not read still names the plan, by its issue title alone. A store
// error stops the sweep and the next start retries. The return is the
// count named.
func (n *Namer) NameOpenPlans(ctx context.Context) int {
	plans, err := n.Store.UnnamedOpenPlans(ctx)
	if err != nil {
		log.Printf("names: open plans without a name: %v", err)
		return 0
	}
	named := 0
	defer func() { log.Printf("names: %d open plans named", named) }()
	for _, t := range plans {
		if ctx.Err() != nil {
			return named
		}
		rev, filed, err := n.Store.PlanRevision(ctx, t.ID, 0)
		if err != nil {
			log.Printf("names: revision of %s: %v", t.ID, err)
			return named
		}
		if !filed {
			continue
		}
		var p Plan
		var outcome string
		if json.Unmarshal(rev.Plan, &p) == nil && p.Spec != nil {
			outcome = p.Spec.Outcome
		}
		if n.Name(ctx, t, CardTitle(t), outcome, p.Summary) != "" {
			named++
		}
	}
	return named
}

// record stores the plan's name; a store that refuses it leaves the
// plan nameless for the next sweep, never an error for the filing.
func (n *Namer) record(ctx context.Context, t Task, name string) {
	// the write has a window of its own: the asks may have spent the
	// caller's whole budget
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordWindow)
	defer cancel()
	if err := n.Store.SetPlanName(ctx, t.ID, name); err != nil {
		log.Printf("names: plan %s of %s: %v", t.ID, t.Repository, err)
	}
}

// nameFacts is the naming ask's user message: the facts that name the
// plan, and from the second ask what the last answer got wrong.
func nameFacts(title, outcome, summary string, taken []string, attempt int) string {
	var b strings.Builder
	b.WriteString("Issue title: " + textutil.Truncate(title, nameFactsBytes))
	b.WriteString("\nSpec outcome: " + textutil.Truncate(outcome, nameFactsBytes))
	b.WriteString("\nSummary: " + textutil.Truncate(summary, nameFactsBytes))
	if attempt > 1 {
		b.WriteString("\nShorter: at most 24 characters.")
	}
	if len(taken) > 0 {
		b.WriteString("\nAlready taken by other open plans: " + strings.Join(taken, ", "))
	}
	return b.String()
}

// nameAnswer is the model's answer as the proposed name: one JSON
// object with one field. Anything else — a bare string, a broken
// object, an empty name — is the empty proposal, which normalization
// refuses like any invalid answer.
func nameAnswer(content string) string {
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(content), &a) != nil {
		return ""
	}
	return a.Name
}

// PlanName is s as a plan's name and whether it holds: normalized
// (normalizePlanName), and, case-insensitively, none of taken, the
// names of the project's other open plans.
func PlanName(s string, taken []string) (string, bool) {
	name, ok := normalizePlanName(s)
	return name, ok && !slices.ContainsFunc(taken, func(o string) bool { return strings.EqualFold(name, o) })
}

// normalizePlanName is a model answer as a plan name: one line of
// single-spaced words, 3 to 24 runes. A control or format character —
// a newline, a bidi override among them — refuses the answer outright:
// shrinking never cuts meaning into gibberish.
func normalizePlanName(s string) (string, bool) {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if n := len([]rune(s)); n < planNameMin || n > planNameMax {
		return "", false
	}
	return s, true
}
