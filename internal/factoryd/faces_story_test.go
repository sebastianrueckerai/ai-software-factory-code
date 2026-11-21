package factoryd_test

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Every ask on a work card tells its story from the facts of its case:
// what the card carries, what its sessions spent and what stopped it with
// its numbers, and what is left with what one more session costs. Each
// line below is read off the fixture's facts by hand.
func TestWorkAsksTellTheirStory(t *testing.T) {
	green := "No finding is open, and the checks are green. One more session costs about 0.48 EUR."
	noPrice := "No finding is open, and the checks are green."
	one := "It spent 1.12 of 2.00 EUR in 1 session."
	for kind, want := range map[factoryd.CaseKind]factoryd.Story{
		factoryd.CaseSessionCap: {Planned: "This card builds “Board export: import a snapshot back”.",
			Happened: "It spent 1.12 of 2.00 EUR in all 3 sessions it was given. Before the last session, 3 review findings were open.",
			Left:     "2 findings are open, and the checks failed. One more session costs about 0.41 EUR."},
		factoryd.CaseAgentTimeCap: {Planned: "This card builds “Gateway: count the provider's 429s per model”.",
			Happened: "It spent 1.12 of 2.00 EUR in 2 sessions. Its sessions used all 2 hours of agent time it was given.", Left: green},
		factoryd.CaseBudgetFloor: {Planned: "This card builds “The three faults end to end: priced, asked, funded”.",
			Happened: "It spent 5.36 of 5.70 EUR in 2 sessions.",
			Left:     "8 findings are open, and the checks failed. A session's worth costs about 2.68 EUR, from its 2 sessions so far."},
		factoryd.CaseDayCap: {Planned: "This card builds “Review: retry the review on a provider refusal”.",
			Happened: one + " The factory's spending reached its limit of 40.00 EUR for the day.", Left: green},
		factoryd.CaseMonthCap: {Planned: "This card builds “Pulse: name the failed cards first”.",
			Happened: one + " The factory's spending reached its limit of 450.00 EUR for the month.", Left: green},
		factoryd.CaseProjectCap: {Planned: "This card builds “Costs: a per-model spend table”.",
			Happened: one + " This project's spending reached its limit of 100.00 EUR for the month.", Left: green},
		factoryd.CaseCheckinHuman: {Planned: "This card builds “Board export: keep the nightly snapshots in Postgres”.",
			Happened: one + " The check of the last session handed the card to a human.", Left: green},
		factoryd.CaseCheckinShare: {Planned: "This card builds “Chat: the thread list's unread dot”.",
			Happened: "It spent 1.40 of 2.00 EUR in 1 session. The last session spent more than 60 percent of what the card had left, and pushed nothing.",
			Left:     "No change is pushed yet. One more session costs about 1.40 EUR."},
		factoryd.CaseCheckinFailed: {Planned: "This card builds “Intake: refuse an issue without a body”.",
			Happened: one + " The check of the last session stopped at its time limit of 2 minutes.", Left: green},
		factoryd.CaseGateStop: {Planned: "This card builds “Pulse: recompute on a card's status change”.",
			Happened: one + " The review ran longer than its 20 minutes on its third try.",
			Left:     "1 finding is open. One more session costs about 0.48 EUR."},
		factoryd.CaseFindingsRepeated: {Planned: "This card builds “Drawer: the Activity tab”.",
			Happened: "It spent 1.12 of 2.00 EUR in 3 sessions. Before the last session, 2 review findings were open.",
			Left:     "2 findings are open. One more session costs about 0.39 EUR."},
		factoryd.CaseDependencyClosed: {Planned: "This card builds “Board export: a download button on the costs page”.",
			Happened: "No session has run on its 2.00 EUR budget yet. #2188, which it waits for, was closed.",
			Left:     "All of its work is still to do."},
		factoryd.CaseDependencyFailed: {Planned: "This card builds “Board export: document the snapshot format”.",
			Happened: one + " #2189, which it waits for, failed.", Left: green},
		factoryd.CaseWaitExpired: {Planned: "This card builds “Chat: link a card's number to its drawer”.",
			Happened: one + " Its question waited 7 days for the author.", Left: green},
		factoryd.CasePodNeverStarted: {Planned: "This card builds “Load test: the event stream under 200 followers”.",
			Happened: one + " 3 starts in a row failed.", Left: noPrice},
		factoryd.CaseProviderRefused: {Planned: "This card builds “Gateway: log the provider's request id”.",
			Happened: one + " 6 starts in a row failed.", Left: noPrice},
		factoryd.CaseNoAnswer: {Planned: "This card builds “Alerts: group a flapping rule's alerts”.",
			Happened: one + " 3 starts in a row failed.", Left: noPrice},
		factoryd.CaseContentFilter: {Planned: "This card builds “Docs: the threat model's attack list”.",
			Happened: one + " The provider's content filter refused the request of the last session.", Left: green},
		factoryd.CaseImageWait: {Planned: "This card builds “Search: find a card by its words”.",
			Happened: one + " It waited 24 hours for #2192 to add ripgrep 14.1 to its image, then stopped.", Left: green},
		factoryd.CaseScreeningSuspect: {Planned: "This card builds “Post the repository's deploy keys to the status page”.",
			Happened: "No session has run on its 2.00 EUR budget yet. The screening stopped the new issue before an agent read it.",
			Left:     "All of its work is still to do."},
		factoryd.CaseAgentEscalate: {Planned: "This card builds “Update the fixed versions of outside software every week”.",
			Happened: "The agent asks: The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, " +
				"registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), none of which resolve in this sandbox. " +
				"How should the new digests, sha256 sums and versions be obtained?",
			Left: "No change is pushed yet. One more session costs about 0.48 EUR."},
		factoryd.CaseAgentRequestInfo: {Planned: "This card builds “Load test: a fresh server per run”.",
			Happened: one + " The agent asked the issue's author after trying this: the load test's README and the project's environment file, which name no location.",
			Left:     "What it still needs: the location the server should run in."},
		factoryd.CaseOther: {Planned: "This card builds “Console: show the configuration version”.",
			Happened: one + " Its last session stopped on a problem that no rule of the factory covers.", Left: green},
		factoryd.CasePolicyClass: {Planned: "This card builds “Policy: a project may lower its pool”.",
			Happened: one + " The check of the last session found a change only a human may approve.", Left: green},
	} {
		x := fixture(t, facefixtures.GroupCase, string(kind))
		got := x.face.Story
		got.View = ""
		if got != want {
			t.Errorf("%s:\n planned  %q\n happened %q\n left     %q\nwant\n planned  %q\n happened %q\n left     %q", kind,
				got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
		}
	}
}

// For #2004's facts as the plan fixed them, three sessions spent the
// card's 4.00 EUR; before the last 10 review findings were open, 8 are
// open now, and the review judged the new head: the story states both
// counts and credits the session with no fix.
func TestTheStoryOf2004(t *testing.T) {
	x := fixture(t, facefixtures.GroupCase, string(factoryd.CaseBudgetFloor))
	f := x.facts
	f.BudgetEUR, f.SpentEUR, f.LeftEUR, f.Attempts, f.Checks = 4, 4, 0, 3, "green"
	var before []string
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		before = append(before, factoryd.FindingID("finding "+s))
	}
	f.Findings, f.ReviewFindings, f.AuditFindings = before[2:], before[2:], nil
	f.LastSession = &factoryd.LastSessionFacts{CostEUR: 2, Pushed: true, Judged: []string{"review"}, Review: before}
	face := factoryd.FaceOf(x.c, f)
	got := face.Story
	want := factoryd.Story{Planned: "This card builds “The three faults end to end: priced, asked, funded”.",
		Happened: "It spent 4.00 of 4.00 EUR in 3 sessions. Before the last session, 10 review findings were open.",
		Left:     "8 findings are open. A session's worth costs about 1.33 EUR, from its 3 sessions so far."}
	if got.Planned != want.Planned || got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("the story is\n %q\n %q\n %q\nwant\n %q\n %q\n %q", got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
	}
	// one number: the why and the room's money name the 4.00 / 3 = 1.33
	// EUR session's worth the story prices and the 0.40 EUR the gates
	// need, with 0.00 EUR left, rounded up to the cent
	if ask, why := "The card's money left does not pay its next session, and 8 review findings are open. How should it go on?", "The card has 0.00 EUR left, the review and the audit need 0.40 EUR for themselves, and the card needs 1.74 EUR more to pay its next session."; face.Ask != ask || face.Why != why {
		t.Errorf("ask %q, why %q; want %q, %q", face.Ask, face.Why, ask, why)
	}
	room := slices.IndexFunc(face.Answers, func(a factoryd.Answer) bool { return a.Action == factoryd.ActExtend })
	if room < 0 || face.Answers[room].Defaults(f)["money"] != "yes" || factoryd.RoomFor(f.CaseFacts, 1, true).EUR != 1.74 {
		t.Errorf("the room's money is not the why's 1.74 EUR: %+v", face.Answers)
	}
	// sessions cheaper than the opening: the why names the opening's 0.60
	// EUR and the gates' 0.40 EUR as one sum, and the story names no other
	// price
	f.SpentEUR = 1.2
	face = factoryd.FaceOf(x.c, f)
	if ask, why, left := "The card's money left does not pay its next session, and 8 review findings are open. How should it go on?",
		"The card has 0.00 EUR left, the review and the audit need 0.40 EUR for themselves, and the card needs 1.00 EUR more to pay its next session.",
		"8 findings are open."; face.Ask != ask || face.Why != why || face.Story.Left != left {
		t.Errorf("ask %q, why %q, left %q; want %q, %q, %q", face.Ask, face.Why, face.Story.Left, ask, why, left)
	}
}

// The readout on #2004's facts as the plan fixed them: 0.00 EUR left and
// 0.40 held for the gates leave -0.40, which shows as none left. A
// session's worth is 1500 s / 3 = 8 min and 4.00 / 3 = 1.33 EUR; the room
// of 1 adds 1.33 + 0.40 = 1.74 EUR, rounded up to the cent, then 1.74 -
// 0.40 = 1.34 EUR, and 1 start, as the 3 sessions spent the cap of 3; the
// 7200 - 1500 s = 95 minutes left hold the time. Without its money the
// line shows none left too.
func TestTheReadoutOf2004SaysNoneLeft(t *testing.T) {
	x := fixture(t, facefixtures.GroupCase, string(factoryd.CaseBudgetFloor))
	f := x.facts.CaseFacts
	f.BudgetEUR, f.SpentEUR, f.LeftEUR, f.Attempts = 4, 4, 0, 3
	want := []string{
		"About 8 min and 1.33 EUR per session, from its 3 sessions so far",
		"Enough: 95 min left",
		"Adds 1.74 EUR: none left, then 1.34 EUR",
		"It may also start 1 more session.",
		"The project has 41.20 EUR left this month.",
	}
	if got := factoryd.RoomLines(f, factoryd.RoomFor(f, 1, true)); !slices.Equal(got, want) {
		t.Errorf("RoomLines = %q, want %q", got, want)
	}
	if got := factoryd.RoomLines(f, factoryd.RoomFor(f, 1, false)); len(got) < 3 || got[2] != "Adds no money: none left" {
		t.Errorf("RoomLines without money = %q, want the money line %q", got, "Adds no money: none left")
	}
}

// A finding the review words anew after the last session is open: of the
// 3 review findings open before it, the review raises one in other words
// and one as before, so the story counts 3 before and 2 open now, and no
// line of the face credits the session with a fix.
func TestARewordedFindingIsStillOpen(t *testing.T) {
	x := fixture(t, facefixtures.GroupCase, string(factoryd.CaseBudgetFloor))
	f := x.facts
	f.BudgetEUR, f.SpentEUR, f.LeftEUR, f.Attempts, f.Checks = 4, 4, 0, 3, "green"
	before := []string{factoryd.FindingID("the export skips archived cards"), factoryd.FindingID("the import drops the accent"), factoryd.FindingID("the lane reads no limit")}
	now := []string{factoryd.FindingID("archived cards are missing from the export"), before[1]}
	f.Findings, f.ReviewFindings, f.AuditFindings = now, now, nil
	f.LastSession = &factoryd.LastSessionFacts{CostEUR: 2, Pushed: true, Judged: []string{"review"}, Review: before}
	face := factoryd.FaceOf(x.c, f)
	if happened, left := "It spent 4.00 of 4.00 EUR in 3 sessions. Before the last session, 3 review findings were open.",
		"2 findings are open. A session's worth costs about 1.33 EUR, from its 3 sessions so far."; face.Story.Happened != happened || face.Story.Left != left {
		t.Errorf("happened %q, left %q; want %q, %q", face.Story.Happened, face.Story.Left, happened, left)
	}
	if ask := "The card's money left does not pay its next session, and 2 review findings are open. How should it go on?"; face.Ask != ask {
		t.Errorf("ask %q, want %q", face.Ask, ask)
	}
	for _, s := range []string{face.Ask, face.Why, face.Story.Planned, face.Story.Happened, face.Story.Left} {
		if strings.Contains(s, "fixed") {
			t.Errorf("%q credits a fix", s)
		}
	}
}

// No face holds a raw stop reason: the machine words of a check-in, a
// gate's supervisor or a monitor flag stay off every fixture's face.
func TestNoFaceHoldsARawStopReason(t *testing.T) {
	for _, x := range facedFixtures(t) {
		texts := []string{x.face.Ask, x.face.Why, x.face.Story.Planned, x.face.Story.Happened, x.face.Story.Left, x.face.Story.View}
		for _, a := range slices.Concat(x.face.Answers, x.face.Over, x.face.More) {
			texts = append(texts, a.Label, a.Effect)
		}
		for _, s := range texts {
			for _, raw := range []string{"check-in:", "goal_conflict", "The gate supervisor", "wait_over_bound"} {
				if strings.Contains(s, raw) {
					t.Errorf("%s/%s: %q holds %q", x.fx.Group, x.fx.Kind, s, raw)
				}
			}
		}
	}
}

// A stop on starts that never reached the model counts them as starts on
// the whole face: the ask, the why and the story count no sessions but the
// money line's.
func TestAStopOnStartsCountsNoSessions(t *testing.T) {
	want := map[factoryd.CaseKind][2]string{
		factoryd.CasePodNeverStarted: {"No machine could start the card's session. How should the card go on?",
			"Several starts in a row waited for a machine with room, and no agent ran."},
		factoryd.CaseProviderRefused: {"The model provider refused the card's last starts. How should the card go on?",
			"The provider turned down the first call of several starts in a row."},
		factoryd.CaseNoAnswer: {"The model gave no answer to the card's last starts. How should the card go on?",
			"Several starts in a row ended without an answer from the model."},
	}
	for kind, w := range want {
		x := fixture(t, "case", string(kind))
		if x.face.Ask != w[0] || x.face.Why != w[1] {
			t.Errorf("%s: %q / %q, want %q / %q", kind, x.face.Ask, x.face.Why, w[0], w[1])
		}
	}
}

// The why adds what the story lacks: a dependency's why says what the card
// cannot do without it, a content filter's why says why the card waits for
// a human instead of sending the request again, and a spend stop's why
// names the rule, not the spend the money line names.
func TestTheWhyAddsWhatTheStoryLacks(t *testing.T) {
	for kind, want := range map[factoryd.CaseKind]string{
		factoryd.CaseContentFilter:    "The factory does not send a refused request again by itself.",
		factoryd.CaseCheckinShare:     "A session may spend only a share of the card's money before a human looks.",
		factoryd.CaseDependencyFailed: "This card needs the work of #2189 and cannot go on without it.",
		factoryd.CaseDependencyClosed: "This card needs the work of #2188 and cannot go on without it.",
	} {
		if got := fixture(t, "case", string(kind)).face.Why; got != want {
			t.Errorf("%s: why %q, want %q", kind, got, want)
		}
	}
}

// A plan's wait on its failed card tells the plan's story: the plan it
// plans, its sessions and the card it waits for, and its cards still to
// merge.
func TestAPlansWaitTellsThePlansStory(t *testing.T) {
	x := fixture(t, facefixtures.GroupVariant, "dependency_failed as a plan's wait")
	want := factoryd.Story{Planned: "This card plans “Board export: document the snapshot format”.",
		Happened: "It spent 1.12 of 2.00 EUR in 1 session. #2189, which it waits for, failed.",
		Left:     "2 of its 3 cards are merged."}
	if got := x.face.Story; got.Planned != want.Planned || got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("the story is\n %q\n %q\n %q\nwant\n %q\n %q\n %q", got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
	}
}

// The last session's line counts a gate's findings only where that gate
// judged the new head: the review's two findings before it count once the
// review judged the new head; the audit's eight, which the audit has not
// judged on the new head, count as nothing. The left line alone names the
// gates that have not checked the newest change, in one phrase, and says
// no finding is open once none is, the checks red or green.
func TestTheLastSessionClaimsNoFixItsGateHasNotJudged(t *testing.T) {
	ids := func(prefix string, n int) []string {
		var out []string
		for i := range n {
			out = append(out, factoryd.FindingID(prefix+string(rune('a'+i))))
		}
		return out
	}
	review, audit := ids("review ", 2), ids("audit ", 8)
	c := factoryd.Case{Kind: factoryd.CaseSessionCap}
	f := factoryd.Facts{CaseFacts: factoryd.CaseFacts{Kind: factoryd.CaseSessionCap, Attempts: 3, SessionCap: 3, BudgetEUR: 2, SpentEUR: 1.5, LeftEUR: 0.5,
		FloorEUR: 0.3, CeilingEUR: 18, Head: "h2", Checks: "green", Findings: review[1:], ReviewFindings: review[1:]},
		Card: factoryd.Task{Kind: "agent", Objective: "Import a snapshot back"}}
	for _, tc := range []struct {
		judged     []string
		happened   string
		left       string
		findingsOn bool
	}{
		{[]string{"review"}, "It spent 1.50 of 2.00 EUR in all 3 sessions it was given. Before the last session, 2 review findings were open.",
			"1 finding is open. The audit has not checked the newest change yet. One more session costs about 0.40 EUR.", true},
		{nil, "It spent 1.50 of 2.00 EUR in all 3 sessions it was given.",
			"The checks are green, and the review and the audit have not checked the newest change yet. One more session costs about 0.40 EUR.", false},
	} {
		g := f
		g.LastSession = &factoryd.LastSessionFacts{CostEUR: 0.4, Pushed: true, Judged: tc.judged, Review: review, Audit: audit}
		if !tc.findingsOn {
			g.Findings, g.ReviewFindings = nil, nil
		}
		s := factoryd.FaceOf(c, g).Story
		if s.Happened != tc.happened || s.Left != tc.left {
			t.Errorf("judged by %v:\n happened %q\n left     %q\nwant\n happened %q\n left     %q", tc.judged, s.Happened, s.Left, tc.happened, tc.left)
		}
	}
	g := f
	g.Findings, g.ReviewFindings, g.Checks = nil, nil, "red"
	g.LastSession = &factoryd.LastSessionFacts{CostEUR: 0.4, Pushed: true, Judged: []string{"review"}, Review: review}
	if s, happened, left := factoryd.FaceOf(c, g).Story, "It spent 1.50 of 2.00 EUR in all 3 sessions it was given. Before the last session, 2 review findings were open.",
		"No finding is open, and the checks failed. One more session costs about 0.40 EUR."; s.Happened != happened || s.Left != left {
		t.Errorf("every finding gone: %q / %q, want %q / %q", s.Happened, s.Left, happened, left)
	}
	g.Checks, g.LastSession.Audit = "green", audit
	if s, left := factoryd.FaceOf(c, g).Story, "No finding is open, and the checks are green. The audit has not checked the newest change yet. One more session costs about 0.40 EUR."; s.Left != left {
		t.Errorf("every finding gone, the checks green and the audit unjudged: %q, want %q", s.Left, left)
	}
	g = f
	g.LastSession = &factoryd.LastSessionFacts{Pushed: true, Judged: []string{"review"}, Audit: audit}
	s := factoryd.FaceOf(c, g).Story
	if happened, left := "It spent 1.50 of 2.00 EUR in all 3 sessions it was given.", "1 finding is open. The audit has not checked the newest change yet."; s.Happened != happened || s.Left != left {
		t.Errorf("the audit's findings unjudged: %q / %q, want %q / %q", s.Happened, s.Left, happened, left)
	}
}

// The case writer writes only what the facts cannot: what stopped the card
// and what is left, in words. The card puts the money line before the
// writer's stop and the findings and the price after its words.
func TestTheWritersWordsStandBesideTheNumbers(t *testing.T) {
	x := fixture(t, facefixtures.GroupCase, string(factoryd.CaseCheckinHuman))
	c := *x.fx.Case
	c.Brief, c.BriefDetail = "The check found that the card keeps the snapshots in Postgres, which the plan rules out.", "Where the snapshots live is still open."
	face := factoryd.CaseFace(c, x.fx.Task, *x.fx.Ask)
	want := factoryd.Story{Planned: "This card builds “Board export: keep the nightly snapshots in Postgres”.",
		Happened: "It spent 1.12 of 2.00 EUR in 1 session. The check found that the card keeps the snapshots in Postgres, which the plan rules out.",
		Left:     "Where the snapshots live is still open. No finding is open, and the checks are green. One more session costs about 0.48 EUR."}
	if got := face.Story; got.Planned != want.Planned || got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("the story is\n %q\n %q\n %q\nwant\n %q\n %q\n %q", got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
	}
	// a face stored after the writer wrote, as a refused press or a re-ask
	// stores it, shows the writer's lines once
	stored := maps.Clone(x.fx.Ask.Detail)
	maps.Copy(stored, factoryd.FaceKeys(factoryd.FaceOf(c, x.facts)))
	if got := factoryd.CaseFace(c, x.fx.Task, factoryd.CaseAsk{At: x.fx.Ask.At, Detail: stored}).Story; got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("stored after the writer wrote, the story is\n %q\n %q\nwant\n %q\n %q", got.Happened, got.Left, want.Happened, want.Left)
	}
}

// The writer's left line, within its bound, stands whole beside the
// longest left line the card composes: 87 runes, a space and 152 make the
// story's 240. A row stored with the writer's lines in it, as the base
// stored them, shows each line once.
func TestTheWritersLeftStandsWholeBesideTheNumbers(t *testing.T) {
	var ids []string
	for i := range 999 {
		ids = append(ids, factoryd.FindingID(fmt.Sprint(i)))
	}
	l := "Where the nightly snapshots live is open: Postgres, or the object store the plan names."
	c := factoryd.Case{Kind: factoryd.CaseCheckinHuman, BriefDetail: l}
	f := factoryd.Facts{CaseFacts: factoryd.CaseFacts{Kind: factoryd.CaseCheckinHuman, Attempts: 9, BudgetEUR: 20000, SpentEUR: 14, LeftEUR: 19986, FloorEUR: 0.3,
		CeilingEUR: 40000, Head: "h2", Checks: "red", Findings: ids, ReviewFindings: ids,
		LastSession: &factoryd.LastSessionFacts{CostEUR: 9999.99, Pushed: true, Audit: ids[:1]}},
		Card: factoryd.Task{Kind: "agent", Objective: "Keep the nightly snapshots"}}
	got := factoryd.CaseFace(c, f.Card, factoryd.CaseAsk{Detail: factoryd.FaceKeys(factoryd.FaceOf(c, f))}).Story.Left
	want := l + " 999 findings are open, and the checks failed. The review and the audit have not checked the newest change yet. One more session costs about 9999.99 EUR."
	if got != want || utf8.RuneCountInString(l) != 87 || utf8.RuneCountInString(got) != 240 {
		t.Errorf("left\n %q\nwant\n %q", got, want)
	}
	h, l := "The check found that the card keeps the snapshots in Postgres.", "Where the snapshots live is still open."
	c.Brief, c.BriefDetail = h, l
	stored := factoryd.FaceKeys(factoryd.FaceOf(c, f))
	stored["story"] = factoryd.Story{Planned: "This card builds “Keep the nightly snapshots”.", Happened: h, Left: l}
	if s := factoryd.CaseFace(c, f.Card, factoryd.CaseAsk{Detail: stored}).Story; s.Happened != h || s.Left != l {
		t.Errorf("a base row shows %q / %q, want %q / %q", s.Happened, s.Left, h, l)
	}
}

// An issue line that says what to press names only answers the board shows
// for the case, by their labels, as the face composes them: every quoted
// answer of every case fixture's line is one of its face's answers, never
// one of More, which the board does not show. A findings stop with a
// session left runs too. No line names the old forms.
func TestTheIssueNamesOnlyTheBoardsAnswers(t *testing.T) {
	quoted := regexp.MustCompile(`“([^”]+)”`)
	named := map[string]bool{}
	xs := facedFixtures(t)
	for _, x := range xs {
		if x.c.Kind != factoryd.CaseFindingsRepeated {
			continue
		}
		x.c.Facts = maps.Clone(x.c.Facts)
		x.c.Facts["attempts"] = 1
		x.facts.Attempts = 1
		x.face = factoryd.FaceOf(x.c, x.facts)
		x.fx.Kind += " with a session left"
		xs = append(xs, x)
	}
	for _, x := range xs {
		reason, _ := x.c.Facts["reason"].(string)
		line := factoryd.TripNext(x.c, reason)
		var labels []string
		for _, a := range x.face.Answers {
			labels = append(labels, a.Label)
		}
		for _, m := range quoted.FindAllStringSubmatch(line, -1) {
			named[m[1]] = true
			if !slices.Contains(labels, m[1]) {
				t.Errorf("%s/%s: the issue names %q, which its face %v does not show:\n%s", x.fx.Group, x.fx.Kind, m[1], labels, line)
			}
		}
		for _, old := range []string{"Fund form", "Fund on the board", "Extend"} {
			if strings.Contains(line, old) {
				t.Errorf("%s/%s: the issue names %q:\n%s", x.fx.Group, x.fx.Kind, old, line)
			}
		}
	}
	for _, label := range []string{"Give more room", "Overrule the review", "Start a fresh card on this work"} {
		if !named[label] {
			t.Errorf("no fixture's issue line names %q", label)
		}
	}
}
