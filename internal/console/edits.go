package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/doclinks"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// cardRefs resolves the references of card t's brief for a: #N is the
// project's card of issue N, else any issue or pull request of the
// repository, and a document link a file of the project's document
// repository with the heading the renderer gives that id. A project with
// no document repository has no documents to hold a link to. err keeps
// the first read that failed, or the configuration that cannot read the
// documents, so the door answers with it and never with a refusal of a
// reference it could not check.
type cardRefs struct {
	ctx    context.Context
	c      *Console
	a      actor
	t      factoryd.Task
	err    error
	docs   factoryd.Repo
	tree   []string
	issues map[int]resolvedIssue
}

// resolvedIssue is #N: the project's card of it, else an issue or a pull
// request of the repository, which merged or not.
type resolvedIssue struct {
	title, state string
	ok           bool
	card         factoryd.Task
	pull, merged bool
}

// probe is a resolver over r's reads with an error of its own, so a
// reference it cannot check fails the probe and not r's door.
func (r *cardRefs) probe() *cardRefs {
	return &cardRefs{ctx: r.ctx, c: r.c, a: r.a, t: r.t, docs: r.docs, tree: r.tree, issues: r.issues}
}

func (r *cardRefs) Issue(n int) (string, string, bool) {
	i := r.resolve(n)
	return i.title, i.state, i.ok
}

// resolve is #n, read once.
func (r *cardRefs) resolve(n int) resolvedIssue {
	if i, ok := r.issues[n]; ok {
		return i
	}
	i, err := r.issue(n)
	if err != nil {
		r.stop(err)
		return resolvedIssue{}
	}
	if r.issues == nil {
		r.issues = map[int]resolvedIssue{}
	}
	r.issues[n] = i
	return i
}

func (r *cardRefs) issue(n int) (resolvedIssue, error) {
	if k, ok, err := r.c.cfg.Tasks.TaskByIssue(r.ctx, r.t.Repository, int64(n)); err != nil {
		return resolvedIssue{}, r.failure("the cards", err)
	} else if ok {
		return resolvedIssue{title: factoryd.CardTitle(k), state: k.Status, ok: true, card: k}, nil
	}
	repo, err := factoryd.SplitRepo(r.t.Repository)
	if err != nil {
		return resolvedIssue{}, refuse(http.StatusInternalServerError, fmt.Sprintf("The brief's references cannot be checked: the card's repository %q is no owner/name.", r.t.Repository))
	}
	var issue struct {
		Title, State string
		PullRequest  *struct{ Merged bool } `json:"pull_request"`
	}
	err = r.c.as(r.ctx, r.a.s, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), n), nil, http.StatusOK, &issue)
	var api *apiError
	switch {
	case errors.As(err, &api) && api.status == http.StatusNotFound:
		return resolvedIssue{}, nil
	case err != nil:
		return resolvedIssue{}, r.failure("issue #"+fmt.Sprint(n), err)
	}
	pr := issue.PullRequest
	return resolvedIssue{title: issue.Title, state: issue.State, ok: true, pull: pr != nil, merged: pr != nil && pr.Merged}, nil
}

func (r *cardRefs) Files(path string) []string {
	v, ok := r.c.inForce(r.ctx)
	if !ok {
		r.stop(r.failure("the policy", errors.New("not loaded")))
		return []string{path}
	}
	_, p, _ := v.ProjectOf(r.t.Repository)
	docRepo := v.Document.ChatOf(p).DocRepo
	if docRepo == "" {
		return []string{path}
	}
	repo, err := factoryd.SplitRepo(docRepo)
	switch {
	case err != nil:
		r.unreachable(fmt.Sprintf("its chat.doc_repo %q is no owner/name", docRepo))
		return []string{path}
	case r.c.cfg.DocsToken == "":
		r.unreachable("no docs token is set (FORGEJO_DOCS_TOKEN)")
		return []string{path}
	}
	r.docs = repo
	if r.tree == nil {
		if r.tree, err = r.c.docTree(r.ctx, repo); err != nil {
			r.stop(r.failure("the documents", err))
			return []string{path}
		}
	}
	return doclinks.Resolve(path, r.tree)
}

// Heading reads file of the document repository Files resolved it in.
func (r *cardRefs) Heading(file, id string) bool {
	if r.docs == (factoryd.Repo{}) {
		return true
	}
	content, _, present, err := r.c.docFile(r.ctx, r.docs, file)
	if err != nil {
		r.stop(r.failure("the document "+file, err))
		return true
	}
	return present && slices.Contains(doclinks.HeadingIDs(content), id)
}

func (r *cardRefs) Err() error { return r.err }

// unreachable is the configuration fault that keeps the console from the
// project's documents: no retry heals it.
func (r *cardRefs) unreachable(why string) {
	r.stop(refuse(http.StatusServiceUnavailable, "The brief's links cannot be checked: the console has no access to the project's document repository, as "+
		why+". A human who runs the factory must set it."))
}

func (r *cardRefs) failure(what string, err error) error {
	return refuse(http.StatusBadGateway, fmt.Sprintf("The brief's references could not be checked: %s could not be read, %s. Try again in a moment.", what, errText(err)))
}

// stop keeps err when it is the first fault that keeps r from checking.
func (r *cardRefs) stop(err error) {
	if r.err == nil {
		r.err = err
	}
}
