package console

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestLab serves the console on a real port over the rig's fakes, seeded
// as one of labScenes, for scripts/console-lab. GET /lab/scenario/{name}
// seeds another scenario in place and names the running card the drawer
// shots open in X-Lab-Drawer. It ends once $CONSOLE_LAB_DIR/stop exists,
// or after 30 minutes.
func TestLab(t *testing.T) {
	if os.Getenv("CONSOLE_LAB") != "1" {
		t.Skip("the screenshot lab runs with CONSOLE_LAB=1")
	}
	dir := cmp.Or(os.Getenv("CONSOLE_LAB_DIR"), "/tmp/claude-1000/console-lab")
	addr := cmp.Or(os.Getenv("CONSOLE_LAB_ADDR"), "127.0.0.1:8090")
	scenario := cmp.Or(os.Getenv("CONSOLE_LAB_SCENARIO"), "busy")

	r := newRig(t)
	r.c.cfg.Timelines = lockedObjects{r.objects}
	forgejo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		files, ok := labPRFiles[req.URL.Path]
		if !ok {
			r.fg.serve(w, req)
			return
		}
		if req.URL.Query().Get("page") != "1" {
			files = "[]"
		}
		io.WriteString(w, files)
	}))
	t.Cleanup(forgejo.Close)
	// the project's documents (roadmap 277): sf/docs over the lab's Forgejo,
	// the roadmap and the two plans it names; the writer token is minted,
	// as the factory mints its tokens, never a literal
	docsFake, docs := docsOver(t, forgejo.URL, labDocs)
	docsToken, _ := factoryd.NewReadToken()
	r.c.cfg.ForgejoURL, r.c.cfg.DocsToken = docs.URL, docsToken
	r.ledger.chat = &policy.ChatConfig{DocRepo: "sf/docs", Confirm: []string{}}
	r.c.cfg.Operator = &factoryd.OperatorModel{GatewayURL: "http://127.0.0.1:9", Token: "lab"} // the panel shows; no turn is sent
	r.c.cfg.Steer = func(context.Context, string, string, string) error { return nil }
	// the capped card of the fire scene is below the fund's floor, so its approvers get the Fund action (roadmap 210)
	r.c.cfg.SessionFunded = func(_ context.Context, id, _ string) (string, error) {
		if id == labID(610) {
			return "the remaining budget cannot fund a call: Raise the budget (Fund on the board) and answer, or close the card.\n" +
				"0.520 EUR of the 2.000 EUR budget remains, and a session's first call on factory/coding-frontier needs 0.600000 EUR", nil
		}
		return "", nil
	}
	r.projects.rows = slices.DeleteFunc(projectRows(), func(p factoryd.Project) bool { return p.Name == "gone" })
	r.projects.rows[0].Process, r.projects.rows[0].Lanes = "spec", testProcesses()["spec"].Lanes
	// demo-app's staging is deployed, its prod never had a green build (roadmap 257)
	demo := &r.projects.rows[slices.IndexFunc(r.projects.rows, func(p factoryd.Project) bool { return p.Name == "demo-app" })]
	demo.Environments = append(demo.Environments, factoryd.Environment{Namespace: "demo-prod", Path: "deploy/prod"})
	r.c.cfg.Kustomization = func(_ context.Context, namespace, _ string) (factoryd.FluxState, error) {
		if namespace == "demo-prod" {
			return factoryd.FluxState{Ready: "False",
				Message: "kustomization path not found: stat /tmp/kustomization-103304189/deploy/prod: no such file or directory"}, nil
		}
		return factoryd.FluxState{Ready: "True", Message: "Applied revision: deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456",
			Applied: "deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"}, nil
	}
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", Objects: "seed",
		Agents: "coder", Process: "spec", Namespaces: []string{"factory"}}}
	r.ledger.monthCaps = map[string]float64{"ai-software-factory-code": 120}
	// the messenger panel (roadmap 145): telegram connected, discord offered,
	// two levels the writer set and one left to its derived default
	r.c.cfg.Messengers = &Messengers{
		Store: &fakeMessengerStore{
			links:  []factoryd.MessengerLink{{Venue: factoryd.VenueTelegram, Address: "@sebastian", VenueUser: "42"}},
			levels: map[string]string{"demo-app/demo-app": factoryd.LevelAll, "sf/infra": factoryd.LevelOff},
		},
		TelegramName: "factorybot",
		Discord:      &DiscordConnect{AuthorizeURL: func(string) string { return "https://discord.example/oauth" }},
	}
	r.ledger.briefs = map[string]string{labBump.Brief: labBumpBrief}
	ck, _ := r.loggedIn(t)

	var mu sync.RWMutex
	if _, err := r.labSeed(scenario); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// the editor's slow save (the 2026-10-15 plan, item 7): commits take
	// ~1.8 s, so the saving… state and a second save queued behind it show;
	// ?on=0 sets the save quick again
	mux.HandleFunc("POST /lab/docs-slow", func(w http.ResponseWriter, req *http.Request) {
		slow := 1800 * time.Millisecond
		if req.URL.Query().Get("on") == "0" {
			slow = 0
		}
		docsFake.mu.Lock()
		docsFake.slow = slow
		docsFake.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// an outside change of a document (items 5+6): master moves as if
	// committed by ?by at 14:08 — ?line names the line it changes, from
	// 1, else the file's last said one; ?text replaces that line, else it
	// appends to it — and the project's stream carries the doc_written
	// row the webhook intake raises for it, via webhook
	mux.HandleFunc("POST /lab/docs-change", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		path := cmp.Or(q.Get("path"), "docs/roadmap.md")
		docsFake.mu.Lock()
		text, ok := docsFake.files[path]
		docsFake.mu.Unlock()
		if !ok {
			http.Error(w, "no such document: "+path, http.StatusNotFound)
			return
		}
		lines := strings.Split(text, "\n")
		n, _ := strconv.Atoi(cmp.Or(q.Get("line"), "0"))
		if n < 1 || n > len(lines) {
			for n = len(lines); n > 0 && strings.TrimSpace(lines[n-1]) == ""; n-- {
			}
		}
		if n < 1 {
			n = 1
		}
		lines[n-1] = cmp.Or(q.Get("text"), lines[n-1]+" Changed outside the console.")
		docsFake.outside(path, strings.Join(lines, "\n"), cmp.Or(q.Get("by"), "Sebastian"), "2026-10-15T14:08:00Z")
		docsFake.mu.Lock()
		head := versionSHA(docsFake.version)
		docsFake.mu.Unlock()
		r.events.Record(req.Context(), factoryd.Event{Repository: "sf/code", Kind: factoryd.EventDocWritten,
			Actor: cmp.Or(q.Get("by"), "Sebastian"), Via: factoryd.ViaWebhook,
			Detail: map[string]any{"path": path, "commit": head}})
		w.WriteHeader(http.StatusNoContent)
	})
	// the roll's gap: the router answers 503 to the event stream while the
	// console's pod stands between its old and its new one, which closes
	// the browser's EventSource for good. ?n arms n such answers, the state
	// a board sits in until it reopens its stream itself.
	var streamFails struct {
		sync.Mutex
		n int
	}
	mux.HandleFunc("POST /lab/stream-fail", func(w http.ResponseWriter, req *http.Request) {
		n, _ := strconv.Atoi(cmp.Or(req.URL.Query().Get("n"), "1"))
		streamFails.Lock()
		streamFails.n = n
		streamFails.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /lab/scenario/{name}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		drawer, err := r.labSeed(req.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("X-Lab-Drawer", drawer)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// the event stream stays open as long as its page; a seed must not wait for it
		if !strings.HasSuffix(req.URL.Path, "/events/stream") {
			mu.RLock()
			defer mu.RUnlock()
		} else {
			streamFails.Lock()
			fail := streamFails.n > 0
			if fail {
				streamFails.n--
			}
			streamFails.Unlock()
			if fail {
				http.Error(w, "no ready backends", http.StatusServiceUnavailable)
				return
			}
		}
		r.c.ServeHTTP(w, req)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	stop := filepath.Join(dir, "stop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(stop)
	if err := os.WriteFile(filepath.Join(dir, "cookie.txt"), []byte(ck.Name+"="+ck.Value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("console lab: http://%s/ shows scenario %s; touch %s to stop\n", addr, scenario, stop)
	for end := time.Now().Add(30 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
	}
}

// labScene is one moment of the code project, everything the fakes answer.
// drawer is the running card the drawer shots open, "" for none.
// labObjects serialises a seed's writes to the rig's objects against the
// reads of a stream, which re-renders the board while a scenario loads.
var labObjects sync.RWMutex

// lockedObjects is the rig's objects under labObjects.
type lockedObjects struct{ m fakeObjects }

func (o lockedObjects) Get(ctx context.Context, k string) ([]byte, error) {
	labObjects.RLock()
	defer labObjects.RUnlock()
	return o.m.Get(ctx, k)
}

func (o lockedObjects) Put(ctx context.Context, k string, b []byte) error {
	labObjects.Lock()
	defer labObjects.Unlock()
	return o.m.Put(ctx, k, b)
}

func (o lockedObjects) Delete(ctx context.Context, k string) error {
	labObjects.Lock()
	defer labObjects.Unlock()
	return o.m.Delete(ctx, k)
}

type labScene struct {
	tasks     []factoryd.BoardTask
	deps      map[string][]string
	plans     map[string][]factoryd.PlanRevision
	questions map[string][]factoryd.PlanQuestion
	approvals []factoryd.ApprovalRequest
	objects   map[string]string
	events    []factoryd.Event
	chat      []factoryd.ChatMessage
	proposals []factoryd.ChatProposal
	// usedProposals are the scene's proposals the member already pressed,
	// their do- row among the chat's
	usedProposals []factoryd.ChatProposal
	// attachments are the conversation's image rows, their bytes in objects
	attachments      []factoryd.ChatAttachment
	spend, chatSpend float64
	github           string
	alerts           []factoryd.Alert
	resources        []cloudres.Resource
	drawer           string
	// autonomy is the project's autonomy level for the scene (roadmap
	// 239); empty: manual.
	autonomy string
	// seen are the member's seen markers by thread, the rows up to which
	// they read it; working the threads with a turn in flight
	seen    map[int64]int64
	working []int64
	// summaries are the compacted threads' summaries by thread
	summaries map[int64]factoryd.ThreadSummary
	// inbox is the member's inbox thread, 0 for one the list makes; open
	// the thread the chat opens, 0 for the last message's
	inbox, open int64
	// archived are the member's archived threads
	archived []int64
	// routines are the code project's routines as its board shows them
	// at the design round's time routinesAt, which now stands for, and
	// runs their runs by routine, newest first (C16)
	routines   []factoryd.RoutineView
	runs       map[string][]factoryd.RoutineRun
	routinesAt time.Time
}

var labScenes = map[string]func(now time.Time) labScene{"busy": busyScene, "idle": idleScene, "fire": fireScene, "quiet": quietScene,
	"assisted": assistedScene, "threads": threadsScene, "compacted": compactedScene, "poke": pokeScene, "escalation": escalationScene,
	"history": historyScene, "wake": wakeScene}

// threadsScene is the busy board with sebastian's four threads: the open
// one on the session store, a planning thread with a turn in flight, a
// costs thread whose answer Sebastian has not read, and yesterday's export
// thread (roadmap 239's second thread and unread badge).
func threadsScene(now time.Time) labScene {
	s := busyScene(now)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	s.chat = []factoryd.ChatMessage{
		{Conversation: 1, Role: "user", Content: "Export the project to GitHub in pull request mode.", At: ago(26 * time.Hour)},
		{Conversation: 1, Role: "assistant", Content: "The export runs in pull request mode once the mirror is set.", At: ago(26*time.Hour - time.Minute)},
		{Conversation: 2, Role: "user", Content: "Give the costs page a from and a to date.", At: ago(50 * time.Minute)},
		{Conversation: 2, Role: "assistant", Content: "Filed the costs card with the acceptance criteria from your words; it waits on the ledger read.", At: ago(41 * time.Minute)},
		{Conversation: 3, Role: "user", Content: "Plan the members page: who is on the project, their roles, invite by Forgejo login.", At: ago(9 * time.Minute)},
		{Conversation: 4, Role: "user", Content: "Why is the session store card waiting?", At: ago(6 * time.Minute)},
		{Conversation: 4, Role: "assistant", Content: "It asked which store the session table should use: the plan named Postgres, the brief allows SQLite for a single replica.", At: ago(6*time.Minute - 20*time.Second)},
	}
	s.proposals = nil
	s.seen = map[int64]int64{1: 2, 2: 3, 3: 5, 4: 7}
	s.working = []int64{3}
	return s
}

// pokeScene is the threads scene with the inbox: the question on #603
// and the merge of #612 read, and the factory's poke on #600's end
// arriving while the session store thread is open, so the inbox row and
// the tab title carry its badge; yesterday's export thread is archived
// (roadmap 239, C14).
func pokeScene(now time.Time) labScene {
	s := threadsScene(now)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	s.inbox, s.open = 5, 4
	s.chat = append(s.chat,
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessageEscalation, TaskID: labID(603), At: ago(28 * time.Minute),
			Content: "#603 needs you: Which store keeps the nightly board snapshots?", Choices: []string{"Postgres", "SQLite", "Ask the author"}},
		factoryd.ChatMessage{Conversation: 5, Role: "user", Content: "Postgres", At: ago(20 * time.Minute)},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessagePoke, TaskID: labID(603), At: ago(20 * time.Minute),
			Content: "Answered #603 with “Postgres”; the card goes on."},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessageEscalation, TaskID: labID(606), At: ago(12 * time.Minute),
			Content: "#606 waits for you: merge PR #612, high risk.", Choices: []string{"Approve"}},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessagePoke, TaskID: labID(600), At: ago(time.Minute),
			Content: "#600 is done: Costs: the day cap line under the table. €0.18 in all."})
	s.seen[5] = int64(len(s.chat) - 1)
	s.archived = []int64{1}
	return s
}

// escalationScene is the busy board at autonomy autonomous with the
// inbox open: the agent's own start with its decider line, then the
// question on #603 as the inbox's escalation, its choices once in the
// answer row between the log and the box (roadmap 239, C14). A later
// ask, already settled by the member's reply, sits over the escalation:
// the row must still offer the escalation, not fall silent on it.
func escalationScene(now time.Time) labScene {
	s := threadsScene(now)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	s.autonomy, s.inbox, s.open = policy.AutonomyAutonomous, 5, 5
	s.chat = append(s.chat,
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessagePoke, TaskID: labID(617), At: ago(40 * time.Minute),
			Content: `Started #617 "Chat form: center the Send button next to the two-row textarea".`, Decider: "autonomous level · started by the agent"},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessageEscalation, TaskID: labID(603), At: ago(28 * time.Minute),
			Content: "#603 needs you: Which store keeps the nightly board snapshots? Postgres shares the factory's backups; SQLite keeps them beside the export and off the cluster.",
			Choices: []string{"Postgres", "SQLite", "Ask the author"}},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", Kind: factoryd.MessageAsk, At: ago(20 * time.Minute),
			Content: "One snapshot a night, or one an hour?", Choices: []string{"One a night", "One an hour"}},
		factoryd.ChatMessage{Conversation: 5, Role: "user", Content: "One a night", At: ago(18 * time.Minute)})
	s.seen[5] = int64(len(s.chat))
	return s
}

// wakeScene is the threads scene with the inbox open at autonomous and
// one wake turn in it (the main agent concept): the factory woke the agent
// on #603's ask; the turn's rows are one row that folds to the wake's
// first line and opens on a click, the settled row's fold.
func wakeScene(now time.Time) labScene {
	s := threadsScene(now)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	s.autonomy, s.inbox, s.open = policy.AutonomyAutonomous, 5, 5
	s.chat = append(s.chat,
		factoryd.ChatMessage{Conversation: 5, Role: "user", BirthKind: "wake_ask", At: ago(15 * time.Minute),
			Content: "#603: Which store keeps the nightly board snapshots?\n\nRead each card. Settle what your permissions allow. For what stays the human's, record what you found with attach_analysis. An ask already answered is done."},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", At: ago(15*time.Minute - 30*time.Second), ToolCalls: []factoryd.ChatToolCall{{
			ID: "call_w", Name: "card", Arguments: `{"task_id":"#603"}`,
			Result: "#603 asks which store keeps the nightly board snapshots; the plan names Postgres."}}},
		factoryd.ChatMessage{Conversation: 5, Role: "assistant", At: ago(15*time.Minute - 45*time.Second),
			Content: "Both stores hold the snapshots: the plan names Postgres, the brief allows SQLite for one replica. The night read is small; either keeps it."})
	s.seen[5] = int64(len(s.chat))
	return s
}

// compactedScene is the threads scene with the open thread compacted: its
// first turns fold behind one line, the summary turn and the last two
// turns follow (roadmap 239, C14).
func compactedScene(now time.Time) labScene {
	s := threadsScene(now)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	earlier := []factoryd.ChatMessage{
		{Conversation: 4, Role: "user", Content: "Which cards wait on the session store?", At: ago(40 * time.Minute)},
		{Conversation: 4, Role: "assistant", Content: "Two: the login redirect (#8) and the remember-me box (#11).", At: ago(40*time.Minute - 15*time.Second)},
		{Conversation: 4, Role: "user", Content: "Keep #11 in the backlog until the store is settled.", At: ago(38 * time.Minute)},
		{Conversation: 4, Role: "assistant", Content: "#11 stays in the backlog.", At: ago(38*time.Minute - 10*time.Second)},
	}
	s.chat = append(slices.Clone(s.chat[:5]), append(earlier, s.chat[5:]...)...)
	s.summaries = map[int64]factoryd.ThreadSummary{4: {Text: "Sebastian asked which cards wait on the session store: the login redirect (#8) and the remember-me box (#11). #11 stays in the backlog until the store is settled.", Upto: 9}}
	s.seen[4] = 11
	return s
}

// assistedScene is the factory's own code at autonomy assisted: the
// agent started the login card itself (its applied move in the chat, the
// event naming it the decider, the inbox's poke with its decider line),
// one card works, one backlog card waits, and the board head's selector
// shows the level.
func assistedScene(now time.Time) labScene {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	running := factoryd.BoardTask{Task: factoryd.Task{ID: "t-a1", Kind: "agent", Status: "running", Gate: factoryd.GateSession, Lane: policy.LaneReady,
		Process: "spec", Repository: "sf/code", IssueNumber: 7, Size: "small", BudgetEUR: 2, SpentEUR: 0.31, CreatedAt: ago(40 * time.Minute),
		Objective: "Speed up the find box.\n\nAcceptance:\n- The find answers in under 100 ms\n"}}
	moved := factoryd.BoardTask{Task: factoryd.Task{ID: "t-a2", Kind: "agent", Status: "queued", Lane: policy.LaneReady,
		Process: "spec", Repository: "sf/code", IssueNumber: 8, Size: "small", BudgetEUR: 2, CreatedAt: ago(30 * time.Minute),
		Objective: "Fix the login redirect.\n\nAcceptance:\n- A signed-in member lands on the board\n"}}
	waiting := factoryd.BoardTask{Task: factoryd.Task{ID: "t-a3", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog,
		Process: "spec", Repository: "sf/code", IssueNumber: 9, Size: "small", CreatedAt: ago(2 * time.Hour),
		Objective: "Trim the roll notes.\n\nAcceptance:\n- The roll comment fits on one line\n"}}
	msgs := []factoryd.ChatMessage{
		{Role: "user", Content: "Start the login redirect card; the slots are free.", At: ago(9 * time.Minute)},
		{Role: "assistant", At: ago(9*time.Minute - 5*time.Second), ToolCalls: []factoryd.ChatToolCall{{
			ID: "call_1", Name: "move_card", Arguments: `{"task_id":"t-a2","lane":"ready"}`, ReadAt: ago(9*time.Minute - 5*time.Second),
			Result: "Moved #8 to lane ready; it leaves the backlog once factoryd picks it up."}}},
		{Role: "assistant", At: ago(9*time.Minute - 3*time.Second), Content: "#8 is queued for a work slot; #7 runs its session. The ready check passed: criteria, size, budget and room."},
		{Conversation: 2, Role: "assistant", Kind: factoryd.MessagePoke, TaskID: "t-a2", At: ago(9*time.Minute - 4*time.Second),
			Content: `Started #8 "Fix the login redirect".`, Decider: "assisted level · started by the agent"},
	}
	events := []factoryd.Event{
		{Kind: factoryd.EventCardMoved, TaskID: "t-a2", IssueNumber: 8, Actor: "sebastian", Via: factoryd.ViaChat, At: ago(9 * time.Minute),
			Detail: map[string]any{"lane": "ready", "autonomy": "assisted", "merge_rule": "human", "decider": "agent",
				"chat_task": factoryd.ChatTaskID("ai-software-factory-code", now)}},
	}
	return labScene{autonomy: policy.AutonomyAssisted, tasks: []factoryd.BoardTask{running, moved, waiting}, inbox: 2, open: 1,
		chat: msgs, events: events, spend: 1.2, chatSpend: 0.18, drawer: "t-a1"}
}

// labDocs are the code project's documents in the lab's sf/docs: the
// roadmap with its rounds and the plans they name, linked the way the
// repository links them (roadmap 277).
var labDocs = map[string]string{
	"docs/roadmap.md": "# The roadmap\n\n" +
		"The factory builds itself in rounds; a round ends with its cards merged and its live check run.\n\n" +
		"## The rounds\n\n" +
		"- **5. The documents in the screen.** Sources: docs/plans/2026-10-15-documents-in-the-screen.md. Where it stands: in progress.\n" +
		"- **4. The chat beside the board.** Sources: docs/plans/2026-09-24-build-plan.md. Where it stands: done.\n\n" +
		"The [build plan](plans/2026-09-24-build-plan.md) and the [documents plan](plans/2026-10-15-documents-in-the-screen.md) hold each round's cards; the architecture lives on the [wiki](https://example.com/wiki).\n",
	"docs/plans/2026-09-24-build-plan.md": "# The build plan\n\n" +
		"Four cards: the event stream, the board, the drawer, the chat.\n\n" +
		"The board polls nothing; the stream pushes every change. Back to the [roadmap](../roadmap.md).\n",
	"docs/plans/2026-10-15-documents-in-the-screen.md": "# The documents in the screen\n\n" +
		"The documents read in the drawer over the board, never over the chat.\n\n" +
		"1. The read: rendering, the endpoint, the drawer's mode\n" +
		"2. The write: the editor, the indicator\n" +
		"3. The backlog: the pull\n\n" +
		"Back to the [roadmap](../roadmap.md).\n",
}

// labPRFiles are the lab's pull requests' changed files by Forgejo path.
var labPRFiles = map[string]string{
	"/api/v1/repos/sf/code/pulls/609/files": `[{"filename":"internal/console/static/console.css","status":"modified","additions":64,"deletions":21},` +
		`{"filename":"internal/console/templates/board.html","status":"modified","additions":3,"deletions":2},` +
		`{"filename":"internal/console/face_test.go","status":"modified","additions":48,"deletions":0},` +
		`{"filename":"internal/console/static/theme.js","status":"modified","additions":4,"deletions":1}]`,
	"/api/v1/repos/sf/code/pulls/622/files": `[{"filename":"internal/factoryd/pulse.go","status":"modified","additions":37,"deletions":9},` +
		`{"filename":"internal/factoryd/pulse_test.go","status":"modified","additions":81,"deletions":4}]`,
}

// labSeed replaces what every fake answers with scenario name's data and
// answers the card its drawer shots open.
func (r *rig) labSeed(name string) (string, error) {
	scene, ok := labScenes[name]
	if !ok {
		return "", fmt.Errorf("no scenario %q: busy, idle, fire, quiet, assisted, threads, compacted, poke, escalation or history", name)
	}
	now := time.Now()
	s := scene(now)
	r.tasks.mu.Lock()
	r.tasks.tasks, r.tasks.deps, r.tasks.plans, r.tasks.questions = s.tasks, s.deps, s.plans, s.questions
	r.tasks.mu.Unlock()
	r.approvals.mu.Lock()
	r.approvals.rows = s.approvals
	r.approvals.mu.Unlock()
	labObjects.Lock()
	clear(r.objects)
	for k, v := range s.objects {
		r.objects[k] = []byte(v)
	}
	labObjects.Unlock()
	// the fake answers a card's newest event by its place in the rows
	slices.SortStableFunc(s.events, func(a, b factoryd.Event) int { return a.At.Compare(b.At) })
	r.events.mu.Lock()
	r.events.rows = nil
	for i, e := range s.events {
		e.ID, e.Repository = int64(i+1), "sf/code"
		r.events.rows = append(r.events.rows, e)
	}
	r.events.next = int64(len(s.events))
	r.events.mu.Unlock()
	r.chat.mu.Lock()
	r.chat.msgs, r.chat.started, r.chat.proposals, r.chat.next = map[string][]factoryd.ChatMessage{}, nil, nil, int64(len(s.chat))
	r.chat.titles, r.chat.seen, r.chat.cleared, r.chat.summaries, r.chat.compacts =
		nil, map[string]int64{}, map[string]int64{}, map[string]factoryd.ThreadSummary{}, map[string]int{}
	r.chat.inboxes, r.chat.archived = nil, map[string]bool{}
	for _, n := range s.archived {
		r.chat.archived[fmt.Sprint("sf/code sebastian ", n)] = true
	}
	if s.inbox > 0 {
		r.chat.inboxes = map[string]factoryd.ChatConversation{"sf/code sebastian": {Number: s.inbox, Title: factoryd.InboxTitle,
			State: factoryd.ThreadActive, Inbox: true, FirstAt: now.Add(-time.Hour), LastAt: now.Add(-time.Hour)}}
	}
	for i, m := range s.chat {
		m.ID, m.Conversation = int64(i+1), cmp.Or(m.Conversation, 1)
		r.chat.msgs["sf/code sebastian"] = append(r.chat.msgs["sf/code sebastian"], m)
	}
	for n, upto := range s.seen {
		r.chat.seen[fmt.Sprint("sf/code sebastian ", n)] = upto
	}
	for n, sum := range s.summaries {
		r.chat.summaries[fmt.Sprint("sf/code sebastian ", n)] = sum
		r.chat.compacts[fmt.Sprint("sf/code sebastian ", n)] = 1
	}
	if len(s.chat) > 0 {
		r.chat.started = map[string]int64{"sf/code sebastian": cmp.Or(s.open, s.chat[len(s.chat)-1].Conversation)}
	}
	for _, p := range s.proposals {
		r.chat.proposals = append(r.chat.proposals, &fakeProposal{ChatProposal: p})
	}
	for _, p := range s.usedProposals {
		// the press sits 16 min back, whatever its row says: only the fact
		// of a press renders, never its time
		r.chat.proposals = append(r.chat.proposals, &fakeProposal{ChatProposal: p, used: now.Add(-16 * time.Minute)})
	}
	r.chat.atts = nil
	for _, a := range s.attachments {
		r.chat.atts = append(r.chat.atts, storedAttachment{repository: "sf/code", login: "sebastian", a: a})
	}
	r.chat.spend = []spendRow{{"sf/code", now, s.spend - s.chatSpend, false}, {"sf/code", now, s.chatSpend, true}}
	r.chat.mu.Unlock()
	r.mirrors.mu.Lock()
	r.mirrors.rows = map[string]factoryd.Mirror{}
	if s.github != "" {
		r.mirrors.rows["ai-software-factory-code "+factoryd.MirrorExport] = factoryd.Mirror{Project: "ai-software-factory-code",
			Direction: factoryd.MirrorExport, Remote: "sebastianrueckerai/ai-software-factory-code", State: s.github,
			Mode: factoryd.ModePullRequests, Since: now.AddDate(0, 0, -9)}
	}
	r.mirrors.mu.Unlock()
	r.ledger.costs = labCosts(now, s.spend, s.tasks)
	r.alerts.rows = s.alerts
	r.resources.rows = s.resources
	r.resources.use = map[string]cloudres.EnvelopeUse{"sf/code": {OpenServers: 1, MonthEUR: 0.43}}
	r.resources.standing = map[string]cloudres.StandingUse{"demo-app/demo-app": {Count: 1, MonthlyEUR: 5.99}}
	// the cluster's nightly backup and dump, last run at their latest
	// 02:00 and 02:30 UTC; the dump's last run failed
	nightly := func(minute int) time.Time {
		t := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 2, minute, 0, 0, time.UTC)
		if t.After(now) {
			t = t.AddDate(0, 0, -1)
		}
		return t
	}
	r.c.cfg.Jobs = fakeJobs{rows: []factoryd.CronJobState{
		{Kind: factoryd.KindScheduledBackup, Namespace: "platform", Name: "sf-cluster-daily", Schedule: "0 0 2 * * *", LastScheduled: nightly(0)},
		{Kind: factoryd.KindCronJob, Namespace: "forgejo", Name: "forgejo-dump", Schedule: "30 2 * * *", LastScheduled: nightly(30),
			LastSuccessful: nightly(30).AddDate(0, 0, -1)},
	}}
	r.routines.mu.Lock()
	r.routines.views, r.routines.runs, r.routines.skips, r.routines.ran = slices.Clone(s.routines), map[string][]factoryd.RoutineRun{}, nil, nil
	for name, runs := range s.runs {
		r.routines.runs[labProject+"/"+name] = runs
	}
	// a Skip once moves the routine's next run on, as factoryd's views do,
	// and its undo puts the scene's view back
	r.routines.onSkip = func(_, name string, skip bool) {
		i := slices.IndexFunc(r.routines.views, func(v factoryd.RoutineView) bool { return v.Routine.Name == name })
		if i < 0 {
			return
		}
		v := &r.routines.views[i]
		if !skip {
			*v = s.routines[i]
			return
		}
		v.State.SkipNext, v.State.SkipBy = true, "sebastian"
		if every := v.Routine.MergesEvery(); every > 0 {
			v.MergesLeft += every
			v.NextWords = fmt.Sprintf("after %d more merges", v.MergesLeft)
			return
		}
		labNext(v, now, s.routinesAt, s.routinesAt.Add(v.Next.Sub(now)-30*time.Second))
	}
	r.routines.mu.Unlock()
	r.c.turns = newTurnGate()
	for _, n := range s.working {
		r.c.turns.enter(threadKey("sf/code", "sebastian", n))
	}
	if p, ok := r.ledger.files["ai-software-factory-code"]; ok {
		p.Autonomy = cmp.Or(s.autonomy, policy.AutonomyManual)
		r.ledger.files["ai-software-factory-code"] = p
	}
	r.c.mu.Lock()
	r.c.checks = nil
	r.c.mu.Unlock()
	return s.drawer, nil
}

// labCosts is the ledger of the 14 days to now: total spread over them by
// fixed weights, each split into the ledger's kinds, and each card's
// spend on its model and its review.
func labCosts(now time.Time, total float64, tasks []factoryd.BoardTask) policy.Costs {
	weights := []float64{3.1, 4.9, 2.4, 1.0, 5.7, 6.1, 4.0, 3.6, 7.5, 5.2, 2.9, 4.7, 6.0, 4.4}
	var sum float64
	for _, w := range weights {
		sum += w
	}
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	c := policy.Costs{From: now.AddDate(0, 0, -13).UTC().Format(policy.DateLayout), To: now.UTC().Format(policy.DateLayout)}
	left := total
	for i, w := range weights {
		eur := round(total * w / sum)
		if i == len(weights)-1 {
			eur = round(left)
		}
		left -= eur
		model, tool, resource := round(eur*0.86), round(eur*0.02), round(eur*0.07)
		c.Days = append(c.Days, policy.DayCost{Day: now.AddDate(0, 0, i-13).UTC().Format(policy.DateLayout), Total: eur,
			ByKind: map[string]float64{policy.KindModel: model, policy.KindTool: tool, policy.KindResource: resource,
				policy.KindSettlement: round(eur - model - tool - resource)}})
	}
	for _, bt := range tasks {
		if bt.SpentEUR == 0 {
			continue
		}
		review := round(bt.SpentEUR * 0.12)
		c.Tasks = append(c.Tasks, policy.TaskCost{TaskID: bt.ID, EUR: bt.SpentEUR, Lines: []policy.CostLine{
			{Kind: policy.KindModel, Ref: cmp.Or(bt.ModelAlias, "coding-cheap"), EUR: bt.SpentEUR - review},
			{Kind: policy.KindModel, Ref: "review/review", EUR: review}}})
	}
	slices.SortFunc(c.Tasks, func(a, b policy.TaskCost) int { return cmp.Compare(b.EUR, a.EUR) })
	return c
}

// labID is issue's task id, stable across scenarios.
func labID(issue int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprint("lab ", issue)))
	return "t-" + hex.EncodeToString(sum[:6])
}

// labCard is issue's card of the code project, filed age before now with
// its issue's title and body as factoryd builds the objective; one that
// has had a session ran it on coding-cheap.
func labCard(now time.Time, issue int64, status, lane string, age time.Duration, title, body string) factoryd.Task {
	alias := "coding-cheap"
	if slices.Contains([]string{"backlog", "queued", "waiting-deps", "closed"}, status) {
		alias = ""
	}
	return factoryd.Task{ID: labID(issue), Kind: "agent", Status: status, Lane: lane, Process: "spec", Repository: "sf/code",
		IssueNumber: issue, Objective: title + "\n\n" + body, Source: "webhook", Author: "sebastian", AuthorWriter: true,
		RiskClass: "standard", BudgetEUR: 2, ModelAlias: alias, CreatedAt: now.Add(-age)}
}

// finished ends t d before now after spending eur.
func finished(t factoryd.Task, now time.Time, d time.Duration, eur float64) factoryd.Task {
	end := now.Add(-d)
	t.FinishedAt, t.SpentEUR = &end, eur
	return t
}

func labEvent(issue int64, kind, actor, via string, at time.Time, detail map[string]any) factoryd.Event {
	return factoryd.Event{At: at, TaskID: labID(issue), IssueNumber: issue, Kind: kind, Actor: actor, Via: via, Detail: detail}
}

// labStatus is factoryd's row of issue's card entering status and gate.
func labStatus(issue int64, status, gate string, at time.Time) factoryd.Event {
	d := map[string]any{"status": status}
	if gate != "" {
		d["gate"] = gate
	}
	return labEvent(issue, factoryd.EventTaskStatus, factoryd.FactoryDecider, factoryd.ViaFactoryd, at, d)
}

// jsonl is rows as JSON lines, a timeline or a transcript.
func jsonl(rows ...map[string]any) string {
	var b strings.Builder
	for _, row := range rows {
		line, _ := json.Marshal(row)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// checksStep is a checks step of four jobs ending at end; failed names the
// job that failed, "" for none.
func checksStep(start, end time.Time, failed string) map[string]any {
	var jobs []any
	state := "success"
	for _, name := range []string{"test", "vet", "lint", "acceptance"} {
		st := "success"
		if name == failed {
			st, state = "failure", "failure"
		}
		jobs = append(jobs, map[string]any{"name": name, "status": st, "queue_s": 12, "run_s": 140})
	}
	return map[string]any{"step": "checks", "time": stamp(end), "started": stamp(start), "state": state, "jobs": jobs}
}

// pushedTimeline is a card's timeline: one session on alias from start
// that pushed, and checks with failed ("" for none).
func pushedTimeline(start time.Time, alias, failed string) []map[string]any {
	return []map[string]any{
		{"step": "intake", "time": stamp(start.Add(30 * time.Second)), "started": stamp(start), "source": "webhook"},
		{"step": "session_start", "time": stamp(start.Add(time.Minute)), "session": "s-1", "model": alias, "attempt": 1},
		{"step": "session_end", "time": stamp(start.Add(19 * time.Minute)), "session": "s-1", "started": stamp(start.Add(time.Minute)),
			"attempt": 1, "outcome": "pushed"},
		{"step": "pushed", "time": stamp(start.Add(19*time.Minute + 5*time.Second)), "session": "s-1", "commit": "4be1c07"},
		checksStep(start.Add(19*time.Minute+10*time.Second), start.Add(26*time.Minute), failed),
	}
}

func busyScene(now time.Time) labScene {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	s := labScene{drawer: labID(606), github: "1 open pull request", spend: 61.40, chatSpend: 3.85,
		objects: map[string]string{}, questions: map[string][]factoryd.PlanQuestion{}}

	merge := labCard(now, 607, "in-review", "ready", 3*time.Hour, "Chat proposals survive a restart: keep them in Postgres",
		"The proposals live in the console's memory, so a restart between a proposal and its Do it loses it. Store them in a table "+
			"with their expiry and purge the old ones in the settlement pass.")
	merge.PRNumber, merge.Gate, merge.GateSince, merge.SpentEUR, merge.ModelAlias = 612, factoryd.GateApproval, at(25*time.Minute), 1.31, "coding-frontier"
	s.objects["sessions/"+merge.ID+"/timeline.jsonl"] = jsonl(pushedTimeline(merge.CreatedAt, "coding-frontier", "")...)

	esc := factoryd.BoardTask{Task: labCard(now, 603, "waiting-human", "ready", 95*time.Minute, "Board export: keep the nightly snapshots",
		"Write the board to a JSON snapshot every night so a lost database still leaves the cards. Keep 30 snapshots."),
		Escalation: factoryd.AgentAsks + "Which store keeps the nightly board snapshots? Postgres shares the factory's backups; " +
			"SQLite keeps them beside the export and off the cluster.",
		EscalatedAt: ago(28 * time.Minute), Choices: []string{"Postgres", "SQLite", "Ask the author"}, EscalationAlert: 41}
	esc.SpentEUR, esc.Usage.Attempts = 0.64, 1

	plan := labCard(now, 558, "waiting-human", "plan", 64*time.Minute, "Console: a project's activity feed",
		"Show each project's events as a feed beside the board: who did what, by which way, when.\nThe feed reads the events table; no new store.\n"+
			"A writer filters it by card and by kind.\nIt pages back 500 rows at most.\nAcceptance: the feed shows a steer within a second of its row.")
	plan.BudgetEUR, plan.SpentEUR, plan.ModelAlias = 5, 0.38, "coding-frontier"
	planJSON, _ := json.Marshal(factoryd.Plan{Summary: "Four cards: the feed's read model first, then the page and the filters side by side, then the live rows.",
		Cards: []factoryd.PlanCard{
			{Key: "feed-read", Role: factoryd.RoleFoundation, Title: "Events: a paged read of a project's feed", Size: "small"},
			{Key: "feed-page", Role: factoryd.RoleFeature, Title: "Console: the feed page beside the board", Size: "medium", DependsOn: []string{"feed-read"}},
			{Key: "feed-filters", Role: factoryd.RoleFeature, Title: "Feed filters: by card and by kind", Size: "small", DependsOn: []string{"feed-read"}},
			{Key: "feed-live", Role: factoryd.RoleIntegration, Title: "Live rows: the stream appends to the feed", Size: "medium",
				DependsOn: []string{"feed-page", "feed-filters"}},
		}})
	const planSHA = "9f1c2be07d4a6c31e8b5f0a2d9c47e1186b3fa0c25d7e94b1a60c8f3d2e5b7a9"
	s.plans = map[string][]factoryd.PlanRevision{plan.ID: {{TaskID: plan.ID, Revision: 1, Plan: planJSON, SHA256: planSHA, CreatedAt: ago(12 * time.Minute)}}}
	s.questions[plan.ID+" 1"] = []factoryd.PlanQuestion{{Text: "How far back does the feed page by default?", Choices: []string{"One day", "One week", "The 500 newest rows"}}}

	drawer := labCard(now, 606, "running", "ready", 72*time.Minute, "Console polish: clamp board chip objectives, theme the scrollbar",
		"Long objectives push the chips off the card; clamp them at three lines. Theme every scrollbar with the page's colours.")
	drawer.PRNumber, drawer.Gate, drawer.GateSince, drawer.SpentEUR, drawer.ModelAlias = 609, factoryd.GateSession, at(37*time.Minute), 1.72, "coding-frontier"
	tl := pushedTimeline(drawer.CreatedAt, "coding-cheap", "acceptance")
	s2 := ago(37 * time.Minute)
	tl = append(tl, map[string]any{"step": "session_start", "time": stamp(s2), "session": "s-2", "model": "coding-frontier", "attempt": 2})
	s.objects["sessions/"+drawer.ID+"/timeline.jsonl"] = jsonl(tl...)
	said := func(d time.Duration, text string) map[string]any {
		return map[string]any{"time": stamp(s2.Add(d)), "kind": "model_call", "detail": map[string]any{"text": text, "cost": 0.02}}
	}
	s.objects["sessions/"+drawer.ID+"/s-2.jsonl"] = jsonl(
		said(40*time.Second, "The acceptance job failed on TestDrawerObjective: the clamp cuts the objective at two lines and the test reads three. "+
			"Reading internal/console/static/console.css first."),
		map[string]any{"time": stamp(s2.Add(55 * time.Second)), "kind": "tool_call", "detail": map[string]any{"tool": "read"}},
		said(4*time.Minute, "The clamp sits on `.card .title` and on the drawer's headline alike. I scope it to the board's chips and leave the drawer unclamped."),
		said(23*time.Minute, "Running go test ./internal/console -run 'TestDrawer|TestCardFace'."),
		said(31*time.Minute, "Both pass. Next: the scrollbar colours for the dark and the light theme."),
	)

	run2 := labCard(now, 611, "running", "ready", 22*time.Minute, "Live check 3.11: split pre-check runs on the parent diff",
		"The split's pre-check reads the child's diff, so a split card fails a check its parent passed. Run it on the parent's diff.")
	run2.Gate, run2.GateSince, run2.SpentEUR = factoryd.GateSession, at(21*time.Minute), 0.41
	run3 := labCard(now, 614, "running", "ready", 47*time.Minute, "Fresh-context and aged-read acceptance tests, the five defects fixed",
		"Add the acceptance tests for a fresh context and an aged read, and fix the five defects the review found.")
	run3.Gate, run3.GateSince, run3.SpentEUR = factoryd.GateSession, at(9*time.Minute), 0.93

	backlog := labCard(now, 598, "backlog", "backlog", 50*time.Hour, "README.md: extend the intro",
		"Say in the first paragraph what the factory is for and who runs it.")
	deps := factoryd.BoardTask{Task: labCard(now, 616, "waiting-deps", "ready", 70*time.Minute, "Board export: a download button on the costs page",
		"Offer the newest snapshot as a download beside the costs table."), WaitingOn: "#603, #611"}
	queued := factoryd.BoardTask{Task: labCard(now, 617, "queued", "ready", 40*time.Minute, "Chat form: center the Send button next to the two-row textarea",
		"The Send button sits at the textarea's top edge; center it on the two rows."), Dependencies: "#611, #614"}
	child := func(issue int64, status, key, title string, age time.Duration) factoryd.Task {
		k := labCard(now, issue, status, "ready", age, title, fmt.Sprintf("Card %s of plan #531, accepted by sebastian. Written by the planner.", key))
		k.Source, k.Parent, k.PlanKey = factoryd.SourcePlan, labID(531), key
		return k
	}
	review := labCard(now, 601, "in-review", "ready", 4*time.Hour, "Pulse: recompute on a card's status change",
		"The pulse recomputes every ten minutes; a card that fails waits that long to show. Recompute on each task_status row, at most once a minute.")
	review.PRNumber, review.Gate, review.GateSince, review.SpentEUR = 605, factoryd.GateMergeQueue, at(8*time.Minute), 0.87
	s.objects["sessions/"+review.ID+"/timeline.jsonl"] = jsonl(pushedTimeline(review.CreatedAt, "coding-cheap", "")...)

	planDone := finished(labCard(now, 531, "done", "plan", 30*time.Hour, "Console: the card drawer",
		"Open a card beside the board with its overview, its activity, its files and its session."), now, 26*time.Hour, 0.52)
	planDone.BudgetEUR, planDone.ModelAlias = 5, "coding-frontier"
	exported := finished(labCard(now, 596, "done", "ready", 9*time.Hour, "Chat: the Undo of a move restores the lane",
		"An undone move leaves the card in ready; put it back where it was."), now, 7*time.Hour, 0.33)
	exported.GitHubPR, exported.GitHubPRURL, exported.GitHubPRState = 14, "https://github.com/sebastianrueckerai/ai-software-factory-code/pull/14", "open"
	failed := finished(labCard(now, 597, "failed", "ready", 6*time.Hour, "Gateway: retry a 529 from the provider with backoff",
		"An overloaded provider answers 529; retry it twice with backoff before the session ends."), now, 5*time.Hour, 2.00)
	failed.ModelAlias = "coding-frontier"
	closed := finished(labCard(now, 590, "closed", "backlog", 5*24*time.Hour, "Board: drag cards between lanes",
		"Drag a card from the backlog into ready."), now, 20*time.Hour, 0)

	s.tasks = []factoryd.BoardTask{{Task: merge}, esc, {Task: plan}, {Task: drawer}, {Task: run2}, {Task: run3}, {Task: backlog}, deps, queued,
		{Task: child(533, "queued", "drawer-files", "Drawer: the Files tab lists the pull request's changes", 26*time.Hour)},
		{Task: review}, {Task: planDone},
		{Task: finished(child(532, "done", "drawer-overview", "Drawer: the Overview tab", 26*time.Hour), now, 22*time.Hour, 0.29)},
		{Task: finished(child(534, "done", "drawer-chat", "Drawer: the session's chat", 26*time.Hour), now, 19*time.Hour, 0.61)},
		{Task: exported},
		{Task: finished(labCard(now, 600, "done", "ready", 5*time.Hour, "Costs: the day cap line under the table",
			"Show today's spend against the day cap under the costs table."), now, 3*time.Hour, 0.18)},
		{Task: finished(child(535, "failed", "drawer-activity", "Drawer: the Activity tab", 26*time.Hour), now, 18*time.Hour, 1.12)},
		{Task: failed}, {Task: closed},
	}
	s.deps = map[string][]string{labID(617): {labID(611), labID(614)}, labID(616): {labID(603), labID(611)}}

	mergeSpec, _ := json.Marshal(map[string]any{
		"risk_class": "high", "migration": true, "migrations": []string{"internal/factoryd/migrations/0031_chat_proposals.sql"},
		"verdict_summary": "Moves the proposals into a table with an expiry index; the settlement pass purges them.",
		"review": map[string]any{"verdict": "approve", "summary": "Moves the proposals into a table with an expiry index; the settlement pass purges them.",
			"findings": []string{"chat.go: the purge deletes without a batch limit; fine at today's volume"}},
		"head_sha": "b7e04c19d2a3", "recommendation": "Merge it: the migration only adds a table and an index, and the acceptance test restarts the console between a proposal and its Do it.",
		"why_human": []string{"the project's merge rule is human", "risk class high is requires_approval",
			"migration: schema migration, human review required: internal/factoryd/migrations/0031_chat_proposals.sql"},
		"audit": map[string]any{"verdict": "findings", "model": "coding-frontier", "deployment": "zai/glm-5.3", "findings": []map[string]any{
			{"item": "token stored in clear", "file": "internal/factoryd/chat.go", "line": 268,
				"why": "the proposal row keeps its token unhashed; a database reader could replay a Do it within its 30 minutes"}}},
	})
	s.approvals = []factoryd.ApprovalRequest{
		{ID: 51, Kind: factoryd.ApprovalMerge, TaskID: merge.ID, Project: "sf/code", PRNumber: 612, Status: "pending", Spec: mergeSpec, CreatedAt: ago(25 * time.Minute)},
		{ID: 52, Kind: factoryd.ApprovalPlan, TaskID: plan.ID, Project: "sf/code", Status: "pending",
			Spec: json.RawMessage(`{"revision":1,"sha256":"` + planSHA + `"}`), CreatedAt: ago(12 * time.Minute)},
	}

	s.events = []factoryd.Event{
		labEvent(603, factoryd.EventQuestionAsked, factoryd.FactoryDecider, factoryd.ViaFactoryd, ago(28*time.Minute),
			map[string]any{"question": "Which store keeps the nightly board snapshots?", "choices": esc.Choices}),
		labStatus(606, "running", factoryd.GateSession, s2),
		labEvent(606, factoryd.EventCardSteered, "sebastian", factoryd.ViaButton, ago(17*time.Minute),
			map[string]any{"text": "Keep the clamp CSS-only; the drawer shows the whole objective."}),
		labStatus(607, "in-review", factoryd.GateApproval, ago(25*time.Minute)),
		labStatus(611, "running", factoryd.GateSession, ago(21*time.Minute)),
		labEvent(611, factoryd.EventCardSteered, "sebastian", factoryd.ViaChat, ago(3*time.Minute),
			map[string]any{"text": "Compare against the parent's merge base, not main."}),
		labStatus(614, "running", factoryd.GateSession, ago(9*time.Minute)),
		labEvent(616, factoryd.EventDependencySet, "sebastian", factoryd.ViaChat, ago(26*time.Minute), nil),
		labEvent(617, factoryd.EventCardMoved, "sebastian", factoryd.ViaButton, ago(12*time.Minute), map[string]any{"lane": "ready"}),
		labStatus(533, "queued", "", ago(24*time.Minute)),
		labStatus(601, "in-review", factoryd.GateMergeQueue, ago(8*time.Minute)),
		labEvent(598, factoryd.EventCardFiled, "sebastian", factoryd.ViaChat, ago(50*time.Hour), nil),
	}

	s.chat, s.proposals, s.usedProposals = busyChat(now, drawer, queued.Task, factoryd.CardTitle)
	// img1 of the image exchange: row 13's screenshot, its bytes in the
	// object store under the attachments' own key
	shot := labShot()
	s.attachments = []factoryd.ChatAttachment{{ID: 901, Conversation: 1, MessageID: 13, Name: "img1", FileName: "run.png",
		ContentType: "image/png", Bytes: int64(len(shot)), At: now.Add(-4 * time.Minute)}}
	s.objects["attachments/1/901"] = shot
	s.alerts = []factoryd.Alert{
		{ID: 41, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: esc.ID, RaisedAt: ago(28 * time.Minute)},
		{ID: 40, Kind: factoryd.AlertSupervisorTrip, Source: "no_write", TaskID: drawer.ID, RaisedAt: ago(58 * time.Minute),
			ResolvedAt: at(57 * time.Minute), Verdict: factoryd.VerdictReal, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "redirect", "diagnosis": "stop rereading console.css; run the failing acceptance test"}},
		{ID: 39, Kind: factoryd.AlertGateBound, Source: "WaitChecks", TaskID: review.ID, RaisedAt: ago(3 * time.Hour), BoundS: 600,
			ResolvedAt: at(170 * time.Minute), Verdict: factoryd.VerdictNoise, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "wait", "diagnosis": "the runner queue held two jobs; both started"}},
		{ID: 37, Kind: factoryd.AlertMonthCap, Source: "50", RaisedAt: ago(4 * 24 * time.Hour), Fingerprint: now.UTC().Format("2006-01") + "/50"},
	}
	lease := now.Add(2 * time.Hour).Truncate(time.Minute)
	s.resources = []cloudres.Resource{
		{ID: 70, Kind: "firewall", Repo: "sf/code", OwnerRef: run3.ID, HoldID: 12, State: "live", LeaseExpires: lease, CreatedAt: ago(8 * time.Minute)},
		{ID: 71, Kind: "server", Repo: "sf/code", OwnerRef: run3.ID, HoldID: 12, State: "live", Size: "cx23", Address: "203.0.113.24",
			LeaseExpires: lease, PricePerHourEUR: 0.0072, AccruedEUR: 0.0072, Purpose: "acceptance run against a fresh Postgres", MonthlyEUR: 4.99,
			CreatedAt: ago(8 * time.Minute)},
		{ID: 64, Kind: "server", Repo: "demo-app/demo-app", OwnerRef: labID(480), HoldID: 9, State: "live", Size: "cx23",
			Lifetime: cloudres.LifetimeStanding, PromotedBy: "sebastian", MonthlyEUR: 5.99, CreatedAt: ago(9 * 24 * time.Hour)},
		{ID: 58, Kind: "server", Repo: "sf/code", OwnerRef: labID(571), HoldID: 7, State: "deleted", Size: "cx33", ReleasedBy: "sebastian",
			LeaseExpires: ago(68 * time.Hour), PricePerHourEUR: 0.0144, AccruedEUR: 0.0576, CreatedAt: ago(72 * time.Hour)},
	}
	s.addRoutines(now, false)
	return s
}

// labShot is the failing acceptance run's screenshot, one PNG the whole
// lab shares: a dark terminal, its error bar in red at the foot.
var labShot = sync.OnceValue(func() string {
	const w, h = 320, 200
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill := func(r image.Rectangle, c color.RGBA) { draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src) }
	fill(image.Rect(0, 0, w, h), color.RGBA{R: 0x17, G: 0x1a, B: 0x21, A: 0xff})
	for y := 20; y < h-32; y += 22 {
		fill(image.Rect(12, y, w-24, y+9), color.RGBA{R: 0x24, G: 0x2a, B: 0x35, A: 0xff})
	}
	fill(image.Rect(0, h-24, w, h), color.RGBA{R: 0xd6, G: 0x4a, B: 0x5a, A: 0xff})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.String()
})

// busyChat is sebastian's conversation in the busy scenario: roadmap 300
// filed as a card — an ask pressed, a refused Do it that self-corrects
// with its acceptance tests — then a question the agent answers after
// reading #606, two proposals for #606's session and #617, and a pasted
// screenshot the agent reads by tool, its ask still open.
func busyChat(now time.Time, running, queued factoryd.Task, title func(factoryd.Task) string) ([]factoryd.ChatMessage, []factoryd.ChatProposal, []factoryd.ChatProposal) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	ref := func(t factoryd.Task) string { return fmt.Sprintf("#%d %s %q", t.IssueNumber, t.ID, title(t)) }
	steerArgs := fmt.Sprintf(`{"task_id":%q,"text":"Keep the drawer's objective unclamped; clamp only the board's chips."}`, running.ID)
	depArgs := fmt.Sprintf(`{"task_id":%q,"depends_on":%q}`, queued.ID, running.ID)
	const fileTitle = "Plan: Documents in the conversation (roadmap 300)"
	const fileBody = "Drag the documents into the chat and read them by tool"
	noTests := `{"title":"` + fileTitle + `","body":"` + fileBody + `","lane":"plan","size":"medium"}`
	withTests := noTests[:len(noTests)-1] + `,"acceptance":["the chat shows the documents a member sends","the agent reads them by tool"]}`
	proposals := []factoryd.ChatProposal{
		{Token: "labSteer606xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", Kind: "proposal", Repository: "sf/code", Login: "sebastian", Tool: "steer",
			Args: json.RawMessage(steerArgs), ExpiresAt: now.Add(30 * time.Minute),
			Description: "Message a running card, " + ref(running) + ", text Keep the drawer's objective unclamped; clamp only the board's chips."},
		{Token: "labDepend617xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", Kind: "proposal", Repository: "sf/code", Login: "sebastian", Tool: "set_dependency",
			Args: json.RawMessage(depArgs), ExpiresAt: now.Add(30 * time.Minute),
			Description: "Set a dependency, " + ref(queued) + ", depends on " + ref(running)},
	}
	used := []factoryd.ChatProposal{
		{Token: "labFile300NoAcceptancexxxxxxxxxxxxxxxxxx", Kind: "proposal", Repository: "sf/code", Login: "sebastian", Tool: "new_card",
			Args: json.RawMessage(noTests), ExpiresAt: now.Add(30 * time.Minute),
			Description: "File a card, title " + fileTitle + ", body " + fileBody + ", lane plan, size medium"},
	}
	filed := factoryd.ChatProposal{Token: "labFile300WithTestsxxxxxxxxxxxxxxxxxxxxxx", Kind: "proposal", Repository: "sf/code", Login: "sebastian",
		Tool: "new_card", Args: json.RawMessage(withTests), ExpiresAt: now.Add(30 * time.Minute),
		Description: "File a card, title " + fileTitle + ", body " + fileBody + ", lane plan, size medium, " +
			"acceptance the chat shows the documents a member sends; the agent reads them by tool"}
	proposals = append(proposals, filed)
	call := func(p factoryd.ChatProposal, id string, read time.Time) factoryd.ChatToolCall {
		return factoryd.ChatToolCall{ID: id, Name: p.Tool, Arguments: string(p.Args), ReadAt: read, Proposal: p.Token,
			Result: "Proposed to sebastian: " + strings.TrimSuffix(p.Description, ".") + ". Nothing runs until they press Do it."}
	}
	ask := func(id, question string, choices []string, at time.Time) factoryd.ChatMessage {
		args, _ := json.Marshal(map[string]any{"question": question, "choices": choices})
		return factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageAsk, Content: question, Choices: choices, At: at,
			ToolCalls: []factoryd.ChatToolCall{{ID: id, Name: "ask", Arguments: string(args), ReadAt: at,
				Result: "Asked the member; this turn ends. A press or their next message answers it."}}}
	}
	msgs := []factoryd.ChatMessage{
		{Role: "user", Content: "File roadmap 300 as a card", At: ago(18 * time.Minute)},
		ask("call_300a", "One card, or one per deliverable?", []string{"One card", "One per deliverable"}, ago(18*time.Minute-9*time.Second)),
		{Role: "user", Content: "One card", At: ago(17 * time.Minute)},
		{Role: "assistant", At: ago(16*time.Minute - 40*time.Second),
			ToolCalls: []factoryd.ChatToolCall{call(used[0], "call_300b", ago(16*time.Minute-40*time.Second))}},
		// the pressed Do it that the ready check refused, and the turn it
		// fed: the filing again, now with its acceptance tests
		{Role: "assistant", At: ago(16*time.Minute - 20*time.Second), ToolCalls: []factoryd.ChatToolCall{
			{ID: "do-" + used[0].Token[:12], Name: "new_card", Arguments: noTests, ReadAt: ago(16*time.Minute - 20*time.Second),
				Result: "Refused: The ready check refuses the filing: card " + fileTitle + ": 0 acceptance tests; want 1 to 10"}}},
		{Role: "assistant", At: ago(15*time.Minute - 50*time.Second), Content: "The ready check wants acceptance tests on a plan card. " +
			"This filing carries two; it waits for your click.",
			ToolCalls: []factoryd.ChatToolCall{call(filed, "call_300c", ago(15*time.Minute-50*time.Second))}},
		{Role: "user", Content: "What is #606 stuck on, and can it finish under its budget?", At: ago(14 * time.Minute)},
		{Role: "assistant", At: ago(14*time.Minute - 4*time.Second), ToolCalls: []factoryd.ChatToolCall{{ID: "call_1", Name: "card",
			Arguments: fmt.Sprintf(`{"task_id":%q}`, running.ID), ReadAt: ago(14*time.Minute - 4*time.Second),
			Result: ref(running) + ": running, gate session for 23 min, session 2 on coding-frontier\nspent €1.72 of €2.00\n" +
				"PR #609: checks 3/4, acceptance failed (TestDrawerObjective)"}}},
		{Role: "assistant", At: ago(14*time.Minute - 9*time.Second), Content: "#606 is in its second session. The first pushed PR #609, then the " +
			"**acceptance** job failed: the clamp cuts the objective at two lines and `TestDrawerObjective` reads three.\n\n" +
			"It has spent €1.72 of its €2.00, so this session is its last under the budget."},
		{Role: "user", Content: "Tell it to keep the drawer unclamped, and hold #617 until #606 is done.", At: ago(12 * time.Minute)},
		{Role: "assistant", At: ago(12*time.Minute - 6*time.Second), ToolCalls: []factoryd.ChatToolCall{
			call(proposals[0], "call_2", ago(12*time.Minute-6*time.Second)), call(proposals[1], "call_3", ago(12*time.Minute-6*time.Second))}},
		{Role: "assistant", At: ago(12*time.Minute - 8*time.Second), Content: "Both wait for your click: the message reaches #606's session at its next step, " +
			"and #617 stays queued until #606 is done."},
		{Role: "user", Content: "The failing run is img1: the acceptance job's screenshot. What broke?", At: ago(4 * time.Minute)},
		{Role: "assistant", At: ago(3*time.Minute - 50*time.Second), Content: "The screenshot shows the failure the session reported: the clamp, " +
			"one CSS rule on the board's chips. Where it lands is yours to say.",
			ToolCalls: []factoryd.ChatToolCall{{ID: "call_301a", Name: "analyze_image",
				Arguments: `{"name":"img1","ask":"What does the failing run show?"}`, ReadAt: ago(3*time.Minute - 50*time.Second),
				Result: "img1 (run.png): The clamp cuts the card's objective at two lines; TestDrawerObjective reads three."}}},
		ask("call_301b", "The fix is one rule: where does it land?", []string{"In #606's session", "A fresh session"}, ago(2*time.Minute-9*time.Second)),
	}
	return msgs, proposals, used
}

// labMonday is the routines round's Monday (C16, design/rounds/04-routines/
// routines.js) at 06:00, when the weekly bump's run 15 is due, on 2026's
// calendar. A scene with the routines stands for a moment of it: their
// runs read as the round's dates, and every age counts back from now.
var labMonday = time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC)

const labProject = "ai-software-factory-code"

// labBumpBrief is the weekly bump's brief, routines/weekly-bump.md.
const labBumpBrief = "The factory pins every outside input of its builds and its cluster: nothing moves until this card moves it. " +
	"Bump each to the newest release of its line in one pull request: the outside images and charts in upstream-images.txt, " +
	"the Debian snapshot, the PostgreSQL versions, the Go toolchain, the downloaded tools."

// The round's four routines of the code project: its weekly bump, the
// factory's template pass, a dependency update and an audit.
var (
	labBump = policy.Routine{Name: "weekly-bump", Title: "Weekly bump", Schedule: []policy.Trigger{{Cron: "0 6 * * 1"}}, Files: policy.FilesHuman,
		Brief: "routines/weekly-bump.md", WhileOpen: policy.WhileOpenHold, Missed: policy.MissedFileOnce, Tell: policy.TellInbox}
	labTemplatePass = policy.Routine{Name: "template-pass", Title: "Template pass", Schedule: []policy.Trigger{{Cron: "3 * * * *"},
		{On: policy.OnConfigurationVersion}}, Files: policy.FilesPullRequest, Pass: policy.PassTemplate, WhileOpen: policy.WhileOpenFold, Tell: policy.TellQuiet}
	labDeps = policy.Routine{Name: "dependency-update", Title: "Dependency update", Schedule: []policy.Trigger{{Cron: "0 6 * * 4"}},
		Files: policy.FilesAgent, Skill: "dependency-update", Tell: policy.TellQuiet}
	labAudit = policy.Routine{Name: "audit", Title: "Audit", Schedule: []policy.Trigger{{Merges: 20}}, Files: policy.FilesAgent, Skill: "audit"}
)

// labNext sets v's next run to its first cron run after the design time
// from, worded as factoryd's views word it at the design time at, which
// now stands for. It comes half a minute later, so a page loaded within
// that half minute still reads "in 20 min". A routine without a cron
// trigger keeps its own.
func labNext(v *factoryd.RoutineView, now, at, from time.Time) {
	next, ok := v.Routine.NextCron(from, time.UTC)
	if !ok {
		return
	}
	// soon as factoryd's views have it: under an hour away, the run after
	// a day or more later, 23 h across the spring's clock change
	after, more := v.Routine.NextCron(next, time.UTC)
	v.Next, v.Soon, v.NextWords = now.Add(next.Sub(at)+30*time.Second), next.Sub(at) < time.Hour && (!more || after.Sub(next) >= 23*time.Hour), next.Format("Mon 15:04")
	if next.Sub(at) >= 7*24*time.Hour {
		v.NextWords = next.Format("Mon 2 Jan 15:04")
	}
}

// addRoutines gives s the round's routines, their runs and the cards
// they filed: #781, and #779 of the dependency update in Done. Due is
// Monday 05:40: the weekly bump files in 20 minutes, its last card #781
// done. Held is 06:00: run 15 is held, as #781 of run 14 is still open in
// Needs you.
func (s *labScene) addRoutines(now time.Time, held bool) {
	at := labMonday.Add(-20 * time.Minute)
	if held {
		at = labMonday
	}
	s.routinesAt = at
	run := func(rt policy.Routine, n int, trigger string, due time.Time, took time.Duration, outcome, note string, issue int64) factoryd.RoutineRun {
		r := factoryd.RoutineRun{Project: labProject, Routine: rt.Name, Run: n, Due: due, Started: due, Ended: due.Add(took), Trigger: trigger,
			Outcome: outcome, Note: note, Issue: issue}
		if issue > 0 {
			r.TaskID = labID(issue)
		}
		return r
	}
	week := func(n int) time.Time { return labMonday.AddDate(0, 0, 7*(n-15)) }
	thursday := labMonday.AddDate(0, 0, -4)
	s.runs = map[string][]factoryd.RoutineRun{
		labBump.Name: {
			run(labBump, 14, "cron", week(14), 51*time.Hour, "done", "", 781),
			run(labBump, 13, "cron", week(13), 33*time.Hour, "done", "", 742),
			run(labBump, 12, "cron", week(12), 146*time.Hour, "closed", "", 701),
			run(labBump, 11, "cron", week(11), 0, "skipped", "skipped once by sebastian", 0),
			run(labBump, 10, "cron", week(10), 4*time.Hour, "done", "", 655),
			run(labBump, 9, "cron", week(9), 25*time.Hour, "done", "", 630),
		},
		labTemplatePass.Name: {
			run(labTemplatePass, 1312, "cron", labMonday.Add(-57*time.Minute), 40*time.Second, "no change", "", 0),
			run(labTemplatePass, 1311, "cron", labMonday.Add(-117*time.Minute), 38*time.Second, "no change", "", 0),
		},
		labDeps.Name: {
			run(labDeps, 9, "cron", thursday, 26*time.Hour, "done", "", 779),
			run(labDeps, 8, "cron", thursday.AddDate(0, 0, -7), 5*time.Hour, "done", "", 741),
		},
		labAudit.Name: {run(labAudit, 4, "merges", labMonday.AddDate(0, 0, -12).Add(8*time.Hour+20*time.Minute), 100*time.Minute, "done", "", 702)},
	}
	s.runs[labDeps.Name][0].CostEUR, s.runs[labAudit.Name][0].CostEUR = 0.94, 1.37

	card := func(rt policy.Routine, r factoryd.RoutineRun, status, title, body string) factoryd.Task {
		lane := policy.LaneReady
		if rt.Files == policy.FilesHuman {
			// a human card's body ends in the line factoryd files it with
			lane, body = "", body+"\n\n"+fmt.Sprintf("%s%s · run %d", factoryd.RoutineProvenance, rt.Name, r.Run)
		}
		t := labCard(now, r.Issue, status, lane, at.Sub(r.Started), title, body)
		t.Source, t.Author, t.AuthorWriter = factoryd.SourceRoutine, "factory-agent", false
		if rt.Files == policy.FilesHuman {
			t.Kind, t.BudgetEUR, t.ModelAlias = "human", 0, ""
		}
		if !r.Ended.IsZero() {
			t = finished(t, now, at.Sub(r.Ended), r.CostEUR)
		}
		return t
	}
	bump, status781 := s.runs[labBump.Name], "done"
	if held {
		bump[0].Outcome, bump[0].Ended, status781 = "filed", time.Time{}, "waiting-human"
	}
	k781 := card(labBump, bump[0], status781, "Weekly bump of the pinned outside inputs", labBumpBrief)
	s.tasks = append(s.tasks, factoryd.BoardTask{Task: k781}, factoryd.BoardTask{Task: card(labDeps, s.runs[labDeps.Name][0], "done",
		"Dependency update: go.mod and package.json, patch and minor", "Run the skill dependency-update.")})
	var open *factoryd.Task
	if held {
		s.runs[labBump.Name] = append([]factoryd.RoutineRun{run(labBump, 15, "cron", labMonday, 0, "held", "held: #781 is still open", 0)}, bump...)
		open = &k781
	}

	view := func(rt policy.Routine, st factoryd.RoutineState) factoryd.RoutineView {
		v := factoryd.RoutineView{Project: labProject, Routine: rt, State: st, Lane: "queued", Last: s.runs[rt.Name][0]}
		labNext(&v, now, at, at)
		if every := rt.MergesEvery(); every > 0 {
			v.MergesLeft = every - st.Merges
			v.NextWords = fmt.Sprintf("after %d more merges", v.MergesLeft)
		}
		return v
	}
	b := view(labBump, factoryd.RoutineState{Project: labProject, Routine: labBump.Name, Runs: s.runs[labBump.Name][0].Run})
	b.Open, b.Held = open, held
	s.routines = []factoryd.RoutineView{b,
		view(labTemplatePass, factoryd.RoutineState{Routine: labTemplatePass.Name, Runs: 1312}),
		view(labDeps, factoryd.RoutineState{Project: labProject, Routine: labDeps.Name, Runs: 9}),
		view(labAudit, factoryd.RoutineState{Project: labProject, Routine: labAudit.Name, Merges: 14, Runs: 4}),
	}
}

func idleScene(now time.Time) labScene {
	s := labScene{spend: 144.16, chatSpend: 6.14}
	done := []struct {
		issue int64
		title string
		age   time.Duration
		eur   float64
	}{
		{618, "Console polish: clamp board chip objectives, theme the scrollbar", 3 * time.Hour, 1.64},
		{617, "Chat form: center the Send button next to the two-row textarea", 5 * time.Hour, 0.41},
		{614, "Fresh-context and aged-read acceptance tests, the five defects fixed", 9 * time.Hour, 1.97},
		{611, "Live check 3.11: split pre-check runs on the parent diff", 14 * time.Hour, 0.88},
		{609, "README.md: extend the intro", 20 * time.Hour, 0.12},
		{607, "Chat proposals survive a restart: keep them in Postgres", 26 * time.Hour, 1.31},
		{606, "Pulse: recompute on a card's status change", 30 * time.Hour, 0.87},
		{603, "Board export: keep the nightly snapshots", 34 * time.Hour, 1.12},
		{601, "Costs: the day cap line under the table", 40 * time.Hour, 0.18},
		{600, "Chat: the Undo of a move restores the lane", 46 * time.Hour, 0.33},
		{597, "Gateway: retry a 529 from the provider with backoff", 52 * time.Hour, 2.00},
		{596, "Drawer: the session's chat", 60 * time.Hour, 0.61},
	}
	for i, d := range done {
		status := "done"
		if i == 10 {
			status = "failed"
		}
		k := finished(labCard(now, d.issue, status, "ready", d.age+50*time.Minute, d.title, "Done as the issue asks."), now, d.age, d.eur)
		s.tasks = append(s.tasks, factoryd.BoardTask{Task: k})
	}
	s.tasks = append([]factoryd.BoardTask{
		{Task: labCard(now, 455, "backlog", "backlog", 53*time.Hour, "Live check: close (Stage 16, 3.4)",
			"Close the stage's live check once the restart round has run.")},
		{Task: labCard(now, 178, "backlog", "backlog", 347*time.Hour, "Console: show the configuration version",
			"Show the commit of the configuration in force in the console's footer.")},
	}, s.tasks...)
	s.alerts = []factoryd.Alert{
		{ID: 62, Kind: factoryd.AlertMonthCap, Source: "100", RaisedAt: now.Add(-20 * time.Hour), Fingerprint: now.UTC().Format("2006-01") + "/100"},
		{ID: 55, Kind: factoryd.AlertMonthCap, Source: "80", RaisedAt: now.Add(-3 * 24 * time.Hour), Fingerprint: now.UTC().Format("2006-01") + "/80"},
		{ID: 37, Kind: factoryd.AlertMonthCap, Source: "50", RaisedAt: now.Add(-7 * 24 * time.Hour), Fingerprint: now.UTC().Format("2006-01") + "/50"},
	}
	return s
}

// historyScene is the idle board after a busy week: 320 more cards
// finished, one every 30 minutes, every seventh failed, more than the
// Done column reads at once.
func historyScene(now time.Time) labScene {
	s := idleScene(now)
	titles := []string{"Board: keep the column's scroll across a swap", "Gateway: log the provider's request id",
		"Costs: the week's total under the table", "Drawer: link the pull request's checks", "Chat: the thread list's unread dot",
		"Intake: refuse an issue without a body", "Pulse: name the oldest waiting card", "Alerts: group a flapping rule's alerts"}
	for i := range 320 {
		status := "done"
		if i%7 == 6 {
			status = "failed"
		}
		age := time.Duration(i+1) * 30 * time.Minute
		s.tasks = append(s.tasks, factoryd.BoardTask{Task: finished(labCard(now, int64(1319-i), status, "ready", age+40*time.Minute,
			titles[i%len(titles)], "Done as the issue asks."), now, age, 0.25)})
	}
	return s
}

func fireScene(now time.Time) labScene {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	s := labScene{drawer: labID(620), spend: 97.30, chatSpend: 3.20, objects: map[string]string{}}

	stale := labCard(now, 619, "running", "ready", 90*time.Minute, "Load test: the event stream under 200 followers",
		"Open 200 followers on one project's stream and measure the heartbeat's board reads.")
	stale.Gate, stale.GateSince, stale.SpentEUR = factoryd.GateSession, at(41*time.Minute), 0.88
	s.objects["sessions/"+stale.ID+"/timeline.jsonl"] = jsonl(map[string]any{"step": "session_start", "time": stamp(ago(41 * time.Minute)),
		"session": "s-1", "model": "coding-cheap", "attempt": 1})

	near := labCard(now, 620, "running", "ready", 65*time.Minute, "Pulse: name the failed cards first",
		"A pulse that lists a failure after the running cards hides it; name failed cards first.")
	near.PRNumber, near.Gate, near.GateSince, near.SpentEUR, near.ModelAlias = 622, factoryd.GateSession, at(14*time.Minute), 1.95, "coding-frontier"
	tl := pushedTimeline(near.CreatedAt, "coding-cheap", "test")
	tl = append(tl, map[string]any{"step": "session_start", "time": stamp(ago(14 * time.Minute)), "session": "s-2", "model": "coding-frontier", "attempt": 2})
	s.objects["sessions/"+near.ID+"/timeline.jsonl"] = jsonl(tl...)
	s.objects["sessions/"+near.ID+"/s-2.jsonl"] = jsonl(
		map[string]any{"time": stamp(ago(13 * time.Minute)), "kind": "model_call", "detail": map[string]any{"text": "The test job failed on TestPulseOrder: the failed card comes second. Sorting the bullets by state first."}},
		map[string]any{"time": stamp(ago(5 * time.Minute)), "kind": "model_call", "detail": map[string]any{"text": "€1.95 of €2.00 spent; I push this fix and stop here."}},
	)

	refused := func(d time.Duration) string {
		return jsonl(
			map[string]any{"step": "session_start", "time": stamp(ago(d + 2*time.Minute)), "session": "s-1", "model": "coding-frontier", "attempt": 1},
			map[string]any{"step": "session_end", "time": stamp(ago(d)), "session": "s-1", "attempt": 1, "outcome": "provider_refused"},
			map[string]any{"step": factoryd.StepProviderRefused, "time": stamp(ago(d)), "status": 429, "reason": "budget exhausted", "backoff_s": 120},
		)
	}
	wait1 := labCard(now, 615, "running", "ready", 25*time.Minute, "Gateway: count the provider's 429s per model",
		"Count each refusal per model alias so the pulse can name a provider that is down.")
	wait1.Gate, wait1.GateSince, wait1.SpentEUR, wait1.ModelAlias = factoryd.GateProvider, at(2*time.Minute), 0.06, "coding-frontier"
	s.objects["sessions/"+wait1.ID+"/timeline.jsonl"] = refused(2 * time.Minute)
	wait2 := labCard(now, 621, "running", "ready", 18*time.Minute, "Review: retry the review on a provider refusal",
		"A refused review call fails the gate; retry it after the provider's backoff.")
	wait2.Gate, wait2.GateSince, wait2.SpentEUR, wait2.ModelAlias = factoryd.GateProvider, at(1*time.Minute), 0.02, "coding-frontier"
	s.objects["sessions/"+wait2.ID+"/timeline.jsonl"] = refused(time.Minute)

	capped := factoryd.BoardTask{Task: labCard(now, 610, "waiting-human", "ready", 5*time.Hour, "Board export: import a snapshot back",
		"Read a nightly snapshot back into an empty database; every card keeps its lane and its accent."),
		Escalation: "the session cap is reached: every finding sent to the agent so far goes to a human\n\n" +
			"- acceptance: TestExportRoundTrip fails on the second import\n- review: the importer drops a card's accent",
		EscalatedAt: ago(12 * time.Minute), EscalationAlert: 66}
	capped.SpentEUR, capped.ModelAlias, capped.Usage.Attempts = 1.48, "coding-frontier", 3

	queued := factoryd.BoardTask{Task: labCard(now, 623, "waiting-deps", "ready", 50*time.Minute, "Board export: document the snapshot format",
		"Describe the snapshot's JSON in docs/export.md."), WaitingOn: "#610"}
	f1 := finished(labCard(now, 604, "failed", "ready", 4*time.Hour, "Export: GitHub pull requests for merged cards",
		"Open a GitHub pull request for every card merged since the export's last run."), now, 70*time.Minute, 2.00)
	f1.ModelAlias = "coding-frontier"
	f2 := finished(labCard(now, 609, "failed", "ready", 3*time.Hour, "Fresh-context and aged-read acceptance tests",
		"Add the acceptance tests for a fresh context and an aged read."), now, 35*time.Minute, 1.21)
	done := finished(labCard(now, 602, "done", "ready", 7*time.Hour, "Costs: the day cap line under the table",
		"Show today's spend against the day cap under the costs table."), now, 6*time.Hour, 0.18)
	s.tasks = []factoryd.BoardTask{capped, {Task: stale}, {Task: near}, {Task: wait1}, {Task: wait2}, queued, {Task: f1}, {Task: f2}, {Task: done}}
	s.addRoutines(now, true)

	monthly := 8.99
	s.approvals = []factoryd.ApprovalRequest{{ID: 67, Kind: factoryd.ApprovalResource, TaskID: stale.ID, Project: "sf/code", Status: "pending",
		MonthlyEUR: &monthly, CreatedAt: ago(38 * time.Minute),
		Reason: "load test (beyond the envelope: a lease of 300 minutes exceeds the envelope's 4 h)",
		Spec: json.RawMessage(`{"server_type":"cx33","purpose":"load test of the event stream","lease_minutes":300,"volume_gb":0,"location":"fsn1",` +
			`"reservation_eur":0.072,"hourly_eur":0.0144,"beyond_envelope":["a lease of 300 minutes exceeds the envelope's 4 h"]}`)}}
	s.events = []factoryd.Event{
		labStatus(619, "running", factoryd.GateSession, ago(41*time.Minute)),
		labStatus(620, "running", factoryd.GateSession, ago(14*time.Minute)),
		labEvent(620, factoryd.EventCardSteered, "sebastian", factoryd.ViaButton, ago(4*time.Minute), map[string]any{"text": "Push what you have."}),
		labStatus(615, "running", factoryd.GateProvider, ago(2*time.Minute)),
		labStatus(621, "running", factoryd.GateProvider, ago(time.Minute)),
		labEvent(610, factoryd.EventQuestionAsked, factoryd.FactoryDecider, factoryd.ViaFactoryd, ago(12*time.Minute), nil),
		labEvent(623, factoryd.EventDependencySet, "sebastian", factoryd.ViaButton, ago(20*time.Minute), nil),
		labStatus(604, "failed", "", ago(70*time.Minute)),
		labStatus(609, "failed", "", ago(35*time.Minute)),
	}
	s.alerts = []factoryd.Alert{
		{ID: 66, Kind: factoryd.AlertEscalation, Source: "cap", TaskID: capped.ID, RaisedAt: ago(12 * time.Minute)},
		{ID: 65, Kind: factoryd.AlertSupervisorTrip, Source: "same_failure", TaskID: f2.ID, RaisedAt: ago(50 * time.Minute),
			ResolvedAt: at(49 * time.Minute), Verdict: factoryd.VerdictReal, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "fail", "diagnosis": "the same assertion failed in three sessions"}},
		{ID: 64, Kind: factoryd.AlertMonthCap, Source: "80", RaisedAt: ago(5 * time.Hour), Fingerprint: now.UTC().Format("2006-01") + "/80"},
	}
	return s
}

func quietScene(now time.Time) labScene {
	s := labScene{drawer: labID(624), spend: 4.10, chatSpend: 0.40, objects: map[string]string{}}
	run := labCard(now, 624, "running", "ready", 16*time.Minute, "README.md: extend the intro",
		"Say in the first paragraph what the factory is for and who runs it.")
	run.Gate, run.GateSince, run.SpentEUR = factoryd.GateSession, &run.CreatedAt, 0.22
	s.objects["sessions/"+run.ID+"/timeline.jsonl"] = jsonl(map[string]any{"step": "session_start", "time": stamp(now.Add(-15 * time.Minute)),
		"session": "s-1", "model": "coding-cheap", "attempt": 1})
	s.objects["sessions/"+run.ID+"/s-1.jsonl"] = jsonl(map[string]any{"time": stamp(now.Add(-14 * time.Minute)), "kind": "model_call",
		"detail": map[string]any{"text": "Reading README.md and docs/overview.md first."}})
	s.tasks = []factoryd.BoardTask{{Task: run},
		{Task: finished(labCard(now, 622, "done", "ready", 4*time.Hour, "Chat form: center the Send button next to the two-row textarea",
			"The Send button sits at the textarea's top edge; center it on the two rows."), now, 3*time.Hour, 0.41)},
		{Task: finished(labCard(now, 621, "done", "ready", 6*time.Hour, "Costs: the day cap line under the table",
			"Show today's spend against the day cap under the costs table."), now, 5*time.Hour, 0.18)},
		{Task: finished(labCard(now, 620, "done", "ready", 9*time.Hour, "Console: show the configuration version",
			"Show the commit of the configuration in force in the console's footer."), now, 8*time.Hour, 0.33)},
	}
	s.events = []factoryd.Event{labStatus(624, "running", factoryd.GateSession, now.Add(-3*time.Minute))}
	return s
}
