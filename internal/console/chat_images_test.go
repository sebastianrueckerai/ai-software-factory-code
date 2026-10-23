package console

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat's default context names the project's sandbox images, the pins
// in force with their tools, and what a card without an image runs: the
// factory image, or image default where the project pins it.
func TestChatContextNamesTheSandboxImages(t *testing.T) {
	const status = "\n\nThe project now:\nStatus: waiting on you; 1 working; 3 need you; spend this month €0.00 of €50.00: the cards €0.00, the chat €0.00\n"
	for _, tc := range []struct {
		pins map[string]policy.ImagePin
		line string
	}{
		{nil, "Sandbox images: none; every card runs the factory image."},
		{map[string]policy.ImagePin{"chromium": {Tools: []string{"chromium", "fonts-liberation", "go", "node"}}},
			"Sandbox images: chromium (chromium, fonts-liberation, go, node); a card without an image runs the factory image."},
		{map[string]policy.ImagePin{"video": {Tools: []string{"ffmpeg"}}, "default": {Tools: []string{"jq", "go"}}},
			"Sandbox images: default (jq, go), video (ffmpeg); a card without an image runs image default."},
	} {
		r, _, g := newChatRig(t)
		r.projects.rows[0].Images = tc.pins
		ck, s := r.loggedIn(t)
		g.replies = [][]string{{say("ok")}}
		chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what runs where?"}})
		system, _ := messages(t, g.calls()[0])[0]["content"].(string)
		if !strings.HasSuffix(system, status+tc.line) {
			t.Errorf("the system message does not end with %q:\n%s", status+tc.line, system)
		}
	}
}

// project_images reads the project's pins in force with their tools,
// egress, short digest and key, its open tool cards (a merged one is open
// no more) and its cards waiting in waiting-image with the image each
// waits for; another project's park is not the project's.
func TestChatProjectImagesRead(t *testing.T) {
	r, g := newReadsRig(t)
	equal(t, "without images", reads(t, r, g, "project_images", `{}`),
		[]string{"Pinned images: none.\nOpen tool cards: none.\nCards in waiting-image: none."})

	r.projects.rows[0].Images = map[string]policy.ImagePin{
		"chromium": {Digest: "sha256:abcdef0123456789" + strings.Repeat("0", 48), Key: "k-0123456789abcdef01234567",
			Tools: []string{"chromium", "fonts-liberation", "go", "node"}, Egress: []string{"registry.npmjs.org"}},
		"default": {Digest: "sha256:" + strings.Repeat("d", 64), Key: "k-" + strings.Repeat("e", 24), Tools: []string{"jq"}},
	}
	r.tasks.mu.Lock()
	for _, k := range []factoryd.Task{
		{ID: "t-tool", Status: "queued", IssueNumber: 50, Source: factoryd.SourceToolRequest, Objective: "Add sox to image video\n\nRequested in the project chat by sebastian: mix."},
		{ID: "t-old", Status: "merged", IssueNumber: 49, Source: factoryd.SourceToolRequest, Objective: "Add jq to image default\n\nRequested."},
		{ID: "t-wait", Status: "waiting-image", IssueNumber: 41, Image: "video", Objective: "Mix the trailer."},
		{ID: "t-w2", Status: "waiting-image", IssueNumber: 42, Image: "browser", Objective: "Shoot the page."},
	} {
		k.Kind, k.Repository = "agent", "sf/code"
		r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: k})
	}
	r.tasks.parks = []factoryd.ImagePark{
		{Task: "t-wait", Project: "ai-software-factory-code", Target: "video", Tool: "sox", ToolTask: "t-tool", ToolIssue: 50},
		{Task: "t-w2", Project: "ai-software-factory-code", Target: "browser"},
		{Task: "t-x", Project: "demo-app", Target: "video", Tool: "sox", ToolTask: "t-y", ToolIssue: 7},
	}
	r.tasks.mu.Unlock()
	equal(t, "the images", reads(t, r, g, "project_images", `{}`), []string{"Pinned images:\n" +
		"  chromium: tools chromium, fonts-liberation, go, node; egress registry.npmjs.org; digest sha256:abcdef012345; key k-0123456789abcdef01234567\n" +
		"  default: tools jq; egress none; digest sha256:dddddddddddd; key k-eeeeeeeeeeeeeeeeeeeeeeee\n" +
		"Open tool cards:\n" +
		"  #50 t-tool \"Add sox to image video\": queued\n" +
		"Cards in waiting-image:\n" +
		"  #41 t-wait: image video, tool card #50\n" +
		"  #42 t-w2: image browser, no tool card"})
}

// new_card's image is a name the project pins in force: the issue's body
// carries it as the Image line the intake reads into the card's row, and
// the filing's row names it. default names the default image, which a
// card without an image runs: no line. Any other name is refused with the
// pinned names and request_image.
func TestChatNewCardImage(t *testing.T) {
	pins := map[string]policy.ImagePin{"chromium": {Tools: []string{"chromium"}}, "default": {Tools: []string{"jq"}}}
	r, g := newWritesRig(t)
	r.projects.rows[0].Images = pins
	before := r.events.count()
	args := `{"title":"Shoot the page","body":"Screenshot the landing page.","lane":"backlog","image":"chromium"}`
	proposed, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"File a card, title Shoot the page, body Screenshot the landing page., lane backlog, image chromium","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 in lane backlog."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Screenshot the landing page.\nImage: chromium")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"lane": "backlog", "title": "Shoot the page", "image": "chromium", "thread": int64(1)}})

	r, g = newWritesRig(t)
	r.projects.rows[0].Images = pins
	args = `{"title":"Shoot the page","body":"Screenshot the landing page.","lane":"backlog","image":"default"}`
	_, done, _ = proposeAndDo(t, r, g, "new_card", args)
	equal(t, "Do it on default", done, ran("new_card", args, "Filed sf/code#41 in lane backlog."))
	equal(t, "the issue's body on default", r.fg.issues[0]["body"], "Screenshot the landing page.")

	code, msg := refused(t, r, "new_card", toolInput{Title: "Shoot the page", Lane: policy.LaneBacklog, Image: "video"})
	equal(t, "an unpinned image", []any{code, msg}, []any{http.StatusConflict,
		"Project ai-software-factory-code pins no image video: its images are chromium, default. Name one of them, leave image out for the default, or add the image with request_image."})
	r.projects.rows[0].Images = nil
	code, msg = refused(t, r, "new_card", toolInput{Title: "Shoot the page", Lane: policy.LaneBacklog, Image: "video"})
	equal(t, "a project without images", []any{code, msg}, []any{http.StatusConflict,
		"Project ai-software-factory-code pins no sandbox images. Leave image out for the default, or add the image with request_image."})
}

// request_image is the member's ask for a tool, confirmed as new_card is:
// Do it hands the project's repository, the arguments and the member to
// factoryd, which files or joins the tool card the result names. A
// refusal of factoryd's reaches the agent as it is.
func TestChatRequestImage(t *testing.T) {
	const after = " After it merges, the factory builds, signs and pins the image; cards can then name it in image."
	r, g := newWritesRig(t)
	r.imageCard, r.imageFiled = factoryd.Task{ID: "t-tool", IssueNumber: 50, Objective: "Add ffmpeg to image video\n\nRequested in the project chat by sebastian: cut it."}, true
	before := r.events.count()
	args := `{"image":"video","need":"cut the demo video","tool":"ffmpeg"}`
	proposed, done, _ := proposeAndDo(t, r, g, "request_image", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Ask for a sandbox tool, tool ffmpeg, need cut the demo video, image video","id":"TOKEN","tool":"request_image"}`,
		`done {}`})
	equal(t, "Do it", done, ran("request_image", args, `Filed tool card #50 t-tool \"Add ffmpeg to image video\".`+after))
	equal(t, "the ask", r.imageAsks, []string{"sf/code: ffmpeg | cut the demo video | video, by sebastian"})
	chatRows(t, r.events, before, "sebastian")

	r.imageFiled = false
	_, done, _ = proposeAndDo(t, r, g, "request_image", `{"need":"mix the sound","tool":"sox"}`)
	equal(t, "a join", done, ran("request_image", `{"need":"mix the sound","tool":"sox"}`,
		`The ask joined tool card #50 t-tool \"Add ffmpeg to image video\", which no session has started.`+after))

	r.imageErr = factoryd.ErrImageRequest("The factory refuses: image sox would be new.")
	code, msg := refused(t, r, "request_image", toolInput{Tool: "sox", Need: "mix"})
	equal(t, "factoryd's refusal", []any{code, msg}, []any{http.StatusConflict, "The factory refuses: image sox would be new."})
	r.imageErr = errors.New("the store is down")
	code, msg = refused(t, r, "request_image", toolInput{Tool: "sox", Need: "mix"})
	equal(t, "a failure", []any{code, msg}, []any{http.StatusBadGateway, "The tool card could not be filed; see the factoryd log."})
}
