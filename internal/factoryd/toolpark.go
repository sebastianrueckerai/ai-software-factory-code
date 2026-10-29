package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"
	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceToolRequest is the source of a tool card: the card that adds a
// tool an agent asked for with request_tool to one of its project's
// images, a child of the asking card, which depends on it.
const SourceToolRequest = policy.SourceToolRequest

// imageWait is how long a card parked on an image waits before a human
// hears of it.
const imageWait = 24 * time.Hour

// ImagePark is a card parked on a sandbox image (image_parks): the image
// it waits for, the tool and need it asked for ("" for a session start
// whose image had no pin in force), its tool card and that card's issue
// ("" and 0 for none), why no image comes ("" while one may), when it
// parked and when it was woken (nil while it waits).
type ImagePark struct {
	Task, Project, Target string
	Tool, Need            string
	ToolTask              string
	ToolIssue             int64
	Refused               string
	CreatedAt             time.Time
	WokenAt               *time.Time `json:",omitempty"`
}

// ToolPark is ParkForTool's answer: the park, whose Refused says why no
// image comes; Ask, a question for a human, where the ask repeats one the
// factory refused or one whose image arrived; Note, the next session's
// line, where the ask is refused at once.
type ToolPark struct {
	Park      ImagePark
	Ask, Note string
}

// imagesExample is the tool card's example of factory/images.yaml.
const imagesExample = `images:
  browser:
    apt: [chromium, fonts-liberation]
    upstream:
      - name: node
        url: https://nodejs.org/dist/v24.21.0/node-v24.21.0-linux-x64.tar.xz
        sha256: fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6
        to: /opt/node
        strip: 1
    env:
      CHROME_BIN: /usr/bin/chromium
    path: [/opt/node/bin]
    egress: [registry.npmjs.org]
`

// toolImage is the image a card on its project's default image asks a
// tool into: the tool's name in lowercase letters, digits and dashes, at
// most 32, "tools" when none is left.
func toolImage(tool string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(tool) {
		if !('a' <= r && r <= 'z' || '0' <= r && r <= '9') {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
		dash = false
	}
	s := b.String()
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return cmp.Or(s, "tools")
}

// ParkForTool parks task taskID on the sandbox image tool goes into: the
// card's own image, else toolImage(tool); for tool "" on the image its
// card names, which has no pin in force, with no tool card. A tool goes
// onto a tool card, the open one of the repository and image that has not
// started, which it joins with its need, else a new one; the park's row
// names the card before it is filed, so a retry files it once. The card
// depends on it and waits in waiting-image. A tool card's own ask is
// refused at once, and so is an ask on the configuration repository and a
// new image past the project's MaxImages names, pinned or declared on
// main: a Note. An ask after a refused park, or for the tool whose park on
// the same image arrived, is an Ask for a human; the refusal is spent, so
// the ask after the answer is judged afresh.
func (r *Runner) ParkForTool(ctx context.Context, taskID, tool, need string) (ToolPark, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ToolPark{}, err
	}
	project, c, ok := r.cfg.Policy().ProjectOf(t.Repository)
	if !ok {
		return ToolPark{}, terminalError("task %s: repository %s is no project's", t.ID, t.Repository)
	}
	own := cardImage(t.Image)
	if tool == "" && own == "" {
		return ToolPark{}, terminalError("task %s runs on its project's default image: there is no image to wait for", t.ID)
	}
	p := ImagePark{Task: t.ID, Project: project, Target: cmp.Or(own, toolImage(tool)), Tool: tool, Need: need}
	prev, had, err := r.store.ImagePark(ctx, t.ID)
	if err != nil {
		return ToolPark{}, err
	}
	retry := had && prev.WokenAt == nil && prev.Target == p.Target && prev.Tool == tool && prev.Need == need
	switch {
	case retry && prev.ToolTask == "":
		return r.parkWaits(ctx, t, prev)
	case tool == "":
		if p, err = r.store.ParkImage(ctx, p, true); err != nil {
			return ToolPark{}, err
		}
		return r.parkWaits(ctx, t, p)
	case had && prev.Refused != "":
		return r.askAgain(ctx, t, p, toolAskedAgain+fmt.Sprintf("%s on image %s, after the factory refused its last ask (%s on image %s): %s\nWhy: %s\n"+
			"Recommendation: %s; or answer how the card goes on without the tool, or close this issue to end the card.",
			tool, p.Target, prev.Tool, prev.Target, prev.Refused, need, refusalAdvice(prev.Refused)), true)
	case had && prev.WokenAt != nil && prev.Refused == "" && prev.ToolTask != "" && prev.Target == p.Target && strings.EqualFold(prev.Tool, tool) && cmp.Or(own, "default") != prev.Target:
		return ToolPark{Park: prev}, nil
	case had && prev.WokenAt != nil && prev.Refused == "" && prev.ToolTask != "" && prev.Target == p.Target && strings.EqualFold(prev.Tool, tool):
		with := ""
		if pin, ok := c.Images[p.Target]; ok && len(pin.Tools) > 0 {
			with = " with " + strings.Join(pin.Tools, ", ")
		}
		return r.askAgain(ctx, t, p, toolAskedAgain+fmt.Sprintf("image %[1]s arrived%[2]s; the agent asks for %[3]s again\nWhy: %[4]s\n"+
			"Recommendation: see whether image %[1]s holds the tool where the card looks for it, then answer with what you find; or close this issue to end the card.",
			p.Target, with, tool, need), false)
	case t.Source == SourceToolRequest:
		return r.refuse(ctx, t, p, toolCardRefusal, " Declare the tool and finish: the factory's build and the check of "+imagesPath+" on this pull request test the file.")
	case r.Imager != nil && t.Repository == r.Imager.Config.Repo.String():
		return r.refuse(ctx, t, p, configRepoRefusal, " Go on without the tool, or ask a human with escalate_to_human.")
	}
	raw, declared, err := r.imagesOnMain(ctx, repo)
	if err != nil {
		return ToolPark{}, err
	}
	if retry {
		return r.onToolCard(ctx, t, repo, prev, c.Images, declared, raw)
	}
	if refusal := capRefusal(project, p.Target, c.Images, declared); refusal != "" {
		return r.refuse(ctx, t, p, refusal, " A human must free a name: ask one with escalate_to_human, or go on without the tool.")
	}
	if p.ToolTask, _, err = r.toolCardOf(ctx, t.Repository, p.Target, t.ID); err != nil {
		return ToolPark{}, err
	}
	if p, err = r.store.ParkImage(ctx, p, true); err != nil {
		return ToolPark{}, err
	}
	return r.onToolCard(ctx, t, repo, p, c.Images, declared, raw)
}

// ErrImageRequest refuses a member's ask in the project chat for a tool:
// why, and what to do instead.
type ErrImageRequest string

func (e ErrImageRequest) Error() string { return string(e) }

// ImageAsk is RequestImage's answer: the tool card the ask is on, the
// image it adds the tool to, and whether the ask filed the card or joined
// it.
type ImageAsk struct {
	Card   Task
	Target string
	Filed  bool
}

// elsewhere is the fitting action of an ask refused for its project.
const elsewhere = " Ask for the tool in the chat of the project whose cards need it."

// RequestImage puts the ask of member by in the project chat of
// repository for tool, for need, on the tool card of image image, else of
// one named after the tool (toolImage), as a session's ask goes on it
// (ParkForTool) with no card that waits: it joins the open card of the
// image no session has started, else files one, which admit, when given,
// judges by its title first. Bad arguments, an ask on the configuration
// repository or a project without agents and a new image past the
// project's MaxImages names are ErrImageRequest.
func (r *Runner) RequestImage(ctx context.Context, repository, tool, need, image, by string, admit func(title string) error) (ImageAsk, error) {
	if refusal := policy.ToolAskRefusal(tool, need); refusal != "" {
		return ImageAsk{}, ErrImageRequest(refusal)
	}
	if image != "" && !policy.IsImageName(image) {
		return ImageAsk{}, ErrImageRequest("image " + image + ": lowercase letters, digits and dashes, at most 32; leave it out for an image named after the tool.")
	}
	project, c, ok := r.cfg.Policy().ProjectOf(repository)
	switch {
	case !ok:
		return ImageAsk{}, fmt.Errorf("repository %s is no project's", repository)
	case r.Imager != nil && repository == r.Imager.Config.Repo.String():
		return ImageAsk{}, ErrImageRequest("The factory refuses: " + repository + " is the factory's configuration repository, which declares no sandbox images." + elsewhere)
	case c.Agents == policy.AgentsNone:
		return ImageAsk{}, ErrImageRequest("The factory refuses: project " + project + " runs no agents, so none of its cards runs in a sandbox image." + elsewhere)
	}
	repo, err := SplitRepo(repository)
	if err != nil {
		return ImageAsk{}, err
	}
	raw, declared, err := r.imagesOnMain(ctx, repo)
	if err != nil {
		return ImageAsk{}, err
	}
	p := ImagePark{Project: project, Target: cmp.Or(image, toolImage(tool)), Tool: tool, Need: need}
	if refusal := capRefusal(project, p.Target, c.Images, declared); refusal != "" {
		return ImageAsk{}, ErrImageRequest("The factory refuses: " + refusal + ". Add the tool to one of them with image, or file a card that removes an image from " + imagesPath + ".")
	}
	var open bool
	if p.ToolTask, open, err = r.toolCardOf(ctx, repository, p.Target, ""); err != nil {
		return ImageAsk{}, err
	}
	if !open && admit != nil {
		if err := admit("Add " + tool + " to image " + p.Target); err != nil {
			return ImageAsk{}, err
		}
	}
	card, filed, err := r.putOnToolCard(ctx, toolAsker{repository: repository, by: by}, repo, p, c.Images, declared, raw)
	return ImageAsk{Card: card, Target: p.Target, Filed: filed}, err
}

// imagesOnMain is repo's images.yaml on main and the images it declares,
// nil and none where it has no file.
func (r *Runner) imagesOnMain(ctx context.Context, repo Repo) ([]byte, map[string]policy.ImageSpec, error) {
	raw, _, err := r.forgejo.FileContent(ctx, repo.Owner, repo.Name, prBase, imagesPath)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", imagesPath, err)
	}
	file, _ := policy.ParseImagesFile(raw)
	return raw, file.Images, nil
}

// toolCardOf is the id of the tool card an ask for image target of
// repository goes on: the open card of the image while no session has
// started it (open), else a new one. The asking card self never waits on
// itself.
func (r *Runner) toolCardOf(ctx context.Context, repository, target, self string) (id string, open bool, err error) {
	card, open, err := r.store.OpenToolCard(ctx, repository, target)
	switch {
	case err != nil:
		return "", false, err
	case open && card.ID == self:
		return "", false, terminalError("task %s is the open tool card of image %s: it cannot wait for itself", self, target)
	case open && slices.Contains(notStarted, card.Status):
		return card.ID, true, nil
	}
	return NewTaskID(), false, nil
}

// toolAsker is who asks a tool card for a tool on repository: card, which
// parks on the image, or, card nil, member by in the project chat.
type toolAsker struct {
	repository string
	card       *Task
	by         string
}

// cardAsker is card t as a tool card's asker.
func cardAsker(t Task) toolAsker { return toolAsker{repository: t.Repository, card: &t} }

// askLine opens the tool card of a's ask p: who needs which tool and why.
func (a toolAsker) askLine(p ImagePark) string {
	if a.card == nil {
		return fmt.Sprintf("Requested in the project chat by %s: %s, %s.", a.by, p.Tool, strings.TrimSuffix(p.Need, "."))
	}
	return fmt.Sprintf("Card #%d (%s) needs %s, which its sandbox lacks: %s", a.card.IssueNumber, a.card.ID, p.Tool, p.Need)
}

// joinLine is a's ask p on the open tool card it joins.
func (a toolAsker) joinLine(p ImagePark) string {
	if a.card == nil {
		return fmt.Sprintf("Requested in the project chat by %s: %s in image %s too: %s.", a.by, p.Tool, p.Target, strings.TrimSuffix(p.Need, "."))
	}
	return fmt.Sprintf("Card #%d (%s) needs %s in image %s too: %s", a.card.IssueNumber, a.card.ID, p.Tool, p.Target, p.Need)
}

// after is what runs on park p's image once it is pinned, which the tool
// card's brief names with the rule that follows.
func (a toolAsker) after(p ImagePark) string {
	switch {
	case a.card != nil:
		return fmt.Sprintf("card #%d restarts on image %s from its branch, where it must still build and test this project", a.card.IssueNumber, p.Target)
	case p.Target == "default":
		return "every card that names no image runs on it, so it must still build and test this project"
	}
	return "cards that name image " + p.Target + " run on it, so it must still build and test this project"
}

// base is the image whose entry park p's target starts from, "" for the
// factory image's stack: a card's own image, the factory's where it runs
// on default and default has no pin; for the chat the target's entry on
// main (declared), else default's.
func (a toolAsker) base(p ImagePark, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec) string {
	if a.card == nil {
		for _, name := range []string{p.Target, "default"} {
			if _, ok := declared[name]; ok {
				return name
			}
		}
		return ""
	}
	name := cmp.Or(cardImage(a.card.Image), "default")
	if _, ok := pinned[name]; !ok && name == "default" {
		return ""
	}
	return name
}

// holds is the tool card's sentence on what image name holds now, pin
// where pinned, for a card the image it runs on.
func (a toolAsker) holds(name string, pin policy.ImagePin, pinned bool) string {
	what := "has no pin in force"
	if pinned {
		reach := "no package registry"
		if len(pin.Egress) > 0 {
			reach = strings.Join(pin.Egress, ", ")
		}
		what = "holds " + strings.Join(pin.Tools, ", ") + " and reaches " + reach
	}
	if a.card == nil {
		return "Image " + name + " " + what + "."
	}
	return fmt.Sprintf("Card #%d runs on image %s now, which %s.", a.card.IssueNumber, name, what)
}

// onFactory is the tool card's sentence on the factory's image, for a
// card the image it runs on.
func (a toolAsker) onFactory() string {
	stack := strings.Join(factoryTools, " and ")
	if a.card == nil {
		return "The factory's image is the sandbox base with " + stack + "."
	}
	return fmt.Sprintf("Card #%d runs on the factory's image now: the sandbox base with %s.", a.card.IssueNumber, stack)
}

// notStarted are the statuses of a tool card no session has read yet,
// which a join's need still reaches.
var notStarted = []string{"queued", "waiting-deps"}

// toolCardRefusal is why a tool card's own ask gets no image;
// configRepoRefusal why an ask on the configuration repository does.
const (
	toolCardRefusal   = "this card declares tools in " + imagesPath + ", and its own sandbox never gets them"
	configRepoRefusal = "this card's repository is the factory's configuration repository, which declares no sandbox images"
)

// The refusals of a park at the imager's wake: its tool card closed
// unmerged, merged without declaring the image, or merged with an image
// whose builds failed (the image and how many builds).
const (
	closedUnmerged    = " closed without a merge"
	mergedUndeclared  = " merged without declaring image "
	mergedBuildFailed = " merged, but image %s's build failed %d times; the builder's run shows why"
)

// cardImage is the image name a card keeps: "" for default.
func cardImage(name string) string {
	if name == "default" {
		return ""
	}
	return name
}

// refusalAdvice is what a human does about refusal before answering.
func refusalAdvice(refusal string) string {
	card, rest, _ := strings.Cut(refusal, " ")
	switch {
	case " "+rest == closedUnmerged:
		return "see why " + card + " closed, and reopen it or file the tool's card again, then answer"
	case strings.HasPrefix(" "+rest, mergedUndeclared):
		return "make " + imagesPath + " declare image " + strings.TrimPrefix(" "+rest, mergedUndeclared) + " with the tool, then answer"
	case strings.HasPrefix(" "+rest, " merged, but image "):
		return "read the builder's run, correct the image's entry in " + imagesPath + ", then answer"
	case refusal == toolCardRefusal:
		return "answer that the card declares the tool and finishes without it"
	case refusal == configRepoRefusal:
		return "answer how the card goes on without the tool"
	}
	return "free an image name in " + imagesPath + ", then answer"
}

// refuse records park p of card t as refused for refusal, woken at once,
// and is the next session's line on it, with hint.
func (r *Runner) refuse(ctx context.Context, t Task, p ImagePark, refusal, hint string) (ToolPark, error) {
	p.Refused = refusal
	p, err := r.store.ParkImage(ctx, p, false)
	if err != nil {
		return ToolPark{}, err
	}
	r.record(ctx, t.ID, "image_park", map[string]any{"image": p.Target, "tool": p.Tool, "need": p.Need, "refused": refusal})
	return ToolPark{Park: p, Note: toolRefused(p.Tool, refusal) + hint}, nil
}

// askAgain is ask, card t's question for a human on its ask p; spend
// records p as the latest park, woken and unrefused, so the ask after the
// answer is judged afresh.
func (r *Runner) askAgain(ctx context.Context, t Task, p ImagePark, ask string, spend bool) (ToolPark, error) {
	if spend {
		var err error
		if p, err = r.store.ParkImage(ctx, p, false); err != nil {
			return ToolPark{}, err
		}
	}
	r.record(ctx, t.ID, "image_park", map[string]any{"image": p.Target, "tool": p.Tool, "need": p.Need, "human": true})
	return ToolPark{Park: p, Ask: ask}, nil
}

// onToolCard puts park p of card t, recorded with its tool card's id, on
// that card (putOnToolCard); then t depends on it and waits. pinned and
// declared are the project's images, file its images.yaml on main.
func (r *Runner) onToolCard(ctx context.Context, t Task, repo Repo, p ImagePark, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec, file []byte) (ToolPark, error) {
	card, _, err := r.putOnToolCard(ctx, cardAsker(t), repo, p, pinned, declared, file)
	if err != nil {
		return ToolPark{}, err
	}
	p.ToolIssue = card.IssueNumber
	if err := r.store.AddTaskDeps(ctx, t.ID, []string{card.ID}); err != nil {
		return ToolPark{}, err
	}
	if t.IssueNumber > 0 && card.IssueNumber > 0 {
		if err := r.forgejo.AddIssueDependency(ctx, repo, t.IssueNumber, card.IssueNumber); err != nil {
			log.Printf("task %s: issue dependency on #%d: %v", t.ID, card.IssueNumber, err)
		}
	}
	return r.parkWaits(ctx, t, p)
}

// putOnToolCard puts ask p of a, recorded with its tool card's id, on that
// card: it files it under the id where no card has it, else joins it with
// a's line in its objective and a comment on its issue, each once; filed
// says which. The tool and the need are an agent's, so what Forgejo shows
// carries them as r.untrusted does. pinned and declared are the project's
// images, file its images.yaml on main.
func (r *Runner) putOnToolCard(ctx context.Context, a toolAsker, repo Repo, p ImagePark, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec, file []byte) (Task, bool, error) {
	card, ok, err := r.store.Task(ctx, p.ToolTask)
	if err != nil {
		return Task{}, false, err
	}
	shown, on := p, Task{Repository: a.repository}
	shown.Tool, shown.Need = r.untrusted(on, p.Tool), r.untrusted(on, p.Need)
	if !ok {
		card, err = r.fileToolCard(ctx, a, shown, pinned, declared, file)
		return card, err == nil, err
	}
	if lines := strings.Split(card.Objective, "\n"); !slices.Contains(lines, a.askLine(shown)) {
		join := a.joinLine(shown)
		if !slices.Contains(lines, join) {
			card.Objective += "\n\n" + join
			if err := r.store.SetObjective(ctx, card.ID, card.Objective); err != nil {
				return Task{}, false, err
			}
		}
		if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, card.IssueNumber, join); err != nil {
			return Task{}, false, err
		}
	}
	return card, false, nil
}

// parkWaits moves card t, recorded as parked on p, to waiting-image.
func (r *Runner) parkWaits(ctx context.Context, t Task, p ImagePark) (ToolPark, error) {
	if t.Status != "waiting-image" {
		if err := r.store.TransitionTask(ctx, t.ID, []string{"running"}, "waiting-image"); err != nil {
			return ToolPark{}, terminalError("task %s is %s, cannot wait for an image: %v", t.ID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-image")
		pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	r.record(ctx, t.ID, "image_park", map[string]any{"image": p.Target, "tool": p.Tool, "need": p.Need, "tool_card": p.ToolTask, "issue": p.ToolIssue})
	return ToolPark{Park: p}, nil
}

// capRefusal is why target cannot be a new image of project, which pins
// pinned and whose file on main declares declared: "" while it is one of
// them or the project has room for it.
func capRefusal(project, target string, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec) string {
	names := map[string]bool{}
	for n := range pinned {
		names[n] = true
	}
	for n := range declared {
		names[n] = true
	}
	if names[target] || len(names) < policy.MaxImages {
		return ""
	}
	return fmt.Sprintf("image %s would be new, and project %s has %d images already (%s), the most a project may have",
		target, project, len(names), strings.Join(slices.Sorted(maps.Keys(names)), ", "))
}

// fileToolCard files the tool card of a's ask p on a's repository under
// p.ToolTask, a card's child of its priority and author, the chat
// member's; pinned and declared are the project's images, file its
// images.yaml on main (nil for none).
func (r *Runner) fileToolCard(ctx context.Context, a toolAsker, p ImagePark, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec, file []byte) (Task, error) {
	proj, ok, err := r.store.ProjectByRepository(ctx, a.repository)
	if err != nil {
		return Task{}, err
	}
	if !ok {
		who := "repository " + a.repository
		if a.card != nil {
			who = "task " + a.card.ID + ": " + who
		}
		return Task{}, terminalError("%s is no project's", who)
	}
	now, err := imageNow(a, p, pinned, declared)
	if err != nil {
		return Task{}, err
	}
	title, body := "Add "+p.Tool+" to image "+p.Target, toolCardBody(a, p, now, file)
	card := Task{ID: p.ToolTask, RiskClass: "standard", BudgetEUR: policy.CardBudget(r.cfg.Policy().Document.Budget, proj.Budget, "small", false, 0),
		Objective: title + "\n\n" + body, Repository: a.repository, Source: SourceToolRequest,
		Rung: RungCheap, Size: "small", Author: a.by, AuthorWriter: true}
	if a.card != nil {
		card.Parent, card.Priority, card.Author = a.card.ID, a.card.Priority, a.card.Author
	}
	card, _, err = FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, proj, title, body, nil, card)
	return card, err
}

// imageNow is the tool card's paragraph on what the image holds now that
// a's ask p starts from (base), as an entry of images.yaml for p's
// target: the image's entry on main (declared), or the factory image's
// stack.
func imageNow(a toolAsker, p ImagePark, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec) (string, error) {
	name := a.base(p, pinned, declared)
	if name == "" {
		entry, err := imagesEntry(p.Target, FactoryStack())
		return fmt.Sprintf("%s Image %s starts from this entry, the same stack; add %s to it:\n%s\n\n", a.onFactory(), p.Target, p.Tool, fenced(entry)), err
	}
	pin, ok := pinned[name]
	now := a.holds(name, pin, ok)
	spec, ok := declared[name]
	if !ok {
		return now + "\n\n", nil
	}
	entry, err := imagesEntry(p.Target, spec)
	if p.Target == name {
		return fmt.Sprintf("%s Add %s to its entry:\n%s\n\n", now, p.Tool, fenced(entry)), err
	}
	return fmt.Sprintf("%s Image %s starts as a copy of its entry under the new name; add %s to it:\n%s\n\n", now, p.Target, p.Tool, fenced(entry)), err
}

// imagesEntry is spec as image name of an images.yaml file.
func imagesEntry(name string, spec policy.ImageSpec) (string, error) {
	var b strings.Builder
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(map[string]map[string]policy.ImageSpec{"images": {name: spec}}); err != nil {
		return "", err
	}
	return b.String(), e.Close()
}

// toolCardBody is the brief of the tool card of a's ask p: who asks and
// why, what follows the merge, what the image holds now (imageNow), the
// file on main, its format and the rules that matter.
func toolCardBody(a toolAsker, p ImagePark, now string, file []byte) string {
	var b strings.Builder
	b.WriteString(a.askLine(p) + "\n\n")
	fmt.Fprintf(&b, "Add %s to image %s in %s. After this card merges, the factory builds the image, signs it and pins it, and %s.\n\n",
		p.Tool, p.Target, imagesPath, a.after(p))
	b.WriteString(now)
	if len(file) == 0 {
		b.WriteString(imagesPath + " on main: none yet; create it.\n\n")
	} else {
		b.WriteString(imagesPath + " on main:\n" + fenced(string(file)) + "\n\n")
	}
	b.WriteString("The file's format, one complete example:\n" + fenced(imagesExample) + "\n\n")
	base := make([]string, len(sandboxTools))
	for i, tool := range sandboxTools {
		base[i] = tool.text
	}
	fmt.Fprintf(&b, "Rules:\n- Every image is built FROM the sandbox base, which has only %s. "+
		"Every image declares this project's own stack too: the cards that run on it build and test this project.\n"+
		"- A project has at most %d images. default is the image of every card that names none.\n"+
		"- Prefer apt packages. Add an upstream line only for a file whose sha256 this card can verify, such as from a checksum file it reads; "+
		"the factory's build checks every sha256 and fails on a wrong one.\n"+
		"- egress names only the hosts the image's tools must reach.\n"+
		"- This card runs on the project's default image and never gets the tools it declares: declare them and finish; "+
		"the factory's build and the check of %s on this pull request test the file.\n\n", strings.Join(base, ", "), policy.MaxImages, imagesPath)
	fmt.Fprintf(&b, "Acceptance:\n- %s declares image %s, and %s is in it.\n- %s passes the factory's check, which the policy gate runs on this pull request.\n\n",
		imagesPath, p.Target, p.Tool, imagesPath)
	b.WriteString("Size: small\nRung: cheap")
	return b.String()
}

// ImageArrived puts task taskID on image, which its park waited for, and
// is the next session's line on tool's arrival, naming the image's tools
// as the policy in force pins them; "" for no tool.
func (r *Runner) ImageArrived(ctx context.Context, taskID, image, tool string) (string, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	if err := r.store.SetTaskImageName(ctx, t.ID, cardImage(image)); err != nil {
		return "", err
	}
	if tool == "" {
		return "", nil
	}
	_, c, _ := r.cfg.Policy().ProjectOf(t.Repository)
	if pin, ok := c.Images[image]; ok {
		return fmt.Sprintf("The tool %s arrived: this session runs on image %s (%s).", tool, image, strings.Join(pin.Tools, ", ")), nil
	}
	return fmt.Sprintf("The tool %s arrived: this session runs on image %s.", tool, image), nil
}

// imageWaited opens the question of a card that waited imageWait for its
// image; toolAskedAgain that of an ask for an image refused before.
const (
	imageWaited    = "The card has waited 24 h for image "
	toolAskedAgain = "The agent asks again for a tool: "
)

// imageWaitAsk is the question of park p after imageWait.
func imageWaitAsk(p ImagePark) string {
	if p.ToolTask == "" {
		return fmt.Sprintf(imageWaited+"%[1]s, which has no pin in force: is its build stuck, or is it no longer declared?\n"+
			"Why: the card names image %[1]s and starts once the factory pins it.\n"+
			"Recommendation: see that %[2]s declares %[1]s and the image builds; answer to try the session again, or close this issue to end the card.", p.Target, imagesPath)
	}
	return fmt.Sprintf(imageWaited+"%[1]s, which tool card #%[2]d adds %[3]s to: is #%[2]d or the image's build stuck?\n"+
		"Why: the card restarts on image %[1]s once #%[2]d has merged and the factory has built and pinned the image.\n"+
		"Recommendation: see that #%[2]d merges and the image builds; answer to run the next session on the card's image as it is, or close this issue to end the card.",
		p.Target, p.ToolIssue, p.Tool)
}

// toolRefused is the next session's line on park p's refusal.
func toolRefused(tool, refusal string) string {
	return "The tool " + tool + " did not arrive: " + refusal + ". This session runs on the image the card ran on before."
}

// parkOnImage parks the card on the image tool goes into (ParkForTool),
// for tool "" on the image its card names, and holds it there: the park's
// question goes to a human, its note to the next session at once; else
// the card waits for the image (awaitImage). notes are the next session's
// findings.
func (t *taskRun) parkOnImage(tool, need string) (final, why string, answer, notes []string) {
	var p ToolPark
	if err := workflow.ExecuteActivity(t.short, ActivityParkForTool, t.taskID, tool, need).Get(t.ctx, &p); err != nil {
		return "failed", "park on an image: " + errMessage(err), nil, nil
	}
	switch {
	case p.Ask != "":
		final, why, answer = t.awaitHuman(p.Ask, nil)
		return final, why, answer, nil
	case p.Note != "":
		return "", "", nil, []string{p.Note}
	}
	return t.awaitImage(p.Park, workflow.Now(t.ctx), "")
}

// awaitImage holds the card parked on p.Target, at no cost, until the
// imager's image_ready of this park or a terminal signal; imageWait after
// since it asks a human, whose answer ends the park on the card's image as
// it is, while the image's arrival ends that wait too. asked: a healed
// run's question of that ask, asked again. A park the imager woke already
// ends at once. The image's arrival puts the card on it (ImageArrived);
// every other end leaves the park (leavePark).
func (t *taskRun) awaitImage(p ImagePark, since time.Time, asked string) (final, why string, answer, notes []string) {
	ready := &imageReady{park: p}
	ready.start(t)
	if p.WokenAt != nil {
		ready.sig = &TaskSignal{Image: p.Target, Refusal: p.Refused}
	}
	if left := imageWait - workflow.Now(t.ctx).Sub(since); asked == "" && ready.sig == nil && left > 0 {
		timerCtx, cancel := workflow.WithCancel(t.ctx)
		timer := workflow.NewTimer(timerCtx, left)
		for expired := false; !expired && ready.sig == nil; {
			sel := workflow.NewSelector(t.ctx)
			for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed} {
				sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
			}
			ready.add(sel)
			sel.AddFuture(timer, func(workflow.Future) { expired = true })
			sel.Select(t.ctx)
			if final, why, ok := t.decided(); ok {
				cancel()
				t.leavePark(p)
				return final, why, nil, nil
			}
		}
		cancel()
	}
	if ready.sig == nil {
		if asked != "" {
			if err := t.ask(healedReason, asked, nil); err != nil {
				t.leavePark(p)
				return "failed", escalateFailed + errMessage(err), nil, nil
			}
			final, why, answer = t.awaitAnswer(asked, ready)
		} else {
			final, why, answer = t.awaitHumanUntil(imageWaitAsk(p), nil, ready)
		}
		if final != "" || ready.sig == nil {
			t.leavePark(p)
			return final, why, answer, nil
		}
	}
	if ready.sig.Refusal != "" {
		t.leavePark(p)
		return "", "", answer, []string{toolRefused(p.Tool, ready.sig.Refusal)}
	}
	var line string
	if err := workflow.ExecuteActivity(t.short, ActivityImageArrived, t.taskID, p.Target, p.Tool).Get(t.ctx, &line); err != nil {
		return "failed", "take the image: " + errMessage(err), nil, nil
	}
	if line != "" {
		notes = []string{line}
	}
	return "", "", answer, notes
}

// leavePark ends park p without its image: the card waits on its tool
// card no more (LeaveImagePark). A leave that fails after the activity's
// retries blocks none of the card's work: its timeline notes that the
// issue's dependency on the tool card stays.
func (t *taskRun) leavePark(p ImagePark) {
	if p.ToolTask == "" {
		return
	}
	err := workflow.ExecuteActivity(t.short, ActivityLeaveImagePark, t.taskID, p.ToolTask).Get(t.ctx, nil)
	if err == nil {
		return
	}
	detail := map[string]any{"image": p.Target, "tool_card": p.ToolTask, "dependency_kept": errMessage(err)}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "image_park", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record the image park's kept dependency", "task", t.taskID, "error", err)
	}
}

// LeaveImagePark drops card taskID's dependency on tool card toolTask, in
// task_deps and on Forgejo, where an open dependency keeps the card's
// issue from closing; a Forgejo that fails the drop fails the activity,
// whose retry drops it.
func (r *Runner) LeaveImagePark(ctx context.Context, taskID, toolTask string) error {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if err := r.store.RemoveTaskDeps(ctx, t.ID, []string{toolTask}); err != nil {
		return err
	}
	card, ok, err := r.store.Task(ctx, toolTask)
	if err != nil || !ok || t.IssueNumber == 0 || card.IssueNumber == 0 {
		return err
	}
	return r.forgejo.SetIssueDependency(ctx, repo, t.IssueNumber, card.IssueNumber, true)
}

// imageReady ends a park's waits on the imager's image_ready of it: its
// image and its park's time; an earlier park's signal is passed over.
type imageReady struct {
	park ImagePark
	ctx  workflow.Context
	sig  *TaskSignal
}

func (r *imageReady) start(t *taskRun) { r.ctx = t.ctx }

func (r *imageReady) add(sel workflow.Selector) {
	if r.sig != nil {
		return
	}
	sel.AddReceive(workflow.GetSignalChannel(r.ctx, SignalImageReady), func(c workflow.ReceiveChannel, _ bool) {
		var s TaskSignal
		c.Receive(r.ctx, &s)
		if s.Image == r.park.Target && s.At.Equal(r.park.CreatedAt) {
			r.sig = &s
		}
	})
}

func (r *imageReady) over() bool { return r.sig != nil }

func (r *imageReady) stop() {}

// imageParkColumns reads an image_parks row p with its tool card's issue.
const imageParkColumns = `p.task, p.project, p.target, p.tool, p.need, p.tool_task, COALESCE(t.issue_number, 0), p.refused, p.created_at, p.woken_at
	FROM image_parks p LEFT JOIN tasks t ON t.id = p.tool_task `

// ImagePark is task's latest park on an image.
func (s *PGStore) ImagePark(ctx context.Context, task string) (ImagePark, bool, error) {
	parks, err := s.imageParks(ctx, `SELECT `+imageParkColumns+`WHERE p.task = $1`, task)
	if err != nil || len(parks) == 0 {
		return ImagePark{}, false, err
	}
	return parks[0], true, nil
}

// openParks selects the parks that wait of cards that have not ended.
const openParks = `JOIN tasks a ON a.id = p.task WHERE p.woken_at IS NULL AND a.status <> ALL($1) AND a.status <> 'merged' `

// OpenImageParks are the parks that wait of cards that have not ended,
// the oldest first.
func (s *PGStore) OpenImageParks(ctx context.Context) ([]ImagePark, error) {
	return s.imageParks(ctx, `SELECT `+imageParkColumns+openParks+`ORDER BY p.created_at, p.task`, FinalStatuses)
}

// ProjectImageParks are OpenImageParks of project's cards.
func (s *PGStore) ProjectImageParks(ctx context.Context, project string) ([]ImagePark, error) {
	return s.imageParks(ctx, `SELECT `+imageParkColumns+openParks+`AND p.project = $2 ORDER BY p.created_at, p.task`, FinalStatuses, project)
}

func (s *PGStore) imageParks(ctx context.Context, query string, args ...any) ([]ImagePark, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImagePark
	for rows.Next() {
		var p ImagePark
		if err := rows.Scan(&p.Task, &p.Project, &p.Target, &p.Tool, &p.Need, &p.ToolTask, &p.ToolIssue, &p.Refused, &p.CreatedAt, &p.WokenAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ParkImage records park p as its task's latest, parked now: waiting
// (waits), else woken at once, as a refusal is.
func (s *PGStore) ParkImage(ctx context.Context, p ImagePark, waits bool) (ImagePark, error) {
	err := s.db.QueryRowContext(ctx, `INSERT INTO image_parks (task, project, target, tool, need, tool_task, refused, created_at, woken_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), CASE WHEN $8 THEN NULL ELSE now() END)
		ON CONFLICT (task) DO UPDATE SET project = EXCLUDED.project, target = EXCLUDED.target, tool = EXCLUDED.tool, need = EXCLUDED.need,
			tool_task = EXCLUDED.tool_task, refused = EXCLUDED.refused, created_at = EXCLUDED.created_at, woken_at = EXCLUDED.woken_at
		RETURNING created_at, woken_at`, p.Task, p.Project, p.Target, p.Tool, p.Need, p.ToolTask, p.Refused, waits).Scan(&p.CreatedAt, &p.WokenAt)
	return p, err
}

// WakeImagePark wakes task's park of created, with refused as why no image
// comes; a later park, or one woken already, stays as it is.
func (s *PGStore) WakeImagePark(ctx context.Context, task string, created time.Time, refused string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE image_parks SET woken_at = now(), refused = $3 WHERE task = $1 AND created_at = $2 AND woken_at IS NULL`,
		task, created, refused)
	return err
}

// OpenToolCard is the tool card of repository's image target that has not
// ended or merged, by its title.
func (s *PGStore) OpenToolCard(ctx context.Context, repository, target string) (Task, bool, error) {
	tasks, err := s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE repository = $1 AND source = $2
		AND split_part(objective, E'\n', 1) LIKE 'Add % to image ' || $3 AND status <> ALL($4) AND status <> 'merged'
		ORDER BY created_at DESC LIMIT 1`, repository, SourceToolRequest, target, FinalStatuses)
	if err != nil || len(tasks) == 0 {
		return Task{}, false, err
	}
	return tasks[0], true, nil
}

// ImageReadAt is when a pass last read key as the desired key of project's
// image, zero when none did.
func (s *PGStore) ImageReadAt(ctx context.Context, project, image, key string) (time.Time, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT read_at FROM image_builds WHERE project = $1 AND image = $2 AND key = $3`, project, image, key).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return at.Time, err
}

// SetTaskImageName sets the project image task id runs on, by name.
func (s *PGStore) SetTaskImageName(ctx context.Context, id, image string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET image = $2 WHERE id = $1`, id, image)
	return oneRow(res, err, "task "+id)
}
