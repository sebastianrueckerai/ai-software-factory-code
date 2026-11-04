package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
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
// their bound. The minute loop calls it. A merged or closing card raises
// no case, so the sweep reads neither.
func (m *Monitor) Sweep(ctx context.Context) error {
	var errs []error
	tree := m.Policy()
	for _, status := range Statuses {
		if Final(status) || status == "merged" || status == "closing" {
			continue
		}
		cards, err := m.Store.ListTasksByStatus(ctx, status)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s cards: %w", status, err))
			continue
		}
		for _, c := range cards {
			if _, _, ok := tree.ProjectOf(c.Repository); !ok || c.Kind == KindChat {
				continue
			}
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
// flag|state. A faced case with that key, at the human or ended, is the
// flag's case, so a standing flag costs no call and a flag rung 0 resolved
// opens nothing twice. A case still open has no face yet: OpenTrip
// finishes it. A live case whose flag no longer holds on its key is
// withdrawn first, by a question_withdrawn row that names it: the wait
// ended, or the flag no longer holds.
func (m *Monitor) card(ctx context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.Runner.CardDigest(ctx, taskID)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, c := range d.cases {
		if c.Kind == CaseMonitorFlag && c.Status != CaseOpen {
			held[c.StateKey] = true
		}
	}
	type opening struct {
		key, state string
		f          DigestFlag
		facts      map[string]any
	}
	var opens []opening
	holds := map[string]bool{}
	for _, f := range d.Flags {
		state, facts, ok := flagCase(d, f)
		key := f.Name + "|" + state
		if !ok {
			continue
		}
		holds[key] = true
		if !held[key] {
			held[key] = true
			opens = append(opens, opening{key, state, f, facts})
		}
	}
	var errs []error
	// first what ended, so a withdrawal settles no ask this pass opens
	for _, c := range d.cases {
		if c.Kind != CaseMonitorFlag || c.Status != CaseOpen && c.Status != CaseAtHuman || holds[c.StateKey] {
			continue
		}
		reason := "the flag no longer holds"
		if flag := c.Facts["flag"]; flag == "wait_over_bound" || flag == "queued_over_bound" {
			reason = "the wait ended"
		}
		// the row names its case, which the events writer withdraws, and
		// settles the case's ask in the inbox
		if _, err := m.Store.Record(ctx, Event{Repository: c.Repository, TaskID: c.TaskID, IssueNumber: int64(d.Head.Issue), Kind: EventQuestionWithdrawn,
			Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"case_id": c.ID, "reason": reason}}); err != nil {
			errs = append(errs, fmt.Errorf("withdraw %s: %w", c.StateKey, err))
		}
	}
	head := digestHeadCommit(d)
	for _, o := range opens {
		o.facts["flag"], o.facts["detail"], o.facts["state"], o.facts["digest"] = o.f.Name, o.f.Detail, o.state, d.Render()
		in := TripInput{TaskID: taskID, Reason: "Monitor flag: " + flagsText([]DigestFlag{o.f}), Head: head, Kind: CaseMonitorFlag, StateKey: o.key, Extra: o.facts}
		if _, err := m.Runner.OpenTrip(ctx, in, tripAttempt{}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.key, err))
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

// eventFlags are the flags about an event of the card's life, whose detail
// names the event.
var eventFlags = []string{"checkin_cut", "session_no_push", "grant_bought_nothing", "finding_confirmed_twice", "budget_moved_no_event",
	"answer_changed_nothing"}

// flagCase is the state flag f of digest d fired on, with the facts it
// adds, and ok false for a flag that opens no case. An ended card opens
// one only for what it left open where Finish closes it, and a merged or
// closing card none. A
// slot held by a wait is a record, and a failed dependency is its run's,
// whose own trip takes it. A wait keys on its kind and start, with its
// owner and its bound: one wait opens one case, so queued_over_bound takes
// the queue's and wait_over_bound leaves it. An event flag keys on its
// detail, and any other flag, about the head's state, on the head.
func flagCase(d CardDigest, f DigestFlag) (string, map[string]any, bool) {
	switch status := d.Head.Status; {
	case Final(status):
		if f.Name != "issue_open_after_end" || !d.issueCloses {
			return "", nil, false
		}
	case status == "merged", status == "closing", f.Name == "slot_held_by_wait", f.Name == "dependency_failed":
		return "", nil, false
	}
	switch {
	case f.Name == "wait_over_bound" || f.Name == "queued_over_bound":
		w, ok := d.ownedWait()
		since := w.Since.UTC().Format(time.RFC3339)
		facts := map[string]any{"wait": w.Kind, "owner_rung": w.Rung, "since": since, "bound": durationText(d.Waits.Bound(w.Kind))}
		return w.Kind + "|" + since, facts, ok && !(f.Name == "wait_over_bound" && w.Kind == "queued")
	case slices.Contains(eventFlags, f.Name):
		return f.Detail, map[string]any{}, true
	}
	return digestHeadCommit(d), map[string]any{}, true
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
