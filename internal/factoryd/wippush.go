package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
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
	parts   []wipPart
}

// wipPart is one file of the pushed work and the lines it changes
// (added plus deleted), the parts a card over its bound is cut along.
type wipPart struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// interimSlot is the one slot of the interim push's body handling, and
// wipSlotWait the longest a check-in waits for it. A var, so a test
// shortens it.
var (
	interimSlot = make(chan struct{}, 1)
	wipSlotWait = 2 * time.Minute
)

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
	commit, bundle, err := r.postWorkerWip(ctx, workerStop, ns, pod, s.messageToken, r.endpointWait)
	if bundle != "" {
		defer os.Remove(bundle) // #nosec G104 -- the shipped bundle's temporary file
	}
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
		// one interim push at a time past the ask: each holds a capped git
		// and the shipped bundle, and the container has room for one. The
		// wait has its own bound, so a slow push ahead delays a check-in
		// by minutes, not by its session's deadline
		wait := time.NewTimer(wipSlotWait)
		defer wait.Stop()
		select {
		case interimSlot <- struct{}{}:
			defer func() { <-interimSlot }()
		case <-wait.C:
			r.wipFailureFresh(ctx, t, s, "queue", "another interim push held the slot for "+wipSlotWait.String())
			return wipOutcome{}
		case <-ctx.Done():
			return wipOutcome{}
		}
		head, parts, err := r.pushWip(ctx, t, s, commit, bundle)
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
		return wipOutcome{head: head, parts: parts}
	}
	return wipOutcome{}
}

// wipFailure records one interim push that did not land: logged for the
// operator, and a wip_push_failed timeline step for the session's story.
func (r *Runner) wipFailure(ctx context.Context, t Task, s sessionSpec, at, text string) {
	log.Printf("task %s: session %s: interim push, at the %s: %s", t.ID, s.id, at, text)
	r.record(ctx, t.ID, "wip_push_failed", map[string]any{"session": s.id, "at": at, "error": text})
}

// wipFailureFresh records a failure on a context that outlives an expired
// one: the timeline's write needs a live context.
func (r *Runner) wipFailureFresh(ctx context.Context, t Task, s sessionSpec, at, text string) {
	fresh, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	r.wipFailure(fresh, t, s, at, text)
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
// diff start..WIP as they judge the end push's, except the card's line
// bound, which only the end push holds, and a clean diff pushes
// under the projects token's basic login, as the GitHub pass fetches the
// factory's repositories with it (github.go), a forced refspec: each
// interim push replaces the branch's tip with the newest WIP tree, no
// history kept between check-ins, and the session's end push — which
// lands its own commit the same way — cannot be refused for the
// replacement. It answers the commit it landed and the files it holds.
func (r *Runner) pushWip(ctx context.Context, t Task, s sessionSpec, commit, bundle string) (string, []wipPart, error) {
	own, err := bareRepo(ctx)
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(own) // #nosec G104 -- a temporary scratch repository
	login := basicLogin(r.cfg.ProjectsUser, r.forgejo.ProjectsToken)
	fork, branch := r.repoURL(r.forgejo.Fork(s.repo)), taskBranch(t)
	// every git call here reads what the sandbox or an earlier push put on
	// a branch: each runs under the address-space cap, with its output capped
	lim := func(maxOut int, stdin io.Reader) gitLimits {
		return gitLimits{stdin: stdin, maxOut: maxOut, addressSpace: wipGitMemory}
	}
	run := func(env []string, args ...string) (string, error) {
		return gitRun(ctx, own, "", env, lim(wipGitOutput, nil), args...)
	}
	fetch := func(remote, ref string) error {
		_, err := gitRun(ctx, own, login, nil, lim(wipGitOutput, nil), "fetch", "-q", remote, "+"+ref+":"+ref)
		return err
	}
	tip := ""
	switch err := fetch(fork, branch); {
	case err == nil:
		if tip, err = run(nil, "rev-parse", "--verify", branch); err != nil {
			return "", nil, err
		}
	case strings.Contains(err.Error(), "couldn't find remote ref") || strings.Contains(err.Error(), "not found"):
		// a first push: the fork has no task branch yet, and main is the start
		if err := fetch(r.repoURL(s.repo), prBase); err != nil {
			return "", nil, fmt.Errorf("the fork holds no task branch and main cannot be fetched: %w", err)
		}
	default:
		return "", nil, fmt.Errorf("fetch the task branch: %w", err)
	}
	// the header is read and bounded here, in Go, before git sees the file: a
	// body that is no bundle, whatever it says it ships, costs the interim
	// push alone, and a header git would spend seconds on never reaches it
	f, err := os.Open(bundle) // #nosec G304 -- the temporary file postWip made
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	size, buf := int64(0), make([]byte, wipHeaderMax)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	n, _ := f.ReadAt(buf, 0)
	hdr, _, ok := bytes.Cut(buf[:n], []byte("\n\n"))
	if !ok {
		return "", nil, errors.New("the shipped body is no bundle: no header end")
	}
	// the bundle's pack is indexed under git's strict object checks, which
	// a fetch from a bundle does not apply: a lying object never lands.
	// The bundle's own ref line names the WIP, and its prerequisites are
	// the objects fetched above
	if got := bundleCommit(hdr); got != commit {
		return "", nil, fmt.Errorf("the bundle ships commit %q, not the %s the answer names", got, commit)
	}
	prerequisites := bundlePrerequisites(hdr)
	if len(prerequisites) > wipMaxPrerequisites {
		return "", nil, fmt.Errorf("the bundle lists %d prerequisites, not the worker's one", len(prerequisites))
	}
	// list-heads reads the header only, so it maps no pack of the repository
	if _, err := run(nil, "bundle", "list-heads", bundle); err != nil {
		return "", nil, fmt.Errorf("the shipped body is no bundle: %w", err)
	}
	for _, id := range prerequisites {
		if _, err := run(nil, "cat-file", "-e", id+"^{commit}"); err != nil {
			return "", nil, fmt.Errorf("the bundle builds on commit %s, which neither the task branch nor main holds: nothing is pushed", id)
		}
	}
	if _, err := gitRun(ctx, own, "", nil, lim(wipGitOutput, io.NewSectionReader(f, int64(len(hdr)+2), size-int64(len(hdr)+2))), "index-pack", "--strict", "--fix-thin", "--stdin"); err != nil {
		return "", nil, fmt.Errorf("index the shipped bundle: %w", err)
	}
	if _, err := run(nil, "rev-parse", "--verify", "--quiet", commit+"^{commit}"); err != nil {
		return "", nil, fmt.Errorf("the bundle ships no commit %s: %w", commit, err)
	}
	if _, err := run(nil, "merge-base", "--is-ancestor", s.startRef, commit); err != nil {
		return "", nil, fmt.Errorf("the shipped WIP %s does not descend from the session's start %s: nothing is pushed", commit, s.startRef)
	}
	if err := checkShippedSize(ctx, own, commit, s.startRef, lim); err != nil {
		return "", nil, err
	}
	tree, err := run(nil, "rev-parse", commit+"^{tree}")
	if err != nil {
		return "", nil, err
	}
	// the pushed commit is factoryd's own: the shipped tree, the start as
	// its only parent, the factory's identity and message — never the
	// sandbox's commit
	head, err := run(wipIdentity, "commit-tree", tree, "-p", s.startRef, "-m", wipMessage(t, s))
	if err != nil {
		return "", nil, err
	}
	// no line bound: the push is the card's work in progress, which
	// survives a session that cannot finish within the bound, and the end
	// push owns the line-bound rule
	g := rulesGit{ctx}
	parts, err := wipParts(g, own, s.startRef, head)
	if err != nil {
		return "", nil, err
	}
	if err := RefuseWorkflowDirChange(g, own, s.startRef, head); err != nil {
		return "", nil, err
	}
	if err := RefuseMisnamedMigrations(g, s.policy.Risk.MigrationGlobs(), own, s.startRef, head); err != nil {
		return "", nil, err
	}
	if _, err := gitRun(ctx, own, login, nil, lim(wipGitOutput, nil), "push", "-q", "--force-with-lease=refs/heads/"+branch+":"+tip, fork, head+":refs/heads/"+branch); err != nil {
		return "", nil, err
	}
	return head, parts, nil
}

// wipPartsText lists the parts as "path lines" pairs for a stop's text.
func wipPartsText(parts []wipPart) string {
	items := make([]string, len(parts))
	for i, p := range parts {
		items[i] = fmt.Sprintf("%s %d", p.Path, p.Lines)
	}
	return strings.Join(items, ", ")
}

// wipParts lists the files the work changes against the start, the most
// changed lines first, at most wipPartsMax of them.
func wipParts(g PushGit, own, start, head string) ([]wipPart, error) {
	out, err := g.Git(own, "diff", "--numstat", "--no-renames", start, head)
	if err != nil {
		return nil, err
	}
	var parts []wipPart
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		added, _ := strconv.Atoi(f[0])
		deleted, _ := strconv.Atoi(f[1])
		parts = append(parts, wipPart{Path: f[2], Lines: added + deleted})
	}
	slices.SortStableFunc(parts, func(a, b wipPart) int { return cmp.Compare(b.Lines, a.Lines) })
	return parts[:min(len(parts), wipPartsMax)], nil
}

// The bounds on what a pod ships, all far above any card's real work (a
// card's diff is bounded at hundreds to a few thousand lines).
const (
	// wipPartsMax bounds the files a stop names.
	wipPartsMax = 40
	// wipMaxPrerequisites bounds the prerequisites of a bundle: the
	// worker's is one, the session's start.
	wipMaxPrerequisites = 16
	// wipHeaderMax bounds the bundle header read from the file.
	wipHeaderMax = 64 << 10
	// wipGitOutput bounds a git call whose answer is an id, a flag or a
	// one-line failure.
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
	// wipTreeOutput bounds the listing of the whole tree: 50,000 entries
	// at about 100 bytes.
	wipTreeOutput = 8 << 20
	// wipMaxEntries bounds the files of the shipped tree.
	wipMaxEntries = 50000
	// wipMaxTotal bounds the shipped objects together, and the blobs of the
	// shipped tree together: four times the bundle cap.
	wipMaxTotal = 64 << 20
	// wipGitMemory is the address space any git call on shipped bytes may
	// map. The container holds 512 MiB: factoryd's own peak (121 MiB
	// observed), the 16 MiB body, one capped child at a time in the whole
	// process (gitRun's slot), and the GitHub exports' git, which is not
	// capped: an export reads the factory's own repositories (a fetch of a
	// 186 MiB repository peaked at 100 MiB). 121 + 16 + 192 + 100 =
	// 429 MiB. The largest legitimate child measured was 45 MiB: a
	// real pack needs the pack in flight, the 32 MiB delta base cache and
	// one inflated object. pack.threads=1 (gitConfig) is needed under the
	// cap: each extra thread maps a stack and an arena.
	wipGitMemory = 192 << 20
)

// checkShippedSize refuses work no card makes before any diff or listing
// reads it: it lists the objects the commit adds past the start, reads
// their sizes from their headers, and holds each to wipMaxObject
// (wipMaxTree for a directory) and the sum to wipMaxTotal. A list or a
// size output past its cap is itself too large.
func checkShippedSize(ctx context.Context, own, commit, start string, lim func(int, io.Reader) gitLimits) error {
	tooLarge := func(why string) error {
		return fmt.Errorf("the shipped work is too large (%s)", why)
	}
	list, err := gitRun(ctx, own, "", nil, lim(wipListOutput, nil), "rev-list", "--objects", "--no-object-names", commit, "^"+start)
	if errors.Is(err, errGitOutput) {
		return tooLarge("more than 100,000 new objects")
	}
	if err != nil {
		return err
	}
	sizes, err := gitRun(ctx, own, "", nil, lim(wipListOutput, strings.NewReader(list+"\n")), "cat-file", "--batch-check=%(objecttype) %(objectsize)")
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
	// the tree as checked out: one object can stand for many paths
	files, err := gitRun(ctx, own, "", nil, lim(wipTreeOutput, nil), "ls-tree", "-r", "-l", "-z", commit)
	if errors.Is(err, errGitOutput) {
		return tooLarge("a tree listing past its bound")
	}
	if err != nil {
		return err
	}
	entries, blobs := 0, 0
	for entry := range strings.SplitSeq(files, "\x00") {
		if entry == "" {
			continue
		}
		meta, _, _ := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if len(fields) != 4 {
			return fmt.Errorf("the shipped tree's listing: %q", entry)
		}
		entries++
		if n, err := strconv.Atoi(fields[3]); err == nil {
			blobs += n
		}
	}
	if entries > wipMaxEntries || blobs > wipMaxTotal {
		return tooLarge(fmt.Sprintf("a tree of %d files and %d bytes", entries, blobs))
	}
	return nil
}

// bundlePrerequisites are the commit ids a bundle header lists as the
// objects its pack builds on: its lines that start with a dash. A line
// that holds no commit id is skipped; index-pack judges the pack.
func bundlePrerequisites(header []byte) []string {
	var ids []string
	for line := range strings.SplitSeq(string(header), "\n") {
		if id, _, _ := strings.Cut(strings.TrimPrefix(line, "-"), " "); strings.HasPrefix(line, "-") && wipCommitID.MatchString(strings.ToLower(id)) {
			ids = append(ids, strings.ToLower(id))
		}
	}
	return ids
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

// gitCapped is a git call on a repository a sandbox wrote to — a task
// branch, a pull request head — under the address-space cap and with its
// output capped.
func gitCapped(ctx context.Context, dir, login string, args ...string) (string, error) {
	return gitRun(ctx, dir, login, nil, gitLimits{maxOut: wipGitOutput, addressSpace: wipGitMemory}, args...)
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
// shipped tree (pushWip) — the attribution (forgejo.go) of a work-in-progress commit, as the worker's
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
