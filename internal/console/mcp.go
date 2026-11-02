package console

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sealed"
)

// chatSendWait bounds chat_send's wait for its turn's end.
var chatSendWait = 4 * time.Minute // a var so tests shorten it

// tokenCall is the chat token a /mcp request came with and its project.
type tokenCall struct {
	row factoryd.ChatToken
	p   factoryd.Project
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
		live := ok && row.Revoked.IsZero() && now.Before(row.Expires)
		var forgejo string
		if live {
			if forgejo, err = sealed.Open(c.cfg.SessionKey, chatTokenKeyLabel, row.Sealed, row.Hash); err != nil {
				// the row is not this key's: the token is over
				log.Printf("console: chat token %d of %s: %v", row.ID, row.Login, err)
				live = false
			}
		}
		if !live {
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
		if err := c.cfg.ChatTokens.TouchChatToken(ctx, row.ID, now); err != nil {
			log.Printf("console: the last use of chat token %d: %v", row.ID, err)
		}
		s := &session{user: row.Login, access: forgejo, chatToken: row.ID, tokenName: row.Name}
		ctx = context.WithValue(context.WithValue(ctx, ctxKey{}, s), tokenKey{}, tokenCall{row: row, p: p})
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
	call, _ := tokenFrom(r.Context())
	srv := mcp.NewServer(&mcp.Implementation{Name: "factory-chat", Version: "1"}, nil)
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

// jsonAnswer is v as a tool's answer: one text block of JSON.
func jsonAnswer(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}
