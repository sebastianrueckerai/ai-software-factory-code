package console

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// attachAnalysis is the attach_analysis tool: what the agent found about
// a card's ask, recorded inside the ask itself, where the member reads it
// with the question (the main agent concept). An ask already answered is
// done; a card with no ask needs escalate, not an analysis.
func (t *turn) attachAnalysis(in toolInput) (string, error) {
	found, recommendation := strings.TrimSpace(in.Found), strings.TrimSpace(in.Recommendation)
	switch {
	case t.c.cfg.AttachAnalysis == nil:
		return "", refuse(http.StatusConflict, "this factory records no analysis in its asks")
	case found == "" || len(found) > 2000:
		return "", refuse(http.StatusBadRequest, "found: what you learned about the ask, 1 to 2000 characters")
	case len(recommendation) > 2000:
		return "", refuse(http.StatusBadRequest, "recommendation: 0 to 2000 characters, empty to offer none")
	}
	k, err := t.card(in)
	if err != nil {
		return "", err
	}
	err = t.c.cfg.AttachAnalysis(t.r.Context(), t.p.Repository, k.ID, found, recommendation, t.a.s.user)
	var settled *factoryd.AskSettledError
	switch {
	case errors.As(err, &settled):
		line := fmt.Sprintf("The ask of #%d is %s", k.IssueNumber, settled.Word)
		if settled.By != "" {
			line += " by " + settled.By
		}
		return "", refuse(http.StatusConflict, line+". An ask already answered is done.")
	case errors.Is(err, factoryd.ErrNoAsk):
		return "", refuse(http.StatusNotFound, fmt.Sprintf("Card #%d asks nothing in the chat; escalate files an ask when you need a person.", k.IssueNumber))
	case err != nil:
		log.Printf("console: the analysis of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return "", refuse(http.StatusBadGateway, "The analysis could not be recorded; see the factoryd log.")
	}
	return fmt.Sprintf("Recorded the analysis inside the ask of #%d; the members read it with the question.", k.IssueNumber), nil
}
