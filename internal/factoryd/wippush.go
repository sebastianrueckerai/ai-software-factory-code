package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The interim push of a session's work in progress, factoryd's half of
// the check-in's shipping: the worker's half (its /wip, which stages one
// WIP commit of its own on the session's start and ships it as a bundle)
// answers, and this half asks, guards and pushes.

// wipOutcome is one interim push's result: the head it landed on the task
// branch, and the sentence of a push rule's refusal when one refused the
// work — a refusal the agent can act on, so it reaches the session and
// the check-in's ask (runner.go). A failure that is no refusal — a pod
// that hangs, a body that is no bundle, a WIP from another history — is
// factoryd's or the sandbox's own, and lands nowhere the agent sees.
type wipOutcome struct {
	head    string
	refusal string
}

// interimSlot is the one slot of interimPush.
var interimSlot = make(chan struct{}, 1)

// interimPush is the check-in's interim push of the session's work in
// progress: it asks the running pod's /wip for the work the agent holds,
// and pushes what the push rules (pushrules.go) let it push to the task
// branch under factoryd's own credential, the projects token, never a
// token the session holds. A session that dies anyway loses only the
// minutes since the last check-in, so nothing here stops or answers
// anything: every failure is logged and recorded, and the check-in runs
// on — the work stays in the sandbox and the next check-in retries. A
// report session ships no card's code and pushes nothing, and a session
// whose start never resolved pushes nothing either. A push that lands
// records the wip_pushed timeline step with the commit it landed.
func (r *Runner) interimPush(ctx context.Context, t Task, s sessionSpec, ns, pod string, workerStop <-chan struct{}) wipOutcome {
	if s.report != "" || !wipCommitID.MatchString(s.startRef) {
		return wipOutcome{}
	}
	// one interim push at a time: each holds a capped git and the shipped
	// bundle, and the container has room for one
	select {
	case interimSlot <- struct{}{}:
		defer func() { <-interimSlot }()
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			r.wipFailure(ctx, t, s, "queue", "another interim push held the slot until the check-in's deadline")
		}
		return wipOutcome{}
	}
	commit, bundle, err := r.postWorkerWip(ctx, workerStop, ns, pod, s.messageToken, r.endpointWait)
	switch {
	case err != nil:
		// factoryd stopping cuts the ask: its own shutdown is the last
		// word, and the stop's logs already say it
		if !errors.Is(err, errWorkerStop) {
			r.wipFailure(ctx, t, s, "ask", r.Scrub.String(err.Error()))
		}
	case commit == "":
		// the tree equals the start's: there is nothing to ship
	default:
		head, err := r.pushWip(ctx, t, s, commit, bundle)
		if err != nil {
			text := r.Scrub.String(err.Error())
			r.wipFailure(ctx, t, s, "push", text)
			if errors.Is(err, ErrPushRefused) || errors.Is(err, ErrPushOverBound) {
				// the rules judged the work itself, and the work can
				// change: the refusal is the agent's to act on
				return wipOutcome{refusal: "factoryd's interim push of your work in progress was refused: " + text}
			}
			return wipOutcome{}
		}
		r.record(ctx, t.ID, "wip_pushed", map[string]any{"session": s.id, "commit": head})
		return wipOutcome{head: head}
	}
	return wipOutcome{}
}

// wipFailure records one interim push that did not land: logged for the
// operator, and a wip_push_failed timeline step for the session's story.
func (r *Runner) wipFailure(ctx context.Context, t Task, s sessionSpec, at, text string) {
	log.Printf("task %s: session %s: interim push, at the %s: %s", t.ID, s.id, at, text)
	r.record(ctx, t.ID, "wip_push_failed", map[string]any{"session": s.id, "at": at, "error": text})
}

// pushWip guards and pushes the work in progress the pod shipped. The
// scratch repo factoryd builds itself — bare, temporary, fetched by
// name, never touching the sandbox — holds the session's start first:
// the fork's task branch when a past push left one, else the repository's
// main, which a first session starts from. Then it takes the bundle —
// verified as a bundle before anything is taken from it, its pack
// indexed under git's strict object checks — and refuses a WIP that does not descend from
// the session's start, the shape the worker's own /wip builds its WIP
// in: a commit from another history is not the session's work. What it
// pushes is factoryd's own commit, built from the shipped tree alone —
// the start as its only parent, the factory's identity and attribution
// message (wipIdentity, wipMessage) — never the sandbox's commit, whose
// author and message are the agent's to forge. The push rules judge the
// diff start..WIP as they judge the end push's, and a clean diff pushes
// under the projects token's basic login, as the GitHub pass fetches the
// factory's repositories with it (github.go), a forced refspec: each
// interim push replaces the branch's tip with the newest WIP tree, no
// history kept between check-ins, and the session's end push — which
// lands its own commit the same way — cannot be refused for the
// replacement. It answers the commit it landed.
func (r *Runner) pushWip(ctx context.Context, t Task, s sessionSpec, commit string, bundle []byte) (string, error) {
	own, err := bareRepo(ctx)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(own) // #nosec G104 -- a temporary scratch repository
	login := basicLogin(r.cfg.ProjectsUser, r.forgejo.ProjectsToken)
	fork, branch := r.repoURL(r.forgejo.Fork(s.repo)), taskBranch(t)
	tip := ""
	if _, err := git(ctx, own, login, "fetch", "-q", fork, "+"+branch+":"+branch); err == nil {
		if tip, err = git(ctx, own, "", "rev-parse", "--verify", branch); err != nil {
			return "", err
		}
	} else {
		if _, err := git(ctx, own, login, "fetch", "-q", r.repoURL(s.repo), "+"+prBase+":"+prBase); err != nil {
			return "", fmt.Errorf("the repositories hold neither the task branch nor main: %w", err)
		}
	}
	file := filepath.Join(own, "wip.bundle")
	if err := os.WriteFile(file, bundle, 0o600); err != nil {
		return "", err
	}
	// every git call from here reads what the sandbox shipped: each runs
	// under the address-space cap, with its output capped
	lim := func(maxOut int, stdin io.Reader) gitLimits {
		return gitLimits{stdin: stdin, maxOut: maxOut, addressSpace: wipGitMemory}
	}
	run := func(env []string, args ...string) (string, error) {
		return gitRun(ctx, own, "", env, lim(wipGitOutput, nil), args...)
	}
	// the body is verified as a bundle before anything is taken from it:
	// a body that is no bundle, whatever its header says it ships, costs
	// the interim push alone
	if _, err := run(nil, "bundle", "verify", file); err != nil {
		return "", fmt.Errorf("the shipped body is no bundle: %w", err)
	}
	// the bundle's pack is indexed under git's strict object checks, which
	// a fetch from a bundle does not apply: a lying object never lands.
	// The bundle's own ref line names the WIP, and its prerequisites are
	// the objects fetched above
	hdr, pack, ok := bytes.Cut(bundle, []byte("\n\n"))
	if !ok {
		return "", errors.New("the shipped body is no bundle: no header end")
	}
	if got := bundleCommit(hdr); got != commit {
		return "", fmt.Errorf("the bundle ships commit %q, not the %s the answer names", got, commit)
	}
	if _, err := gitRun(ctx, own, "", nil, lim(wipGitOutput, bytes.NewReader(pack)), "index-pack", "--strict", "--fix-thin", "--stdin"); err != nil {
		return "", fmt.Errorf("index the shipped bundle: %w", err)
	}
	if _, err := run(nil, "rev-parse", "--verify", "--quiet", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("the bundle ships no commit %s: %w", commit, err)
	}
	if _, err := run(nil, "merge-base", "--is-ancestor", s.startRef, commit); err != nil {
		return "", fmt.Errorf("the shipped WIP %s does not descend from the session's start %s: nothing is pushed", commit, s.startRef)
	}
	if err := checkShippedSize(ctx, own, commit, s.startRef, lim); err != nil {
		return "", err
	}
	tree, err := run(nil, "rev-parse", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	// the pushed commit is factoryd's own: the shipped tree, the start as
	// its only parent, the factory's identity and message — never the
	// sandbox's commit
	head, err := run(wipIdentity, "commit-tree", tree, "-p", s.startRef, "-m", wipMessage(t, s))
	if err != nil {
		return "", err
	}
	g, bound := rulesGit{ctx}, 0
	if sizeBounded(t, s) {
		bound = EffectiveLineBound(t.Size, s.policy.Risk.LargeLines, t.Usage)
	}
	// the start holds the main the session started from, so the
	// line-bound rule judges the session's own diff and its
	// merge-with-main recovery cannot land — what no merge can fit waits
	// for the session's end push, which owns the recovery
	if head, _, err = FitBound(g, own, head, tree, s.startRef, s.startRef, s.startRef, bound, wipIdentity, wipMessage(t, s)); err != nil {
		return "", err
	}
	if err := RefuseWorkflowDirChange(g, own, s.startRef, head); err != nil {
		return "", err
	}
	if err := RefuseMisnamedMigrations(g, s.policy.Risk.MigrationGlobs(), own, s.startRef, head); err != nil {
		return "", err
	}
	if _, err := gitRun(ctx, own, login, nil, lim(wipGitOutput, nil), "push", "-q", "--force-with-lease=refs/heads/"+branch+":"+tip, fork, head+":refs/heads/"+branch); err != nil {
		return "", err
	}
	return head, nil
}

// The bounds on what a pod ships, all far above any card's real work (a
// card's diff is bounded at hundreds to a few thousand lines).
const (
	// wipGitOutput bounds a git call whose answer is an id, a flag or a
	// one-line failure: bundle verify prints a line per missing prerequisite.
	wipGitOutput = 4 << 10
	// wipRulesOutput bounds the push rules' listings: a diff's file names
	// or a directory's top-level entries, a few MiB at 100,000 entries.
	wipRulesOutput = 8 << 20
	// wipListOutput bounds the list of shipped objects: about 100,000.
	wipListOutput = 4 << 20
	// wipMaxObject bounds one shipped object: a file this size is no
	// card's source, and git inflates and diffs it whole.
	wipMaxObject = 8 << 20
	// wipMaxTree bounds one shipped directory: about 50,000 entries.
	wipMaxTree = 2 << 20
	// wipMaxTotal bounds the shipped objects together, four times the
	// bundle cap a pack of text compresses by.
	wipMaxTotal = 64 << 20
	// wipGitMemory is the address space any git call on shipped bytes may
	// map. One push runs at a time (interimSlot), and factoryd's container
	// holds 512 MiB: its own peak (121 MiB observed) and the 16 MiB body
	// leave 375, so the cap keeps 119 MiB of margin. A real pack needs the
	// pack in flight, the 32 MiB delta base cache (gitConfig), one inflated
	// object (at most wipMaxObject) and git's libraries: well under it.
	wipGitMemory = 256 << 20
)

// checkShippedSize refuses work no card makes before any diff or listing
// reads it: it lists the objects the commit adds past the start, reads
// their sizes from their headers, and holds each to wipMaxObject
// (wipMaxTree for a directory) and the sum to wipMaxTotal. A list or a
// size output past its cap is itself too large.
func checkShippedSize(ctx context.Context, own, commit, start string, lim func(int, io.Reader) gitLimits) error {
	tooLarge := func(why string) error {
		return fmt.Errorf("%w: the shipped work is too large (%s): no card's work comes near; keep to the card's changes", ErrPushRefused, why)
	}
	list, err := gitRun(ctx, own, "", nil, lim(wipListOutput, nil), "rev-list", "--objects", commit, "^"+start)
	if errors.Is(err, errGitOutput) {
		return tooLarge("more than 100,000 new objects")
	}
	if err != nil {
		return err
	}
	var ids strings.Builder
	for line := range strings.SplitSeq(list, "\n") {
		id, _, _ := strings.Cut(line, " ")
		ids.WriteString(id + "\n")
	}
	sizes, err := gitRun(ctx, own, "", nil, lim(wipListOutput, strings.NewReader(ids.String())), "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	if errors.Is(err, errGitOutput) {
		return tooLarge("more than 100,000 new objects")
	}
	if err != nil {
		return err
	}
	total := 0
	for line := range strings.SplitSeq(sizes, "\n") {
		typ, n, _ := strings.Cut(line, " ")
		size, err := strconv.Atoi(n)
		if err != nil {
			return fmt.Errorf("the shipped objects' sizes: %q", line)
		}
		if size > wipMaxObject || typ == "tree" && size > wipMaxTree {
			return tooLarge(fmt.Sprintf("a %s of %d bytes", typ, size))
		}
		total += size
	}
	if total > wipMaxTotal {
		return tooLarge(fmt.Sprintf("%d bytes of new objects", total))
	}
	return nil
}

// bundleCommit is the commit a bundle header's ref line names for
// refs/heads/wip, or "".
func bundleCommit(header []byte) string {
	for _, line := range strings.Split(string(header), "\n") {
		if id, ref, ok := strings.Cut(line, " "); ok && ref == "refs/heads/wip" {
			return id
		}
	}
	return ""
}

// rulesGit is the push rules' git (PushGit) over factoryd's own: the
// capped, hardened git call, with none of factoryd's environment, as PushGit's
// doc names the interim push's git.
type rulesGit struct{ ctx context.Context }

// rulesLimits caps the rules' calls on shipped objects: memory, and an
// output that fails rather than reads cut off.
var rulesLimits = gitLimits{maxOut: wipRulesOutput, addressSpace: wipGitMemory}

func (g rulesGit) Git(dir string, args ...string) (string, error) {
	return gitRun(g.ctx, dir, "", nil, rulesLimits, args...)
}

func (g rulesGit) RunGit(dir string, env []string, args ...string) (string, error) {
	return gitRun(g.ctx, dir, "", env, rulesLimits, args...)
}

// wipIdentity and wipMessage are the identity and message of every
// commit the interim push writes: the commit factoryd builds from the
// shipped tree (pushWip) and the line-bound rule's merge of it — the
// attribution (forgejo.go) of a work-in-progress commit, as the worker's
// end push writes its own.
var wipIdentity = []string{
	"GIT_AUTHOR_NAME=factory-agent", "GIT_AUTHOR_EMAIL=factory-agent@factory.local",
	"GIT_COMMITTER_NAME=factory-agent", "GIT_COMMITTER_EMAIL=factory-agent@factory.local",
}

// wipMessage is the interim push's commit message.
func wipMessage(t Task, s sessionSpec) string {
	return Attribution{Role: "agent", Task: t.ID, Session: s.id, Human: t.Author}.
		Message("factory task " + t.ID + ": work in progress")
}
