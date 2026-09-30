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

// messageWorker posts text to the running worker's message endpoint
// (harness.MessagePort), which hands it to the harness. The worker's 501
// is harness.ErrNotSupported, its 409 (the session has ended)
// harness.ErrNotRunning.
func (r *Runner) messageWorker(ctx context.Context, ns, pod, token, text string) error {
	ip, err := r.kube.PodIP(ctx, ns, pod)
	if err != nil {
		return fmt.Errorf("pod address: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(ip, strconv.Itoa(r.messagePort()))+"/message", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
// supervisor; both come back as the trip. A session that ended meanwhile
// needs none.
func (r *Runner) nudge(ctx context.Context, t Task, s sessionSpec, sup *Supervisor, ns, pod, token string, n Nudge) *Trip {
	detail := map[string]any{"session": s.id, "tool": n.Tool, "count": n.Count}
	err := r.messageWorker(ctx, ns, pod, token, n.Text())
	switch {
	case err == nil:
		sup.NudgeSent(n)
		log.Printf("task %s: nudged session %s after %d identical %s calls", t.ID, s.id, n.Count, n.Tool)
	case errors.Is(err, harness.ErrNotSupported):
		detail["fallback"] = "redirect"
	default:
		detail["error"] = err.Error()
	}
	r.record(ctx, t.ID, "nudge", detail)
	if err == nil || errors.Is(err, harness.ErrNotRunning) {
		return nil
	}
	return sup.nudgeUndelivered(n, err)
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
