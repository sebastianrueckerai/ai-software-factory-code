package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// SourceMonthCap is the source of the human-lane task filed when the month
// cap is reached.
const SourceMonthCap = "month_cap"

// monthCapReached and projectCapReached open the escalation of a card
// stopped at the month cap or at its project's, dayCapReached the day
// cap's.
const (
	monthCapReached   = "the month cap is reached"
	projectCapReached = "the project's month cap is reached"
	dayCapReached     = "the day cap is reached"
)

// monthCapResumes, projectCapResumes and dayCapResumes open the reason of a
// card the cap stopped, whose wait ends on its own (capWaitReason).
const (
	monthCapResumes   = monthCapReached + ": the card resumes on its own"
	projectCapResumes = projectCapReached + ": the card resumes on its own"
	dayCapResumes     = dayCapReached + ": the card resumes on its own"
)

// capOpenings is, by the refusal's decision, the opening of the reasons of
// a card stopped at the cap (waiting for a human, and resuming on its own),
// the source of its escalation alerts, its period and its name.
var capOpenings = map[string]struct{ waits, resumes, source, period, name string }{
	policy.RefusedDayCap:     {dayCapReached, dayCapResumes, "day_cap", "day", "day cap"},
	policy.RefusedMonthCap:   {monthCapReached, monthCapResumes, SourceMonthCap, "month", "month cap"},
	policy.RefusedProjectCap: {projectCapReached, projectCapResumes, "project_cap", "month", "project's month cap"},
}

// capGateway says the gateway refused a session's call at the cap
// decision; it opens the second line of the reason.
func capGateway(decision string) string {
	return "the gateway refused this task's spend (" + decision + ")"
}

// capFloor says the attempt's floor refused the attempt at the cap
// decision before it started; it opens the second line of the reason.
func capFloor(decision string) string {
	return "the attempt's floor refused the attempt before it started (" + decision + ")"
}

// capRefused is the escalation reason of a session the gateway refused at
// a month cap, which waits for a human: the reason of a run from before
// the cap resumes on its own.
func capRefused(r policy.Refusal) string {
	return capOpenings[r.Decision].waits + ": " + capGateway(r.Decision) + ": " + r.Reason
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

// dayTurn is when the UTC day of at ends: the day cap counts the ledger's
// UTC day (budget.day_eur), so it resets then.
func dayTurn(at time.Time) time.Time {
	y, m, d := at.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// monthTurn is when the UTC month of at ends: the month and the project
// caps count the ledger's UTC month (policy.MonthSpentSQL).
func monthTurn(at time.Time) time.Time {
	y, m, _ := at.UTC().Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}

// capTurn is when the cap decision refused resets on its own after at.
func capTurn(decision string, at time.Time) time.Time {
	if decision == policy.RefusedDayCap {
		return dayTurn(at)
	}
	return monthTurn(at)
}

// projectFile matches the project file a state names.
var projectFile = regexp.MustCompile(`projects/[^\s/]+\.yaml`)

// capWaitReason is the reason of a card by (capGateway, capFloor) refused
// at the cap decision, which resets at until: its first line is the card's
// question row (escalationQuestion), with the actions; the second names by
// and its state.
func capWaitReason(decision, by, state string, until time.Time) string {
	var what, key, file string
	switch decision {
	case policy.RefusedDayCap:
		what, key, file = "the day's free headroom", "budget.day_eur", "policy.yaml"
	case policy.RefusedMonthCap:
		what, key, file = "the month's free headroom", "budget.month_eur", "policy.yaml"
	default:
		what, key, file = "the project's free month headroom", "budget.month_eur", cmp.Or(projectFile.FindString(state), "the project's file")
	}
	o := capOpenings[decision]
	return fmt.Sprintf("%s once %s funds a session, at the latest when the UTC %s turns at %s. "+
		"Raise %s in %s to resume it sooner, or close the card.\n%s: %s", o.resumes, what, o.period, stamp(until), key, file, by, state)
}

// capWaitDecision is the decision whose resumes opening reason starts
// with, "" for none.
func capWaitDecision(reason string) string {
	for decision, o := range capOpenings {
		if strings.HasPrefix(reason, o.resumes) {
			return decision
		}
	}
	return ""
}

// capWaitOf is the wait on a cap the task's last escalation holds: its
// reason and the turn of the cap's period; "" when it asks otherwise.
func capWaitOf(events []IntakeEvent) (reason string, until time.Time) {
	for _, e := range slices.Backward(events) {
		if e.Kind != IntakeEscalation {
			continue
		}
		if d := capWaitDecision(e.Body); d != "" {
			return e.Body, capTurn(d, e.CreatedAt)
		}
		break
	}
	return "", time.Time{}
}

// MonthSpend is the UTC calendar month's variable spend: Month as
// YYYY-MM, EUR booked in the ledger, and Refusal the reason of the month's
// newest refused_month_cap of a model call, made at RefusedAt ("" for none):
// the gateway's headroom, or LiteLLM's proxy-wide budget.
type MonthSpend struct {
	Month     string
	EUR       float64
	Refusal   string
	RefusedAt time.Time
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
	// AlertByTask is the newest alert of kind linked to task taskID.
	AlertByTask(ctx context.Context, kind, taskID string) (Alert, bool, error)
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error
	projectLookup
	taskReader
}

// MonthAlarm raises the month's spend against budget.month_eur (item 64 a):
// an alert at 50 and at 80 percent once per UTC month, and at the cap an
// alert and a human-lane issue on the configuration repository that names
// the spend and links the month_eur line, once per cap in force. It closes
// the cap's issue when a raise lifts the cap above the spend, or the month
// ends.
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
	// Metrics counts the outcome of the cards the alarm closes; nil counts none.
	Metrics *telemetry.Metrics
	// Scrub masks credentials in the refusal the card and the alert show; nil
	// masks the generic shapes only.
	Scrub *redact.Scrubber
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
// the cap counts as reached at a model call it refused too, since a call is
// refused before the ledger meets the cap, but only a call refused once the
// gateway enforced the version in force (gateway.PolicyTTL after it
// activated). A cap of 0 stops all spend by design and raises nothing.
func (a *MonthAlarm) Check(ctx context.Context) error {
	v := a.Current()
	limit := v.Document.Budget.MonthEUR
	m, err := a.Store.MonthSpend(ctx, time.Now())
	if err != nil {
		return err
	}
	m.Refusal = a.Scrub.String(m.Refusal)
	closeErr := a.closeLifted(ctx, v, m, limit)
	if limit <= 0 {
		return closeErr
	}
	refused := m.Refusal != "" && !m.RefusedAt.Before(v.ActivatedAt.Add(gateway.PolicyTTL))
	for _, pct := range []int{50, 80, 100} {
		if m.EUR < limit*float64(pct)/100 && !(pct == 100 && refused) {
			continue
		}
		fp := m.Month + "/" + strconv.Itoa(pct)
		if pct == 100 {
			fp += "/" + strconv.FormatFloat(limit, 'f', -1, 64)
		}
		row, ok, err := a.Store.AlertByFingerprint(ctx, AlertMonthCap, fp)
		if err != nil {
			return errors.Join(err, closeErr)
		}
		if !ok {
			detail := map[string]any{"month": m.Month, "spent_eur": m.EUR, "cap_eur": limit, "refusal": m.Refusal, "policy_version": v.ID}
			if row.ID, err = a.Store.InsertAlert(ctx, Alert{Kind: AlertMonthCap, Source: strconv.Itoa(pct), Fingerprint: fp,
				Verdict: VerdictReal, ResolvedBy: "factoryd", Detail: detail}); err != nil {
				return errors.Join(err, closeErr)
			}
			log.Printf("month cap: %s spent %.2f EUR of %.2f EUR, %d percent alert", m.Month, m.EUR, limit, pct)
		}
		if pct == 100 && row.TaskID == "" && row.ID != 0 {
			if err := a.file(ctx, v, m, row.ID); err != nil {
				return errors.Join(err, closeErr)
			}
		}
	}
	return closeErr
}

// closeLifted closes every open cap card whose month has ended, and those of
// this month whose cap a raise lifted above the card's. A card that fails to
// close does not stop the others; the failures are joined.
func (a *MonthAlarm) closeLifted(ctx context.Context, v policy.Version, m MonthSpend, limit float64) error {
	open, err := a.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range open {
		if t.Source != SourceMonthCap {
			continue
		}
		row, ok, err := a.Store.AlertByTask(ctx, AlertMonthCap, t.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		month, _ := row.Detail["month"].(string)
		capEUR, _ := row.Detail["cap_eur"].(float64)
		var msg string
		switch {
		case month < m.Month:
			msg = fmt.Sprintf("The UTC month %s ended: the cap card closes.", month)
		case month == m.Month && limit > capEUR:
			msg = fmt.Sprintf("budget.month_eur is %s in policy version %d: the cap card closes.", strconv.FormatFloat(limit, 'f', -1, 64), v.ID)
		default:
			continue
		}
		if err := closeIssueAsFactory(ctx, a.Store, a.Forgejo, t, msg); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
			errs = append(errs, err)
			continue
		}
		countOutcome(ctx, a.Metrics, a.Store, t, "done")
		log.Printf("month cap: %s closed issue #%d of task %s", msg, t.IssueNumber, t.ID)
	}
	return errors.Join(errs...)
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
	if outerGuardRefused(m.Refusal, m.EUR, limit) {
		link = fmt.Sprintf("%s/%s/src/commit/%s/k8s/platform/litellm/helmrelease.yaml", a.Forgejo.PublicURL, a.Repo, v.Commit)
	}
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
	tl.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": cmp.Or(t.Author, forgejo.BotUser), "issue": number, "budget_eur": t.BudgetEUR})
	if err := recordAsk(ctx, store, tl, t, q, "factoryd", t.Source, ViaFactoryd, decider, nil); err != nil {
		return Task{}, 0, err
	}
	return t, number, nil
}

// CapWaits wakes the cards waiting on a cap when a policy version loads.
type CapWaits struct {
	Store interface {
		ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
		IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	}
	// Signal delivers the policy_loaded signal to a task's workflow.
	Signal func(ctx context.Context, taskID string) error
}

// Wake signals every waiting-human card whose last escalation is a cap wait.
// It signals them all even when one fails, and returns the failures.
func (c *CapWaits) Wake(ctx context.Context, v policy.Version) error {
	tasks, err := c.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return fmt.Errorf("list the waiting cards: %w", err)
	}
	var errs []error
	woken := 0
	for _, t := range tasks {
		events, err := c.Store.IntakeEvents(ctx, t.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: events: %w", t.ID, err))
			continue
		}
		if reason, _ := capWaitOf(events); reason == "" {
			continue
		}
		if err := c.Signal(ctx, t.ID); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, err))
			continue
		}
		woken++
	}
	log.Printf("config: version %d loaded, %d cap waits woken", v.ID, woken)
	return errors.Join(errs...)
}
