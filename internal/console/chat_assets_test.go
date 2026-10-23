package console

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// assetRig is a chat rig whose tools run without a Do it and whose thread
// holds img1, a PNG, and img2, a JPEG.
func assetRig(t *testing.T) (r *rig, g *chatGateway, jpg []byte, post func(args ...string) []string) {
	t.Helper()
	r, _, g = newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{Confirm: []string{}}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	jpg = b.Bytes()
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("Got both.")}}
	if code, _, _ := chatPostFiles(t, r, ck, s, chatPath, url.Values{"message": {"see img1 and img2"}},
		testFile{name: "shot.png", data: png1x1}, testFile{name: "photo.jpg", data: jpg}); code != 200 {
		t.Fatalf("the send with the images = %d, want 200", code)
	}
	return r, g, jpg, func(call ...string) []string {
		g.replies = [][]string{{call[0]}, {say("Done.")}}
		_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"go"}})
		return events
	}
}

// A card filed with attachments names them in its body, before the field
// lines doFileCard ends it with, and then
// carries them as assets of its issue: the stored bytes under the asset
// names, in the order given, in the form field attachment.
func TestChatNewCardUploadsTheThreadsImages(t *testing.T) {
	r, _, jpg, post := assetRig(t)
	args := `{"title":"Fix the button","body":"It is red.","lane":"ready","size":"small","acceptance":["The button is blue"],"attachments":["img2","img1"]}`
	events := post(call(1, "new_card", args))
	filed := "Filed sf/code#41 in lane ready."
	if len(events) != 4 || !strings.Contains(events[0], `"result":"`+filed+`"`) {
		t.Fatalf("the stream = %v, want the filing alone", events)
	}
	if len(r.fg.issues) != 1 || r.fg.issues[0]["body"] != "It is red.\n\nAcceptance:\n- The button is blue\n\nAttachments: img2.jpg, img1.png\n\nSize: small" {
		t.Fatalf("the issues = %v, want one carrying the Attachments line before the field lines", r.fg.issues)
	}
	equal(t, "the assets", r.fg.assets, []asset{
		{path: "/api/v1/repos/sf/code/issues/41/assets", name: "img2.jpg", field: "attachment", file: "img2.jpg", data: jpg},
		{path: "/api/v1/repos/sf/code/issues/41/assets", name: "img1.png", field: "attachment", file: "img1.png", data: png1x1}})
}

// A name given twice goes up once: two assets of one name would make the
// session's look ask which one.
func TestChatNewCardAttachesARepeatedNameOnce(t *testing.T) {
	r, _, _, post := assetRig(t)
	post(call(1, "new_card", `{"title":"Fix the button","lane":"ready","size":"small","acceptance":["x"],"attachments":["img1","img1"]}`))
	if len(r.fg.issues) != 1 || !strings.Contains(r.fg.issues[0]["body"].(string), "\n\nAttachments: img1.png\n\n") {
		t.Fatalf("the issues = %v, want one naming img1.png once", r.fg.issues)
	}
	equal(t, "the assets", r.fg.assets, []asset{
		{path: "/api/v1/repos/sf/code/issues/41/assets", name: "img1.png", field: "attachment", file: "img1.png", data: png1x1}})
}

// A name the thread holds no image under refuses before any issue exists,
// naming the images there are.
func TestChatNewCardUnknownAttachmentFilesNothing(t *testing.T) {
	r, _, _, post := assetRig(t)
	events := post(call(1, "new_card", `{"title":"Fix the button","lane":"ready","size":"small","acceptance":["x"],"attachments":["img1","img7"]}`))
	if len(events) != 3 || !strings.Contains(events[0], `"result":"Refused: no image \"img7\": the conversation's images are img2, img1"`) {
		t.Fatalf("the stream = %v", events)
	}
	if len(r.fg.issues) != 0 || len(r.fg.assets) != 0 {
		t.Fatalf("%d issues and %d assets, want none", len(r.fg.issues), len(r.fg.assets))
	}
}

// An upload Forgejo refuses leaves the card filed: the answer is the
// filing, and names the images that did not go up.
func TestChatNewCardUploadFailureStillAnswersTheFiling(t *testing.T) {
	r, _, _, post := assetRig(t)
	r.fg.failAssets = true
	events := post(call(1, "new_card", `{"title":"Fix the button","lane":"ready","size":"small","acceptance":["x"],"attachments":["img1","img2"]}`))
	want := `Filed sf/code#41 in lane ready. Forgejo did not take img1, img2: attach them again with comment's attachments.`
	if len(events) != 4 || !strings.Contains(events[0], `"result":"`+want+`"`) {
		t.Fatalf("the stream = %v, want the filing and the failed names", events)
	}
	if len(r.fg.issues) != 1 {
		t.Fatalf("%d issues, want the one filed", len(r.fg.issues))
	}
}

// A comment with an attachment ends in the Attachments line and uploads
// the image as an asset of the comment it posted.
func TestChatCommentUploadsTheThreadsImage(t *testing.T) {
	r, _, _, post := assetRig(t)
	events := post(call(1, "comment", `{"task_id":"t-m","text":"Look at this.","attachments":["img1"]}`))
	if len(events) != 4 || !strings.Contains(events[0], `"result":"Commented on #5."`) {
		t.Fatalf("the stream = %v", events)
	}
	equal(t, "comments", r.fg.comments, map[string]string{"/api/v1/repos/sf/code/issues/5/comments": "Look at this.\n\nAttachments: img1.png"})
	equal(t, "the assets", r.fg.assets, []asset{
		{path: "/api/v1/repos/sf/code/issues/comments/1/assets", name: "img1.png", field: "attachment", file: "img1.png", data: png1x1}})
}

// An unknown name posts nothing; a refused upload leaves the comment and
// names the image.
func TestChatCommentAttachmentRefusalsAndFailure(t *testing.T) {
	r, _, _, post := assetRig(t)
	events := post(call(1, "comment", `{"task_id":"t-m","text":"Look.","attachments":["img4"]}`))
	if len(events) != 3 || !strings.Contains(events[0], `"result":"Refused: no image \"img4\": the conversation's images are img2, img1"`) || len(r.fg.comments) != 0 {
		t.Fatalf("the stream = %v, comments %v, want a refusal posting nothing", events, r.fg.comments)
	}
	r.fg.failAssets = true
	events = post(call(1, "comment", `{"task_id":"t-m","text":"Look.","attachments":["img2"]}`))
	want := `Commented on #5. Forgejo did not take img2: attach them again with comment's attachments.`
	if len(events) != 4 || !strings.Contains(events[0], `"result":"`+want+`"`) || len(r.fg.comments) != 1 {
		t.Fatalf("the stream = %v, comments %v, want the comment standing", events, r.fg.comments)
	}
}
