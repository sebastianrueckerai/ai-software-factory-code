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
// with the ask (the main agent concept). An ask already settled is done;
// a card with no ask needs escalate, not an analysis.
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
	if t.c.cfg.HandUpRung2 != nil {
		rec := factoryd.RungTwoRecommendation{Grounds: recommendation}
		if in.Recommended != nil {
			rec.Action, rec.Args = strings.TrimSpace(in.Recommended.Action), in.Recommended.Args
		}
		handled, err := t.c.cfg.HandUpRung2(t.r.Context(), k.ID, in.CaseID, found, rec, t.readList())
		var refused factoryd.ErrActRefused
		switch {
		case errors.As(err, &refused):
			return "", refuse(http.StatusConflict, refused.Error()+".")
		case err != nil:
			log.Printf("console: the hand-up of %s on %s: %v", t.a.s.user, t.p.Repository, err)
			return "", refuse(http.StatusBadGateway, fmt.Sprintf("The hand-up could not be recorded: %s. Attach the analysis again.", errText(err)))
		case handled != 0:
			t.noteEnded(handled)
			return fmt.Sprintf("Handed #%d to the human with your analysis and recommendation; the card waits at its face.", k.IssueNumber), nil
		}
	}
	err = t.c.cfg.AttachAnalysis(t.r.Context(), k.ID, found, recommendation,
		factoryd.Event{Repository: t.p.Repository, Actor: t.a.s.user, Via: factoryd.ViaChat})
	var settled *factoryd.AskSettledError
	switch {
	case errors.As(err, &settled):
		line := fmt.Sprintf("The %s of #%d is %s", askNoun(settled.Kind), k.IssueNumber, settled.Word)
		if settled.By != "" {
			line += " by " + settled.By
		}
		return "", refuse(http.StatusConflict, line+". A settled ask is done; escalate files a new one when you need a person.")
	case errors.Is(err, factoryd.ErrNoAsk):
		return "", refuse(http.StatusNotFound, fmt.Sprintf("Card #%d asks nothing in the chat; escalate files an ask when you need a person.", k.IssueNumber))
	case err != nil:
		log.Printf("console: the analysis of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return "", refuse(http.StatusBadGateway, fmt.Sprintf("The analysis could not be recorded: %s. Attach the analysis again.", errText(err)))
	}
	return fmt.Sprintf("Recorded the analysis inside the ask of #%d; the humans read it with the ask.", k.IssueNumber), nil
}

// askNoun is an ask kind's noun for a refusal: the approval kinds' own
// words with request, a question's own word.
func askNoun(kind string) string {
	switch kind {
	case factoryd.ApprovalMerge, factoryd.ApprovalPlan, factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		return strings.ReplaceAll(kind, "_", " ") + " request"
	}
	return "question"
}
