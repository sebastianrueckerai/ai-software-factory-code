package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func wantSums(t *testing.T, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("series = %v, want %v", got, want)
		}
	}
}

// Finish counts the task's outcome once, labelled with its project and
// merge rule (the repository and human when no project row names it), the
// alias of its last session and its attempts, and records its spend in
// EUR by alias and outcome: merged for a done task with a pull request.
func TestFinishCountsTheOutcomeAndTheCost(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
	merged := prTask
	merged.Status, merged.PRNumber, merged.SpentEUR, merged.ModelAlias = "in-review", 7, 0.42, "coding-frontier"
	merged.Usage.Attempts = 2
	r, store, _ := newActivityRunner(t, merged, fj)
	m, reader := testMetrics(t)
	r.cfg.Metrics = m
	store.projects = []Project{{Name: "demo", Repository: "sf/repo", MergeRule: policy.MergeAuto}}
	for range 2 {
		if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
			t.Fatal(err)
		}
	}
	report := prTask
	report.ID, report.Status, report.SpentEUR, report.ModelAlias, report.Parent = "t-report", "running", 0.05, "coding-cheap", "t1"
	report.Usage.Attempts = 1
	store.CreateTask(context.Background(), report)
	if _, err := execActivity(t, r, ActivityFinish, "t-report", "done", "report posted as issue comment 3"); err != nil {
		t.Fatal(err)
	}
	roll := prTask
	roll.ID, roll.Status, roll.PRNumber, roll.Source = "t-roll", "in-review", 8, SourceRoll
	store.CreateTask(context.Background(), roll)
	if _, err := execActivity(t, r, ActivityFinish, "t-roll", "done", "pull request merged by factory-merge"); err != nil {
		t.Fatal(err)
	}
	store.projects = nil
	failed := prTask
	failed.ID, failed.Status, failed.SpentEUR, failed.ModelAlias = "t2", "running", 0.1, "coding-cheap"
	failed.Usage.Attempts = 3
	store.CreateTask(context.Background(), failed)
	if _, err := execActivity(t, r, ActivityFinish, "t2", "failed", "3 attempts exhausted"); err != nil {
		t.Fatal(err)
	}

	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=2,merge_rule=auto,model_alias=coding-frontier,project=demo,state=done":    1,
		"attempts=1,merge_rule=auto,model_alias=coding-cheap,project=demo,state=done":       1,
		"attempts=0,merge_rule=auto,model_alias=none,project=demo,state=done":               1,
		"attempts=3,merge_rule=human,model_alias=coding-cheap,project=sf/repo,state=failed": 1,
	})
	got := histograms(t, reader, "factory.task.cost")
	want := map[string]histPoint{
		"model_alias=coding-frontier,outcome=merged": {1, 0.42},
		"model_alias=coding-cheap,outcome=done":      {1, 0.05},
		"model_alias=none,outcome=merged":            {1, 0},
		"model_alias=coding-cheap,outcome=failed":    {1, 0.1},
	}
	if len(got) != len(want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("cost = %v, want %v", got, want)
		}
	}
}

// Every review answer counts once by verdict, strict_improvement and
// round; an answer that is no verdict (refused, failed, invalid) counts
// as none, and a roll's review, which asks no model, not at all.
func TestModelReviewCountsEachAnswer(t *testing.T) {
	m, reader := testMetrics(t)
	review := func(task Task, answer string, earlier ...Review) {
		t.Helper()
		r, _, _ := newGateRunner(t, task, &gateForgejo{diff: "diff --git a/x b/x\n+x\n"})
		r.cfg.Metrics = m
		r.supervisor = testSupervisorModel((&fakeLiteLLM{content: answer}).srv(t).URL)
		if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, earlier); err != nil {
			t.Fatal(err)
		}
	}
	concerns := Review{Verdict: "concerns", Summary: "s"}
	review(gateTask, `{"verdict":"concerns","summary":"no test","findings":["x: add a test"]}`)
	review(gateTask, `{"verdict":"approve","summary":"tested","strict_improvement":true}`, concerns)
	review(gateTask, `{"verdict":"approve","summary":"tested","strict_improvement":false}`, concerns, concerns)
	review(gateTask, "LGTM")
	roll := gateTask
	roll.Source = SourceRoll
	review(roll, `{"verdict":"approve","summary":"never asked"}`)

	wantSums(t, sums(t, reader, "factory.review.verdict"), map[string]float64{
		"round=1,strict_improvement=unset,verdict=concerns": 1,
		"round=2,strict_improvement=true,verdict=approve":   1,
		"round=3,strict_improvement=false,verdict=approve":  1,
		"round=1,strict_improvement=unset,verdict=none":     1,
	})
}

// A human's answer that raised the task's agent time counts one
// extra_time intervention; the same usage recorded again (a retried
// activity) and usage that only grew by the agent's own work count none.
func TestRecordUsageCountsATimeGrant(t *testing.T) {
	task := prTask
	task.Usage = TaskUsage{AgentTime: 18 * time.Minute, Attempts: 3}
	r, _, _ := newActivityRunner(t, task, &prForgejo{})
	m, reader := testMetrics(t)
	r.cfg.Metrics = m
	for _, u := range []TaskUsage{
		{AgentTime: 19 * time.Minute, Attempts: 3},
		{AgentTime: 19 * time.Minute, Attempts: 3, ExtraAttempts: 1, ExtraTime: 9 * time.Minute},
		{AgentTime: 19 * time.Minute, Attempts: 3, ExtraAttempts: 1, ExtraTime: 9 * time.Minute},
		{AgentTime: 25 * time.Minute, Attempts: 4, ExtraAttempts: 1, ExtraTime: 9 * time.Minute},
	} {
		if _, err := execActivity(t, r, ActivityRecordUsage, "t1", u); err != nil {
			t.Fatal(err)
		}
	}
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{"kind=extra_time": 1})
}

// A session records the alias of its model on the task: the last
// session's is the task's.
func TestSessionRecorderKeepsTheModelAlias(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "running"})
	for _, model := range []string{"factory/coding-cheap", "factory/coding-frontier"} {
		if _, err := NewSessionRecorder(context.Background(), store, &fakeObjectStore{}, "t1", "s-"+model, "", model); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.ModelAlias != "coding-frontier" {
		t.Fatalf("model alias = %q, want the last session's", got.ModelAlias)
	}
}

// A comment that answers a task waiting on a human counts once: a
// writer's on a task waiting for a human's answer is an escalation
// answer, the poster's on a task waiting for the poster a poster answer.
// A redelivery, an untrusted stranger and a comment on a running task
// count nothing.
func TestAnswersCountAsInterventions(t *testing.T) {
	in, store, _ := newTestIntake(t)
	m, reader := testMetrics(t)
	in.Metrics = m
	store.tasks["t-h"] = Task{ID: "t-h", Repository: "sf/ai-software-factory-code", Status: "waiting-human", IssueNumber: 30, Kind: "agent"}
	store.tasks["t-p"] = Task{ID: "t-p", Repository: "sf/ai-software-factory-code", Status: "waiting-poster", IssueNumber: 31, Kind: "agent", Author: "visitor"}
	store.tasks["t-r"] = Task{ID: "t-r", Repository: "sf/ai-software-factory-code", Status: "running", IssueNumber: 32, Kind: "agent"}
	comment := func(issue int, user string, id int) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"action":     "created",
			"issue":      map[string]any{"number": issue, "user": map[string]string{"login": "visitor"}},
			"comment":    map[string]any{"id": id, "user": map[string]string{"login": user}, "body": "an answer"},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", user, rr.Code)
		}
	}
	comment(30, "sebastian", 1)
	comment(30, "sebastian", 1)
	comment(30, "mallory", 2)
	comment(31, "visitor", 3)
	comment(31, "stranger", 4)
	comment(31, "sebastian", 5)
	comment(32, "sebastian", 6)
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{
		"kind=escalation_answer": 2,
		"kind=poster_answer":     1,
	})
}
