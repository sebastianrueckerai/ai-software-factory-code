package console

import (
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
var chatSendWait = 4 * time.Minute // a var so tests shorten it

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
		" as yourself, as the console's chat does, and wait up to 4 minutes for the turn's end: its reply, the tools it called, " +
		"the proposals it raised, which run only once you confirm them, and what it applied. Without a conversation it writes in " +
		"this token's own conversation. A turn that outlasts the wait goes on; chat_read reads its end."},
		func(_ context.Context, _ *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, any, error) {
			// the request's own context: the SDK ends the handler's with it
			// on its newest protocol only, and a client that gives up ends it
			return c.chatSend(r.Context(), call, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_read", Description: "Read a conversation with the project agent of " + call.p.Name +
		": its last 40 messages, or with after at most 40 messages after that message id, oldest first; for more, call again with the " +
		"last id it answered. Each message has its id, role, text, time, source (console, or the name of the chat token that wrote it) " +
		"and its proposals' states. Without a conversation it reads this token's own. Reading marks nothing read."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
			return c.chatRead(ctx, call, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_confirm", Description: "Confirm one proposal of the project agent of " + call.p.Name +
		", as the console's Do it does, and wait up to 4 minutes for the turn's end. You may confirm every proposal you could press in the console, " +
		"a merge approval included, which Forgejo records as your own review. Each proposal runs once and expires. The answer is chat_send's: " +
		"what ran is in applied, a refusal in error."},
		func(_ context.Context, _ *mcp.CallToolRequest, in confirmInput) (*mcp.CallToolResult, any, error) {
			return c.chatConfirm(r.Context(), call, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "chat_threads", Description: "List your conversations with the project agent of " + call.p.Name +
		": each one's number, title, time of its last message, and whether it holds a reply you have not read."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return c.chatThreads(ctx, call)
		})
	return srv
}

// mcpThread is a conversation as chat_threads lists it.
type mcpThread struct {
	Number int64     `json:"number"`
	Title  string    `json:"title"`
	Last   time.Time `json:"last,omitzero"`
	Unread bool      `json:"unread"`
}

func (c *Console) chatThreads(ctx context.Context, call tokenCall) (*mcp.CallToolResult, any, error) {
	convs, err := c.cfg.Chat.Conversations(ctx, call.row.Repository, call.row.Login)
	if err != nil {
		log.Printf("console: the conversations of %s on %s: %v", call.row.Login, call.row.Repository, err)
		return nil, nil, errors.New("cannot read the conversations")
	}
	out := make([]mcpThread, 0, len(convs))
	for _, cv := range convs {
		out = append(out, mcpThread{Number: cv.Number, Title: cv.Title, Last: cv.LastAt, Unread: cv.Unread > 0})
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
	After        int64 `json:"after,omitempty" jsonschema:"answer at most 40 messages after this message id, oldest first; for more, call again with the last id answered"`
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
	ID      string    `json:"id"`
	Tool    string    `json:"tool"`
	What    string    `json:"what"`
	Expires time.Time `json:"expires"`
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
	mu     sync.Mutex
	a      sendAnswer
	gone   bool
	joined bool
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
		k.a.Proposals = append(k.a.Proposals, mcpProposal{ID: id, Tool: tool, What: what, Expires: expires})
	case "applied":
		m, _ := data.(map[string]string)
		k.a.Applied = append(k.a.Applied, mcpApplied{Tool: m["tool"], Result: m["result"]})
	case "budget":
		if b, ok := data.(budgetEvent); ok {
			k.a.Budget = b.Message.Content
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
	k := &collector{a: sendAnswer{Conversation: th.Number, Tools: []mcpTool{}, Proposals: []mcpProposal{}, Applied: []mcpApplied{}}}
	t.send, t.unread = k.send, true
	a, err := k.wait(ctx, func() error { return t.say(msg, func() {}) })
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	k := &collector{a: sendAnswer{Conversation: p.Conversation, Tools: []mcpTool{}, Proposals: []mcpProposal{}, Applied: []mcpApplied{}}}
	t.send, t.unread = k.send, true
	a, err := k.wait(ctx, func() error { return t.press(in.Proposal, nil, func() {}) })
	if err != nil {
		return nil, nil, err
	}
	for _, tl := range a.Tools {
		if why, refused := strings.CutPrefix(tl.Result, "Refused: "); refused && tl.Name == p.Tool && a.Error == "" {
			a.Error = why
		}
	}
	return jsonAnswer(a)
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
	ID    string `json:"id"`
	State string `json:"state"`
}

// chatReadMax bounds chat_read's rows.
const chatReadMax = 40

// chatRead answers a thread's last chatReadMax rows, or as many after
// in.After, from the store: it never marks the thread seen nor opens it.
func (c *Console) chatRead(ctx context.Context, call tokenCall, in readInput) (*mcp.CallToolResult, any, error) {
	th, err := c.tokenThread(ctx, call, in.Conversation)
	if err != nil {
		return nil, nil, err
	}
	out := []mcpMessage{}
	if th.Number == 0 {
		return jsonAnswer(out)
	}
	var rows []factoryd.ChatMessage
	if in.After > 0 {
		rows, err = c.cfg.Chat.ChatMessagesAfter(ctx, call.row.Repository, call.row.Login, th.Number, in.After, chatReadMax)
	} else {
		rows, err = c.cfg.Chat.ChatMessages(ctx, call.row.Repository, call.row.Login, th.Number, chatReadMax)
	}
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", call.row.Login, call.row.Repository, err)
		return nil, nil, errors.New("cannot read the conversation")
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
		out = append(out, row)
	}
	if len(tokens) == 0 {
		return jsonAnswer(out)
	}
	states, err := c.cfg.Chat.ProposalStates(ctx, call.row.Repository, call.row.Login, tokens, time.Now())
	if err != nil {
		log.Printf("console: the chat proposals of %s on %s: %v", call.row.Login, call.row.Repository, err)
	}
	state := make(map[string]string, len(states))
	for _, st := range states {
		state[st.Token] = st.State
	}
	for i := range out {
		for j := range out[i].Proposals {
			out[i].Proposals[j].State = state[out[i].Proposals[j].ID]
		}
	}
	return jsonAnswer(out)
}

// jsonAnswer is v as a tool's answer: one text block of JSON.
func jsonAnswer(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}
