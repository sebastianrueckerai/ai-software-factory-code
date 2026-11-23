package factoryd_test

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// humanKinds are the 19 human card kinds of the census, by their fixtures,
// with the answers each face offers, from the plan's answer table: a work
// card's "I did it" and its way out where one can happen, and a template
// kind's own answers, which say what the factory saw. done is the effect
// of "I did it" and later the effect of "Not now", "" where the face has
// none.
var humanKinds = []struct {
	kind        string
	answers     []string
	template    bool
	done, later string
}{
	{"human issue", []string{"I did it", "Have the factory do it", "Not now"}, false, noCheck, stays},
	{"reproduction-rule card", []string{"I did it", "Not now"}, false, noCheck, stays},
	{"workflow-files card", []string{"I did it", "I cannot do this", "Not now"}, false,
		"The factory does not check this. The card closes, and the plan goes on.", stays + " The plan waits for it."},
	{"card moved to a human", []string{"I did it", "Not now"}, false, noCheck, stays},
	{"factory pull request on a project without agents", []string{"Merge it", "Reject and close it"}, true, "", ""},
	{"chat escalation", []string{"Move them to S3", "Not now"}, false, "", stays},
	{"alert (human)", []string{"Noted", "Not now"}, true, "", stays},
	{"alert that cleared (human)", []string{"Noted", "Not now"}, true, "", stays},
	{"chain stall", []string{"Run the chain again", "Look again", "End the card"}, true, "", ""},
	{"chain repair (human)", []string{"I did it", "Not now"}, true,
		"Closes the card and lets the red at commit 3c9a1e0 stand. A red chain at a newer head files a new card.", stays},
	{"roll refused", []string{"I did it", "Not now"}, true,
		"Closes the card. The factory judges the images of main's next green run again.", stays + " The new images stay off the cluster until they are signed."},
	{"roll failed", []string{"Try the roll again", "Not now", "End the card"}, true, "", stays + " Every card merged since the last roll waits for a green chain."},
	{"rerun", []string{"I did it", "Not now"}, true, mainRun, stays + " The merge queue stays paused until a push run of main is green."},
	{"still red", []string{"I did it", "Not now"}, true, mainRun, stays + " The merge queue stays paused until a push run of main is green."},
	{"config rejected", []string{"I did it", "Not now"}, true,
		"Closes the card. The factory checks each new commit of factory/, and a valid one takes effect within 30 seconds.", stays + " The configuration before it stays in force."},
	{"month cap", []string{"Noted", "Not now"}, true, "", stays + " Every model call waits until a human resets the month's count in the console's status panel, the limit is raised by a commit, or the month ends."},
	{"platform deaths", []string{"Noted", "Not now"}, true, "", stays},
	{"judgment budget", []string{"Add money to the chat", "Not now", "Noted"}, true, "", stays + " Every stopped card comes to you until the budget is raised or the month ends."},
	{"production held", []string{"Not now", "Deploy now", "Send back to the planner", "Take the plan off main"}, true, "",
		stays + " Production of code stays held while plan “empty commit fires gate” (#1947) is open on main."},
	{"routine for a human", []string{"I did it", "Have the factory do it", "Not now"}, false, noCheck, stays},
}

// The effects of the human cards' answers that many kinds share.
const (
	noCheck = "The factory does not check this. The card closes."
	stays   = "The card stays in Needs you."
	mainRun = "Closes the card. The factory reads main's next push run, and a green one ends the queue's pause."
)

// A human card asks one thing: 2 to 4 answers in the vocabulary's words,
// or the asker's own, each with its effect. "I did it" names the check the
// factory runs, or says that it runs none; "Not now" keeps the card in
// Needs you and names what waits on it. A template kind says what the
// factory saw, with its numbers, and every card that waits for another
// card says so first. #2101, a plan's card for a human, says what to do,
// why a human does it and which plan waits, and offers a way out.
func TestHumanCardsAskOneThing(t *testing.T) {
	vocabulary := map[string]bool{}
	for _, a := range factoryd.Vocabulary {
		vocabulary[a.Label] = true
	}
	number := regexp.MustCompile(`\d`)
	for _, k := range humanKinds {
		x := fixture(t, facefixtures.GroupCard, k.kind)
		face := x.face
		if got := labels(face.Answers); !slices.Equal(got, k.answers) {
			t.Errorf("%s: answers %q, want %q", k.kind, got, k.answers)
		}
		effects := map[string]string{}
		for _, a := range face.Answers {
			if !vocabulary[a.Label] && !(k.kind == "chat escalation" && a.Value != "") {
				t.Errorf("%s: %q is no label of the vocabulary", k.kind, a.Label)
			}
			if a.Effect == "" {
				t.Errorf("%s: %q has no effect line", k.kind, a.Label)
			}
			effects[a.Label] = a.Effect
		}
		if effects["I did it"] != k.done || effects["Not now"] != k.later {
			t.Errorf("%s: I did it %q, Not now %q; want %q, %q", k.kind, effects["I did it"], effects["Not now"], k.done, k.later)
		}
		if k.template && !number.MatchString(face.Story.Happened) {
			t.Errorf("%s: the story line %q says nothing the factory saw with its numbers", k.kind, face.Story.Happened)
		}
		f := x.facts
		f.Dependencies = []factoryd.DepFact{{TaskID: facefixtures.TaskID(2100), Issue: 2100, Status: "queued"}}
		if why := factoryd.FaceOf(x.c, f).Why; !strings.HasPrefix(why, "It waits for #2100") {
			t.Errorf("%s: waiting for #2100, the why line reads %q", k.kind, why)
		}
	}

	face := fixture(t, facefixtures.GroupCard, "workflow-files card").face
	const ask, why = "Change two lines in the runner's build files",
		"The factory's push rule keeps agents out of workflow files, so a human does this card, and plan #1947 waits for it."
	if face.Ask != ask || face.Why != why {
		t.Errorf("#2101 asks %q, why %q; want %q, %q", face.Ask, face.Why, ask, why)
	}
	back := face.Answers[1]
	if want := "This card ends, and the planner plans the whole plan again with your note. Its started cards stop."; back.Effect != want {
		t.Errorf("I cannot do this: %q, want %q", back.Effect, want)
	}
	if len(back.Fields) != 1 || !back.Fields[0].Required || back.Fields[0].Hint != "One line for the planner: what keeps you from doing it" {
		t.Errorf("I cannot do this opens %+v, want one required note with its hint", back.Fields)
	}
	if i := slices.IndexFunc(face.More, func(a factoryd.Answer) bool { return a.Label == "End the card" }); i < 0 ||
		face.More[i].Effect != "Ends the card and closes its issue. The cards that wait for it ask you first." {
		t.Errorf("#2101's End the card = %+v, want it to say the cards that wait for it ask first", face.More)
	}
	pr := fixture(t, facefixtures.GroupCard, "factory pull request on a project without agents").face
	if merge, reject := pr.Answers[0].Effect, pr.Answers[1].Effect; merge != "Merges the pull request as you. The card ends." || reject != "Closes the pull request. The card ends." {
		t.Errorf("the factory pull request's effects %q, %q; want each to say the card ends", merge, reject)
	}
}

// A card that waits for its issue's author asks the agent's question, and
// the human answers it in their own words, later, or ends the card.
func TestAuthorWaitAsksTheAuthor(t *testing.T) {
	x := fixture(t, facefixtures.GroupCase, string(factoryd.CaseAgentRequestInfo))
	if x.fx.Task.Status != "waiting-poster" {
		t.Fatalf("the card is %s, want waiting on its author", x.fx.Task.Status)
	}
	if want := "Which Hetzner location does the load test's server use, fsn1 or nbg1?"; x.face.Ask != want {
		t.Errorf("ask %q, want the agent's question %q", x.face.Ask, want)
	}
	if got, want := labels(x.face.Answers), []string{"Answer in my words", "Not now", "End the card"}; !slices.Equal(got, want) {
		t.Errorf("answers %q, want %q", got, want)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). Every card
// that waits on a human holds a case from its filing: each of the 19 human
// kinds as its filer files it, a human card whose question offers no
// answers of its own, and a card whose agent asked the issue's author. A
// card that asks at once is in Needs you with its case at the human;
// factoryd's own human card waits for its face with its case open. Each
// case's face offers 2 to 4 answers.
func TestEveryNeedsYouCardHoldsACase(t *testing.T) {
	ctx := context.Background()
	s, err := factoryd.OpenStore(ctx, schematest.DSN(t, "factoryd_human_face"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	type filed struct {
		name string
		card factoryd.Task
		file func() error
	}
	var cards []filed
	parents := map[string]bool{}
	for _, k := range humanKinds {
		fx := fixtureOf(t, facefixtures.GroupCard, k.kind)
		if p := fx.Task.Parent; p != "" && !parents[p] {
			// the plan or the chat a card belongs to stands first
			parents[p] = true
			if err := s.CreateTask(ctx, factoryd.Task{ID: p, Kind: "agent", Track: policy.TrackPlan, Status: "done", Repository: facefixtures.Repository, RiskClass: "standard", Objective: "Its parent"}); err != nil {
				t.Fatal(err)
			}
		}
		cards = append(cards, filed{k.kind, fx.Task, func() error {
			_, _, err := s.CreateHumanCard(ctx, fx.Task, *fx.Human)
			return err
		}})
	}
	bare := factoryd.Task{ID: "t-0bare0", Kind: "human", Status: "waiting-human", Repository: facefixtures.Repository, IssueNumber: 3001,
		Source: factoryd.SourceChat, RiskClass: "standard", Objective: "Should the nightly snapshots move to S3?"}
	cards = append(cards, filed{"a question with no answers of its own", bare, func() error {
		q := factoryd.HumanQuestion{Ask: "Should the nightly snapshots move to S3?", Why: "Only a human sets where the snapshots go."}
		_, _, err := s.CreateHumanCard(ctx, bare, factoryd.HumanAsk{Q: q, By: "sebastian", Asker: "agent", Via: factoryd.ViaChat})
		return err
	}})
	info := fixtureOf(t, facefixtures.GroupCase, string(factoryd.CaseAgentRequestInfo))
	cards = append(cards, filed{"after request_info", info.Task, func() error {
		if err := s.CreateTask(ctx, info.Task); err != nil {
			return err
		}
		_, err := s.Record(ctx, factoryd.Event{Repository: info.Task.Repository, TaskID: info.Task.ID, IssueNumber: info.Task.IssueNumber,
			Kind: factoryd.EventQuestionAsked, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: info.Ask.Detail})
		return err
	}})
	for _, c := range cards {
		if err := c.file(); err != nil {
			t.Fatalf("%s: file it: %v", c.name, err)
		}
	}
	desk, err := s.CaseDesk(ctx, facefixtures.Repository)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		got, ok, err := s.Task(ctx, c.card.ID)
		want, at := "waiting-human", factoryd.CaseAtHuman
		switch {
		case c.card.Status == "waiting-poster":
			want = "waiting-poster"
		case c.card.Kind == "human" && factoryd.HumanCardStatus(c.card.Source) == "waiting-agent":
			// factoryd's own human card climbs first, out of Needs you
			want, at = "waiting-agent", factoryd.CaseOpen
		}
		if err != nil || !ok || got.Status != want {
			t.Errorf("%s: the card reads %s, %v, %v; want it %s", c.name, got.Status, ok, err, want)
			continue
		}
		i := slices.IndexFunc(desk.Cases, func(k factoryd.Case) bool { return k.TaskID == c.card.ID })
		if i < 0 || desk.Cases[i].Status != at {
			t.Errorf("%s: no %s case among %d live cases", c.name, at, len(desk.Cases))
			continue
		}
		if face := factoryd.CaseFace(desk.Cases[i], got, desk.Asks[desk.Cases[i].ID]); len(face.Offers) < 2 || len(face.Offers) > 4 {
			t.Errorf("%s: the case's face offers %+v, want 2 to 4 answers", c.name, face.Offers)
		}
	}
}

// The platform card's ask, why line, story and face show no machine
// artefact and none of the ladder's words, and its ask passes the filing
// door, over every kind of death: a session's and a gate pod's of a card on
// the card's repository and on another, a CI job's, one with no node, and
// more than the face lists.
func TestPlatformCardFaceIsPlain(t *testing.T) {
	cards := map[string]factoryd.Task{
		"t-own":   {ID: "t-own", Repository: facefixtures.Repository, IssueNumber: 2004},
		"t-other": {ID: "t-other", Repository: "sf/infra", IssueNumber: 14},
	}
	end := time.Date(2026, 11, 23, 14, 2, 0, 0, time.UTC)
	rules := []factoryd.DeathRule{factoryd.RuleGuardKill, factoryd.RuleNodeOOM, factoryd.RuleNodePressure, factoryd.RuleNodeRebooted,
		factoryd.RuleNodeNotReady, factoryd.RuleDisruption, factoryd.RuleGoneAfterRestart, factoryd.RuleDeleted}
	var deaths []factoryd.PodDeath
	for i := range 12 {
		rec := factoryd.DeathRecord{Class: factoryd.DeathPlatform, Rule: rules[i%len(rules)], At: end.Add(-time.Duration(i) * 7 * time.Minute),
			Node: "sf-" + strconv.Itoa(i%3+1), Resource: "memory"}
		if i == 4 {
			rec.Node = ""
		}
		d := factoryd.PodDeath{Kind: "session", TaskID: "t-own", SessionID: "s" + strconv.Itoa(i), Record: rec}
		switch i % 4 {
		case 1:
			d.Kind, d.TaskID, d.SessionID, d.Runner = "ci", "", "", "ci-job-"+strconv.Itoa(40+i)
		case 2:
			d.Kind, d.SessionID, d.Record.PodUID = "gate", "", "uid-"+strconv.Itoa(i)
		case 3:
			d.TaskID = "t-other"
		}
		deaths = append(deaths, d)
	}
	q := factoryd.PlatformDeathsQuestion(deaths, cards, facefixtures.Repository)
	ladder := regexp.MustCompile(`(?i)\b(rungs?|cases?|pools?)\b`)
	for part, text := range map[string]string{"ask": q.Ask, "why": q.Why, "happened": q.Happened, "face": q.Info} {
		if faults := slices.Concat(facefixtures.HumanFaults(text), facefixtures.TextFaults(text)); len(faults) > 0 {
			t.Errorf("the %s %q: %q", part, text, faults)
		}
		if w := ladder.FindString(text); w != "" {
			t.Errorf("the %s %q says %q", part, text, w)
		}
	}
	if refusals := factoryd.CheckBrief(factoryd.BriefText{Title: q.Ask, Why: q.Why, Body: q.Info}, factoryd.BriefAsk, everyRef{}); len(refusals) > 0 {
		t.Errorf("the door refuses the card's brief: %q", refusals)
	}
	for _, want := range []string{"- 14:02 UTC, card #2004: the node's memory guard killed the pod on sf-1\n",
		"- 13:55 UTC, CI job ci-job-41: the node ran out of memory on sf-2\n",
		"- 13:48 UTC, a gate of card #2004: the node ran low on memory on sf-3\n",
		"- 13:41 UTC, card sf/infra#14: the node restarted on sf-1\n",
		"- 13:34 UTC, card #2004: the node stopped answering\n", "- and 2 more\n"} {
		if !strings.Contains(q.Info, want) {
			t.Errorf("the face lacks %q:\n%s", want, q.Info)
		}
	}
}
