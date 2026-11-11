package console

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sealed"
)

// chatSendWait bounds chat_send's wait for its turn's end.
var chatSendWait = 50 * time.Second // a var so tests shorten it

// tokenCall is the chat token a /mcp request came with, its project and
// the session its human's request runs as.
type tokenCall struct {
	row factoryd.ChatToken
	p   factoryd.Project
	s   *session
}

type tokenKey struct{}

func tokenFrom(ctx context.Context) (tokenCall, bool) {
	call, ok := ctx.Value(tokenKey{}).(tokenCall)
	return call, ok
}

// chatTokenRefused is the one answer to a missing, unknown, expired or
// revoked chat token.
const chatTokenRefused = "unknown, expired or revoked chat token"

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, msg, http.StatusUnauthorized)
}

// bearer admits a request on the chat token of its Authorization header
// alone, never on a cookie, while the token's human writes its project.
// The request then runs as that human: a session on the Forgejo token
// sealed behind the chat token.
func (c *Console) bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, now := r.Context(), time.Now()
		scheme, value, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		value = strings.TrimSpace(value)
		if !strings.EqualFold(scheme, "Bearer") || value == "" {
			unauthorized(w, chatTokenRefused)
			return
		}
		row, ok, err := c.cfg.ChatTokens.ChatTokenByHash(ctx, chatTokenHash(value))
		if err != nil {
			log.Printf("console: the chat token read: %v", err)
			http.Error(w, "cannot read the chat tokens", http.StatusServiceUnavailable)
			return
		}
		if !ok || !row.Revoked.IsZero() || !now.Before(row.Expires) {
			unauthorized(w, chatTokenRefused)
			return
		}
		p, found, err := c.projectAt(ctx, row.Repository)
		if err != nil {
			log.Printf("console: the project at %s: %v", row.Repository, err)
			http.Error(w, "cannot read the projects", http.StatusServiceUnavailable)
			return
		}
		if !found {
			unauthorized(w, "project "+row.Repository+" is gone; make a new chat token")
			return
		}
		writes, _, err := c.writer(ctx, row.Login)
		if err == nil && writes {
			writes, err = c.writesRepo(ctx, row.Login, row.Repository)
		}
		if err != nil {
			log.Printf("console: may %s write %s: %v", row.Login, row.Repository, err)
			http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
			return
		}
		if !writes {
			http.Error(w, row.Login+" no longer writes "+row.Repository, http.StatusForbidden)
			return
		}
		forgejo, err := sealed.Open(c.cfg.SessionKey, chatTokenKeyLabel, row.Sealed, row.Hash)
		if err != nil || forgejo == "" {
			// the row is not this key's, or holds no Forgejo token: the token is over
			log.Printf("console: chat token %d of %s opens no Forgejo token: %v", row.ID, row.Login, err)
			unauthorized(w, chatTokenRefused)
			return
		}
		if err := c.cfg.ChatTokens.TouchChatToken(ctx, row.ID, now); err != nil {
			log.Printf("console: the last use of chat token %d: %v", row.ID, err)
		}
		s := &session{user: row.Login, access: forgejo, chatToken: row.ID, tokenName: row.Name}
		ctx = context.WithValue(context.WithValue(ctx, ctxKey{}, s), tokenKey{}, tokenCall{row: row, p: p, s: s})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// mcpHandler serves the chat's MCP tools to a human's own agent, behind
// bearer. Every request stands alone (stateless).
func (c *Console) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(c.mcpServer, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the server's WriteTimeout would cut an answer after a minute
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(chatSendWait + 30*time.Second)); err != nil {
			log.Printf("console: /mcp keeps the server's write deadline: %v", err)
		}
		h.ServeHTTP(w, r)
	})
}

// mcpServer is the MCP server of one request that bearer admitted.
func (c *Console) mcpServer(r *http.Request) *mcp.Server {
	call, ok := tokenFrom(r.Context())
	if !ok {
		return nil
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "factory-chat", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_send", Description: "Send a message to the project agent of " + call.p.Name +
		" as yourself, as the console's chat does, and wait up to 50 seconds for the turn's end: its reply, the tools it called, " +
		"the proposals it raised, which run only once you confirm them, and what it applied. Without a conversation it writes in " +
		"this token's own conversation. The inbox refuses a message: answer it with its buttons in the console. A turn longer than 50 seconds answers ended: false and goes on; chat_read with after set to the last id gets the rest."},
		func(_ context.Context, _ *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, any, error) {
			// the request's own context: the SDK ends the handler's with it
			// on its newest protocol only, and a client that gives up ends it
			return c.chatSend(r.Context(), call, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_read", Description: "Read a conversation with the project agent of " + call.p.Name +
		": its last 40 messages, or with after at most 40 messages after that message id, oldest first, and at most about 60 KB of their text. " +
		"more says messages are left: call again with after set to the last id answered. Each message has its id, role, text, time, " +
		"source (console, or the name of the chat token that wrote it) and its proposals' states. Without a conversation it reads this " +
		"token's own. Reading marks nothing read."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
			return c.chatRead(ctx, call, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_confirm", Description: "Confirm one proposal of the project agent of " + call.p.Name +
		", as the console's Do it does, and wait up to 50 seconds for the turn's end. You may confirm every proposal you could press in the console, " +
		"a merge approval included, which Forgejo records as your own review. Each proposal runs once and expires. The answer is chat_send's: " +
		"what ran is in applied and a refused action in error; a proposal that cannot be pressed is refused as the tool's error. A turn longer than 50 seconds answers ended: false and goes on; chat_read with after set to the last id gets the rest."},
		func(_ context.Context, _ *mcp.CallToolRequest, in confirmInput) (*mcp.CallToolResult, any, error) {
			return c.chatConfirm(r.Context(), call, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_threads", Description: "List your conversations with the project agent of " + call.p.Name +
		": each one's number, title, time of its last message, whether it holds a reply you have not read, and whether it is your inbox."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return c.chatThreads(ctx, call)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_fund", Description: "Fund the chat of " + call.p.Name + " once its budget stopped it, " +
		"as the console's Fund and go on does: this month's judgment budget rises by eur whole euros, or without eur by the amount the stop suggests, " +
		"and every thread the budget stopped goes on by itself; chat_read reads it. Only an approver of the project may fund the chat."},
		func(_ context.Context, _ *mcp.CallToolRequest, in fundInput) (*mcp.CallToolResult, any, error) {
			return c.tokenFund(r.Context(), call, in)
		})
	return srv
}

// mcpThread is a conversation as chat_threads lists it.
type mcpThread struct {
	Number int64     `json:"number"`
	Title  string    `json:"title"`
	Last   time.Time `json:"last,omitzero"`
	Unread bool      `json:"unread"`
	Inbox  bool      `json:"inbox"`
}

func (c *Console) chatThreads(ctx context.Context, call tokenCall) (*mcp.CallToolResult, any, error) {
	convs, err := c.cfg.Chat.Conversations(ctx, call.row.Repository, call.row.Login)
	if err != nil {
		log.Printf("console: the conversations of %s on %s: %v", call.row.Login, call.row.Repository, err)
		return nil, nil, errors.New("cannot read the conversations")
	}
	out := make([]mcpThread, 0, len(convs))
	for _, cv := range convs {
		out = append(out, mcpThread{Number: cv.Number, Title: cv.Title, Last: cv.LastAt, Unread: cv.Unread > 0, Inbox: cv.Inbox})
	}
	return jsonAnswer(out)
}

// sendInput is chat_send's arguments.
type sendInput struct {
	Message      string `json:"message" jsonschema:"your message to the project agent, up to 8 KiB"`
	Conversation int64  `json:"conversation,omitempty" jsonschema:"a conversation's number from chat_threads; without one, this token's own"`
}

// readInput is chat_read's arguments.
type readInput struct {
	Conversation int64 `json:"conversation,omitempty" jsonschema:"a conversation's number from chat_threads; without one, this token's own"`
	After        int64 `json:"after,omitempty" jsonschema:"answer the messages after this message id, oldest first; while more is true, call again with the last id answered"`
}

// tokenThread is the thread a token's tool works in: n, which must be
// one of its human's threads in the project, or with n 0 the token's
// own, Number 0 before its first turn and once its human deleted it.
func (c *Console) tokenThread(ctx context.Context, call tokenCall, n int64) (factoryd.ChatConversation, error) {
	own := n == 0
	if own {
		if n = call.row.Conversation; n == 0 {
			return factoryd.ChatConversation{}, nil
		}
	}
	list, err := c.cfg.Chat.Conversations(ctx, call.row.Repository, call.row.Login)
	if err != nil {
		log.Printf("console: the conversations of %s on %s: %v", call.row.Login, call.row.Repository, err)
		return factoryd.ChatConversation{}, errors.New("cannot read the conversations")
	}
	if i := slices.IndexFunc(list, func(th factoryd.ChatConversation) bool { return th.Number == n }); i >= 0 {
		return list[i], nil
	}
	if own {
		return factoryd.ChatConversation{}, nil
	}
	return factoryd.ChatConversation{}, fmt.Errorf("conversation %d is not yours in %s", n, call.row.Repository)
}

// ownThread makes the token's own thread at its first turn, titled with
// its name and never its human's current one. Of two first turns at
// once the first claim wins, and the other's thread goes.
func (c *Console) ownThread(ctx context.Context, call tokenCall) (int64, error) {
	n, err := c.cfg.Chat.SideConversation(ctx, call.row.Repository, call.row.Login, call.row.Name)
	if err != nil {
		log.Printf("console: the thread of chat token %d: %v", call.row.ID, err)
		return 0, errors.New("cannot open the token's conversation")
	}
	got, claim := c.cfg.ChatTokens.ClaimChatTokenConversation(ctx, call.row.ID, n)
	if claim != nil || got != n {
		// a client that left mid-claim must not leave the thread behind
		if _, _, err := c.cfg.Chat.DeleteConversation(context.WithoutCancel(ctx), call.row.Repository, call.row.Login, n); err != nil {
			log.Printf("console: drop thread %d of chat token %d: %v", n, call.row.ID, err)
		}
	}
	if claim != nil {
		log.Printf("console: the thread of chat token %d: %v", call.row.ID, claim)
		return 0, errors.New("cannot open the token's conversation")
	}
	return got, nil
}

// sendAnswer is chat_send's answer.
type sendAnswer struct {
	Conversation int64         `json:"conversation"`
	Reply        string        `json:"reply"`
	Tools        []mcpTool     `json:"tools"`
	Proposals    []mcpProposal `json:"proposals"`
	Applied      []mcpApplied  `json:"applied"`
	Budget       string        `json:"budget"`
	Error        string        `json:"error"`
	Ended        bool          `json:"ended"`
}

type mcpTool struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

type mcpProposal struct {
	ID      string                `json:"id"`
	Tool    string                `json:"tool"`
	What    string                `json:"what"`
	Expires time.Time             `json:"expires"`
	Pool    *factoryd.PoolPreview `json:"pool,omitempty"`
}

type mcpApplied struct {
	Tool   string `json:"tool"`
	Result string `json:"result"`
}

// collector is a token's turn's event stream read into chat_send's
// answer. The turn sends from its own goroutine and from its tools';
// once the answer went out, every later event drops. joined: the message
// joined another turn, whose end is not this one's.
type collector struct {
	mu      sync.Mutex
	project string // the token's project, which a budget stop names
	a       sendAnswer
	gone    bool
	joined  bool
}

func (k *collector) send(event string, data any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.gone {
		return
	}
	switch event {
	case "text":
		m, _ := data.(map[string]string)
		k.a.Reply += m["text"]
	case "tool":
		m, _ := data.(map[string]any)
		name, _ := m["name"].(string)
		result, _ := m["result"].(string)
		k.a.Tools = append(k.a.Tools, mcpTool{Name: name, Result: result})
	case "proposal":
		m, _ := data.(map[string]any)
		id, _ := m["id"].(string)
		tool, _ := m["tool"].(string)
		what, _ := m["description"].(string)
		expires, _ := m["expires"].(time.Time)
		pool, _ := m["pool"].(*factoryd.PoolPreview)
		k.a.Proposals = append(k.a.Proposals, mcpProposal{ID: id, Tool: tool, What: what, Expires: expires, Pool: pool})
	case "applied":
		m, _ := data.(map[string]string)
		k.a.Applied = append(k.a.Applied, mcpApplied{Tool: m["tool"], Result: m["result"]})
	case "budget":
		if b, ok := data.(budgetEvent); ok {
			k.a.Budget = b.Message.Content
			if f := b.Budget; f != nil {
				k.a.Budget += "\n" + fundAction(k.project, f)
			}
		}
	case "error":
		m, _ := data.(map[string]string)
		k.a.Error = m["message"]
	case "stopped":
		if s, ok := data.(stopEvent); ok {
			k.a.Error = s.Message.Content
		}
	case "joined":
		k.joined = true
	}
}

// fundAction is the action the chat's own budget stop names to a token:
// chat_fund at the stop's suggestion for an approver, who funds it for a
// writer. A fund resumes the stopped thread (doChatFund).
func fundAction(project string, f *chatBudget) string {
	const then = "this thread then goes on by itself, and chat_read reads it."
	if f.CanFund {
		return fmt.Sprintf("Fund the chat with chat_fund, €%.0f suggested: %s", f.SuggestEUR, then)
	}
	return "An approver of " + project + " funds the chat: " + then
}

// wait runs turn, which returns at the turn's end, and answers what the
// stream said by then, by chatSendWait or by ctx's end, whichever comes
// first; the turn goes on detached past the wait. A refusal before the
// stream opened is the error.
func (k *collector) wait(ctx context.Context, turn func() error) (sendAnswer, error) {
	ended := make(chan error, 1)
	go func() { ended <- turn() }()
	timer := time.NewTimer(chatSendWait)
	defer timer.Stop()
	var err error
	done := false
	select {
	case err = <-ended:
		done = true
	case <-timer.C:
	case <-ctx.Done():
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gone, k.a.Ended = true, done && !k.joined
	return k.a, err
}

// chatSend runs a turn of the token's human, as the console's chat POST
// does, in the given thread or the token's own.
func (c *Console) chatSend(ctx context.Context, call tokenCall, in sendInput) (*mcp.CallToolResult, any, error) {
	if c.cfg.Operator == nil {
		return nil, nil, errors.New("the console has no chat")
	}
	th, err := c.tokenThread(ctx, call, in.Conversation)
	if err != nil {
		return nil, nil, err
	}
	if th.State == factoryd.ThreadArchived {
		return nil, nil, refuse(http.StatusConflict, fmt.Sprintf("Conversation %d is archived: restore it to write in it.", th.Number))
	}
	t := c.sessionTurn(ctx, call.p, call.s, th.Number)
	msg, err := t.input(in.Message)
	if err != nil {
		return nil, nil, err
	}
	if th.Number == 0 {
		if th.Number, err = c.ownThread(ctx, call); err != nil {
			return nil, nil, err
		}
		th.Title = call.row.Name
		t.conv, t.a.thread = th.Number, th.Number
	}
	// a titled thread keeps its title: the token's own, or one its human named
	msg.Names = names(th) && th.Title == ""
	k := &collector{project: call.p.Name, a: sendAnswer{Conversation: th.Number, Tools: []mcpTool{}, Proposals: []mcpProposal{}, Applied: []mcpApplied{}}}
	t.send, t.unread = k.send, true
	a, err := k.wait(ctx, func() error { return t.say(th, msg, func() {}) })
	if err != nil {
		return nil, nil, errors.New(t.sentence(err))
	}
	return jsonAnswer(a)
}

// confirmInput is chat_confirm's arguments.
type confirmInput struct {
	Proposal string `json:"proposal" jsonschema:"a proposal's id from chat_send or chat_read"`
}

// chatConfirm presses the token's human's proposal through the button's
// own path, in the thread that raised it. A tool the press ran and the
// policy or Forgejo refused is the answer's error as well as its tool
// block.
func (c *Console) chatConfirm(ctx context.Context, call tokenCall, in confirmInput) (*mcp.CallToolResult, any, error) {
	if c.cfg.Operator == nil {
		return nil, nil, errors.New("the console has no chat")
	}
	t := c.sessionTurn(ctx, call.p, call.s, 0)
	p, err := t.peek("proposal", in.Proposal)
	if err != nil {
		return nil, nil, errors.New(t.sentence(err))
	}
	k := &collector{project: call.p.Name, a: sendAnswer{Conversation: p.Conversation, Tools: []mcpTool{}, Proposals: []mcpProposal{}, Applied: []mcpApplied{}}}
	t.send, t.unread = k.send, true
	a, err := k.wait(ctx, func() error { return t.press(in.Proposal, nil, func() {}) })
	if err != nil {
		return nil, nil, errors.New(t.sentence(err))
	}
	for _, tl := range a.Tools {
		if why, refused := strings.CutPrefix(tl.Result, "Refused: "); refused && tl.Name == p.Tool {
			a.Error = why
			break
		}
	}
	return jsonAnswer(a)
}

// fundInput is chat_fund's arguments.
type fundInput struct {
	EUR *int64 `json:"eur,omitempty" jsonschema:"whole euros to add to this month's judgment budget; without it, the amount the budget stop suggests"`
}

// fundAnswer is chat_fund's answer.
type fundAnswer struct {
	FromEUR float64 `json:"from_eur"`
	ToEUR   float64 `json:"to_eur"`
	Result  string  `json:"result"`
}

// tokenFund presses Fund and go on for the token's human, past that
// control's approver rule, through doChatFund, which resumes every thread
// the chat's budget stopped. Without eur it funds what the stop suggests;
// a chat not stopped suggests nothing, and eur raises its budget anyway.
func (c *Console) tokenFund(ctx context.Context, call tokenCall, in fundInput) (*mcp.CallToolResult, any, error) {
	if c.cfg.Operator == nil {
		return nil, nil, errors.New("the console has no chat")
	}
	t := c.sessionTurn(ctx, call.p, call.s, 0)
	t.a.decider = "member"
	if err := c.admit(ctx, accessTable["POST /projects/{name}/chat/budget"], t.a, call.p.Repository, ""); err != nil {
		return nil, nil, errors.New(t.sentence(err))
	}
	stop, err := c.chatStopNow(ctx, call.p)
	if err != nil {
		log.Printf("console: the chat stop of %s: %v", call.p.Repository, err)
		return nil, nil, errors.New("cannot read the judgment budget")
	}
	var eur float64
	switch {
	case in.EUR != nil:
		eur = float64(*in.EUR)
	case stop.stopped:
		eur, _ = stop.event.Detail["suggest_eur"].(float64)
	default:
		return nil, nil, errors.New("The chat is not stopped, so no fund is suggested: give eur to raise its budget anyway.")
	}
	from, to, err := c.doChatFund(ctx, t.a, call.p.Name, eur, chatFundNote, "")
	if err != nil {
		return nil, nil, errors.New(t.sentence(err))
	}
	result := fmt.Sprintf("Raised the judgment budget from €%.2f to €%.2f.", from, to)
	if stop.stopped {
		result += " Every thread the budget stopped goes on by itself; chat_read reads it."
	} else {
		result += " The chat was not stopped."
	}
	return jsonAnswer(fundAnswer{FromEUR: from, ToEUR: to, Result: result})
}

// mcpMessage is a thread's row as chat_read answers it.
type mcpMessage struct {
	ID        int64              `json:"id"`
	Role      string             `json:"role"`
	Text      string             `json:"text"`
	At        time.Time          `json:"at"`
	Source    string             `json:"source"`
	Proposals []mcpProposalState `json:"proposals,omitempty"`
}

type mcpProposalState struct {
	ID    string                `json:"id"`
	State string                `json:"state"`
	Pool  *factoryd.PoolPreview `json:"pool,omitempty"`
}

// chatReadMax bounds chat_read's rows, chatReadBytes their text as the
// answer's JSON holds it: Claude Code cuts a tool's answer at 25,000
// tokens by default.
const (
	chatReadMax   = 40
	chatReadBytes = 60 << 10
)

// readAnswer is chat_read's answer: the messages, oldest first, and more
// while messages after the last one are left.
type readAnswer struct {
	Messages []mcpMessage `json:"messages"`
	More     bool         `json:"more"`
}

// chatRead answers a thread's last chatReadMax rows, or as many after
// in.After, from the store: it never marks the thread seen nor opens it.
// It stops at a whole row before the text, as JSON, passes chatReadBytes,
// the first row always answered, so a read after the last one always
// moves on.
func (c *Console) chatRead(ctx context.Context, call tokenCall, in readInput) (*mcp.CallToolResult, any, error) {
	th, err := c.tokenThread(ctx, call, in.Conversation)
	if err != nil {
		return nil, nil, err
	}
	out := readAnswer{Messages: []mcpMessage{}}
	if th.Number == 0 {
		return jsonAnswer(out)
	}
	var rows []factoryd.ChatMessage
	if in.After > 0 {
		rows, err = c.cfg.Chat.ChatMessagesAfter(ctx, call.row.Repository, call.row.Login, th.Number, in.After, chatReadMax+1)
	} else {
		rows, err = c.cfg.Chat.ChatMessages(ctx, call.row.Repository, call.row.Login, th.Number, chatReadMax)
	}
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", call.row.Login, call.row.Repository, err)
		return nil, nil, errors.New("cannot read the conversation")
	}
	if len(rows) > chatReadMax {
		rows, out.More = rows[:chatReadMax], true
	}
	size := 0
	for i, m := range rows {
		text, err := encoded(m.Content)
		if err != nil {
			return nil, nil, err
		}
		if size += len(text); i > 0 && size > chatReadBytes {
			rows, out.More = rows[:i], true
			break
		}
	}
	var tokens []string
	for _, m := range rows {
		row := mcpMessage{ID: m.ID, Role: m.Role, Text: m.Content, At: m.At, Source: cmp.Or(m.Token, "console")}
		for _, tc := range m.ToolCalls {
			if tc.Proposal != "" {
				row.Proposals = append(row.Proposals, mcpProposalState{ID: tc.Proposal})
				tokens = append(tokens, tc.Proposal)
			}
		}
		out.Messages = append(out.Messages, row)
	}
	if len(tokens) == 0 {
		return jsonAnswer(out)
	}
	states, err := c.cfg.Chat.ProposalStates(ctx, call.row.Repository, call.row.Login, tokens, time.Now())
	if err != nil {
		log.Printf("console: the chat proposals of %s on %s: %v", call.row.Login, call.row.Repository, err)
	}
	state := make(map[string]factoryd.ProposalState, len(states))
	for _, st := range states {
		state[st.Token] = st
	}
	for i := range out.Messages {
		ps := out.Messages[i].Proposals
		for j := range ps {
			ps[j].State, ps[j].Pool = state[ps[j].ID].State, state[ps[j].ID].Pool
		}
	}
	return jsonAnswer(out)
}

// jsonAnswer is v as a tool's answer: one text block of JSON.
func jsonAnswer(v any) (*mcp.CallToolResult, any, error) {
	b, err := encoded(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

// encoded is v as one line of JSON that keeps <, > and & as they are: the
// \u003c form would make a page of markup six times its size.
func encoded(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
