package factoryd

import (
	"context"
	"fmt"
	"log"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// LaneHuman is what an issue labelled human asks for: the human lane,
// which is no lane of a process.
const LaneHuman = "human"

// The lane labels: factoryd keeps one on each card's issue.
const (
	LabelBacklog = "backlog"
	LabelPlan    = "plan"
	LabelAgent   = "agent"
)

// laneLabels are the lane labels of lanes (plan only where there is a
// plan lane), which every project's repository carries beside BootLabels.
func laneLabels(lanes []policy.Lane) map[string]string {
	out := map[string]string{}
	for _, l := range lanes {
		switch l.Name {
		case policy.LaneBacklog:
			out[LabelBacklog] = "#c5def5"
		case policy.LanePlan:
			out[LabelPlan] = "#5319e7"
		case policy.LaneReady:
			out[LabelAgent] = BootLabels[LabelAgent]
		}
	}
	return out
}

// startingLabels ask for a starting lane; a refused entry takes them off.
var startingLabels = []string{LabelAgent, "bug", LabelPlan}

// laneOf is lane name of lanes and whether there is one.
func laneOf(lanes []policy.Lane, name string) (policy.Lane, bool) {
	i := slices.IndexFunc(lanes, func(l policy.Lane) bool { return l.Name == name })
	if i < 0 {
		return policy.Lane{}, false
	}
	return lanes[i], true
}

// IssueLane is intake's lane rule, the same for every process: human wins;
// then agent or bug asks for ready; then plan asks for plan where the
// process has a plan lane and reads as backlog elsewhere; then backlog or
// improvement is backlog; no lane label asks for ready.
func IssueLane(names []string, lanes []policy.Lane) string {
	has := func(n string) bool { return slices.Contains(names, n) }
	switch {
	case has(LaneHuman):
		return LaneHuman
	case has(LabelAgent) || has("bug"):
		return policy.LaneReady
	case has(LabelPlan):
		if _, ok := laneOf(lanes, policy.LanePlan); ok {
			return policy.LanePlan
		}
		return policy.LaneBacklog
	case has(LabelBacklog) || has("improvement"):
		return policy.LaneBacklog
	}
	return policy.LaneReady
}

// laneLabelled reports whether names ask for lane by its own label (agent,
// bug or plan), not by the default entry of an issue with no lane label.
func laneLabelled(names []string, lane string) bool {
	if lane == policy.LanePlan {
		return true
	}
	return slices.Contains(names, LabelAgent) || slices.Contains(names, "bug")
}

// MayMove reports whether user may put a card of repository into a lane
// moved by movedBy: writers write the repository, approvers are its
// project's.
func (in *Intake) MayMove(ctx context.Context, repository, user, movedBy string) (bool, error) {
	if movedBy == policy.MovedByApprovers {
		return slices.Contains(in.Policy().ApproversOf(repository), user), nil
	}
	return in.trusted(ctx, repository, user)
}

// refusal is the one line a refused entry into lane of repository
// comments for user.
func refusal(lane policy.Lane, repository, user string) string {
	who := "writers of " + repository
	if lane.MovedBy == policy.MovedByApprovers {
		who = "the approvers"
	}
	return fmt.Sprintf("factoryd: lane %s takes a card from %s; %s is none. The card stays in backlog.", lane.Name, who, user)
}

// refuseEntry lands issue number of repository, labelled names, in the
// backlog as the bot: the labels that asked for a starting lane off,
// backlog on, and one comment why, posted only once the labels are.
func (in *Intake) refuseEntry(ctx context.Context, via, repository string, number int64, names []string, why string) {
	kept := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(startingLabels, n) })
	if !slices.Contains(kept, LabelBacklog) {
		kept = append(kept, LabelBacklog)
	}
	repo, err := SplitRepo(repository)
	if err != nil {
		log.Printf("%s: issue #%d on %s: %v", via, number, repository, err)
		return
	}
	if err := in.Forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, number, kept); err != nil {
		// the starting label stays, so the next pass judges the entry again
		// and comments once the restore took
		log.Printf("%s: issue #%d on %s: restore the backlog label: %v", via, number, repository, err)
		return
	}
	if _, err := in.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, number, why); err != nil {
		log.Printf("%s: issue #%d on %s: comment the refused entry: %v", via, number, repository, err)
	}
	log.Printf("%s: issue #%d on %s: %s", via, number, repository, why)
}

// enterOnCreation is the entry of a new issue's card, which task holds
// with its author, into the lane its labels (names) ask for in project p:
// a lane label is judged against the poster, and so is the default entry
// (no lane label, or bug), which enters ready from anyone where ready is
// moved by writers and from an approver only where it is moved by the
// approvers. When a label event's sender (not the heal pass) brings the
// issue in, its lane label was added later and judges the sender too.
// It returns the card as it is to be created and, for a refused entry,
// which lands in the backlog, the line refuseEntry comments once the row
// exists.
func (in *Intake) enterOnCreation(ctx context.Context, p Project, task Task, names []string, defaultBranch, sender string) (Task, string, error) {
	name := IssueLane(names, p.Lanes)
	task.Lane, task.Process, task.Status = policy.LaneBacklog, p.Process, "backlog"
	if name == policy.LaneBacklog {
		return task, "", nil
	}
	lane, ok := laneOf(p.Lanes, name)
	if !ok {
		return Task{}, "", fmt.Errorf("project %s: process %q has no lane %s", p.Name, p.Process, name)
	}
	// bug is the default entry at creation, a lane label only when added
	byLabel := name == policy.LanePlan || slices.Contains(names, LabelAgent)
	allowed := lane.MovedBy == policy.MovedByWriters && (task.AuthorWriter || !byLabel)
	if lane.MovedBy == policy.MovedByApprovers {
		allowed = slices.Contains(in.Policy().ApproversOf(task.Repository), task.Author)
	}
	if !allowed {
		return task, refusal(lane, task.Repository, task.Author), nil
	}
	if byLabel && sender != "" && sender != task.Author {
		if ok, err := in.MayMove(ctx, task.Repository, sender, lane.MovedBy); err != nil {
			return Task{}, "", err
		} else if !ok {
			return task, refusal(lane, task.Repository, sender), nil
		}
	}
	sha, err := in.baseSHA(ctx, task.Repository, defaultBranch)
	if err != nil {
		return Task{}, "", err
	}
	task.Lane, task.Status, task.RequiredCapabilities, task.SHA = name, "queued", []string{lane.Class}, sha
	return task, "", nil
}

// keepLaneLabel leaves issue number of repository, labelled names, with
// lane's own label as its one lane label: the others (backlog, plan, and
// agent off ready) come off as the bot.
func (in *Intake) keepLaneLabel(ctx context.Context, via, repository string, number int64, names []string, lane string) {
	own := map[string]string{policy.LaneBacklog: LabelBacklog, policy.LanePlan: LabelPlan, policy.LaneReady: LabelAgent}[lane]
	kept := slices.DeleteFunc(slices.Clone(names), func(n string) bool {
		return n != own && (n == LabelBacklog || n == LabelPlan || n == LabelAgent)
	})
	repo, err := SplitRepo(repository)
	if len(kept) == len(names) || err != nil {
		return
	}
	if err := in.Forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, number, kept); err != nil {
		log.Printf("%s: issue #%d on %s: keep one lane label: %v", via, number, repository, err)
	}
}

// enterOnLabelAdded is a lane label added by sender to backlog card t,
// asking for starting lane name of project p.
func (in *Intake) enterOnLabelAdded(ctx context.Context, via string, p Project, t Task, name string, names []string, defaultBranch, sender string) error {
	return in.move(ctx, via, p, t, name, names, defaultBranch, sender)
}

// enterOnLabelRemoved is a lane label removed by sender from backlog card
// t, which leaves no lane label and so asks for lane ready of project p.
func (in *Intake) enterOnLabelRemoved(ctx context.Context, via string, p Project, t Task, names []string, defaultBranch, sender string) error {
	return in.move(ctx, via, p, t, policy.LaneReady, names, defaultBranch, sender)
}

// move takes backlog card t into starting lane name when sender may put it
// there, and starts its task; else the card stays in the backlog. A label
// event with no sender (the heal pass) or the bot's own moves nothing.
func (in *Intake) move(ctx context.Context, via string, p Project, t Task, name string, names []string, defaultBranch, sender string) error {
	if sender == "" || sender == in.BotUser {
		return nil
	}
	lane, ok := laneOf(p.Lanes, name)
	if !ok {
		return fmt.Errorf("project %s: process %q has no lane %s", p.Name, p.Process, name)
	}
	if name == policy.LanePlan && t.Parent != "" {
		in.refuseEntry(ctx, via, t.Repository, t.IssueNumber, names, "factoryd: lane plan takes no card of a plan: plans do not nest. The card stays in backlog.")
		return nil
	}
	allowed, err := in.MayMove(ctx, t.Repository, sender, lane.MovedBy)
	if err != nil {
		return err
	}
	if !allowed {
		in.refuseEntry(ctx, via, t.Repository, t.IssueNumber, names, refusal(lane, t.Repository, sender))
		return nil
	}
	// a retried card's replacement keeps the failed card's base, which its copied branch is built on
	sha := t.SHA
	if t.Source != SourcePlan || sha == "" {
		if sha, err = in.baseSHA(ctx, t.Repository, defaultBranch); err != nil {
			return err
		}
	}
	if ok, err := in.Store.EnterLane(ctx, t.ID, name, lane.Class, sha); err != nil || !ok {
		return err
	}
	log.Printf("%s: issue #%d on %s: %s moved task %s to lane %s", via, t.IssueNumber, t.Repository, sender, t.ID, name)
	in.keepLaneLabel(ctx, via, t.Repository, t.IssueNumber, names, name)
	t.Lane, t.Status = name, "queued"
	mirrorTaskState(ctx, in.Forgejo, t, t.Status)
	return in.enterLane(ctx, p, t, sender, defaultBranch)
}

// fileEntry is the entry of agent task t, which factoryd files as the bot
// (FileAgentTask), into project p's process: the default entry, lane
// ready, where ready is moved by writers, else the backlog. A roll takes
// ready's class and no lane. It returns the task and its labels.
func fileEntry(p Project, t Task, labels []string) (Task, []string, error) {
	ready, ok := laneOf(p.Lanes, policy.LaneReady)
	if !ok {
		return Task{}, nil, fmt.Errorf("project %s follows no process with a lane ready", p.Name)
	}
	t.Kind, t.Status, t.RequiredCapabilities = "agent", "queued", []string{ready.Class}
	switch {
	case t.Source == SourceRoll:
	case ready.MovedBy == policy.MovedByWriters:
		t.Lane, t.Process = policy.LaneReady, p.Process
	default:
		t.Lane, t.Process, t.Status, t.RequiredCapabilities = policy.LaneBacklog, p.Process, "backlog", nil
		labels = append(slices.DeleteFunc(slices.Clone(labels), func(n string) bool { return slices.Contains(startingLabels, n) }), LabelBacklog)
	}
	return t, labels, nil
}
