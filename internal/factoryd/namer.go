package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"log"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Plan names: every plan carries a short name a person reads at a
// glance — the child card's plan chip, the approval header, the find
// box — written when the plan's first revision files. The small model
// proposes; code decides. The answer is normalized (one line, 3 to 24
// runes, single spaces) and checked against the project's open plans'
// names; a bad answer is asked again, at most three attempts, each
// carrying what the last one got wrong. When no answer holds, the
// title's slug names the plan, numbered past a collision. Naming never
// fails and never blocks a filing: every error, refusal or bad answer
// ends in the slug, and a name is cosmetic.

// planNameMin and planNameMax bound a name after normalization: three
// runes at least, twenty-four at most — a chip, not a sentence.
const (
	planNameMin = 3
	planNameMax = 24
)

// nameAttempts bounds the naming asks: past the third bad answer the
// slug names the plan.
const nameAttempts = 3

// nameFactsBytes bounds each fact a naming ask carries.
const nameFactsBytes = 600

// namerPrompt is the naming ask's system prompt: the answer must work
// as a plan's name on a card, one JSON field like every supervisor
// answer.
const namerPrompt = "You name build plans. Answer with a JSON object with one field \"name\": short plain words that say what the plan does, fixes or enables, at most 24 characters, one line, no trailing period."

// NameStore is what the namer reads and writes.
type NameStore interface {
	// OpenPlanNames are the names of the project's open plans, every
	// one but the plan being named: what a new name must not repeat.
	OpenPlanNames(ctx context.Context, projectID, excludeID string) ([]string, error)
	// SetPlanName records the plan's short name.
	SetPlanName(ctx context.Context, id, name string) error
}

// Namer names a project's plans.
type Namer struct {
	Store   NameStore
	Model   *SupervisorModel
	Current func() policy.Version
}

// Name names t's plan and records the name. The small model proposes,
// at most three asks, each told what the last answer got wrong; a
// failed ask is not retried — a filing never waits on a broken gateway
// — and whenever no answer held, the title's slug names the plan. The
// return is the name: naming never fails.
func (n *Namer) Name(ctx context.Context, t Task, title, outcome, summary string) string {
	taken, err := n.Store.OpenPlanNames(ctx, t.Repository, t.ID)
	if err != nil {
		log.Printf("names: open plans of %s: %v", t.Repository, err)
		taken = nil
	}
	v := n.Current()
	// the policy names the title alias for chat threads only; a plan
	// names through the same small tier
	alias := cmp.Or(v.Document.FactoryModel(t.RequiredCapabilities, policy.PurposeTitle), policy.Small)
	for attempt := 1; attempt <= nameAttempts; attempt++ {
		answer, _, err := n.Model.Chat(ctx, t.ID, alias, policy.PurposeTitle,
			v.Document.Limits.SupervisorTimeout, namerPrompt, nameFacts(title, outcome, summary, taken, attempt))
		if err != nil {
			break
		}
		if name, ok := normalizePlanName(nameAnswer(answer)); ok && !nameTaken(name, taken) {
			n.record(ctx, t, name)
			return name
		}
	}
	slug := planSlug(title, taken)
	n.record(ctx, t, slug)
	return slug
}

// record stores the plan's name; a store that refuses it leaves the
// plan nameless for the next sweep, never an error for the filing.
func (n *Namer) record(ctx context.Context, t Task, name string) {
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

// nameTaken is whether name, case-insensitively, is one of taken.
func nameTaken(name string, taken []string) bool {
	return slices.ContainsFunc(taken, func(o string) bool { return strings.EqualFold(name, o) })
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

// planSlug is title's deterministic name, the fallback that never
// fails: lowercased, runs of non-alphanumerics one dash, cut at a word
// boundary inside 24 runes, and numbered past every name another open
// plan holds — the number's room cut out of the base, so a suffixed
// name still fits the bound. A title with nothing to say says "plan".
func planSlug(title string, taken []string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	base := cutSlug(strings.Trim(b.String(), "-"), planNameMax)
	if base == "" {
		base = "plan"
	}
	slug := base
	for i := 2; nameTaken(slug, taken) && i <= len(taken)+2; i++ {
		suffix := "-" + strconv.Itoa(i)
		slug = cutSlug(base, planNameMax-len(suffix)) + suffix
	}
	return slug
}

// cutSlug is s at most max runes, cut at a word boundary when one lies
// at or inside the cut, hard-cut when none does.
func cutSlug(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := max
	if runes[cut] != '-' {
		for i := cut - 1; i > 0; i-- {
			if runes[i] == '-' {
				cut = i
				break
			}
		}
	}
	return strings.Trim(string(runes[:cut]), "-")
}
