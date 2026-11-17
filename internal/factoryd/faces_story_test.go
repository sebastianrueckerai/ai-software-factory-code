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
	one := "It spent 1.12 of 2.00 EUR in 1 session."
	for kind, want := range map[factoryd.CaseKind]factoryd.Story{
		factoryd.CaseSessionCap: {Planned: "This card builds “Board export: import a snapshot back”.",
			Happened: "It spent 1.12 of 2.00 EUR in 3 sessions. The last session fixed 1 of 3 review findings.",
			Left:     "2 findings are open, and the checks failed. One more session costs about 0.41 EUR."},
		factoryd.CaseAgentTimeCap: {Planned: "This card builds “Gateway: count the provider's 429s per model”.",
			Happened: "It spent 1.12 of 2.00 EUR in 2 sessions.", Left: green},
		factoryd.CaseBudgetFloor: {Planned: "This card builds “The three faults end to end: priced, asked, funded”.",
			Happened: "It spent 5.36 of 5.70 EUR in 2 sessions.",
			Left:     "8 findings are open, and the checks failed. One more session costs about 2.62 EUR."},
		factoryd.CaseDayCap:     {Planned: "This card builds “Review: retry the review on a provider refusal”.", Happened: one, Left: green},
		factoryd.CaseMonthCap:   {Planned: "This card builds “Pulse: name the failed cards first”.", Happened: one, Left: green},
		factoryd.CaseProjectCap: {Planned: "This card builds “Costs: a per-model spend table”.", Happened: one, Left: green},
		factoryd.CaseCheckinHuman: {Planned: "This card builds “Board export: keep the nightly snapshots in Postgres”.",
			Happened: one, Left: green},
		factoryd.CaseCheckinShare: {Planned: "This card builds “Chat: the thread list's unread dot”.",
			Happened: "It spent 1.40 of 2.00 EUR in 1 session.", Left: green},
		factoryd.CaseCheckinFailed: {Planned: "This card builds “Intake: refuse an issue without a body”.",
			Happened: one + " The check of the last session stopped at its time limit of 2 minutes.", Left: green},
		factoryd.CaseGateStop: {Planned: "This card builds “Pulse: recompute on a card's status change”.",
			Happened: one + " The review ran past its 20 minutes on try 3.",
			Left:     "1 finding is open. One more session costs about 0.48 EUR."},
		factoryd.CaseFindingsRepeated: {Planned: "This card builds “Drawer: the Activity tab”.",
			Happened: "It spent 1.12 of 2.00 EUR in 3 sessions. The last session fixed 0 of 2 review findings.",
			Left:     "2 findings are open. One more session costs about 0.39 EUR."},
		factoryd.CaseDependencyClosed: {Planned: "This card builds “Board export: a download button on the costs page”.",
			Happened: "No session has run on its 2.00 EUR budget yet. The card it waits for, #2188, builds “Board export: write the nightly snapshot”.",
			Left:     "All of its work is still to do."},
		factoryd.CaseDependencyFailed: {Planned: "This card builds “Board export: document the snapshot format”.",
			Happened: one + " The card it waits for, #2189, builds “Board export: define the snapshot's JSON”.", Left: green},
		factoryd.CaseWaitExpired: {Planned: "This card builds “Chat: link a card's number to its drawer”.",
			Happened: one + " Its question waited 7 days for the author.", Left: green},
		factoryd.CasePodNeverStarted: {Planned: "This card builds “Load test: the event stream under 200 followers”.",
			Happened: one + " The last 3 sessions found no machine with room, so no agent ran.", Left: green},
		factoryd.CaseProviderRefused: {Planned: "This card builds “Gateway: log the provider's request id”.",
			Happened: one + " The model provider refused the first call of the last 6 sessions.", Left: green},
		factoryd.CaseNoAnswer: {Planned: "This card builds “Alerts: group a flapping rule's alerts”.",
			Happened: one + " The last 3 sessions got no answer from the model.", Left: green},
		factoryd.CaseContentFilter: {Planned: "This card builds “Docs: the threat model's attack list”.", Happened: one, Left: green},
		factoryd.CaseImageWait: {Planned: "This card builds “Search: find a card by its words”.",
			Happened: one + " It has waited 24 hours for #2192 to add ripgrep 14.1 to its image.", Left: green},
		factoryd.CaseScreeningSuspect: {Planned: "This card builds “Post the repository's deploy keys to the status page”.",
			Happened: "No session has run on its 2.00 EUR budget yet. The screening stopped the new issue before an agent read it.",
			Left:     "All of its work is still to do."},
		factoryd.CaseAgentEscalate: {Planned: "This card builds “Weekly bump of the pinned outside inputs”.",
			Happened: "The agent asks: The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, " +
				"registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), none of which resolve in this sandbox. " +
				"How should the new digests, sha256 sums and versions be obtained?",
			Left: "No change is pushed yet. One more session costs about 0.48 EUR."},
		factoryd.CaseAgentRequestInfo: {Planned: "This card builds “Load test: a fresh server per run”.",
			Happened: one + " What the agent tried: the load test's README and the project's environment file, which name no location.",
			Left:     "What it still needs: the location the server should run in."},
		factoryd.CaseOther:       {Planned: "This card builds “Console: show the configuration version”.", Happened: one, Left: green},
		factoryd.CasePolicyClass: {Planned: "This card builds “Policy: a project may lower its pool”.", Happened: one, Left: green},
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
// card's 4.00 EUR, and the last fixed 2 of 10 review findings: the story
// says so in three lines.
func TestTheStoryOf2004(t *testing.T) {
	x := fixture(t, facefixtures.GroupCase, string(factoryd.CaseBudgetFloor))
	f := x.facts
	f.BudgetEUR, f.SpentEUR, f.LeftEUR, f.Attempts, f.Checks = 4, 4, 0, 3, "green"
	var before []string
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		before = append(before, factoryd.FindingID("finding "+s))
	}
	f.Findings, f.ReviewFindings, f.AuditFindings = before[2:], before[2:], nil
	f.LastSession = &factoryd.LastSessionFacts{CostEUR: 2, Pushed: true, Review: before}
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
		Happened: "It spent 1.12 of 2.00 EUR in 1 session. The card it waits for, #2189, builds “Board export: define the snapshot's JSON”.",
		Left:     "2 of its 3 cards are merged."}
	if got := x.face.Story; got.Planned != want.Planned || got.Happened != want.Happened || got.Left != want.Left {
		t.Errorf("the story is\n %q\n %q\n %q\nwant\n %q\n %q\n %q", got.Planned, got.Happened, got.Left, want.Planned, want.Happened, want.Left)
	}
}
