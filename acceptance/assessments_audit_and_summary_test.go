package acceptance

// assessments_audit_and_summary (roadmap 66): the receipt's assessments
// section carries every model assessment labelled by source — the
// review's verdict with its summary, the audit's verdict, each judgement
// criterion's verdict with its evidence — and none of them among the
// facts. The records are seeded in the real store beside the suite's
// throwaway Postgres, and the expected entries are derived again from
// those records alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// accAssessmentStep is one timeline record's assessment fields as anyone
// can read them.
type accAssessmentStep struct {
	Step    string `json:"step"`
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Note    string `json:"note"`
}

// accAssessmentsFromRecords reads the task's timeline record and verdict
// rows as anyone can and answers the assessments they hold: one entry per
// record, the review's carrying its summary, the audit's its verdict, the
// judgement rows their criterion and evidence.
func accAssessmentsFromRecords(ctx context.Context, t *testing.T, objects *accObjects, s *factoryd.PGStore, id string) []factoryd.ReceiptAssessment {
	t.Helper()
	body, err := objects.Get(ctx, "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []factoryd.ReceiptAssessment
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		var e accAssessmentStep
		if json.Unmarshal(line, &e) != nil || e.Step == "" {
			continue
		}
		switch e.Step {
		case "model_review":
			out = append(out, factoryd.ReceiptAssessment{Source: factoryd.CriteriaSourceReview,
				Verdict: e.Verdict, Note: e.Summary})
		case "audit":
			out = append(out, factoryd.ReceiptAssessment{Source: "audit", Verdict: e.Verdict})
		}
	}
	return append(out, accReceiptAssessments(ctx, t, s, id)...)
}

// TestAssessmentsAuditAndSummary proves the receipt's assessments section
// beside its facts: exactly one labelled entry per assessment record, the
// review's entry carrying its summary, and none of the assessments'
// values among the facts.
func TestAssessmentsAuditAndSummary(t *testing.T) {
	ctx := context.Background()
	fj := serve(t, &accReceiptForgejo{head: "c1-head", mergeCommit: "m9", diff: accReceiptDiff})
	s, _ := accReceiptStore(t, "acceptance_receipt_assessments")
	objects := &accObjects{files: map[string][]byte{}}
	r := accReceiptRunner(t, s, fj, objects)
	const id = "ra1"
	if err := s.CreateTask(ctx, factoryd.Task{ID: id, Status: "merged", Objective: "the assessments proof's card",
		Repository: "sf/code", Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: 86,
		SHA: "c0-base", PRNumber: 9, RequiredCapabilities: []string{"coder"}}); err != nil {
		t.Fatal(err)
	}

	// the records: a model review holding a verdict, a note and a summary,
	// an audit verdict, and judgement criteria verdict rows
	tl := &factoryd.Timeline{Objects: objects}
	if err := tl.Append(ctx, id, "model_review", map[string]any{"pr": 9, "commit": "c1", "model": "factory/big",
		"verdict": "approve", "summary": "the change is a strict improvement, narrow and tested",
		"note": "the model's own words beside its verdict", "round": 1}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Append(ctx, id, "audit", map[string]any{"pr": 9, "commit": "c1", "model": "factory/big",
		"deployment": true, "verdict": "clear"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCriteriaVerdicts(ctx, id, factoryd.CriteriaSourceJudgement, factoryd.FactoryDecider, []factoryd.CriterionVerdict{
		{Criterion: "reads_calm", Verdict: "met", Evidence: "the page keeps its calm with 200 rows on it", Kind: factoryd.CriterionJudgement},
		{Criterion: "edits_safe", Verdict: "met", Evidence: "the edit path wrote every row it showed", Kind: factoryd.CriterionJudgement},
	}); err != nil {
		t.Fatal(err)
	}

	rec, err := r.BuildMergeReceipt(ctx, id, 9)
	if err != nil {
		t.Fatal(err)
	}

	// exactly one entry per record, each with its source label: the
	// review's, the audit's, then the judgement rows'
	want := accAssessmentsFromRecords(ctx, t, objects, s, id)
	same(t, "assessments", rec.Assessments, want)
	if len(rec.Assessments) != 4 {
		t.Fatalf("assessments = %+v, want one review, one audit and the two judgement rows' entries", rec.Assessments)
	}
	review, audit := rec.Assessments[0], rec.Assessments[1]
	if review.Source != factoryd.CriteriaSourceReview || audit.Source != "audit" ||
		rec.Assessments[2].Source != factoryd.CriteriaSourceJudgement || rec.Assessments[3].Source != factoryd.CriteriaSourceJudgement {
		t.Fatalf("the assessments' source labels = %+v, want review, audit, judgement, judgement", rec.Assessments)
	}

	// the review's entry carries the summary, not the note beside it
	if review.Verdict != "approve" || review.Note != "the change is a strict improvement, narrow and tested" {
		t.Errorf("the review's entry = %+v, want its record's verdict and summary", review)
	}
	if review.Note == "the model's own words beside its verdict" {
		t.Errorf("the review's entry carries the record's note %q, want its summary", review.Note)
	}
	if audit.Verdict != "clear" {
		t.Errorf("the audit's entry = %+v, want its record's verdict", audit)
	}
	for _, row := range []struct{ criterion, evidence string }{ // each judgement entry carries its row's criterion and evidence
		{"reads_calm", "the page keeps its calm with 200 rows on it"},
		{"edits_safe", "the edit path wrote every row it showed"},
	} {
		var a *factoryd.ReceiptAssessment
		for i := range rec.Assessments {
			if rec.Assessments[i].Source == factoryd.CriteriaSourceJudgement && rec.Assessments[i].Criterion == row.criterion {
				a = &rec.Assessments[i]
			}
		}
		if a == nil {
			t.Errorf("no judgement entry for the criterion %q among %+v", row.criterion, rec.Assessments)
			continue
		}
		if a.Evidence != row.evidence {
			t.Errorf("the judgement entry of %s = %+v, want its record's evidence %q", row.criterion, a, row.evidence)
		}
	}

	// none of the assessments' values sits among the facts
	facts, err := json.Marshal(rec.Facts)
	if err != nil {
		t.Fatal(err)
	}
	stray := []string{review.Source, audit.Source, factoryd.CriteriaSourceJudgement,
		review.Verdict, review.Note, "the model's own words beside its verdict", audit.Verdict,
		"reads_calm", "the page keeps its calm with 200 rows on it", "edits_safe", "the edit path wrote every row it showed"}
	for _, v := range stray {
		if strings.Contains(string(facts), v) {
			t.Errorf("the assessment value %q sits among the facts:\n%s", v, facts)
		}
	}
}
