package console

import (
	"errors"
	"net/http"
	"testing"
)

// errsentence_catchall_arms_form: errSentence's two catch-all arms —
// any error no specific arm maps, and a Forgejo refusal whose cause is
// no API answer — put the chat's refusal in Task 8's form: what failed,
// the error, the next action, and no factoryd log pointer.
func TestProofErrSentenceCatchAllArmsInForm(t *testing.T) {
	t.Run("an unmapped error", func(t *testing.T) {
		r, _ := batchAnswerRig(t)
		_, s := r.loggedIn(t)
		r.c.cfg.Tasks = failingPlans{r.c.cfg.Tasks, errors.New("plan t-p's revision 1 did not answer")}
		var events []string
		trn := r.chatTurnOf(t, s, &events)
		tool, _ := r.c.chatTool("plan")
		got, _ := trn.runTool(tool, `{"task_id":"t-p"}`, toolInput{TaskID: "t-p"})
		if want := "Refused: The action failed: plan t-p's revision 1 did not answer. " +
			"Try the call again, or read the board to see the factory's state."; got != want {
			t.Errorf("the catch-all arm = %q, want %q", got, want)
		}
	})
	t.Run("a forgejo refusal without an api answer", func(t *testing.T) {
		r, _, _ := newChatRig(t)
		_, s := r.loggedIn(t)
		r.c.cfg.HTTP = &http.Client{Transport: failPath{http.DefaultTransport,
			"pulls/9", "the diff of pull request 9 did not answer"}}
		var events []string
		trn := r.chatTurnOf(t, s, &events)
		tool, _ := r.c.chatTool("diff")
		got, _ := trn.runTool(tool, `{"task_id":"t-r"}`, toolInput{TaskID: "t-r"})
		if want := "Refused: The diff could not be read: the diff of pull request 9 did not answer. " +
			"Try the diff tool again, or open the pull request on Forgejo."; got != want {
			t.Errorf("the forgejo arm = %q, want %q", got, want)
		}
	})
}
