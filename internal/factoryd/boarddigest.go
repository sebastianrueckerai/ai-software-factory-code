package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// BoardDigest is a project's board as a few lines: its live cards with
// their flags, the queue with what holds each card, the work slots, the
// open plans by their cards' statuses, and the caps.
type BoardDigest struct {
	Cards []BoardDigestCard   // live cards: not done, failed or closed, and not in the queue
	Queue []BoardDigestQueued // queued cards no slot took yet, in filing order
	Slots BoardDigestSlots
	Plans []BoardDigestPlan
	Caps  []BoardDigestCap
}

// BoardDigestCard is a live card: its status and gate, the time since its
// newest status event, whether it holds a work slot, and its digest's
// flags.
type BoardDigestCard struct {
	Issue        int
	Status, Gate string
	Age          time.Duration
	SlotHeld     bool
	Flags        []DigestFlag
}

// BoardDigestQueued is a queued card, by issue and task id (the one name
// a card without an issue has), and what holds it: "no issue", "waits on
// #N", "project month cap", "work bound H of B", or "next".
type BoardDigestQueued struct {
	Issue  int
	TaskID string
	Reason string
}

// BoardDigestSlots are the work slots the project's cards hold, and its
// work_in_progress.
type BoardDigestSlots struct{ Held, Bound int }

// BoardDigestPlan is an open plan: its issue, its name and its cards by
// status.
type BoardDigestPlan struct {
	Issue    int
	Name     string
	Children map[string]int
}

// BoardDigestCap is a cap by name, the spend it counts and its size.
type BoardDigestCap struct {
	Name             string
	SpentEUR, CapEUR float64
}

// BoardDigest builds repository's board digest at the runner's clock: one
// CardDigest per live card out of the queue, for its flags, and admit's
// checks for each queued card.
func (r *Runner) BoardDigest(ctx context.Context, repository string) (BoardDigest, error) {
	now, v := r.clock(), r.cfg.Policy()
	live, err := r.store.BoardTasks(ctx, repository)
	if err != nil {
		return BoardDigest{}, err
	}
	slices.SortFunc(live, func(a, b BoardTask) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.IssueNumber, b.IssueNumber), strings.Compare(a.ID, b.ID))
	})
	held, err := r.store.CountAdmitted(ctx, repository)
	if err != nil {
		return BoardDigest{}, err
	}
	b := BoardDigest{Slots: BoardDigestSlots{Held: held, Bound: v.WorkInProgressOf(repository)}}
	for _, t := range live {
		if !waitsForSlot(t.Task) {
			card, err := r.boardCard(ctx, t.ID, now)
			if err != nil {
				return BoardDigest{}, err
			}
			b.Cards = append(b.Cards, card)
			continue
		}
		why, err := admissionWait(ctx, r.store, v.Tree, now, t.Task)
		if err != nil {
			return BoardDigest{}, err
		}
		if why == "" {
			why = "next"
			if held >= b.Slots.Bound {
				why = fmt.Sprintf("work bound %d of %d", held, b.Slots.Bound)
			}
		}
		b.Queue = append(b.Queue, BoardDigestQueued{Issue: int(t.IssueNumber), TaskID: t.ID, Reason: why})
	}
	if b.Plans, err = r.boardPlans(ctx, repository); err != nil {
		return BoardDigest{}, err
	}
	e, err := r.store.Enforced(ctx, now)
	if err != nil {
		return BoardDigest{}, err
	}
	b.Caps = []BoardDigestCap{{"month", e.Month, v.Document.Budget.MonthEUR}, {"day", e.Day, v.Document.Budget.DayEUR}}
	if _, cap := v.ProjectCap(repository); !math.IsInf(cap, 1) {
		spent, err := r.store.ProjectMonthSpend(ctx, repository, now)
		if err != nil {
			return BoardDigest{}, err
		}
		b.Caps = append(b.Caps, BoardDigestCap{"project", spent.EUR, cap})
	}
	return b, nil
}

// boardCard is live card taskID's line, its age the time from its newest
// task_status event, or its filing, to now.
func (r *Runner) boardCard(ctx context.Context, taskID string, now time.Time) (BoardDigestCard, error) {
	rows, err := r.readCard(ctx, taskID)
	if err != nil {
		return BoardDigestCard{}, err
	}
	d, err := r.cardDigest(ctx, rows)
	if err != nil {
		return BoardDigestCard{}, err
	}
	moved := rows.task.CreatedAt
	for _, e := range rows.events {
		if e.Kind == EventTaskStatus {
			moved = e.At
		}
	}
	return BoardDigestCard{Issue: d.Head.Issue, Status: d.Head.Status, Gate: d.Head.Gate, Age: max(now.Sub(moved), 0),
		SlotHeld: d.Head.SlotHeld, Flags: d.Flags}, nil
}

// boardPlans are repository's open plans, each with its cards counted by
// status.
func (r *Runner) boardPlans(ctx context.Context, repository string) ([]BoardDigestPlan, error) {
	plans, err := r.store.OpenPlans(ctx, repository)
	if err != nil {
		return nil, err
	}
	out := make([]BoardDigestPlan, len(plans))
	for i, p := range plans {
		cards, err := r.store.PlanCards(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		out[i] = BoardDigestPlan{Issue: int(p.IssueNumber), Name: p.PlanName, Children: map[string]int{}}
		for _, c := range cards {
			out[i].Children[c.Status]++
		}
	}
	return out, nil
}

// planStatusOrder is the order a plan's cards are counted in: done, then
// the live statuses in the reverse of their order in Statuses, then
// failed and closed.
var planStatusOrder = func() []string {
	live := slices.DeleteFunc(slices.Clone(Statuses), Final)
	slices.Reverse(live)
	return slices.Concat([]string{"done"}, live, []string{"failed", "closed"})
}()

// Render is the board digest as text: a head line, one line per live
// card, then the queue, the open plans and the caps.
func (b BoardDigest) Render() string {
	var s strings.Builder
	fmt.Fprintf(&s, "cards %d live · slots %d of %d · queue %d\n", len(b.Cards)+len(b.Queue), b.Slots.Held, b.Slots.Bound, len(b.Queue))
	for _, c := range b.Cards {
		slot, flags := "", ""
		if c.SlotHeld {
			slot = "slot"
		}
		if len(c.Flags) > 0 {
			flags = "flags " + flagsText(c.Flags)
		}
		age := fmt.Sprintf("%dh%02dm", int(c.Age/time.Hour), int(c.Age%time.Hour/time.Minute))
		line := fmt.Sprintf("%-6s %-21s %-7s  %-4s  %s", fmt.Sprintf("#%d", c.Issue), statusText(c.Status, c.Gate), age, slot, flags)
		s.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	if len(b.Queue) > 0 {
		s.WriteString("queue:\n")
	}
	for _, q := range b.Queue {
		card := fmt.Sprintf("#%d", q.Issue)
		if q.Issue == 0 {
			card += " " + q.TaskID
		}
		fmt.Fprintf(&s, "%-5s  %s\n", card, q.Reason)
	}
	if len(b.Plans) > 0 {
		s.WriteString("plans:\n")
	}
	for _, p := range b.Plans {
		var counts []string
		for _, status := range planStatusOrder {
			if n := p.Children[status]; n > 0 {
				counts = append(counts, fmt.Sprintf("%s %d", status, n))
			}
		}
		fmt.Fprintf(&s, "%s  %s\n", strings.TrimSpace(fmt.Sprintf("#%d %s", p.Issue, p.Name)), cmp.Or(strings.Join(counts, " · "), "no cards"))
	}
	caps := make([]string, len(b.Caps))
	for i, c := range b.Caps {
		caps[i] = fmt.Sprintf("%s %.2f of %.2f EUR", c.Name, c.SpentEUR, c.CapEUR)
	}
	s.WriteString("caps:\n" + strings.Join(caps, " · ") + "\n")
	return s.String()
}
