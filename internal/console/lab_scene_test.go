package console

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// labGates are the gates a task row names.
var labGates = []string{factoryd.GateIntake, factoryd.GateSession, factoryd.GateChecks, factoryd.GatePolicy, factoryd.GateMigration,
	factoryd.GateReproduction, factoryd.GateCriteria, factoryd.GateReview, factoryd.GateApproval, factoryd.GateMerge, factoryd.GateHuman,
	factoryd.GateCommissioning, factoryd.GateScreening, factoryd.GateAudit, factoryd.GateProvider, factoryd.GateDependencies,
	factoryd.GateMergeQueue}

// labReply is one scripted answer of the model.
type labReply []labChunk

type labChunk struct {
	Say  string
	Call [][2]string
}

// labSceneJSON is labScene as a recording script posts it to /lab/scene.
type labSceneJSON struct {
	Project       labProject
	Remote        string
	Projects      []factoryd.Project
	Config        *policy.ProjectConfig
	MonthCap      float64
	PRFiles       map[string]string
	Tasks         []factoryd.BoardTask
	Deps          map[string][]string
	Plans         map[string][]factoryd.PlanRevision
	Questions     map[string][]factoryd.PlanQuestion
	Approvals     []factoryd.ApprovalRequest
	Objects       map[string]string
	Events        []factoryd.Event
	Chat          []factoryd.ChatMessage
	Proposals     []factoryd.ChatProposal
	UsedProposals []factoryd.ChatProposal
	Spend         float64
	ChatSpend     float64
	GitHub        string
	Alerts        []factoryd.Alert
	Drawer        string
	Autonomy      string
	Seen          map[int64]int64
	Working       []int64
	Inbox, Open   int64
	Archived      []int64
	Routines      []factoryd.RoutineView
	Runs          map[string][]factoryd.RoutineRun
	RoutinesAt    time.Time
	Metrics       factoryd.MetricsReport
	Replies       []labReply
}

func (j labSceneJSON) scene() labScene {
	return labScene{project: j.Project, remote: j.Remote, projects: j.Projects, config: j.Config, monthCap: j.MonthCap,
		prFiles: j.PRFiles, tasks: j.Tasks, deps: j.Deps, plans: j.Plans, questions: j.Questions, approvals: j.Approvals,
		objects: j.Objects, events: j.Events, chat: j.Chat, proposals: j.Proposals, usedProposals: j.UsedProposals,
		spend: j.Spend, chatSpend: j.ChatSpend, github: j.GitHub, alerts: j.Alerts, drawer: j.Drawer, autonomy: j.Autonomy,
		seen: j.Seen, working: j.Working, inbox: j.Inbox, open: j.Open, archived: j.Archived, routines: j.Routines,
		runs: j.Runs, routinesAt: j.RoutinesAt, metrics: j.Metrics, replies: j.Replies}
}

// check refuses a scene the console never holds: a status, gate or lane
// outside its list, a time after now, a reference to a task the scene
// lacks, a task without its timeline object.
func (j labSceneJSON) check(now time.Time) error {
	ids := map[string]bool{}
	for _, t := range j.Tasks {
		ids[t.ID] = true
	}
	for _, t := range j.Tasks {
		if !slices.Contains(factoryd.Statuses, t.Status) {
			return fmt.Errorf("task %s: status %q is none of %v", t.ID, t.Status, factoryd.Statuses)
		}
		if t.Gate != "" && !slices.Contains(labGates, t.Gate) {
			return fmt.Errorf("task %s: gate %q is none of %v", t.ID, t.Gate, labGates)
		}
		if !slices.Contains(policy.LaneNames, t.Lane) {
			return fmt.Errorf("task %s: lane %q is none of %v", t.ID, t.Lane, policy.LaneNames)
		}
	}
	late := func(what string, at time.Time) error {
		if at.After(now) {
			return fmt.Errorf("%s %s is after now", what, at.Format(time.RFC3339))
		}
		return nil
	}
	var errs []error
	for _, t := range j.Tasks {
		errs = append(errs, late("task "+t.ID+" CreatedAt", t.CreatedAt))
	}
	for _, e := range j.Events {
		errs = append(errs, late("event At", e.At))
	}
	for _, m := range j.Chat {
		errs = append(errs, late("chat message At", m.At))
	}
	for _, a := range j.Alerts {
		errs = append(errs, late("alert RaisedAt", a.RaisedAt))
	}
	for _, a := range j.Approvals {
		errs = append(errs, late("approval CreatedAt", a.CreatedAt))
	}
	known := func(what, id string) error {
		if id != "" && !ids[id] {
			return fmt.Errorf("%s %q names no task of the scene", what, id)
		}
		return nil
	}
	for _, t := range j.Tasks {
		errs = append(errs, known("parent", t.Parent))
	}
	for k, vs := range j.Deps {
		errs = append(errs, known("dependency key", k))
		for _, v := range vs {
			errs = append(errs, known("dependency", v))
		}
	}
	for _, e := range j.Events {
		errs = append(errs, known("event task", e.TaskID))
	}
	for _, a := range j.Alerts {
		errs = append(errs, known("alert task", a.TaskID))
	}
	errs = append(errs, known("Drawer", j.Drawer))
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	for _, t := range j.Tasks {
		if _, ok := j.Objects["sessions/"+t.ID+"/timeline.jsonl"]; !ok {
			return fmt.Errorf("task %s lacks the object sessions/%s/timeline.jsonl", t.ID, t.ID)
		}
	}
	return nil
}

func TestLabSceneJSON(t *testing.T) {
	r := newRig(t)
	now := time.Now()
	body := fmt.Sprintf(`{"Project":{"Name":"nord-booking","Repository":"nord/booking","Login":"sebastian"},
	 "Tasks":[{"ID":"t-1","Kind":"agent","Status":"running","Lane":"ready","Repository":"nord/booking","IssueNumber":12,
	   "Objective":"Waitlist","CreatedAt":%q,"Gate":"session"}],
	 "Objects":{"sessions/t-1/timeline.jsonl":""}}`, now.Add(-time.Hour).Format(time.RFC3339))
	var j labSceneJSON
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		t.Fatal(err)
	}
	if err := j.check(now); err != nil {
		t.Fatal(err)
	}
	r.seedScene(j.scene())
	if got := r.tasks.tasks[0].Status; got != "running" {
		t.Fatalf("status %q", got)
	}
}

func TestLabSceneJSONWaitingDeps(t *testing.T) {
	var j labSceneJSON
	body := `{"Tasks":[{"ID":"t-1","Status":"waiting-deps","Lane":"ready","Gate":"dependencies","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatal(err)
	}
	if err := j.check(time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestLabSceneJSONRefuses(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ name, body, want string }{
		{"status", `{"Tasks":[{"ID":"t-1","Status":"flying","Lane":"ready","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `status "flying"`},
		{"gate", `{"Tasks":[{"ID":"t-1","Status":"running","Lane":"ready","Gate":"flying","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `gate "flying"`},
		{"timeline", `{"Tasks":[{"ID":"t-1","Status":"running","Lane":"ready","CreatedAt":"2026-01-01T00:00:00Z"}]}`, `sessions/t-1/timeline.jsonl`},
		{"future", fmt.Sprintf(`{"Tasks":[{"ID":"t-1","Status":"backlog","Lane":"backlog","CreatedAt":%q}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, now.Add(time.Hour).Format(time.RFC3339)), "CreatedAt"},
		{"parent", `{"Tasks":[{"ID":"t-1","Status":"backlog","Lane":"backlog","Parent":"t-9","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `parent "t-9"`},
	} {
		var j labSceneJSON
		if err := json.Unmarshal([]byte(tc.body), &j); err != nil {
			t.Fatal(tc.name, err)
		}
		err := j.check(now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q in it", tc.name, err, tc.want)
		}
	}
}

func TestLabSceneChatReplies(t *testing.T) {
	r, _, g := newChatRig(t)
	g.lenient = true
	g.script("nord-booking", []labReply{{{Say: "A waitlist, "}, {Say: "noted."}}})
	ck, s := r.loggedIn(t)
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"add a waitlist"}})
	equal(t, "stream", []any{code, events}, []any{200, []string{`text {"text":"A waitlist, "}`, `text {"text":"noted."}`, `done {}`}})
	rows, _ := r.chat.ChatMessages(t.Context(), "sf/code", "sebastian", 0, 10)
	if last := rows[len(rows)-1]; last.Role != "assistant" || last.Content != "A waitlist, noted." {
		t.Fatalf("last row %+v, want the scripted answer", last)
	}
	code, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"and now?"}})
	t.Logf("unscripted: %d %v", code, events)
}
