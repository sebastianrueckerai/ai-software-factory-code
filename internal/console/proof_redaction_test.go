package console

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// error_redacted_before_message: an error carrying a token-shaped
// secret reaches the refusal message masked, so no credential reaches a
// human or the chat.
func TestProofRefusalMasksTheTokenBeforeTheMessage(t *testing.T) {
	const token = "ghp_AbCdEf0123456789AbCdEf0123456789AbCdEf01"
	r, _ := batchAnswerRig(t)
	_, s := r.loggedIn(t)
	r.c.cfg.Escalate = func(context.Context, string, factoryd.HumanQuestion, string) (int64, error) {
		return 0, errors.New("forgejo rejected the card: the bearer token " + token + " expired")
	}
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	tool, _ := r.c.chatTool("escalate")
	args := `{"ask":"Stop the roll?","why":"the ledger is short","info":"the pool is dry","recommendation":"stop it","choices":["stop it","go on"]}`
	in := toolInput{Ask: "Stop the roll?", Why: "the ledger is short", Info: "the pool is dry",
		Recommendation: "stop it", Choices: []string{"stop it", "go on"}}
	got, _ := trn.runTool(tool, args, in)
	if strings.Contains(got, token) {
		t.Errorf("the refusal leaks the token: %q", got)
	}
	if !strings.Contains(got, "[token]") {
		t.Errorf("the refusal does not show the token mask: %q", got)
	}
	if want := "Refused: The escalation could not be filed: forgejo rejected the card: the bearer token [token] expired. " +
		"Try again, or post the question on the card's issue."; got != want {
		t.Errorf("the tool result = %q, want %q", got, want)
	}
}
