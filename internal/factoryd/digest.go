package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Digester is one Small call over text under ask on a task's account,
// purpose digest: OperatorModel.Digest, the chat's digest.
type Digester interface {
	Digest(ctx context.Context, taskID, thread, ask, text string) (string, error)
}

// refusedSentence is a session tool's words for its call the gateway
// refused: what was refused, the decision and its reason, and for a
// provider limit the reset its class is frozen until, as the freeze card's
// face prints it, what the session does, and the card's answer that ends
// the freeze sooner.
func refusedSentence(what string, refused *policy.Refusal) string {
	s := fmt.Sprintf("%s was refused (%s): %s", what, refused.Decision, refused.Reason)
	if !refused.Until.IsZero() {
		s = strings.TrimSuffix(s, ".") + ". The class is frozen until " + stamp(refused.Until) + ": go on without it, as the card resumes by itself at the reset. " +
			fmt.Sprintf("An approver ends the freeze sooner with the answer %q on the class's freeze card, or with a retry of a frozen card in the chat.",
				choices(ActRetry + "/" + variantThaw)[0].Label)
	}
	return s
}

// fileDigestAsk is a session's digest's ask when the session gives none.
const fileDigestAsk = "Summarise this file."

// DigestHandler is POST /internal/digest, the gateway's hand-over of a
// session's digest: {task_id, path, ask} answers {"text"}, the path and
// what the Small model answered over the file's text, read from the
// task's running session's workspace (POST /file). The file's bytes go to
// the model, never back to the session. Every refusal is one sentence, and
// one inside a provider freeze adds what ends the freeze.
func (r *Runner) DigestHandler(d Digester) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			TaskID string `json:"task_id"`
			Path   string `json:"path"`
			Ask    string `json:"ask"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.TaskID == "" || in.Path == "" {
			http.Error(w, "task_id and path are required", http.StatusBadRequest)
			return
		}
		ctx := req.Context()
		t, ok, err := r.store.Task(ctx, in.TaskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no task "+in.TaskID, http.StatusNotFound)
			return
		}
		if Final(t.Status) {
			http.Error(w, "task "+in.TaskID+" is "+t.Status, http.StatusConflict)
			return
		}
		rctx, cancel := context.WithTimeout(ctx, fileReadTimeout)
		data, err := r.workspaceFile(rctx, t, "/file", in.Path, policy.MaxDigestBytes,
			fmt.Sprintf("%s is larger than %d KiB, more than one digest reads: read the part you need with the read tool's offset and limit", in.Path, policy.MaxDigestBytes>>10))
		cancel()
		var no *lookRefusal
		if errors.As(err, &no) {
			http.Error(w, no.sentence, no.status)
			return
		}
		if err != nil {
			log.Printf("digest of task %s at %s: %v", t.ID, in.Path, err)
			http.Error(w, fmt.Sprintf("the session's sandbox did not answer the read of %s: try again shortly", in.Path), http.StatusBadGateway)
			return
		}
		text, err := d.Digest(ctx, t.ID, "", cmp.Or(in.Ask, fileDigestAsk), string(data))
		var refused *policy.Refusal
		switch {
		case errors.As(err, &refused):
			http.Error(w, refusedSentence("the digest of "+in.Path, refused), http.StatusConflict)
			return
		case err != nil:
			log.Printf("digest of task %s at %s: %v", t.ID, in.Path, err)
			http.Error(w, fmt.Sprintf("the small model did not answer on %s: ask again shortly", in.Path), http.StatusBadGateway)
			return
		case strings.TrimSpace(text) == "":
			http.Error(w, fmt.Sprintf("the small model answered nothing on %s: ask again, or read the part you need", in.Path), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": in.Path + ": " + text}) // #nosec G104 -- best-effort response write
	}
}
