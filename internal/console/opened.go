package console

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// openedView is the opened card (C46), the drawer's first tab: the head,
// which is the small card's face read large, the ask block of the card's
// open ask, the stored summary, and the folds, each closed: the agent's
// view, the full brief, the plan, the card's life, what each session got,
// the other actions and what the factory's words mean.
type openedView struct {
	Face faceView
	// Issue and PR are the links of the card's number and its open pull
	// request, PRNumber the pull request's.
	Issue, PR string
	PRNumber  int64
	Ask       *askBlock
	// Sent: the card's answer was taken and its run has not read it yet.
	// Pending: answers on the card's issue its run has not taken.
	Sent, Pending bool
	// Problems are why a merge the human approved has not merged yet and
	// what of the card's requests could not be read.
	Problems []string
	Summary  template.HTML
	Brief    briefFold
	Plan     *planFold
	Life     lifeFold
	Sessions []sessionFold
	// AgentView is the exact text the card's agent gets, "" when it could
	// not be read.
	AgentView string
	More      []answerView
	Glosses   []gloss
	// SessionsHint and MoreHint are the hints of their folds.
	SessionsHint, MoreHint string
}

// askBlock is the open ask of a card: the question, why it came up and the
// story, then the answers, the suggested one first. Position of Of is the
// page among the card's open asks, Prev and Next the pages beside it. Who
// names who may answer, for a viewer who may not.
type askBlock struct {
	Question, Why template.HTML
	Story         []storyLine
	// Words are the writers' words too long for the story, each whole
	// in a fold of its own
	Words        []ownWords
	Answers      []answerView
	Position, Of int
	Prev, Next   string
	// Who says who may give the answers the viewer may not; Note what a
	// disabled answer waits on, or what ends the request without a press.
	Who, Note string
	// denied are the labels of the answers only an approver may give,
	// which the viewer may not
	denied []string
}

// whoMay names who may give the answers of ask its viewer may not: the
// line stands beside the answers the viewer may give, or alone in their
// place when the viewer may give none.
func (ask *askBlock) whoMay() {
	switch n := len(ask.denied); {
	case n == 0:
	case n == len(ask.Answers):
		ask.Answers, ask.Who = nil, "Only the project's approvers may answer this."
	default:
		last := ask.denied[n-1]
		if n > 1 {
			last = strings.Join(ask.denied[:n-1], ", ") + " or " + last
		}
		ask.Who = "Only the project's approvers may choose " + last + "."
	}
}

// storyLine is a line of the ask's story; Quote is a writer's own words.
type storyLine struct {
	Lead  template.HTML
	Quote string
}

// ownWords are a writer's words in a fold of their own: Title names the
// writer, Hint counts the words.
type ownWords struct{ Title, Hint, Text string }

// fieldView is one field of an answer as its press opens it: the name it
// posts under, its label, the hint of a required field, its default, and
// for a choice or a box its options.
type fieldView struct {
	ID, Name, Label, Hint, Value string
	Kind                         string
	Required                     bool
	// Max bounds a note's words, as its door takes them.
	Max     int
	Options []optionView
}

type optionView struct {
	Value, Label string
	Checked      bool
}

// briefFold is the full brief: the card's own text, its steps, the
// acceptance items and the criteria they cite, the cards it depends on and
// what they delivered; for a card that asks, the evidence.
type briefFold struct {
	Body       template.HTML
	Acceptance []string
	Criteria   []string
	DependsOn  []template.HTML
	Carries    []template.HTML
	Evidence   []evidence
	Approvals  []approvalCard
	Hint       string
}

// evidence is one part of an ask's evidence: a label and its text, as
// markup or as a quote of the writer's words; Findings counts the open
// findings a list of them holds.
type evidence struct {
	Label    string
	Text     template.HTML
	Quote    string
	Raw      string
	Findings int
}

// planFold is the plan a card belongs to, or a plan's own: its outcome,
// its cards with this one marked, its section when the card builds one,
// and the link that opens the plan's card.
type planFold struct {
	Name, Outcome string
	Cards         []planRow
	Section       template.HTML
	Whole         string
	Hint          string
}

type planRow struct {
	Issue int
	Title template.HTML
	State string
	This  bool
}

// lifeFold is the card's life: its runs, the line that sums them, and
// where it came from.
type lifeFold struct {
	Runs      []run
	Line      string
	Thread    *threadLink
	Routine   *routineProvenance
	GitHub    string
	GitHubURL string
	GitHubPR  int64
	Diff      string
	// Spent of Budget is the card's money, Near from 80 percent of it,
	// Calls its model calls in tokens and modeled cost, Model its agent's.
	Spent, Budget float64
	Near          bool
	Calls, Model  string
}

// sessionFold is what one session of the card got: its number, how it
// ended and the exact text its model read.
type sessionFold struct {
	N       int
	Outcome string
	Text    string
}

// opened is the opened card of card bt, k as the board builds it, its
// timeline's steps read: problem names a part that could not be read.
func (c *Console) opened(ctx context.Context, r *http.Request, v *boardView, bt factoryd.BoardTask, k card, steps []map[string]any, problem func(what, action string, err error)) *openedView {
	seen := map[string]int{}
	o := &openedView{Face: c.faceOf(ctx, v, need{Card: k}, false), Issue: k.IssueURL}
	o.Face.Why = c.linkRefs(ctx, v, k.Repository, k.Why)
	// the opened card says how long a stale card has been quiet
	if i := slices.IndexFunc(o.Face.Marks, func(m fine) bool { return m.Words == "stale" }); i >= 0 && k.StaleText != "" {
		o.Face.Marks[i].Words += ", " + k.StaleText
	}
	if k.PRURL != "" && k.Status != "merged" && !factoryd.Final(k.Status) {
		o.PR, o.PRNumber = k.PRURL, k.PRNumber
	}
	o.Summary = c.linkRefs(ctx, v, k.Repository, k.Summary)
	o.Pending = bt.AnswersPending > 0

	facts := factoryd.Facts{CaseFacts: c.roomFactsOf(ctx, v, bt.Task), Card: bt.Task}
	for _, a := range v.pending {
		if a.TaskID != bt.ID {
			continue
		}
		ap := c.approvalCard(ctx, a, button(r), v.byID, v.csrf, v.approves(a.Project), false)
		if v.requests == nil {
			v.requests = map[int64]approvalCard{}
		}
		v.requests[a.ID] = ap
		ap.Filter, ap.Writer, ap.IDSuffix = k.Filter, true, "-d"
		if a.Kind == factoryd.ApprovalMerge && a.DecisionNote != "" {
			o.Problems = append(o.Problems, a.DecisionNote)
		}
		if ap.Problem != "" {
			o.Problems = append(o.Problems, ap.Problem)
		}
		if a.Kind == factoryd.ApprovalPlan {
			ap.scrubLayers(v.policy.Document.LinkHosts)
		}
		o.Brief.Approvals = append(o.Brief.Approvals, ap)
	}
	cs, err := c.caseSetOf(ctx, bt.Repository, bt)
	casesRead := err == nil
	if !casesRead {
		problem("cases", "Reload the board.", err)
	}
	v.cases = cs
	var row *caseRow
	if cs.holds(bt.Task) {
		row = c.caseNeed(ctx, v, button(r), bt, k, cs, casesInView(r)[bt.ID]).Case
	}
	onCase, helperPage := false, false
	switch {
	case row != nil:
		f := factoryd.Facts{CaseFacts: factoryd.FactsOf(row.Case), Card: row.card(k).Task, Case: row.Case,
			Recommend: factoryd.Recommendation{Action: row.Face.Preselected, By: row.Face.Source, Grounds: row.Face.Recommendation, Args: row.Face.PreselectedArgs}}
		if f.Card.Kind == "human" {
			// a human card ends closed by End the card, so the cards waiting for it ask
			waiters, err := c.cfg.Tasks.Dependents(ctx, f.Card.ID)
			if err != nil {
				log.Printf("console: the cards waiting for %s: %v", f.Card.ID, err)
			}
			f.Waiters = factoryd.LiveDependents(waiters)
		}
		o.Ask = c.askOf(ctx, v, k, &o.Face, row.Face, seen)
		o.Ask.Position, o.Ask.Of = row.At, row.Of
		if p, ok := factoryd.PromiseOn(row.Case); ok && !factoryd.PromiseFits(p.Promise) {
			// a promise in a plan's own words too long for the story
			o.Ask.Words = append(o.Ask.Words, ownWords{Title: "The promise in the plan's words", Hint: fmt.Sprintf("%d words", len(strings.Fields(p.Promise))),
				Text: sanitize.Outbound(scrubText(p.Promise), v.policy.Document.LinkHosts)})
		}
		base := "/cards/" + k.ID + "/drawer?tab=overview&case=" + url.QueryEscape(k.ID) + ":"
		if row.PrevKey != "" {
			o.Ask.Prev = base + row.PrevKey
		}
		if row.NextKey != "" {
			o.Ask.Next = base + row.NextKey
		}
		o.Ask.Answers = c.answersOf(ctx, v, k, k.ID+"-"+row.Key, row.Face, f, func(a factoryd.Answer, key string) (*pressForm, func(string) string) {
			if !mayAnswer(v, k, a.Action) || requestPage(row.Case) && row.Approval != nil && approverOnly(v, k, *row.Approval, a) {
				o.Ask.denied = append(o.Ask.denied, a.Label)
				return nil, nil
			}
			if o.Pending && a.OwnWords() {
				// the answers the run has not taken come first
				return nil, nil
			}
			return casePress(v, k, *row, a, key)
		}, r.URL.Query().Get("press"), seen)
		if t := row.card(k).Task; t.Kind == "human" && t.PRNumber != 0 {
			c.pinMerge(ctx, button(r), t, o.Ask.Answers)
		}
		o.Ask.whoMay()
		if row.Owner != nil {
			o.Ask.Note = helperNote(*row.Owner)
		}
		// a page behind the card's live ask says why its answers wait
		if live := cs.liveAsk[row.card(k).ID]; !requestPage(row.Case) && factoryd.AnsweredOnCard(row.Case.Kind) && live.ID > row.Case.ID {
			o.Ask.Note = "A newer ask on the card replaced this one."
			if live.Status != factoryd.CaseAtHuman {
				o.Ask.Note = "A newer ask on this card waits on the factory first."
			}
		}
		if row.Approval != nil {
			requestAsk(o.Ask, *row.Approval)
		}
		// a monitor's flag sits beside the card's run, so the card's own
		// actions stand beside its answers; a helper's page ends its step,
		// so of the card's own actions only its end stands beside it
		onCase = row.Case.Kind != factoryd.CaseMonitorFlag || row.Owner != nil
		helperPage = row.Owner != nil
		late := func(a factoryd.Answer) answerView {
			return c.answerOf(ctx, v, k, k.ID, a.Key(), a, f, func(a factoryd.Answer, key string) (*pressForm, func(string) string) {
				return casePress(v, k, *row, a, key)
			}, seen)
		}
		asks, more := lateAnswers(v, k, *row, f)
		for _, a := range asks {
			o.Ask.Answers = append(o.Ask.Answers, late(a))
		}
		for _, a := range more {
			o.More = append(o.More, late(a))
		}
		o.Brief.Evidence = c.evidenceOf(ctx, v, k, row.Face)
		if k.Reason != "" {
			o.Brief.Evidence = append(o.Brief.Evidence, evidence{Label: "The raw text, for copying", Raw: sanitize.Outbound(scrubText(k.Reason), v.policy.Document.LinkHosts)})
		}
	case casesRead && k.Status == "waiting-human" && len(o.Brief.Approvals) == 0:
		o.Sent = true
	}
	// a merge's, a plan's and a criteria request ask through their cases
	// alone; a resource request opens no case and asks by itself
	asking := slices.DeleteFunc(slices.Clone(o.Brief.Approvals), func(ap approvalCard) bool { return waitsForFace(ap.ApprovalRequest) })
	if o.Ask == nil && !o.Sent && len(asking) > 0 {
		pending := make([]factoryd.ApprovalRequest, len(asking))
		for i, ap := range asking {
			pending[i] = ap.ApprovalRequest
		}
		// a resource request opens no case of its own: it leads the card's
		// asks, as the first page of its cases (pagesOf) or of its requests
		lead := max(slices.IndexFunc(asking, func(ap approvalCard) bool { return ap.Kind == factoryd.ApprovalResource }), 0)
		ap := asking[lead]
		page := factoryd.CardPages(bt, nil, nil, pending, func(a factoryd.ApprovalRequest) factoryd.Request {
			return asking[slices.IndexFunc(asking, func(ap approvalCard) bool { return ap.ID == a.ID })].Request
		})[lead]
		kase, face := page.Case, page.Face
		press := func(an factoryd.Answer, key string) (*pressForm, func(string) string) {
			if approverOnly(v, k, ap, an) {
				o.Ask.denied = append(o.Ask.denied, an.Label)
			}
			return requestPress(v, k, ap, an, key)
		}
		ask := k.ID + "-r" + strconv.FormatInt(ap.ID, 10)
		face = v.scrubbed(face)
		o.Ask = c.askOf(ctx, v, k, &o.Face, face, seen)
		o.Ask.Position, o.Ask.Of = 1, 1
		f := factoryd.Facts{CaseFacts: factoryd.FactsOf(kase), Card: bt.Task, Case: kase}
		o.Ask.Answers = c.answersOf(ctx, v, k, ask, face, f, press, r.URL.Query().Get("press"), seen)
		o.Ask.whoMay()
		o.Brief.Evidence = c.evidenceOf(ctx, v, k, face)
	}
	for _, a := range factoryd.MoreOf(bt.Task, facts) {
		tool := slices.Contains([]string{factoryd.AnswerSteer, factoryd.AnswerEditBrief, factoryd.AnswerAccent, factoryd.AnswerThread,
			factoryd.AnswerHold, factoryd.AnswerRelease}, a.Action)
		switch {
		case onCase && !tool && !(helperPage && a.Action == factoryd.ActClose), !c.mayPress(v, k, a):
		case slices.ContainsFunc(o.More, func(m answerView) bool { return m.Label == a.Label }),
			o.Ask != nil && slices.ContainsFunc(o.Ask.Answers, func(m answerView) bool { return m.Label == a.Label }):
		default:
			o.More = append(o.More, c.answerOf(ctx, v, k, k.ID, a.Key(), a, facts, func(a factoryd.Answer, key string) (*pressForm, func(string) string) {
				return cardPress(v, k, a, key)
			}, seen))
		}
	}

	if bt.Source == factoryd.SourceRoutine && c.cfg.Routines != nil {
		if o.Life.Routine, err = c.routineLineOf(ctx, bt.ID); err != nil {
			problem("routine", "Reload the board.", err)
		}
	}
	brief, err := factoryd.ReadCardBrief(ctx, c.cfg.Tasks, bt.ID)
	if err != nil {
		problem("brief", "Reload the board.", err)
	} else {
		if bt.Kind != "human" {
			o.AgentView = brief.AgentText()
		}
		// the life fold names the routine, so the body drops its stamp
		body := brief.Text.Body
		if i := strings.LastIndex(body, "\n"); o.Life.Routine != nil && strings.HasPrefix(body[i+1:], factoryd.RoutineProvenance) {
			body = strings.TrimSpace(body[:max(i, 0)])
		}
		o.Brief.Body = c.linkMarkup(ctx, v, k.Repository, body)
		for _, it := range brief.Acceptance {
			o.Brief.Acceptance = append(o.Brief.Acceptance, it.Text)
		}
		for _, cr := range brief.Criteria {
			o.Brief.Criteria = append(o.Brief.Criteria, cmp.Or(cr.Claim, cr.Suite))
		}
		for _, d := range brief.DependsOn {
			if d.State == "done" {
				continue
			}
			o.Brief.DependsOn = append(o.Brief.DependsOn, c.linkRefs(ctx, v, k.Repository, fmt.Sprintf("#%d", d.Issue)))
		}
		o.Plan = c.planFoldOf(ctx, v, k, brief.Plan)
	}
	if bt.Source == factoryd.SourceRoll {
		carried, err := c.cfg.Tasks.RollCarries(ctx, bt.ID)
		if err != nil {
			problem("cards the roll carries", "Reload the board.", err)
		}
		for _, ct := range carried {
			o.Brief.Carries = append(o.Brief.Carries, c.linkRefs(ctx, v, k.Repository, fmt.Sprintf("#%d", ct.IssueNumber)))
		}
	}
	if bt.Track == policy.TrackPlan {
		p, err := factoryd.ReadPlanBrief(ctx, c.cfg.Tasks, bt.Task)
		if err != nil {
			problem("plan", "Open the card again, or read the plan on its issue.", err)
		}
		if o.Plan = c.planFoldOf(ctx, v, k, p); o.Plan == nil {
			o.Plan = &planFold{Name: cmp.Or(bt.PlanName, factoryd.CardTitle(bt.Task)), Hint: "no revision stored"}
		}
		o.Plan.Whole = ""
	}
	o.Brief.Hint = briefHint(o.Brief)

	o.Life.Runs = c.lifeOf(ctx, bt.Task, steps)
	o.Life.Line = lifeLine(o.Life.Runs, bt.SpentEUR)
	o.Life.Thread = c.cardThread(ctx, bt.Task, current(r).user)
	o.Life.GitHub, o.Life.GitHubURL, o.Life.GitHubPR = k.GitHubPRState, k.GitHubPRURL, k.Task.GitHubPR
	o.Life.Spent, o.Life.Budget, o.Life.Near, o.Life.Calls, o.Life.Model = k.EnforcedSpent(), k.BudgetEUR, k.NearBudget, k.CallsText, k.ModelAlias
	if k.PRURL != "" {
		o.Life.Diff = k.PRURL + ".diff"
	}

	if c.cfg.SessionBriefs != nil {
		rows, err := c.cfg.SessionBriefs(ctx, bt.ID)
		if err != nil {
			problem("sessions' texts", "Reload the board.", err)
		}
		ran := sessionsOf(steps)
		for _, s := range rows {
			f := sessionFold{N: s.Session, Text: s.Brief}
			if i := slices.IndexFunc(ran, func(x sessionRef) bool { return x.N == s.Session }); i >= 0 {
				f.Outcome = ran[i].Outcome
			}
			o.Sessions = append(o.Sessions, f)
		}
	}
	o.Glosses = glosses(seen)
	o.SessionsHint, o.MoreHint = sessionsHint(k, o.Sessions), moreHint(o.More)
	return o
}

// holds says card t holds a case at the human that its row of Needs you
// shows (factoryd.ShowsCases, through the cases cs read for it). The
// board, a card's swap and its opened card read it alike.
func (cs caseSet) holds(t factoryd.Task) bool { return len(cs.byCard[t.ID]) > 0 }

// askOf is the ask block of face on card k: the question and its why in
// the factory's words with their terms marked into seen, and the story,
// whose writer's words stand quoted. A planned line that only names the
// card's title, which the opened card's head shows, stays out. A question
// that is the card's title and a why that is the card's own stand once, in
// head, as the factory's words with their terms marked.
func (c *Console) askOf(ctx context.Context, v *boardView, k card, head *faceView, face factoryd.Face, seen map[string]int) *askBlock {
	a := &askBlock{}
	bare := func(t string) string { return strings.TrimRight(strings.TrimSpace(t), ".?!") }
	if bare(face.Ask) == bare(k.Title) {
		head.title(c.factoryText(ctx, v, k.Repository, k.Title, seen), k.Title)
	} else {
		a.Question = c.factoryText(ctx, v, k.Repository, face.Ask, seen)
	}
	if bare(face.Why) == bare(k.Why) {
		head.Why = c.factoryText(ctx, v, k.Repository, k.Why, seen)
	} else {
		a.Why = c.factoryText(ctx, v, k.Repository, face.Why, seen)
	}
	planned := face.Story.Planned
	if factoryd.TitleLine(planned, k.Task) {
		planned = ""
	}
	for _, line := range []string{planned, face.Story.Happened, face.Story.Left, face.Story.View} {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if lead, words, ok := factoryd.QuotedLine(line); ok {
			if utf8.RuneCountInString(words) > factoryd.StoryMax {
				writer, _, view := strings.Cut(lead, "'s view: ")
				if !view {
					writer = "The agent"
				}
				a.Story = append(a.Story, storyLine{Lead: c.factoryText(ctx, v, k.Repository, writer+"'s own words stand whole in the fold below.", seen)})
				a.Words = append(a.Words, ownWords{Title: writer + "'s own words", Hint: fmt.Sprintf("%d words", len(strings.Fields(words))), Text: words})
				continue
			}
			a.Story = append(a.Story, storyLine{Lead: c.factoryText(ctx, v, k.Repository, lead, seen), Quote: words})
			continue
		}
		a.Story = append(a.Story, storyLine{Lead: c.factoryText(ctx, v, k.Repository, line, seen)})
	}
	return a
}

// presser is the form that sends answer a by key, and the name each of its
// fields posts under; nil where the card cannot take it.
type presser func(a factoryd.Answer, key string) (*pressForm, func(field string) string)

// lateAnswers are the answers case row holds now on facts f that its stored
// face lacks and the viewer of v may take: asks, the answers of the ask,
// which stand in the ask block after the stored ones, and more, the card's
// other actions. A request's page holds only the answers its request takes.
func lateAnswers(v *boardView, k card, row caseRow, f factoryd.Facts) (asks, more []factoryd.Answer) {
	live := factoryd.FaceOf(row.Case, f)
	own := len(live.Answers) + len(live.Over)
	for i, a := range slices.Concat(live.Answers, live.Over, live.More) {
		same := func(o factoryd.Answer) bool { return o.Label == a.Label }
		if !mayAnswer(v, k, a.Action) || slices.ContainsFunc(row.Face.Offers, func(o factoryd.AskOffer) bool { return o.Value == a.Key() || o.Label == a.Label }) ||
			slices.ContainsFunc(asks, same) || slices.ContainsFunc(more, same) {
			continue
		}
		if p, _ := casePress(v, k, row, a, a.Key()); row.Approval != nil && p == nil && a.Action != factoryd.AnswerThread {
			continue
		}
		if i < own {
			asks = append(asks, a)
		} else {
			more = append(more, a)
		}
	}
	return asks, more
}

// answersOf are the answers of face on card k on facts f, the suggested
// one first, each with the press that sends it; pressed is the answer
// whose fields stand open, the one the small card's pick opened the card
// with. ask names the ask in its fields' ids, so a swap to another ask
// keeps none of this one's words.
func (c *Console) answersOf(ctx context.Context, v *boardView, k card, ask string, face factoryd.Face, f factoryd.Facts, press presser, pressed string, seen map[string]int) []answerView {
	var out []answerView
	for _, o := range face.Offers {
		a, ok := factoryd.AnswerFor(o.Value, f)
		if !ok {
			a, ok = factoryd.AnswerOf(o.Value)
		}
		if !ok {
			a = factoryd.Answer{}
		}
		a.Label, a.Effect = o.Label, o.Effect
		view := c.answerOf(ctx, v, k, ask, o.Value, a, f, press, seen)
		view.Open = pressed != "" && pressed == o.Value && len(view.Fields) > 0
		if face.Preselected != "" && o.Value == face.Preselected {
			view.Suggested, view.By, view.Grounds = true, face.Source, clause(face.Recommendation)
			out = slices.Insert(out, 0, view)
			continue
		}
		out = append(out, view)
	}
	return out
}

// answerOf is answer a by key of card k on facts f as its button shows it: its
// label, its effect line in the factory's words, and the fields its press
// opens with their defaults, named as its press posts them and with ids
// under scope. A room's fields open on the card's room facts, the facts
// Act weighs its press on, with the readout of their defaults.
func (c *Console) answerOf(ctx context.Context, v *boardView, k card, scope, key string, a factoryd.Answer, f factoryd.Facts, press presser, seen map[string]int) answerView {
	// a writer's words reach a label, an effect and a field: each shows
	// masked, as the face's own texts do
	clean := func(s string) string { return sanitize.Outbound(scrubText(s), v.policy.Document.LinkHosts) }
	view := answerView{Action: clean(key), Label: clean(a.Label), Effect: c.factoryText(ctx, v, k.Repository, clean(a.Effect), seen)}
	if a.Action == factoryd.AnswerThread {
		view.Thread = k.ID
		return view
	}
	p, name := press(a, key)
	if p == nil {
		return view
	}
	view.Press = p
	defaults := a.Defaults(f)
	id := "f-" + fieldID.ReplaceAllString(scope+"-"+view.Action, "-")
	if a.Action == factoryd.ActExtend {
		f.CaseFacts = c.roomFactsOf(ctx, v, f.Card)
		defaults = a.Defaults(f)
		args, _, err := factoryd.ArgsOf(a, func(field string) string { return defaults[field] })
		room := readoutOf(f.CaseFacts, args, err, name)
		room.ID, room.Get, room.Params = id+"-room", "/tasks/"+f.Card.ID+"/room", name("sessions")+","+name("money")
		view.Room = &room
	}
	bound := factoryd.MaxAnswer
	if a.Action == factoryd.ActReplan {
		bound = factoryd.MaxReplanComment
	}
	for _, field := range a.Fields {
		if field.Kind == factoryd.FieldFixed {
			p.Values = append(p.Values, hidden{name(field.Name), defaults[field.Name]})
			continue
		}
		fv := fieldView{ID: id + "-" + field.Name, Name: name(field.Name), Label: field.Label, Value: clean(defaults[field.Name]), Required: field.Required,
			Max: cmp.Or(p.Max, bound)}
		if field.Required && fv.Value == "" {
			fv.Hint = field.Hint
		}
		switch field.Kind {
		case factoryd.FieldNumber:
			fv.Kind = "number"
		case factoryd.FieldMoney:
			fv.Kind = "money"
		case factoryd.FieldChoice:
			fv.Kind = "choice"
		case factoryd.FieldCheck:
			fv.Kind = "check"
			if len(field.Options) > 0 {
				fv.Kind = "checks"
			}
		default:
			fv.Kind = "text"
			if field.Name == "note" || field.Name == "summary" || field.Name == "body" {
				fv.Kind = "note"
			}
		}
		for _, op := range field.Options {
			fv.Options = append(fv.Options, optionView{Value: op.Value, Label: clean(cmp.Or(op.Label, op.Value)), Checked: op.Value == defaults[field.Name]})
		}
		if fv.Value != "" && slices.Contains([]string{"text", "note", "number", "money"}, fv.Kind) {
			// a typed field's id moves with its default, so a swap keeps
			// what the human typed (hx-preserve) only while the default
			// stands
			h := fnv.New32a()
			h.Write([]byte(fv.Value)) // #nosec G104 -- a hash's Write never fails
			fv.ID += fmt.Sprintf("-%08x", h.Sum32())
		}
		view.Fields = append(view.Fields, fv)
	}
	return view
}

var fieldID = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

// dataTo is where card k stands once answer a is taken: a card's end out
// of the board's live columns, a note, one promise's answer or a run of a
// plan's tests where it stands, a decided request in its status's column,
// any other answer in progress.
func dataTo(k card, a factoryd.Answer) string {
	switch a.Action {
	case factoryd.ActNote, factoryd.ActSettle, factoryd.AnswerEditBrief, factoryd.AnswerAccent, factoryd.AnswerSteer, factoryd.ActAnswerCriteria, factoryd.ActRunCriteria:
		return ""
	case factoryd.ActApproveMerge, factoryd.ActDenyMerge, factoryd.AnswerApprove, factoryd.AnswerDeny:
		return k.Status
	case factoryd.ActClose, factoryd.AnswerDropAndEnd, factoryd.AnswerClosePlan:
		if k.Kind == "human" {
			return "off"
		}
		return "closed"
	}
	if k.Kind == "human" {
		return "off"
	}
	return "running"
}

// casePress is the press of answer a by key on case row of card k: a plan
// question's or confirmation's to the plan, a human card's choice to its
// answer, any other answer to the case's resolve; nil on a page an answer
// on the card does not take.
func casePress(v *boardView, k card, row caseRow, a factoryd.Answer, key string) (*pressForm, func(string) string) {
	if owner := row.Owner; owner != nil {
		// a helper's answer acts on the helper, and its parent's card
		// stays where it is until the board reads it again
		row.Owner = nil
		p, name := casePress(v, *owner, row, a, key)
		if p != nil {
			p.DataTo = ""
		}
		return p, name
	}
	if requestPage(row.Case) {
		if row.Approval == nil {
			return nil, nil
		}
		return requestPress(v, k, *row.Approval, a, key)
	}
	if a.Action == factoryd.ActRerunChecks && row.Approval != nil && !row.Approval.Rerun() {
		// only a factory pull request's red checks run again from its merge ask
		return nil, nil
	}
	p := &pressForm{CSRF: v.csrf, Filter: k.Filter, DataTo: dataTo(k, a)}
	named := func(note string) func(string) string {
		return func(field string) string {
			if field == "note" {
				return note
			}
			return key + "." + field
		}
	}
	if factoryd.PlanCase(row.Case) && a.Action == factoryd.ActClose {
		// the plan's own end, no answer of its question
		return cardPress(v, k, a, key)
	}
	if a.Action == factoryd.AnswerDropAndEnd || a.Action == factoryd.AnswerClosePlan {
		return planClosePress(p, k)
	}
	if h, ok := factoryd.HoldoutOn(row.Case); ok {
		// an extra promise's page keeps it or drops it, and the card waits
		verb := map[string]string{factoryd.AnswerKeepHoldout: "okay", factoryd.AnswerDropHoldout: "drop"}[a.Action]
		if verb == "" {
			return nil, nil
		}
		p.Post, p.DataTo = "/tasks/"+k.ID+"/plan/holdout", ""
		p.Values = []hidden{{"revision", strconv.Itoa(factoryd.FactInt(row.Case, "revision"))}, {"index", strconv.Itoa(h.Idx)}, {"verb", verb}}
		return p, named("note")
	}
	switch {
	case row.Plan != nil && a.Action == factoryd.AnswerRevisePlan && row.Case.Kind == factoryd.CasePlanQuestion:
		// a question's page sends the whole plan back with the human's words
		p.Post = "/tasks/" + k.ID + "/plan/changes"
		return p, named("text")
	case row.Plan != nil && a.Action != factoryd.ActNote:
		p.Post, p.Max = "/tasks/"+k.ID+"/plan/answer", maxPlanAnswer
		p.Values = []hidden{{"revision", strconv.Itoa(row.Plan.Revision)}, {"index", strconv.Itoa(row.Plan.Index)}}
		switch {
		case a.Action == factoryd.AnswerConfirmPlan:
			p.Values = append(p.Values, hidden{"decision", "confirm"})
		case a.Action == factoryd.AnswerRevisePlan:
			p.Values = append(p.Values, hidden{"decision", "revise"})
			return p, named("answer")
		case a.Action == factoryd.AnswerPlanChoice && len(a.Fields) > 0:
			return p, named("own")
		case a.Action == factoryd.AnswerPlanChoice:
			j := slices.IndexFunc(row.Plan.Choices, func(ch planChoice) bool { return ch.Label == a.Label })
			if j < 0 {
				// the planner's pick among no choices of its question goes
				// as its words
				p.Values = append(p.Values, hidden{"own", a.Label})
				break
			}
			p.Values = append(p.Values, hidden{"choice", strconv.Itoa(row.Plan.Choices[j].Index)})
		default:
			return nil, nil
		}
		return p, named("note")
	case factoryd.ChoiceKind(row.Case.Kind) && a.Value == "" && a.Action == factoryd.ActClose:
		// the card's own end, no choice of its question
		return cardPress(v, k, a, key)
	case factoryd.ChoiceKind(row.Case.Kind):
		if !row.Answers {
			return nil, nil
		}
		// a choice posts its value; the human's own words go alone, a
		// comment on the issue
		p.Post = "/tasks/" + k.ID + "/answer"
		p.Values = []hidden{{"case", strconv.FormatInt(row.Case.ID, 10)}}
		if a.Value != "" {
			p.Values = append(p.Values, hidden{"value", a.Value})
		}
		if slices.Contains([]string{factoryd.AnswerPark, factoryd.AnswerKeepWaiting, factoryd.AnswerRerunAgain}, a.Value) {
			p.DataTo = ""
		}
		return p, named("answer")
	case factoryd.AnsweredOnCard(row.Case.Kind) && !row.Answers:
		return nil, nil
	}
	p.Post = "/tasks/" + k.ID + "/resolve"
	p.Values = []hidden{{"case_id", strconv.FormatInt(row.Case.ID, 10)}, {"action", key}}
	return p, named(key + ".note")
}

// pinMerge names on human card t's Merge it the head of its pull request,
// read as a, and has the press post that head, so the merge takes the head
// the human read. A head that cannot be read offers no Merge it.
func (c *Console) pinMerge(ctx context.Context, a actor, t factoryd.Task, answers []answerView) {
	key := factoryd.Answer{Value: factoryd.ActApproveMerge}.Key()
	i := slices.IndexFunc(answers, func(v answerView) bool { return v.Action == key && v.Press != nil })
	if i < 0 {
		return
	}
	repo, err := factoryd.SplitRepo(t.Repository)
	head := ""
	if err == nil {
		head, err = c.pullHead(ctx, a.s, repo, t.PRNumber)
	}
	if err != nil {
		log.Printf("console: the head of %s#%d: %v", t.Repository, t.PRNumber, err)
		answers[i].Press = nil
		answers[i].Effect = template.HTML(fmt.Sprintf("The head of pull request #%d could not be read, so it does not merge from here. Reload the card.", t.PRNumber)) // #nosec G203 -- a constant sentence and a number
		return
	}
	answers[i].Effect = template.HTML(template.HTMLEscapeString(factoryd.HumanMergeEffect(head))) // #nosec G203 -- the effect line is escaped
	answers[i].Press.Values = append(answers[i].Press.Values, hidden{"head", head})
}

// requestAsk fits the ask of merge request ap to its rows as they read
// now: Merge it waits on its head's checks and on each human promise's
// verdict, which the pages before it take.
func requestAsk(ask *askBlock, ap approvalCard) {
	if ap.Kind == factoryd.ApprovalMerge && (len(ap.Unsettled) > 0 || ap.HeadUnread) {
		for i := range ask.Answers {
			if ask.Answers[i].Action == factoryd.ActApproveMerge {
				ask.Answers[i].Press, ask.Answers[i].Fields = nil, nil
				switch n := len(ap.Unsettled); n {
				case 0:
				case 1:
					ask.Note = "Merge it opens once the promise on the page before this one has a verdict."
				default:
					ask.Note = fmt.Sprintf("Merge it opens once the %d promises on the pages before this one have a verdict.", n)
				}
			}
		}
	}
}

// requestPress is the press of answer a by key on approval request ap of
// card k, a resource request's decision, which no case holds; nil for an
// answer the request does not take, or one only an approver may press that
// the viewer may not.
func requestPress(v *boardView, k card, ap approvalCard, a factoryd.Answer, key string) (*pressForm, func(string) string) {
	if approverOnly(v, k, ap, a) {
		return nil, nil
	}
	return requestForm(v, k, ap, a)
}

// approverOnly says answer a of request ap posts to a door only approvers
// pass, and the viewer is none.
func approverOnly(v *boardView, k card, ap approvalCard, a factoryd.Answer) bool {
	p, _ := requestForm(v, k, ap, a)
	return p != nil && !ap.Approver && accessTable["POST /approvals/{id}/"+path.Base(p.Post)].need&approver != 0
}

func requestForm(v *boardView, k card, ap approvalCard, a factoryd.Answer) (*pressForm, func(string) string) {
	p := &pressForm{CSRF: v.csrf, Filter: k.Filter, DataTo: dataTo(k, a)}
	base := fmt.Sprintf("/approvals/%d/", ap.ID)
	named := func(note string) func(string) string {
		return func(field string) string {
			if field == "note" {
				return note
			}
			return field
		}
	}
	switch {
	case a.Action == factoryd.AnswerApprove:
		p.Post, p.Values = base+"approve", []hidden{{"action", "approve"}}
	case a.Action == factoryd.AnswerDeny:
		p.Post = base + "deny"
	default:
		return nil, nil
	}
	return p, named("note")
}

// planClosePress is p as the plan's own Close of card k, whose note
// waives each open promise: the card's resolve with no case.
func planClosePress(p *pressForm, k card) (*pressForm, func(string) string) {
	p.Post, p.Values, p.DataTo = "/tasks/"+k.ID+"/resolve", []hidden{{"action", factoryd.ActClose}}, "closed"
	return p, func(field string) string { return factoryd.ActClose + "." + field }
}

// cardPress is the press of card k's own action a by key, which no case
// holds: the card's resolve with no case. Only its end moves the card.
func cardPress(v *boardView, k card, a factoryd.Answer, key string) (*pressForm, func(string) string) {
	p := &pressForm{CSRF: v.csrf, Filter: k.Filter, Post: "/tasks/" + k.ID + "/resolve", Values: []hidden{{"action", key}}}
	if a.Action == factoryd.ActClose {
		p.DataTo = dataTo(k, a)
	}
	return p, func(field string) string { return key + "." + field }
}

// mayAnswer says the viewer of v may give action on card k: an action
// approverRules names is an approver's.
func mayAnswer(v *boardView, k card, action string) bool {
	rule, ok := approverRules[action]
	return !ok || rule.need&approver == 0 || v.approves(k.Repository)
}

// mayPress says the viewer of v may press card k's own action a: a
// replan, more room and a retry are an approver's where the card takes
// them, a word to the session needs one to take it, a thread the chat.
func (c *Console) mayPress(v *boardView, k card, a factoryd.Answer) bool {
	switch a.Action {
	case factoryd.ActReplan:
		return k.MayReplan
	case factoryd.ActExtend:
		return k.MayExtend
	case factoryd.ActRetry:
		return k.MayRetry
	case factoryd.AnswerSteer:
		return c.cfg.Steer != nil
	case factoryd.AnswerThread:
		return c.cfg.Chat != nil
	}
	return true
}

// evidenceOf is the evidence of an ask on card k: the face's info, the
// findings its ask and story count, and the grounds of its suggestion.
func (c *Console) evidenceOf(ctx context.Context, v *boardView, k card, face factoryd.Face) []evidence {
	var out []evidence
	if face.Info != "" {
		out = append(out, evidence{Label: "What the factory knows", Text: c.linkMarkup(ctx, v, k.Repository, face.Info)})
	}
	if n := len(face.Findings); n > 0 {
		var b strings.Builder
		for _, f := range face.Findings {
			b.WriteString(`<li class="quote">` + string(c.linkRefs(ctx, v, k.Repository, f)) + "</li>")
		}
		out = append(out, evidence{Label: fmt.Sprintf("The %d %s", n, plural(n, "open finding")), Text: template.HTML("<ul>" + b.String() + "</ul>"), Findings: n}) // #nosec G203 -- each item is linkRefs's escaped HTML
	}
	if face.Recommendation != "" && face.Source != "" && clause(face.Recommendation) != face.Recommendation {
		out = append(out, evidence{Label: "Why " + face.Source + " suggests it", Quote: face.Recommendation})
	}
	return out
}

// clause is the first sentence of a writer's grounds, the one clause the
// suggested answer names; "" when even that runs long, so the full brief
// holds them alone.
func clause(grounds string) string {
	grounds = strings.Join(strings.Fields(grounds), " ")
	first := grounds
	if i := strings.Index(grounds, ". "); i >= 0 {
		first = grounds[:i+1]
	}
	if utf8.RuneCountInString(first) > 160 {
		return ""
	}
	return first
}

// planFoldOf is plan p as card k's fold shows it, nil for none.
func (c *Console) planFoldOf(ctx context.Context, v *boardView, k card, p *factoryd.BriefPlan) *planFold {
	if p == nil {
		return nil
	}
	clean := func(s string) string { return sanitize.Outbound(scrubText(s), v.policy.Document.LinkHosts) }
	f := &planFold{Name: clean(p.Name), Outcome: clean(p.Outcome), Whole: "#card=" + p.Task}
	ended := 0
	for i, ref := range p.Cards {
		row := planRow{Issue: ref.Issue, Title: template.HTML(template.HTMLEscapeString(clean(ref.Title))), State: ref.State, This: i+1 == p.Index && p.Index > 0} // #nosec G203 -- the title is escaped
		if ref.Issue > 0 {
			row.Title = c.linkRefs(ctx, v, k.Repository, fmt.Sprintf("#%d", ref.Issue))
		}
		if factoryd.Final(ref.State) || ref.State == "merged" {
			ended++
		}
		f.Cards = append(f.Cards, row)
	}
	if p.Section != "" {
		f.Section = c.linkRefs(ctx, v, k.Repository, "[the part of the plan's document this card builds]("+p.Section+")")
	}
	f.Hint = fmt.Sprintf("%s, %d %s, %d ended", f.Name, len(p.Cards), plural(len(p.Cards), "card"), ended)
	return f
}

// requestHints are what the full brief holds of each kind of request.
var requestHints = map[string]string{factoryd.ApprovalMerge: "the pull request", factoryd.ApprovalResource: "the server",
	factoryd.ApprovalPlan: "the plan", factoryd.ApprovalCriteria: "the promises", factoryd.ApprovalCriteriaRerun: "the promises"}

// briefHint is the full brief's hint: what it holds, counted.
func briefHint(b briefFold) string {
	var parts []string
	for _, ap := range b.Approvals {
		if h := requestHints[ap.Kind]; h != "" && !slices.Contains(parts, h) {
			parts = append(parts, h)
		}
	}
	if n := len(b.Acceptance); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "promise")))
	}
	if n := len(b.DependsOn); n > 0 {
		parts = append(parts, fmt.Sprintf("waits on %d %s", n, plural(n, "card")))
	}
	if n := len(b.Carries); n > 0 {
		parts = append(parts, fmt.Sprintf("carries %d %s", n, plural(n, "card")))
	}
	for _, e := range b.Evidence {
		if e.Findings > 0 {
			parts = append(parts, fmt.Sprintf("%d open %s", e.Findings, plural(e.Findings, "finding")))
		}
	}
	if len(b.Evidence) > 0 {
		parts = append(parts, "the evidence")
	}
	return strings.Join(parts, ", ")
}

// moreHint is the hint of the other actions: the first label and the
// count of the rest.
func moreHint(more []answerView) string {
	switch len(more) {
	case 0:
		return "none"
	case 1:
		return more[0].Label
	}
	return fmt.Sprintf("%s, and %d more", more[0].Label, len(more)-1)
}

// sessionsHint is the hint of what each session of card k got: a human's
// card runs none, and an ended card no more.
func sessionsHint(k card, s []sessionFold) string {
	switch {
	case len(s) == 0 && (k.Kind == "human" || factoryd.Final(k.Status)):
		return "no session"
	case len(s) == 0:
		return "no session yet"
	}
	return fmt.Sprintf("%d %s", len(s), plural(len(s), "session"))
}
