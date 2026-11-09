package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// expectedBudget is the budget card t holds when nothing moved it but its
// events: its filing's budget plus every card_funded and card_extended
// amount. The filing's budget is the one its card_created row records.
// A card filed before the rows recorded it starts from the budget its
// size gate's row found, and counts the rows from that one on, else from
// the policy's (policy.CardBudget, as intake and the planner file a card).
func (r *Runner) expectedBudget(ctx context.Context, t Task, events []Event) (float64, error) {
	var eur float64
	filed := false
	if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventCardCreated }); i >= 0 {
		eur, filed = events[i].Detail["budget_eur"].(float64)
	}
	if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventCardFunded && e.Detail["by"] == sizeGateBy }); !filed && i >= 0 {
		if from, ok := events[i].Detail["from_eur"].(float64); ok {
			eur, filed, events = from, true, events[i:]
		}
	}
	if !filed {
		p, _, err := r.store.ProjectByRepository(ctx, t.Repository)
		if err != nil {
			return 0, err
		}
		_, asked := askedBudget(t.Objective)
		eur = policy.CardBudget(r.cfg.Policy().Document.Budget, p.Budget, t.Size, t.Rung == RungFrontier, asked)
	}
	for _, e := range events {
		if e.Kind == EventCardFunded || e.Kind == EventCardExtended {
			amount, _ := e.Detail["amount_eur"].(float64)
			eur += amount
		}
	}
	return eur, nil
}

// copiesBudget reports whether card t took its budget from another card:
// a successor, or a retry's replacement, whose funding rows stay with the
// card it replaces.
func copiesBudget(t Task, events []Event) bool {
	return t.Source == SourceSuccessor || slices.ContainsFunc(events, func(e Event) bool {
		replaces, _ := e.Detail["replaces"].(string)
		return e.Kind == EventCardCreated && replaces != ""
	})
}

// ownedWait is a wait the AI owns: its kind, a policy.Waits key, the rung
// that owns it, and since when.
type ownedWait struct {
	Kind  string
	Rung  int
	Since time.Time
}

// ownedWait is the card's one wait the AI owns at d.at, the first row
// that holds. A failed merge waits for its retry in the merge gate, so
// its row comes before the gate's. A provider backoff ages from the time
// its retry is due. A run is quiet since it last moved, by a timeline
// line or a status move, or answered, by a model call: a session writes
// no line between its check-ins. A wait at the human, a step at rung 3
// among them, has no row.
func (d CardDigest) ownedWait() (ownedWait, bool) {
	h := d.Head
	quiet := h.StatusSince
	for _, at := range []time.Time{d.LastStepAt, d.lastCallAt} {
		if at.After(quiet) {
			quiet = at
		}
	}
	switch {
	case h.Status == "queued" && h.QueueReason != "" && h.QueueSince != nil:
		return ownedWait{"queued", 0, *h.QueueSince}, true
	case h.Status == "waiting-deps":
		return ownedWait{"dependency", 0, h.StatusSince}, true
	case h.Status == "in-review" && d.lastStep == "merge_failed":
		return ownedWait{"merge_retry", 0, d.LastStepAt}, true
	case h.Status == "in-review" && h.Gate != GateApproval:
		return ownedWait{"gate", 0, cmp.Or(h.Since, h.StatusSince)}, true
	case h.Status == "running" && d.providerRefused():
		return ownedWait{"provider_backoff", 0, cmp.Or(d.retryDue, d.LastStepAt)}, true
	case h.Status == "running" && d.at.Sub(quiet) > d.Waits.RunQuiet:
		return ownedWait{"run_quiet", 0, quiet}, true
	}
	var oldest *CaseStep
	for i, s := range d.caseSteps {
		if s.Rung < 3 && s.Outcome == StepInProgress && s.DueAt != nil && d.at.After(*s.DueAt) && (oldest == nil || s.StartedAt.Before(oldest.StartedAt)) {
			oldest = &d.caseSteps[i]
		}
	}
	if oldest == nil {
		return ownedWait{}, false
	}
	return ownedWait{"rung_answer", oldest.Rung, oldest.StartedAt}, true
}

// providerRefused: the card's newest session ended on a provider refusal,
// by its session_end's outcome.
func (d CardDigest) providerRefused() bool {
	for i := len(d.Steps) - 1; i >= 0; i-- {
		if d.Steps[i].Kind == "session" {
			return d.Steps[i].state == string(OutcomeProviderRefused)
		}
	}
	return false
}

// digestFlags are the patterns the card's life d shows, by name: one pass
// over its steps, then t's cases, its row, its dependencies, its owned
// wait, its events oldest first, and a plan's proofs, all read from
// rows. Unproven are the plan's criteria no verdict row holds.
func digestFlags(d CardDigest, rows cardRows, budget float64, unproven []string) []DigestFlag {
	t, cases, deps := rows.task, rows.cases, rows.deps
	events, now := rows.events, d.at
	var flags []DigestFlag
	add := func(name, detail string) { flags = append(flags, DigestFlag{name, detail}) }
	steps := d.Steps
	confirmed := map[string]int{}
	review := ""
	// a grant is open until a session pushes, or, on a plan, the planner's
	// session files a revision; afterNoPush names the first session after
	// it that ended without a push
	granted, afterNoPush := false, ""
	run, runFirst := 0, ""
	for _, s := range steps {
		switch s.Kind {
		case "proofs":
			if s.Actor != FactoryDecider {
				continue
			}
			for _, id := range strings.Fields(s.state) {
				add("prover_refused_thrice", id)
			}
		case "answer", "granted", "funded", "extended":
			if granted && afterNoPush != "" {
				add("grant_bought_nothing", afterNoPush)
			}
			granted, afterNoPush = true, ""
		case "revision":
			// the planner's session that filed a revision bought it: the
			// grant closes on the revision, as on a push
			granted, afterNoPush = false, ""
		case "session":
			if s.state == "" {
				continue
			}
			attempt, _, _ := strings.Cut(s.Actor, " ")
			name := "session " + attempt
			if s.state == string(OutcomePushed) {
				granted = false
			} else if granted && afterNoPush == "" {
				afterNoPush = name
			}
			if s.state != string(OutcomeNoPush) {
				run = 0
				continue
			}
			if run++; run == 1 {
				runFirst = name
			}
			if run == d.Waits.NoPushSessions {
				add("session_no_push", runFirst)
			}
		case "review":
			if s.state != "" {
				review = s.state
			}
		case "checkin":
			if s.state == "" {
				add("checkin_cut", offsetText(s.Offset))
			}
			for _, id := range s.Findings {
				if confirmed[id]++; confirmed[id] == 2 {
					add("finding_confirmed_twice", id)
				}
			}
		}
	}
	if granted && afterNoPush != "" {
		add("grant_bought_nothing", afterNoPush)
	}
	redAt := map[string]map[string]bool{}
	newest := -1
	for i, s := range steps {
		if s.Kind != "proofs" || len(s.gate) == 0 {
			continue
		}
		newest = i
		for _, p := range s.gate {
			if p.State != proofOpen || p.Met {
				continue
			}
			if redAt[p.ID] == nil {
				redAt[p.ID] = map[string]bool{}
			}
			redAt[p.ID][s.commit] = true
		}
	}
	for id, heads := range redAt {
		if len(heads) > 1 {
			add("opened_proof_red_twice", id)
		}
	}
	if newest >= 0 {
		for _, p := range steps[newest].gate {
			if p.Changed {
				add("opened_proof_changed", p.ID+" "+p.Path)
			}
		}
	}
	for _, id := range unproven {
		add("criterion_unproven", id)
	}
	for _, c := range cases {
		if c.Status != CaseOpen && c.Status != CaseAtHuman {
			continue
		}
		if c.Money != nil && review == "concerns" {
			add("money_ask_hides_findings", "")
		}
		if slices.ContainsFunc(cases, func(o Case) bool { return o.ID < c.ID && o.Status == CaseResolved && o.StateKey == c.StateKey }) {
			add("answer_changed_nothing", c.StateKey)
		}
		if Final(t.Status) {
			add("issue_open_after_end", string(c.Kind))
		}
	}
	for _, dep := range deps {
		if dep.Status == "failed" {
			add("dependency_failed", fmt.Sprintf("#%d", dep.Issue))
		}
	}
	switch since := now.Sub(rows.proverDue); {
	case rows.proofsStored > 0 && rows.proofsStored < rows.proofsWanted:
		add("proofs_missing", fmt.Sprintf("%d of %d wanted proofs without a store", rows.proofsWanted-rows.proofsStored, rows.proofsWanted))
	case rows.proofsStored == 0 && !rows.proverDue.IsZero() && since > time.Hour:
		add("proofs_missing", textutil.Plural(int(since/time.Hour), "hour", "hours"))
	}
	if defaultKind(t.Kind) == "agent" && Cents(t.BudgetEUR) != Cents(budget) {
		add("budget_moved_no_event", fmt.Sprintf("%.2f EUR not %.2f EUR", t.BudgetEUR, budget))
	}
	// a rung's step is past its bound once its wait holds: it carries its
	// own due_at
	if w, ok := d.ownedWait(); ok && (w.Kind == "rung_answer" || d.at.Sub(w.Since) > d.Waits.Bound(w.Kind)) {
		since := " since " + offsetText(w.Since.Sub(steps[0].At))
		add("wait_over_bound", w.Kind+since)
		// the queue's wait starts at the row's queue_since, often the
		// filing itself, so its offset from the first step reads +0:00:
		// the detail names the wait's length instead, as the board's
		// queue does
		if w.Kind == "queued" {
			add("queued_over_bound", d.Head.QueueReason+" for "+durationText(d.at.Sub(w.Since).Truncate(time.Minute)))
		}
	}
	// the newest review and audit rows on the head, the head a session's
	// newest push: the queue's merge of main moves the commit and runs
	// neither again
	var reviewRow, auditRow *Event
	for i, e := range events {
		switch e.Kind {
		case EventSessionPushed:
			reviewRow, auditRow = nil, nil
		case EventModelReview:
			reviewRow = &events[i]
		case EventGateAudit:
			auditRow = &events[i]
		}
	}
	for _, row := range []struct {
		stage, key string
		e          *Event
	}{{"review", "reviewed", reviewRow}, {"audit", "ran", auditRow}} {
		if row.e == nil || t.Status != "in-review" {
			continue
		}
		if ran, _ := row.e.Detail[row.key].(bool); !ran {
			reason, _ := row.e.Detail["reason"].(string)
			add("review_refused_not_run", row.stage+": "+digestText(strings.TrimPrefix(reason, reviewNotStarted)))
		}
	}
	slices.SortFunc(flags, func(a, b DigestFlag) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Detail, b.Detail))
	})
	return slices.Compact(flags)
}
