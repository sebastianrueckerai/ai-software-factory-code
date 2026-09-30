package factoryd

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceMonthCap is the source of the human-lane task filed when the month
// cap is reached.
const SourceMonthCap = "month_cap"

// monthCapReached opens the escalation of a session the gateway refused
// at the month cap.
const monthCapReached = "the month cap is reached: the gateway refused this task's spend (refused_month_cap): "

// MonthSpend is the UTC calendar month's variable spend: Month as
// YYYY-MM, EUR booked in the ledger, and whether the gateway refused a
// call at the month cap.
type MonthSpend struct {
	Month   string
	EUR     float64
	Refused bool
}

// MonthBook is what the month alarm reads and keeps.
type MonthBook interface {
	CreateTask(ctx context.Context, t Task) error
	MonthSpend(ctx context.Context) (MonthSpend, error)
	InsertAlert(ctx context.Context, a Alert) (int64, error)
	AlertByFingerprint(ctx context.Context, kind, fingerprint string) (Alert, bool, error)
	SetAlertTask(ctx context.Context, id int64, taskID string, detail map[string]any) error
}

// MonthAlarm raises the month's spend against budget.month_eur (item 64 a):
// an alert at 50 and at 80 percent, and at the cap an alert and a
// human-lane issue on the configuration repository that names the spend
// and links the month_eur line, each once per UTC month.
type MonthAlarm struct {
	Store   MonthBook
	Forgejo *Forgejo
	// Repo holds the tree in directory Dir, read through Config.
	Repo    Repo
	Dir     string
	Config  *ForgejoConfig
	Current func() policy.Version
}

// Loop checks every interval until ctx ends.
func (a *MonthAlarm) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := a.Check(ctx); err != nil {
				log.Printf("month cap: %v", err)
			}
		}
	}
}

// Check raises every threshold the month has passed and has no alert for;
// the cap counts as reached at its first refusal too, since a call is
// refused before the ledger meets the cap. A cap of 0 stops all spend by
// design and raises nothing.
func (a *MonthAlarm) Check(ctx context.Context) error {
	v := a.Current()
	limit := v.Document.Budget.MonthEUR
	if limit <= 0 {
		return nil
	}
	m, err := a.Store.MonthSpend(ctx)
	if err != nil {
		return err
	}
	for _, pct := range []int{50, 80, 100} {
		if m.EUR < limit*float64(pct)/100 && !(pct == 100 && m.Refused) {
			continue
		}
		fp := m.Month + "/" + strconv.Itoa(pct)
		row, ok, err := a.Store.AlertByFingerprint(ctx, AlertMonthCap, fp)
		if err != nil {
			return err
		}
		if !ok {
			detail := map[string]any{"month": m.Month, "spent_eur": m.EUR, "cap_eur": limit, "refused": m.Refused, "policy_version": v.ID}
			if row.ID, err = a.Store.InsertAlert(ctx, Alert{Kind: AlertMonthCap, Source: strconv.Itoa(pct), Fingerprint: fp,
				Verdict: VerdictReal, ResolvedBy: "factoryd", Detail: detail}); err != nil {
				return err
			}
			log.Printf("month cap: %s spent %.2f EUR of %.2f EUR, %d percent alert", m.Month, m.EUR, limit, pct)
		}
		if pct == 100 && row.TaskID == "" && row.ID != 0 {
			if err := a.file(ctx, v, m, row.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// file opens the cap's human-lane issue and links it to alert id.
func (a *MonthAlarm) file(ctx context.Context, v policy.Version, m MonthSpend, id int64) error {
	b, err := a.Config.File(ctx, v.Commit, policy.FilePolicy)
	if err != nil {
		return err
	}
	line := 0
	for i, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "month_eur:") {
			line = i + 1
		}
	}
	if line == 0 {
		return fmt.Errorf("%s at %s has no month_eur line", policy.FilePolicy, v.Commit)
	}
	limit := v.Document.Budget.MonthEUR
	link := fmt.Sprintf("%s/%s/src/commit/%s/%s/%s#L%d", a.Forgejo.PublicURL, a.Repo, v.Commit, a.Dir, policy.FilePolicy, line)
	title := fmt.Sprintf("budget: the month cap of %.2f EUR is reached (%s)", limit, m.Month)
	body := fmt.Sprintf("The factory's variable spend this month (UTC, over the cost ledger: model calls, paid tools, commissioned servers) "+
		"is %.2f EUR of the cap of %.2f EUR (`budget.month_eur`, %s).\n\n"+
		"The gateway refuses every further call with `refused_month_cap` until the month ends or the cap is raised; "+
		"a task it refused waits on a human with the refusal on its card. Raising the cap is a commit to the `month_eur` line, "+
		"like any setting. Close this issue once decided.\n\nfiled by factoryd", m.EUR, limit, link)
	t, ref, err := FileTask(ctx, a.Store, a.Forgejo, nil, a.Repo, title, body, []string{"human"}, Task{
		ID: NewTaskID(), Kind: "human", RiskClass: "standard", Objective: title + "\n\n" + body, SHA: v.Commit,
		Source: SourceMonthCap, Author: a.Forgejo.BotUser,
	})
	if err != nil {
		return err
	}
	return a.Store.SetAlertTask(ctx, id, t.ID, map[string]any{"issue": ref.Number})
}
