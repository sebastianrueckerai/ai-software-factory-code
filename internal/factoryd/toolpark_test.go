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

// One ask files one tool card on the project's repository: a small child
// of the asking card on the cheap rung, which the asking card depends on,
// with the need, the file on main, the format, the base's tools and the
// cap. The card waits in waiting-image.
func TestParkForToolFilesOneCard(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n"
	r, store, rf, wf := parkRig(t, file, "default")
	ctx := context.Background()
	p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut the demo video to 10 s")
	if err != nil {
		t.Fatal(err)
	}
	if p.Human || p.Park.Refused != "" || p.Park.Target != "ffmpeg" || p.Park.ToolTask == "" || p.Park.ToolIssue != 31 || p.Park.CreatedAt.IsZero() {
		t.Fatalf("park = %+v, want a tool card for image ffmpeg", p)
	}
	card := store.tasks[p.Park.ToolTask]
	if card.Source != SourceToolRequest || card.Parent != "t-asker" || card.Rung != RungCheap || card.Size != "small" || card.Lane != policy.LaneReady ||
		card.Status != "queued" || card.Priority != 2 || card.IssueNumber != 31 || card.Repository != codeRepo {
		t.Fatalf("tool card = %+v", card)
	}
	body := "Card #41 (t-asker) needs ffmpeg, which its sandbox lacks: cut the demo video to 10 s\n\n" +
		"Add ffmpeg to image ffmpeg in factory/images.yaml. After this card merges, the factory builds the image, signs it and pins it, " +
		"and card #41 restarts on it from its branch.\n\n" +
		"factory/images.yaml on main:\n```\n" + file + "```\n\n" +
		"The file's format, one complete example:\n```\n" + imagesExample + "```\n\n" +
		"Rules:\n" +
		"- Every image is built FROM the sandbox base, which has only git, curl, ripgrep (rg), ssh, gcc and make, python3, PostgreSQL 17 (server and client, for tests). " +
		"An image that must also build and test this project declares the project's own stack too.\n" +
		"- A project has at most 4 images. default is the image of every card that names none.\n" +
		"- An upstream line needs the real sha256 of its file, as its publisher lists it.\n" +
		"- egress names only the hosts the tool must reach.\n\n" +
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

	// a card on the default image asks into an image named for its tool
	second, err := r.ParkForTool(ctx, "t-second", "SoX 14", "mix the trailer's sound")
	if err != nil {
		t.Fatal(err)
	}
	if second.Park.Target != "sox-14" || second.Park.ToolTask == p.Park.ToolTask || len(rf.issues) != 2 ||
		!strings.HasPrefix(rf.issues[1], "Add SoX 14 to image sox-14\n\n") {
		t.Fatalf("second park = %+v, issues %q: want its own card for image sox-14", second, rf.issues)
	}
}

// A second ask for the same image joins the open tool card: the comment
// on its issue and the need in its brief, no second card; the joining card
// depends on it too.
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

// A new image past the project's MaxImages names, pinned or declared, is
// refused at once: no card, the card keeps running for its next session.
// The same ask again goes to a human with the refusal, and the refusal is
// spent: the ask after the human's answer is judged afresh. An ask for a
// name the project has passes the cap.
func TestParkForToolRefusesANewImageAtTheCap(t *testing.T) {
	const file = "images:\n  default:\n    apt: [jq]\n  video:\n    apt: [ffmpeg]\n  docs:\n    apt: [pandoc]\n"
	r, store, rf, _ := parkRig(t, file, "default", "browser")
	ctx := context.Background()
	refusal := "image sox would be new, and project ai-software-factory-code has 4 images already (browser, default, docs, video), the most a project may have: " +
		"a human must free a name, so ask one with escalate_to_human or go on without sox"
	p, err := r.ParkForTool(ctx, "t-asker", "sox", "mix the sound")
	if err != nil {
		t.Fatal(err)
	}
	if p.Park.Refused != refusal || p.Human || p.Park.ToolTask != "" || len(rf.issues) != 0 {
		t.Fatalf("park = %+v, issues %d; want the cap's refusal, no card", p, len(rf.issues))
	}
	if store.tasks["t-asker"].Status != "running" {
		t.Fatalf("refused card is %s, want running", store.tasks["t-asker"].Status)
	}
	again, err := r.ParkForTool(ctx, "t-asker", "sox", "mix the sound")
	if err != nil {
		t.Fatal(err)
	}
	if again.Park.Refused != refusal || !again.Human || len(rf.issues) != 0 {
		t.Fatalf("the second ask = %+v, want the refusal for a human", again)
	}
	if row, _, _ := store.ImagePark(ctx, "t-asker"); row.Refused != "" {
		t.Fatalf("row after the human's turn = %+v, want the refusal spent", row)
	}
	third, err := r.ParkForTool(ctx, "t-asker", "sox", "mix the sound")
	if err != nil || third.Human || third.Park.Refused != refusal {
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

// A card refused an image before goes to a human when it asks for it again
// whatever refused it: here the tool card closed without a merge.
func TestParkForToolAfterAWakeRefusal(t *testing.T) {
	r, store, rf, _ := parkRig(t, "", "default")
	ctx := context.Background()
	if _, err := store.ParkImage(ctx, ImagePark{Task: "t-asker", Project: "ai-software-factory-code", Target: "ffmpeg", Tool: "ffmpeg",
		Refused: "#31 closed without a merge"}, false); err != nil {
		t.Fatal(err)
	}
	p, err := r.ParkForTool(ctx, "t-asker", "ffmpeg", "cut")
	if err != nil || !p.Human || p.Park.Refused != "#31 closed without a merge" || len(rf.issues) != 0 {
		t.Fatalf("park = %+v %v, issues %d", p, err, len(rf.issues))
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
	if p.Park.Target != "video" || p.Park.ToolTask != "" || p.Park.Refused != "" || p.Human || len(rf.issues) != 0 {
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
// OpenToolCard finds the open card of a project's image, OpenImageParks
// the waiting rows, and WakeImagePark wakes the park it names only.
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
		{ID: tool, Status: "queued", Repository: "sf/parks", IssueNumber: 50, Objective: "Add ffmpeg to image video"},
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
	if card, ok, err := s.OpenToolCard(ctx, "parks", "video"); err != nil || !ok || card.ID != tool || card.IssueNumber != 50 {
		t.Fatalf("open tool card = %+v %v %v", card, ok, err)
	}
	if _, ok, err := s.OpenToolCard(ctx, "parks", "browser"); err != nil || ok {
		t.Fatalf("another image's card: %v %v", ok, err)
	}
	refused, err := s.ParkImage(ctx, ImagePark{Task: other, Project: "parks", Target: "sox", Tool: "sox", Refused: "the cap"}, false)
	if err != nil || refused.WokenAt == nil {
		t.Fatalf("a refused park = %+v %v, want it woken at once", refused, err)
	}
	open, err := s.OpenImageParks(ctx)
	if err != nil || len(open) != 1 || open[0].Task != asker || open[0].ToolIssue != 50 {
		t.Fatalf("open parks = %+v %v, want the asker's alone", open, err)
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
	// a merged tool card is open no more
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'merged', finished_at = now() WHERE id = $1`, tool); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.OpenToolCard(ctx, "parks", "video"); err != nil || ok {
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
// "tool|need", and ImageArrived with the activity's line, recording each
// call as "image|tool" and its time.
func (h *workflowHarness) onPark(parks ...ToolPark) {
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
	h.onPark(ToolPark{Park: ImagePark{Target: "sox", Tool: "sox", Refused: "image sox would be new"}})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	want := "The tool sox did not arrive: image sox would be new. This session runs on the image the card ran on before."
	if len(h.sessions) != 2 || !slices.Contains(h.sessions[1].Findings, want) || len(h.arrived) != 0 || len(h.escalated) != 0 {
		t.Fatalf("sessions %+v, arrived %q, escalated %q", h.sessions, h.arrived, h.escalated)
	}
	if h.sessions[1].Sessions != 4 {
		t.Fatalf("second session's cap %d, want 4", h.sessions[1].Sessions)
	}
}

// A refusal of an image the card was refused before goes to a human, as
// an escalation does; the answer runs the next session.
func TestWorkflowRepeatedToolRefusalAsksAHuman(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(missingTool("s1", "sox", "mix"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.onPark(ToolPark{Park: ImagePark{Target: "sox", Tool: "sox", Refused: "#50 closed without a merge"}, Human: true})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "do without sox", Comment: 700})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	want := "The agent asks again for sox on image sox, which the factory refused: #50 closed without a merge\nWhy: mix\n" +
		"Recommendation: free an image name or fix factory/images.yaml, then answer: the agent may ask again; or answer how to go on without the tool, or close this issue to end the card."
	if !slices.Equal(h.escalated, []string{want}) || escalationSource(want) != "image" {
		t.Fatalf("escalated %q, want %q", h.escalated, want)
	}
	if len(h.sessions) != 2 || len(h.sessions[1].Answers) != 1 || h.sessions[1].Sessions != 4 {
		t.Fatalf("sessions = %+v", h.sessions)
	}
}

// A wake with a refusal (the tool card closed unmerged) keeps the image and
// tells the next session why.
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
	if len(h.arrived) != 0 || len(h.sessions) != 2 || !slices.Contains(h.sessions[1].Findings, want) {
		t.Fatalf("arrived %q, sessions %+v", h.arrived, h.sessions)
	}
}

// 24 hours without a wake ask a human, naming the tool card and the
// image; the image's arrival ends that wait too, with no answer, and a
// human's answer ends the park on the card's image as it is.
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
		if !slices.Equal(h.arrived, []string{"ffmpeg|ffmpeg"}) || len(h.sessions) != 2 || len(h.sessions[1].Answers) != 0 {
			t.Fatalf("arrived %q, sessions %+v", h.arrived, h.sessions)
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
		if len(h.arrived) != 0 || len(h.sessions) != 2 || len(h.sessions[1].Answers) != 1 || h.sessions[1].Sessions != 4 {
			t.Fatalf("arrived %q, sessions %+v", h.arrived, h.sessions)
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
		if len(h.escalated) != 0 || len(h.sessions) != 1 {
			t.Fatalf("escalated %q, sessions %d", h.escalated, len(h.sessions))
		}
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
