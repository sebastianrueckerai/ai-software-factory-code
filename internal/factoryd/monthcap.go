package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceMonthCap is the source of the human-lane task filed when the month
// cap is reached.
const SourceMonthCap = "month_cap"

// monthCapReached and projectCapReached open the escalation of a session
// the gateway refused at the month cap or at its project's.
const (
	monthCapReached   = "the month cap is reached: the gateway refused this task's spend (refused_month_cap): "
	projectCapReached = "the project's month cap is reached: the gateway refused this task's spend (refused_project_cap): "
)

// capRefused is the escalation reason of a session the gateway refused at
// a month cap.
func capRefused(r policy.Refusal) string {
	if r.Decision == policy.RefusedProjectCap {
		return projectCapReached + r.Reason
	}
	return monthCapReached + r.Reason
}

// CapRefusal is the gateway's refusal of a model call of a task's session
// at the day's, the month's or the project's cap, when its audit row was
// written, and whether the gateway allowed a model call of the session's
// agent before.
type CapRefusal struct {
	policy.Refusal
	At      time.Time
	Allowed bool `json:",omitempty"`
}

// until is when the refusal's cap resets on its own: the day cap's at the
// UTC day's turn; zero for a month's cap, which only a human raises.
func (c CapRefusal) until() time.Time {
	if c.Decision != policy.RefusedDayCap {
		return time.Time{}
	}
	return dayTurn(c.At)
}

// reason is the card's escalation: a month's cap waits for a human, the
// day's resumes on its own (dayCapRefused).
func (c CapRefusal) reason() string {
	if c.Decision != policy.RefusedDayCap {
		return capRefused(c.Refusal)
	}
	return dayCapRefused(dayCapGateway, c.Reason, c.until())
}

// dayCapReached opens the reason of a card the day cap stopped.
const dayCapReached = "the day cap is reached"

// What refused at the day cap, which opens the reason's second line: the
// gateway a session's call, or the attempt's floor the attempt before it
// started.
const (
	dayCapGateway = "the gateway refused this task's spend (refused_day_cap)"
	dayCapFloor   = "the attempt's floor refused the attempt before it started (refused_day_cap)"
)

// dayTurn is when the UTC day of at ends: the day cap counts the ledger's
// UTC day (budget.day_eur), so it resets then.
func dayTurn(at time.Time) time.Time {
	y, m, d := at.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// dayCapResumes opens the reason of a card the day cap stopped, whose
// wait ends on its own (dayWait).
const dayCapResumes = dayCapReached + ": the card resumes on its own"

// dayCapRefused is the reason of a card by (dayCapGateway, dayCapFloor)
// refused at the day cap, which resets at until: its first line is the
// card's question row (escalationQuestion), with the actions; the second
// names by and its state.
func dayCapRefused(by, state string, until time.Time) string {
	return fmt.Sprintf("%s once the day's free headroom funds a session, at the latest when the UTC day turns at %s. "+
		"Raise budget.day_eur in policy.yaml to resume it sooner, or close the card.\n%s: %s", dayCapResumes, stamp(until), by, state)
}

// dayCapWait is the wait on the day cap the task's last escalation holds:
// its reason and the turn of its UTC day; "" when it asks otherwise.
func dayCapWait(events []IntakeEvent) (reason string, until time.Time) {
	for _, e := range slices.Backward(events) {
		if e.Kind != IntakeEscalation {
			continue
		}
		if strings.HasPrefix(e.Body, dayCapResumes) {
			return e.Body, dayTurn(e.CreatedAt)
		}
		break
	}
	return "", time.Time{}
}

// MonthSpend is the UTC calendar month's variable spend: Month as
// YYYY-MM, EUR booked in the ledger, and Refusal the reason of the month's
// first refused_month_cap of a model call ("" for none): the gateway's
// headroom, or LiteLLM's proxy-wide budget.
type MonthSpend struct {
	Month   string
	EUR     float64
	Refusal string
}

// MonthBook is what the month alarm reads and keeps.
type MonthBook interface {
	CreateTask(ctx context.Context, t Task) error
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	MonthSpend(ctx context.Context, at time.Time) (MonthSpend, error)
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
	// Timeline records the card's card_created; nil records none.
	Timeline *Timeline
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
// the cap counts as reached at the first model call it refused too, since
// a call is refused before the ledger meets the cap. A cap of 0 stops all spend by
// design and raises nothing.
func (a *MonthAlarm) Check(ctx context.Context) error {
	v := a.Current()
	limit := v.Document.Budget.MonthEUR
	if limit <= 0 {
		return nil
	}
	m, err := a.Store.MonthSpend(ctx, time.Now())
	if err != nil {
		return err
	}
	for _, pct := range []int{50, 80, 100} {
		if m.EUR < limit*float64(pct)/100 && !(pct == 100 && m.Refusal != "") {
			continue
		}
		fp := m.Month + "/" + strconv.Itoa(pct)
		row, ok, err := a.Store.AlertByFingerprint(ctx, AlertMonthCap, fp)
		if err != nil {
			return err
		}
		if !ok {
			detail := map[string]any{"month": m.Month, "spent_eur": m.EUR, "cap_eur": limit, "refusal": m.Refusal, "policy_version": v.ID}
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
	t := Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard", SHA: v.Commit, Source: SourceMonthCap, Author: a.Forgejo.BotUser}
	t, number, err := fileHumanTask(ctx, a.Store, a.Forgejo, a.Timeline, a.Repo, monthCapCardQuestion(limit, m.Month, m.EUR, link, m.Refusal), nil, t, "")
	if err != nil {
		return err
	}
	return a.Store.SetAlertTask(ctx, id, t.ID, map[string]any{"issue": number})
}

// fileHumanTask files human task t on repo from question q through
// FileTask, the ask its title, labelled
// human and labels, unless the bot's open issue of the same title that no
// task row holds is there: an issue filed by a check whose task row then
// failed is taken on, not filed again. An issue a task holds is that
// card's, such as an earlier routine run's still open. number is the
// issue's. Either way the card records card_created and its ask on tl,
// decider its poke's decider line ("" for none).
func fileHumanTask(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
}, forgejo *Forgejo, tl *Timeline, repo Repo, q HumanQuestion, labels []string, t Task, decider string) (Task, int64, error) {
	title := askTitle(q)
	body := questionBody(q, "factoryd asks ("+t.Source+"). Answer on the card in the console or in the chat's inbox, or close this issue once decided.")
	t.Objective = title + "\n\n" + body
	open, err := forgejo.OpenIssues(ctx, repo.Owner, repo.Name)
	if err != nil {
		return Task{}, 0, err
	}
	var number int64
	for _, is := range open {
		if is.Title != title || is.Poster.UserName != forgejo.BotUser {
			continue
		}
		if _, held, err := store.TaskByIssue(ctx, repo.String(), is.Index); err != nil {
			return Task{}, 0, err
		} else if !held {
			number = is.Index
		}
	}
	labels = append([]string{"human"}, labels...)
	if number == 0 {
		var ref IssueRef
		if t, ref, err = FileTask(ctx, store, forgejo, repo, title, body, labels, t); err != nil {
			return Task{}, 0, err
		}
		number = ref.Number
	} else {
		t.Status, t.Repository, t.IssueNumber = "waiting-human", repo.String(), number
		if err := forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, number, append(labels, "state/waiting-human")); err != nil {
			return Task{}, 0, err
		}
		if err := store.CreateTask(ctx, t); err != nil {
			return Task{}, 0, err
		}
	}
	tl.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": cmp.Or(t.Author, forgejo.BotUser), "issue": number})
	if err := recordAsk(ctx, store, tl, t, q, "factoryd", t.Source, ViaFactoryd, decider); err != nil {
		return Task{}, 0, err
	}
	return t, number, nil
}
