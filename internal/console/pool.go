package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// poolLine is the project face's pool status: one projection of the loaded
// policy's discretionary pool and the month's agent-funded usage against it
// — the same figure FundCard guards the pool with (AgentFundedEUR), never a
// second aggregate and never the ProjectMonthSpend total. Present: a pool
// stands (discretionary_eur above 0). Readable: the usage could be read; an
// unreadable one is never zero. Ready: the loaded policy's actual rules —
// autonomy, the resolved confirm list, raise_max_eur and money left —
// already permit an unattended raise; no display action changes them.
type poolLine struct {
	Present, Readable, Ready             bool
	ConfiguredEUR, UsedEUR, RemainingEUR float64
	// Month is the UTC month the usage counts ("2026-11") and Policy the
	// loaded policy's identity, the version and commit whose file holds
	// the pool.
	Month, Policy string
	Words         string
}

// projectPool is p's pool line from v, the policy in force, at now: the
// configured, used and remaining EUR — usage is kept whole when a pool is
// lowered below it, only the remaining clamps to zero — with the words that
// distinguish an absent or disabled pool, an exhausted or lowered one, a
// missing per-raise guard, a fund the member confirms (their money, which
// meets no pool bound) and an unreadable usage. A positive pool alone is
// never unattended authority.
func (c *Console) projectPool(ctx context.Context, v policy.Version, p factoryd.Project, now time.Time) poolLine {
	out := poolLine{Month: now.UTC().Format("2006-01"),
		Policy: fmt.Sprintf("policy version %d (commit %.7s)", v.ID, v.Commit)}
	pc, loaded := v.Tree.Projects[p.Name]
	if !loaded || pc.Budget == nil || factoryd.Cents(pc.Budget.DiscretionaryEUR) <= 0 {
		out.Words = "no pool: discretionary_eur is absent or 0, every agent raise waits for a member"
		return out
	}
	b := *pc.Budget
	out.Present, out.ConfiguredEUR = true, b.DiscretionaryEUR
	used, err := c.cfg.Chat.AgentFundedEUR(ctx, p.Repository, now)
	if err != nil {
		out.Words = fmt.Sprintf("the pool stands at €%.2f but its usage could not be read: how much is left is unknown, and a standing pool is not unattended authority", b.DiscretionaryEUR)
		return out
	}
	out.UsedEUR, out.Readable, out.RemainingEUR = used, true, max(b.DiscretionaryEUR-used, 0)
	confirmed := slices.Contains(v.Document.ChatOf(pc).Confirm, "fund")
	guarded := confirmed || factoryd.Cents(b.RaiseMaxEUR) > 0
	switch {
	case factoryd.Cents(used) > factoryd.Cents(b.DiscretionaryEUR):
		out.Words = fmt.Sprintf("€%.2f funded this UTC month of a pool now at €%.2f: usage stands and nothing is left until the pool is raised or the month turns", used, b.DiscretionaryEUR)
	case factoryd.Cents(used) == factoryd.Cents(b.DiscretionaryEUR):
		out.Words = fmt.Sprintf("the pool is spent: €%.2f of €%.2f funded this UTC month, €0.00 left until the pool is raised or the month turns", used, b.DiscretionaryEUR)
	default:
		out.Words = fmt.Sprintf("€%.2f of €%.2f funded this UTC month, €%.2f left", used, b.DiscretionaryEUR, out.RemainingEUR)
		if guarded {
			out.Words += fmt.Sprintf(", each raise at most €%.2f", b.RaiseMaxEUR)
		}
	}
	switch {
	case !guarded:
		out.Words += "; raise_max_eur is absent or 0: every agent raise refuses, so the pool funds nothing until it is set"
	case confirmed:
		out.Words += "; fund is confirmed at this autonomy: a raise is the member's money and meets no pool bound"
	case factoryd.Cents(out.RemainingEUR) > 0:
		out.Ready = true
		out.Words += "; the loaded policy permits an unattended raise from it"
	}
	return out
}

// poolChat is the pool line a chat page read shows: the loaded policy's
// identity and the month with the same words the settings, policy and
// project pages read — one projection through them all.
func (c *Console) poolChat(ctx context.Context, v policy.Version, p factoryd.Project) string {
	pl := c.projectPool(ctx, v, p, time.Now())
	return fmt.Sprintf("Pool (%s, %s UTC): %s", pl.Policy, pl.Month, pl.Words)
}

// The month pool's write core (roadmap 308): the set_pool change the
// proposal routes confirm writes budget.discretionary_eur of the
// project's file into the configuration repository's main, one commit,
// through the same per-repository lock as every project-file write, and
// records the pool_changed row that names the member who confirmed it.
// The proposal routes capture the file the change read; this core guards
// the commit on that source, so a top-up is never added to a pool the
// proposal never saw.

// The pool change's actions: set makes the pool the amount, top_up grows
// it by the amount.
const (
	poolSet   = "set"
	poolTopUp = "top_up"
)

// poolMaxCents is the most a pool, or a pool change's amount, holds in
// cents. At and under it every sum of two pools fits an int64 with room
// and every cents value reads back exact as a float64, so the write path
// carries whole cents end to end — no float addition, no conversion that
// can overflow or round — and crosses into EUR only to show and load.
const poolMaxCents = 90_000_000_000_000 // EUR 900 000 000 000

// poolChange is one confirmed change of a project's month pool: the
// action, its amount in EUR (a set's target, a top-up's increment), the
// member's reason and the blob sha of the project file the proposal read
// when it captured the change. The member who confirmed it is not the
// change's to name: the actor is.
type poolChange struct {
	action string
	eur    float64
	reason string
	source string
}

// poolArgs is a set_pool call's arguments as a confirmed press runs them:
// the captured action and reason with the amount the member confirmed,
// theirs when they corrected it, in the tool's own order.
type poolArgs struct {
	Action string  `json:"action"`
	EUR    float64 `json:"eur"`
	Reason string  `json:"reason"`
}

// poolAmounts are the cents a pool edit found and wrote: the pool before
// the change and after it.
type poolAmounts struct{ from, to int64 }

// poolResult is what a landed pool change answers: the pool before and
// after it and the amount the member confirmed, in EUR, the pull request
// that landed it, and whether the policy in force already holds the
// after — a reload's acknowledgement is not activation, so the loaded
// policy is read.
type poolResult struct {
	from, confirmed, to float64
	pr                  int64
	loaded              bool
}

// poolEUR is cents as EUR.
func poolEUR(cents int64) float64 { return float64(cents) / 100 }

// doSetPool changes project name's month pool as the member a: the
// change's amount into budget.discretionary_eur of the project's file,
// one commit through commitProjectFile guarded by the source the
// proposal captured. The write asks the file for a budget block with an
// explicit month_eur and a raise_max_eur above 0 — the bounds the pool
// needs, which only a policy PR writes — and touches nothing else. The
// change records exactly one pool_changed row naming the member; a row
// that cannot be written after the merge answers a partial failure
// naming the pull request and the missing row, and the change is never
// repeated.
func (c *Console) doSetPool(ctx context.Context, name string, ch poolChange, a actor) (poolResult, error) {
	cents, err := poolCents(ch)
	if err != nil {
		return poolResult{}, err
	}
	path := policy.ProjectFile(name)
	repo := ""
	if v, ok := c.inForce(ctx); ok {
		if p, ok := v.Projects[name]; ok {
			repo = p.Repository
		}
	}
	if repo == "" {
		return poolResult{}, refuse(http.StatusConflict, "the policy in force holds no project "+name)
	}
	start := time.Now()
	amounts := poolAmounts{}
	pr, err := c.commitProjectFileGuarded(ctx, a, name, poolMessage(name, ch), ch.source, poolEdit(path, ch.action, cents, &amounts), nil)
	if err != nil {
		return poolResult{}, err
	}
	res := poolResult{from: poolEUR(amounts.from), confirmed: poolEUR(cents), to: poolEUR(amounts.to), pr: pr}
	if v, ok := c.inForce(ctx); ok {
		if p, ok := v.Projects[name]; ok && p.Budget != nil && factoryd.Cents(p.Budget.DiscretionaryEUR) == amounts.to {
			res.loaded = true
		}
	}
	detail := map[string]any{"action": ch.action, "from_eur": res.from, "confirmed_eur": res.confirmed, "to_eur": res.to,
		"reason": ch.reason, "pr": c.cfg.InfraRepo.String() + "#" + strconv.FormatInt(pr, 10)}
	if _, err := c.cfg.Events.Record(context.WithoutCancel(ctx), factoryd.Event{Repository: repo, Kind: factoryd.EventPoolChanged,
		Actor: a.s.user, Via: cmp.Or(a.via, factoryd.ViaChat), Detail: detail}); err != nil {
		return res, refuse(http.StatusInternalServerError, fmt.Sprintf(
			"The pool change is merged as %s#%d, but its pool_changed row could not be written: %v. The change stands: write the missing row (action %s, from_eur %.2f, confirmed_eur %.2f, to_eur %.2f, reason %q) and do not repeat the change.",
			c.cfg.InfraRepo, pr, err, ch.action, res.from, res.confirmed, res.to, ch.reason))
	}
	if !res.loaded {
		return res, refuse(http.StatusServiceUnavailable, fmt.Sprintf(
			"The pool change is merged as %s#%d, but the policy in force still holds the pool of %s at EUR %.2f: the loader picks the merge up on its next poll. The pool_changed row is written; the new ceiling counts only once the policy holds it.",
			c.cfg.InfraRepo, pr, name, res.from))
	}
	log.Printf("console: %s %s the pool of %s to EUR %.2f via %s (%s#%d) in %.1fs", a.s.user, ch.action, name, res.to, a.via, c.cfg.InfraRepo, pr, time.Since(start).Seconds())
	return res, nil
}

// poolCents is the change's amount as whole cents, refusing the amounts
// a pool cannot hold: an unknown action, a change without its reason or
// its source, a set below zero, a top-up at or below zero, a number that
// is not finite, an amount over the pool's bound, or one with a fraction
// below a cent.
func poolCents(ch poolChange) (int64, error) {
	switch {
	case ch.action != poolSet && ch.action != poolTopUp:
		return 0, refuse(http.StatusBadRequest, "the pool's action is "+poolSet+" or "+poolTopUp)
	case ch.reason == "":
		return 0, refuse(http.StatusBadRequest, "a pool change names its reason")
	case ch.source == "":
		return 0, refuse(http.StatusBadRequest, "a pool change names the source of the project file it read")
	case math.IsNaN(ch.eur) || math.IsInf(ch.eur, 0):
		return 0, refuse(http.StatusBadRequest, "a pool change's amount is a finite number in EUR")
	case ch.action == poolSet && ch.eur < 0:
		return 0, refuse(http.StatusBadRequest, "a set takes a non-negative amount in EUR")
	case ch.action == poolTopUp && ch.eur <= 0:
		return 0, refuse(http.StatusBadRequest, "a top-up takes a positive amount in EUR")
	case ch.eur*100 > poolMaxCents:
		return 0, refuse(http.StatusBadRequest, "a pool change's amount is too large")
	}
	cents := factoryd.Cents(ch.eur)
	if poolEUR(cents) != ch.eur {
		return 0, refuse(http.StatusBadRequest, "a pool change's amount is whole cents")
	}
	return cents, nil
}

// poolMessage is the pool change's commit message and pull request
// title.
func poolMessage(name string, ch poolChange) string {
	if ch.action == poolTopUp {
		return fmt.Sprintf("Project %s: month pool +%.2f EUR", name, ch.eur)
	}
	return fmt.Sprintf("Project %s: month pool %.2f EUR", name, ch.eur)
}

// capturePool is in's set_pool call captured as the proposal's own change:
// the project file read from infra's main with the blob sha its Do it
// guards on, the pool it holds with the bounds the write needs checked,
// and the call's amount judged by poolCents — all at the proposal, so the
// member reads the operation, the suggestion, the reason and the ceiling
// the change would stand at before anything writes. A top-up over the
// pool's bound refuses here with the write's own words.
func (t *turn) capturePool(in toolInput) error {
	path := policy.ProjectFile(t.p.Name)
	raw, blob, present, err := t.c.infraFile(t.r.Context(), path)
	switch {
	case err != nil:
		return err
	case !present:
		return refuse(http.StatusNotFound, "There is no file "+path+" on infra's main.")
	}
	cents, err := poolCents(poolChange{action: in.Action, eur: in.EUR, reason: in.Reason, source: blob})
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return refuse(http.StatusConflict, fmt.Sprintf("%s holds no YAML the pool can read: a policy PR corrects it", path))
	}
	from, err := poolBounds(&doc, path)
	if err != nil {
		return refuse(http.StatusConflict, err.Error())
	}
	if in.Action == poolTopUp && from+cents > poolMaxCents {
		return refuse(http.StatusBadRequest, fmt.Sprintf("a top-up over the pool of %s does not fit: propose a set of the target instead", path))
	}
	t.pool = &factoryd.PoolCapture{Action: in.Action, Reason: in.Reason, Source: blob, From: from, EUR: in.EUR}
	return nil
}

// poolPress is one confirmed pool press's typed outcome: the captured
// change ran once through the guarded writer, and it either landed — the
// result and the applied line the stream answers — or refused with the
// status and words the press answers: a stale source 409, a candidate
// the change check refuses 422, a missing prerequisite 409, a Forgejo
// refusal, the PR-bearing pending 503 or the repair 500. Never an SSE
// refusal text.
type poolPress struct {
	res   poolResult
	words string
	err   error
}

// readmit re-admits the human pressing a pool proposal through the
// tool's own access rule — approver over the chat's project — before
// anything writes: the authority the proposal-time admission granted may
// have been revoked since, and the press answers a real 403, never an
// SSE refusal.
func (t *turn) readmit(tool string) error {
	return t.admitTool(tool, toolInput{})
}

// poolPress is the confirmed pool press's one write: the captured change
// with the amount the member confirmed, theirs when they corrected it,
// through the guarded writer once — no second approval, no fresh source
// read, no replay, and no second call inside apply or runTool. The
// guarded writer's own guard answers the stale source; its refusals are
// the press's typed outcome.
func (t *turn) poolPress(p factoryd.ChatProposal, fix *poolCorrection) poolPress {
	eur := p.Pool.EUR
	if fix != nil {
		eur = fix.eur
	}
	ch := poolChange{action: p.Pool.Action, eur: eur, reason: p.Pool.Reason, source: p.Pool.Source}
	res, err := t.c.doSetPool(t.r.Context(), t.p.Name, ch, t.a)
	if err != nil {
		return poolPress{err: err}
	}
	return poolPress{res: res, words: fmt.Sprintf(
		"The pool of %s is €%.2f now, was €%.2f: %s#%d carries the change and the policy in force holds it.",
		t.p.Name, res.to, res.from, t.c.cfg.InfraRepo, res.pr)}
}

// landed streams a pool press's already-landed result: the tool block and
// the applied line of the write that happened before the stream opened,
// the row in the conversation naming the member who pressed, and done.
// Nothing writes here.
func (t *turn) landed(p factoryd.ChatProposal, fix *poolCorrection, pr poolPress) {
	t.a.decider = "member"
	start := time.Now()
	args := string(p.Args)
	if fix != nil {
		if b, err := json.Marshal(poolArgs{Action: p.Pool.Action, EUR: fix.eur, Reason: p.Pool.Reason}); err == nil {
			args = string(b)
		}
	}
	t.freshRead = time.Now()
	if _, ok := t.save(factoryd.ChatMessage{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{
		{ID: "do-" + p.Token[:12], Name: p.Tool, Arguments: args, Result: pr.words, At: start,
			Ms: time.Since(start).Milliseconds(), ReadAt: t.freshRead}}}); !ok {
		return
	}
	t.block(p.Tool, args, time.Since(start), pr.words)
	t.send("applied", map[string]string{"tool": p.Tool, "result": pr.words})
	t.send("done", struct{}{})
}

// poolDesc is the set_pool proposal's line for the member: the standing
// pool the proposal captured, the operation with its suggested amount,
// the reason, and the ceiling the change would stand at — what a Do it,
// as-is or with the amount corrected, writes.
func (t *turn) poolDesc(title string) string {
	cents := factoryd.Cents(t.pool.EUR)
	to := cents
	op := fmt.Sprintf("set it at %.2f EUR", t.pool.EUR)
	if t.pool.Action == poolTopUp {
		to += t.pool.From
		op = fmt.Sprintf("top it up by %.2f EUR", t.pool.EUR)
	}
	return fmt.Sprintf("%s, the pool stands at %.2f EUR, %s, reason %s, it would stand at %.2f EUR",
		title, poolEUR(t.pool.From), op, t.pool.Reason, poolEUR(to))
}

// poolEdit is the change of budget.discretionary_eur to cents: the bounds
// the file must hold checked by poolBounds, then the one key set, with
// nothing else — no other key, comment or cap — moving. The arithmetic is
// whole cents end to end; the file's own value is read as the exact
// decimal it holds and the new one written as the cents the member
// confirmed.
func poolEdit(path, action string, cents int64, amounts *poolAmounts) func(doc *yaml.Node) error {
	return func(doc *yaml.Node) error {
		from, err := poolBounds(doc, path)
		if err != nil {
			return err
		}
		to := cents
		if action == poolTopUp {
			if to += from; to > poolMaxCents {
				return fmt.Errorf("a top-up over the pool of %s does not fit: propose a set of the target instead", path)
			}
		}
		amounts.from, amounts.to = from, to
		b := doc.Content[0].Content[keyIndex(doc.Content[0], "budget")+1]
		setBudgetKey(b, "discretionary_eur", poolDecimal(to))
		return nil
	}
}

// poolBounds is the pool the file holds in cents once its bounds are
// checked: a budget block with an explicit month_eur key and a
// raise_max_eur above 0 — the bounds the pool needs, which only a policy
// PR writes — and a discretionary_eur the pool can take exactly, absent
// meaning zero. A document that parses to no mapping root — an empty
// file, one of whitespace, a scalar — holds no YAML the pool can read.
// An absent month_eur is not an intentional month of zero: the read
// refuses rather than read one, and a policy PR writes the cap. A budget
// or a pool the file shares through a YAML alias is never read: the
// edit would land everywhere the alias reads, so it refuses and a
// policy PR unshares the value first.
func poolBounds(doc *yaml.Node, path string) (int64, error) {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return 0, fmt.Errorf("%s holds no YAML the pool can read: a policy PR corrects it", path)
	}
	m := doc.Content[0]
	i := keyIndex(m, "budget")
	if i < 0 {
		return 0, fmt.Errorf("%s sets no budget block: a policy PR writes one, with month_eur and raise_max_eur, before a pool can change", path)
	}
	b := m.Content[i+1]
	shared := poolAliased(doc)
	if b.Kind == yaml.AliasNode || shared[b] {
		return 0, fmt.Errorf("%s aliases its budget or its pool: a policy PR removes the YAML alias before a pool change", path)
	}
	if b.Kind != yaml.MappingNode {
		return 0, fmt.Errorf("%s sets no budget block: a policy PR writes one, with month_eur and raise_max_eur, before a pool can change", path)
	}
	if keyIndex(b, "month_eur") < 0 {
		return 0, fmt.Errorf("%s sets no budget.month_eur: a policy PR writes the project's month cap first (an intentional month of zero writes month_eur: 0)", path)
	}
	raises := false
	if j := keyIndex(b, "raise_max_eur"); j >= 0 {
		if v, err := strconv.ParseFloat(b.Content[j+1].Value, 64); err == nil && v > 0 {
			raises = true
		}
	}
	if !raises {
		return 0, fmt.Errorf("%s sets no budget.raise_max_eur above 0: a policy PR writes the per-raise bound first (every raise of the pool would refuse)", path)
	}
	from := int64(0)
	if j := keyIndex(b, "discretionary_eur"); j >= 0 {
		v := b.Content[j+1]
		if v.Kind == yaml.AliasNode || shared[v] {
			return 0, fmt.Errorf("%s aliases its budget or its pool: a policy PR removes the YAML alias before a pool change", path)
		}
		c, ok := poolFileCents(v.Value)
		if !ok {
			return 0, fmt.Errorf("%s holds a discretionary_eur the pool cannot take: a policy PR corrects it", path)
		}
		from = c
	}
	return from, nil
}

// poolAliased is every node of doc another node aliases: an edit of a
// node in the set lands everywhere its alias reads, so the pool never
// edits through or into one.
func poolAliased(doc *yaml.Node) map[*yaml.Node]bool {
	shared := map[*yaml.Node]bool{}
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.AliasNode && n.Alias != nil {
			shared[n.Alias] = true
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(doc)
	return shared
}

// poolFileCents is a project file's pool scalar as whole cents: a plain
// decimal number of at most two places, non-negative and under the
// pool's bound. Anything else — a third place, an exponent, a negative,
// a value over the bound — is a value the pool cannot take exactly.
func poolFileCents(scalar string) (int64, bool) {
	whole, frac, _ := strings.Cut(scalar, ".")
	if len(frac) > 2 || !poolDigits(whole) || !poolDigits(frac) {
		return 0, false
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > poolMaxCents/100 {
		return 0, false
	}
	f := int64(0)
	switch len(frac) {
	case 1:
		f = int64(frac[0]-'0') * 10
	case 2:
		f = int64(frac[0]-'0')*10 + int64(frac[1]-'0')
	}
	if cents := w*100 + f; cents <= poolMaxCents {
		return cents, true
	}
	return 0, false
}

// poolDigits is s all decimal digits.
func poolDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// poolDecimal is cents as the file's plain decimal: whole euros without
// a point, a fraction always two places.
func poolDecimal(cents int64) string {
	if cents%100 == 0 {
		return strconv.FormatInt(cents/100, 10)
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// setBudgetKey sets key of mapping b to value, in its place with its
// comment kept when it is there, appended when it is not. The value's
// tag is the one it resolves to — an int for a whole euro, a float
// otherwise — so the file keeps writing the amount plainly.
func setBudgetKey(b *yaml.Node, key, value string) {
	tag := "!!float"
	if !strings.Contains(value, ".") {
		tag = "!!int"
	}
	if i := keyIndex(b, key); i >= 0 {
		b.Content[i+1].Value, b.Content[i+1].Tag, b.Content[i+1].Style = value, tag, 0
		return
	}
	b.Content = append(b.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
}
