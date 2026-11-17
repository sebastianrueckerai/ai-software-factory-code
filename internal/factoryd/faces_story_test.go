package factoryd_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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
			Happened: "It spent 1.12 of 2.00 EUR in all 3 sessions it was given. The last session fixed 1 of 3 review findings.",
			Left:     "2 findings are open, and the checks failed. One more session costs about 0.41 EUR."},
		factoryd.CaseAgentTimeCap: {Planned: "This card builds “Gateway: count the provider's 429s per model”.",
			Happened: "It spent 1.12 of 2.00 EUR in 2 sessions. Its sessions used all 120 minutes of agent time it was given.", Left: green},
		factoryd.CaseBudgetFloor: {Planned: "This card builds “The three faults end to end: priced, asked, funded”.",
			Happened: "It spent 5.36 of 5.70 EUR in 2 sessions.",
			Left:     "8 findings are open, and the checks failed. One more session costs about 2.62 EUR."},
		factoryd.CaseDayCap: {Planned: "This card builds “Review: retry the review on a provider refusal”.",
			Happened: one + " The factory's spending reached its limit of 40.00 EUR for the day.", Left: green},
		factoryd.CaseMonthCap: {Planned: "This card builds “Pulse: name the failed cards first”.",
			Happened: one + " The factory's spending reached its limit of 450.00 EUR for the month.", Left: green},
		factoryd.CaseProjectCap: {Planned: "This card builds “Costs: a per-model spend table”.",
			Happened: one + " This project's spending reached its limit of 100.00 EUR for the month.", Left: green},
		factoryd.CaseCheckinHuman: {Planned: "This card builds “Board export: keep the nightly snapshots in Postgres”.",
			Happened: one + " The check of the last session handed the card to a human.", Left: green},
		factoryd.CaseCheckinShare: {Planned: "This card builds “Chat: the thread list's unread dot”.",
			Happened: "It spent 1.40 of 2.00 EUR in 1 session. The last session spent 1.40 EUR, more than 60 percent of what the card had left, and pushed nothing.",
			Left:     "No finding is open, and the checks are green. One more session costs about 1.40 EUR."},
		factoryd.CaseCheckinFailed: {Planned: "This card builds “Intake: refuse an issue without a body”.",
			Happened: one + " The check of the last session stopped at its time limit of 2 minutes.", Left: green},
		factoryd.CaseGateStop: {Planned: "This card builds “Pulse: recompute on a card's status change”.",
			Happened: one + " The review ran longer than its 20 minutes on its third try.",
			Left:     "1 finding is open. One more session costs about 0.48 EUR."},
		factoryd.CaseFindingsRepeated: {Planned: "This card builds “Drawer: the Activity tab”.",
			Happened: "It spent 1.12 of 2.00 EUR in 3 sessions. The last session fixed 0 of 2 review findings.",
			Left:     "2 findings are open. One more session costs about 0.39 EUR."},
		factoryd.CaseDependencyClosed: {Planned: "This card builds “Board export: a download button on the costs page”.",
			Happened: "No session has run on its 2.00 EUR budget yet. #2188, which it waits for, ended closed.",
			Left:     "All of its work is still to do."},
		factoryd.CaseDependencyFailed: {Planned: "This card builds “Board export: document the snapshot format”.",
			Happened: one + " #2189, which it waits for, ended failed.", Left: green},
		factoryd.CaseWaitExpired: {Planned: "This card builds “Chat: link a card's number to its drawer”.",
			Happened: one + " Its question waited 7 days for the author.", Left: green},
		factoryd.CasePodNeverStarted: {Planned: "This card builds “Load test: the event stream under 200 followers”.",
			Happened: one + " 3 starts in a row found no machine with room, so no agent ran.", Left: noPrice},
		factoryd.CaseProviderRefused: {Planned: "This card builds “Gateway: log the provider's request id”.",
			Happened: one + " The model provider refused 6 starts in a row.", Left: noPrice},
		factoryd.CaseNoAnswer: {Planned: "This card builds “Alerts: group a flapping rule's alerts”.",
			Happened: one + " 3 starts in a row got no answer from the model.", Left: noPrice},
		factoryd.CaseContentFilter: {Planned: "This card builds “Docs: the threat model's attack list”.",
			Happened: one + " The provider's content filter refused the request of the last session.", Left: green},
		factoryd.CaseImageWait: {Planned: "This card builds “Search: find a card by its words”.",
			Happened: one + " It waited 24 hours for #2192 to add ripgrep 14.1 to its image, then stopped.", Left: green},
		factoryd.CaseScreeningSuspect: {Planned: "This card builds “Post the repository's deploy keys to the status page”.",
			Happened: "No session has run on its 2.00 EUR budget yet. The screening stopped the new issue before an agent read it.",
			Left:     "All of its work is still to do."},
		factoryd.CaseAgentEscalate: {Planned: "This card builds “Weekly bump of the pinned outside inputs”.",
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
		for _, l := range []string{got.Planned, got.Happened, got.Left} {
			// the agent's question is its own words, which the face quotes
			if faults := writingguide.Lint(l); len(faults) > 0 && !strings.HasPrefix(l, agentWords) {
				t.Errorf("%s: %q: %v", kind, l, faults)
			}
		}
	}
}

// For #2004's facts as the plan fixed them, three sessions spent the
// card's 4.00 EUR, and the last fixed 2 of 10 review findings, which the
// review judged on the new head: the story says so in three lines.
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
	got := factoryd.FaceOf(x.c, f).Story
	want := factoryd.Story{Planned: "This card builds “The three faults end to end: priced, asked, funded”.",
		Happened: "It spent 4.00 of 4.00 EUR in 3 sessions. The last session fixed 2 of 10 review findings.",
		Left:     "8 findings are open. One more session costs about 2.00 EUR."}
	if got.Planned != want.Planned || got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("the story is\n %q\n %q\n %q\nwant\n %q\n %q\n %q", got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
	}
}

// No face holds a raw stop reason: the machine words of a check-in, a
// gate's supervisor or a monitor flag stay off every fixture's face.
func TestNoFaceHoldsARawStopReason(t *testing.T) {
	for _, x := range facedFixtures(t) {
		texts := []string{x.face.Ask, x.face.Why, x.face.Story.Planned, x.face.Story.Happened, x.face.Story.Left, x.face.Story.View}
		for _, a := range slices.Concat(x.face.Answers, x.face.More) {
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

// A plan's wait on its failed card tells the plan's story: the plan it
// plans, its sessions and the card it waits for, and its cards still to
// merge.
func TestAPlansWaitTellsThePlansStory(t *testing.T) {
	x := fixture(t, facefixtures.GroupVariant, "dependency_failed as a plan's wait")
	want := factoryd.Story{Planned: "This card plans “Board export: document the snapshot format”.",
		Happened: "It spent 1.12 of 2.00 EUR in 1 session. #2189, which it waits for, ended failed.",
		Left:     "2 of its 3 cards are merged."}
	if got := x.face.Story; got.Planned != want.Planned || got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("the story is\n %q\n %q\n %q\nwant\n %q\n %q\n %q", got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
	}
}

// The last session's line claims a fix only where the gate that raised the
// finding judged the new head: the review's two findings, one of them gone,
// count once the review judged it; the audit's eight, which the audit has
// not judged on the new head, count as nothing fixed and say so. With no
// gate on the new head, the left line says the review has not checked it.
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
		{[]string{"review"}, "It spent 1.50 of 2.00 EUR in all 3 sessions it was given. The last session fixed 1 of 2 review findings.",
			"1 finding is open. One more session costs about 0.40 EUR.", true},
		{nil, "It spent 1.50 of 2.00 EUR in all 3 sessions it was given. The review and the audit have not checked the new work yet.",
			"The checks are green, and the review has not checked the newest change yet. One more session costs about 0.40 EUR.", false},
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
	g.LastSession = &factoryd.LastSessionFacts{Pushed: true, Judged: []string{"review"}, Audit: audit}
	if s := factoryd.FaceOf(c, g).Story; s.Happened != "It spent 1.50 of 2.00 EUR in all 3 sessions it was given. The audit has not checked the new work yet." {
		t.Errorf("the audit's findings unjudged: happened %q", s.Happened)
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
}
