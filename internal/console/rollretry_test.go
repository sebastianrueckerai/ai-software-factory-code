package console

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	retryComment = "factoryd: retry ordered: the failed attempts' bound is reset and one roll task of main's newest green images starts. Closing this issue."
	retryHeld    = "factoryd: retry ordered: the failed attempts' bound is reset and one roll task of main's newest green images starts once no plan is open on main. Closing this issue."
)

// rollRetryRig is the failed-roll card t-hq (#51 on sf/code) on the real Act
// and the real store, with three failed roll requests of sf/code, and the
// real RetryRoll behind the console on a Forgejo that records its calls.
func rollRetryRig(t *testing.T) (*rig, *realAct, func() []string) {
	t.Helper()
	q := factoryd.HumanQuestion{Ask: "3 rolls failed since the last one merged: main's newest images are not live. Retry the roll, park it or close it?",
		Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerRetryRoll, Label: "Retry the roll"}, {Value: factoryd.AnswerPark, Label: "Park"}, {Value: factoryd.AnswerClose, Label: "Close"}}}
	r := newRig(t)
	card := factoryd.Task{ID: "t-hq", Kind: "human", Status: "waiting-human", Source: factoryd.SourceRollFailed,
		Repository: "sf/code", IssueNumber: 51, SHA: "abc1234", Objective: "needs you"}
	r.tasks.tasks = []factoryd.BoardTask{{Task: card, Escalation: q.Ask, Question: &q}}
	ra := newRealAct(t, "t-hq", 51, factoryd.SourceRollFailed, q, policy.AutonomyManual)
	ra.wire(r, "t-hq", false)
	for i := int64(1); i <= 3; i++ {
		id := fmt.Sprintf("t-r%d", i)
		if err := ra.pg.CreateTask(context.Background(), factoryd.Task{ID: id, Kind: "agent", Status: "failed", Source: factoryd.SourceRoll, Track: policy.TrackDirect, Repository: "sf/code", IssueNumber: 60 + i}); err != nil {
			t.Fatal(err)
		}
		if err := ra.pg.InsertRollRequest(context.Background(), factoryd.RollRequest{SHA: fmt.Sprintf("%040d", i), Repository: "sf/code", RunID: 40 + i, State: factoryd.RollFailed, TaskID: id}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		calls = append(calls, req.Method+" "+req.URL.Path+" "+string(body))
		mu.Unlock()
		switch {
		case req.Method == http.MethodGet:
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": 9001}`)
		}
	}))
	t.Cleanup(srv.Close)
	fj := &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	r.c.cfg.RetryRoll = func(ctx context.Context, card factoryd.Task) error { return factoryd.RetryRoll(ctx, ra.pg, fj, card) }
	return r, ra, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func failedRolls(t *testing.T, ra *realAct) int {
	t.Helper()
	got, err := ra.pg.FailedRolls(context.Background(), "sf/code")
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}

func wantRetryCalls(t *testing.T, calls []string, comment string) {
	t.Helper()
	want := []string{
		"GET /api/v1/repos/sf/code/issues/51/dependencies ",
		`POST /api/v1/repos/sf/code/issues/51/comments {"body":"` + comment + `"}`,
		`PATCH /api/v1/repos/sf/code/issues/51 {"state":"closed"}`,
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("forgejo calls %q, want %q", calls, want)
	}
}

// The human's Retry the roll on the real Act: the three failed roll requests
// go, the card's issue closes as factoryd with the retry's comment, the case
// resolves as retry, and a second press is refused as the case ended.
func TestRetryTheRollResetsTheLadderThroughAct(t *testing.T) {
	r, ra, calls := rollRetryRig(t)
	if n := failedRolls(t, ra); n != 3 {
		t.Fatalf("seeded %d failed rolls, want 3", n)
	}
	ck, s := r.loggedIn(t)
	press := func() *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-hq/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerRetryRoll}, "case": {fmt.Sprint(ra.caseID)}}), ck)
	}
	if rec := press(); rec.Code != http.StatusSeeOther {
		t.Fatalf("Retry the roll = %d %q, want 303", rec.Code, rec.Body)
	}
	if n := failedRolls(t, ra); n != 0 {
		t.Fatalf("%d failed rolls left, want none", n)
	}
	wantRetryCalls(t, calls(), retryComment)
	if k := ra.caseNow(t); k.Status != factoryd.CaseResolved || k.Resolution == nil || k.Resolution.Action != factoryd.AnswerRetryRoll {
		t.Fatalf("case %s %+v, want it resolved as retry", k.Status, k.Resolution)
	}
	if c := r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"); len(c) != 0 {
		t.Fatalf("the console posted %q, want no comment of the human's", c)
	}
	rec := press()
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "the card has no live ask") {
		t.Fatalf("second press = %d %q, want 409 naming that the card has no live ask", rec.Code, rec.Body)
	}
	if got := len(calls()); got != 3 {
		t.Fatalf("%d forgejo calls after the second press, want the first press's 3", got)
	}
}

// The chat's answer tool runs the same retry.
func TestTheChatsRetryTheRollResetsTheLadder(t *testing.T) {
	r, ra, calls := rollRetryRig(t)
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", CaseID: ra.caseID, Action: factoryd.AnswerRetryRoll}); err != nil {
		t.Fatalf("the chat's retry: %v", err)
	}
	if n := failedRolls(t, ra); n != 0 {
		t.Fatalf("%d failed rolls left, want none", n)
	}
	wantRetryCalls(t, calls(), retryComment)
	if k := ra.caseNow(t); k.Status != factoryd.CaseResolved || k.Resolution == nil || k.Resolution.Action != factoryd.AnswerRetryRoll {
		t.Fatalf("case %s %+v, want it resolved as retry", k.Status, k.Resolution)
	}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", CaseID: ra.caseID, Action: factoryd.AnswerRetryRoll}); err == nil {
		t.Fatal("a second retry on the ended case was accepted")
	}
}

// With a plan open on the repository, the retry's comment says the roll
// starts once no plan is open on main.
func TestRetryTheRollWhileAPlanIsOpenSaysWhen(t *testing.T) {
	r, ra, calls := rollRetryRig(t)
	ctx := context.Background()
	if err := ra.pg.CreateTask(ctx, factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 40, Track: policy.TrackPlan}); err != nil {
		t.Fatal(err)
	}
	if err := ra.pg.CreateTask(ctx, factoryd.Task{ID: "t-c1", Kind: "agent", Status: "merged", Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "a", Track: policy.TrackDirect,
		Repository: "sf/code", IssueNumber: 41}); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.pg.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-c1", IssueNumber: 41, Kind: "pull_request_merged", Actor: "factory-merge", Via: factoryd.ViaFactoryd}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", CaseID: ra.caseID, Action: factoryd.AnswerRetryRoll}); err != nil {
		t.Fatal(err)
	}
	wantRetryCalls(t, calls(), retryHeld)
}

// A retry that fails names its error scrubbed: URL userinfo and a bearer
// value never reach the human.
func TestRetryTheRollFailureIsScrubbed(t *testing.T) {
	r, _ := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{rollFailedCard()}
	r.c.cfg.RetryRoll = func(context.Context, factoryd.Task) error {
		return fmt.Errorf("close: Get https://user:hunter2@git.example/x: Authorization: Bearer abcdef0123456789abcdef")
	}
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-roll/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerRetryRoll}}), ck)
	body := rec.Body.String()
	if rec.Code != http.StatusInternalServerError || strings.Contains(body, "hunter2") || strings.Contains(body, "abcdef0123456789") || !strings.Contains(body, "The roll's retry failed") {
		t.Fatalf("a failed retry = %d %q, want a 500 with both secrets masked", rec.Code, body)
	}
}
