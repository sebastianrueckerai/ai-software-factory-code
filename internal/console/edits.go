package console

import "net/http"

// maxSummaryLen bounds a human's card summary: what its face's title line
// can hold, one clause of a heading.
const maxSummaryLen = 255

// setSummary is POST /tasks/{id}/summary (doSetSummary).
func (c *Console) setSummary(w http.ResponseWriter, r *http.Request) {
	_, err := c.doSetSummary(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("summary"))
	c.pressed(w, r, err)
}

// setAccent is POST /tasks/{id}/accent (doSetAccent). The accent shows as
// a chip and the card's left edge; it never changes a state rendering and
// never enters an agent's brief.
func (c *Console) setAccent(w http.ResponseWriter, r *http.Request) {
	_, err := c.doSetAccent(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("value"))
	c.pressed(w, r, err)
}
