package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// CaseHealReport counts what ReconcileCases changed: Withdrawn the cases
// of ended cards, of replaced plan revisions and of merges the factory
// denied; Resolved the merge cases a decision ended; Moved the cases of
// cards that moved on with no answer; Opened the pending merge requests
// that had no case.
type CaseHealReport struct {
	Withdrawn, Resolved, Moved, Opened int
}

// ReconcileCases ends repository's live cases the events writer cannot
// reach, and opens a case for each pending merge request it missed. The heal pass runs
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
		WHERE c.repository = $1 AND c.status IN `+caseLiveStatuses+` AND t.status IN ('done', 'failed', 'closed') AND c.kind NOT IN ($2, $3)`,
		repository, CaseMonitorFlag, CasePlanCriteria)
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
	// the ten minutes a face takes at most, unless a rung below the human
	// owns it with a wake on a card a run still holds: the monitor's sweep
	// faces it at the wake, and a face that fails keeps the wake for the
	// next sweep. The run ends the case of a wait it leaves. A merged card
	// takes no face, so its case goes as any other
	if r.Moved, err = s.withdrawCases(ctx, `SELECT c.id, 'the card moved on to ' || t.status FROM cases c JOIN tasks t ON t.id = c.task_id
		WHERE c.repository = $1 AND c.status IN `+caseLiveStatuses+` AND c.kind NOT IN ($2, $3, $4, $6, $7)
		AND t.status IN ('running', 'in-review', 'queued', 'waiting-deps', 'waiting-image', 'merged')
		AND NOT (c.owner_rung < 3 AND c.next_wake_at IS NOT NULL AND t.status <> 'merged')
		AND (c.status = $5 AND (case_face_comment(c.id) <> 0 OR t.issue_number = 0) OR c.opened_at < now() - interval '10 minutes')`,
		repository, CaseMergeApproval, CasePlanQuestion, CasePlanConfirmation, CaseAtHuman, CaseMonitorFlag, CasePlanCriteria); err != nil {
		return r, err
	}
	r.Opened, err = s.openMissedMerges(ctx, repository)
	return r, err
}

// endDecidedMergeCases ends repository's live merge cases whose request
// is no longer pending by its decision, and counts how they ended.
func (s *PGStore) endDecidedMergeCases(ctx context.Context, repository string) (resolved, withdrawn int, err error) {
	type decision struct {
		id               int64
		kind             CaseKind
		status, by, note string
	}
	ds, err := queryAll(ctx, s.db, `SELECT c.id, c.kind, a.status, a.decided_by, a.decision_note
		FROM cases c JOIN approval_requests a ON a.id = (c.facts->>'approval')::bigint
		WHERE c.repository = $1 AND c.kind IN ($2, $3) AND c.status IN `+caseLiveStatuses+` AND a.status <> 'pending'`,
		[]any{repository, CaseMergeApproval, CasePlanCriteria}, func(row rowScanner) (d decision, err error) {
			return d, row.Scan(&d.id, &d.kind, &d.status, &d.by, &d.note)
		})
	if err != nil {
		return 0, 0, err
	}
	for _, d := range ds {
		ended, err := s.endDecidedCase(ctx, d.id, d.kind, d.status, d.by, d.note)
		switch {
		case errors.Is(err, ErrNotFound), ended == "":
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

// endDecidedCase ends case id by its request's decision under the lock Act
// holds while it answers the case. A case Act is answering is Act's to end:
// the heal skips it ("") and the next pass takes it.
func (s *PGStore) endDecidedCase(ctx context.Context, id int64, kind CaseKind, status, by, note string) (string, error) {
	if s.TryLockCase != nil {
		unlock, ok := s.TryLockCase(id)
		if !ok {
			return "", nil
		}
		defer unlock()
	}
	return decideRequestCase(ctx, s.db, id, kind, status, by, note)
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

// openMissedMerges opens the merge case of each of repository's pending
// merge requests whose card has no live merge case, a request older than
// the cases table among them, and counts the cases it opened. A request
// younger than a minute is left to its approval_requested row, written
// after it; a request whose case a confirmation resolved stays pending until
// the criteria pass and is left as it is; an ended card's is left alone, as
// the ended rule would withdraw its case.
func (s *PGStore) openMissedMerges(ctx context.Context, repository string) (int, error) {
	ids, err := queryAll(ctx, s.db, `SELECT a.id FROM approval_requests a JOIN tasks t ON t.id = a.task_id
		WHERE t.repository = $1 AND a.kind IN ($2, $3, $4) AND a.status = 'pending' AND a.created_at < now() - interval '1 minute'
		AND (a.kind <> $2 OR t.status NOT IN ('done', 'failed', 'closed'))
		AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.task_id = a.task_id AND c.kind IN ($5, $6) AND (c.status IN `+caseLiveStatuses+` OR c.status = $7 AND c.resolution->>'action' = $8)
		    AND (c.facts->>'approval')::bigint = a.id)`,
		[]any{repository, ApprovalMerge, ApprovalCriteria, ApprovalCriteriaRerun, CaseMergeApproval, CasePlanCriteria, CaseResolved, ActConfirmCriteria}, func(row rowScanner) (id int64, err error) {
			return id, row.Scan(&id)
		})
	if err != nil {
		return 0, err
	}
	return openEach(ctx, s.db, ids, func(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
		return openRequestCase(ctx, tx, id)
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

// caseSignalTries is how many failed sends of one pending case signal end
// it, at a failure no retry cures: a frontend that is down or slow never
// ends a row. caseSignalLogEvery is how many failed sends pass between one
// row's log lines, one an hour at the heal's minute. caseSignalWait is how
// long a row waits for its own sender before the heal pass sends it, and
// caseSignalBatch how many one pass sends.
const (
	caseSignalTries    = 60
	caseSignalLogEvery = 60
	caseSignalWait     = time.Minute
	caseSignalBatch    = 100
)

// resendCaseSignals sends each case signal still pending caseSignalWait
// after it was written again (a send that failed, or one factoryd's exit cut
// off), and ends it once a send delivers it, or its card's run is over, or a
// send no retry cures fails it once it failed caseSignalTries times. A run
// takes a second copy of either signal as a no-op.
func resendCaseSignals(ctx context.Context, store Store, wf Workflows, now time.Time) {
	rows, err := store.PendingCaseSignals(ctx, now.Add(-caseSignalWait), caseSignalBatch)
	if err != nil {
		log.Printf("heal pass: pending case signals: %s", maskedErr(err))
		return
	}
	scrub := redact.New()
	for _, p := range rows {
		done := Final(p.Status) || p.Kind == "human"
		if !done {
			err := wf.Signal(ctx, p.TaskID, p.Name, p.Signal)
			done = err == nil
			switch {
			case err == nil:
			case !retryableSignal(err) && p.Tries+1 >= caseSignalTries:
				log.Printf("heal pass: case %d's %s to task %s: no send delivered it in %d tries, it ends: %s", p.CaseID, p.Name, p.TaskID, p.Tries+1,
					scrub.String(err.Error()))
				done = true
			case p.Tries%caseSignalLogEvery == 0:
				log.Printf("heal pass: case %d's %s to task %s failed %d times, now: %s", p.CaseID, p.Name, p.TaskID, p.Tries+1, scrub.String(err.Error()))
			}
		}
		if err := store.SettleCaseSignal(ctx, p.CaseID, p.Name, done); err != nil {
			log.Printf("heal pass: case %d's %s: %s", p.CaseID, p.Name, maskedErr(err))
		}
	}
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
		log.Printf("heal pass: cases: projects: %s", maskedErr(err))
		return
	}
	for _, p := range ps {
		if p.Repository == "" {
			continue
		}
		r, err := store.ReconcileCases(ctx, p.Repository)
		if err != nil {
			log.Printf("heal pass: cases of %s: %s", p.Repository, maskedErr(err))
			continue
		}
		if r != (CaseHealReport{}) {
			log.Printf("heal pass: cases of %s: %d withdrawn, %d resolved, %d moved on, %d opened",
				p.Repository, r.Withdrawn, r.Resolved, r.Moved, r.Opened)
		}
	}
}
