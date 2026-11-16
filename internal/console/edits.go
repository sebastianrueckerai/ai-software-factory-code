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
// repository with the heading the renderer gives that id. err keeps the
// first read that failed, so the door refuses with it and never with a
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
	if docRepo == "" || r.c.cfg.DocsToken == "" {
		return false
	}
	repo, err := factoryd.SplitRepo(docRepo)
	if err != nil {
		return false
	}
	if r.tree == nil {
		if r.tree, err = r.c.docTree(r.ctx, repo); err != nil {
			r.fail("the documents", err)
			return false
		}
	}
	files := doclinks.Resolve(path, r.tree)
	if len(files) != 1 {
		return false
	}
	if id == "" {
		return true
	}
	content, _, present, err := r.c.docFile(r.ctx, repo, files[0])
	if err != nil {
		r.fail("the document "+files[0], err)
		return false
	}
	return present && slices.Contains(docHeadingIDs(content), id)
}

func (r *cardRefs) fail(what string, err error) {
	if r.err == nil {
		r.err = refuse(http.StatusBadGateway, fmt.Sprintf("The brief's references could not be checked: %s could not be read, %s. Try again in a moment.", what, errText(err)))
	}
}
