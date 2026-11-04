package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// monitorKinds are the events that move a card's flags: task_status,
// task_ended, session_end, session_pushed, gate_head, model_review, gate_audit,
// card_funded, card_extended, question_asked, question_answered,
// plan_question_answered, approval_requested, merge_approved, merge_denied,
// pull_request_merged, card_created.
var monitorKinds = []string{EventTaskStatus, EventTaskEnded, EventSessionEnd, EventSessionPushed, EventGateHead, EventModelReview,
	EventGateAudit, EventCardFunded, EventCardExtended, EventQuestionAsked, EventQuestionAnswered, EventPlanQuestionAnswered,
	EventApprovalRequested, EventMergeApproved, EventMergeDenied, EventPullRequestMerged, EventCardCreated}

// monitorBatch bounds the events one read takes and the due cases one
// sweep wakes.
const monitorBatch = 100

// Monitor opens a case for every flag a card's digest raises that is new
// for its state: on the board's events behind a cursor, and on every live
// card once a minute for the waits no event ends. No model runs in it:
// rung 0 takes what needs no judgment and hands the rest up.
type Monitor struct {
	Store  Store
	Runner *Runner
	Policy func() policy.Tree
	Now    func() time.Time
	// mu holds one card at a time, so the pass and the sweep never open
	// one flag's case twice
	mu sync.Mutex
}

// Loop runs a pass every interval until ctx ends.
func (m *Monitor) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := m.Pass(ctx); err != nil {
				log.Printf("monitor: %v", err)
			}
		}
	}
}

// Pass reads the events past the cursor in batches of 100 and runs card on
// each card they touch once, then moves the cursor past the batch. A card
// that fails is logged and skipped: the minute sweep reads it again, and a
// batch read again after a crash opens nothing twice.
func (m *Monitor) Pass(ctx context.Context) error {
	var after int64
	for {
		events, err := m.Store.MonitorEvents(ctx, after, monitorKinds, monitorBatch)
		if err != nil || len(events) == 0 {
			return err
		}
		seen := map[string]bool{}
		for _, e := range events {
			if e.TaskID == "" || seen[e.TaskID] {
				continue
			}
			seen[e.TaskID] = true
			if err := m.logged(ctx, e.TaskID); err != nil {
				return err
			}
		}
		after = events[len(events)-1].ID
		if err := m.Store.SetMonitorCursor(ctx, after); err != nil {
			return err
		}
		if len(events) < monitorBatch {
			return nil
		}
	}
}

// Sweep runs card on every live card of every project, wakes the due cases
// (rung 0 again through OpenTrip's repeat path) and expires the steps past
// their bound. The minute loop calls it.
func (m *Monitor) Sweep(ctx context.Context) error {
	var errs []error
	projects := m.Policy().Projects
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		cards, err := m.Store.BoardTasks(ctx, projects[name].Repository)
		if err != nil {
			errs = append(errs, fmt.Errorf("live cards of %s: %w", name, err))
			continue
		}
		for _, c := range cards {
			if err := m.logged(ctx, c.ID); err != nil {
				return err
			}
		}
	}
	now := m.Now()
	due, err := m.Store.DueCases(ctx, now, monitorBatch)
	if err != nil {
		errs = append(errs, fmt.Errorf("due cases: %w", err))
	}
	for _, c := range due {
		if err := m.wake(ctx, c); err != nil && ctx.Err() == nil {
			log.Printf("monitor: wake case %d of card %s: %v", c.ID, c.TaskID, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if _, err := m.Store.ExpireSteps(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("expire steps: %w", err))
	}
	return errors.Join(errs...)
}

// logged runs card on card taskID and logs what fails. It answers only the
// end of ctx.
func (m *Monitor) logged(ctx context.Context, taskID string) error {
	if err := m.card(ctx, taskID); err != nil && ctx.Err() == nil {
		log.Printf("monitor: card %s: %v", taskID, err)
	}
	return ctx.Err()
}

// card builds the digest, reads its flags, and opens a case for each flag
// that is new for its state key: kind monitor_flag, problem the flag's
// line, facts the digest's facts plus flag and state, state key
// flag|state. A case with that key in any status is the flag's case, so a
// standing flag costs no call and a flag rung 0 resolved opens nothing
// twice. The card's own dependency_failed case on a dependency is the
// flag's case for it. An ended card raises only what it left open, and a
// merged or closing card raises nothing, since no face parks either.
func (m *Monitor) card(ctx context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.Runner.CardDigest(ctx, taskID)
	if err != nil {
		return err
	}
	cases, err := m.Store.CasesOf(ctx, taskID)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, c := range cases {
		switch c.Kind {
		case CaseMonitorFlag:
			held[c.StateKey] = true
		case CaseDependencyFailed:
			held["dependency_failed|"+c.StateKey] = true
		}
	}
	head := digestHeadCommit(d)
	var errs []error
	for _, f := range d.Flags {
		if !raises(d.Head.Status, f.Name) {
			continue
		}
		state, facts, ok := flagState(d, f, head)
		key := f.Name + "|" + state
		if !ok || held[key] {
			continue
		}
		held[key] = true
		facts["flag"], facts["detail"], facts["state"], facts["digest"] = f.Name, f.Detail, state, d.Render()
		in := TripInput{TaskID: taskID, Reason: "Monitor flag: " + flagsText([]DigestFlag{f}), Head: head, Kind: CaseMonitorFlag, StateKey: key, Extra: facts}
		if _, err := m.Runner.OpenTrip(ctx, in, tripAttempt{}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// wake runs rung 0 on due case c again through OpenTrip's repeat path, on
// the case's kind, key, reason and head, and clears its wake: the rung
// that holds the case after owns it.
func (m *Monitor) wake(ctx context.Context, c Case) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	reason, _ := c.Facts["reason"].(string)
	head, _ := c.Facts["head"].(string)
	out, err := m.Runner.OpenTrip(ctx, TripInput{TaskID: c.TaskID, Reason: cmp.Or(reason, c.Problem), Head: head, Kind: c.Kind, StateKey: c.StateKey},
		tripAttempt{})
	if err != nil {
		return err
	}
	// a case rung 0 resolved has no wake left
	return caseEnded(m.Store.SetCaseWake(ctx, c.ID, out.Rung, c.WakeOn, nil))
}

// raises reports whether a card in status opens a case for flag.
func raises(status, flag string) bool {
	switch {
	case Final(status):
		return flag == "issue_open_after_end"
	case status == "merged", status == "closing":
		return false
	}
	return true
}

// flagState is the state flag f of digest d fired on, with the facts it
// adds: a wait's kind and start, with its owner and its bound; a failed
// dependency's task id, matched by the flag's issue; head for any other
// flag. ok is false when d cannot name the state.
func flagState(d CardDigest, f DigestFlag, head string) (string, map[string]any, bool) {
	switch f.Name {
	case "wait_over_bound", "queued_over_bound":
		w, ok := d.ownedWait()
		since := w.Since.UTC().Format(time.RFC3339)
		return w.Kind + "|" + since, map[string]any{"wait": w.Kind, "owner_rung": w.Rung, "since": since, "bound": durationText(d.Waits.Bound(w.Kind))}, ok
	case "dependency_failed":
		i := slices.IndexFunc(d.deps, func(dep Dependency) bool { return dep.Status == "failed" && fmt.Sprintf("#%d", dep.Issue) == f.Detail })
		if i < 0 {
			return "", nil, false
		}
		return d.deps[i].TaskID, map[string]any{}, true
	}
	return head, map[string]any{}, true
}

// digestHeadCommit is the head the newest judged line of digest d names,
// "" before the gates judged one.
func digestHeadCommit(d CardDigest) string {
	for _, s := range slices.Backward(d.Steps) {
		if s.commit != "" {
			return s.commit
		}
	}
	return ""
}

// MonitorEvents are the events of kinds past the monitor's cursor and past
// after, oldest first, at most limit. The first read plants the cursor at
// the newest event, so no history raises a flag.
func (s *PGStore) MonitorEvents(ctx context.Context, after int64, kinds []string, limit int) ([]Event, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO monitor_cursor (event_id) SELECT coalesce(max(id), 0) FROM events
		ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE id > greatest($1, (SELECT event_id FROM monitor_cursor))
		AND kind = ANY($2) ORDER BY id LIMIT $3`, after, kinds, limit)
}

// SetMonitorCursor moves the monitor's cursor to eventID, never back.
func (s *PGStore) SetMonitorCursor(ctx context.Context, eventID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE monitor_cursor SET event_id = greatest(event_id, $1)`, eventID)
	return err
}
