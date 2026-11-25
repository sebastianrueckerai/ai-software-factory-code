package factoryd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/follow"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// FollowMoves starts and finds a project's one move of the follow routine,
// the Temporal workflow follow/<project>.
type FollowMoves interface {
	StartFollowMove(ctx context.Context, in FollowMoveInput) error
	// RunningFollowMove is the move that runs for project, if one does.
	RunningFollowMove(ctx context.Context, project string) (FollowMoveInput, bool, error)
}

// FollowDeps are what a move reads and writes beside the runner's own: the
// workflow that runs it, the cards that stand, the alerts that fired and
// the dependencies' probes.
type FollowDeps struct {
	Moves FollowMoves
	Cards interface {
		StandingQuestion(ctx context.Context, repository, marker string) (Task, bool, error)
	}
	Alerts AlertRowReader
	// Probe runs one pass of the dependency probes and answers each failing
	// one with its error.
	Probe func(ctx context.Context) []string
}

// followMarker starts the ask of a failure card, which a standing question
// finds.
const followMarker = "The follow move of "

// followBranch is the branch of attempt n of the move of part to the target
// the changes make: factory/follow-<part>-<10 hex of the changes>, with -<n>
// from the second attempt on.
func followBranch(part string, changes map[string][]byte, attempt int) string {
	h := sha256.New()
	for _, p := range slices.Sorted(maps.Keys(changes)) {
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(changes[p]))
		h.Write(changes[p])
	}
	b := "factory/follow-" + part + "-" + hex.EncodeToString(h.Sum(nil))[:10]
	if attempt > 1 {
		b += "-" + strconv.Itoa(attempt)
	}
	return b
}

// TreeFiles is every file of ref's tree in repo, by path, read from the
// repository's archive.
func (f *Forgejo) TreeFiles(ctx context.Context, repo Repo, ref string) (map[string][]byte, error) {
	body, err := f.doLimit(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/archive/%s.tar.gz", repo.Owner, repo.Name, ref), nil, http.StatusOK, f.callAuth(), 256<<20, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("the archive of %s at %s: %w", repo, ref, err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the archive of %s at %s: %w", repo, ref, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		_, name, _ := strings.Cut(h.Name, "/")
		if files[name], err = io.ReadAll(tr); err != nil {
			return nil, fmt.Errorf("the archive of %s at %s: %w", repo, ref, err)
		}
	}
}

// followPass is a run of a pull-request routine of pass follow: one move of
// the project's parts to the upstream lists, a program's work that starts
// no session. It answers the run's outcome and its report line. A run ends
// with the line that names the move that runs, the failure card that
// stands, what is behind or nothing; else it opens the pull request of the
// first part that is behind, whose target has no attempt yet, and starts the
// workflow that carries it to its merge and its judgment.
func (r *Runner) followPass(ctx context.Context, p Project, rt policy.Routine, run int) (outcome, note string, err error) {
	if r.Follow == nil {
		return "", "", terminalError("routine %s: the follow pass needs a factoryd wired for it, and this one is not", rt.Name)
	}
	if mv, running, err := r.Follow.Moves.RunningFollowMove(ctx, p.Name); err != nil || running {
		return "held", fmt.Sprintf("held: the move of part %s runs, pull request #%d", mv.Part, mv.PR), err
	}
	if card, standing, err := r.Follow.Cards.StandingQuestion(ctx, p.Repository, followMarker); err != nil || standing {
		return "held", fmt.Sprintf("held: the failure card %s is open: answer it, and the next run goes on", ShownName(card)), err
	}
	repo := p.Repo()
	_, results, err := r.followBehind(ctx, p)
	if err != nil {
		return "", "", err
	}
	parts := r.cfg.Policy().Projects[p.Name].Parts
	behind, tried := 0, []string{}
	for _, res := range results {
		if res.Behind() == 0 {
			continue
		}
		behind++
		branch := followBranch(res.Part.Name, res.Changes, 1)
		pr, err := r.forgejo.FindPullRequestByHead(ctx, repo.Owner, repo.Name, prBase, r.forgejo.Fork(repo), branch)
		if err != nil {
			return "", "", err
		}
		if pr != nil && pr.State != "open" {
			tried = append(tried, res.Part.Name)
			continue
		}
		in, err := r.startMove(ctx, p, rt, run, res, branch, pr)
		if err != nil {
			return "", "", err
		}
		return "reported", moveLine(in), nil
	}
	if behind == 0 {
		line := "nothing is behind"
		if hand := countLane(parts, policy.LaneHand); hand > 0 {
			line += fmt.Sprintf("; parts of the hand lane: %d", hand)
		}
		return "no change", line, nil
	}
	return "no change", fmt.Sprintf("%s behind, and each target has its attempt already: %s. A newer version of the lists makes a new target", plural(behind, "part"), strings.Join(tried, ", ")), nil
}

// followBehind runs the follow program over main's tree as the bot, and
// answers main's head and the result of each part.
func (r *Runner) followBehind(ctx context.Context, p Project) (string, []follow.Result, error) {
	repo := p.Repo()
	head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return "", nil, fmt.Errorf("main's head: %w", err)
	}
	files, err := r.forgejo.TreeFiles(ctx, repo, head)
	if err != nil {
		return "", nil, err
	}
	results, err := follow.Run(files, r.cfg.Policy().Projects[p.Name].Parts, r.followStore())
	if err != nil {
		return "", nil, terminalError("the follow program refuses main at %s:\n%v", ShortSHA(head), err)
	}
	return head, results, nil
}

func countLane(parts []policy.Part, lane string) int {
	n := 0
	for _, p := range parts {
		if p.Lane == lane {
			n++
		}
	}
	return n
}

// followStore is the store of the outside files and images, which the bot
// reads as it reads the code.
func (r *Runner) followStore() follow.Store {
	return follow.HTTPStore{Base: r.forgejo.BaseURL, User: r.forgejo.BotUser, Password: r.forgejo.BotPassword, Client: r.forgejo.HTTP}
}

// moveLine is the report line of a move that started.
func moveLine(in FollowMoveInput) string {
	first := in.Moves[0]
	if more := len(in.Moves) - 1; more > 0 {
		first += fmt.Sprintf(" and %s more", plural(more, "reference"))
	}
	return fmt.Sprintf("part %s (%s): %s, pull request #%d", in.Part, in.Lane, first, in.PR)
}

// startMove carries part res of project p to its pull request, unless
// adopt names the open one of an attempt that stopped before its workflow
// started, and starts the workflow that moves it on.
func (r *Runner) startMove(ctx context.Context, p Project, rt policy.Routine, run int, res follow.Result, branch string, adopt *PullRequestRef) (FollowMoveInput, error) {
	repo, fork := p.Repo(), r.forgejo.Fork(p.Repo())
	in := FollowMoveInput{Project: p.Name, Routine: rt.Name, Run: run, Part: res.Part.Name, Lane: res.Part.Lane, Notes: res.Part.Notes, After: res.Part.After}
	for _, m := range res.Moves {
		in.Moves = append(in.Moves, fmt.Sprintf("%s: %s to %s", m.Key, m.Before, m.After))
	}
	if adopt != nil {
		in.PR = adopt.Number
	} else {
		if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
			return in, err
		}
		if err := r.forgejo.SyncFork(ctx, fork); err != nil {
			return in, err
		}
		changes, err := fileChanges(ctx, r.forgejo, fork, prBase, res.Changes, nil)
		if err != nil {
			return in, err
		}
		title := "Follow the lists: part " + res.Part.Name
		if _, err := r.forgejo.CommitFiles(ctx, fork.Owner, fork.Name, prBase, branch, Attribution{Role: "follow"}, title, changes); err != nil {
			return in, err
		}
		pr, err := r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, title, followBody(in))
		if err != nil {
			return in, errors.Join(err, r.forgejo.DeleteBranch(context.WithoutCancel(ctx), fork.Owner, fork.Name, branch))
		}
		in.PR = pr.Number
	}
	return in, r.Follow.Moves.StartFollowMove(ctx, in)
}

// followBody is the text of a move's pull request.
func followBody(in FollowMoveInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The follow routine moves part %s, lane %s, to the versions of the upstream lists.\n\n", in.Part, in.Lane)
	for _, m := range in.Moves {
		b.WriteString("- " + m + "\n")
	}
	if in.Notes != "" {
		b.WriteString("\nRelease notes: " + in.Notes + "\n")
	}
	if in.Lane == policy.LaneAlone {
		b.WriteString("\nThe factory merges it once its checks are green, and judges the merge.\n")
	} else {
		b.WriteString("\nA human merges it or closes it. Closing it leaves the part until its next version.\n")
	}
	return b.String()
}
