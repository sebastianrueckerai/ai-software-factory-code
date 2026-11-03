package console

import (
	"context"
	"maps"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tokenOf is the chat token the context's session came in by, nil for a
// context with no session, a cookie or a wake.
func tokenOf(ctx context.Context) *session {
	if s, ok := ctx.Value(ctxKey{}).(*session); ok && s.chatToken != 0 {
		return s
	}
	return nil
}

// stamped is ev with the chat token behind ctx's session named in its
// detail; the event's actor stays the human. A token already in the
// detail stays.
func stamped(ctx context.Context, ev factoryd.Event) factoryd.Event {
	s := tokenOf(ctx)
	if s == nil {
		return ev
	}
	detail := maps.Clone(ev.Detail)
	if detail == nil {
		detail = map[string]any{}
	}
	if _, set := detail["token"]; !set {
		detail["token"] = map[string]any{"id": s.chatToken, "name": s.tokenName}
	}
	ev.Detail = detail
	return ev
}

// stampedEvents names the chat token behind every event a token's session
// records.
type stampedEvents struct{ Events }

func (e stampedEvents) Record(ctx context.Context, ev factoryd.Event) (int64, error) {
	return e.Events.Record(ctx, stamped(ctx, ev))
}

// stampedTasks stamps the two events the task store writes in its own
// transaction.
type stampedTasks struct{ Tasks }

func (t stampedTasks) FundCard(ctx context.Context, id string, eur float64, e factoryd.Event) (float64, float64, error) {
	return t.Tasks.FundCard(ctx, id, eur, stamped(ctx, e))
}

func (t stampedTasks) ExtendCard(ctx context.Context, id string, d policy.Draw, lines int, e factoryd.Event) (float64, float64, error) {
	return t.Tasks.ExtendCard(ctx, id, d, lines, stamped(ctx, e))
}

// stampedChat names the chat token behind every row a token's turn writes.
type stampedChat struct{ Chat }

func (c stampedChat) AddChatMessage(ctx context.Context, repository, login string, m factoryd.ChatMessage) (int64, error) {
	if s := tokenOf(ctx); s != nil && m.Token == "" {
		m.Token = s.tokenName
	}
	return c.Chat.AddChatMessage(ctx, repository, login, m)
}

// stampedAttach stamps the analysis_attached row the store writes in its
// own transaction.
func stampedAttach(attach func(ctx context.Context, taskID, found, recommendation string, e factoryd.Event) error) func(ctx context.Context, taskID, found, recommendation string, e factoryd.Event) error {
	return func(ctx context.Context, taskID, found, recommendation string, e factoryd.Event) error {
		return attach(ctx, taskID, found, recommendation, stamped(ctx, e))
	}
}
