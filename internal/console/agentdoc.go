package console

// The console's agents for the context map: the project agent, driven
// through the real chat POST handler with its context, status and
// offered tools assembled by the real code, and the operator model's
// compaction and vision calls. Every store the drive reaches runs on an
// in-memory fixture; a method the drive reaches that no fixture covers
// panics on the nil embedded interface, so a capture that stopped
// exercising the real path fails loudly instead of shipping a different
// context silently.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	agentDocName = "fixture"
	agentDocRepo = "owner/name"
	agentDocUser = "fixture-user"
	agentDocTask = "t-chat-fixture-2026-11"
	agentDocAsk  = "What blocks the staging deploy?"
)

// AgentDocConsoleIDs are the console agents the map captures.
func AgentDocConsoleIDs() []string {
	return []string{"project-agent", "compact", "vision"}
}

// AgentDocConsole captures agent id's first model request through the
// real console code against the fake gateway at gwURL, under policy
// version v, and answers the spans the request is made of.
func AgentDocConsole(ctx context.Context, id, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error) {
	op := &factoryd.OperatorModel{GatewayURL: gwURL, Token: "agentdoc", HTTP: hc}
	switch id {
	case "project-agent":
		return agentDocChatAgent(ctx, op, v)
	case "compact":
		return agentDocCompact(ctx, op, v)
	case "vision":
		return agentDocVision(ctx, op)
	}
	return nil, fmt.Errorf("no console agent %s", id)
}

// agentDocChatAgent drives POST /projects/fixture/chat: the real handler
// stores the human's message, assembles the turn's context and offered
// tools, and streams the project agent's first model call. The spans
// come from the same builders reading the same fixture state before the
// drive; the thread is a card's, so no naming call races the capture.
func agentDocChatAgent(ctx context.Context, op *factoryd.OperatorModel, v policy.Version) ([]agentdoc.Span, error) {
	p := agentDocProject()
	chat := &agentDocChat{thread: factoryd.ChatConversation{Number: 1, Login: agentDocUser,
		State: factoryd.ThreadActive, TaskID: "t-fixture-card"}}
	c := agentDocConsole(op, chat, v)

	pr := agentDocRequest(ctx, "GET", "/projects/"+agentDocName+"/chat", nil, "")
	pt, ok := c.chatTurn(httptest.NewRecorder(), pr)
	if !ok {
		return nil, fmt.Errorf("the fixture project's turn did not open")
	}
	if _, err := pt.thread(pr, true); err != nil {
		return nil, err
	}
	status, err := pt.snapshot()
	if err != nil {
		return nil, err
	}
	now := status
	images := imagesFact(p)
	if images != "" {
		now += "\n" + images
	}
	// the same day the drive stamps into the prompt; a capture that
	// crosses midnight fails its own location and is simply run again
	date := time.Now().UTC().Format("2006-01-02")
	defs := pt.offered()

	r, err := agentDocPost(ctx)
	if err != nil {
		return nil, err
	}
	c.chatPost(httptest.NewRecorder(), r)

	spans := []agentdoc.Span{
		{Name: "persona", Symbol: "(*turn).prompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: pt.prompt()},
		{Name: "the project now", Symbol: "(*turn).context", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem,
			Text: "\n\nThe project now:\nToday is " + date + " UTC.\n" + now},
		{Name: "board status", Symbol: "(*turn).snapshot", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: status},
		{Name: "sandbox images", Symbol: "imagesFact", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: images},
		{Name: "the human's message", Symbol: "(*Console).chatPost", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser, Text: agentDocAsk},
	}
	if images == "" {
		spans = append(spans[:3], spans[4:]...)
	}
	for _, d := range defs {
		spans = append(spans, agentdoc.Span{Name: "tool " + d.Name, Symbol: "(*turn).offered",
			Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceTools, Text: agentDocJSON(d)})
	}
	return spans, nil
}

// agentDocCompact drives the real compaction on a fixture thread: five
// rows, whose first two turns fold into one summary call.
func agentDocCompact(ctx context.Context, op *factoryd.OperatorModel, v policy.Version) ([]agentdoc.Span, error) {
	chat := &agentDocChat{thread: factoryd.ChatConversation{Number: 1, Login: agentDocUser, State: factoryd.ThreadActive}}
	for _, m := range []factoryd.ChatMessage{
		{Role: "user", Content: "Set the staging budget to 40 euros."},
		{Role: "assistant", Content: "I set the staging budget to 40 euros; the card waits for your OK."},
		{Role: "user", Content: "The OK is given. File the release note next."},
		{Role: "assistant", Content: "The release note is filed as card #14."},
		{Role: "user", Content: "Good. Keep an eye on the chain."},
	} {
		chat.next++
		m.ID, m.At = chat.next, time.Now()
		chat.rows = append(chat.rows, m)
		chat.thread.Messages++
	}
	c := agentDocConsole(op, chat, v)
	r := httptest.NewRequest("POST", "/", nil)
	t := &turn{c: c, r: r.WithContext(ctx), p: agentDocProject(), owner: agentDocUser, conv: 1}
	t.stream(func(string) error { return nil }, nil)
	if _, err := t.compact(agentDocTask, keepTurns); err != nil {
		return nil, err
	}
	rows := liveRows(chat.thread, chat.rows)
	start := tailStart(rows, keepTurns)
	if start == 0 {
		return nil, fmt.Errorf("the fixture thread holds nothing before its tail")
	}
	chunk := chunks(transcript(rows[:start]), compactChunk)
	if len(chunk) != 1 {
		return nil, fmt.Errorf("the fixture thread folds into %d calls, want one", len(chunk))
	}
	return []agentdoc.Span{
		{Name: "the summariser's rules", Symbol: "compactPrompt", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: compactPrompt},
		{Name: "the compaction frame", Symbol: "(*turn).compact", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser,
			Text: "The summary so far:\n(none yet)\n\nThe turns after it:\n"},
		{Name: "the folded turns", Symbol: "transcript", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser, Text: chunk[0]},
	}, nil
}

// agentDocVision drives the operator model's look at the fixture image.
func agentDocVision(ctx context.Context, op *factoryd.OperatorModel) ([]agentdoc.Span, error) {
	img, err := agentDocImage()
	if err != nil {
		return nil, err
	}
	thread := agentDocRepo + ":" + agentDocUser + ":1"
	if _, err := op.Vision(ctx, agentDocTask, policy.DefaultChat.VisionModel, thread, agentDocAsk, img, "image/png"); err != nil {
		return nil, err
	}
	return []agentdoc.Span{
		{Name: "the ask", Symbol: "(*OperatorModel).Vision", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser, Text: agentDocAsk},
		{Name: "the image", Symbol: "(*OperatorModel).Vision", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser,
			Text: "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)},
	}, nil
}

// agentDocProject is the fixture project: a coder project the human
// writes, with a namespace its tools may read.
func agentDocProject() factoryd.Project {
	return factoryd.Project{Name: agentDocName, Repository: agentDocRepo, Agents: policy.AgentsCoder,
		Objects: policy.ObjectsSeed, File: true, Namespaces: []string{"fixture-ns"}}
}

// agentDocConsole wires the fixture Console: every store the drive
// reaches is an in-memory fixture over an empty board.
func agentDocConsole(op *factoryd.OperatorModel, chat Chat, v policy.Version) *Console {
	return &Console{turns: newTurnGate(), stops: newStopGate(), ended: make(chan struct{}), cfg: Config{
		Chat: chat, Operator: op, Policies: agentDocPolicies{v}, Projects: agentDocProjects{},
		Writers: func(context.Context) (string, map[string][]string, error) {
			return agentDocRepo, map[string][]string{agentDocRepo: {agentDocUser}}, nil
		},
		Writes:    func(context.Context, string, string) (bool, error) { return true, nil },
		Tasks:     agentDocTasks{},
		Events:    agentDocEvents{},
		Costs:     agentDocCosts{},
		Approvals: agentDocApprovals{},
	}}
}

// agentDocRequest is a request of the fixture human on the fixture
// project: the session rides the context, no cookie is forged.
func agentDocRequest(ctx context.Context, method, target string, body *bytes.Buffer, contentType string) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, body)
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.SetPathValue("name", agentDocName)
	return r.WithContext(context.WithValue(ctx, ctxKey{}, &session{user: agentDocUser}))
}

// agentDocPost is the human's chat message as the console's client
// sends it: one multipart form.
func agentDocPost(ctx context.Context) (*http.Request, error) {
	var b bytes.Buffer
	f := multipart.NewWriter(&b)
	if err := f.WriteField("message", agentDocAsk); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return agentDocRequest(ctx, "POST", "/projects/"+agentDocName+"/chat", &b, f.FormDataContentType()), nil
}

// agentDocImage is a one-pixel PNG, the vision call's fixture.
func agentDocImage() ([]byte, error) {
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 210, G: 120, B: 30, A: 255})
	if err := png.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// agentDocJSON is v as it sits inside a marshalled request body; a value
// that will not marshal matches nothing and fails the capture.
func agentDocJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// agentDocChat is the chat store the drives run on: one thread and its
// rows in memory, ChatMessages answering the newest limit oldest first
// as the store does.
type agentDocChat struct {
	Chat
	thread factoryd.ChatConversation
	rows   []factoryd.ChatMessage
	next   int64
}

func (s *agentDocChat) EnsureChatTask(context.Context, string, string, float64, time.Time) (factoryd.Task, error) {
	return factoryd.Task{ID: agentDocTask, Kind: factoryd.KindChat, Status: "running"}, nil
}

func (s *agentDocChat) AddChatMessage(_ context.Context, _ string, _ string, m factoryd.ChatMessage) (int64, error) {
	s.next++
	m.ID, m.At = s.next, time.Now()
	s.rows = append(s.rows, m)
	s.thread.Messages++
	return m.ID, nil
}

func (s *agentDocChat) ChatMessages(context.Context, string, string, int64, int) ([]factoryd.ChatMessage, error) {
	rows := s.rows
	if len(rows) > chatContextMax {
		rows = rows[len(rows)-chatContextMax:]
	}
	return rows, nil
}

func (s *agentDocChat) ChatOpenQuestion(context.Context, string, string, int64) (string, bool, error) {
	return "", false, nil
}

func (s *agentDocChat) LiveAskBinds(context.Context, string, string, int64, time.Time) ([]factoryd.ChatProposal, error) {
	return nil, nil
}

func (s *agentDocChat) ChatAttachments(context.Context, string, string, int64) ([]factoryd.ChatAttachment, error) {
	return nil, nil
}

func (s *agentDocChat) CurrentConversation(context.Context, string, string) (int64, error) {
	return s.thread.Number, nil
}

func (s *agentDocChat) Conversation(context.Context, string, string, int64) (factoryd.ChatConversation, bool, error) {
	return s.thread, true, nil
}

func (s *agentDocChat) ProjectMonthSpend(context.Context, string, time.Time) (factoryd.ProjectSpend, error) {
	return factoryd.ProjectSpend{}, nil
}

func (s *agentDocChat) ThreadTokens(context.Context, string, time.Time) (int, error) {
	return 0, nil
}

func (s *agentDocChat) SaveSummary(context.Context, string, string, int64, factoryd.ThreadSummary) error {
	return nil
}

type agentDocPolicies struct{ v policy.Version }

func (p agentDocPolicies) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return p.v, true, nil
}

type agentDocProjects struct{}

func (agentDocProjects) Projects(context.Context) ([]factoryd.Project, error) {
	return []factoryd.Project{agentDocProject()}, nil
}

type agentDocTasks struct{ Tasks }

func (agentDocTasks) Task(context.Context, string) (factoryd.Task, bool, error) {
	return factoryd.Task{}, false, nil
}

func (agentDocTasks) BoardTasks(context.Context, string) ([]factoryd.BoardTask, error) {
	return nil, nil
}

func (agentDocTasks) FinishedTasks(context.Context, factoryd.FinishedQuery) ([]factoryd.BoardTask, int, error) {
	return nil, 0, nil
}

type agentDocEvents struct{ Events }

func (agentDocEvents) NewestTaskEvents(context.Context, []string) (map[string]factoryd.Event, error) {
	return nil, nil
}

// agentDocCosts seats the cost stores on zeros; the Costs interface
// carries a method of its own name, so it cannot be embedded.
type agentDocCosts struct{}

func (agentDocCosts) Costs(context.Context, time.Time, time.Time) (policy.Costs, error) {
	return policy.Costs{}, nil
}

func (agentDocCosts) Enforced(context.Context, time.Time) (policy.Enforced, error) {
	return policy.Enforced{}, nil
}

func (agentDocCosts) DaySpend(context.Context, time.Time) (float64, error) { return 0, nil }

func (agentDocCosts) PremiumMonthSpend(context.Context, time.Time) (float64, error) {
	return 0, nil
}

func (agentDocCosts) ModelCalls(context.Context, string) ([]policy.ModelCall, error) {
	return nil, nil
}

func (agentDocCosts) CallTotals(context.Context, []string) (map[string]policy.CallTotals, error) {
	return nil, nil
}

type agentDocApprovals struct{ Approvals }

func (agentDocApprovals) ApprovalRequests(context.Context, string) ([]factoryd.ApprovalRequest, error) {
	return nil, nil
}
