package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	if s.report != "" || s.startRef == "" {
		return wipOutcome{}
	}
	commit, bundle, err := r.postWorkerWip(ctx, workerStop, ns, pod, s.messageToken, r.endpointWait)
	switch {
	case err != nil:
		// factoryd stopping cuts the ask: its own shutdown is the last
		// word, and the stop's logs already say it
		if !errors.Is(err, errWorkerStop) {
			r.wipFailure(ctx, t, s, "ask", err)
		}
	case commit == "":
		// the tree equals the start's: there is nothing to ship
	default:
		head, err := r.pushWip(ctx, t, s, commit, bundle)
		if err != nil {
			r.wipFailure(ctx, t, s, "push", err)
			if errors.Is(err, ErrPushRefused) || errors.Is(err, ErrPushOverBound) {
				// the rules judged the work itself, and the work can
				// change: the refusal is the agent's to act on
				return wipOutcome{refusal: err.Error()}
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
func (r *Runner) wipFailure(ctx context.Context, t Task, s sessionSpec, at string, err error) {
	log.Printf("task %s: session %s: interim push, at the %s: %v", t.ID, s.id, at, err)
	r.record(ctx, t.ID, "wip_push_failed", map[string]any{"session": s.id, "at": at, "error": err.Error()})
}

// pushWip guards and pushes the work in progress the pod shipped. The
// scratch repo factoryd builds itself — bare, temporary, fetched by
// name, never touching the sandbox — holds the session's start first:
// the fork's task branch when a past push left one, else the repository's
// main, which a first session starts from. Then it takes the bundle —
// verified as a bundle before anything is taken from it, its objects
// fscked as they arrive — and refuses a WIP that does not descend from
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
	if _, err := git(ctx, own, login, "fetch", "-q", fork, "+"+branch+":"+branch); err != nil {
		if _, err := git(ctx, own, login, "fetch", "-q", r.repoURL(s.repo), "+"+prBase+":"+prBase); err != nil {
			return "", fmt.Errorf("the repositories hold neither the task branch nor main: %w", err)
		}
	}
	file := filepath.Join(own, "wip.bundle")
	if err := os.WriteFile(file, bundle, 0o600); err != nil {
		return "", err
	}
	// the body is verified as a bundle before anything is taken from it:
	// a body that is no bundle, whatever its header says it ships, costs
	// the interim push alone
	if _, err := git(ctx, own, "", "bundle", "verify", file); err != nil {
		return "", fmt.Errorf("the shipped body is no bundle: %w", err)
	}
	// the bundle carries the WIP on its own branch, with the start as
	// its prerequisite, so the fetch is answered from the bundle alone;
	// fscking the transfer keeps a lying object from landing in the repo
	if _, err := git(ctx, own, "", "-c", "transfer.fsckObjects=true", "fetch", "-q", file, "+refs/heads/wip:refs/heads/wip"); err != nil {
		return "", fmt.Errorf("fetch the shipped bundle: %w", err)
	}
	if _, err := git(ctx, own, "", "rev-parse", "--verify", "--quiet", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("the bundle ships no commit %s: %w", commit, err)
	}
	if _, err := git(ctx, own, "", "merge-base", "--is-ancestor", s.startRef, commit); err != nil {
		return "", fmt.Errorf("the shipped WIP %s does not descend from the session's start %s: nothing is pushed", commit, s.startRef)
	}
	tree, err := git(ctx, own, "", "rev-parse", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	// the pushed commit is factoryd's own: the shipped tree, the start as
	// its only parent, the factory's identity and message — never the
	// sandbox's commit
	head, err := gitEnv(ctx, own, "", wipIdentity, "commit-tree", tree, "-p", s.startRef, "-m", wipMessage(t, s))
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
	if _, err := git(ctx, own, login, "push", "-q", fork, "+"+head+":refs/heads/"+branch); err != nil {
		return "", err
	}
	return head, nil
}

// rulesGit is the push rules' git (PushGit) over factoryd's own: the
// hardened git call, with none of factoryd's environment, as PushGit's
// doc names the interim push's git.
type rulesGit struct{ ctx context.Context }

func (g rulesGit) Git(dir string, args ...string) (string, error) {
	return git(g.ctx, dir, "", args...)
}

func (g rulesGit) RunGit(dir string, env []string, args ...string) (string, error) {
	return gitEnv(g.ctx, dir, "", env, args...)
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
