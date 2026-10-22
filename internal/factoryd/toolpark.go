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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceToolRequest is the source of a tool card: the card that adds a
// tool an agent asked for with request_tool to one of its project's
// images, a child of the asking card, which depends on it.
const SourceToolRequest = "tool_request"

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
// image comes, and Human when the card was refused that image before.
type ToolPark struct {
	Park  ImagePark
	Human bool
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
// onto a tool card, the open one of the project and image, which it joins
// with its need, else a new one; the card depends on it and waits in
// waiting-image. A new image past the project's MaxImages names, pinned
// or declared on main, is refused at once, no card filed, and so is an
// image the card was refused before: then Human, and the refusal is
// spent, so the ask after a human's answer is judged afresh. A retry finds
// the park it made.
func (r *Runner) ParkForTool(ctx context.Context, taskID, tool, need string) (ToolPark, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ToolPark{}, err
	}
	project, c, ok := r.cfg.Policy().ProjectOf(t.Repository)
	if !ok {
		return ToolPark{}, terminalError("task %s: repository %s is no project's", t.ID, t.Repository)
	}
	if tool == "" && t.Image == "" {
		return ToolPark{}, terminalError("task %s runs on its project's default image: there is no image to wait for", t.ID)
	}
	p := ImagePark{Task: t.ID, Project: project, Target: cmp.Or(t.Image, toolImage(tool)), Tool: tool, Need: need}
	prev, had, err := r.store.ImagePark(ctx, t.ID)
	switch {
	case err != nil:
		return ToolPark{}, err
	case had && prev.WokenAt == nil && prev.Target == p.Target && prev.Tool == tool && prev.Need == need:
		return r.parkWaits(ctx, t, prev, false)
	case tool == "":
		return r.parkWaits(ctx, t, p, true)
	}
	raw, _, err := r.forgejo.FileContent(ctx, repo.Owner, repo.Name, prBase, imagesPath)
	if errors.Is(err, ErrNotFound) {
		raw, err = nil, nil
	}
	if err != nil {
		return ToolPark{}, fmt.Errorf("read %s: %w", imagesPath, err)
	}
	file, _ := policy.ParseImagesFile(raw)
	refusedBefore := had && prev.Refused != ""
	if refusedBefore && prev.Target == p.Target {
		p.Refused = prev.Refused
	} else {
		p.Refused = capRefusal(project, p.Target, tool, c.Images, file.Images)
	}
	if p.Refused != "" {
		row := p
		if refusedBefore {
			row.Refused = ""
		}
		if _, err := r.store.ParkImage(ctx, row, false); err != nil {
			return ToolPark{}, err
		}
		r.record(ctx, t.ID, "image_park", map[string]any{"image": p.Target, "tool": tool, "need": need, "refused": p.Refused, "human": refusedBefore})
		return ToolPark{Park: p, Human: refusedBefore}, nil
	}
	card, open, err := r.store.OpenToolCard(ctx, project, p.Target)
	if err != nil {
		return ToolPark{}, err
	}
	if open {
		join := fmt.Sprintf("Card #%d (%s) needs %s in image %s too: %s", t.IssueNumber, t.ID, tool, p.Target, need)
		if !strings.Contains(card.Objective, join) {
			if err := r.store.SetObjective(ctx, card.ID, card.Objective+"\n\n"+join); err != nil {
				return ToolPark{}, err
			}
			if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, card.IssueNumber, join); err != nil {
				return ToolPark{}, err
			}
		}
	} else if card, err = r.fileToolCard(ctx, t, p, raw); err != nil {
		return ToolPark{}, err
	}
	p.ToolTask, p.ToolIssue = card.ID, card.IssueNumber
	if err := r.store.AddTaskDeps(ctx, t.ID, []string{card.ID}); err != nil {
		return ToolPark{}, err
	}
	if t.IssueNumber > 0 && card.IssueNumber > 0 {
		if err := r.forgejo.AddIssueDependency(ctx, repo, t.IssueNumber, card.IssueNumber); err != nil {
			log.Printf("task %s: issue dependency on #%d: %v", t.ID, card.IssueNumber, err)
		}
	}
	return r.parkWaits(ctx, t, p, true)
}

// parkWaits records park p of card t, unless a retry finds it recorded,
// and moves the card to waiting-image.
func (r *Runner) parkWaits(ctx context.Context, t Task, p ImagePark, write bool) (ToolPark, error) {
	if write {
		var err error
		if p, err = r.store.ParkImage(ctx, p, true); err != nil {
			return ToolPark{}, err
		}
	}
	if t.Status != "waiting-image" {
		if err := r.store.TransitionTask(ctx, t.ID, []string{"running"}, "waiting-image"); err != nil {
			return ToolPark{}, terminalError("task %s is %s, cannot wait for an image: %v", t.ID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-image")
	}
	r.record(ctx, t.ID, "image_park", map[string]any{"image": p.Target, "tool": p.Tool, "need": p.Need, "tool_card": p.ToolTask, "issue": p.ToolIssue})
	return ToolPark{Park: p}, nil
}

// capRefusal is why target cannot be a new image of project, which pins
// pinned and whose file on main declares declared: "" while it is one of
// them or the project has room for it.
func capRefusal(project, target, tool string, pinned map[string]policy.ImagePin, declared map[string]policy.ImageSpec) string {
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
	return fmt.Sprintf("image %s would be new, and project %s has %d images already (%s), the most a project may have: "+
		"a human must free a name, so ask one with escalate_to_human or go on without %s", target, project, len(names), strings.Join(slices.Sorted(maps.Keys(names)), ", "), tool)
}

// fileToolCard files the tool card of park p, asked for by card t, on t's
// repository: its file is factory/images.yaml on main, nil for none.
func (r *Runner) fileToolCard(ctx context.Context, t Task, p ImagePark, file []byte) (Task, error) {
	proj, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return Task{}, err
	}
	if !ok {
		return Task{}, terminalError("task %s: repository %s is no project's", t.ID, t.Repository)
	}
	title, body := "Add "+p.Tool+" to image "+p.Target, toolCardBody(t, p, file)
	card, _, err := FileAgentTask(ctx, r.store, r.forgejo, r.cfg.Workflows, r.timeline, proj, title, body, nil, Task{
		ID: NewTaskID(), Priority: t.Priority, RiskClass: "standard", BudgetEUR: policy.CardBudget(r.cfg.Policy().Document.Budget, proj.Budget, "small", false, 0),
		Objective: title + "\n\n" + body, Repository: t.Repository, Source: SourceToolRequest, Parent: t.ID,
		Rung: RungCheap, Size: "small", Author: t.Author, AuthorWriter: true,
	})
	return card, err
}

// toolCardBody is the brief of park p's tool card: the need and the asking
// card t, the file on main, its format, the rules that matter and what
// follows the merge.
func toolCardBody(t Task, p ImagePark, file []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Card #%d (%s) needs %s, which its sandbox lacks: %s\n\n", t.IssueNumber, t.ID, p.Tool, p.Need)
	fmt.Fprintf(&b, "Add %s to image %s in %s. After this card merges, the factory builds the image, signs it and pins it, and card #%d restarts on it from its branch.\n\n",
		p.Tool, p.Target, imagesPath, t.IssueNumber)
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
	fmt.Fprintf(&b, "Rules:\n- Every image is built FROM the sandbox base, which has only %s. An image that must also build and test this project declares the project's own stack too.\n"+
		"- A project has at most %d images. default is the image of every card that names none.\n"+
		"- An upstream line needs the real sha256 of its file, as its publisher lists it.\n"+
		"- egress names only the hosts the tool must reach.\n\n", strings.Join(base, ", "), policy.MaxImages)
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
	if err := r.store.SetTaskImageName(ctx, t.ID, image); err != nil {
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
	toolAskedAgain = "The agent asks again for "
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
// for tool "" on the image its card names, and holds it there: a refusal
// goes to the next session at once, or to a human when the card was
// refused that image before; else the card waits for the image
// (awaitImage). notes are the next session's findings.
func (t *taskRun) parkOnImage(tool, need string) (final, why string, answer, notes []string) {
	var p ToolPark
	if err := workflow.ExecuteActivity(t.short, ActivityParkForTool, t.taskID, tool, need).Get(t.ctx, &p); err != nil {
		return "failed", "park on an image: " + errMessage(err), nil, nil
	}
	switch {
	case p.Park.Refused != "" && p.Human:
		final, why, answer = t.awaitHuman(fmt.Sprintf(toolAskedAgain+"%s on image %s, which the factory refused: %s\nWhy: %s\n"+
			"Recommendation: free an image name or fix %s, then answer: the agent may ask again; or answer how to go on without the tool, or close this issue to end the card.",
			tool, p.Park.Target, p.Park.Refused, need, imagesPath), nil)
		return final, why, answer, nil
	case p.Park.Refused != "":
		return "", "", nil, []string{toolRefused(tool, p.Park.Refused)}
	}
	return t.awaitImage(p.Park, workflow.Now(t.ctx), "")
}

// awaitImage holds the card parked on p.Target, at no cost, until the
// imager's image_ready of this park or a terminal signal; imageWait after
// since it asks a human, whose answer ends the park on the card's image as
// it is, while the image's arrival ends that wait too. asked: a healed
// run's question of that ask, asked again. A park the imager woke already
// ends at once. The image's arrival puts the card on it (ImageArrived).
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
				return final, why, nil, nil
			}
		}
		cancel()
	}
	if ready.sig == nil {
		if asked != "" {
			if err := t.ask(healedReason, asked, nil); err != nil {
				return "failed", escalateFailed + errMessage(err), nil, nil
			}
			final, why, answer = t.awaitAnswer(asked, ready)
		} else {
			final, why, answer = t.awaitHumanUntil(imageWaitAsk(p), nil, ready)
		}
		if final != "" || ready.sig == nil {
			return final, why, answer, nil
		}
	}
	if ready.sig.Refusal != "" {
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

// OpenImageParks are the parks that wait, the oldest first.
func (s *PGStore) OpenImageParks(ctx context.Context) ([]ImagePark, error) {
	return s.imageParks(ctx, `SELECT `+imageParkColumns+`WHERE p.woken_at IS NULL ORDER BY p.created_at, p.task`)
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

// OpenToolCard is the tool card of project's image target that has not
// ended or merged, the newest park's.
func (s *PGStore) OpenToolCard(ctx context.Context, project, target string) (Task, bool, error) {
	tasks, err := s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = (SELECT p.tool_task FROM image_parks p JOIN tasks t ON t.id = p.tool_task
		WHERE p.project = $1 AND p.target = $2 AND t.status NOT IN ('done', 'failed', 'closed', 'merged') ORDER BY p.created_at DESC LIMIT 1)`, project, target)
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
