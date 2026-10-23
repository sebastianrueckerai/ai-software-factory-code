package factoryd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/manual"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// messageTokenHash is what the worker pod gets of the session's message
// bearer: factoryd alone holds the bearer.
func messageTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// workerMessages waits briefly (a nudge holds up the session's event loop)
// but past the worker's own bound on the hand-over.
var workerMessages = &http.Client{Timeout: 2 * harness.MessageTimeout}

// workerReads has no timeout of its own: a check-in tool call or an image
// read runs as long as its own bound (checkinToolTimeout, imageReadTimeout)
// says, and the context holds it.
var workerReads = &http.Client{}

// messageWorker posts text to the running worker's POST /message, which
// hands it to the harness. The worker's 501 is harness.ErrNotSupported,
// its 409 (the session has ended) harness.ErrNotRunning.
func (r *Runner) messageWorker(ctx context.Context, ns, pod, token, text string) error {
	return r.postWorker(ctx, ns, pod, token, "/message", map[string]string{"text": text})
}

// recordsWorker delivers the parent session's record into the pod, where
// it lands at /workspace/.factory/sessions/<session>.jsonl for the
// check-in's tools and the coding agent to read.
func (r *Runner) recordsWorker(ctx context.Context, ns, pod, token, session, record string) error {
	return r.postWorker(ctx, ns, pod, token, "/records", map[string]string{"session": session, "record": record})
}

// briefWorker hands the check-in's handover to the pod, which holds its
// conversation's start for it.
func (r *Runner) briefWorker(ctx context.Context, ns, pod, token, handover string) error {
	return r.postWorker(ctx, ns, pod, token, "/brief", map[string]string{"handover": handover})
}

// answerPush answers the worker asking for its push token, which it does
// once every agent process in its pod is dead: write on the bot's fork,
// minted at the first ask into minted (Forgejo takes one token of a name;
// the session's end revokes it) and posted to the worker's POST /push.
// The worker's 409 (no push waits) is harness.ErrNotRunning.
func (r *Runner) answerPush(ctx context.Context, t Task, s sessionSpec, ns, pod string, minted *string) error {
	if s.report != "" {
		return errors.New("a report session pushes nothing")
	}
	if *minted == "" {
		tok, err := r.forgejo.MintToken(ctx, TokenName(t.ID), "write:repository", []Repo{r.forgejo.Fork(s.repo)})
		if err != nil {
			return fmt.Errorf("mint the push token: %w", err)
		}
		*minted = tok.Secret
	}
	return r.postWorker(ctx, ns, pod, s.messageToken, "/push", map[string]string{"token": *minted})
}

// postWorker posts body as JSON to path on the running worker's endpoint
// (harness.MessagePort) with the session's bearer.
func (r *Runner) postWorker(ctx context.Context, ns, pod, bearer, path string, body any) error {
	ip, err := r.kube.PodIP(ctx, ns, pod)
	if err != nil {
		return fmt.Errorf("pod address: %w", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(ip, strconv.Itoa(r.messagePort()))+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := workerMessages.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotImplemented:
		return harness.ErrNotSupported
	case http.StatusConflict:
		return harness.ErrNotRunning
	}
	return fmt.Errorf("worker answered %d: %s", resp.StatusCode, textutil.Truncate(string(bytes.TrimSpace(answer)), 300))
}

func (r *Runner) messagePort() int {
	if r.workerPort != 0 {
		return r.workerPort
	}
	return harness.MessagePort
}

// nudge sends n into the running session and records it on the timeline.
// A harness that takes no message gets today's redirect with the same
// text (a fresh session), and a nudge that cannot be delivered trips the
// supervisor; both come back as the trip, as does the loop a delivered
// nudge closes. A session that ended meanwhile needs none.
func (r *Runner) nudge(ctx context.Context, t Task, s sessionSpec, sup *Supervisor, ns, pod, token string, n Nudge) *Trip {
	detail := map[string]any{"session": s.id, "tool": n.Tool, "count": n.Count}
	err := r.messageWorker(ctx, ns, pod, token, n.Text())
	var loop *Trip
	switch {
	case err == nil:
		loop = sup.NudgeSent(n)
		log.Printf("task %s: nudged session %s after %d identical %s calls", t.ID, s.id, n.Count, n.Tool)
	case errors.Is(err, harness.ErrNotSupported):
		detail["fallback"] = "redirect"
	default:
		detail["error"] = err.Error()
	}
	r.record(ctx, t.ID, "nudge", detail)
	if err == nil || errors.Is(err, harness.ErrNotRunning) {
		return loop
	}
	return sup.nudgeUndelivered(n, err)
}

// MessageSession hands text from user, a writer of the task's project, to
// the task's running session the way the supervisor's message goes (6.6):
// the worker's POST /message, recorded as a message step from user. A task
// with no session running in this factoryd answers ErrNoLiveSession, the
// worker's answers are messageWorker's.
func (r *Runner) MessageSession(ctx context.Context, taskID, user, text string) error {
	r.liveMu.Lock()
	ls, ok := r.live[taskID]
	basics := ok && !ls.basics
	if basics {
		ls.basics = true
	}
	r.liveMu.Unlock()
	if !ok {
		return ErrNoLiveSession
	}
	w := ls.worker
	msg := "A message from " + user + ", a writer of this project: " + text
	if basics {
		msg = "You are the agent session of card " + taskID + " in the AI software factory. " + manual.Loop +
			". When you end, the factory commits your working tree, pushes it and opens the pull request. " +
			"A writer of this project talks to you through the card's chat; the gates judge what you push. " +
			"The manual tool reads the rest.\n\n" + msg
	}
	err := r.messageWorker(ctx, w.ns, w.pod, w.token, msg)
	detail := map[string]any{"session": w.session, "from": user}
	if err != nil {
		detail["error"] = err.Error()
	}
	r.record(ctx, taskID, "message", detail)
	return err
}

// message sends the supervisor's guidance into the running session and
// records it on the timeline.
func (r *Runner) message(ctx context.Context, t Task, s sessionSpec, ns, pod, text string) error {
	err := r.messageWorker(ctx, ns, pod, s.messageToken, text)
	detail := map[string]any{"session": s.id}
	if err != nil {
		detail["error"] = err.Error()
	}
	r.record(ctx, t.ID, "message", detail)
	return err
}
