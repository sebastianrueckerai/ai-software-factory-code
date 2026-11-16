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

// editBrief is POST /tasks/{id}/brief, the drawer's Edit the brief
// (doEdit): the title, the why line and the summary.
func (c *Console) editBrief(w http.ResponseWriter, r *http.Request) {
	_, err := c.doEdit(r.Context(), button(r), r.PathValue("id"), factoryd.BriefText{
		Title: r.PostFormValue("title"), Why: r.PostFormValue("why"), Summary: r.PostFormValue("summary")})
	c.pressed(w, r, err)
}

// setAccent is POST /tasks/{id}/accent (doSetAccent). The accent shows as
// a chip and the card's left edge; it never changes a state rendering and
// never enters an agent's brief.
func (c *Console) setAccent(w http.ResponseWriter, r *http.Request) {
	_, err := c.doSetAccent(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("value"))
	c.pressed(w, r, err)
}

// cardRefs resolves the references of card t's brief for a: #N is the
// project's card of issue N, else any issue or pull request of the
// repository, and a document link a file of the project's document
// repository with the heading the renderer gives that id. A project with
// no document repository has no documents to hold a link to. err keeps
// the first read that failed, or the configuration that cannot read the
// documents, so the door answers with it and never with a refusal of a
// reference it could not check.
type cardRefs struct {
	ctx  context.Context
	c    *Console
	a    actor
	t    factoryd.Task
	err  error
	tree []string
}

func (r *cardRefs) Issue(n int) (string, string, bool) {
	if k, ok, err := r.c.cfg.Tasks.TaskByIssue(r.ctx, r.t.Repository, int64(n)); err != nil {
		r.fail("the cards", err)
		return "", "", false
	} else if ok {
		return factoryd.CardTitle(k), k.Status, true
	}
	repo, err := factoryd.SplitRepo(r.t.Repository)
	if err != nil {
		r.fail("the repository", err)
		return "", "", false
	}
	var issue struct{ Title, State string }
	err = r.c.as(r.ctx, r.a.s, http.MethodGet, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), n), nil, http.StatusOK, &issue)
	var api *apiError
	switch {
	case errors.As(err, &api) && api.status == http.StatusNotFound:
		return "", "", false
	case err != nil:
		r.fail("issue #"+fmt.Sprint(n), err)
		return "", "", false
	}
	return issue.Title, issue.State, true
}

func (r *cardRefs) Heading(path, id string) bool {
	v, ok := r.c.inForce(r.ctx)
	if !ok {
		r.fail("the policy", errors.New("not loaded"))
		return false
	}
	_, p, _ := v.ProjectOf(r.t.Repository)
	docRepo := v.Document.ChatOf(p).DocRepo
	if docRepo == "" {
		return true
	}
	repo, err := factoryd.SplitRepo(docRepo)
	switch {
	case err != nil:
		r.fail("the documents", fmt.Errorf("the document repository %q is no owner/name", docRepo))
		return false
	case r.c.cfg.DocsToken == "":
		r.fail("the documents", errors.New("no docs token is seated (FORGEJO_DOCS_TOKEN)"))
		return false
	}
	if r.tree == nil {
		if r.tree, err = r.c.docTree(r.ctx, repo); err != nil {
			r.fail("the documents", err)
			return false
		}
	}
	files := doclinks.Resolve(path, r.tree)
	if id == "" {
		return len(files) > 0
	}
	for _, f := range files {
		content, _, present, err := r.c.docFile(r.ctx, repo, f)
		if err != nil {
			r.fail("the document "+f, err)
			return false
		}
		if present && slices.Contains(docHeadingIDs(content), id) {
			return true
		}
	}
	return false
}

func (r *cardRefs) fail(what string, err error) {
	if r.err == nil {
		r.err = refuse(http.StatusBadGateway, fmt.Sprintf("The brief's references could not be checked: %s could not be read, %s. Try again in a moment.", what, errText(err)))
	}
}
