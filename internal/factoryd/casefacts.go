package factoryd

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// CaseFacts is what the ladder knows about a case when it decides: the
// state of the work, the money, the open findings, the dependencies and
// what the pool still holds. Stored as the case's facts.
type CaseFacts struct {
	Head           string `json:"head,omitempty"`
	PR             int64  `json:"pr,omitempty"`
	PROpen         bool   `json:"pr_open,omitempty"`         // PR is set and the card neither merged nor ended
	Checks         string `json:"checks,omitempty"`          // green, red, pending or ""
	ReviewApproved bool   `json:"review_approved,omitempty"` // the newest model_review on Head was reviewed and approved
	RerunOnHead    bool   `json:"rerun_on_head,omitempty"`   // a case on Head was resolved by running its checks again
	Stage          string `json:"stage,omitempty"`           // the gate the card is in
	Attempts       int    `json:"attempts"`
	SessionCap     int    `json:"session_cap"`
	AgentTimeS     int64  `json:"agent_time_s"`
	AgentTimeCapS  int64  `json:"agent_time_cap_s"`
	// Model is the model the next session runs on, "" when none is known
	// yet: the class's session_model, else the card's last session's.
	Model      string   `json:"model,omitempty"`
	BudgetEUR  float64  `json:"budget_eur"`
	SpentEUR   float64  `json:"spent_eur"`          // Task.EnforcedSpent()
	LeftEUR    float64  `json:"left_eur"`           // Task.Left()
	FloorEUR   float64  `json:"floor_eur"`          // what the next opening needs, as the opening's floor reads it (needOf)
	CeilingEUR float64  `json:"ceiling_eur"`        // the card's own, else CardCeiling
	Findings   []string `json:"findings,omitempty"` // the finding ids (findingID) Head's newest checks, review and audit raised
	// LastDrawAt is the time of the card's newest draw from the pool when
	// its row recorded the head and the checks then: DrawHead and
	// DrawChecks. A draw from before rows recorded them leaves all three
	// empty, and DrawFindings too.
	LastDrawAt *time.Time `json:"last_draw_at,omitempty"`
	DrawHead   string     `json:"draw_head,omitempty"`
	DrawChecks string     `json:"draw_checks,omitempty"`
	// DrawFindings are the finding ids the newest draw's row recorded,
	// nil when it recorded none, which is not the empty list.
	DrawFindings    []string    `json:"draw_findings"`
	PushedSinceDraw bool        `json:"pushed_since_draw,omitempty"`
	Pushed          bool        `json:"pushed,omitempty"` // a session pushed a branch head
	Dependencies    []DepFact   `json:"dependencies,omitempty"`
	Plan            string      `json:"plan,omitempty"`    // the plan's task id, for a plan or a plan's card
	Running         bool        `json:"running,omitempty"` // a session is live
	Autonomy        string      `json:"autonomy"`
	PoolLeft        policy.Draw `json:"pool_left"`
	CardDrawLeft    policy.Draw `json:"card_draw_left"`
	Partial         []string    `json:"partial,omitempty"` // the readers that failed, by name
}

// DepFact is one dependency of the card that is not done.
type DepFact struct {
	TaskID   string `json:"task_id"`
	Issue    int64  `json:"issue"`
	Status   string `json:"status"`
	Why      string `json:"why,omitempty"`      // the end reason of an ended one
	Replaced string `json:"replaced,omitempty"` // the replacement's task id, if retried
}

// tripFacts reads every fact of card t it can and names the readers that
// failed in Partial, their facts left zero. It fails only when ctx ends,
// since facts read past that are no facts.
func (r *Runner) tripFacts(ctx context.Context, t Task) (CaseFacts, error) {
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	f := CaseFacts{PR: t.PRNumber, PROpen: t.PRNumber > 0 && t.Status != "merged" && !Final(t.Status), Stage: t.Gate,
		Attempts: t.Usage.Attempts, SessionCap: SessionCap(t, v.Document.Limits.Sessions) + t.Usage.SessionsAdded(),
		AgentTimeS: int64(t.Usage.AgentTime / time.Second), Model: sessionModel(v.Document, t, t.ModelAlias),
		BudgetEUR: t.BudgetEUR, SpentEUR: t.EnforcedSpent(), LeftEUR: t.Left(), CeilingEUR: cmp.Or(t.CeilingEUR, CardCeiling(v.Document, project)),
		Running: t.Status == "running", Autonomy: project.AutonomyState()}
	if InPlan(t) {
		f.Plan = GroupOf(t)
	}
	esc := v.EscalationOf(t.Repository)
	// the readers in order, each after the head the events name
	readers := []struct {
		name string
		read func() error
	}{
		{"events", func() error {
			events, err := r.store.TaskEvents(ctx, t.ID, cardDigestEvents)
			if err == nil {
				f.eventFacts(events)
			}
			return err
		}},
		{"timeline", func() error {
			tl, err := r.objects.Get(ctx, "sessions/"+t.ID+"/timeline.jsonl")
			if err == nil {
				lines, _ := lineSteps(tl)
				f.Findings = headFindings(lines, f.Head)
			}
			return err
		}},
		{"checks", func() error {
			if f.Head == "" {
				return nil
			}
			res, err := r.HeadChecks(ctx, t.ID, f.Head)
			if err == nil {
				f.Checks = checksWord(res.State)
			}
			return err
		}},
		{"config", func() error {
			tc, err := r.TaskConfig(ctx, t.ID, 0)
			if err == nil {
				f.AgentTimeCapS = int64((policy.SizeAgentTime(t.Size, tc.AgentTime) + t.Usage.TimeAdded()) / time.Second)
			}
			return err
		}},
		{"floor", func() error {
			if f.Model == "" || r.cfg.Prices == nil {
				f.FloorEUR = v.Document.Budget.SessionFloor(f.Model != "" && r.frontier(f.Model))
				return nil
			}
			n, err := r.needOf(ctx, v, t, f.Model, true)
			if err == nil && n.unread != "" {
				err = errors.New(n.unread)
			}
			f.FloorEUR = n.eur
			return err
		}},
		{"dependencies", func() (err error) {
			f.Dependencies, err = r.depFacts(ctx, t.ID)
			return err
		}},
		{"cases", func() error {
			cases, err := r.store.CasesOf(ctx, t.ID)
			f.RerunOnHead = err == nil && f.Head != "" && slices.ContainsFunc(cases, func(c Case) bool {
				return c.Resolution != nil && c.Resolution.Action == ActRerunChecks && c.Facts["head"] == f.Head
			})
			return err
		}},
		{"pool", func() error {
			drawn, err := r.store.PoolDrawn(ctx, t.Repository, r.clock())
			if err == nil {
				f.PoolLeft = esc.Pool.Minus(drawn)
			}
			return err
		}},
		{"card draw", func() error {
			drawn, err := r.store.CardDrawn(ctx, t.ID)
			if err == nil {
				f.CardDrawLeft = esc.CardDrawOf(t.Size).Minus(drawn)
			}
			return err
		}},
	}
	for _, rd := range readers {
		if err := ctx.Err(); err != nil {
			return f, err
		}
		if rd.read() != nil {
			f.Partial = append(f.Partial, rd.name)
		}
	}
	return f, ctx.Err()
}

// eventFacts sets f's head, its review, whether a session pushed, and its
// last draw from the card's events, newest first: the head of the newest
// gate_head row, the newest model_review of that head, and the newest
// draw from the pool, when its row recorded the head and the checks, with
// the findings it recorded and whether a session pushed after it.
func (f *CaseFacts) eventFacts(events []Event) {
	word := func(e Event, k string) string { s, _ := e.Detail[k].(string); return s }
	pushed := func(e Event) bool { return e.Kind == EventSessionPushed }
	f.Pushed = slices.ContainsFunc(events, pushed)
	if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventGateHead }); i >= 0 {
		f.Head = word(events[i], "head")
	}
	if i := slices.IndexFunc(events, func(e Event) bool {
		return e.Kind == EventModelReview && f.Head != "" && word(e, "commit") == f.Head
	}); i >= 0 {
		reviewed, _ := events[i].Detail["reviewed"].(bool)
		f.ReviewApproved = reviewed && word(events[i], "verdict") == "approve"
	}
	i := slices.IndexFunc(events, func(e Event) bool {
		return (e.Kind == EventCardFunded || e.Kind == EventCardExtended) && e.Detail["pool"] == true
	})
	if i < 0 {
		return
	}
	head, hasHead := events[i].Detail["head"].(string)
	checks, hasChecks := events[i].Detail["checks"].(string)
	if !hasHead || !hasChecks {
		return
	}
	at := events[i].At
	f.LastDrawAt, f.DrawHead, f.DrawChecks = &at, head, checks
	f.PushedSinceDraw = slices.ContainsFunc(events[:i], pushed)
	switch ids := events[i].Detail["findings"].(type) {
	case []string:
		f.DrawFindings = append([]string{}, ids...)
	case []any:
		f.DrawFindings = []string{}
		for _, id := range ids {
			if s, ok := id.(string); ok {
				f.DrawFindings = append(f.DrawFindings, s)
			}
		}
	}
	slices.Sort(f.DrawFindings)
}

// headFindings are the sorted finding ids that head's newest checks, its
// newest review and its newest audit raised, of the timeline's steps in
// line order.
func headFindings(steps []DigestStep, head string) []string {
	if head == "" {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, s := range slices.Backward(steps) {
		if s.commit != head || seen[s.Kind] || !slices.Contains([]string{"checks", "review", "audit"}, s.Kind) {
			continue
		}
		seen[s.Kind] = true
		ids = append(ids, s.Findings...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// checksWord is a head's checks state as the facts name it.
func checksWord(s ChecksState) string {
	switch s {
	case ChecksSuccess:
		return "green"
	case ChecksFailure:
		return "red"
	}
	return "pending"
}

// CardCeiling is the most the policy lets one card of project be funded
// to: task_max_eur, or the project's own when it is lower.
func CardCeiling(d policy.Document, p policy.ProjectConfig) float64 {
	if p.Budget != nil && p.Budget.TaskMaxEUR > 0 {
		return min(d.Budget.TaskMaxEUR, p.Budget.TaskMaxEUR)
	}
	return d.Budget.TaskMaxEUR
}

// depFacts are card id's dependencies that are not done: an ended one
// with the reason of its task_ended row, a failed one with its
// replacement when a retry filed one (RetryID).
func (r *Runner) depFacts(ctx context.Context, id string) ([]DepFact, error) {
	deps, err := r.store.OpenDependencies(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []DepFact
	for _, d := range deps {
		fact := DepFact{TaskID: d.TaskID, Issue: d.Issue, Status: d.Status}
		if Final(d.Status) {
			events, err := r.store.TaskEvents(ctx, d.TaskID, cardDigestEvents)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventTaskEnded }); i >= 0 {
				fact.Why, _ = events[i].Detail["reason"].(string)
			}
		}
		if d.Status == "failed" {
			_, retried, err := r.store.Task(ctx, RetryID(d.TaskID))
			if err != nil {
				return nil, err
			}
			if retried {
				fact.Replaced = RetryID(d.TaskID)
			}
		}
		out = append(out, fact)
	}
	return out, nil
}
