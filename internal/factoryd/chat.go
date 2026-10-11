package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// KindChat is the kind of a project's chat task: the standing task the
// project agent's model calls book under, its one capability
// policy.ClassChat. It runs no workflow and is no card: the board, the
// heal pass and the card actions leave it out.
const KindChat = "chat"

// ChatTaskID is project name's chat task in the UTC month of at: a new
// task each month, so its budget is the month's.
func ChatTaskID(name string, at time.Time) string {
	return "t-chat-" + name + "-" + at.UTC().Format("2006-01")
}

// ChatMessage is one row of a member's conversation with the project
// agent: the member's text (role user), or the agent's text and the tool
// calls it made with their results (role assistant).
type ChatMessage struct {
	ID        int64          `json:"id"`
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []ChatToolCall `json:"tool_calls,omitempty"`
	At        time.Time      `json:"at"`
}

// ChatToolCall is one tool call of the agent and what it answered.
type ChatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
}

// EnsureChatTask is project name's chat task for the month of at, filed
// running with budget the first time: a later call that month changes
// nothing. An earlier month's task still running closes (finished now),
// so the settlement pass settles it and the gateway takes no more of its
// calls.
func (s *PGStore) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (Task, error) {
	t := Task{ID: ChatTaskID(name, at), Kind: KindChat, Status: "running", BudgetEUR: budget, Repository: repository,
		RequiredCapabilities: []string{policy.ClassChat},
		Objective:            "the project agent's chat of " + name + " in " + at.UTC().Format("2006-01"), Source: "chat"}
	if err := s.insertTask(ctx, t, `ON CONFLICT (id) DO NOTHING`); err != nil {
		return Task{}, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'closed', finished_at = now()
		WHERE kind = $1 AND repository = $2 AND id <> $3 AND status = 'running'`, KindChat, repository, t.ID); err != nil {
		return Task{}, err
	}
	got, ok, err := s.Task(ctx, t.ID)
	if err == nil && !ok {
		err = fmt.Errorf("chat task %s not found after its insert", t.ID)
	}
	return got, err
}

// AddChatMessage appends m to login's conversation on repository.
func (s *PGStore) AddChatMessage(ctx context.Context, repository, login string, m ChatMessage) error {
	calls := any(nil)
	if len(m.ToolCalls) > 0 {
		b, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return err
		}
		calls = b
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_messages (repository, login, role, content, tool_calls) VALUES ($1, $2, $3, $4, $5::jsonb)`,
		repository, login, m.Role, m.Content, calls)
	return err
}

// ChatMessages are the newest limit messages of login's conversation on
// repository, oldest first.
func (s *PGStore) ChatMessages(ctx context.Context, repository, login string, limit int) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, content, tool_calls, at FROM (
		SELECT * FROM chat_messages WHERE repository = $1 AND login = $2 ORDER BY id DESC LIMIT $3) newest ORDER BY id`,
		repository, login, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var calls []byte
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &calls, &m.At); err != nil {
			return nil, err
		}
		if len(calls) > 0 {
			if err := json.Unmarshal(calls, &m.ToolCalls); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectSpend is a project's ledger spend in one UTC month, and the part
// its chat tasks booked.
type ProjectSpend struct {
	EUR, ChatEUR float64
}

// ProjectMonthSpend is repository's spend over the ledger in the UTC month
// of at, as the gateway's project headroom counts it.
func (s *PGStore) ProjectMonthSpend(ctx context.Context, repository string, at time.Time) (ProjectSpend, error) {
	var p ProjectSpend
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(l.eur), 0)::float8, COALESCE(sum(l.eur) FILTER (WHERE t.kind = $3), 0)::float8
		FROM ledger l JOIN tasks t ON t.id = l.task_id
		WHERE t.repository = $1 AND l.day >= date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC')::date
		AND l.day < (date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC') + interval '1 month')::date`,
		repository, at, KindChat).Scan(&p.EUR, &p.ChatEUR)
	return p, err
}
