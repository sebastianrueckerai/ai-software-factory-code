package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// poolLine is a project's escalation pool as every face reads it: what its
// cards drew this UTC month on every rung (PoolDrawn) against the pool the
// policy tree sets (EscalationOf), in three parts: sessions, agent time and
// money. Readable: the draws could be read; an unreadable read is never
// zero. Overdrawn: a part is past its pool, where a human's draw may take
// it. Present: the project file sets the pool's money itself, which a
// top-up grows; without, the money is the policy's and a set gives the
// project its own.
type poolLine struct {
	Present, Readable, Overdrawn bool
	Pool, Drawn                  policy.Draw
	// Month is the UTC month the draws count ("2026-11") and Policy the
	// loaded policy's identity, the version and commit whose tree sets
	// the pool.
	Month, Policy string
	Words         string
}

// ProjectPool is p's pool line from v, the policy in force, at now — the
// explicit instant the read-helper's contract takes: the project face,
// the settings, the policy page and the chat's pool line all read this
// one projection at their own now.
func (c *Console) ProjectPool(ctx context.Context, v policy.Version, p factoryd.Project, now time.Time) poolLine {
	out := poolLine{Month: now.UTC().Format("2006-01"), Policy: fmt.Sprintf("policy version %d (commit %.7s)", v.ID, v.Commit),
		Pool: v.Tree.EscalationOf(p.Repository).Pool}
	if pc, ok := v.Tree.Projects[p.Name]; ok && pc.Escalation != nil {
		out.Present = factoryd.Cents(pc.Escalation.Pool.EUR) > 0
	}
	drawn, err := c.cfg.PoolDrawn(ctx, p.Repository, now)
	if err != nil {
		log.Printf("console: the pool of %s: %v", p.Repository, err)
		out.Words = fmt.Sprintf("the pool is %d sessions, %s of agent time and €%.2f this UTC month, but what its cards drew could not be read",
			out.Pool.Sessions, agentHours(out.Pool.AgentTime), out.Pool.EUR)
		return out
	}
	out.Drawn, out.Readable = drawn, true
	out.Words = fmt.Sprintf("%d of %d sessions, %s of %s agent time and €%.2f of €%.2f drawn this UTC month",
		drawn.Sessions, out.Pool.Sessions, agentHours(drawn.AgentTime), agentHours(out.Pool.AgentTime), drawn.EUR, out.Pool.EUR)
	var over []string
	if drawn.Sessions > out.Pool.Sessions {
		over = append(over, "sessions")
	}
	if drawn.AgentTime > out.Pool.AgentTime {
		over = append(over, "agent time")
	}
	if factoryd.Cents(drawn.EUR) > factoryd.Cents(out.Pool.EUR) {
		over = append(over, "money")
	}
	if len(over) > 0 {
		out.Overdrawn = true
		out.Words += ", overdrawn in " + textutil.AndList(over) + ": only the human draws more until the month turns"
	}
	return out
}

// agentHours is agent time as the pool counts it, in hours and minutes.
func agentHours(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	case m < 60:
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// poolChat is the pool line a chat page read shows: the loaded policy's
// identity and the month with the same words the settings, policy and
// project pages read — one projection through them all.
func (c *Console) poolChat(ctx context.Context, v policy.Version, p factoryd.Project) string {
	pl := c.ProjectPool(ctx, v, p, time.Now())
	return fmt.Sprintf("Pool (%s, %s UTC): %s", pl.Policy, pl.Month, pl.Words)
}

// The pool's write core (roadmap 308): the set_pool change the proposal
// routes confirm writes escalation.pool.eur of the project's file into
// the configuration repository's main, one commit,
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

// poolChange is one confirmed change of a project's escalation pool: the
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

// doSetPool changes project name's escalation pool money as the human a:
// the change's amount into escalation.pool.eur of the project's file, one
// commit through commitProjectFile guarded by the source the proposal
// captured, touching nothing else; the loader's check of the candidate
// holds the card draw within the pool. The change records exactly one
// pool_changed row naming the human; a row
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
	held := res.from
	if v, ok := c.inForce(ctx); ok {
		if p, ok := v.Projects[name]; ok {
			held = v.EscalationOf(p.Repository).Pool.EUR
			res.loaded = factoryd.Cents(held) == amounts.to
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
			c.cfg.InfraRepo, pr, name, held))
	}
	log.Printf("console: %s %s the pool of %s to EUR %.2f via %s (%s#%d) in %.1fs", a.s.user, ch.action, name, res.to, a.via, c.cfg.InfraRepo, pr, time.Since(start).Seconds())
	return res, nil
}

// poolCents is the change's amount as whole cents, refusing the amounts
// a pool cannot hold: an unknown action, a change without its reason or
// its source, an amount at or below zero (a pool of 0 takes the policy's
// figure), a number that is not finite, an amount over the pool's bound,
// or one with a fraction below a cent.
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
	case ch.action == poolSet && ch.eur <= 0:
		return 0, refuse(http.StatusBadRequest, "a set takes an amount above 0 EUR: a pool of 0 takes the policy's figure")
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
		return fmt.Sprintf("Project %s: escalation pool +%.2f EUR", name, ch.eur)
	}
	return fmt.Sprintf("Project %s: escalation pool %.2f EUR", name, ch.eur)
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
	from, err := poolBounds(&doc, path, in.Action)
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
	was := fmt.Sprintf("was €%.2f", res.from)
	if res.from == 0 {
		was = "was the policy's"
	}
	return poolPress{res: res, words: fmt.Sprintf(
		"The pool of %s is €%.2f now, %s: %s#%d carries the change and the policy in force holds it.",
		t.p.Name, res.to, was, t.c.cfg.InfraRepo, res.pr)}
}

// poolLanded streams a pool press's already-landed result: the tool block and
// the applied line of the write that happened before the stream opened,
// the row in the conversation naming the member who pressed, and done.
// Nothing writes here.
func (t *turn) poolLanded(p factoryd.ChatProposal, fix *poolCorrection, pr poolPress) {
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
	stands := fmt.Sprintf("the pool stands at %.2f EUR", poolEUR(t.pool.From))
	if t.pool.From == 0 {
		stands = "the pool's money is the policy's"
	}
	return fmt.Sprintf("%s, %s, %s, reason %s, it would stand at %.2f EUR", title, stands, op, t.pool.Reason, poolEUR(to))
}

// poolEdit is the change of escalation.pool.eur to cents: the file's own
// value checked by poolBounds, then the one key set, the escalation block
// and its pool written when the file has none, with nothing else, no
// other key, comment or part, moving. The arithmetic is whole cents end to
// end; the file's own value is read as the exact decimal it holds and the
// new one written as the cents the human confirmed.
func poolEdit(path, action string, cents int64, amounts *poolAmounts) func(doc *yaml.Node) error {
	return func(doc *yaml.Node) error {
		from, err := poolBounds(doc, path, action)
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
		setKey(mappingAt(mappingAt(doc.Content[0], "escalation"), "pool"), "eur", poolDecimal(to))
		return nil
	}
}

// mappingAt is m's mapping at key, appended empty when m has none.
func mappingAt(m *yaml.Node, key string) *yaml.Node {
	if i := keyIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// poolBounds is the pool money the file sets in cents once the file is
// checked: a mapping root, an escalation block and its pool that are
// mappings when present and shared through no YAML alias, and an eur the
// pool can take exactly, absent meaning zero. A top-up grows only the
// money the file sets itself: without it the pool is the policy's, which a
// set replaces. A document that parses to no mapping root, an empty file,
// one of whitespace or a scalar, holds no YAML the pool can read. A block
// or a value the file shares through a YAML alias is never edited: the
// edit would land everywhere the alias reads, so it refuses and a policy
// PR unshares the value first.
func poolBounds(doc *yaml.Node, path, action string) (int64, error) {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return 0, fmt.Errorf("%s holds no YAML the pool can read: a policy PR corrects it", path)
	}
	shared := poolAliased(doc)
	aliased := fmt.Errorf("%s aliases its escalation block or its pool: a policy PR removes the YAML alias before a pool change", path)
	n := doc.Content[0]
	for _, key := range []string{"escalation", "pool", "eur"} {
		i := keyIndex(n, key)
		if i < 0 {
			if action == poolTopUp {
				return 0, fmt.Errorf("%s sets no escalation.pool.eur, so the pool's money is the policy's: set the pool instead of a top-up", path)
			}
			return 0, nil
		}
		v := n.Content[i+1]
		switch {
		case v.Kind == yaml.AliasNode || shared[v]:
			return 0, aliased
		case key != "eur" && v.Kind != yaml.MappingNode:
			return 0, fmt.Errorf("%s holds an escalation block the pool cannot read: a policy PR corrects it", path)
		case key == "eur":
			c, ok := poolFileCents(v.Value)
			if !ok {
				return 0, fmt.Errorf("%s holds an escalation.pool.eur the pool cannot take: a policy PR corrects it", path)
			}
			return c, nil
		}
		n = v
	}
	return 0, nil
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

// setKey sets key of mapping b to value, in its place with its
// comment kept when it is there, appended when it is not. The value's
// tag is the one it resolves to — an int for a whole euro, a float
// otherwise — so the file keeps writing the amount plainly.
func setKey(b *yaml.Node, key, value string) {
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
