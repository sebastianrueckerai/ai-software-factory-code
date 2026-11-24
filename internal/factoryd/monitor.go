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
// pull_request_merged, card_created, issue_closed.
var monitorKinds = []string{EventTaskStatus, EventTaskEnded, EventSessionEnd, EventSessionPushed, EventGateHead, EventModelReview,
	EventGateAudit, EventCardFunded, EventCardExtended, EventQuestionAsked, EventQuestionAnswered, EventPlanQuestionAnswered,
	EventApprovalRequested, EventMergeApproved, EventMergeDenied, EventPullRequestMerged, EventCardCreated, EventIssueClosed}

// monitorBatch bounds the events one read takes and the due cases one
// case-wake pass wakes.
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

// Sweep runs card on every live card of every project. Its own pass runs
// it every minute. A merged or closing card raises no case, so the sweep
// reads neither.
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
	// an ended card's monitor flag has no event left to clear it: while
	// its case sits, the sweep reads the card, so a condition that
	// cleared with no row on the card — its issue closed on the forge
	// with none — withdraws
	flagged, err := m.Store.FlaggedEnds(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("ended cards with a flag's case: %w", err))
	}
	for _, id := range flagged {
		if err := m.logged(ctx, id); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// WakeCases is the case-wake pass, run every 5 seconds: it ends the steps
// past their bound and climbs their cases, then wakes each due case
// through OpenTrip's repeat path (wake), at most monitorBatch. A case on a
// card that cannot wait for a human is left to the heal pass, unless it
// outlives its card.
func (m *Monitor) WakeCases(ctx context.Context) error {
	var errs []error
	now := m.Now()
	// a rung's step past its bound ends first, so the wake of its case,
	// due at the same time, finds the case climbed
	expired, err := m.Store.ExpireSteps(ctx, now)
	if err != nil {
		errs = append(errs, fmt.Errorf("expire steps: %w", err))
	}
	if err := m.Runner.ClimbExpired(ctx, expired); err != nil {
		errs = append(errs, fmt.Errorf("climb after expired steps: %w", err))
	}
	due, err := m.Store.DueCases(ctx, now, monitorBatch)
	if err != nil {
		errs = append(errs, fmt.Errorf("due cases: %w", err))
	}
	for _, c := range due {
		if t, ok, err := m.Store.Task(ctx, c.TaskID); err == nil && ok && !slices.Contains(openStatuses, t.Status) && !outlivesCard(c, t) {
			continue
		}
		if err := m.wake(ctx, c); err != nil && ctx.Err() == nil {
			log.Printf("monitor: wake case %d of card %s: %v", c.ID, c.TaskID, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}

// Retake runs rung 0 again on every case at the human of a project whose
// kind rung 0 tries a firing row on (firesOn), its own pass once a minute:
// a row that fires on the case's facts now takes it from the human
// (retake). Every other case is skipped unread. A repository no project
// names is not read, as the sweep reads none of its cards.
func (m *Monitor) Retake(ctx context.Context) error {
	var errs []error
	tree := m.Policy()
	for _, name := range slices.Sorted(maps.Keys(tree.Projects)) {
		cases, err := m.Store.OpenCasesIn(ctx, tree.Projects[name].Repository)
		if err != nil {
			errs = append(errs, fmt.Errorf("cases of %s: %w", name, err))
			continue
		}
		for _, c := range cases {
			if c.Status != CaseAtHuman || !firesOn(c.Kind) {
				continue
			}
			if err := m.retake(ctx, c); err != nil {
				errs = append(errs, fmt.Errorf("case %d of card %s: %w", c.ID, c.TaskID, err))
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return errors.Join(errs...)
}

// retake is Runner.retake on case c, one card at a time with the passes.
func (m *Monitor) retake(ctx context.Context, c Case) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Runner.retake(ctx, c)
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
// line, facts the digest's facts plus flag and state read on the head of
// the card's branch, state key flag|state. A faced case with that key, at the human or ended, is the
// flag's case, so a standing flag costs no call and a flag rung 0 resolved
// opens nothing twice. A case still open has no face yet: OpenTrip
// finishes it. A live case whose flag no longer holds on its key is
// withdrawn first, by a question_withdrawn row that names it when it is
// at the human, else with no row: the wait ended, the issue is closed, or
// the flag no longer holds. An issue closed on the forge with no row on
// the card counts as closed there too.
func (m *Monitor) card(ctx context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.Runner.CardDigest(ctx, taskID)
	if err != nil {
		return err
	}
	forgeClosed := m.closedOnForge(ctx, taskID, d)
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
		// the issue closed on the forge with no row on the card cleared
		// the flag's condition there, though the digest, which reads rows
		// alone, still raises the flag
		cleared := !holds[c.StateKey] || c.Facts["flag"] == "issue_open_after_end" && forgeClosed
		if c.Kind != CaseMonitorFlag || c.Status != CaseOpen && c.Status != CaseAtHuman || !cleared {
			continue
		}
		reason := "the flag no longer holds"
		switch flag := c.Facts["flag"]; {
		case flag == "wait_over_bound" || flag == "queued_over_bound":
			reason = "the wait ended"
		case flag == "issue_open_after_end" && d.issueClosed || flag == "issue_open_after_end" && forgeClosed:
			reason = "the issue is closed"
		}
		var err error
		if c.Status == CaseOpen {
			// no face asked anyone: the case ends with no row
			err = m.Runner.EndCase(ctx, c.ID, reason)
		} else {
			// the issue's comment withdraws the question where the face
			// asked it; the row names its case, which the events writer
			// withdraws, and settles the case's ask in the inbox
			err = m.withdrawAtHuman(ctx, d, c, reason)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("withdraw %s: %w", c.StateKey, err))
		}
	}
	if len(opens) == 0 {
		return errors.Join(errs...)
	}
	// a case judges the work as it stands: the head of the card's branch,
	// which its pull request shows before any gate judged it; with no branch
	// past the base, the head the gates judged last
	head, err := m.Runner.workHead(ctx, taskID)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("the head of the card's branch: %w", err))...)
	}
	head = cmp.Or(head, digestHeadCommit(d))
	for _, o := range opens {
		o.facts["flag"], o.facts["detail"], o.facts["state"], o.facts["digest"] = o.f.Name, o.f.Detail, o.state, d.Render()
		in := TripInput{TaskID: taskID, Reason: MonitorFlagReason(o.f), Head: head, Kind: CaseMonitorFlag, StateKey: o.key, Extra: o.facts}
		if _, err := m.Runner.OpenTrip(ctx, in, tripAttempt{}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.key, err))
		}
	}
	return errors.Join(errs...)
}

// withdrawAtHuman withdraws monitor case c at the human of digest d's card
// for reason: the comment that withdraws its question (postWithdrawal),
// then the question_withdrawn row that names the case and the comment.
func (m *Monitor) withdrawAtHuman(ctx context.Context, d CardDigest, c Case, reason string) error {
	t, _, err := m.Runner.task(ctx, c.TaskID)
	if err != nil {
		return err
	}
	detail := map[string]any{"case_id": c.ID, "case_kind": string(c.Kind), "reason": reason, "problem": c.Problem}
	id, err := m.Runner.postWithdrawal(ctx, t, c, reason)
	if err != nil {
		return err
	}
	if id != 0 {
		detail["withdrawal"] = id
	}
	_, err = m.Store.Record(ctx, Event{Repository: c.Repository, TaskID: c.TaskID, IssueNumber: int64(d.Head.Issue), Kind: EventQuestionWithdrawn,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail})
	return err
}

// closedOnForge tells whether taskID's issue, whose close no event row
// holds — the close predates the card's end, or its webhook never landed
// — is closed on the forge: the condition of a live issue_open_after_end
// case that cleared there. It asks only a card with such a case, and a
// failed read answers false: the next pass asks again.
func (m *Monitor) closedOnForge(ctx context.Context, taskID string, d CardDigest) bool {
	if d.issueClosed || m.Runner.forgejo == nil {
		return false
	}
	live := slices.ContainsFunc(d.cases, func(c Case) bool {
		return c.Kind == CaseMonitorFlag && c.Facts["flag"] == "issue_open_after_end" &&
			(c.Status == CaseOpen || c.Status == CaseAtHuman)
	})
	if !live {
		return false
	}
	t, ok, err := m.Store.Task(ctx, taskID)
	if err != nil || !ok || t.IssueNumber == 0 {
		return false
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return false
	}
	closed, err := m.Runner.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber)
	return err == nil && closed
}

// wake runs rung 0 on due case c again through OpenTrip's repeat path, on
// the case's kind, key, reason and head, and clears the wake it was due
// at: the rung that holds the case after owns it. A rung that took the
// case set its own wake at its step's due, which stays.
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
	live, err := m.Store.CaseByID(ctx, c.ID)
	if err != nil {
		return err
	}
	if live.NextWakeAt == nil || c.NextWakeAt != nil && !live.NextWakeAt.Equal(*c.NextWakeAt) {
		return nil
	}
	// a case rung 0 resolved has no wake left
	return caseEnded(m.Store.SetCaseWake(ctx, c.ID, out.Rung, c.WakeOn, nil))
}

// eventFlags are the flags whose detail names what they fired on: an
// event of the card's life, or one of its cases.
var eventFlags = []string{"checkin_cut", "checkin_refused", "session_no_push", "grant_bought_nothing", "finding_confirmed_twice", "budget_moved_no_event",
	"answer_changed_nothing", "faced_unjudged"}

// flagCase is the state flag f of digest d fired on, with the facts it
// adds, and ok false for a flag that opens no case. An ended card opens
// one only for a case at the human no rung judged, some of which outlive
// their card, and for what it left open where Finish closes it, until
// its issue's close is among its events; a merged or closing card none. A
// failed dependency is its run's, whose own trip takes it. A wait keys on
// its kind and start, with its owner and its bound: one wait opens one
// case, so queued_over_bound takes the queue's and wait_over_bound leaves
// it. An event flag keys on its
// detail, and any other flag, about the head's state, on the head.
func flagCase(d CardDigest, f DigestFlag) (string, map[string]any, bool) {
	switch status := d.Head.Status; {
	case Final(status):
		if f.Name != "faced_unjudged" && (f.Name != "issue_open_after_end" || !d.issueCloses || d.issueClosed) {
			return "", nil, false
		}
	case status == "merged", status == "closing", f.Name == "dependency_failed":
		return "", nil, false
	}
	switch {
	case f.Name == "wait_over_bound" || f.Name == "queued_over_bound":
		w, ok := d.ownedWait()
		since := w.Since.UTC().Format(time.RFC3339)
		facts := map[string]any{"wait": w.Kind, "owner_rung": w.Rung, "since": since, "bound": DurationText(d.Waits.Bound(w.Kind))}
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

// FlaggedEnds are the ids of the ended cards that hold a live monitor
// flag's case, distinct, and no chat card's: the sweep reads each, so a
// flag whose condition cleared with no event left on the card — its
// issue closed on the forge with no row — still withdraws.
func (s *PGStore) FlaggedEnds(ctx context.Context) ([]string, error) {
	return queryAll(ctx, s.db, `SELECT c.task_id FROM cases c JOIN tasks t ON t.id = c.task_id
		WHERE c.kind = $1 AND c.status IN `+caseLiveStatuses+` AND t.status = ANY($2) AND t.kind <> $3
		GROUP BY c.task_id ORDER BY c.task_id`,
		[]any{CaseMonitorFlag, FinalStatuses, KindChat}, func(row rowScanner) (id string, err error) {
			return id, row.Scan(&id)
		})
}

// monitorFlagPrefix starts the reason and the problem of a monitor flag's case.
const monitorFlagPrefix = "Monitor flag: "

// MonitorFlagReason is the reason of flag f's monitor case.
func MonitorFlagReason(f DigestFlag) string {
	return monitorFlagPrefix + flagsText([]DigestFlag{f})
}
