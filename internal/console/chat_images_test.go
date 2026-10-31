package console

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat's default context names the project's sandbox images, the pins
// in force with their tools, and what a card without an image runs: the
// factory image, or image default where the project pins it. A project
// without agents has no board and runs no card in a sandbox: its status
// line says so with its real spend against its month cap, its context
// names no images, and its agent is offered neither project_images nor
// request_image.
func TestChatContextNamesTheSandboxImages(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	status := "\n\nThe project now:\nToday is " + today + " UTC.\nStatus: waiting on you; 1 working; 3 need you; spend this month €0.00 of €50.00: the cards €0.00, the chat €0.00"
	for _, tc := range []struct {
		agents string
		pins   map[string]policy.ImagePin
		line   string
	}{
		{policy.AgentsCoder, nil, "\nSandbox images: none; every card runs the factory image."},
		{policy.AgentsCoder, map[string]policy.ImagePin{"chromium": {Tools: []string{"chromium", "fonts-liberation", "go", "node"}}},
			"\nSandbox images: chromium (chromium, fonts-liberation, go, node); a card without an image runs the factory image."},
		{policy.AgentsCoder, map[string]policy.ImagePin{"video": {Tools: []string{"ffmpeg"}}, "default": {Tools: []string{"jq", "go"}}},
			"\nSandbox images: default (jq, go), video (ffmpeg); a card without an image runs image default."},
		{policy.AgentsNone, map[string]policy.ImagePin{"video": {Tools: []string{"ffmpeg"}}}, ""},
	} {
		r, chat, g := newChatRig(t)
		want := status
		if tc.agents == policy.AgentsNone {
			chat.spend = []spendRow{{"sf/code", time.Now(), 3.25, false}, {"sf/code", time.Now(), 0.5, true}}
			want = "\n\nThe project now:\nToday is " + today + " UTC.\nStatus: agents none, every issue goes to the human lane; " +
				"spend this month €3.75 of €50.00: the cards €3.25, the chat €0.50"
		}
		r.projects.rows[0].Agents, r.projects.rows[0].Images = tc.agents, tc.pins
		ck, s := r.loggedIn(t)
		g.replies = [][]string{{say("ok")}}
		chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what runs where?"}})
		call := g.calls()[0]
		system, _ := messages(t, call)[0]["content"].(string)
		if !strings.HasSuffix(system, want+tc.line) {
			t.Errorf("%s: the system message does not end with %q:\n%s", tc.agents, want+tc.line, system)
		}
		var offered []string
		for _, tool := range call["tools"].([]any) {
			if name := tool.(map[string]any)["function"].(map[string]any)["name"].(string); name == "project_images" || name == "request_image" {
				offered = append(offered, name)
			}
		}
		if want := map[string][]string{policy.AgentsCoder: {"project_images", "request_image"}}[tc.agents]; !slices.Equal(offered, want) {
			t.Errorf("%s: the image tools offered are %v, want %v", tc.agents, offered, want)
		}
	}
}

// A project factoryd does not work on, its file gone, has no board
// either: its status line says so with its spend against its month cap.
func TestChatStatusOfAProjectFactorydDoesNotWorkOn(t *testing.T) {
	r, _, g := newChatRig(t)
	r.projects.rows[0].File = false
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("ok")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"how is it?"}})
	system, _ := messages(t, g.calls()[0])[0]["content"].(string)
	want := "\n\nThe project now:\nToday is " + time.Now().UTC().Format("2006-01-02") + " UTC.\nStatus: no board, factoryd does not work on its repository; spend this month €0.00 of €50.00: the cards €0.00, the chat €0.00\n" +
		"Sandbox images: none; every card runs the factory image."
	if !strings.HasSuffix(system, want) {
		t.Errorf("the system message does not end with %q:\n%s", want, system)
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
	args := `{"title":"Shoot the page","body":"Screenshot the landing page.","acceptance":["The page is shot"],"track":"direct","track_reason":"one screenshot","size":"small","image":"chromium"}`
	proposed, done, _ := proposeAndDo(t, r, g, "new_card", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"File a card, title Shoot the page, body Screenshot the landing page., track direct, track_reason one screenshot, size small, image chromium, acceptance The page is shot","id":"TOKEN","tool":"new_card"}`,
		`done {}`})
	equal(t, "Do it", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body", r.fg.issues[0]["body"], "Screenshot the landing page.\n\nAcceptance:\n- The page is shot\n\nSize: small\nImage: chromium")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", IssueNumber: 41, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "reason": "one screenshot", "title": "Shoot the page", "size": "small", "image": "chromium", "thread": int64(1)}})

	r, g = newWritesRig(t)
	r.projects.rows[0].Images = pins
	args = `{"title":"Shoot the page","body":"Screenshot the landing page.","acceptance":["The page is shot"],"track":"direct","track_reason":"one screenshot","size":"small","image":"default"}`
	_, done, _ = proposeAndDo(t, r, g, "new_card", args)
	equal(t, "Do it on default", done, ran("new_card", args, "Filed sf/code#41 on track direct."))
	equal(t, "the issue's body on default", r.fg.issues[0]["body"], "Screenshot the landing page.\n\nAcceptance:\n- The page is shot\n\nSize: small")

	unpinned := "Project ai-software-factory-code pins no image video: its images are chromium, default. Name one of them, leave image out for the default, or add the image with request_image."
	code, msg := refused(t, r, "new_card", toolInput{Title: "Shoot the page", Track: policy.TrackDirect, TrackReason: "one screenshot", Image: "video"})
	equal(t, "an unpinned image", []any{code, msg}, []any{http.StatusConflict, unpinned})
	// the proposal judges the image, so the agent corrects it before Do it
	ck, s := r.loggedIn(t)
	args = `{"image":"video","title":"Shoot the page","track":"direct","track_reason":"one screenshot"}`
	g.replies = [][]string{{call(1, "new_card", args)}, {say("Refused.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"file it on video"}})
	equal(t, "the proposal of an unpinned image", events, []string{
		`tool {"arguments":` + args + `,"duration_ms":0,"name":"new_card","result":"Refused: ` + unpinned + `"}`, `text {"text":"Refused."}`, `done {}`})
	r.projects.rows[0].Images = nil
	code, msg = refused(t, r, "new_card", toolInput{Title: "Shoot the page", Track: policy.TrackDirect, TrackReason: "one screenshot", Image: "video"})
	equal(t, "a project without images", []any{code, msg}, []any{http.StatusConflict,
		"Project ai-software-factory-code pins no sandbox images. Leave image out for the default, or add the image with request_image."})
}

// request_image is the member's ask for a tool, confirmed as new_card is:
// Do it hands the project's repository, the arguments and the member to
// factoryd, which files or joins the tool card the result names. A filed
// card is recorded as the chat's filing, with its track, title, image and
// tool; a join files nothing and records nothing. A refusal of factoryd's
// reaches the agent as it is.
func TestChatRequestImage(t *testing.T) {
	const after = " After it merges, the factory builds, signs and pins the image; cards can then name it in image."
	tool := factoryd.Task{ID: "t-tool", IssueNumber: 50, Track: policy.TrackDirect, Objective: "Add ffmpeg to image video\n\nRequested in the project chat by sebastian: ffmpeg, cut it."}
	r, g := newWritesRig(t)
	r.imageAsk = factoryd.ImageAsk{Card: tool, Target: "video", Filed: true}
	before := r.events.count()
	args := `{"image":"video","need":"cut the demo video","tool":"ffmpeg"}`
	proposed, done, _ := proposeAndDo(t, r, g, "request_image", args)
	equal(t, "proposal", proposed, []string{
		`proposal {"arguments":` + args + `,"description":"Ask for a sandbox tool, tool ffmpeg, need cut the demo video, image video","id":"TOKEN","tool":"request_image"}`,
		`done {}`})
	equal(t, "Do it", done, ran("request_image", args, `Filed tool card #50 t-tool \"Add ffmpeg to image video\".`+after))
	equal(t, "the ask", r.imageAsks, []string{"sf/code: ffmpeg | cut the demo video | video, by sebastian"})
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-tool", IssueNumber: 50, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "title": "Add ffmpeg to image video", "image": "video", "tool": "ffmpeg", "thread": int64(1)}})

	r.imageAsk.Filed = false
	before = r.events.count()
	_, done, _ = proposeAndDo(t, r, g, "request_image", `{"need":"mix the sound","tool":"sox"}`)
	equal(t, "a join", done, ran("request_image", `{"need":"mix the sound","tool":"sox"}`,
		`The ask joined tool card #50 t-tool \"Add ffmpeg to image video\", which no session has started.`+after))
	chatRows(t, r.events, before, "sebastian")

	r.imageErr = factoryd.ErrImageRequest("The factory refuses: image sox would be new.")
	code, msg := refused(t, r, "request_image", toolInput{Tool: "sox", Need: "mix"})
	equal(t, "factoryd's refusal", []any{code, msg}, []any{http.StatusConflict, "The factory refuses: image sox would be new."})
	r.imageErr = errors.New("the store is down")
	code, msg = refused(t, r, "request_image", toolInput{Tool: "sox", Need: "mix"})
	equal(t, "a failure", []any{code, msg}, []any{http.StatusBadGateway, "The tool card could not be filed; see the factoryd log."})
}

// At assisted the agent asks for a tool without the member's OK, as it
// files a card: the filing's row names the level, the agent as the
// decider and the chat task, which the inbox's poke reads.
func TestChatRequestImageAtAssisted(t *testing.T) {
	r, _, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	r.imageAsk = factoryd.ImageAsk{Card: factoryd.Task{ID: "t-tool", IssueNumber: 50, Track: policy.TrackDirect, Objective: "Add ffmpeg to image ffmpeg\n\nRequested."},
		Target: "ffmpeg", Filed: true}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	g.replies = [][]string{{call(1, "request_image", `{"need":"cut the demo video","tool":"ffmpeg"}`)}, {say("Asked.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"we need ffmpeg"}})
	if !strings.Contains(events[0], `"result":"Filed tool card #50 t-tool`) {
		t.Fatalf("the unconfirmed ask did not run: %v", events)
	}
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-tool", IssueNumber: 50, Kind: factoryd.EventCardFiled,
		Detail: map[string]any{"track": "direct", "title": "Add ffmpeg to image ffmpeg", "image": "ffmpeg", "tool": "ffmpeg", "autonomy": "assisted", "merge_rule": "human",
			"decider": "agent", "chat_task": factoryd.ChatTaskID("ai-software-factory-code", time.Now()), "thread": int64(1)}})
}

// request_image's filing meets the chat's admission, as new_card's does:
// a wake files no tool card on a closed intake, and no ask files one past
// the project's queue cap. The refusal names the state and what to do.
func TestChatRequestImageAdmission(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeHuman}}
	r.imageAsk = factoryd.ImageAsk{Card: factoryd.Task{ID: "t-tool", IssueNumber: 50, Track: policy.TrackDirect}, Target: "ffmpeg", Filed: true}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/projects/ai-software-factory-code/intake", url.Values{"csrf": {s.csrf}, "intake": {"closed"}}), ck); rec.Code != http.StatusNoContent {
		t.Fatalf("the intake switch's POST = %d: %s", rec.Code, rec.Body.String())
	}
	before := r.events.count()
	g.replies = [][]string{{call(1, "request_image", `{"need":"cut the demo video","tool":"ffmpeg"}`)}, {say("The intake is closed.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_heartbeat", "The daily heartbeat: look for what to start."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, row := range rows {
		for _, tc := range row.ToolCalls {
			results = append(results, tc.Result)
		}
	}
	equal(t, "the wake's ask", results, []string{
		"Refused: The intake of ai-software-factory-code is closed: a wake files nothing. Tell the member what needs filing; their own ask still files and starts."})
	chatRows(t, r.events, before, "sebastian")

	// t-m, t-r and t-e, none admitted, fill a cap of 3
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: policy.MergeHuman, QueueCap: 3}}
	code, msg := refused(t, r, "request_image", toolInput{Tool: "ffmpeg", Need: "cut"})
	equal(t, "an ask past the queue cap", []any{code, msg}, []any{http.StatusConflict,
		"The ready check refuses the filing: card Add ffmpeg to image ffmpeg: the queue holds 3 of its 3 cards; file it when one starts"})
	chatRows(t, r.events, before, "sebastian")
}
