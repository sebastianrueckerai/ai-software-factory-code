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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
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
var workerMessages = &http.Client{Timeout: 2 * harness.MessageTimeout, CheckRedirect: noRedirect, Transport: podTransport()}

// workerReads has no timeout of its own: a check-in tool call or an image
// read runs as long as its own bound (checkinToolTimeout, fileReadTimeout)
// says, and the context holds it.
var workerReads = &http.Client{CheckRedirect: noRedirect, Transport: podTransport()}

// podTransport bounds the response headers a sandbox's pod may send: the
// default allows 10 MiB.
func podTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxResponseHeaderBytes = 64 << 10
	return t
}

// noRedirect keeps factoryd from following a sandbox's redirect to a URL
// of the pod's choosing.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// messageWorker posts p's step and facts to the running worker's POST
// /message, which builds the prompt again and hands it to the harness.
// The worker's 501 is harness.ErrNotSupported, its 409 (the session has
// ended) harness.ErrNotRunning.
func (r *Runner) messageWorker(ctx context.Context, ns, pod, token string, p prompts.Prompt) error {
	return r.postWorker(ctx, ns, pod, token, "/message", map[string]string{"step": p.Step(), "text": p.Facts()})
}

// answerPush answers the worker asking for its push token, which it does
// once every agent process in its pod is dead: write on the bot's fork,
// minted fresh per ask under the ask's own name task-<id>-push-own-<n> and
// posted to the worker's POST /push with the name. The fresh name per ask
// is what keeps a retry alive when the refused push's token was revoked by
// a concurrent cleanup (issue 1836): Forgejo takes one token of a name, so
// a re-mint of the revoked name would fail and a re-post of the revoked
// secret would die at the next push. The mint clears its name's leftovers
// first — a session factoryd lost must not block its successor's token —
// and the ask's ordinal keys minted, so the window's paced re-ask lines
// re-post the same token instead of minting again. The worker's 409 (no
// push waits) is harness.ErrNotRunning.
func (r *Runner) answerPush(ctx context.Context, t Task, s sessionSpec, ns, pod string, minted map[int]string, ask harness.PushRequest) error {
	if s.report != "" {
		return errors.New("a report session pushes nothing")
	}
	if ask.Own < 1 {
		ask.Own = 1
	}
	name := PushTokenName(t.ID, "own-"+strconv.Itoa(ask.Own))
	secret, answered := minted[ask.Own]
	if !answered {
		if err := r.forgejo.RevokeTokenNamed(ctx, name); err != nil {
			return fmt.Errorf("clear the push token's name: %w", err)
		}
		tok, err := r.forgejo.MintToken(ctx, name, "write:repository", []Repo{r.forgejo.Fork(s.repo)})
		if err != nil {
			return fmt.Errorf("mint the push token: %w", err)
		}
		minted[ask.Own], secret = tok.Secret, tok.Secret
	}
	return r.postWorker(ctx, ns, pod, s.messageToken, "/push", map[string]string{"token": secret, "name": name})
}

// postWorker posts body as JSON to path on the running worker's endpoint
// (harness.MessagePort) with the session's bearer. A pod that has ended is
// harness.ErrNotRunning, and gets no post.
func (r *Runner) postWorker(ctx context.Context, ns, pod, bearer, path string, body any) error {
	ip, err := r.kube.PodIP(ctx, ns, pod)
	if errors.Is(err, errPodEnded) {
		return fmt.Errorf("%w: %w", harness.ErrNotRunning, err)
	}
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
	return &workerAnswer{code: resp.StatusCode, text: textutil.Truncate(string(bytes.TrimSpace(answer)), 300)}
}

// workerAnswer is the worker's answer to a post it did not take.
type workerAnswer struct {
	code int
	text string
}

func (a *workerAnswer) Error() string { return fmt.Sprintf("worker answered %d: %s", a.code, a.text) }

// postFinal reports whether a post's err holds for every retry: the
// worker refused it (a 4xx answer), or no session runs (its 409, or a pod
// that has ended).
func postFinal(err error) bool {
	var a *workerAnswer
	return errors.Is(err, harness.ErrNotRunning) || errors.As(err, &a) && a.code >= 400 && a.code < 500
}

// SetTestEndpoints is a test seam: the acceptance tests point the runner's
// worker endpoint port and the git base at their own stubs, which
// NewRunner's absolute-URL check on cfg.GitBaseURL would otherwise refuse.
// Production never calls it.
func (r *Runner) SetTestEndpoints(workerPort int, gitBase string) {
	r.workerPort = workerPort
	r.cfg.GitBaseURL = gitBase
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
	p, err := prompts.Build("nudge", n.Text())
	if err == nil {
		err = r.messageWorker(ctx, ns, pod, token, p)
	}
	var loop *Trip
	switch {
	case err == nil:
		loop = sup.NudgeSent(n)
		log.Printf("task %s: nudged session %s after %d identical %s calls", t.ID, s.id, n.Count, n.Tool)
	case errors.Is(err, harness.ErrNotSupported):
		detail["fallback"] = "redirect"
	default:
		detail["error"] = r.Scrub.String(err.Error())
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
// whose session is in its start check-in answers a SessionStartingError,
// one with no session running in this factoryd ErrNoLiveSession, the
// worker's answers are messageWorker's.
func (r *Runner) MessageSession(ctx context.Context, taskID, user, text string) error {
	r.liveMu.Lock()
	ls, ok := r.live[taskID]
	since, starting := r.starting[taskID]
	r.liveMu.Unlock()
	switch {
	case !ok && starting:
		return &SessionStartingError{Since: since}
	case !ok:
		return ErrNoLiveSession
	}
	w := ls.worker
	p, err := prompts.Build("message-responder", sessionMessage(user, text))
	if err == nil {
		err = r.messageWorker(ctx, w.ns, w.pod, w.token, p)
	}
	detail := map[string]any{"session": w.session, "from": user}
	if err != nil {
		detail["error"] = r.Scrub.String(err.Error())
	}
	r.record(ctx, taskID, "message", detail)
	return err
}

// sessionMessage is the one composer of what a writer's message looks
// like inside the session: the wrapper that names who speaks. The guide
// and the chat form ride the session's first message (Harness.Message).
func sessionMessage(user, text string) string {
	from := user + ", a writer of this project"
	if user == ChatUser {
		from = "the project agent"
	}
	return "A message from " + from + ": " + text
}

// message sends the supervisor's guidance into the running session and
// records it on the timeline.
func (r *Runner) message(ctx context.Context, t Task, s sessionSpec, ns, pod, text string) error {
	p, err := prompts.Build("supervisor-guidance", text)
	if err == nil {
		err = r.messageWorker(ctx, ns, pod, s.messageToken, p)
	}
	detail := map[string]any{"session": s.id}
	if err != nil {
		detail["error"] = r.Scrub.String(err.Error())
	}
	r.record(ctx, t.ID, "message", detail)
	return err
}
