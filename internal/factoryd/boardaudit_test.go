package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// downRefs is briefRefs whose reads failed.
type downRefs struct{ briefRefs }

func (downRefs) Err() error { return errors.New("forgejo answered 502") }

// storedFaceRow is a question_asked row that stores its face: the ask, its
// why line and the answers by their labels.
func storedFaceRow(ask, why string, labels ...string) map[string]any {
	offers := []any{}
	for _, l := range labels {
		offers = append(offers, map[string]any{"value": l, "label": l})
	}
	return map[string]any{"question": ask, "ask": ask, "why": why, "story": map[string]any{}, "offers": offers}
}

// The board of 2026-11-16 as the spec names it: #2100 queued in a plan
// under its outcome title with no why line, #2101 a plan's card for a
// human whose ask is a statement over 110 characters with one button,
// #2004 out of money under a title of two statements with more buttons
// than four, and #2090 a human's own issue moved into the plan with no why
// line, which keeps its text as written. A helper's ask with one button
// counts on its card, #2100, and on #2110, a finished card the board shows
// for its helper's ask. The same board with the accepted briefs, the
// register's faces and a request no case holds has no finding. A read the
// audit could not make fails it, naming what it read.
func TestAuditBoardFindsTheNamedFaults(t *testing.T) {
	ctx := context.Background()
	dsn := schematest.DSN(t, "boardaudit")
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// the audit reads alone: a write would fail on this store
	ro, err := OpenStoreReadOnly(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	create := func(task Task) {
		t.Helper()
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
	}
	ask := func(task Task, detail map[string]any) {
		t.Helper()
		if _, err := s.Record(ctx, Event{Repository: task.Repository, TaskID: task.ID, IssueNumber: task.IssueNumber, Kind: EventQuestionAsked,
			Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}); err != nil {
			t.Fatalf("%s: ask: %v", task.ID, err)
		}
	}
	board := func(repo, prefix string) (plan, c2100, c2101, c2004, c2090 Task) {
		t.Helper()
		plan = Task{ID: prefix + "-plan", Kind: "agent", Track: policy.TrackPlan, Status: "done", Repository: repo, IssueNumber: 1947,
			RiskClass: "standard", Objective: "Fire the gate on an empty commit"}
		create(plan)
		if _, err := s.AddPlanRevision(ctx, plan.ID, mustJSON(Plan{}), "r1"); err != nil {
			t.Fatal(err)
		}
		card := func(n int64, kind, status, source string) Task {
			c := Task{ID: fmt.Sprintf("%s-%d", prefix, n), Kind: kind, Status: status, Repository: repo, IssueNumber: n,
				Source: source, Parent: plan.ID, RiskClass: "standard"}
			if kind == "agent" {
				c.Track = policy.TrackDirect
			}
			return c
		}
		return plan, card(2100, "agent", "queued", SourcePlan), card(2101, "human", "waiting-human", SourcePlan),
			card(2004, "agent", "waiting-human", SourcePlan), card(2090, "agent", "queued", SourceWebhook)
	}

	_, c2100, c2101, c2004, c2090 := board("sf/live", "live")
	c2100.Objective = title2100
	c2101.Objective, c2101.Why = title2101, "The case is the human card."
	c2004.Objective = title2004
	c2090.Objective = "Board export: keep the nightly snapshots\n\n## Context\n\nThe export writes one snapshot a night."
	for _, c := range []Task{c2100, c2101, c2004, c2090} {
		create(c)
	}
	ask(c2101, storedFaceRow("Is the work of this card done?", c2101.Why, "Done"))
	c2110 := Task{ID: "live-2110", Kind: "agent", Track: policy.TrackDirect, Status: "done", Repository: "sf/live", IssueNumber: 2110, Source: SourcePlan,
		Parent: "live-plan", RiskClass: "standard", Objective: "Count the hits on the stats page", Why: "So a human sees which links people use.",
		Summary: "The stats page counts each link's hits. A test opens two links and reads the counts."}
	create(c2110)
	for _, h := range []struct {
		card       Task
		source, at string
	}{{c2100, SourceSizing, "Size the card again?"}, {c2110, SourceProver, "Read the proof again?"}} {
		helper := Task{ID: h.card.ID + "-" + h.source, Kind: "agent", Track: policy.TrackDirect, Status: "running", Repository: "sf/live",
			Source: h.source, Parent: h.card.ID, RiskClass: "standard", Objective: h.at}
		create(helper)
		ask(helper, storedFaceRow(h.at, "The helper waits for a human.", "Not now"))
	}
	ask(c2004, storedFaceRow("Fund it, or park it?", "The card ran out of money with review findings open.",
		"Add money", "Park it", "Successor", "Confirm audit", "End the card"))

	const (
		verb    = "A work title starts with its verb, for example 'Narrow the close refusal to plans that hold criteria'."
		why     = "The why line gives the reason in one sentence: who needs this, or which problem it removes."
		summary = "The summary says what gets built, what it changes and how it is checked, under 600 characters."
	)
	want := []BoardFinding{
		{Issue: 2004, Field: "title", Refusal: plainWords(": priced, asked, funded")},
		{Issue: 2004, Field: "title", Refusal: verb},
		{Issue: 2004, Field: "why", Refusal: why},
		{Issue: 2004, Field: "summary", Refusal: summary},
		{Issue: 2004, Field: "answers", Refusal: `An ask offers 2 to 4 answers, and "Fund it, or park it?" offers 5: Add money, Park it, Successor, Confirm audit, End the card.`},
		{Issue: 2090, Field: "why", Refusal: "A card of a plan has no why line. Write in one sentence why the plan needs it.", HumanIssue: true},
		{Issue: 2100, Field: "title", Refusal: "The title has 105 characters. Keep it under 70."},
		{Issue: 2100, Field: "title", Refusal: plainWords("card/2084")},
		{Issue: 2100, Field: "title", Refusal: plainWords("doClose")},
		{Issue: 2100, Field: "title", Refusal: plainWords(": the review finding answered on branch card/2084")},
		{Issue: 2100, Field: "title", Refusal: verb},
		{Issue: 2100, Field: "why", Refusal: why},
		{Issue: 2100, Field: "summary", Refusal: summary},
		{Issue: 2100, Field: "answers", Refusal: `An ask offers 2 to 4 answers, and "Size the card again?" offers 1: Not now.`},
		{Issue: 2101, Field: "title", Refusal: "The ask has 111 characters. Keep it under 110."},
		{Issue: 2101, Field: "title", Refusal: plainWords(": the red goes green and the merge records the six verdicts")},
		{Issue: 2101, Field: "title", Refusal: "An ask is one question or one instruction."},
		{Issue: 2101, Field: "answers", Refusal: `An ask offers 2 to 4 answers, and "Is the work of this card done?" offers 1: Done.`},
		{Issue: 2110, Field: "answers", Refusal: `An ask offers 2 to 4 answers, and "Read the proof again?" offers 1: Not now.`},
	}
	green := func(context.Context, string, string) (bool, error) { return true, nil }
	got, err := AuditBoard(ctx, ro, "sf/live", briefRefs{}, green)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the live board's findings:\n%s\nwant:\n%s", findingLines(got), findingLines(want))
	}
	if _, err := AuditBoard(ctx, ro, "sf/live", downRefs{}, green); err == nil || !strings.Contains(err.Error(), "the references of #") {
		t.Errorf("a read of the references that failed = %v, want the audit failed naming the card", err)
	}

	plan, c2100, c2101, c2004, c2090 := board("sf/accepted", "ok")
	c2100.Objective, c2100.Why, c2100.Summary = acceptedBrief.Title, acceptedBrief.Why, acceptedBrief.Summary
	c2101.Objective, c2101.Why = "Change two lines in the runner's build files\n\nEdit Dockerfile.runner and ci.yml.",
		"The factory's push rule keeps agents out of workflow files, so a human does this card."
	c2004.Objective, c2004.Why, c2004.Summary = "Turn the three money fixes into end-to-end tests", "So a budget stop asks the price it needs.",
		"Three acceptance tests drive a card into its budget stop. Each checks the price the stop asks and the fund that clears it."
	c2090.Objective, c2090.Why = "Board export: keep the nightly snapshots", "The nightly snapshots fill the disk."
	for _, c := range []Task{c2100, c2004, c2090} {
		create(c)
	}
	if _, _, err := s.CreateHumanCard(ctx, c2101, PlanHumanAsk(plan, CardTitle(c2101))); err != nil {
		t.Fatal(err)
	}
	ask(c2004, map[string]any{"question": "Add 2.00 EUR to finish the card, or end it?", "source": "budget"})
	if _, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalResource, TaskID: c2100.ID, Project: "sf/accepted", Spec: json.RawMessage(`{}`),
		Reason: "The card needs a server for its load test."}); err != nil {
		t.Fatal(err)
	}
	got, err = AuditBoard(ctx, ro, "sf/accepted", briefRefs{}, green)
	if err != nil || len(got) > 0 {
		t.Errorf("the accepted board's findings:\n%s\nerror %v, want none", findingLines(got), err)
	}
	desk, err := s.CaseDesk(ctx, "sf/accepted")
	if err != nil || len(desk.Cases) != 2 || slices.ContainsFunc(desk.Cases, func(c Case) bool { return c.Status != CaseAtHuman }) {
		t.Fatalf("the accepted board holds the cases %+v, %v; want the asks of #2101 and #2004 at the human", desk.Cases, err)
	}

	broken := Task{ID: "broken-3001", Kind: "agent", Track: policy.TrackDirect, Status: "in-review", Repository: "sf/broken", IssueNumber: 3001,
		Source: SourcePlan, RiskClass: "standard", Objective: acceptedBrief.Title, Why: acceptedBrief.Why, Summary: acceptedBrief.Summary}
	create(broken)
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: broken.ID, Project: "sf/broken", PRNumber: 7, Spec: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuditBoard(ctx, ro, "sf/broken", briefRefs{}, green); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("approval request %d: spec", id)) {
		t.Errorf("a request whose spec does not read = %v, want the audit failed naming the request", err)
	}
}

func findingLines(fs []BoardFinding) string {
	var out string
	for _, f := range fs {
		out += fmt.Sprintf("  #%d %s %v: %s\n", f.Issue, f.Field, f.HumanIssue, f.Refusal)
	}
	return out
}
