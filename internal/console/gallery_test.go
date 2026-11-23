package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"golang.org/x/net/html"
)

// galleryEntry is one face of the gallery: its group and its kind, and
// the seat that puts it on a fresh rig and answers what the gallery opens.
type galleryEntry struct {
	Group, Kind string
	Seat        func(*rig) string
}

// galleryBoard is the group of the board's own items no fixture holds:
// a card with three cases, the plain cards of Needs you, the routine's
// card and the status strip's actions.
const galleryBoard = "board"

// The targets a seat answers besides a card's task id: a routine's ghost
// and its drawer, "routine:<project>/<name>", and the status strip.
const (
	galleryRoutine = "routine:"
	galleryStatus  = "status"
)

// galleryEntries are the gallery's faces: every fixture of facefixtures,
// then the board's own items.
func galleryEntries() []galleryEntry {
	all := facefixtures.All()
	var out []galleryEntry
	for _, fx := range all {
		out = append(out, galleryEntry{fx.Group, fx.Kind, func(r *rig) string { return r.seedScene(fixtureScene(fx, all)) }})
	}
	return append(append(out, boardEntries(all)...), inboxEntries(all)...)
}

// galleryInbox is the group of the inbox's rows, one entry per birth kind
// the factory writes; its seat answers galleryInbox and the inbox's number.
const galleryInbox = "inbox"

// inboxEntries are the inbox's rows of every birth kind, as the factory
// writes them on the gallery's cards and as the panel renders them; a
// note to the agent comes with the agent's reply.
func inboxEntries(all []facefixtures.Fixture) []galleryEntry {
	fixture := func(group, kind string) facefixtures.Fixture {
		i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool { return fx.Group == group && fx.Kind == kind })
		if i < 0 {
			panic("gallery: no fixture " + group + "/" + kind)
		}
		return all[i]
	}
	capped, merge, plan := fixture(facefixtures.GroupCase, string(factoryd.CaseSessionCap)), fixture(facefixtures.GroupApproval, factoryd.ApprovalMerge),
		fixture(facefixtures.GroupApproval, factoryd.ApprovalPlan)
	card := capped.Task
	ref := "#" + strconv.FormatInt(card.IssueNumber, 10)
	poke := func(e factoryd.Event, t factoryd.Task) factoryd.ChatMessage {
		e.TaskID, e.Repository = t.ID, t.Repository
		m, ok := factoryd.PokeOf(e, t, factoryd.Task{})
		if !ok {
			panic("gallery: no poke of " + e.Kind)
		}
		return m
	}
	note := func(birth, text string, reply ...factoryd.ChatMessage) []factoryd.ChatMessage {
		return append([]factoryd.ChatMessage{{Role: "user", BirthKind: birth, Content: text}}, reply...)
	}
	said := func(text string) factoryd.ChatMessage { return factoryd.ChatMessage{Role: "assistant", Content: text} }
	agent := map[string]any{"autonomy": "assisted", "decider": "agent"}
	merged := card
	merged.Status = "merged"
	statement := strings.Repeat("The criteria gate ran twice on the same head, and the review raised the same two findings each time. ", 40)
	rows := map[string][]factoryd.ChatMessage{
		factoryd.EventQuestionAsked: {poke(factoryd.Event{Kind: factoryd.EventQuestionAsked, Detail: capped.Ask.Detail}, card),
			poke(factoryd.Event{Kind: factoryd.EventQuestionAsked, Detail: merge.Ask.Detail}, merge.Task)},
		factoryd.EventQuestionWithdrawn: {poke(factoryd.Event{Kind: factoryd.EventQuestionWithdrawn, Detail: map[string]any{"reason": "the card ended"}}, card)},
		factoryd.EventTaskEnded: {poke(factoryd.Event{Kind: factoryd.EventTaskEnded, Detail: map[string]any{"state": "failed", "reason": "checks: the checks failed on c1."}}, card),
			poke(factoryd.Event{Kind: factoryd.EventTaskEnded, Detail: map[string]any{"state": "done"}}, card)},
		factoryd.EventAllChildrenDone:   {poke(factoryd.Event{Kind: factoryd.EventAllChildrenDone, Detail: map[string]any{"cards": float64(5)}}, plan.Task)},
		factoryd.EventAutonomyChanged:   {poke(factoryd.Event{Kind: factoryd.EventAutonomyChanged, Actor: "anna", Detail: map[string]any{"from": "manual", "to": "assisted"}}, factoryd.Task{})},
		factoryd.EventCardFiled:         {poke(factoryd.Event{Kind: factoryd.EventCardFiled, Detail: agent}, card)},
		factoryd.EventCardRetried:       {poke(factoryd.Event{Kind: factoryd.EventCardRetried, Detail: agent}, card)},
		factoryd.EventSessionGranted:    {poke(factoryd.Event{Kind: factoryd.EventSessionGranted, Detail: agent}, card)},
		factoryd.EventChatBudgetSpent:   {poke(factoryd.Event{Kind: factoryd.EventChatBudgetSpent, Detail: map[string]any{"budget_eur": 5.0, "spent_eur": 5.0}}, factoryd.Task{ID: "t-chat-code", Kind: factoryd.KindChat})},
		factoryd.EventPullRequestMerged: {poke(factoryd.Event{Kind: factoryd.EventPullRequestMerged, Detail: map[string]any{"chain": "failure", "repair": float64(2190)}}, merged)},
		factoryd.EventRoutineRan: {poke(factoryd.Event{Kind: factoryd.EventRoutineRan, Detail: map[string]any{"routine": "long-waits", "run": float64(3), "schedule": "every day at 08:00",
			"poke": "Cards that wait long for a human\n\n- " + ref + " “" + factoryd.CardTitle(card) + "” · code · since 09:12 · waits for: a human's answer"}}, factoryd.Task{})},
		factoryd.WakeAsk: note(factoryd.WakeAsk, "Wake: 1 ask waits for a human: "+ref+".\n"+ref+": The card used all 3 sessions. How should it go on?",
			said("The card waits for a human's call on its sessions. I left the ask on the card.")),
		factoryd.EventCaseClimbed: note(factoryd.EventCaseClimbed, "Wake: 1 case waits for the factory agent: "+ref+".\n"+ref+":\n"+statement,
			factoryd.ChatMessage{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{Name: "escalate", Arguments: `{"ask":"Raise the session cap?"}`, Result: "Filed the escalation as #2190 in Needs you."}}},
			said("The review repeats its findings, so I asked the human on a card.")),
		factoryd.WakeCard: note(factoryd.WakeCard, "The watched card "+ref+" moved: read the card and act on where it stands.",
			said("It moved to Needs you at its session cap. Nothing to do until a human answers.")),
		factoryd.WakeTimer: note(factoryd.WakeTimer, "Timer: check "+ref+"'s checks. Run the check it stood for, then set a new timer if the thing still needs watching.",
			said("The checks of "+ref+" are green now.")),
		factoryd.WakeRefill: note(factoryd.WakeRefill, "The queue of code is below 3: read docs/roadmap.md of the project's doc repository, file every item whose Priority is filled and whose number no card cites, then end.",
			said("No roadmap item is ripe.")),
	}
	var out []galleryEntry
	for _, birth := range factoryd.BirthKinds() {
		out = append(out, galleryEntry{galleryInbox, birth, func(r *rig) string {
			s := galleryScene()
			for _, fx := range []facefixtures.Fixture{capped, merge, plan} {
				s.seat(fx)
			}
			if birth == factoryd.EventChatBudgetSpent {
				// the chat stands stopped, so its row offers the fund
				now := time.Now()
				chat := factoryd.Task{ID: factoryd.ChatTaskID(labCodeProject.Name, now), Kind: factoryd.KindChat, Status: "running", Repository: facefixtures.Repository,
					BudgetEUR: 5, SpentEUR: 5, Source: factoryd.SourceChat, CreatedAt: now.Add(-10 * 24 * time.Hour)}
				s.tasks = append(s.tasks, factoryd.BoardTask{Task: chat})
				s.events = append(s.events, factoryd.Event{At: now.Add(-30 * time.Minute), TaskID: chat.ID, Kind: factoryd.EventChatBudgetSpent,
					Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"budget_eur": 5.0, "spent_eur": 5.0, "suggest_eur": 2.0}})
			}
			r.seedScene(s)
			// the inbox is read, never answered: no model call runs
			r.c.cfg.Operator = &factoryd.OperatorModel{}
			ctx := context.Background()
			inbox, err := r.chat.EnsureInbox(ctx, labCodeProject.Repository, labCodeProject.Login)
			if err != nil {
				panic(err)
			}
			for _, m := range rows[birth] {
				m.Conversation = inbox
				if _, err := r.chat.AddChatMessage(ctx, labCodeProject.Repository, labCodeProject.Login, m); err != nil {
					panic(err)
				}
			}
			return galleryInbox + ":" + strconv.FormatInt(inbox, 10)
		}})
	}
	return out
}

// galleryScene is an empty moment of the code project: no card, and no
// case a fake would make up for a waiting card.
func galleryScene() labScene {
	return labScene{spend: 61.40, chatSpend: 3.85, objects: map[string]string{}, cases: map[string][]factoryd.Case{}, asks: map[int64]factoryd.CaseAsk{},
		plans: map[string][]factoryd.PlanRevision{}, questions: map[string][]factoryd.PlanQuestion{}}
}

// fixtureScene is the code project holding fixture fx, and the plan a
// plan's card belongs to, so its chip and its drawer name the plan. A
// helper task stands on its parent, whose cards the scene shows.
func fixtureScene(fx facefixtures.Fixture, all []facefixtures.Fixture) labScene {
	s := galleryScene()
	s.drawer = fx.Task.ID
	s.seat(fx)
	if fx.Parent != nil {
		s.drawer = fx.Parent.ID
	}
	switch i := slices.IndexFunc(all, func(p facefixtures.Fixture) bool { return p.Task.ID == fx.Task.Parent && p.Plan != nil }); {
	case i >= 0:
		s.seat(all[i])
	case fx.Parent != nil:
		s.seat(facefixtures.Fixture{Task: *fx.Parent})
	}
	return s
}

// seat adds fixture fx to s as the store holds it today: the board's row
// of its card reads its live ask from its case and the case's newest ask
// row (BoardAsk), and a plan card's revision holds its questions.
func (s *labScene) seat(fx facefixtures.Fixture) {
	bt := factoryd.BoardTask{Task: fx.Task}
	if c := fx.Case; c != nil {
		var row map[string]any
		bt.EscalatedAt = c.OpenedAt
		if fx.Ask != nil {
			row, bt.EscalatedAt = fx.Ask.Detail, fx.Ask.At
			s.asks[c.ID] = *fx.Ask
		}
		bt.AskCase, bt.QuestionComment = c.ID, c.CommentID
		bt.Escalation, bt.Choices, bt.Question = factoryd.BoardAsk(*c, row)
		s.cases[fx.Task.ID] = append(s.cases[fx.Task.ID], *c)
	}
	if fx.Approval != nil {
		s.approvals = append(s.approvals, *fx.Approval)
	}
	// each session that ran got the card's brief as its agent reads it
	text := factoryd.CardBrief{Text: factoryd.BriefText{Title: factoryd.CardTitle(fx.Task), Why: fx.Task.Why, Summary: fx.Task.Summary}}.AgentText()
	for n := 1; n <= fx.Task.Usage.Attempts; n++ {
		if s.briefs == nil {
			s.briefs = map[string][]factoryd.SessionBrief{}
		}
		s.briefs[fx.Task.ID] = append(s.briefs[fx.Task.ID], factoryd.SessionBrief{Session: n, Brief: text, At: fx.Task.CreatedAt})
	}
	if p := fx.Plan; p != nil {
		revision := 0
		if len(p.Questions) > 0 {
			revision = 1
		}
		plan, err := json.Marshal(p)
		if err != nil {
			panic(err)
		}
		s.plans[fx.Task.ID] = []factoryd.PlanRevision{{TaskID: fx.Task.ID, Revision: revision, Plan: plan, SHA256: facefixtures.PlanSHA, CreatedAt: fx.Task.CreatedAt}}
		s.questions[fmt.Sprintf("%s %d", fx.Task.ID, revision)] = p.Questions
	}
	s.tasks = append(s.tasks, bt)
}

// boardEntries are the board's own items: a card holding three cases at
// the human (its run's ask and two of the monitor's flags beside it), a card closed into its successor, a plan question whose
// suggested choice is as long as a choice may be, a card queued at its
// project's month cap, a card behind another in the merge queue, a card
// waiting on its author, a card whose title and why line hold words
// longer than a line, the routine's card, and the status strip at each of
// its actions.
func boardEntries(all []facefixtures.Fixture) []galleryEntry {
	fixture := func(group, kind string) facefixtures.Fixture {
		i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool { return fx.Group == group && fx.Kind == kind })
		if i < 0 {
			panic("gallery: no fixture " + group + "/" + kind)
		}
		return all[i]
	}
	plain := func(t factoryd.Task) func(*rig) string {
		return func(r *rig) string {
			s := galleryScene()
			s.drawer, s.tasks = t.ID, []factoryd.BoardTask{{Task: t}}
			return r.seedScene(s)
		}
	}
	now := time.Now()
	return []galleryEntry{
		{galleryBoard, "three open cases", func(r *rig) string {
			fx := fixture(facefixtures.GroupCase, string(factoryd.CaseAgentEscalate))
			s := fixtureScene(fx, all)
			for i, flag := range []string{"wait_over_bound", "session_no_push"} {
				c := *fixture(facefixtures.GroupFlag, flag).Case
				c.ID, c.TaskID = fx.Case.ID+int64(i+1), fx.Task.ID
				s.cases[fx.Task.ID] = append(s.cases[fx.Task.ID], c)
				s.asks[c.ID] = factoryd.CaseAsk{At: c.OpenedAt, Detail: maps.Clone(c.Facts)}
			}
			return r.seedScene(s)
		}},
		{galleryBoard, "closed into its successor", func(r *rig) string {
			s := galleryScene()
			old := factoryd.Task{ID: facefixtures.TaskID(2403), Kind: "agent", Status: "failed", Track: policy.TrackDirect, Repository: facefixtures.Repository,
				IssueNumber: 2403, Source: factoryd.SourceWebhook, Author: "alice", CreatedAt: now.Add(-5 * time.Hour), FinishedAt: new(now.Add(-time.Hour)),
				Objective: "Export the board as CSV\n\nOne row per card, its title, state and spend."}
			succ := factoryd.Task{ID: factoryd.SuccessorID(old.ID), Kind: "agent", Status: "queued", Track: policy.TrackDirect, Repository: facefixtures.Repository,
				IssueNumber: 2404, Source: factoryd.SourceSuccessor, Author: "factory-agent", CreatedAt: now.Add(-time.Hour),
				Objective: "Export the board as CSV, carried on\n\nThe export's findings so far, carried on."}
			s.drawer, s.tasks = old.ID, []factoryd.BoardTask{{Task: old}, {Task: succ}}
			s.events = []factoryd.Event{{At: now.Add(-time.Hour), TaskID: old.ID, Kind: factoryd.EventTaskEnded, Actor: factoryd.FactoryDecider,
				Via: factoryd.ViaFactoryd, Detail: map[string]any{"final": "failed", "reason": "closed into successor " + succ.ID}}}
			return r.seedScene(s)
		}},
		{galleryBoard, "a long suggested choice", func(r *rig) string {
			fx := fixture(facefixtures.GroupCase, string(factoryd.CasePlanQuestion))
			const long = "Page back to the newest 500 rows on quiet days and busy days"
			q := fx.Plan.Questions[0]
			q.Choices, q.Recommended = []string{"One day", "One week", long}, long
			p := *fx.Plan
			p.Questions = []factoryd.PlanQuestion{q}
			fx.Plan = &p
			row := roundTripped(factoryd.PlanQuestionRow(q, 1, 1))
			opened, err := factoryd.CaseFromAsk(fx.Task, factoryd.Event{TaskID: fx.Task.ID, Kind: factoryd.EventQuestionAsked, Detail: row}, "")
			if err != nil {
				panic(err)
			}
			k := *fx.Case
			k.Facts, k.Problem, k.StateKey = opened.Facts, opened.Problem, opened.StateKey
			fx.Case, fx.Ask = &k, &factoryd.CaseAsk{At: fx.Ask.At, Detail: row}
			return r.seedScene(fixtureScene(fx, all))
		}},
		{galleryBoard, "queued at the month cap", plain(factoryd.Task{ID: facefixtures.TaskID(2405), Kind: "agent", Status: "queued", Track: policy.TrackDirect,
			Repository: facefixtures.Repository, IssueNumber: 2405, Source: factoryd.SourceWebhook, Author: "alice", CreatedAt: now.Add(-3 * time.Hour),
			QueueReason: factoryd.QueueMonthCap, QueueSince: new(now.Add(-2 * time.Hour)),
			Objective: "Show the month's spend per project\n\nA line per project on the costs page.",
			Why:       "The costs page shows the factory's total, so a project's share stays hidden."})},
		{galleryBoard, "behind another card in the merge queue", func(r *rig) string {
			s := galleryScene()
			head := factoryd.Task{ID: facefixtures.TaskID(2406), Kind: "agent", Status: "in-review", Track: policy.TrackDirect, Repository: facefixtures.Repository,
				IssueNumber: 2406, Source: factoryd.SourceWebhook, Author: "alice", CreatedAt: now.Add(-6 * time.Hour), PRNumber: 2416,
				Gate: factoryd.GateMergeQueue, GateSince: new(now.Add(-2 * time.Hour)),
				Objective: "Settle every acceptance claim\n\nEach claim of the plan's acceptance card runs once."}
			behind := factoryd.Task{ID: facefixtures.TaskID(2407), Kind: "agent", Status: "in-review", Track: policy.TrackDirect, Repository: facefixtures.Repository,
				IssueNumber: 2407, Source: factoryd.SourceWebhook, Author: "alice", CreatedAt: now.Add(-4 * time.Hour), PRNumber: 2417,
				Gate: factoryd.GateMergeQueue, GateSince: new(now.Add(-time.Hour)),
				Objective: "Narrow the close refusal\n\nThe refusal names the card's state."}
			s.drawer, s.tasks = behind.ID, []factoryd.BoardTask{{Task: head, QueueAhead: 0}, {Task: behind, QueueAhead: 2, MergeBehind: head.IssueNumber}}
			r.seedScene(s)
			return behind.ID
		}},
		{galleryBoard, "a human card that waits for a card", func(r *rig) string {
			// #2101 as the board read it, waiting for #2100, whose title is
			// the one the spec gives it (the captured one names a branch)
			fx := fixture(facefixtures.GroupCard, "workflow-files card")
			s := fixtureScene(fx, all)
			s.tasks[slices.IndexFunc(s.tasks, func(b factoryd.BoardTask) bool { return b.ID == fx.Task.ID })].Dependencies = []factoryd.DepFact{{TaskID: facefixtures.TaskID(2100), Issue: 2100, Status: "queued"}}
			s.tasks = append(s.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: facefixtures.TaskID(2100), Kind: "agent", Status: "queued", Track: policy.TrackDirect,
				Repository: facefixtures.Repository, IssueNumber: 2100, Source: factoryd.SourcePlan, Author: "controller", CreatedAt: now.Add(-2 * time.Hour),
				Objective: "Narrow the close refusal to plans that hold criteria\n\nThe close refusal applies only to plans that hold criteria.",
				Why:       "A plan without criteria cannot close today, because the close refusal asks every plan for criteria."}})
			return r.seedScene(s)
		}},
		{galleryBoard, "author wait", func(r *rig) string {
			return r.seedScene(fixtureScene(fixture(facefixtures.GroupCase, string(factoryd.CaseAgentRequestInfo)), all))
		}},
		{galleryBoard, "long words", plain(factoryd.Task{ID: facefixtures.TaskID(2402), Kind: "agent", Status: "queued", Track: policy.TrackDirect,
			Repository: facefixtures.Repository, IssueNumber: 2402, Source: factoryd.SourceAPI, Author: "factory-chat", ChatFiled: true, CreatedAt: now.Add(-2 * time.Hour),
			Objective: "Fix TestWallClockStopShipsTheWorkAcrossEveryRetryAndHandover\n\nThe test fails on a retry.",
			Why:       "The page https://git.example/sf/code/wiki/Wall-clock-stops-on-the-runner-and-its-retries says why the run stops before it ships."})},
		{galleryBoard, "routine card", func(r *rig) string {
			s := galleryScene()
			s.addRoutines(now, false)
			r.seedScene(s)
			return galleryRoutine + labCodeProject.Name + "/" + labBump.Name
		}},
		{galleryBoard, "status: Fund the chat", func(r *rig) string {
			s := galleryScene()
			chat := factoryd.Task{ID: factoryd.ChatTaskID(labCodeProject.Name, now), Kind: factoryd.KindChat, Status: "running", Repository: facefixtures.Repository,
				BudgetEUR: 5, SpentEUR: 5.02, Source: factoryd.SourceChat, CreatedAt: now.Add(-10 * 24 * time.Hour)}
			s.tasks = []factoryd.BoardTask{{Task: chat}}
			s.events = []factoryd.Event{{At: now.Add(-30 * time.Minute), TaskID: chat.ID, Kind: factoryd.EventChatBudgetSpent, Actor: factoryd.FactoryDecider,
				Via: factoryd.ViaFactoryd, Detail: map[string]any{"budget_eur": 5.0, "spent_eur": 5.02, "suggest_eur": 2.0}}}
			r.seedScene(s)
			return galleryStatus
		}},
		{galleryBoard, "status: Deploy now", func(r *rig) string {
			r.seedScene(galleryScene())
			return galleryStatus
		}},
	}
}

// galleryFace is one entry as the gallery page shows it: the small card
// as the board's column holds it, the opened card beside it, and a
// problem when the entry did not render. Strip: the faces are the status
// strip and its panel, not a card and its drawer. Inbox: the inbox's rows
// as GET /chat answers them, which the page renders as the panel does,
// with the stopped chat's fund state.
type galleryFace struct {
	ID, Group, Kind, Problem string
	Small, Open              template.HTML
	Strip                    bool
	Inbox, InboxBudget       string
}

// galleryFaces seats e on a fresh rig and renders its faces: the board's
// columns holding the card alone and the card's drawer at its overview, a
// routine's ghost and its drawer, or the status strip shut and open.
func galleryFaces(t *testing.T, e galleryEntry) (galleryFace, error) {
	f := galleryFace{ID: "g-" + gallerySlug(e.Group+"-"+e.Kind), Group: e.Group, Kind: e.Kind}
	r := newRig(t)
	r.fg.setPullHead(labPull, labPullHead)
	ck, _ := r.loggedIn(t)
	target := e.Seat(r)
	get := func(path string) (string, error) {
		rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck)
		if rec.Code != http.StatusOK {
			line, _, _ := strings.Cut(rec.Body.String(), "\n")
			return "", fmt.Errorf("GET %s = %d: %s", path, rec.Code, line)
		}
		return rec.Body.String(), nil
	}
	page, err := get("/?project=" + labCodeProject.Name)
	if err != nil {
		return f, err
	}
	var open string
	switch {
	case strings.HasPrefix(target, galleryInbox+":"):
		var answer chatGetAnswer
		if body, err := get(chatPath + "?conversation=" + strings.TrimPrefix(target, galleryInbox+":")); err != nil {
			return f, err
		} else if err := json.Unmarshal([]byte(body), &answer); err != nil {
			return f, err
		}
		b, err := json.Marshal(answer.Messages)
		if err != nil {
			return f, err
		}
		f.Inbox = string(b)
		b, err = json.Marshal(answer.Budget)
		f.InboxBudget = string(b)
		return f, err
	case target == galleryStatus:
		f.Strip = true
		f.Small, f.Open, err = statusFaces(page)
		return f, err
	case strings.HasPrefix(target, galleryRoutine):
		name := strings.TrimPrefix(target, galleryRoutine)
		if f.Small, err = boardItems(page, "data-ghost", name); err == nil {
			open, err = get("/routines/" + name + "/drawer")
		}
	case endedHumanCard(r, target):
		open, err = get("/cards/" + target + "/drawer?tab=overview")
	default:
		if f.Small, err = boardItems(page, "data-card", target); err == nil {
			open, err = get("/cards/" + target + "/drawer?tab=overview")
		}
	}
	f.Open = template.HTML(open)
	return f, err
}

// roundTripped is row as the store reads it back, its lists []any.
func roundTripped(row map[string]any) map[string]any {
	b, err := json.Marshal(row)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

// gallerySlug is s with every run of characters but letters and digits one
// dash, lower case.
func gallerySlug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// endedHumanCard says card id of r's store is a human card that ended: the
// board holds no small card of it, and its opened card alone shows it.
func endedHumanCard(r *rig, id string) bool {
	return slices.ContainsFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == id && b.Kind == "human" && factoryd.Final(b.Status) })
}

// boardItems are the board's columns that show the items whose attr is
// value, each holding those items alone.
func boardItems(page, attr, value string) (template.HTML, error) {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "", err
	}
	var columns []*html.Node
	for n := range doc.Descendants() {
		if n.Type == html.ElementNode && n.Data == "section" && slices.Contains(strings.Fields(htmlAttr(n, "class")), "column") {
			columns = append(columns, n)
		}
	}
	var b strings.Builder
	for _, col := range columns {
		var cards *html.Node
		for c := range col.ChildNodes() {
			if c.Type == html.ElementNode && htmlAttr(c, "class") == "cards" {
				cards = c
			}
		}
		if cards == nil {
			continue
		}
		var drop []*html.Node
		shown := false
		for c := range cards.ChildNodes() {
			if c.Type == html.ElementNode && htmlAttr(c, attr) == value {
				shown = true
			} else {
				drop = append(drop, c)
			}
		}
		if !shown {
			continue
		}
		for _, c := range drop {
			cards.RemoveChild(c)
		}
		if err := html.Render(&b, col); err != nil {
			return "", err
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("the board shows no item whose %s is %q", attr, value)
	}
	return template.HTML(b.String()), nil
}

// statusFaces are the page's status strip shut, and open on its panel.
func statusFaces(page string) (shut, open template.HTML, err error) {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "", "", err
	}
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode || htmlAttr(n, "id") != "status-line" {
			continue
		}
		var b strings.Builder
		if err := html.Render(&b, n); err != nil {
			return "", "", err
		}
		n.Attr = append(n.Attr, html.Attribute{Key: "open"})
		var o strings.Builder
		if err := html.Render(&o, n); err != nil {
			return "", "", err
		}
		return template.HTML(b.String()), template.HTML(o.String()), nil
	}
	return "", "", errors.New("the page has no status strip")
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// galleryPage renders every entry of the gallery, an entry that fails
// with its problem in place of its faces.
func galleryPage(t *testing.T) []galleryFace {
	var out []galleryFace
	for _, e := range galleryEntries() {
		f, err := galleryFaces(t, e)
		if err != nil {
			f.Problem = err.Error()
		}
		out = append(out, f)
	}
	return out
}

// censusCardKinds are the card kinds of the board-cards census
// (docs/board-cards-problem-2026-11-16.md, appendix A), a combined row split
// into one kind per name it holds.
var censusCardKinds = []string{
	"direct work card", "plan card", "plan child: foundation", "plan child: feature", "plan child: integration", "delta card",
	"retry replacement", "successor", "delegated card", "helper task: screening", "helper task: sizing", "helper task: prover",
	"tool request", "roll", "main fix", "revert", "group revert", "chain repair (agent)", "alert (agent)", "routine (agent)",
	"template update", "schema copy", "GitHub sync", "image removal",
	"human issue", "reproduction-rule card", "workflow-files card", "card moved to a human", "factory pull request on a project without agents",
	"chat escalation", "alert (human)", "chain stall", "chain repair (human)", "roll refused", "roll failed", "rerun", "still red",
	"config rejected", "month cap", "provider freeze", "judgment budget", "production held", "routine for a human", "platform deaths",
}

// humanCardSources are the card sources that file a human card alone,
// which stands in Needs you; every other source's card stands in each
// column.
var humanCardSources = []string{
	factoryd.SourceRollRefused, factoryd.SourceRollFailed, factoryd.SourceRerun, factoryd.SourceStillRed, factoryd.SourceChainStall,
	factoryd.SourceChat, factoryd.SourceChatBudget, factoryd.SourceMonthCap, factoryd.SourceFreezeCard, factoryd.SourceConfig, factoryd.SourceProductionHeld,
	factoryd.SourcePlatformDeaths,
}

// galleryColumns are the board's columns as a source entry's kind names
// them, with the title the board heads each with.
var galleryColumns = map[string]string{"needs-you": "Needs you", "queued": "Queued", "in-progress": "In progress", "done": "Done"}

// The gallery holds an entry for every case kind, human action, approval
// kind and monitor flag, for every card source in each column it stands
// in, for every card kind of the census and for the board's own items;
// each entry's seat renders its small card and its opened card, and a
// source's small card stands in the column its kind names.
func TestGalleryHoldsEveryKind(t *testing.T) {
	entries := galleryEntries()
	helperParents := slices.DeleteFunc(facefixtures.All(), func(fx facefixtures.Fixture) bool { return fx.Group != facefixtures.GroupSource || fx.Parent == nil })
	holds := func(group string, match func(kind string) bool) bool {
		return slices.ContainsFunc(entries, func(e galleryEntry) bool { return e.Group == group && match(e.Kind) })
	}
	named := func(want string) func(string) bool { return func(k string) bool { return k == want } }
	need := func(group string, kinds []string) {
		for _, k := range kinds {
			if !holds(group, named(k)) {
				t.Errorf("the gallery holds no entry of %s %q", group, k)
			}
		}
	}
	var cases []string
	for _, k := range factoryd.CaseKinds() {
		cases = append(cases, string(k))
	}
	need(facefixtures.GroupCase, cases)
	need(facefixtures.GroupAction, factoryd.HumanActions())
	need(facefixtures.GroupApproval, factoryd.ApprovalKinds())
	need(facefixtures.GroupFlag, factoryd.MonitorFlags())
	need(facefixtures.GroupCard, censusCardKinds)
	// a freeze card at the two ends no answer of its ask makes, and the
	// face of a card that waits with no question to ask again
	need(facefixtures.GroupCard, []string{"provider freeze a retry press closed", "provider freeze its watcher gave up"})
	need(facefixtures.GroupVariant, []string{"other with no question to ask again"})
	for _, s := range factoryd.CardSources() {
		columns := slices.Collect(maps.Keys(galleryColumns))
		if slices.Contains(humanCardSources, s) {
			columns = []string{"needs-you"}
		}
		for _, col := range columns {
			need(facefixtures.GroupSource, []string{s + "/" + col})
		}
	}
	need(galleryInbox, factoryd.BirthKinds())
	need(galleryBoard, []string{"three open cases", "closed into its successor", "a long suggested choice", "queued at the month cap",
		"behind another card in the merge queue", "a human card that waits for a card", "author wait", "long words", "routine card", "status: Fund the chat", "status: Deploy now"})

	for _, e := range entries {
		t.Run(e.Group+"/"+e.Kind, func(t *testing.T) {
			f, err := galleryFaces(t, e)
			if err != nil {
				t.Fatal(err)
			}
			if action, ok := strings.CutPrefix(e.Kind, "status: "); ok && !strings.Contains(string(f.Open), ">"+action+"</button>") {
				t.Errorf("the status panel offers no %s:\n%s", action, f.Open)
			}
			if e.Kind == "author wait" && !strings.Contains(string(f.Open), ">Answer in my words</button>") {
				t.Errorf("the card that waits on its author offers no answer:\n%s", f.Open)
			}
			if source, col, ok := strings.Cut(e.Kind, "/"); ok && e.Group == facefixtures.GroupSource {
				// a helper task stands on its parent: its ask in Needs you,
				// else the parent's card in its own column
				want := galleryColumns[col]
				if i := slices.IndexFunc(helperParents, func(p facefixtures.Fixture) bool { return p.Kind == e.Kind }); i >= 0 && col != "needs-you" {
					want = columnTitles[fiveColumn(*helperParents[i].Parent)]
				}
				small := string(f.Small)
				if n := strings.Count(small, `<section class="column`); n != 1 || !strings.Contains(small, ">"+want+` <span class="count">`) {
					t.Errorf("the %s card stands in %d columns, wanted %s alone:\n%s", source, n, want, small)
				}
			}
		})
	}
}
