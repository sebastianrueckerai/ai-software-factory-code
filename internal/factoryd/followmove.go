package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The memo keys of a move's workflow: the input it started with, and the
// mark it sets once its judgment is over.
const (
	followMemoMove  = "move"
	followMemoEnded = "ended"
)

// The activities of a move.
const (
	ActivityFollowChecks = "FollowChecks"
	ActivityFollowLand   = "FollowLand"
	ActivityFollowJudge  = "FollowJudge"
	ActivityFollowEnd    = "FollowEnd"
	ActivityFollowAsk    = "FollowAsk"
	ActivityFollowPR     = "FollowPR"
	ActivityFollowClose  = "FollowClose"
)

// followPoll is how often a move in the human lane reads its pull request,
// and followHumanBound how long it waits for a human before it closes the
// pull request.
var (
	followPoll       = 5 * time.Minute
	followHumanBound = 14 * 24 * time.Hour
)

// FollowMoveInput is one move of the follow routine: the part and its pull
// request, with what the cards and the report line say of it. Run is the
// routine's run that opened it, 0 for a move a human's Try again started.
type FollowMoveInput struct {
	Project, Routine string
	Run              int
	Part, Lane       string
	PR               int64
	Moves            []string // "<reference>: <before> to <after>"
	Notes, After     string
}

// FollowVerdict is a step's answer: Ok, or Why it did not hold, with the
// text of what failed.
type FollowVerdict struct {
	Ok  bool
	Why string
}

// FollowLanded is the merge of a move's pull request: its commit on main and
// when it happened, or Why it did not merge.
type FollowLanded struct {
	Commit string
	At     time.Time
	Why    string
}

// FollowPullRequest is the move's pull request as a poll reads it.
type FollowPullRequest struct {
	Open, Merged bool
	Commit       string
	At           time.Time
}

// FollowEnding is how a move ends in its routine's run.
type FollowEnding struct{ Outcome, Note string }

// FollowMoveWorkflow is one move of the follow routine (workflow id
// follow/<project>): the pull request's checks, its merge, the judgment of
// the merge, and the next run. A step that does not hold ends the move as
// failed.
func FollowMoveWorkflow(ctx workflow.Context, in FollowMoveInput) error {
	waits := workflow.WithActivityOptions(ctx, followOptions(landChecksBound+mergeWaitBound+5*time.Minute, 0))
	judges := workflow.WithActivityOptions(ctx, followOptions(rolloutCap+rolloutSettle+followJudgeSlack, time.Minute))
	ends := workflow.WithActivityOptions(ctx, finishOptions())
	end := func(outcome, note string) error {
		return workflow.ExecuteActivity(ends, ActivityFollowEnd, in, FollowEnding{outcome, note}).Get(ctx, nil)
	}
	failed := func(why string) error { return end("failed", why) }

	var checks FollowVerdict
	if err := workflow.ExecuteActivity(waits, ActivityFollowChecks, in).Get(ctx, &checks); err != nil {
		return failed("the checks did not read: " + failureWords(err))
	}
	if !checks.Ok {
		return failed(checks.Why)
	}
	var landed FollowLanded
	if in.Lane == policy.LaneHuman {
		var err error
		if landed, err = awaitHuman(ctx, in, ends); err != nil {
			return failed("the human's merge did not read: " + failureWords(err))
		}
		if landed.Why != "" {
			return end("closed", landed.Why)
		}
	} else if err := workflow.ExecuteActivity(waits, ActivityFollowLand, in).Get(ctx, &landed); err != nil {
		return failed("the merge did not run: " + failureWords(err))
	} else if landed.Why != "" {
		return failed(landed.Why)
	}
	var judged FollowVerdict
	if err := workflow.ExecuteActivity(judges, ActivityFollowJudge, in, landed).Get(ctx, &judged); err != nil {
		return failed("the judgment did not run: " + failureWords(err))
	}
	if !judged.Ok {
		return failed(judged.Why)
	}
	if err := workflow.UpsertMemo(ctx, map[string]any{followMemoEnded: true}); err != nil {
		return err
	}
	return end("done", "moved: "+strings.Join(in.Moves, "; "))
}

// awaitHuman files the card that asks a human to merge the move's pull
// request and reads the pull request every followPoll until it is merged or
// closed, or followHumanBound passes and the pull request is closed. A
// landed with Why says why the move ends without a merge.
func awaitHuman(ctx workflow.Context, in FollowMoveInput, short workflow.Context) (FollowLanded, error) {
	if err := workflow.ExecuteActivity(short, ActivityFollowAsk, in).Get(ctx, nil); err != nil {
		return FollowLanded{}, err
	}
	deadline := workflow.Now(ctx).Add(followHumanBound)
	for {
		if err := workflow.Sleep(ctx, followPoll); err != nil {
			return FollowLanded{}, err
		}
		var pr FollowPullRequest
		if err := workflow.ExecuteActivity(short, ActivityFollowPR, in).Get(ctx, &pr); err != nil {
			return FollowLanded{}, err
		}
		switch {
		case pr.Merged:
			return FollowLanded{Commit: pr.Commit, At: pr.At}, nil
		case !pr.Open:
			return FollowLanded{Why: "the pull request was closed without a merge: the part stays as it is until the lists carry its next version"}, nil
		case workflow.Now(ctx).After(deadline):
			why := "no human decided in " + followHumanBound.String() + ", so the factory closed the pull request: the part stays as it is until the lists carry its next version"
			if err := workflow.ExecuteActivity(short, ActivityFollowClose, in, why).Get(ctx, nil); err != nil {
				return FollowLanded{}, err
			}
			return FollowLanded{Why: why}, nil
		}
	}
}

// followJudgeSlack is how long the judgment's activity may run past the
// waits it holds.
const followJudgeSlack = 10 * time.Minute

func followOptions(bound, heartbeat time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: bound,
		HeartbeatTimeout:    heartbeat,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 10 * time.Second, MaximumInterval: time.Minute, MaximumAttempts: 5,
			NonRetryableErrorTypes: []string{errTerminal}},
	}
}

// followRepo is the repository of the move's project.
func (r *Runner) followRepo(ctx context.Context, in FollowMoveInput) (Repo, error) {
	p, ok, err := r.Routines.Project(ctx, in.Project)
	if err != nil || !ok {
		return Repo{}, errors.Join(err, terminalError("move %s: no project %s in the store", in.Part, in.Project))
	}
	return p.Repo(), nil
}

// followStep records a step of a move, in words: a comment on its pull
// request, where a human reads the move, and an event.
func (r *Runner) followStep(ctx context.Context, repo Repo, in FollowMoveInput, step string) {
	if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, in.PR, "Follow routine: "+step); err != nil {
		log.Printf("follow %s: comment on #%d: %v", in.Part, in.PR, err)
	}
	if r.timeline == nil || r.timeline.Events == nil {
		return
	}
	if _, err := r.timeline.Events.Record(ctx, Event{Repository: repo.String(), Kind: EventFollowStep, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"project": in.Project, "routine": in.Routine, "run": in.Run, "part": in.Part, "pr": in.PR, "step": step}}); err != nil {
		log.Printf("follow %s: the step's event: %v", in.Part, err)
	}
}

// followChecks are the contexts the project's pull request of a move must
// show before it merges.
func (r *Runner) followChecks(in FollowMoveInput) []string {
	return r.cfg.Policy().Projects[in.Project].Chain.Checks
}

// FollowChecks waits for the checks of the move's pull request: each named
// by the project's chain checks reported, and none failed or pending.
func (r *Runner) FollowChecks(ctx context.Context, in FollowMoveInput) (FollowVerdict, error) {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return FollowVerdict{}, err
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, in.PR)
	if err != nil {
		return FollowVerdict{}, err
	}
	switch {
	case pr.Merged:
		return FollowVerdict{Ok: true}, nil
	case pr.State != "open":
		return FollowVerdict{Why: fmt.Sprintf("pull request #%d was closed without a merge", in.PR)}, nil
	}
	if why := r.forgejo.awaitChecks(ctx, repo, pr.HeadSHA, r.followChecks(in)); why != "" {
		return FollowVerdict{Why: why}, nil
	}
	r.followStep(ctx, repo, in, "the checks of pull request head "+ShortSHA(pr.HeadSHA)+" are green.")
	return FollowVerdict{Ok: true}, nil
}

// FollowLand merges the move's pull request as the bot once its checks hold,
// and answers the merge commit.
func (r *Runner) FollowLand(ctx context.Context, in FollowMoveInput) (FollowLanded, error) {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return FollowLanded{}, err
	}
	why := r.forgejo.LandPullRequest(ctx, repo, in.PR, Attribution{Role: "follow"}, "Follow the lists: part "+in.Part, nil, r.followChecks(in)...)
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, in.PR)
	switch {
	case err != nil:
		return FollowLanded{}, err
	case pr.Merged:
		r.followStep(ctx, repo, in, "merged as "+ShortSHA(pr.MergeCommit)+".")
		return FollowLanded{Commit: pr.MergeCommit, At: r.Clock()}, nil
	case why == "":
		why = fmt.Sprintf("pull request #%d was closed without a merge", in.PR)
	}
	return FollowLanded{Why: why}, nil
}

// followModelSystem is the one call of a judgment: the small class answers
// with one word.
const followModelSystem = `Answer with a JSON object {"ok": true}.`

// FollowJudge judges the merge commit of a move. The Flux statuses that the
// project's chain requires are success on it, a failure at once, none by
// rolloutCap a failure of that status; then the dependency probes answer and
// one call on the small class through the gateway returns; then no alert of
// the project's namespaces fired since the merge, after rolloutSettle.
func (r *Runner) FollowJudge(ctx context.Context, in FollowMoveInput, landed FollowLanded) (FollowVerdict, error) {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return FollowVerdict{}, err
	}
	tree := r.cfg.Policy()
	project := tree.Projects[in.Project]
	if why, err := r.followStatuses(ctx, repo, project.Chain.Requires, landed); err != nil || why != "" {
		return FollowVerdict{Why: why}, err
	}
	r.followStep(ctx, repo, in, "Flux applied "+ShortSHA(landed.Commit)+": "+strings.Join(project.Chain.Requires, ", ")+" are success.")
	if r.Follow.Probe != nil {
		if failing := r.Follow.Probe(ctx); len(failing) > 0 {
			return FollowVerdict{Why: "a dependency does not answer: " + strings.Join(failing, "; ")}, nil
		}
	}
	if err := r.followModelAnswers(ctx, in); err != nil {
		return FollowVerdict{Why: "the small model class does not answer through the gateway: " + err.Error()}, nil
	}
	r.followStep(ctx, repo, in, "the dependencies answer and the small model class answers.")
	until := r.Clock().Add(rolloutSettle)
	for r.Clock().Before(until) {
		activity.RecordHeartbeat(ctx)
		if err := r.pause(ctx, min(rollPoll, until.Sub(r.Clock()))); err != nil {
			return FollowVerdict{}, err
		}
	}
	fired, err := platformAlertsSince(ctx, r.Follow.Alerts, r.followNamespaces(ctx, in), landed.At)
	if err != nil {
		return FollowVerdict{}, err
	}
	if len(fired) > 0 {
		a := fired[0]
		return FollowVerdict{Why: fmt.Sprintf("the alert “%s” of namespace %s fired after the merge: %s", a.Source, a.Detail["namespace"], a.Detail["summary"])}, nil
	}
	r.followStep(ctx, repo, in, "no alert of the project's namespaces fired in the "+rolloutSettle.String()+" after the merge: the move is settled.")
	return FollowVerdict{Ok: true}, nil
}

// followNamespaces are the namespaces of the move's project.
func (r *Runner) followNamespaces(ctx context.Context, in FollowMoveInput) []string {
	p, _, _ := r.Routines.Project(ctx, in.Project)
	return p.Namespaces
}

// followStatuses waits for each of required to be success on the merge
// commit and answers why one is not: a failure or error status at once, a
// status that is not success rolloutCap after the merge.
func (r *Runner) followStatuses(ctx context.Context, repo Repo, required []string, landed FollowLanded) (string, error) {
	if len(required) == 0 {
		return "", nil
	}
	until := landed.At.Add(rolloutCap)
	for {
		st, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, landed.Commit)
		if err != nil {
			if !r.Clock().Before(until) {
				return "", err
			}
			log.Printf("follow: the statuses of %s: %v", ShortSHA(landed.Commit), err)
		}
		waiting := ""
		for _, name := range required {
			i := slices.IndexFunc(st.Statuses, func(s CommitStatus) bool { return s.Context == name })
			switch {
			case i < 0:
				waiting = name + " has not reported"
			case st.Statuses[i].State == "failure" || st.Statuses[i].State == "error":
				return fmt.Sprintf("the status “%s” of the merge commit %s is %s: %s", name, ShortSHA(landed.Commit), st.Statuses[i].State, st.Statuses[i].Description), nil
			case st.Statuses[i].State != "success":
				waiting = name + " is " + st.Statuses[i].State
			}
		}
		if waiting == "" {
			return "", nil
		}
		now := r.Clock()
		if !now.Before(until) {
			return fmt.Sprintf("the merge commit %s has no success status within %s: %s", ShortSHA(landed.Commit), rolloutCap, waiting), nil
		}
		activity.RecordHeartbeat(ctx)
		if err := r.pause(ctx, min(rollPoll, until.Sub(now))); err != nil {
			return "", err
		}
	}
}

// followModelAnswers is the one model call of a judgment: the small class
// through the gateway on the project's chat task, which the digest purpose
// admits on any task.
func (r *Runner) followModelAnswers(ctx context.Context, in FollowMoveInput) error {
	v := r.cfg.Policy()
	p, ok, err := r.Routines.Project(ctx, in.Project)
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("no project %s", in.Project))
	}
	task, err := r.store.EnsureChatTask(ctx, p.Name, p.Repository, v.Document.ChatOf(v.Projects[p.Name]).MonthlyEUR, r.Clock())
	if err != nil {
		return err
	}
	out, _, err := r.supervisor.Chat(ctx, task.ID, policy.Small, policy.PurposeDigest, v.Document.Limits.SupervisorTimeout, followModelSystem, map[string]string{"say": "ok"})
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("the answer is empty")
	}
	return nil
}

// platformAlertsSince are the alerts of namespaces that began at or after
// since and no one judged noise, firing now or resolved since: a firing a
// rollout's window held is a row without a card, and counts.
func platformAlertsSince(ctx context.Context, rows AlertRowReader, namespaces []string, since time.Time) ([]Alert, error) {
	all, err := rows.NewestAlertRows(ctx, AlertSigNoz, "")
	if err != nil {
		return nil, fmt.Errorf("alerts: %w", err)
	}
	var out []Alert
	for _, a := range all {
		ns, _ := a.Detail["namespace"].(string)
		at, _ := a.Detail["starts_at"].(string)
		start, err := time.Parse(time.RFC3339, at)
		if err == nil && slices.Contains(namespaces, ns) && !start.Before(since) && a.Verdict != VerdictNoise {
			out = append(out, a)
		}
	}
	return out, nil
}

// FollowEnd ends the move in its routine's run: the outcome and the note the
// run row keeps, a step on the pull request, and on a good move the
// routine's next run at once, so the chain goes on while a part is behind.
func (r *Runner) FollowEnd(ctx context.Context, in FollowMoveInput, e FollowEnding) error {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return err
	}
	note := fmt.Sprintf("part %s: %s", in.Part, e.Note)
	r.followStep(ctx, repo, in, e.Outcome+": "+e.Note)
	if in.Run > 0 {
		if err := r.Routines.EndRoutineRun(ctx, in.Project, in.Routine, in.Run, e.Outcome, note, "", 0, 0); err != nil {
			return err
		}
	}
	if e.Outcome != "done" {
		return nil
	}
	return r.cfg.Workflows.StartRoutineRun(ctx, RoutineRunInput{Project: in.Project, Routine: in.Routine, Trigger: TriggerRunNow, Due: r.Clock()})
}

// FollowAsk files the card that asks a human to merge the move's pull
// request, as the roller files its cards: Merge or Reject, with the
// versions, the maker's notes, what a human does after the merge, and the
// words that Reject leaves the part until its next version. A card of the
// pull request stands once.
func (r *Runner) FollowAsk(ctx context.Context, in FollowMoveInput) error {
	p, ok, err := r.Routines.Project(ctx, in.Project)
	if err != nil || !ok {
		return errors.Join(err, terminalError("move %s: no project %s in the store", in.Part, in.Project))
	}
	repo := p.Repo()
	have, err := r.store.TasksByPR(ctx, p.Repository, in.PR)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(have, func(t Task) bool { return t.Source == SourceFollow }) {
		return nil
	}
	b := SourceBriefs[SourceFollow](BriefFacts{Name: in.Part})
	b.Body = followBody(in)
	refusal := fmt.Sprintf("part %s is of the human lane, and moves %s", in.Part, strings.Join(in.Moves, "; "))
	if in.Notes != "" {
		refusal += ". The maker's release notes: " + in.Notes
	}
	if in.After != "" {
		refusal += ". After the merge: " + strings.TrimSuffix(strings.TrimSpace(in.After), ".")
	}
	refusal += ". Reject closes the pull request and leaves the part as it is until the lists carry its next version"
	card, _, err := FileFactoryPRTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, r.cfg.Policy().Document.Budget, p, b, nil,
		Task{ID: NewTaskID(), RiskClass: "standard", Objective: IssueObjective(b.Title, b.Body), Source: SourceFollow}, in.PR, refusal)
	if err != nil {
		return err
	}
	r.followStep(ctx, repo, in, "asked a human: card "+ShownName(card)+".")
	return nil
}

// FollowPR reads the move's pull request: open, or merged and with what
// commit and when.
func (r *Runner) FollowPR(ctx context.Context, in FollowMoveInput) (FollowPullRequest, error) {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return FollowPullRequest{}, err
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, in.PR)
	if err != nil {
		return FollowPullRequest{}, err
	}
	if pr.Merged {
		r.followStep(ctx, repo, in, "merged by "+cmp.Or(pr.MergedBy, "a human")+" as "+ShortSHA(pr.MergeCommit)+".")
	}
	return FollowPullRequest{Open: pr.State == "open", Merged: pr.Merged, Commit: pr.MergeCommit, At: pr.MergedAt}, nil
}

// FollowClose closes the move's pull request, for why.
func (r *Runner) FollowClose(ctx context.Context, in FollowMoveInput, why string) error {
	repo, err := r.followRepo(ctx, in)
	if err != nil {
		return err
	}
	if err := r.forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, in.PR); err != nil {
		return err
	}
	r.followStep(ctx, repo, in, "closed the pull request: "+why)
	return nil
}
