package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// Proposal is the token of a call proposed to the member.
	Proposal string `json:"proposal,omitempty"`
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

// ChatProposal is a proposal of the project agent waiting for its
// member's Do it (kind proposal), or an applied action's undo (kind undo):
// tool with args, bound to the member and the project until it expires.
// Description is a proposal's line for the member, or the name of the
// tool whose action an undo undoes.
type ChatProposal struct {
	Token, Kind, Repository, Login, Tool string
	Args                                 json.RawMessage
	Description                          string
	ExpiresAt                            time.Time
}

// TakeProposal's refusals.
var (
	ErrNoProposal     = errors.New("no such proposal or undo")
	ErrOthersProposal = errors.New("the proposal or undo is another member's")
)

// PutProposal stores p.
func (s *PGStore) PutProposal(ctx context.Context, p ChatProposal) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_proposals (token, kind, repository, login, tool, args, description, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`,
		p.Token, p.Kind, p.Repository, p.Login, p.Tool, []byte(p.Args), p.Description, p.ExpiresAt)
	return err
}

// TakeProposal uses token for login on repository at now, once: the row
// unused and unexpired, marked used. ErrOthersProposal for such a row of
// another login, which stays unused; ErrNoProposal for none.
func (s *PGStore) TakeProposal(ctx context.Context, token, repository, login string, now time.Time) (ChatProposal, error) {
	p := ChatProposal{Token: token, Repository: repository, Login: login}
	var args []byte
	err := s.db.QueryRowContext(ctx, `UPDATE chat_proposals SET used_at = $4
		WHERE token = $1 AND repository = $2 AND login = $3 AND used_at IS NULL AND expires_at >= $4
		RETURNING kind, tool, args, description, expires_at`, token, repository, login, now).
		Scan(&p.Kind, &p.Tool, &args, &p.Description, &p.ExpiresAt)
	switch {
	case err == nil:
		p.Args = args
		return p, nil
	case !errors.Is(err, sql.ErrNoRows):
		return ChatProposal{}, err
	}
	var others bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM chat_proposals
		WHERE token = $1 AND repository = $2 AND used_at IS NULL AND expires_at >= $3)`, token, repository, now).Scan(&others); err != nil {
		return ChatProposal{}, err
	}
	if others {
		return ChatProposal{}, ErrOthersProposal
	}
	return ChatProposal{}, ErrNoProposal
}

// PurgeProposals deletes the rows expired or used before before and
// answers how many.
func (s *PGStore) PurgeProposals(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat_proposals WHERE expires_at < $1 OR used_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
