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
	"time"

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
	// Sent: the card's answer was taken and its run has not read it yet.
	Sent bool
	// Page names the page of the ask in view, which a board read keeps.
	Page string
	// Found is the card's own title, linked, on a face that asks, where a
	// find matched the title alone; own is that title.
	Found, own template.HTML
	// Routine is the routine the face stands for, nil for a card.
	Routine *routineFoot
}

// routineFoot is the foot of a routine's face (C16, C46): its key, the
// project and name its drawer opens by, its title in words, which names
// its door, its schedule in words, its next
// run (Soon under an hour away), and its last run, a held run's words
// (Held) or the card it runs (Running).
type routineFoot struct {
	Key, Title          string
	Schedule, Next      string
	Soon, Held, Running bool
	Last                template.HTML
}

// text is what face f says, in words: a card in Needs you its band, its
// paragraph and its answer row, any other its paragraph and its foot.
func (f faceView) text() string {
	w := []string{f.Ref, htmlText(f.Place), htmlText(f.Title) + f.Stop, htmlText(f.Why)}
	for _, m := range f.Marks {
		w = append(w, m.Words)
	}
	if f.Unchecked {
		w = append(w, "brief not checked")
	}
	if !f.Needs {
		return strings.Join(append(w, htmlText(f.State), f.Clock, htmlText(f.Waits), f.Accent), " ")
	}
	w = append(w, f.Since)
	if f.Sent {
		w = append(w, "The answer is on its way to the card.")
	}
	if a := f.Ask; a != nil && a.Suggested != nil {
		w = append(w, a.Suggested.Label, a.By)
	}
	return strings.Join(w, " ")
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

// answerView is one answer of a face: its key, its label, the line that
// says what happens next, who suggests it and on what grounds, the fields
// its press opens with their defaults, and the press that sends it, nil
// where the viewer may not. Thread is the card whose thread its press
// opens.
// Open: its fields stand open, as the small card's pick asked.
type answerView struct {
	Action, Label string
	Effect        template.HTML
	Suggested     bool
	By, Grounds   string
	Fields        []fieldView
	Press         *pressForm
	Thread        string
	Open          bool
}

// pressForm is the form of the one button: where it posts, with what,
// and Target, the element its answer replaces, "" for an answer that
// replaces none.
type pressForm struct {
	Post, DataTo, Target string
	Values               []hidden
	CSRF                 string
	Filter               boardFilter
	// Max bounds the press's words where its door takes fewer than an
	// answer's, 0 where it takes as many.
	Max int
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
		Stale: k.Stale, Final: k.Final() && !needs && !c.working(ctx, v, k), Needs: needs}
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
		f.Since, f.own = n.Waiting(), c.linkRefs(ctx, v, k.Repository, factoryd.CardTitle(k.Task))
		if row := n.Case; row != nil {
			f.Key, f.Page = "case "+k.Place, row.Key
			f.Since = row.Waiting()
			if f.Since != "" && k.Status == "waiting-human" {
				f.Since = "waits for you, " + f.Since
			}
			if row.Owner != nil {
				f.Marks = append(f.Marks, fine{factoryd.HelperStep(row.Owner.Source) + " asks", "flag"})
			}
			c.askFace(ctx, v, &f, k, *row)
		} else {
			// as the opened card: a card a request waits on took no answer
			f.Sent = k.Status == "waiting-human" && !slices.ContainsFunc(v.pending, func(a factoryd.ApprovalRequest) bool { return a.TaskID == k.ID })
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
// the card's own title where the ask is the fallback that names no work,
// its why line, and the answer row, whose one button sends the suggested
// answer as the opened card's would.
func (c *Console) askFace(ctx context.Context, v *boardView, f *faceView, k card, row caseRow) {
	face := row.Face
	lead := face.Ask
	if lead == factoryd.WorkDoneAsk {
		lead = cmp.Or(factoryd.CardTitle(k.Task), lead)
	}
	f.title(c.linkRefs(ctx, v, k.Repository, lead), lead)
	f.Why = c.linkRefs(ctx, v, k.Repository, face.Why)
	facts := factoryd.Facts{CaseFacts: factoryd.FactsOf(row.Case), Card: row.card(k).Task, Case: row.Case}
	late, _ := lateAnswers(v, k, row, facts)
	a := &askView{Position: row.At, Of: row.Of, Others: len(face.Offers) + len(late)}
	if row.Of > 1 {
		base := fmt.Sprintf("/projects/%s/cards/%s/cases/", row.Project, k.ID)
		if row.PrevKey != "" {
			a.Prev = base + row.PrevKey + row.Query
		}
		if row.NextKey != "" {
			a.Next = base + row.NextKey + row.Query
		}
	}
	f.Ask = a
	i := slices.IndexFunc(face.Offers, func(o factoryd.AskOffer) bool { return face.Preselected != "" && o.Value == face.Preselected })
	if i < 0 {
		return
	}
	o := face.Offers[i]
	answer, ok := factoryd.AnswerFor(o.Value, facts)
	if !ok {
		answer, _ = factoryd.AnswerOf(o.Value)
	}
	a.Suggested = &answerView{Action: o.Value, Label: o.Label}
	a.By = pickBy(face.Source)
	a.Others--
	ap := row.Approval
	switch {
	case slices.ContainsFunc(answer.Fields, needsField) || !k.MayWriteSummary:
	case answer.Action == factoryd.ActApproveMerge && k.Kind == "human":
		// the opened card names the head the merge takes
	case ap != nil && (!ap.Approver || answer.Action == factoryd.ActApproveMerge && (len(ap.Unsettled) > 0 || ap.HeadUnread)):
	case ap != nil && row.Case.ID == 0:
		a.Press, _ = requestPress(v, k, *ap, answer, o.Value)
	default:
		a.Press, _ = casePress(v, k, row, answer, o.Value)
	}
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

// approvalFace is resource request a's small card in Needs you, where no
// row of its card holds it: the register's face of the request, its
// suggestion the one button where the viewer may approve.
func (c *Console) approvalFace(ctx context.Context, v *boardView, a approvalCard) faceView {
	f := faceView{ID: a.TaskID, Needs: true, Since: a.Waiting(), Open: a.Writer}
	if a.Issue > 0 {
		f.Ref = "#" + strconv.FormatInt(a.Issue, 10)
	}
	t := v.byID[a.TaskID]
	t.ID, t.Repository = cmp.Or(t.ID, a.TaskID), cmp.Or(t.Repository, a.Project)
	page := factoryd.CardPages(factoryd.BoardTask{Task: t}, nil, nil, []factoryd.ApprovalRequest{a.ApprovalRequest}, func(factoryd.ApprovalRequest) factoryd.Request { return a.Request })[0]
	k, face := page.Case, page.Face
	f.Place = c.placeOf(ctx, v, card{Task: t, PlanName: a.PlanName}, true)
	c.askFace(ctx, v, &f, card{Task: t, MayWriteSummary: a.Writer, Filter: a.Filter}, caseRow{Case: k, Face: v.scrubbed(face), At: 1, Of: 1, Approval: &a})
	return f
}

// scrubbed is face with its texts as a face shows them: no secret, and a
// link only to a host the policy allows.
func (v *boardView) scrubbed(face factoryd.Face) factoryd.Face {
	hosts := v.policy.Document.LinkHosts
	face.Findings = slices.Clone(face.Findings)
	texts := []*string{&face.Ask, &face.Money, &face.Why, &face.Story.Planned, &face.Story.Happened, &face.Story.Left, &face.Story.View, &face.Info, &face.Recommendation, &face.Source}
	for i := range face.Findings {
		texts = append(texts, &face.Findings[i])
	}
	for _, s := range texts {
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
		p, ok, _ := v.planOf(ctx, c, t.ID)
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
		return template.HTML(template.HTMLEscapeString(words)) // #nosec G203 -- the words are escaped above
	case t.PlanCard():
		name := k.PlanName
		if name == "" {
			_, name, _ = c.planIssue(ctx, v.byID, t.Parent)
		}
		if name == "" {
			return ""
		}
		out := `<a class="ref-link" href="#card=` + template.HTMLEscapeString(t.Parent) + `">` + template.HTMLEscapeString(name) + `</a>`
		if p, ok, _ := v.planOf(ctx, c, t.Parent); ok {
			if i := slices.IndexFunc(p.FilingOrder(), func(pc factoryd.PlanCard) bool { return pc.Key == t.PlanKey }); t.PlanKey != "" && i >= 0 {
				out += fmt.Sprintf(" %d/%d", i+1, len(p.Cards))
			}
		}
		return template.HTML(out) // #nosec G203 -- name, parent and the ids are escaped; the rest is constant markup and numbers
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
		return template.HTML("↻ " + template.HTMLEscapeString(name)) // #nosec G203 -- the routine name is escaped
	}
	return ""
}

// planOf is plan id's newest revision, read once a render: ok while it
// holds cards, err when it could not be read.
func (v *boardView) planOf(ctx context.Context, c *Console, id string) (factoryd.Plan, bool, error) {
	if r, ok := v.plans[id]; ok {
		return r.plan, r.plan.Cards != nil, r.err
	}
	_, p, _, err := c.planTable(ctx, id, 0)
	if err != nil {
		log.Printf("console: the plan of %s: %v", id, err)
		v.problems = append(v.problems, "the plan of "+id+" could not be read")
	}
	if v.plans == nil {
		v.plans = map[string]planRead{}
	}
	v.plans[id] = planRead{p, err}
	return p, p.Cards != nil, err
}

// planRead is a plan's read: its newest revision, or why it could not be
// read.
type planRead struct {
	plan factoryd.Plan
	err  error
}

// helperWords are the states a card reads while its helper task of each
// source runs.
var helperWords = map[string]string{factoryd.SourceScreening: "screening", factoryd.SourceSizing: "sizing", factoryd.SourceProver: "writing the proofs"}

// helperNote is what the opened card says of a page of helper task h:
// whose ask it is and what its answers act on.
func helperNote(h card) string {
	step := factoryd.HelperStep(h.Source)
	return "This ask is " + step + "'s, a step of this card: its answers act on " + step + " alone."
}

// working says work runs on card k beyond its own state: its newest
// helper task runs, or it is a plan done planning with a card that has
// not ended. The status line counts it working and its face wears the hue
// of running work.
func (c *Console) working(ctx context.Context, v *boardView, k card) bool {
	if k.helperRuns() {
		return true
	}
	_, open := c.planState(ctx, v, k.Task)
	return open
}

// helperRuns says card k's newest helper task runs: the card's state then
// names the helper's step.
func (k card) helperRuns() bool { return k.Helper != nil && k.Helper.Status == "running" }

// stateOf sets face f's state from card k: its word in its hue, its one
// time, and while it is active the marks of an exceptional state. A card
// whose helper task runs names the helper's step, a merged card where its
// chain stands, a failed one its failure, a done one the chain covered
// how it went live. A finished card on which work still runs wears the
// hue of running work.
func (c *Console) stateOf(ctx context.Context, v *boardView, f *faceView, k card) {
	word := template.HTML(template.HTMLEscapeString(c.stateWord(ctx, v, k.Task))) // #nosec G203 -- stateWord is escaped
	f.Hue = k.StatusClass
	switch {
	case c.working(ctx, v, k):
		f.Hue = statusClass("running")
	case fiveColumn(k.Task) == colQueued:
		f.Hue = statusClass("queued")
	}
	switch {
	case k.helperRuns():
		word = template.HTML(template.HTMLEscapeString(helperWords[k.Helper.Source])) // #nosec G203 -- a constant helper word, escaped
	case k.Status == "merged" && k.Stage != "":
		f.Hue = k.ChainHue()
		stage := template.HTMLEscapeString(k.Stage)
		switch {
		case k.RollID != "":
			word = template.HTML(`<a href="#card=` + template.HTMLEscapeString(k.RollID) + `">` + stage + `</a>`) // #nosec G203 -- the card id and stage are escaped
		case k.StageURL != "":
			word = template.HTML(`<a href="` + template.HTMLEscapeString(k.StageURL) + `" target="_blank" rel="noopener">` + stage + `</a>`) // #nosec G203 -- the forge address is configuration and escaped, the stage is escaped
		default:
			word = c.linkRefs(ctx, v, k.Repository, k.Stage)
		}
	case k.Status == "failed" && k.Failure != "":
		word += ": " + c.linkRefs(ctx, v, k.Repository, sanitize.Outbound(scrubText(k.Failure), v.policy.Document.LinkHosts))
	}
	f.State = word
	switch {
	case k.helperRuns():
		f.Clock = minutes(time.Since(k.Helper.CreatedAt))
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
// queue, and first a human's hold on it; nothing while its helper runs,
// whose step its state names.
func (c *Console) waitsOf(ctx context.Context, v *boardView, k card) template.HTML {
	var words string
	switch {
	case k.helperRuns():
		// the state names the helper's step, the one thing the card waits on
	case k.HoldLine != "":
		words = k.HoldLine
	case k.WaitingOn != "":
		words = "waits for " + k.WaitingOn
	case k.Blocked > 0 && k.Status == "queued":
		words = "waits for " + k.BlockedOn
	case k.Status == "waiting-image":
		words = "waits for its image"
	case k.Status == "queued":
		words = queueWait(k.Task, k.SlotWait)
	case k.Status == "in-review" && k.Gate == factoryd.GateMergeQueue && k.HeldFor > 0:
		words = fmt.Sprintf("waits for plan #%d to close", k.HeldFor)
	case k.Status == "in-review" && k.Gate == factoryd.GateMergeQueue && k.QueueAhead < 0:
		words = "waits to merge after the open plan"
	case k.Status == "in-review" && k.Gate == factoryd.GateMergeQueue && k.MergeBehind > 0:
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
}
