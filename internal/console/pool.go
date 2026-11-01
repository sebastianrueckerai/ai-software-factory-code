package console

import (
	"context"
	"fmt"
	"slices"
	"time"

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
