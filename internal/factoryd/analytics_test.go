package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
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
// The merge itself is no outcome: the card counts when its chain ends it.
func TestFinishCountsTheOutcomeAndTheCost(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}, pulls: map[int64]string{7: mergedAt(7, "m7")}}
	merged := prTask
	merged.Status, merged.PRNumber, merged.SpentEUR, merged.ModelAlias = "in-review", 7, 0.42, "big"
	merged.Usage.Attempts = 2
	r, store, _ := newActivityRunner(t, merged, fj)
	m, reader := testMetrics(t)
	r.completion.Metrics = m
	store.projects = []Project{{Name: "demo", Repository: "sf/repo", MergeRule: policy.MergeAuto}}
	for _, reason := range []string{"pull request merged", "live: covered by run 12", "live: covered by run 12"} {
		if _, err := execActivity(t, r, ActivityFinish, "t1", "done", reason); err != nil {
			t.Fatal(err)
		}
	}
	report := prTask
	report.ID, report.Status, report.SpentEUR, report.ModelAlias, report.Parent = "t-report", "running", 0.05, "small", "t1"
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
	store.projects = []Project{}
	failed := prTask
	failed.ID, failed.Status, failed.SpentEUR, failed.ModelAlias = "t2", "running", 0.1, "small"
	failed.Usage.Attempts = 3
	store.CreateTask(context.Background(), failed)
	if _, err := execActivity(t, r, ActivityFinish, "t2", "failed", "3 attempts exhausted"); err != nil {
		t.Fatal(err)
	}

	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=2,lane=none,merge_rule=auto,model_alias=big,process=none,project=demo,state=done":         1,
		"attempts=1,lane=none,merge_rule=auto,model_alias=small,process=none,project=demo,state=done":       1,
		"attempts=0,lane=none,merge_rule=auto,model_alias=none,process=none,project=demo,state=done":        1,
		"attempts=3,lane=none,merge_rule=human,model_alias=small,process=none,project=sf/repo,state=failed": 1,
	})
	got := histograms(t, reader, "factory.task.cost")
	want := map[string]histPoint{
		"lane=none,model_alias=big,outcome=merged,process=none":   {1, 0.42},
		"lane=none,model_alias=small,outcome=done,process=none":   {1, 0.05},
		"lane=none,model_alias=none,outcome=merged,process=none":  {1, 0},
		"lane=none,model_alias=small,outcome=failed,process=none": {1, 0.1},
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
		"lane=none,process=none,round=1,strict_improvement=unset,verdict=concerns": 1,
		"lane=none,process=none,round=2,strict_improvement=true,verdict=approve":   1,
		"lane=none,process=none,round=3,strict_improvement=false,verdict=approve":  1,
		"lane=none,process=none,round=1,strict_improvement=unset,verdict=none":     1,
	})
}

// A human's answer that raised the task's agent time counts one
// extra_time intervention; the same usage recorded again (a retried
// activity), usage that only grew by the agent's own work and a planner's
// retry time count none.
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
		{AgentTime: 25 * time.Minute, Attempts: 4, ExtraAttempts: 1, ExtraTime: 9 * time.Minute, RetryTime: 10 * time.Minute},
	} {
		if _, err := execActivity(t, r, ActivityRecordUsage, "t1", u); err != nil {
			t.Fatal(err)
		}
	}
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{"kind=extra_time,lane=none,process=none": 1})
}

// A session records the alias of its model on the task: the last
// session's is the task's.
func TestSessionRecorderKeepsTheModelAlias(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "running"})
	for _, model := range []string{"factory/small", "factory/big"} {
		if _, err := NewSessionRecorder(context.Background(), store, &fakeObjectStore{}, "t1", "s-"+model, "", model); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.ModelAlias != "big" {
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
		"kind=escalation_answer,lane=none,process=none": 2,
		"kind=poster_answer,lane=none,process=none":     1,
	})
}

// The endings outside a workflow count their outcome as Finish does: a
// backlog card whose issue a writer closed, and a human-lane task whose issue
// closed.
func TestIntakeEndingsCountTheOutcome(t *testing.T) {
	in, store, _ := newTestIntake(t)
	m, reader := testMetrics(t)
	in.Metrics = m
	store.tasks["t-s"] = Task{ID: "t-s", Repository: "sf/ai-software-factory-code", Status: "backlog", IssueNumber: 30, Kind: "agent", Author: "visitor"}
	store.tasks["t-h"] = Task{ID: "t-h", Repository: "sf/ai-software-factory-code", Status: "waiting-human", IssueNumber: 31, Kind: "human"}
	for _, issue := range []int{30, 31, 31} {
		b, _ := json.Marshal(map[string]any{"action": "closed", "issue": map[string]any{"number": issue, "user": map[string]string{"login": "visitor"}},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"}, "sender": map[string]string{"login": "sebastian"}})
		if rr := postWebhook(in, "issues", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("close #%d: status %d", issue, rr.Code)
		}
	}
	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=0,lane=none,merge_rule=human,model_alias=none,process=none,project=sf-ai-software-factory-code,state=closed": 1,
		"attempts=0,lane=none,merge_rule=human,model_alias=none,process=none,project=sf-ai-software-factory-code,state=done":   1,
	})
}

// A human's decision on a factory pull request counts when its webhook
// event is new (a redelivery, under the same or a new delivery id, is not), from the console (which acts as the human in Forgejo) and
// from Forgejo alike: an approving review, a review asking for changes,
// and a close without a merge. The bot's close, a merge and an untrusted
// reviewer count nothing.
func TestIntakeCountsPullRequestDecisions(t *testing.T) {
	in, store, _ := newTestIntake(t)
	m, reader := testMetrics(t)
	in.Metrics = m
	store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", PRNumber: 21}
	send := func(event, action, sender string, merged bool, delivery string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"action": action,
			"pull_request": map[string]any{"number": 21, "merged": merged,
				"head": map[string]any{"ref": "factory/t-pr", "sha": "h1", "repo": map[string]any{"full_name": "factory-agent/ai-software-factory-code"}}},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"sender":     map[string]string{"login": sender},
			"review":     map[string]any{"content": "r"},
		})
		req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(b))
		req.Header.Set("X-Forgejo-Event-Type", event)
		req.Header.Set("X-Forgejo-Signature", sign(webhookSecret, b))
		req.Header.Set("X-Forgejo-Delivery", delivery)
		rr := httptest.NewRecorder()
		in.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", event, sender, rr.Code, rr.Body)
		}
	}
	send("pull_request_review_approved", "reviewed", "sebastian", false, "d1")
	send("pull_request_review_approved", "reviewed", "sebastian", false, "d1")
	// Forgejo's redelivery sends the same review under a new delivery id
	send("pull_request_review_approved", "reviewed", "sebastian", false, "d1-redelivered")
	send("pull_request_review_approved", "reviewed", "mallory", false, "d2")
	send("pull_request_review_rejected", "reviewed", "sebastian", false, "d3")
	send("pull_request", "closed", "factory-agent", false, "d4")
	send("pull_request", "closed", "sebastian", false, "d5")
	send("pull_request", "closed", "sebastian", false, "d6")
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{
		"kind=approve,lane=none,process=none": 1, "kind=request_changes,lane=none,process=none": 1, "kind=reject,lane=none,process=none": 1,
	})
}

// The four task metrics carry the task row's process and lane, none where
// the row holds none (a roll): the outcome and the cost at Finish, the
// review answer, and a human's intervention from the workflow's usage and
// from the intake.
func TestMetricsCarryProcessAndLane(t *testing.T) {
	ctx := context.Background()
	m, reader := testMetrics(t)
	card := prTask
	card.Status, card.PRNumber, card.SpentEUR, card.ModelAlias, card.Lane, card.Process = "in-review", 7, 0.3, "small", "ready", "spec"
	card.Usage = TaskUsage{AgentTime: 18 * time.Minute, Attempts: 1}
	r, store, _ := newActivityRunner(t, card, &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}, pulls: map[int64]string{7: mergedAt(7, "m7")}})
	r.cfg.Metrics, r.completion.Metrics = m, m
	store.projects = []Project{{Name: "linkshort", Repository: "sf/repo"}}
	if _, err := execActivity(t, r, ActivityRecordUsage, "t1", TaskUsage{AgentTime: 19 * time.Minute, Attempts: 1, ExtraTime: 9 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"pull request merged", "live: covered by run 12"} {
		if _, err := execActivity(t, r, ActivityFinish, "t1", "done", reason); err != nil {
			t.Fatal(err)
		}
	}
	roll := prTask
	roll.ID, roll.Status, roll.Source = "t-roll", "in-review", SourceRoll
	store.CreateTask(ctx, roll)
	if _, err := execActivity(t, r, ActivityFinish, "t-roll", "failed", "checks red"); err != nil {
		t.Fatal(err)
	}
	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=1,lane=ready,merge_rule=human,model_alias=small,process=spec,project=linkshort,state=done": 1,
		"attempts=0,lane=none,merge_rule=human,model_alias=none,process=none,project=linkshort,state=failed": 1,
	})
	if got, want := histograms(t, reader, "factory.task.cost"), map[string]histPoint{
		"lane=ready,model_alias=small,outcome=merged,process=spec": {1, 0.3},
		"lane=none,model_alias=none,outcome=failed,process=none":   {1, 0},
	}; !maps.Equal(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}

	reviewed := gateTask
	reviewed.Lane, reviewed.Process = "ready", "maintenance"
	g, _, _ := newGateRunner(t, reviewed, &gateForgejo{diff: "diff --git a/x b/x\n+x\n"})
	g.cfg.Metrics = m
	g.supervisor = testSupervisorModel((&fakeLiteLLM{content: `{"verdict":"concerns","summary":"no test"}`}).srv(t).URL)
	if _, err := execActivity(t, g, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}, []Review(nil)); err != nil {
		t.Fatal(err)
	}
	wantSums(t, sums(t, reader, "factory.review.verdict"), map[string]float64{
		"lane=ready,process=maintenance,round=1,strict_improvement=unset,verdict=concerns": 1,
	})

	in, istore, _ := newTestIntake(t)
	in.Metrics = m
	istore.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", PRNumber: 21, Lane: "ready", Process: "maintenance"}
	b, _ := json.Marshal(map[string]any{
		"action": "reviewed",
		"pull_request": map[string]any{"number": 21,
			"head": map[string]any{"ref": "factory/t-pr", "sha": "h1", "repo": map[string]any{"full_name": "factory-agent/ai-software-factory-code"}}},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":     map[string]string{"login": "sebastian"},
		"review":     map[string]any{"content": "r"},
	})
	post(t, in, "pull_request_review_approved", b)
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{
		"kind=extra_time,lane=ready,process=spec":     1,
		"kind=approve,lane=ready,process=maintenance": 1,
	})
}

// A report task's row holds no lane; its metrics carry its parent's
// process and lane: the presort's card waits in backlog, the screening's
// is in ready.
func TestReportMetricsTakeParentLane(t *testing.T) {
	ctx := context.Background()
	m, reader := testMetrics(t)
	parent := prTask
	parent.Status, parent.Lane, parent.Process = "backlog", "backlog", "maintenance"
	r, store, _ := newActivityRunner(t, parent, &prForgejo{labels: map[int64][]string{4: {"improvement", "state/backlog"}}})
	r.completion.Metrics = m
	store.projects = []Project{{Name: "demo", Repository: "sf/repo"}}
	store.CreateTask(ctx, Task{ID: "t-ready", Status: "running", Repository: "sf/repo", IssueNumber: 5, Lane: "ready", Process: "spec"})
	for _, report := range []Task{
		{ID: "t-presort", Parent: "t1", Source: SourcePresort},
		{ID: "t-screen", Parent: "t-ready", Source: SourceScreening},
	} {
		report.Status, report.Repository, report.IssueNumber, report.ModelAlias, report.SpentEUR = "running", "sf/repo", 4, "small", 0.05
		report.Usage.Attempts = 1
		store.CreateTask(ctx, report)
		if _, err := execActivity(t, r, ActivityFinish, report.ID, "done", "report posted"); err != nil {
			t.Fatal(err)
		}
	}
	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=1,lane=backlog,merge_rule=human,model_alias=small,process=maintenance,project=demo,state=done": 1,
		"attempts=1,lane=ready,merge_rule=human,model_alias=small,process=spec,project=demo,state=done":          1,
	})
	if got, want := histograms(t, reader, "factory.task.cost"), map[string]histPoint{
		"lane=backlog,model_alias=small,outcome=done,process=maintenance": {1, 0.05},
		"lane=ready,model_alias=small,outcome=done,process=spec":          {1, 0.05},
	}; !maps.Equal(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}
