package console

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// run is one run of a stage in a card's life (C10): when it started, how
// long it took, what its model calls cost on a building or planning run,
// and the notes that ended or marked it.
type run struct {
	Kind    string // queued, planning, building, verifying, waiting on you
	Class   string // the stage's hue class
	Start   time.Time
	Minutes int
	EUR     float64
	Attempt int
	Model   string
	Notes   []string
}

// At is the run's start as the drawer shows it.
func (r run) At() string { return r.Start.UTC().Format("15:04") }

// Duration is the run's length in minutes, or hours and minutes.
func (r run) Duration() string { return minutes(time.Duration(r.Minutes) * time.Minute) }

// Note is the run's notes on one line.
func (r run) Note() string { return strings.Join(r.Notes, " · ") }

// Costs is whether the run shows a cost: a building or planning run.
func (r run) Costs() bool { return r.Kind == "building" || r.Kind == "planning" }

var runClasses = map[string]string{"queued": "st-closed", "planning": "st-running", "building": "st-running", "verifying": "st-review", "waiting on you": "st-waiting"}

// runKind is the stage a status puts a card in, "" for a final one.
func runKind(status, lane string) string {
	switch status {
	case "backlog", "queued", "waiting-deps", "waiting-image":
		return "queued"
	case "running", "closing":
		if lane == policy.LanePlan {
			return "planning"
		}
		return "building"
	case "in-review":
		return "verifying"
	case "waiting-human", "waiting-poster":
		return "waiting on you"
	}
	return ""
}

// point is one moment of a card's life the runs are read from: a status
// row, a session's start or end, a check, a review, a stall, a question.
type point struct {
	at    time.Time
	order int // at the same moment: a session's end first, then a status row, then the other steps, so the end notes the run that ends and the row opens the run a start fills
	kind  string
	note  string
	start bool // the point opens a run of kind
	end   bool // a session's end: note is its outcome
	calls int  // a session's attempt number
	model string
}

// runsOf is card t's life as runs of its stages, in order, from its
// status rows, its timeline's steps and its model calls: a status row
// opens the run its status names, a session's start opens a building or
// planning run (its attempt and model on the run, or on the run the
// status row just opened), the session's end, a check, a review, a
// question and its withdrawal are notes on the run they fall in, a stall
// on the provider a note inside the run (the card's state did not
// change). The last run ends at the card's end, or now.
func runsOf(t factoryd.Task, events []factoryd.Event, steps []map[string]any, calls []policy.ModelCall, now time.Time) []run {
	var pts []point
	for _, e := range events {
		switch e.Kind {
		case factoryd.EventTaskStatus:
			status, _ := e.Detail["status"].(string)
			if k := runKind(status, t.Lane); k != "" {
				pts = append(pts, point{at: e.At, order: 1, kind: k, start: true})
			}
		case factoryd.EventQuestionAsked:
			q, _ := e.Detail["question"].(string)
			pts = append(pts, point{at: e.At, order: 1, note: "asked: " + strings.TrimSpace(q)})
		case factoryd.EventQuestionWithdrawn:
			q, _ := e.Detail["question"].(string)
			why, _ := e.Detail["reason"].(string)
			pts = append(pts, point{at: e.At, order: 1, note: "withdrawn because " + why + ": " + strings.TrimSpace(q)})
		}
	}
	for _, m := range steps {
		at := stepTime(m)
		switch m["step"] {
		case "session_start":
			k := "building"
			if t.Lane == policy.LanePlan {
				k = "planning"
			}
			pts = append(pts, point{at: at, order: 2, kind: k, start: true, calls: int(numAt(m, "attempt")), model: textAt(m, "model")})
		case "session_end":
			// the end names the attempt and the outcome; the model is the run's
			pts = append(pts, point{at: at, end: true, calls: int(numAt(m, "attempt")), model: textAt(m, "model"), note: textAt(m, "outcome")})
		case "checks":
			var jobs, passed []string
			if list, ok := m["jobs"].([]any); ok {
				for _, j := range list {
					job, _ := j.(map[string]any)
					jobs = append(jobs, textAt(job, "name"))
					if textAt(job, "status") == "success" {
						passed = append(passed, textAt(job, "name"))
					}
				}
			}
			note := "checks " + textAt(m, "state")
			if len(jobs) > 0 {
				note = fmt.Sprintf("checks %d/%d", len(passed), len(jobs))
				for _, j := range jobs {
					if !slices.Contains(passed, j) {
						note += ", " + j + " failed"
					}
				}
			}
			pts = append(pts, point{at: at, order: 2, note: note})
		case "model_review":
			pts = append(pts, point{at: at, order: 2, note: "review by factoryd: " + cmp.Or(textAt(m, "verdict"), textAt(m, "outcome"))})
		case factoryd.StepProviderRefused:
			pts = append(pts, point{at: at, order: 2, note: fmt.Sprintf("stalled on the provider, %d", int(numAt(m, "status")))})
		case "held":
			pts = append(pts, point{at: at, order: 2, note: "held for a roll"})
		}
	}
	sort.SliceStable(pts, func(i, j int) bool {
		if !pts[i].at.Equal(pts[j].at) {
			return pts[i].at.Before(pts[j].at)
		}
		return pts[i].order < pts[j].order
	})
	var runs []run
	cur := func() *run {
		if len(runs) == 0 {
			return nil
		}
		return &runs[len(runs)-1]
	}
	open := func(kind string, at time.Time) *run {
		runs = append(runs, run{Kind: kind, Class: runClasses[kind], Start: at})
		return cur()
	}
	if !t.CreatedAt.IsZero() {
		open("queued", t.CreatedAt)
	}
	for _, p := range pts {
		r := cur()
		switch {
		case p.start && p.model != "":
			// a session's start: the run the status row just opened takes it
			if r == nil || r.Kind != p.kind || r.Attempt != 0 {
				r = open(p.kind, p.at)
			}
			r.Attempt, r.Model = p.calls, p.model
		case p.start:
			// a status row opens a run only where the stage changes: a new
			// attempt in the same stage is the session's start to open
			if r == nil || r.Kind != p.kind {
				open(p.kind, p.at)
			}
		case p.end:
			if r == nil {
				r = open("building", p.at)
			}
			note := fmt.Sprintf("attempt %d, %s, %s", cmp.Or(p.calls, r.Attempt), cmp.Or(p.model, r.Model), p.note)
			if p.note == "retry_stronger" {
				note += fmt.Sprintf(", stronger after %d", cmp.Or(p.calls, r.Attempt))
			}
			r.Notes = append(r.Notes, note)
		case p.note != "":
			if r == nil {
				r = open("queued", p.at)
			}
			r.Notes = append(r.Notes, p.note)
		}
	}
	end := now
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	for i := range runs {
		next := end
		if i+1 < len(runs) {
			next = runs[i+1].Start
		}
		runs[i].Minutes = int(math.Round(next.Sub(runs[i].Start).Minutes()))
		if runs[i].Costs() {
			for _, c := range calls {
				if !c.At.Before(runs[i].Start) && c.At.Before(next) {
					runs[i].EUR += c.EUR
				}
			}
		}
	}
	return runs
}

// lifeLine is the Time section's at-a-glance line: the factory's time
// (planning, building and verifying) in n attempts, the waiting time
// (queued and waiting on you), the spend; a part that is zero is left out.
func lifeLine(runs []run, spent float64) string {
	var factory, waiting, attempts int
	for _, r := range runs {
		switch r.Kind {
		case "building", "planning":
			factory += r.Minutes
			attempts++
		case "verifying":
			factory += r.Minutes
		default:
			waiting += r.Minutes
		}
	}
	var parts []string
	if attempts > 0 {
		s := "s"
		if attempts == 1 {
			s = ""
		}
		parts = append(parts, fmt.Sprintf("%s factory in %d attempt%s", minutes(time.Duration(factory)*time.Minute), attempts, s))
	}
	if waiting > 0 {
		parts = append(parts, minutes(time.Duration(waiting)*time.Minute)+" waiting")
	}
	if spent > 0 {
		parts = append(parts, fmt.Sprintf("\u20ac%.2f", spent))
	}
	return strings.Join(parts, " · ")
}

// lifeOf reads card t's runs: its events, its timeline and its model
// calls; a read that fails leaves its part out and is logged.
func (c *Console) lifeOf(ctx context.Context, t factoryd.Task) []run {
	events, err := c.cfg.Events.TaskEvents(ctx, t.ID, eventLimitMax)
	if err != nil {
		log.Printf("console: the events of %s: %v", t.ID, err)
	}
	steps, err := c.readTimeline(ctx, t.ID)
	if err != nil {
		log.Printf("console: the timeline of %s: %v", t.ID, err)
	}
	var calls []policy.ModelCall
	if c.cfg.Costs != nil {
		if calls, err = c.cfg.Costs.ModelCalls(ctx, t.ID); err != nil {
			log.Printf("console: the model calls of %s: %v", t.ID, err)
		}
	}
	return runsOf(t, events, steps, calls, time.Now())
}
