package console

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// faceView is a board item's small card (C46): one paragraph of the title
// and its why line, and one foot line of the state with its one time, the
// place, the number, what the card waits for, the unchecked brief and the
// colour. A card in Needs you has the band of its number, place and wait
// in place of the foot, its ask as its title, and the suggested answer.
// The small card marks no terms: its texts take links alone.
type faceView struct {
	// ID is the card's task, Ref its number ("#2100", "" without an
	// issue), Key where the board puts it, which a swap compares.
	ID, Ref, Key string
	Title, Why   template.HTML
	// Stop is the full stop a title without an end mark takes.
	Stop  string
	Place template.HTML
	// State is the state's word, Hue its colour's class, Clock the card's
	// one time and Marks an active card's exceptional states.
	State template.HTML
	Hue   string
	Clock string
	Marks []fine
	Waits template.HTML
	Ask   *askView
	// Needs: the item stands in Needs you, Since how long it waits.
	Needs bool
	Since string
	// Final: done, failed or closed, Status which of them.
	Final, Unchecked, Stale bool
	Status, Accent          string
	// Open: the viewer may open the card.
	Open bool
	// CaseID names the ask in view, which a board read keeps.
	CaseID int64
}

// askView is the answer row of a card in Needs you: the answer suggested
// with who suggests it, the count of the other answers on the page, the
// press that sends the suggestion, and the carousel of the card's asks.
type askView struct {
	Suggested *answerView
	By        string
	Others    int
	// Press sends the suggestion, nil where the small card cannot: an
	// answer with a field to fill, one whose form the opened card holds, or
	// a viewer who may not answer.
	Press        *pressForm
	Position, Of int
	Prev, Next   string
}

// answerView is one answer of a face: its key, its label and the fields
// its press opens.
type answerView struct {
	Action, Label string
	Fields        []factoryd.Field
}

// pressForm is the form of the one button: where it posts, with what.
type pressForm struct {
	Post, DataTo string
	Values       []hidden
	CSRF         string
	Filter       boardFilter
}

// faceOf is the one builder of a board item's small card: a card in its
// column, or a row of Needs you (needs), a card with its ask in view or
// an approval.
func (c *Console) faceOf(ctx context.Context, v *boardView, n need, needs bool) faceView {
	if n.Approval != nil {
		return c.approvalFace(ctx, v, *n.Approval)
	}
	k := n.Card
	f := faceView{ID: k.ID, Key: k.Place, Accent: k.Accent, Status: k.Status, Open: k.MayWriteSummary, Unchecked: k.BriefUnchecked,
		Stale: k.Stale, Final: k.Final() && !needs, Needs: needs}
	if k.IssueNumber > 0 {
		f.Ref = "#" + strconv.FormatInt(k.IssueNumber, 10)
	}
	f.title(c.linkRefs(ctx, v, k.Repository, k.Title), k.Title)
	if k.Why != "" && !f.Final {
		f.Why = c.linkRefs(ctx, v, k.Repository, k.Why)
	}
	f.Place = c.placeOf(ctx, v, k, needs)
	if needs {
		if k.NearBudget {
			f.Marks = append(f.Marks, fine{"⚠ near its budget", "flag"})
		}
		for _, s := range k.Fine {
			f.Marks = append(f.Marks, fine{s.Words, "flag"})
		}
		f.Since = n.Waiting()
		if row := n.Case; row != nil {
			f.Key, f.CaseID = "case "+k.Place, row.Case.ID
			if !row.Placed {
				f.Key = ""
			}
			f.Since = row.Waiting()
			c.askFace(ctx, v, &f, k, *row, n.PlanDecide())
		}
		return f
	}
	c.stateOf(ctx, v, &f, k)
	f.Waits = c.waitsOf(ctx, v, k)
	return f
}

// title sets the face's title, html, from its words: a full stop after
// words that end without a mark.
func (f *faceView) title(html template.HTML, words string) {
	f.Title, f.Stop = html, ""
	if words = strings.TrimSpace(words); words != "" && !strings.ContainsAny(words[len(words)-1:], ".?!") {
		f.Stop = "."
	}
}

// askFace sets face f of card k from its ask row: the ask as its title,
// its why line, and the answer row. plan is the plan ask's form, which
// sends a plan question's suggested choice.
func (c *Console) askFace(ctx context.Context, v *boardView, f *faceView, k card, row caseRow, plan *decide) {
	face := row.Face
	f.title(c.linkRefs(ctx, v, k.Repository, face.Ask), face.Ask)
	f.Why = c.linkRefs(ctx, v, k.Repository, face.Why)
	a := &askView{Position: row.At, Of: row.Of, Others: len(face.Offers)}
	if row.Of > 1 {
		base := fmt.Sprintf("/projects/%s/cards/%s/cases/", row.Project, k.ID)
		if row.PrevID != 0 {
			a.Prev = base + strconv.FormatInt(row.PrevID, 10) + row.Query
		}
		if row.NextID != 0 {
			a.Next = base + strconv.FormatInt(row.NextID, 10) + row.Query
		}
	}
	f.Ask = a
	i := slices.IndexFunc(face.Offers, func(o factoryd.AskOffer) bool { return face.Preselected != "" && o.Value == face.Preselected })
	if i < 0 {
		return
	}
	o := face.Offers[i]
	answer, _ := factoryd.AnswerOf(o.Value)
	a.Suggested = &answerView{Action: o.Value, Label: o.Label, Fields: answer.Fields}
	a.By = pickBy(face.Source)
	a.Others--
	if slices.ContainsFunc(answer.Fields, needsField) || !k.MayWriteSummary {
		return
	}
	press := &pressForm{CSRF: v.csrf, Filter: k.Filter, DataTo: "running"}
	switch {
	case row.Decides:
		press.Post = "/tasks/" + k.ID + "/resolve"
		press.Values = []hidden{{"case_id", strconv.FormatInt(row.Case.ID, 10)}, {"action", o.Value}}
	case row.Answers && factoryd.ChoiceCase(row.Case, k.Kind) && k.Question != nil:
		j := slices.IndexFunc(k.Question.Choices, func(q factoryd.QuestionChoice) bool { return q.Label == o.Label })
		if j < 0 {
			return
		}
		value := k.Question.Choices[j].Value
		press.Post = "/tasks/" + k.ID + "/answer"
		press.Values = []hidden{{"case", strconv.FormatInt(row.Case.ID, 10)}, {"value", value}}
		press.DataTo = "off"
		if slices.Contains([]string{factoryd.AnswerPark, factoryd.AnswerKeepWaiting, factoryd.AnswerRerunAgain}, value) {
			press.DataTo = ""
		}
	case row.Approval != nil:
		ap := row.Approval
		if o.Value != factoryd.ActApproveMerge || !ap.Approver || len(ap.Unsettled) > 0 || ap.HeadUnread {
			return
		}
		press.Post, press.DataTo = fmt.Sprintf("/approvals/%d/approve", ap.ID), ap.TaskStatus
		press.Values = []hidden{{"action", "approve"}}
	case plan != nil && plan.Plan != nil && !plan.Plan.Confirm:
		j := slices.IndexFunc(plan.Plan.Choices, func(p planChoice) bool { return p.Label == o.Label })
		if j < 0 {
			return
		}
		press.Post = "/tasks/" + k.ID + "/plan/answer"
		press.Values = []hidden{{"revision", strconv.Itoa(plan.Plan.Revision)}, {"index", strconv.Itoa(plan.Plan.Index)},
			{"choice", strconv.Itoa(plan.Plan.Choices[j].Index)}}
	default:
		return
	}
	a.Press = press
}

// needsField says the human must fill field f before an answer sends: a
// required one, or an amount or a pick. An optional note may stay empty.
func needsField(f factoryd.Field) bool { return f.Required || f.Kind != factoryd.FieldText }

// pickBy is who suggests an answer as the answer row names it: "project
// agent's pick" for the project agent.
func pickBy(by string) string {
	if by = strings.TrimPrefix(strings.TrimSpace(by), "the "); by == "" {
		return ""
	}
	return by + "'s pick"
}

// approvalFace is approval a's small card in Needs you, where no case of
// its card holds it: the register's face of the request, its suggestion
// the one button where the viewer may approve.
func (c *Console) approvalFace(ctx context.Context, v *boardView, a approvalCard) faceView {
	f := faceView{ID: a.TaskID, Needs: true, Since: a.Waiting(), Open: a.Writer}
	if a.Issue > 0 {
		f.Ref = "#" + strconv.FormatInt(a.Issue, 10)
	}
	t := v.byID[a.TaskID]
	t.ID, t.Repository = cmp.Or(t.ID, a.TaskID), cmp.Or(t.Repository, a.Project)
	k, face := requestFace(a.ApprovalRequest, t)
	f.Place = c.placeOf(ctx, v, card{Task: t, PlanName: a.PlanName}, true)
	c.askFace(ctx, v, &f, card{Task: t, MayWriteSummary: a.Writer, Filter: a.Filter}, caseRow{Case: k, Face: v.scrubbed(face), At: 1, Of: 1, Approval: &a}, nil)
	return f
}

// requestFace is the register's case and face of approval request a on
// card t; a request whose spec does not read faces as its kind's.
func requestFace(a factoryd.ApprovalRequest, t factoryd.Task) (factoryd.Case, factoryd.Face) {
	k, err := factoryd.RequestCase(a)
	switch {
	case a.Kind == factoryd.ApprovalPlan:
		k = factoryd.Case{Kind: factoryd.CasePlanConfirmation, TaskID: a.TaskID, Repository: a.Project}
	case err != nil:
		log.Printf("console: approval request %d: %v", a.ID, err)
		k = factoryd.Case{TaskID: a.TaskID, Repository: a.Project, Facts: map[string]any{"approval": a.ID, "approval_kind": a.Kind}}
	}
	return k, factoryd.CaseFace(k, t, factoryd.CaseAsk{})
}

// scrubbed is face with its texts as a face shows them: no secret, and a
// link only to a host the policy allows.
func (v *boardView) scrubbed(face factoryd.Face) factoryd.Face {
	hosts := v.policy.Document.LinkHosts
	for _, s := range []*string{&face.Ask, &face.Money, &face.Why, &face.Story.Planned, &face.Story.Happened, &face.Story.Left, &face.Story.View, &face.Info, &face.Recommendation, &face.Source} {
		*s = sanitize.Outbound(scrubText(*s), hosts)
	}
	for i := range face.Offers {
		face.Offers[i].Label = sanitize.Outbound(scrubText(face.Offers[i].Label), hosts)
	}
	return face
}

// placeOf is card k's place: a plan's card its plan's name, which opens
// the plan's card, and its position among the plan's cards; a plan its
// name, its title where an ask takes the title's place, and its count of
// cards; a routine's card the routine, by its name
// when its run is known; any other card none.
func (c *Console) placeOf(ctx context.Context, v *boardView, k card, asks bool) template.HTML {
	t := k.Task
	switch {
	case t.Track == policy.TrackPlan:
		p, ok := v.planOf(ctx, c, t.ID)
		words := "plan"
		if name := t.PlanName; name != "" || asks {
			words += " " + cmp.Or(name, factoryd.CardTitle(t))
		}
		if ok && len(p.Cards) > 0 {
			sep := ", "
			if words == "plan" {
				sep = " of "
			}
			words += fmt.Sprintf("%s%d %s", sep, len(p.Cards), plural(len(p.Cards), "card"))
		}
		return template.HTML(template.HTMLEscapeString(words))
	case t.Source == factoryd.SourcePlan && t.Parent != "":
		name := k.PlanName
		if name == "" {
			_, name, _ = c.planIssue(ctx, v.byID, t.Parent)
		}
		if name == "" {
			return ""
		}
		out := `<a class="ref-link" href="#card=` + template.HTMLEscapeString(t.Parent) + `">` + template.HTMLEscapeString(name) + `</a>`
		if p, ok := v.planOf(ctx, c, t.Parent); ok {
			if i := slices.IndexFunc(p.Cards, func(pc factoryd.PlanCard) bool { return pc.Key == t.PlanKey }); t.PlanKey != "" && i >= 0 {
				out += fmt.Sprintf(" %d/%d", i+1, len(p.Cards))
			}
		}
		return template.HTML(out)
	case t.Source == factoryd.SourceRoutine:
		name := "routine"
		if c.cfg.Routines != nil {
			run, ok, err := c.cfg.Routines.RunOfTask(ctx, t.ID)
			if err != nil {
				log.Printf("console: the routine of %s: %v", t.ID, err)
			}
			if ok {
				name = run.Routine
			}
		}
		return template.HTML("↻ " + template.HTMLEscapeString(name))
	}
	return ""
}

// planOf is plan id's newest revision, read once a render.
func (v *boardView) planOf(ctx context.Context, c *Console, id string) (factoryd.Plan, bool) {
	if p, ok := v.plans[id]; ok {
		return p, p.Cards != nil
	}
	_, p, _, err := c.planTable(ctx, id, 0)
	if err != nil {
		log.Printf("console: the plan of %s: %v", id, err)
		v.problems = append(v.problems, "the plan of "+id+" could not be read")
	}
	if v.plans == nil {
		v.plans = map[string]factoryd.Plan{}
	}
	v.plans[id] = p
	return p, p.Cards != nil
}

// stateOf sets face f's state from card k: its word in its hue, its one
// time, and while it is active the marks of an exceptional state. A
// merged card says where its chain stands, a failed one names its
// failure, a done one the chain covered how it went live.
func (c *Console) stateOf(ctx context.Context, v *boardView, f *faceView, k card) {
	word := template.HTML(template.HTMLEscapeString(stateWord(k.Task)))
	f.Hue = k.StatusClass
	if fiveColumn(k.Task) == colQueued {
		f.Hue = statusClass("queued")
	}
	switch {
	case k.Status == "merged" && k.Stage != "":
		f.Hue = k.ChainHue()
		stage := template.HTMLEscapeString(k.Stage)
		switch {
		case k.RollID != "":
			word = template.HTML(`<a href="#card=` + template.HTMLEscapeString(k.RollID) + `">` + stage + `</a>`)
		case k.StageURL != "":
			word = template.HTML(`<a href="` + template.HTMLEscapeString(k.StageURL) + `" target="_blank" rel="noopener">` + stage + `</a>`)
		default:
			word = c.linkRefs(ctx, v, k.Repository, k.Stage)
		}
	case k.Status == "failed" && k.Failure != "":
		word += ": " + c.linkRefs(ctx, v, k.Repository, sanitize.Outbound(scrubText(k.Failure), v.policy.Document.LinkHosts))
	}
	f.State = word
	switch {
	case f.Final && k.Cover != "":
		f.Clock = k.Cover
	case k.Stage != "" && k.StageAge != "":
		f.Clock = k.StageAge
	default:
		f.Clock = k.Total
	}
	if f.Final {
		return
	}
	if k.Checks != "" && k.Status == "in-review" {
		f.Marks = append(f.Marks, fine{k.Checks + " checks", ""})
	}
	if k.NearBudget {
		f.Marks = append(f.Marks, fine{"⚠ near its budget", "flag"})
	}
	for _, s := range k.Fine {
		f.Marks = append(f.Marks, fine{s.Words, "flag"})
	}
}

// waitsOf is why card k does not go on, in words, each card it waits for
// a link with its title and state: the cards it depends on, its image,
// its turn among the queued cards, or the cards ahead of it in the merge
// queue.
func (c *Console) waitsOf(ctx context.Context, v *boardView, k card) template.HTML {
	var words string
	switch {
	case k.WaitingOn != "":
		words = "waits for " + k.WaitingOn
	case k.Blocked > 0 && k.Status == "queued":
		words = "waits for " + k.BlockedOn
	case k.Status == "waiting-image":
		words = "waits for its image"
	case k.Status == "queued":
		words = queueWait(k.Task, k.SlotWait)
	case k.Status == "in-review" && k.Gate == factoryd.GateMergeQueue && k.QueueAhead < 0:
		words = "waits to merge after the open plan"
	case k.Status == "in-review" && k.Gate == factoryd.GateMergeQueue && k.QueueAhead > 0 && k.MergeBehind > 0:
		words = fmt.Sprintf("waits to merge behind #%d", k.MergeBehind)
		if more := k.QueueAhead - 1; more > 0 {
			words += fmt.Sprintf(" and %d more %s", more, plural(more, "card"))
		}
	case k.Status == "in-review" && k.Gate == factoryd.GateMergeQueue && k.QueueAhead > 0:
		words = fmt.Sprintf("waits to merge behind %d %s", k.QueueAhead, plural(k.QueueAhead, "card"))
	}
	return c.linkRefs(ctx, v, k.Repository, words)
}

// queueWait is why queued card t does not start: its turn among the
// queued cards (slot, with the count when known) while admit recorded no
// other refusal, else that refusal in words; "" for a refusal it has no
// words for.
func queueWait(t factoryd.Task, slot string) string {
	switch r := t.QueueReason; {
	case slot != "":
		return slot
	case r == "" || strings.HasPrefix(r, factoryd.WorkBoundReason):
		return "waits for its turn to start"
	case r == factoryd.QueueMonthCap:
		return "waits for the project's month cap to rise"
	case strings.HasPrefix(r, factoryd.QueueWaitsOn):
		return "waits for #" + strings.TrimPrefix(r, factoryd.QueueWaitsOn)
	case r == factoryd.QueueNoIssue:
		return "waits for its issue to be filed"
	}
	return ""
}

// setFaces builds the face of every item of d.
func (c *Console) setFaces(ctx context.Context, v *boardView, d *boardData) {
	for i := range d.Columns {
		col := &d.Columns[i]
		for j := range col.Needs {
			col.Needs[j].Face = c.faceOf(ctx, v, col.Needs[j], true)
		}
		for j := range col.Cards {
			col.Cards[j].Face = c.faceOf(ctx, v, need{Card: col.Cards[j]}, false)
		}
	}
	for i := range d.Approvals {
		d.Approvals[i].Face = c.faceOf(ctx, v, d.Approvals[i], true)
	}
	for i := range d.Escalations {
		d.Escalations[i].Face = c.faceOf(ctx, v, d.Escalations[i], true)
	}
	for i := range d.HumanTasks {
		d.HumanTasks[i].Face = c.faceOf(ctx, v, need{Card: d.HumanTasks[i]}, false)
	}
}
