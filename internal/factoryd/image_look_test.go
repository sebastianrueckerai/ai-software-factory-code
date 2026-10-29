package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	uuidA = "aaaaaaaa-1111-4111-8111-111111111111"
	uuidB = "bbbbbbbb-2222-4222-8222-222222222222"
	uuidC = "cccccccc-3333-4333-8333-333333333333"
	uuidD = "dddddddd-4444-4444-8444-444444444444"
)

// pngBytes is a PNG header and some pixel bytes, which the sniffer reads
// as image/png.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 64)...)

type fakeVision struct {
	mu    sync.Mutex
	calls []string
	image []byte
	err   error
}

func (f *fakeVision) Vision(_ context.Context, taskID, alias, thread, ask string, image []byte, contentType string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"|"+alias+"|"+thread+"|"+ask+"|"+contentType)
	f.image = image
	return "a login form, the button grey", f.err
}

type lookEnv struct {
	runner   *Runner
	store    *fakeStore
	vision   *fakeVision
	mu       sync.Mutex
	fetched  []string // the attachments Forgejo served, with their auth
	forgejo  int      // the requests Forgejo answered
	workerIn []string // the bodies the pod's worker read, with their bearer
	serve    map[string][]byte
}

// newLookEnv is a runner whose coder class names vision_model vision,
// task t-1 on card sf/app#12 running a session in pod factory-agents-x/p1,
// and child t-2 (card #13, no images of its own) of parent t-1. Card #12
// carries img1.png on its issue and two image.png and a notes.txt on a
// comment; the workspace holds shots/home.png and a text file.
func newLookEnv(t *testing.T) *lookEnv {
	t.Helper()
	e := &lookEnv{store: newFakeStore(), vision: &fakeVision{}, serve: map[string][]byte{
		uuidA: pngBytes, uuidB: pngBytes, uuidC: append([]byte("GIF89a"), pngBytes...), uuidD: []byte("just words"),
	}}
	fj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.forgejo++
		e.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/repos/sf/app/issues/12":
			io.WriteString(w, `{"number":12,"assets":[{"name":"img1.png","size":72,"uuid":"`+uuidA+`"}]}`)
		case "/api/v1/repos/sf/app/issues/12/comments":
			io.WriteString(w, `[{"id":5,"body":"see","assets":[{"name":"image.png","size":72,"uuid":"`+uuidB+`"},{"name":"image.png","size":78,"uuid":"`+uuidC+`"},{"name":"notes.txt","size":3,"uuid":"`+uuidD+`"}]}]`)
		case "/api/v1/repos/sf/app/issues/13":
			io.WriteString(w, `{"number":13,"assets":[]}`)
		case "/api/v1/repos/sf/app/issues/13/comments":
			io.WriteString(w, `[]`)
		case "/api/v1/repos/sf/app/issues/14":
			io.WriteString(w, `{"number":14,"assets":[]}`)
		case "/api/v1/repos/sf/app/issues/14/comments":
			io.WriteString(w, `[{"id":9,"body":"`+strings.Repeat("x", maxForgejoAnswer)+`"}]`)
		case "/api/v1/repos/sf/app/issues/15":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			uuid, ok := strings.CutPrefix(r.URL.Path, "/attachments/")
			if !ok {
				http.NotFound(w, r)
				return
			}
			user, pw, _ := r.BasicAuth()
			e.mu.Lock()
			e.fetched = append(e.fetched, uuid+" "+user+":"+pw)
			e.mu.Unlock()
			w.Write(e.serve[uuid])
		}
	}))
	t.Cleanup(fj.Close)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.workerIn = append(e.workerIn, r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(b))
		e.mu.Unlock()
		var in struct{ Path string }
		json.Unmarshal(b, &in)
		switch in.Path {
		case "shots/home.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
		case "/etc/hostname":
			http.Error(w, "/etc/hostname is outside the workspace", http.StatusForbidden)
		case "old.png":
			http.NotFound(w, r)
		default:
			http.Error(w, "no file "+in.Path+" in the workspace", http.StatusNotFound)
		}
	}))
	t.Cleanup(worker.Close)
	_, port, _ := net.SplitHostPort(worker.Listener.Addr().String())
	kube := newFakeKube()
	kube.pods["factory-agents-x/p1"] = &fakePod{ns: "factory-agents-x", name: "p1", phase: "Running"}
	ctx := context.Background()
	for _, task := range []Task{
		{ID: "t-1", Repository: "sf/app", IssueNumber: 12, Status: "running", RequiredCapabilities: []string{"coder"}, Track: "direct"},
		{ID: "t-2", Repository: "sf/app", IssueNumber: 13, Status: "running", RequiredCapabilities: []string{"coder"}, Parent: "t-1", Track: "direct"},
		{ID: "t-3", Repository: "sf/app", IssueNumber: 14, Status: "running", RequiredCapabilities: []string{"coder"}, Track: "direct"},
		{ID: "t-4", Repository: "sf/app", IssueNumber: 15, Status: "running", RequiredCapabilities: []string{"coder"}, Track: "direct"},
	} {
		if err := e.store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.PutLiveSession(ctx, LiveSession{TaskID: "t-1", SessionID: "s1", Namespace: "factory-agents-x", Pod: "p1", MessageToken: "msg-tok"}); err != nil {
		t.Fatal(err)
	}
	cfg := testRunnerConfig()
	pv := testPolicyVersion()
	coder := pv.Document.Classes["coder"]
	coder.VisionModel = "vision"
	pv.Document.Classes["coder"] = coder
	cfg.Policy = func() policy.Version { return pv }
	e.runner = &Runner{cfg: cfg, store: e.store, kube: kube, forgejo: &Forgejo{BaseURL: fj.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	e.runner.workerPort, _ = strconv.Atoi(port)
	return e
}

func (e *lookEnv) look(t *testing.T, body string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	e.runner.ImageLookHandler(e.vision)(rr, httptest.NewRequest(http.MethodPost, "/internal/image-look", strings.NewReader(body)))
	return rr.Code, strings.TrimSpace(rr.Body.String())
}

// A card image named as the card's text names it: Forgejo's attachment,
// read with the bot's login, looked at by the coder's vision alias on the
// card's own task, the ask passed on.
func TestImageLookAtACardImage(t *testing.T) {
	e := newLookEnv(t)
	code, body := e.look(t, `{"task_id":"t-1","card_image":"img1","ask":"is the button blue?"}`)
	if code != http.StatusOK || body != `{"text":"img1.png: a login form, the button grey"}` {
		t.Fatalf("look = %d %s", code, body)
	}
	if len(e.vision.calls) != 1 || e.vision.calls[0] != "t-1|vision||is the button blue?|image/png" || !bytes.Equal(e.vision.image, pngBytes) {
		t.Fatalf("vision calls = %v, image %d bytes", e.vision.calls, len(e.vision.image))
	}
	if len(e.fetched) != 1 || e.fetched[0] != uuidA+" factory-agent:bot-pw" {
		t.Fatalf("fetched = %v", e.fetched)
	}
}

// A comment's image by its link; a name two images share refuses naming
// their links; a name no image has refuses naming the card's images, the
// text file among the attachments left out.
func TestImageLookResolvesCommentImagesAndRefusesAmbiguity(t *testing.T) {
	e := newLookEnv(t)
	code, body := e.look(t, `{"task_id":"t-1","card_image":"![image](/attachments/`+uuidC+`)"}`)
	if code != http.StatusOK || body != `{"text":"image.png: a login form, the button grey"}` || e.vision.calls[0] != "t-1|vision|||image/gif" {
		t.Fatalf("by link = %d %s, vision %v", code, body, e.vision.calls)
	}
	code, body = e.look(t, `{"task_id":"t-1","card_image":"image.png"}`)
	if want := "2 images on card sf/app#12 are named image.png: name one by its link, /attachments/" + uuidB + ", /attachments/" + uuidC; code != http.StatusConflict || body != want {
		t.Fatalf("shared name = %d %q, want 409 %q", code, body, want)
	}
	code, body = e.look(t, `{"task_id":"t-1","card_image":"img9"}`)
	if want := "no image img9 on card sf/app#12: its images are img1.png, image.png (/attachments/" + uuidB + "), image.png (/attachments/" + uuidC + ")"; code != http.StatusNotFound || body != want {
		t.Fatalf("unknown = %d %q, want 404 %q", code, body, want)
	}
	code, body = e.look(t, `{"task_id":"t-1","card_image":"notes.txt"}`)
	if code != http.StatusNotFound || !strings.HasPrefix(body, "no image notes.txt on card sf/app#12") {
		t.Fatalf("a text attachment = %d %q", code, body)
	}
	if len(e.vision.calls) != 1 {
		t.Fatalf("vision calls = %v, want only the one by link", e.vision.calls)
	}
}

// A plan's card finds the screenshot its plan was filed with on the
// parent's issue.
func TestImageLookReadsTheParentCardsImages(t *testing.T) {
	e := newLookEnv(t)
	code, body := e.look(t, `{"task_id":"t-2","card_image":"img1.png"}`)
	if code != http.StatusOK || body != `{"text":"img1.png: a login form, the button grey"}` || e.vision.calls[0] != "t-2|vision|||image/png" {
		t.Fatalf("look = %d %s, vision %v", code, body, e.vision.calls)
	}
}

// A workspace file is read from the running session's pod with the
// session's message bearer; the worker's refusal is passed on with its
// status; a task without a live session refuses.
func TestImageLookAtAWorkspaceFile(t *testing.T) {
	e := newLookEnv(t)
	code, body := e.look(t, `{"task_id":"t-1","path":"shots/home.png","ask":"the header's colour"}`)
	if code != http.StatusOK || body != `{"text":"shots/home.png: a login form, the button grey"}` {
		t.Fatalf("look = %d %s", code, body)
	}
	if e.vision.calls[0] != "t-1|vision||the header's colour|image/png" || !bytes.Equal(e.vision.image, pngBytes) {
		t.Fatalf("vision = %v", e.vision.calls)
	}
	if len(e.workerIn) != 1 || e.workerIn[0] != `/image Bearer msg-tok {"path":"shots/home.png"}` {
		t.Fatalf("worker = %v", e.workerIn)
	}
	if e.forgejo != 0 {
		t.Fatalf("a workspace look asked Forgejo %d times", e.forgejo)
	}
	if code, body := e.look(t, `{"task_id":"t-1","path":"old.png"}`); code != http.StatusConflict ||
		body != "task t-1's session runs a worker without the workspace read: its next session has it" {
		t.Fatalf("an old worker = %d %q", code, body)
	}
	if code, body := e.look(t, `{"task_id":"t-1","path":"shots/none.png"}`); code != http.StatusNotFound || body != "no file shots/none.png in the workspace" {
		t.Fatalf("missing = %d %q", code, body)
	}
	if code, body := e.look(t, `{"task_id":"t-1","path":"/etc/hostname"}`); code != http.StatusForbidden || body != "/etc/hostname is outside the workspace" {
		t.Fatalf("outside = %d %q", code, body)
	}
	if code, body := e.look(t, `{"task_id":"t-2","path":"shots/home.png"}`); code != http.StatusConflict ||
		body != "task t-2 runs no session now: a workspace file is read from the running session's sandbox" {
		t.Fatalf("no session = %d %q", code, body)
	}
	if len(e.vision.calls) != 1 {
		t.Fatalf("vision calls = %v", e.vision.calls)
	}
}

func TestImageLookRefusals(t *testing.T) {
	e := newLookEnv(t)
	e.serve[uuidA] = []byte("<html>not a picture</html>")
	if code, body := e.look(t, `{"task_id":"t-1","card_image":"img1"}`); code != http.StatusUnsupportedMediaType ||
		body != "img1.png is text/html; charset=utf-8, not a PNG, JPEG, WebP or GIF image" {
		t.Fatalf("not an image = %d %q", code, body)
	}
	e.serve[uuidA] = bytes.Repeat([]byte{1}, policy.MaxLookBytes+1)
	if code, body := e.look(t, `{"task_id":"t-1","card_image":"img1"}`); code != http.StatusRequestEntityTooLarge || body != "img1.png on card sf/app#12 is larger than 8 MiB" {
		t.Fatalf("too large = %d %q", code, body)
	}
	e.serve[uuidA] = pngBytes
	e.vision.err = &policy.Refusal{Decision: "refused_budget", Reason: "task t-1 has 0.000100 EUR left"}
	if code, body := e.look(t, `{"task_id":"t-1","card_image":"img1"}`); code != http.StatusConflict ||
		body != "the look at img1.png was refused (refused_budget): task t-1 has 0.000100 EUR left" {
		t.Fatalf("refused = %d %q", code, body)
	}
	e.vision.err = errors.New("stream ended before [DONE]")
	if code, body := e.look(t, `{"task_id":"t-1","card_image":"img1"}`); code != http.StatusBadGateway ||
		body != "the vision model did not answer on img1.png: try again shortly" {
		t.Fatalf("vision error = %d %q", code, body)
	}
	for in, want := range map[string]string{
		`{"task_id":"t-1"}`: "task_id and exactly one of path and card_image are required",
		`{"task_id":"t-1","path":"a.png","card_image":"img1"}`: "task_id and exactly one of path and card_image are required",
		`{"task_id":"t-9","card_image":"img1"}`:                "no task t-9",
	} {
		if _, body := e.look(t, in); body != want {
			t.Errorf("%s = %q, want %q", in, body, want)
		}
	}

	if code, body := e.look(t, `{"task_id":"t-3","card_image":"img1"}`); code != http.StatusConflict ||
		body != "the comments of card sf/app#14 are longer than 1 MiB, so their images are not read: look at an image of the issue itself or a workspace file" {
		t.Fatalf("long comments = %d %q", code, body)
	}
	if code, body := e.look(t, `{"task_id":"t-4","card_image":"img1"}`); code != http.StatusBadGateway ||
		body != "Forgejo did not answer the read of img1: try again shortly" {
		t.Fatalf("Forgejo failing = %d %q", code, body)
	}

	pv := testPolicyVersion()
	e.runner.cfg.Policy = func() policy.Version { return pv }
	if code, body := e.look(t, `{"task_id":"t-1","card_image":"img1"}`); code != http.StatusConflict ||
		body != `the classes ["coder"] of task t-1 name no vision_model, which a look calls` {
		t.Fatalf("no vision model = %d %q", code, body)
	}
}
