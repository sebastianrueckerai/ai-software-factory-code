package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// HoldAskAfter is how long production waits on open plans before the
// keeper asks a person, and again after each ask (decision 5).
const HoldAskAfter = 6 * time.Hour

// SourceProductionHeld is the source of the human card that asks what to
// do about held production.
const SourceProductionHeld = "production-held"

// The held-production ask's answers. A replan's value is the answer, a
// colon and the plan's id (ReplanChoice).
const (
	AnswerHoldWait     = "hold_wait"
	AnswerDeployAnyway = "deploy_anyway"
	AnswerReplanOnTop  = "replan_on_top"
	AnswerTakeOffMain  = "take_off_main"
)

// ErrNotHeld refuses a Deploy now while no plan holds production.
var ErrNotHeld = errors.New("production is not held")

// promoteJob is the job of a project's main run that asks /release and
// rolls prod (the template's ci.yml).
const promoteJob = "promote"

// ReplanChoice reads a replan answer's value: the plan, and whether its
// merged cards come off main first.
func ReplanChoice(value string) (plan string, takeOff bool, ok bool) {
	answer, plan, _ := strings.Cut(value, ":")
	if plan == "" || answer != AnswerReplanOnTop && answer != AnswerTakeOffMain {
		return "", false, false
	}
	return plan, answer == AnswerTakeOffMain, true
}

// ReleaseStore is what the ReleaseKeeper reads and writes.
type ReleaseStore interface {
	OpenGroups(ctx context.Context) (map[string][]Group, error)
	Release(ctx context.Context, repository string) (Release, error)
	Releases(ctx context.Context) ([]Release, error)
	PutReleaseHold(ctx context.Context, repository string, since, asked, was, wasHeld time.Time) (bool, error)
	SetDeployNow(ctx context.Context, repository, commit string, since time.Time) error
	EndReleaseHold(ctx context.Context, repository string) error
	ClearToldHeld(ctx context.Context, repository, commit string) error
	FixesMergedSince(ctx context.Context, repository string, since time.Time) ([]Task, error)
	PlanCards(ctx context.Context, plan string) ([]Task, error)
	ChainCard(ctx context.Context, repository, source string) (Task, bool, error)
	ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
	CreateTask(ctx context.Context, t Task) error
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error
}

// ReleaseKeeper keeps production's hold while a plan is open on main, for
// the factory's own repository Own and for each project whose CI asks
// /release: the hold's clock, a person asked after HoldAskAfter or at once
// when a fix merges, the hold's end once no plan is open, a promote job
// told held run again, and Deploy now.
type ReleaseKeeper struct {
	Store    ReleaseStore
	Forgejo  *Forgejo
	Timeline *Timeline
	Own      Repo
	Now      func() time.Time
}

// Pass keeps the hold of Own and of every project whose CI asked /release.
// One repository's error is logged and the rest go on; the errors are
// joined.
func (k *ReleaseKeeper) Pass(ctx context.Context) error {
	rows, err := k.Store.Releases(ctx)
	if err != nil {
		return err
	}
	all, err := k.Store.OpenGroups(ctx)
	if err != nil {
		return err
	}
	own := k.Own.String()
	kept := map[string]Release{own: {Repository: own}}
	for _, r := range rows {
		if r.Repository == own || !r.AsksAt.IsZero() {
			kept[r.Repository] = r
		}
	}
	var errs []error
	for _, repository := range slices.Sorted(maps.Keys(kept)) {
		if err := k.keep(ctx, kept[repository], all[repository]); err != nil {
			log.Printf("release keeper: %s: %v", repository, err)
			errs = append(errs, fmt.Errorf("%s: %w", repository, err))
		}
	}
	return errors.Join(errs...)
}

// keep is one repository's pass over its row as read: the hold while
// groups are open on its main, else its end where a clock runs, then a
// promote job to run again. A promote job told held runs again once no
// group is open, or while one is when its commit is Deploy now's; at the
// hold's end an empty ToldHeld is unknown (a failed ask record leaves it
// unwritten), so the newest run's promote job runs again. Own's roll is
// the roller's.
func (k *ReleaseKeeper) keep(ctx context.Context, row Release, groups []Group) error {
	now, repository := k.Now(), row.Repository
	var err error
	ended := false
	switch {
	case len(groups) > 0:
		err = k.hold(ctx, row, groups, now)
	case !row.HeldSince.IsZero():
		if err := k.Store.EndReleaseHold(ctx, repository); err != nil {
			return err
		}
		err = k.closeAsk(ctx, repository, "factoryd: no plan is open on main any more, so production goes ahead. Closing this issue.")
		k.released(ctx, repository, "closed", row.HeldSince, now, FactoryDecider, ViaFactoryd, "")
		ended = true
	}
	switch {
	case repository == k.Own.String(), row.ToldHeld == "" && !ended, len(groups) > 0 && row.ToldHeld != row.DeployNow:
		return err
	}
	return errors.Join(err, k.promoteAgain(ctx, repository, row.ToldHeld, len(groups) > 0))
}

// hold starts the clock of row's hold at the first merge unless it runs,
// and asks a person: a standing ask hears of each new fix; else a new ask
// is filed once a fix merged, or once HoldAskAfter passed since the
// clock's start and since the last ask or its close. The ask's time is
// written first, only where the row's ask and clock are still the ones
// this pass read, and the ask goes out only where that write landed, so a
// Deploy now or another pass meanwhile wins. An ask that does not go out
// puts its time back, unless Forgejo refused it for good: then the next
// comes HoldAskAfter later, not each minute.
func (k *ReleaseKeeper) hold(ctx context.Context, row Release, groups []Group, now time.Time) error {
	repository, since := row.Repository, row.HeldSince
	if since.IsZero() {
		since = groups[0].FirstMerge
		if since.IsZero() {
			since = now
		}
		if landed, err := k.Store.PutReleaseHold(ctx, repository, since, row.AskedAt, row.AskedAt, row.HeldSince); err != nil || !landed {
			return err
		}
	}
	card, filed, err := k.Store.ChainCard(ctx, repository, SourceProductionHeld)
	if err != nil {
		return err
	}
	standing := filed && !Final(card.Status)
	from := since
	if row.AskedAt.After(from) {
		from = row.AskedAt
	}
	fixes, err := k.Store.FixesMergedSince(ctx, repository, from)
	if err != nil {
		return err
	}
	last := row.AskedAt
	if filed && card.FinishedAt != nil && card.FinishedAt.After(last) {
		last = *card.FinishedAt
	}
	due := now.Sub(since) >= HoldAskAfter && (last.IsZero() || now.Sub(last) >= HoldAskAfter)
	if len(fixes) == 0 && (standing || !due) {
		return nil
	}
	p, err := k.project(ctx, repository)
	if err != nil {
		return err
	}
	repo := p.Repo()
	var q HumanQuestion
	if !standing {
		if q, err = k.question(ctx, p.Name, since, now, groups, fixes); err != nil {
			return err
		}
	}
	if landed, err := k.Store.PutReleaseHold(ctx, repository, since, now, row.AskedAt, since); err != nil || !landed {
		return err
	}
	if standing {
		comment := "factoryd: fixes " + fixRefs(fixes) + " merged too and wait for production."
		if len(fixes) == 1 {
			comment = "factoryd: fix " + fixRefs(fixes) + " merged too and waits for production."
		}
		err = k.Forgejo.CommentOnce(ctx, repo.Owner, repo.Name, card.IssueNumber, comment)
	} else {
		_, err = EscalateToHuman(ctx, k.Store, k.Forgejo, k.Timeline, repo, q, "the production hold", Task{Source: SourceProductionHeld})
	}
	if forgejoTerminal(err) {
		log.Printf("release keeper: %s: Forgejo refused the ask, which is not filed: %v; the next ask comes after %s", repository, err, hours(HoldAskAfter))
		return nil
	}
	if err != nil {
		if _, undo := k.Store.PutReleaseHold(ctx, repository, since, row.AskedAt, now, since); undo != nil {
			log.Printf("release keeper: %s: put the ask's time back: %v", repository, undo)
		}
		return err
	}
	return nil
}

// question is the hold's ask on project name: after its hours, or naming
// the fixes merged while held first; the open plans with what each waits
// on; wait, deploy anyway, and the two replans of groups[0], the plan that
// holds longest (OpenGroups orders by first merge). Within MaxChoices, the
// other plans are named, to be replanned from their drawers.
func (k *ReleaseKeeper) question(ctx context.Context, name string, since, now time.Time, groups []Group, fixes []Task) (HumanQuestion, error) {
	reason := HoldReason(groups)
	q := HumanQuestion{
		Ask:     fmt.Sprintf("Production of %s has waited %s: %s. Wait, deploy anyway, or replan?", name, hours(now.Sub(since)), reason),
		Choices: []QuestionChoice{{AnswerHoldWait, "Wait"}, {AnswerDeployAnyway, "Deploy anyway"}},
	}
	var why []string
	if len(fixes) > 0 {
		merged := "Fixes " + fixRefs(fixes)
		if len(fixes) == 1 {
			merged = "Fix " + fixRefs(fixes)
		}
		q.Ask = fmt.Sprintf("%s merged, but production of %s is held: %s. Deploy anyway, wait, or replan?", merged, name, reason)
		for _, f := range fixes {
			what := "a red main"
			if f.Source == SourceAlert {
				what = "an alert"
			}
			why = append(why, fmt.Sprintf("#%d fixes %s.", f.IssueNumber, what))
		}
	}
	q.Why = strings.Join(append(why, fmt.Sprintf("Merged is not live: while a plan is open on main, no merge of %s goes to production, "+
		"so a half-built feature never goes live. Production has waited since %s UTC.", name, since.UTC().Format("2006-01-02 15:04"))), " ")
	info := []string{"The open plans and what each waits on:"}
	var others []string
	for i, g := range groups {
		cards, err := k.Store.PlanCards(ctx, g.Plan)
		if err != nil {
			return HumanQuestion{}, err
		}
		cards = slices.DeleteFunc(cards, func(c Task) bool { return c.Status == "merged" || c.Status == "done" || c.Status == "closed" })
		slices.SortFunc(cards, func(a, b Task) int { return cmp.Compare(a.IssueNumber, b.IssueNumber) })
		waits := "waits for the plan to close"
		if len(cards) > 0 {
			on := make([]string, len(cards))
			for i, c := range cards {
				on[i] = fmt.Sprintf("#%d (%s)", c.IssueNumber, c.Status)
			}
			waits = "waits on " + strings.Join(on, ", ")
		}
		merged, total := g.Merged()
		info = append(info, fmt.Sprintf("- plan #%d: %d of %d cards merged; %s.", g.Issue, merged, total, waits))
		if i > 0 {
			others = append(others, fmt.Sprintf("#%d", g.Issue))
			continue
		}
		q.Choices = append(q.Choices,
			QuestionChoice{AnswerReplanOnTop + ":" + g.Plan, fmt.Sprintf("Replan plan #%d on top", g.Issue)},
			QuestionChoice{AnswerTakeOffMain + ":" + g.Plan, fmt.Sprintf("Take plan #%d off main and replan", g.Issue)})
	}
	q.Info = strings.Join(info, "\n") + "\n\nWait keeps production held and asks again in " + hours(HoldAskAfter) + ". " +
		"Deploy anyway sends main as it is to production, half-built plans included. Replan on top plans the rest again on top of what is merged. " +
		"Take it off main and replan reverts the plan's merged cards first, so production goes ahead without them."
	switch len(others) {
	case 0:
	case 1:
		q.Info += "\n\nPlan " + others[0] + " is open on main too: replan it from its drawer."
	default:
		q.Info += "\n\nPlans " + andList(others) + " are open on main too: replan them from their drawers."
	}
	return q, nil
}

// fixRefs are the fixes' issues as a person reads them: "#40 and #41".
func fixRefs(fixes []Task) string {
	refs := make([]string, len(fixes))
	for i, f := range fixes {
		refs[i] = fmt.Sprintf("#%d", f.IssueNumber)
	}
	return andList(refs)
}

// hours is d in whole hours: "1 hour", "6 hours".
func hours(d time.Duration) string { return textutil.Plural(int(d/time.Hour), "hour", "hours") }

// promoteAgain runs a promote job again once its run finished, so it asks
// /release again and promotes; told is the commit a promote job was told
// held. While held, told is Deploy now's commit: its newest run's job runs,
// wherever main moved since. Once the hold ended, only main's newest run's
// does: a newer commit clears told, since its own run asks, and an empty
// told is unknown, so the newest run's job runs. A told commit with no run
// is cleared. Forgejo without a re-run, or refusing it for good, is logged
// and clears told; another failure keeps it for the next pass.
func (k *ReleaseKeeper) promoteAgain(ctx context.Context, repository, told string, held bool) error {
	p, runs, err := k.pushRuns(ctx, repository)
	if err != nil {
		return err
	}
	if held {
		runs = slices.DeleteFunc(runs, func(r ActionRun) bool { return r.CommitSHA != told })
	}
	run := newestMain(runs)
	switch {
	case told != "" && (run == nil || run.CommitSHA != told):
		return k.Store.ClearToldHeld(ctx, repository, told)
	case run == nil || !run.Finished():
		return nil
	}
	err = k.rerunPromote(ctx, p, *run)
	if errors.Is(err, ErrNoRerun) || errors.Is(err, ErrRefused) {
		log.Printf("release keeper: %s: %v: production waits for main's next run", repository, err)
	} else if err != nil {
		return err
	}
	if told == "" {
		return nil
	}
	return k.Store.ClearToldHeld(ctx, repository, told)
}

// rerunPromote re-runs run's promote job, and no other. A job list Forgejo
// does not serve (before Forgejo 17) answers ErrNoRerun; a run without a
// promote job has nothing to run again.
func (k *ReleaseKeeper) rerunPromote(ctx context.Context, p Project, run ActionRun) error {
	repo, token := p.Repo(), k.Forgejo.Access(p).Actions
	jobs, err := k.Forgejo.ActionRunJobs(ctx, token, repo.Owner, repo.Name, run.ID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("jobs of run %d of %s: %w: %v", run.ID, repo, ErrNoRerun, err)
	}
	if err != nil {
		return err
	}
	i := slices.IndexFunc(jobs, func(j ActionJob) bool { return j.Name == promoteJob })
	if i < 0 {
		log.Printf("release keeper: %s: run %d has no %s job to run again", repo, run.ID, promoteJob)
		return nil
	}
	return k.Forgejo.RerunActionJob(ctx, token, repo.Owner, repo.Name, jobs[i].ID)
}

// Hold is what holds repository's production now: zero unless it is Own
// or its CI asks /release, and a plan is open on its main. Since is the
// clock's start, the first merge until a pass wrote it.
func (k *ReleaseKeeper) Hold(ctx context.Context, repository string) (Hold, error) {
	row, err := k.Store.Release(ctx, repository)
	if err != nil || repository != k.Own.String() && row.AsksAt.IsZero() {
		return Hold{}, err
	}
	all, err := k.Store.OpenGroups(ctx)
	if err != nil || len(all[repository]) == 0 {
		return Hold{}, err
	}
	groups := all[repository]
	since := row.HeldSince
	if since.IsZero() {
		since = groups[0].FirstMerge
	}
	return Hold{Since: since, Groups: groups}, nil
}

// DeployNow releases main's newest push commit of held repository to
// production, by whom and via what: the commit, the clock's restart and
// the cleared ask in one write, the standing ask closed, the release
// recorded. The roller, /release and the next Pass do the rest.
func (k *ReleaseKeeper) DeployNow(ctx context.Context, repository, by, via string) (string, error) {
	h, err := k.Hold(ctx, repository)
	if err != nil {
		return "", err
	}
	if !h.Held() {
		return "", ErrNotHeld
	}
	_, runs, err := k.pushRuns(ctx, repository)
	if err != nil {
		return "", err
	}
	head := newestMain(runs)
	if head == nil {
		return "", fmt.Errorf("main of %s has no push run, so no commit can go to production: push to main first", repository)
	}
	now, commit := k.Now(), head.CommitSHA
	if err := k.Store.SetDeployNow(ctx, repository, commit, now); err != nil {
		return "", err
	}
	note := fmt.Sprintf("factoryd: %s chose Deploy now: main at %s goes to production. Closing this issue.", by, commit[:min(7, len(commit))])
	if err := k.closeAsk(ctx, repository, note); err != nil {
		log.Printf("release keeper: %s: close the ask after Deploy now: %v", repository, err)
	}
	k.released(ctx, repository, "deploy_now", h.Since, now, by, via, commit)
	return commit, nil
}

// project is repository's project, an error without one.
func (k *ReleaseKeeper) project(ctx context.Context, repository string) (Project, error) {
	p, ok, err := k.Store.ProjectByRepository(ctx, repository)
	if err == nil && !ok {
		err = fmt.Errorf("no project has the repository %s", repository)
	}
	return p, err
}

// pushRuns are repository's project and its newest push runs, read with
// the project's Actions token.
func (k *ReleaseKeeper) pushRuns(ctx context.Context, repository string) (Project, []ActionRun, error) {
	p, err := k.project(ctx, repository)
	if err != nil {
		return Project{}, nil, err
	}
	repo := p.Repo()
	runs, err := k.Forgejo.ActionRuns(ctx, k.Forgejo.Access(p).Actions, repo.Owner, repo.Name,
		url.Values{"event": {"push"}, "limit": {strconv.Itoa(rollRunsRead)}})
	return p, runs, err
}

// closeAsk closes repository's standing ask with comment, where one stands.
func (k *ReleaseKeeper) closeAsk(ctx context.Context, repository, comment string) error {
	card, ok, err := k.Store.ChainCard(ctx, repository, SourceProductionHeld)
	if err != nil || !ok || Final(card.Status) {
		return err
	}
	return closeIssueAsFactory(ctx, k.Store, k.Forgejo, card, comment)
}

// released records the end of repository's hold, held since since: how,
// by actor via via, and Deploy now's commit.
func (k *ReleaseKeeper) released(ctx context.Context, repository, how string, since, now time.Time, actor, via, commit string) {
	detail := map[string]any{"how": how, "held_since": since.UTC().Format(time.RFC3339), "hours": int(now.Sub(since) / time.Hour)}
	if commit != "" {
		detail["commit"] = commit
	}
	k.Timeline.RecordProjectRow(ctx, Event{At: now, Repository: repository, Kind: EventProductionReleased, Actor: actor, Via: via, Detail: detail})
}
