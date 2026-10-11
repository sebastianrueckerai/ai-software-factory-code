package console

import "net/http"

// maxSummaryLen bounds a human's card summary: what its face's title line
// can hold, one clause of a heading.
const maxSummaryLen = 255

// setSummary is POST /tasks/{id}/summary (doSetSummary).
func (c *Console) setSummary(w http.ResponseWriter, r *http.Request) {
	if _, err := c.doSetSummary(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("summary")); err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, "The card's summary is set.", nil)
}

// setAccent is POST /tasks/{id}/accent (doSetAccent). The accent shows as
// a chip and the card's left edge; it never changes a state rendering and
// never enters an agent's brief.
func (c *Console) setAccent(w http.ResponseWriter, r *http.Request) {
	if _, err := c.doSetAccent(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("value")); err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, "The card's accent is set.", nil)
}
