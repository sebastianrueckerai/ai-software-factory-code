package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// CaseHealReport counts what ReconcileCases changed: Withdrawn the cases
// of ended cards, of replaced plan revisions and of merges the factory
// denied; Resolved the merge cases a decision ended; Moved the cases of
// cards that moved on with no answer; Opened the asks and the pending
// merge requests that had no case.
type CaseHealReport struct {
	Withdrawn, Resolved, Moved, Opened int
}

// ReconcileCases ends repository's live cases the events writer cannot
// reach, and opens a case for each ask and pending merge request it
// missed, those older than the cases table among them. The heal pass runs
// it once a minute.
func (s *PGStore) ReconcileCases(ctx context.Context, repository string) (CaseHealReport, error) {
	var r CaseHealReport
	var err error
	// first, so a decided merge on an ended card reads as decided
	if r.Resolved, r.Withdrawn, err = s.endDecidedMergeCases(ctx, repository); err != nil {
		return r, err
	}
	// a monitor flag's case on an ended card asks its issue closed: the
	// monitor withdraws it once the issue closes or the flag no longer holds
	ended, err := s.withdrawCases(ctx, `SELECT c.id, 'card ended ' || t.status FROM cases c JOIN tasks t ON t.id = c.task_id
		WHERE c.repository = $1 AND c.status IN `+caseLiveStatuses+` AND t.status IN ('done', 'failed', 'closed') AND c.kind <> $2`,
		repository, CaseMonitorFlag)
	if err != nil {
		return r, err
	}
	replaced, err := s.withdrawCases(ctx, `SELECT c.id, 'plan revision ' || p.revision || ' replaced it'
		FROM cases c JOIN LATERAL (SELECT max(revision) AS revision FROM plan_revisions WHERE task_id = c.task_id) p ON true
		WHERE c.repository = $1 AND c.status IN `+caseLiveStatuses+` AND c.kind IN ($2, $3) AND (c.facts->>'revision')::int < p.revision`,
		repository, CasePlanQuestion, CasePlanConfirmation)
	if err != nil {
		return r, err
	}
	r.Withdrawn += ended + replaced
	// a merge waits while its card is in review, a plan question is asked
	// while its planner runs and a monitor flag's case sits beside the
	// card's run: each has its own end. A trip opens its
	// case while the card still runs and moves the card only with its
	// face, so a case goes once it has its face (at the human with its
	// comment, or with none on a card with no issue) or once it sat past
	// the ten minutes a face takes at most
	if r.Moved, err = s.withdrawCases(ctx, `SELECT c.id, 'the card moved on to ' || t.status FROM cases c JOIN tasks t ON t.id = c.task_id
		WHERE c.repository = $1 AND c.status IN `+caseLiveStatuses+` AND c.kind NOT IN ($2, $3, $4, $6)
		AND t.status IN ('running', 'in-review', 'queued', 'waiting-deps', 'waiting-image', 'merged')
		AND (c.status = $5 AND (c.comment_id <> 0 OR t.issue_number = 0) OR c.opened_at < now() - interval '10 minutes')`,
		repository, CaseMergeApproval, CasePlanQuestion, CasePlanConfirmation, CaseAtHuman, CaseMonitorFlag); err != nil {
		return r, err
	}
	if r.Opened, err = s.openMissedAsks(ctx, repository); err != nil {
		return r, err
	}
	merges, err := s.openMissedMerges(ctx, repository)
	r.Opened += merges
	return r, err
}

// endDecidedMergeCases ends repository's live merge cases whose request
// is no longer pending by its decision, and counts how they ended.
func (s *PGStore) endDecidedMergeCases(ctx context.Context, repository string) (resolved, withdrawn int, err error) {
	type decision struct {
		id               int64
		status, by, note string
	}
	ds, err := queryAll(ctx, s.db, `SELECT c.id, a.status, a.decided_by, a.decision_note
		FROM cases c JOIN approval_requests a ON a.id = (c.facts->>'approval')::bigint
		WHERE c.repository = $1 AND c.kind = $2 AND c.status IN `+caseLiveStatuses+` AND a.status <> 'pending'`,
		[]any{repository, CaseMergeApproval}, func(row rowScanner) (d decision, err error) {
			return d, row.Scan(&d.id, &d.status, &d.by, &d.note)
		})
	if err != nil {
		return 0, 0, err
	}
	for _, d := range ds {
		switch ended, err := decideMergeCase(ctx, s.db, d.id, d.status, d.by, d.note); {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return resolved, withdrawn, err
		case ended == CaseWithdrawn:
			withdrawn++
		default:
			resolved++
		}
	}
	return resolved, withdrawn, nil
}

// withdrawCases withdraws each live case query answers, for the reason
// it answers beside the case's id, and counts the cases it withdrew.
func (s *PGStore) withdrawCases(ctx context.Context, query string, args ...any) (int, error) {
	type withdrawal struct {
		id     int64
		reason string
	}
	ws, err := queryAll(ctx, s.db, query, args, func(row rowScanner) (w withdrawal, err error) {
		return w, row.Scan(&w.id, &w.reason)
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, w := range ws {
		switch err := withdrawCase(ctx, s.db, w.id, w.reason); {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return n, err
		default:
			n++
		}
	}
	return n, nil
}

// openMissedAsks opens the case of each waiting-human card of repository
// from its newest escalation row with a question, when that question has
// an ask (white space alone is none), as that ask's question_asked would
// have, and counts the cases it opened. An ask younger than a
// minute is left to its writer, which records the event after it moves
// the card. A card with a live case, or with a case opened or ended since
// that row, has its case: a trip opens its case before its ask, and an
// answer ends it before the run moves the card on. A monitor case beside
// the card is no ask of the card's. A card that left
// waiting-human since that row waits on something else, as a planner on
// its plan's questions.
func (s *PGStore) openMissedAsks(ctx context.Context, repository string) (int, error) {
	asks, err := queryAll(ctx, s.db, `SELECT t.id, t.kind, t.source, esc.body, esc.comment_id, esc.question FROM tasks t
		JOIN LATERAL (SELECT e.body, e.comment_id, e.question, e.created_at FROM intake_events e
		    WHERE e.task_id = t.id AND e.kind = $2 AND e.question IS NOT NULL ORDER BY e.id DESC LIMIT 1) esc ON true
		WHERE t.repository = $1 AND t.status = 'waiting-human' AND esc.question->>'ask' ~ '\S'
		AND esc.created_at < now() - interval '1 minute'
		AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.task_id = t.id AND c.kind <> $4
		    AND (c.status IN `+caseLiveStatuses+` OR c.opened_at >= esc.created_at OR c.resolved_at >= esc.created_at))
		AND NOT EXISTS (SELECT 1 FROM events v WHERE v.task_id = t.id AND v.kind = $3 AND v.at > esc.created_at
		    AND v.detail->>'status' <> 'waiting-human')`,
		[]any{repository, IntakeEscalation, EventTaskStatus, CaseMonitorFlag}, func(row rowScanner) (Event, error) {
			e := Event{Kind: EventQuestionAsked}
			var kind, source, body string
			var comment int64
			var raw []byte
			if err := row.Scan(&e.TaskID, &kind, &source, &body, &comment, &raw); err != nil {
				return e, err
			}
			var q HumanQuestion
			if err := json.Unmarshal(raw, &q); err != nil {
				return e, err
			}
			e.Detail = map[string]any{"question": textutil.Truncate(q.Ask, 300), "ask": q.Ask, "why": q.Why, "info": q.Info, "comment": comment}
			if kind == "human" {
				// recordAsk's event names the card's source and no reason
				e.Detail["source"] = source
			} else {
				e.Detail["source"], e.Detail["reason"] = escalationSource(body), textutil.Truncate(body, 300)
			}
			if q.Recommendation != "" {
				e.Detail["recommendation"] = q.Recommendation
			}
			if len(q.Choices) > 0 {
				e.Detail["choices"] = q.Labels()
			}
			return e, nil
		})
	if err != nil {
		return 0, err
	}
	return openEach(ctx, s.db, asks, askCase)
}

// openMissedMerges opens the merge case of each of repository's pending
// merge requests whose card has no live merge case, a request older than
// the cases table among them, and counts the cases it opened. A request
// younger than a minute is left to its approval_requested row, written
// after it; an ended card's is left alone, as the ended rule would
// withdraw its case.
func (s *PGStore) openMissedMerges(ctx context.Context, repository string) (int, error) {
	ids, err := queryAll(ctx, s.db, `SELECT a.id FROM approval_requests a JOIN tasks t ON t.id = a.task_id
		WHERE t.repository = $1 AND a.kind = $2 AND a.status = 'pending' AND a.created_at < now() - interval '1 minute'
		AND t.status NOT IN ('done', 'failed', 'closed')
		AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.task_id = a.task_id AND c.kind = $3 AND c.status IN `+caseLiveStatuses+`)`,
		[]any{repository, ApprovalMerge, CaseMergeApproval}, func(row rowScanner) (id int64, err error) {
			return id, row.Scan(&id)
		})
	if err != nil {
		return 0, err
	}
	return openEach(ctx, s.db, ids, func(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
		return openMergeCase(ctx, tx, id)
	})
}

// openEach runs open on each of items, each in its own transaction as the
// events writer runs it inside its own, and counts the cases it opened.
func openEach[T any](ctx context.Context, db *sql.DB, items []T, open func(context.Context, *sql.Tx, T) (bool, error)) (int, error) {
	n := 0
	for _, item := range items {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return n, err
		}
		opened, err := open(ctx, tx, item)
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			_ = tx.Rollback() // #nosec G104 -- the error that matters is err
			return n, err
		}
		if opened {
			n++
		}
	}
	return n, nil
}

// queryAll reads every row of query through scan, so its caller writes
// with the rows closed.
func queryAll[T any](ctx context.Context, db *sql.DB, query string, args []any, scan func(rowScanner) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// reconcileCases runs ReconcileCases on every project's repository and
// logs what it changed.
func reconcileCases(ctx context.Context, store Store, projects interface {
	Projects(ctx context.Context) ([]Project, error)
}) {
	if projects == nil {
		return
	}
	ps, err := projects.Projects(ctx)
	if err != nil {
		log.Printf("heal pass: cases: projects: %v", err)
		return
	}
	for _, p := range ps {
		if p.Repository == "" {
			continue
		}
		r, err := store.ReconcileCases(ctx, p.Repository)
		if err != nil {
			log.Printf("heal pass: cases of %s: %v", p.Repository, err)
			continue
		}
		if r != (CaseHealReport{}) {
			log.Printf("heal pass: cases of %s: %d withdrawn, %d resolved, %d moved on, %d opened",
				p.Repository, r.Withdrawn, r.Resolved, r.Moved, r.Opened)
		}
	}
}
