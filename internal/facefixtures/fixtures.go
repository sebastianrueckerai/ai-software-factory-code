// Package facefixtures holds one card of every kind the board shows, as the
// store holds it now: every card source in each column it stands in, every
// case kind, approval kind, monitor flag and human action, and every card
// kind of the board-cards census. Every text the factory writes comes from
// the function that writes it; the fixtures supply what those functions
// take: the issues, the agents' and the planner's words, the findings, the
// logs. Only #2004, #2091, #2100, #2101 and #1850 keep the texts the board
// read on 2026-11-16. The console's gallery seats each one; tests of
// factoryd and of the console range over All.
package facefixtures

import (
	"cmp"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Fixture is one card of the gallery: its group and its kind in that
// group, the card, the case it holds at the human (nil for none) and the
// facts the action predicates weigh for it. Ask is the case's newest
// question_asked row, nil for a case no ask put there; Approval is the
// pending request the card waits on and Plan a plan card's newest
// revision, nil for none. Brief is the facts a Go-filed card's brief is
// written from (factoryd.SourceBriefs). Parent is a helper task's parent,
// the card the board shows the helper on; nil for any other card. Human is
// the ask a human card is filed with (CreateHumanCard), nil for any other
// card.
type Fixture struct {
	Group, Kind string
	Task        factoryd.Task
	Case        *factoryd.Case
	Facts       factoryd.CaseFacts
	Ask         *factoryd.CaseAsk
	Approval    *factoryd.ApprovalRequest
	Plan        *factoryd.Plan
	Brief       factoryd.BriefFacts
	Parent      *factoryd.Task
	Human       *factoryd.HumanAsk
}

// The groups of All. A source's Kind is the source and the column it
// stands in, "<source>/<column>"; a card's Kind the census's name of it;
// every other group's Kind the kind's own name.
const (
	GroupSource   = "source"
	GroupCase     = "case"
	GroupApproval = "approval"
	GroupFlag     = "flag"
	GroupAction   = "action"
	GroupCard     = "card"
	// GroupVariant holds a kind's face in a state its case fixture does
	// not hold, its Kind "<kind> <the state>".
	GroupVariant = "variant"
)

// Repository is the repository of every fixture's card: the code project
// the console's rig shows. Project is the project's name in the texts.
const (
	Repository = "sf/code"
	Project    = "code"
)

// PlanSHA is the hash of every fixture's plan revision, which a plan's
// approval request names.
const PlanSHA = "9f1c2be07d4a6c31e8b5f0a2d9c47e1186b3fa0c25d7e94b1a60c8f3d2e5b7a9"

// Columns are the board's columns as a source's Kind names them, with the
// status that puts a card there.
var Columns = []struct{ Name, Status string }{
	{"needs-you", "waiting-human"}, {"queued", "queued"}, {"in-progress", "running"}, {"done", "done"},
}

//go:embed live
var live embed.FS

// liveText is the text of live card n as the board read it on 2026-11-16.
func liveText(n int64) string {
	b, err := live.ReadFile(fmt.Sprintf("live/%d.md", n))
	if err != nil {
		panic(err) // embedded at build
	}
	return string(b)
}

// All is every fixture, built against the clock now.
func All() []Fixture {
	b := &builder{now: time.Now(), seq: 2200}
	b.plan1947()
	b.plan1990()
	b.sources()
	b.caseKinds()
	b.approvals()
	b.flags()
	b.actions()
	b.cards()
	b.variants()
	return b.out
}

type builder struct {
	now time.Time
	seq int64
	out []Fixture
}

func (b *builder) next() int64 {
	b.seq++
	return b.seq
}

func (b *builder) ago(d time.Duration) *time.Time {
	at := b.now.Add(-d)
	return &at
}

// TaskID is the task id of issue n's fixture card.
func TaskID(n int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "face %d", n))
	return "t-" + hex.EncodeToString(sum[:6])
}

// same is a fixture's mask: its texts hold no secret and no outside link.
func same(s string) string { return s }

// card is the agent card of issue n in status with objective, filed age
// ago by source: a queued card waits for a work slot, a running one is in
// its session on big, a waiting one stopped after its first session, and
// an ended one ended an hour ago.
func (b *builder) card(n int64, status, source string, age time.Duration, objective string) factoryd.Task {
	t := factoryd.Task{ID: TaskID(n), Kind: "agent", Status: status, Track: policy.TrackDirect, Process: "spec", Repository: Repository,
		IssueNumber: n, Objective: objective, Source: source, Author: "sebastian", AuthorWriter: true, RiskClass: "standard",
		BudgetEUR: 2, CreatedAt: b.now.Add(-age), RequiredCapabilities: []string{"coder"}, Rung: factoryd.RungCheap, Size: "small"}
	switch status {
	case "queued":
		t.Gate, t.GateSince, t.QueueReason, t.QueueSince = factoryd.GateIntake, b.ago(age), factoryd.WorkBoundReason+"4 of 4", b.ago(age-time.Minute)
	case "running":
		t.Gate, t.GateSince, t.AdmittedAt, t.ModelAlias, t.SpentEUR = factoryd.GateSession, b.ago(18*time.Minute), b.ago(20*time.Minute), "big", 0.74
		t.Usage.Attempts = 1
	case "in-review":
		t.Gate, t.GateSince, t.ModelAlias, t.SpentEUR, t.PRNumber = factoryd.GateApproval, b.ago(25*time.Minute), "big", 1.31, n+40
		t.Usage.Attempts = 1
	case "waiting-human", "waiting-poster":
		t.ModelAlias, t.SpentEUR, t.PRNumber = "big", 1.12, n+40
		t.Usage.Attempts = 1
	case "done", "failed", "closed":
		t.FinishedAt, t.ModelAlias, t.SpentEUR = b.ago(time.Hour), "big", 0.96
		t.Usage.Attempts = 1
		if status == "done" {
			t.PRNumber, t.MergeCommit = n+40, "5e1f0a2c9d3b7e41"
		}
	}
	return t
}

// briefed is card's card of issue n filed with brief br: its objective
// the brief's title and text, its why line and summary the brief's.
func (b *builder) briefed(n int64, status, source string, age time.Duration, br factoryd.BriefText) factoryd.Task {
	t := b.card(n, status, source, age, factoryd.IssueObjective(br.Title, br.Body))
	t.Why, t.Summary = br.Why, br.Summary
	return t
}

// issue is the card of issue n a human filed with title and body.
func (b *builder) issue(n int64, status string, age time.Duration, title, body string) factoryd.Task {
	return b.card(n, status, factoryd.SourceWebhook, age, factoryd.IssueObjective(title, body))
}

// facts are the facts of card t at a case of kind: room for a session, a
// pull request open while the card has one, money left, and the pool and
// the card's draw bound with room.
func facts(t factoryd.Task, kind factoryd.CaseKind) factoryd.CaseFacts {
	f := factoryd.CaseFacts{Kind: kind, Attempts: t.Usage.Attempts, SessionCap: 3, AgentTimeS: 1500, AgentTimeCapS: 7200, SessionLeastS: 300,
		Model: "big", BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR, LeftEUR: t.BudgetEUR - t.SpentEUR, FloorEUR: 0.3, CeilingEUR: 18,
		Autonomy: policy.AutonomyManual, Stage: t.Gate, Running: t.Status == "running", PosterGrant: t.Usage.PosterAttempts < factoryd.MaxPosterAttempts,
		DayCapEUR: 40, MonthCapEUR: 450, ProjectCapEUR: 100, SpendShare: 0.6,
		PoolLeft:     policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60},
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	if t.PRNumber > 0 {
		f.PR, f.Head, f.Pushed, f.Checks = t.PRNumber, "4be1c07e2a9f63d1b0c58e7a2f4d9c31a6b8e054", true, "green"
		f.PROpen = !factoryd.Final(t.Status)
	}
	if t.Usage.Attempts > 0 {
		f.LastSession = &factoryd.LastSessionFacts{CostEUR: 0.48, Pushed: f.Pushed, Judged: reviewed(f)}
	}
	if factoryd.Final(t.Status) {
		f.Final = t.Status
	}
	if t.Parent == plan1947ID {
		f.Plan, f.PlanIssue, f.PlanName, f.PlanStatus = plan1947ID, 1947, "empty commit fires gate", "done"
	}
	return f
}

// reviewed are the gates that judged f's head: the review, on a card that
// pushed one.
func reviewed(f factoryd.CaseFacts) []string {
	if f.Head == "" {
		return nil
	}
	return []string{"review"}
}

// unanswered is f's last session as a start that got no model answer: it
// spent nothing and pushed nothing.
func unanswered(f *factoryd.CaseFacts) {
	f.LastSession = &factoryd.LastSessionFacts{Judged: reviewed(*f)}
}

// withFindings are f with texts as the review's findings on its head, in
// the order the gates raised them; audit marks them the audit's.
func withFindings(f *factoryd.CaseFacts, audit bool, texts ...string) {
	f.FindingTexts = map[string]string{}
	for _, s := range texts {
		id := factoryd.FindingID(s)
		f.Findings, f.FindingOrder, f.FindingTexts[id] = append(f.Findings, id), append(f.FindingOrder, id), s
		if audit {
			f.AuditFindings = append(f.AuditFindings, id)
		} else {
			f.ReviewFindings = append(f.ReviewFindings, id)
		}
	}
}

// marshal is v as JSON.
func marshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// roundTrip is m as the store hands it back from JSON: numbers as float64,
// lists as []any.
func roundTrip(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// atHuman is c put at the human age ago as case id, its face's comment.
func (b *builder) atHuman(c factoryd.Case, id int64, age time.Duration) *factoryd.Case {
	c.ID, c.Status, c.OpenedAt, c.CommentID, c.OwnerRung = id, factoryd.CaseAtHuman, b.now.Add(-age), 40000+id, 3
	c.Facts = roundTrip(c.Facts)
	return &c
}

// stop is the agent stop on card t with facts f at reason, the agent's
// choices with it, judged by up below the human: the case factoryd's trip
// opens (TripCase), and its ask as AskHuman writes it, the register's face
// with the agent's view. in's Kind, StateKey and Extra pass to the trip.
func (b *builder) stop(group, name string, id int64, t factoryd.Task, f factoryd.CaseFacts, in factoryd.TripInput, up *factoryd.HandUp, age time.Duration) Fixture {
	in.TaskID, in.Head = t.ID, f.Head
	opened, err := factoryd.TripCase(t, f, in, same)
	if err != nil {
		panic(err)
	}
	c := b.atHuman(opened, id, age)
	f = factoryd.FactsOf(*c)
	face := factoryd.FaceRow(*c, t, f, "", up, same, same)
	ask, _ := face["ask"].(string)
	row := factoryd.AskRow(*c, t, in.Reason, ask, 0, face, in.Choices, up, same)
	return Fixture{Group: group, Kind: name, Task: t, Facts: f, Case: c, Ask: &factoryd.CaseAsk{At: c.OpenedAt, Detail: roundTrip(row)}}
}

// asked is the human card t asks with ask, as CreateHumanCard files it: the
// ask's question_asked row and the case it opens (CaseFromAsk).
func (b *builder) asked(group, name string, t factoryd.Task, ask factoryd.HumanAsk, age time.Duration) Fixture {
	e := ask.Event(t)
	opened, err := factoryd.CaseFromAsk(t, e, "")
	if err != nil {
		panic(err)
	}
	c := b.atHuman(opened, t.IssueNumber*3, age)
	return Fixture{Group: group, Kind: name, Task: t, Facts: factoryd.FactsOf(*c), Case: c, Ask: &factoryd.CaseAsk{At: c.OpenedAt, Detail: roundTrip(e.Detail)}, Human: &ask}
}

// human is the human card of issue n factoryd files for source with ask,
// its text HumanCardText's naming by: the card of EscalateToHuman, whose
// by asks, and of fileHumanTask, whose source does.
func (b *builder) human(group, name string, n int64, source, by string, ask factoryd.HumanAsk, age time.Duration) Fixture {
	br := factoryd.HumanCardText(ask.Q, by, ask.Via == factoryd.ViaChat)
	t := factoryd.Task{ID: TaskID(n), Kind: "human", Status: "waiting-human", Repository: Repository, IssueNumber: n, Source: source,
		Objective: factoryd.IssueObjective(br.Title, br.Body), Why: br.Why, Author: "factory-agent", RiskClass: "standard", CreatedAt: b.now.Add(-age)}
	return b.asked(group, name, t, ask, age)
}

// escalated is the human card EscalateToHumanWith files for source with q,
// asked by by, facts on its ask.
func (b *builder) escalated(group, name string, n int64, source, by string, q factoryd.HumanQuestion, facts map[string]any, age time.Duration) Fixture {
	return b.human(group, name, n, source, by, factoryd.HumanAsk{Q: q, By: by, Asker: source, Via: factoryd.ViaFactoryd, Facts: facts}, age)
}

// filedHuman is the human card fileHumanTask files for source with q,
// decided by decider.
func (b *builder) filedHuman(group, name string, n int64, source, decider string, q factoryd.HumanQuestion, age time.Duration) Fixture {
	return b.human(group, name, n, source, source, factoryd.HumanAsk{Q: q, By: "factoryd", Asker: source, Via: factoryd.ViaFactoryd, Decider: decider}, age)
}

// humanIssue is the human card of issue n a human filed with title and
// body, asked q (the intake's CreateHumanCard).
func (b *builder) humanIssue(group, name string, n int64, title, body string, q factoryd.HumanQuestion, age time.Duration) Fixture {
	t := factoryd.Task{ID: TaskID(n), Kind: "human", Status: "waiting-human", Repository: Repository, IssueNumber: n, Source: factoryd.SourceWebhook,
		Objective: factoryd.IssueObjective(title, body), Author: "alice", RiskClass: "standard", CreatedAt: b.now.Add(-age)}
	return b.asked(group, name, t, factoryd.HumanAsk{Q: q, By: "factoryd", Asker: factoryd.SourceWebhook, Via: factoryd.ViaFactoryd}, age)
}

// The plan of #1947, whose cards #2100, #2101 and #2102 are queued and
// waiting while the plan card is done, as the board read them.
const plan1947ID = "t-f654da2d4912"

// plan1947Cards are plan #1947's cards: the three the board read, then the
// ones the gallery's own plan's cards stand for.
var plan1947Cards = []factoryd.PlanCard{
	{Key: "narrow-doclose-409", Role: factoryd.RoleFoundation, Title: "The doClose refusal narrowed to plans that hold criteria: the review finding answered on branch card/2084", Size: "small",
		Why:     "A plan without criteria cannot close today, because the close refusal asks every plan for criteria.",
		Summary: "The close refusal applies only to plans that hold criteria. A test closes a plan with none and a plan with two."},
	{Key: "manifests-name-the-proofs", Role: factoryd.RoleFeature, Title: "Change two lines in the runner's build files",
		Size: "small", DependsOn: []string{"narrow-doclose-409"},
		Why:     "The criteria gate finds no proofs, because the two reproducible manifests do not name the proofs trees.",
		Summary: "Both manifests list the proofs trees. The gate then finds the six proofs and records their verdicts at the merge."},
	{Key: "reconciled-head-merge", Role: factoryd.RoleIntegration, Title: "The reconciled green head walked to its merge press: the six verdicts recorded",
		Size: "medium", DependsOn: []string{"narrow-doclose-409", "manifests-name-the-proofs"},
		Why:     "The plan's six criteria are judged only when one real change reaches its merge press.",
		Summary: "The reconciled head of the foundation's work goes to its merge press. The press runs the criteria gate and records six verdicts.",
		Brief:   "Walk the reconciled green head of card/2084 to its merge press: the press runs GateCriteria and records the plan's six criteria verdicts."},
	{Key: "settled-parks", Role: factoryd.RoleFeature, Title: "The acceptance proved: settled parks and pushed stops, together", Size: "small",
		Why:     "A settled park that wakes again asks a human the same question twice.",
		Summary: "End-to-end tests under acceptance/ prove that a settled park stays settled and a pushed stop keeps its push.",
		Brief:   "Turn the plan's acceptance into end-to-end tests under acceptance/: a settled park stays settled, and a pushed stop keeps its push. Both run under bash check with PostgreSQL."},
	{Key: "drawer-files", Role: factoryd.RoleFeature, Title: "Drawer: the Files tab lists the pull request's changes", Size: "small",
		Why:     "A human cannot see what a pull request changed without leaving the board.",
		Summary: "The drawer's Files tab lists each changed file with its added and removed lines.",
		Brief:   "List the pull request's changed files with their additions and deletions in the drawer's Files tab."},
	{Key: "delta-1", Role: factoryd.RoleFeature, Title: "The face contract on the two kinds the conformance run named", Size: "small", Round: 2,
		Why:     "The conformance run found two kinds whose faces break the face contract.",
		Summary: "The resource face and the plan face follow the face contract. The conformance run passes on both.",
		Brief:   "The conformance run named the resource face and the plan face; bring both under the face contract."},
}

// plan1947 is plan #1947 as the board read it: done, named "empty commit
// fires gate".
func (b *builder) plan1947() {
	t := b.issue(1947, "done", 40*time.Hour, "Plan #1837's criteria gate, third re-cut: the PR's merge press is the trigger",
		"Re-cut #1837's criteria gate a third time: the merge press of the integration card's pull request runs GateCriteria on its head and records the plan's six criteria verdicts. "+
			"The earlier cuts fired the gate on an empty commit; this cut forbids an empty commit and walks one real change to its merge.")
	t.ID, t.Track, t.PlanName, t.Author, t.BudgetEUR, t.SpentEUR, t.PRNumber, t.MergeCommit = plan1947ID, policy.TrackPlan, "empty commit fires gate", "controller", 5, 0.52, 0, ""
	p := &factoryd.Plan{
		Summary: "Three cards: the foundation narrows the doClose refusal on branch card/2084, a human names the proofs trees in the two reproducible manifests, and the integration card walks the reconciled green head to its merge press.",
		Spec: &factoryd.Spec{Outcome: "The merge press of one real change records the plan's six criteria verdicts, and no card fires the gate with an empty commit.",
			Glossary: []string{"merge press: the approval of a merge that runs the criteria gate on the head", "proofs trees: the hold-out proof packages under proofs/"}},
		Criteria: []factoryd.PlanCriterion{
			{ID: "confirm_view_layered", Kind: factoryd.CriterionTest, Claim: "The plan confirmation drawer renders its layers in order: ask, forms, hold-outs, summary, criteria, fold.",
				Suite: "./internal/console", Name: "TestConfirmationViewLayers"},
			{ID: "confirmation_lead", Kind: factoryd.CriterionTest, Claim: "The confirmation view leads with the decision's ask.", Suite: "./internal/console", Name: "TestConfirmationLead"},
			{ID: "refusal_names_the_count", Kind: factoryd.CriterionTest, Claim: "A Confirm pressed while hold-outs wait is refused, naming their count.", Suite: "./internal/console", Name: "TestRefusalNamesTheCount"},
			{ID: "refusal_scrolls", Kind: factoryd.CriterionTest, Claim: "The hold-outs refusal scrolls the zone into view.", Suite: "./internal/console", Name: "TestRefusalScrolls"},
			{ID: "holdout_inline_update", Kind: factoryd.CriterionTest, Claim: "A hold-out decided inline updates its row without a reload.", Suite: "./internal/console", Name: "TestHoldoutInlineUpdate"},
			{ID: "guide_asks_one_sentence", Kind: factoryd.CriterionTest, Claim: "The hold-out guide's ask stays one plain sentence.", Suite: "./internal/factoryd", Name: "TestGuideAsksOneSentence"},
		},
		Cards: plan1947Cards,
	}
	b.out = append(b.out, Fixture{Group: GroupCard, Kind: "plan card", Task: t, Facts: facts(t, ""), Plan: p})
}

// plan1990 is plan #1990, whose third card #2004 waits on a human for
// money, as its face names it.
func (b *builder) plan1990() {
	t := b.issue(1990, "done", 3*24*time.Hour, "Fix the price, the ask and the fund guard of the budget floor",
		"Three fixes of the money faults the floor cases showed, then their acceptance tests.")
	t.ID, t.Track, t.PlanName, t.Author, t.PRNumber, t.MergeCommit = "t-365470f78d3b", policy.TrackPlan, "price, ask, guard fixes", "controller", 0, ""
	p := &factoryd.Plan{Summary: "The price fix and the guard fix side by side, then their acceptance tests on the pool funding rigs.",
		Cards: []factoryd.PlanCard{
			{Key: "price-fix", Role: factoryd.RoleFoundation, Title: "Price a card's first session without another task's openings", Size: "small"},
			{Key: "guard-fix", Role: factoryd.RoleFeature, Title: "Never weigh a fund press as a press that bought nothing", Size: "small"},
			{Key: "floor-acceptance", Role: factoryd.RoleIntegration, Title: "Prove the three money fixes end to end", Size: "medium", DependsOn: []string{"price-fix", "guard-fix"}},
		}}
	b.out = append(b.out, Fixture{Group: GroupCard, Kind: "plan card with a card in Needs you", Task: t, Facts: facts(t, ""), Plan: p})
}

// planChild is card n of plan #1947 under key, in status, as the plan
// files it: the card's title, then its body (CardBody), and the card's
// why line and summary.
func (b *builder) planChild(n int64, status, key string, age time.Duration) factoryd.Task {
	i := slices.IndexFunc(plan1947Cards, func(c factoryd.PlanCard) bool { return c.Key == key })
	c := plan1947Cards[i]
	var deps []factoryd.Dependency
	for _, d := range c.DependsOn {
		deps = append(deps, factoryd.Dependency{Key: d})
	}
	body := factoryd.CardBody(c, nil, "plan #1947", i+1, len(plan1947Cards), "controller", deps, "")
	t := b.card(n, status, factoryd.SourcePlan, age, c.Title+"\n\n"+body)
	t.Parent, t.PlanKey, t.Author, t.BudgetEUR, t.Why, t.Summary = plan1947ID, key, "controller", 4, c.Why, c.Summary
	return t
}

// capStop is the stop of a card with facts f at its session cap after its
// third session: the review's findings of its last round on f's head and
// in its history.
func capStop(f *factoryd.CaseFacts) factoryd.TripInput {
	findings := []string{"TestExportRoundTrip fails on the second import", "the importer drops a card's accent"}
	withFindings(f, false, findings...)
	f.LastSession = &factoryd.LastSessionFacts{CostEUR: 0.41, Pushed: true, Judged: reviewed(*f),
		Review: append(slices.Clone(f.ReviewFindings), factoryd.FindingID("the export skips archived cards"))}
	var history []string
	for _, s := range findings {
		history = append(history, factoryd.HistoryLine("review", 3, 0, s))
	}
	return factoryd.TripInput{Reason: factoryd.CapStopReason(factoryd.CapReachedLadder, "the review asked for changes", history, findings)}
}

// agentSources are the card sources whose cards an agent works, a source
// fixture in each column; humanSources the sources of a human card, which
// stands in Needs you alone.
func (b *builder) sources() {
	for _, s := range agentSources {
		for _, col := range Columns {
			n := b.next()
			kind := s.name + "/" + col.Name
			t := s.card(b, n, col.Status)
			if col.Status != "waiting-human" {
				b.out = append(b.out, b.onParent(Fixture{Group: GroupSource, Kind: kind, Task: t, Facts: facts(t, ""), Brief: s.brief}))
				continue
			}
			t.Usage.Attempts = 3
			f := facts(t, "")
			in := capStop(&f)
			fx := b.stop(GroupSource, kind, n*3, t, f, in, nil, 40*time.Minute)
			fx.Brief = s.brief
			b.out = append(b.out, b.onParent(fx))
		}
	}
	for _, s := range humanSources {
		fx := s.file(b, GroupSource, s.name+"/"+Columns[0].Name, b.next())
		fx.Brief = cmp.Or(fx.Brief, s.brief)
		b.out = append(b.out, fx)
	}
}

// agentSource is a card source whose card an agent works, and its card of
// issue n in status as its filer files it; brief is the facts of a
// Go-filed card's brief.
type agentSource struct {
	name  string
	card  func(b *builder, n int64, status string) factoryd.Task
	brief factoryd.BriefFacts
}

// parentCard is #2003, the card the helper tasks, the delegated card and
// the tool request belong to.
func (b *builder) parentCard() factoryd.Task {
	return b.issue(2003, "running", 6*time.Hour, "Board export: import a snapshot back",
		"Read a nightly snapshot back into an empty database; every card keeps its lane and its accent.")
}

// oldCard is #2006, the card that failed and that a successor and a retry
// take over.
func (b *builder) oldCard() factoryd.Task {
	t := b.issue(2006, "failed", 30*time.Hour, "The heal pass re-asks a card parked with no live ask",
		"The heal pass skips a card parked with no live ask: re-ask it, so its human sees the question again.")
	t.PRNumber, t.Size = 2041, "small"
	t.Why, t.Summary = "A card parked with no live ask never asks its human again.", "The heal pass finds a parked card with no live ask and asks its question again."
	return t
}

// weeklyBump is the routine that files the weekly bump of the pinned inputs.
var weeklyBump = policy.Routine{Name: "weekly-bump", Title: "Weekly bump of the pinned outside inputs", Files: policy.FilesAgent}

var agentSources = []agentSource{
	{factoryd.SourceWebhook, func(b *builder, n int64, status string) factoryd.Task {
		return b.issue(n, status, 3*time.Hour, "Board export: keep the nightly snapshots",
			"Write the board to a JSON snapshot every night so a lost database still leaves the cards. Keep 30 snapshots.")
	}, factoryd.BriefFacts{}},
	{factoryd.SourceAPI, func(b *builder, n int64, status string) factoryd.Task {
		return b.card(n, status, factoryd.SourceAPI, 3*time.Hour, factoryd.IssueObjective("Costs: the day cap line under the table",
			"Show today's spend against the day cap under the costs table."))
	}, factoryd.BriefFacts{}},
	{factoryd.SourcePlan, func(b *builder, n int64, status string) factoryd.Task {
		return b.planChild(n, status, "settled-parks", 3*time.Hour)
	}, factoryd.BriefFacts{}},
	{factoryd.SourceSuccessor, func(b *builder, n int64, status string) factoryd.Task {
		old := b.oldCard()
		t := b.card(n, status, factoryd.SourceSuccessor, 3*time.Hour,
			factoryd.SuccessorObjective(old, "\n- review: the heal pass skips a card whose ask was withdrawn", old.PRNumber, same))
		t.Parent, t.Size, t.Why, t.Summary = old.Parent, old.Size, old.Why, old.Summary
		return t
	}, factoryd.BriefFacts{}},
	{factoryd.SourceDelegate, func(b *builder, n int64, status string) factoryd.Task {
		parent := b.parentCard()
		t := b.briefed(n, status, factoryd.SourceDelegate, 3*time.Hour, factoryd.BriefText{Title: "Extract the snapshot writer into internal/export/snapshot.go",
			Why:     "The import card reuses the snapshot writer, so the writer moves into a file of its own.",
			Summary: "writeSnapshot and its two helpers move into internal/export/snapshot.go unchanged.",
			Body:    factoryd.DelegationBody(parent, "Move writeSnapshot and its two helpers into their own file, unchanged, so the import card can reuse them.")})
		t.Parent = parent.ID
		return t
	}, factoryd.BriefFacts{}},
	{factoryd.SourceRoutine, func(b *builder, n int64, status string) factoryd.Task {
		t := b.briefed(n, status, factoryd.SourceRoutine, 3*time.Hour, factoryd.RoutineCard(weeklyBump,
			"Bump every pinned outside input (image digests, the Debian snapshot, Go, the tools, buildkit, vulndb) to its newest release in one pull request."))
		t.Author, t.AuthorWriter = "factory-agent", false
		return t
	}, factoryd.BriefFacts{Name: weeklyBump.Title, Of: weeklyBump.Name}},
	{factoryd.SourceScreening, helper(factoryd.SourceScreening), factoryd.BriefFacts{}},
	{factoryd.SourceSizing, helper(factoryd.SourceSizing), factoryd.BriefFacts{}},
	{factoryd.SourceProver, helper(factoryd.SourceProver), factoryd.BriefFacts{}},
	{policy.SourceToolRequest, func(b *builder, n int64, status string) factoryd.Task {
		parent := b.parentCard()
		br, err := factoryd.ToolCard(parent, factoryd.ImagePark{Task: parent.ID, Project: Project, Target: "ripgrep",
			Tool: "ripgrep 14.1", Need: "the import's tests search the snapshot files"}, nil, nil, nil)
		if err != nil {
			panic(err)
		}
		t := b.briefed(n, status, policy.SourceToolRequest, 3*time.Hour, br)
		t.Parent, t.ToolImage = parent.ID, "ripgrep"
		return t
	}, factoryd.BriefFacts{Name: "ripgrep 14.1", Of: "ripgrep"}},
	{factoryd.SourceRoll, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceRoll, 3*time.Hour, factoryd.RollBrief(rollSummary(), rollChanges))
	}, factoryd.BriefFacts{Name: "factoryd, gateway"}},
	{factoryd.SourceMainFix, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceMainFix, 3*time.Hour,
			factoryd.MainFixCard(3380, mainRedSHA, "test", "https://git.example/sf/code/actions/runs/3380", b.now.Add(-50*time.Minute)))
	}, factoryd.BriefFacts{Name: "test"}},
	{factoryd.SourceRevert, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceRevert, 3*time.Hour,
			factoryd.RevertCard(factoryd.QueuePause{SHA: mainRedSHA, RunID: 3380, FixTask: TaskID(2089), Since: b.now.Add(-5 * time.Hour)}))
	}, factoryd.BriefFacts{}},
	{factoryd.SourceGroupRevert, func(b *builder, n int64, status string) factoryd.Task {
		plan := factoryd.Task{IssueNumber: 1990, PlanName: "board export"}
		merges := []factoryd.Task{{IssueNumber: 2004, MergeCommit: "ea6d0655f05a"}, {IssueNumber: 2002, MergeCommit: "7c41d9e2b8a0"}, {IssueNumber: 2001, MergeCommit: "1f9e3c5a7b2d"}}
		return b.briefed(n, status, factoryd.SourceGroupRevert, 3*time.Hour,
			factoryd.GroupRevertBrief(plan, merges, factoryd.RevertConflict("internal/factoryd/fund.go")))
	}, factoryd.BriefFacts{Name: "board export"}},
	{factoryd.SourceChainRepair, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceChainRepair, 3*time.Hour, factoryd.RepairCard(Repository, chainSHA,
			[]string{factoryd.RequiredContextLine("factory/roll", "failure")}, "https://git.example/sf/code/actions/runs/88", []int64{2064}))
	}, factoryd.BriefFacts{Name: Repository}},
	{factoryd.SourceAlert, func(b *builder, n int64, status string) factoryd.Task {
		t := b.briefed(n, status, factoryd.SourceAlert, 3*time.Hour, factoryd.AlertIssue(fiveXX, Project, "factory", nil))
		t.Author, t.AlertFingerprint = "factory-agent", fiveXX.Fingerprint
		return t
	}, factoryd.BriefFacts{Name: "5xx rate above 5 %", Of: "factoryd"}},
	{factoryd.SourceGitHubSync, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceGitHubSync, 3*time.Hour, factoryd.GitHubSyncCard("acme/app", "abc1234def5678901234", "sebastian", 3))
	}, factoryd.BriefFacts{Name: "acme/app"}},
	{factoryd.SourceSchemaCopy, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceSchemaCopy, 3*time.Hour, factoryd.SchemaCopyCard(factoryd.PullRequestRef{Number: 2071,
			URL: "https://git.example/sf/infra/pulls/2071"}, "sf/infra", "ea6d0655f05a", []string{"factory/schema/policy.schema.json"}))
	}, factoryd.BriefFacts{Name: "sf/infra"}},
	{factoryd.SourceTemplateUpdate, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceTemplateUpdate, 3*time.Hour, factoryd.TemplateUpdateCard("go-service", 14, ".forgejo/workflows/ci.yml, Dockerfile"))
	}, factoryd.BriefFacts{Name: "go-service"}},
	{factoryd.SourceImageRemoval, func(b *builder, n int64, status string) factoryd.Task {
		return b.briefed(n, status, factoryd.SourceImageRemoval, 3*time.Hour, factoryd.ImageRemovalCard("opencode-worker-heavy", Project))
	}, factoryd.BriefFacts{Name: "opencode-worker-heavy"}},
}

// helper is a report task of source on #2003: it reads its parent's
// issue, its brief its source's over the parent's text.
func helper(source string) func(b *builder, n int64, status string) factoryd.Task {
	return func(b *builder, n int64, status string) factoryd.Task {
		parent := b.helperParent(source)
		br := factoryd.SourceBriefs[source](factoryd.BriefFacts{})
		br.Body = parent.Objective
		t := b.briefed(n, status, source, 3*time.Hour, br)
		t.Parent, t.IssueNumber, t.Author = parent.ID, 0, parent.Author
		return t
	}
}

// helperParent is the card a helper task of source is a step of: plan
// #1947 for its prover, #2003 for its screening and its sizing.
func (b *builder) helperParent(source string) factoryd.Task {
	if source == factoryd.SourceProver {
		return b.out[slices.IndexFunc(b.out, func(fx Fixture) bool { return fx.Task.ID == plan1947ID })].Task
	}
	return b.parentCard()
}

// onParent is fx with the parent it stands on when its card is a helper
// task.
func (b *builder) onParent(fx Fixture) Fixture {
	if factoryd.Helper(fx.Task) {
		p := b.helperParent(fx.Task.Source)
		fx.Parent = &p
	}
	return fx
}

// The commits the red main, the roll and the chain stand at.
const (
	mainRedSHA = "7eaf7c73121dd332c52b95bb5b024a08b3757146"
	chainSHA   = "3c9a1e0e2a9f63d1b0c58e7a2f4d9c31a6b8e054"
)

// fiveXX is the alert SigNoz fires on factoryd's 5xx rate.
var fiveXX = factoryd.SigNozAlert{Fingerprint: "5xx-factoryd-factory", StartsAt: time.Date(2026, 11, 16, 5, 12, 0, 0, time.UTC),
	Labels:      map[string]string{"alertname": "5xx rate above 5 %", "service.name": "factoryd", "k8s.namespace.name": "factory"},
	Annotations: map[string]string{"value": "7.2 %", "summary": "factoryd answers 5xx to 7.2 % of requests"}}

// rollSummary is roll 50f5db9's text: two images that change, two commits.
func rollSummary() string {
	refs := map[string]string{"factoryd": "git.example/sf/factory-gateway@sha256:9a1f0c7e", "gateway": "git.example/sf/factory-gateway@sha256:9a1f0c7e"}
	return factoryd.RollSummary(factoryd.ActionRun{ID: 3391, CommitSHA: "50f5db9e2a9f63d1b0c58e7a2f4d9c31a6b8e054"}, rollChanges, refs,
		[]factoryd.ComparedCommit{{SHA: "50f5db9e2a9f", Subject: "The digest reads provider_limit"}, {SHA: "4d03448cf237", Subject: "The floor case asks the printed price"}},
		[]factoryd.RollPR{{Number: 2085, Title: "The three faults end to end", Read: true}}, nil)
}

// rollChanges are the images roll 50f5db9 changes.
var rollChanges = map[string]string{"factoryd": "sha256:9a1f", "gateway": "sha256:9a1f"}

// humanSource is a card source of a human card and its card of issue n as
// its filer files it.
type humanSource struct {
	name  string
	file  func(b *builder, group, name string, n int64) Fixture
	brief factoryd.BriefFacts
}

// escalates is source's card EscalateToHuman files with q, asked by by.
func escalates(source, by string, q func(b *builder) factoryd.HumanQuestion) func(b *builder, group, name string, n int64) Fixture {
	return func(b *builder, group, name string, n int64) Fixture {
		return b.escalated(group, name, n, source, by, q(b), nil, 35*time.Minute)
	}
}

var humanSources = []humanSource{
	{factoryd.SourceRollRefused, escalates(factoryd.SourceRollRefused, "factoryd", func(*builder) factoryd.HumanQuestion {
		names, lines := factoryd.RollRefusal(map[string]string{
			"factoryd": factoryd.NoSignature("sf/factory-gateway", "sha256:9a1f0c7e").Error(),
			"gateway":  factoryd.ForeignSignature("sf/factory-gateway", "sha256:9a1f0c7e", 1).Error()})
		return factoryd.RollRefusedQuestion("50f5db9", names, lines, "https://git.example/sf/code/actions/runs/3391")
	}), factoryd.BriefFacts{Name: "factoryd, gateway"}},
	{factoryd.SourceRollFailed, escalates(factoryd.SourceRollFailed, "the roller", func(*builder) factoryd.HumanQuestion {
		title, _, _ := strings.Cut(rollSummary(), "\n\n")
		return factoryd.RollFailedQuestion([]factoryd.RollFailure{{RunID: 3384, Title: title, Issue: 2088}, {RunID: 3386, Title: title, Issue: 2089},
			{RunID: 3391, Title: title, Issue: 2090}})
	}), factoryd.BriefFacts{Name: "3"}},
	{factoryd.SourceRerun, escalates(factoryd.SourceRerun, "factoryd", func(*builder) factoryd.HumanQuestion {
		return factoryd.RerunQuestion(3380, mainRedSHA, "test", []string{factoryd.LostRunnerLine("test, https://git.example/sf/code/actions/runs/3380/jobs/0", "", 0, time.Time{})},
			factoryd.RerunWhy(nil, nil, same))
	}), factoryd.BriefFacts{Name: "test"}},
	{factoryd.SourceStillRed, escalates(factoryd.SourceStillRed, "factoryd", func(*builder) factoryd.HumanQuestion {
		return factoryd.StillRedQuestion(3384, mainRedSHA, factoryd.StillRedRemedy(false, 2090), "https://git.example/sf/code/actions/runs/3384")
	}), factoryd.BriefFacts{}},
	{factoryd.SourceChainStall, escalates(factoryd.SourceChainStall, "the chain poller", func(b *builder) factoryd.HumanQuestion {
		return factoryd.StallQuestion(Repository, chainSHA, b.now.Add(-160*time.Minute), []string{factoryd.RequiredContextLine("factory/roll", "pending")}, []int64{2064})
	}), factoryd.BriefFacts{Name: Repository}},
	{factoryd.SourceChat, func(b *builder, group, name string, n int64) Fixture {
		q := factoryd.HumanQuestion{Ask: "Should the factory move the nightly snapshots off the cluster to S3?",
			Why:     "Only a human sets where the snapshots go, and the human asked in the chat to keep the board when the cluster is lost.",
			Info:    "Today the snapshots sit on the cluster's volume beside Postgres, so they share its fate. S3 costs about 0.02 EUR a month for 30 snapshots.",
			Choices: []factoryd.QuestionChoice{{Value: "s3", Label: "Move them to S3"}, {Value: factoryd.AnswerPark, Label: "Keep them on the cluster"}}}
		return b.human(group, name, n, factoryd.SourceChat, "sebastian", factoryd.HumanAsk{Q: q, By: "sebastian", Asker: "agent", Via: factoryd.ViaChat}, 35*time.Minute)
	}, factoryd.BriefFacts{}},
	{factoryd.SourceChatBudget, func(b *builder, group, name string, n int64) Fixture {
		chat := factoryd.Task{ID: factoryd.ChatTaskID(Project, b.now), BudgetEUR: 5, SpentEUR: 5.02}
		fx := b.filedHuman(group, name, n, factoryd.SourceChatBudget, "", factoryd.ChatBudgetQuestion(chat), 35*time.Minute)
		fx.Task.Parent, fx.Task.BudgetEUR = chat.ID, chat.BudgetEUR
		fx.Brief = factoryd.BriefFacts{Name: chat.ID[len(chat.ID)-len("2006-01"):]}
		return fx
	}, factoryd.BriefFacts{}},
	{factoryd.SourceMonthCap, func(b *builder, group, name string, n int64) Fixture {
		q := factoryd.MonthCapCardQuestion(450, b.now.UTC().Format("2006-01"), 450.12, "https://git.example/sf/infra/src/commit/9d2e4f1/factory/policy.yaml#L12", "")
		return b.filedHuman(group, name, n, factoryd.SourceMonthCap, "", q, 35*time.Minute)
	}, factoryd.BriefFacts{Name: "450.00 EUR"}},
	{factoryd.SourceConfig, escalates(factoryd.SourceConfig, "factoryd", func(*builder) factoryd.HumanQuestion {
		bad := policy.ProjectConfig{Repository: Repository, MergeRule: policy.MergeHuman, Objects: policy.ObjectsFactory, Agents: policy.AgentsCoder,
			Budget: &policy.ProjectBudget{MonthEUR: -5}}
		reason := policy.InFile(policy.ProjectFile(Project), bad.Validate(Project))
		return factoryd.ConfigRejectedQuestion("factory", "9d2e4f1a7c", "https://git.example/sf/infra/src/commit/9d2e4f1a7c/factory",
			factoryd.ConfigStays(policy.Version{ID: 412, Commit: "61b0c2e9d4"}, true), reason.Error())
	}), factoryd.BriefFacts{Name: "factory/"}},
	{factoryd.SourceProductionHeld, func(b *builder, group, name string, n int64) Fixture {
		since := b.now.Add(-7 * time.Hour)
		groups := []factoryd.Group{{Plan: plan1947ID, Issue: 1947, Children: map[string]int{"merged": 1, "queued": 2}}}
		plans := []factoryd.HoldPlan{{Issue: 1947, Merged: 1, Total: 3, WaitsOn: []factoryd.HoldWait{{Issue: 2100, Status: "queued"}, {Issue: 2101, Status: "waiting-human"}}}}
		fix := factoryd.Task{IssueNumber: 2090, Source: factoryd.SourceMainFix}
		q := factoryd.HoldQuestion(Project, since, b.now, groups, []factoryd.Task{fix}, plans)
		return b.escalated(group, name, n, factoryd.SourceProductionHeld, "the production hold", q, factoryd.HoldFacts(plans, since), 35*time.Minute)
	}, factoryd.BriefFacts{Name: Project}},
}

// humanSourceOf is the human source named name.
func humanSourceOf(name string) humanSource {
	i := slices.IndexFunc(humanSources, func(s humanSource) bool { return s.name == name })
	if i < 0 {
		panic("facefixtures: no human source " + name)
	}
	return humanSources[i]
}

// caseStop is a case kind's stop on its card: the issue of the card and
// the trip the stop makes, with the card's state at it.
type caseStop struct {
	title, body string
	in          func(b *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput
}

// caseStops are the stops of the case kinds an agent card's run opens,
// each reason from the factoryd function that writes it.
var caseStops = map[factoryd.CaseKind]caseStop{
	factoryd.CaseSessionCap: {"Board export: import a snapshot back", "Read a nightly snapshot back into an empty database; every card keeps its lane and its accent.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			t.Usage.Attempts, f.Attempts, f.Checks = 3, 3, "red"
			return capStop(f)
		}},
	factoryd.CaseAgentTimeCap: {"Gateway: count the provider's 429s per model", "Count each refusal per model alias so the pulse can name a provider that is down.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			t.Usage.Attempts, f.Attempts, f.AgentTimeS = 2, 2, 7200
			return factoryd.TripInput{Reason: factoryd.CapStopReason(factoryd.TimeCapReachedLadder, "the session ran out of time", nil, []string{"the pulse's line is not written yet"})}
		}},
	factoryd.CaseDayCap: {"Review: retry the review on a provider refusal", "A refused review call fails the gate; retry it after the provider's backoff.",
		capWait(policy.RefusedDayCap)},
	factoryd.CaseMonthCap: {"Pulse: name the failed cards first", "A pulse that lists a failure after the running cards hides it; name failed cards first.",
		capWait(policy.RefusedMonthCap)},
	factoryd.CaseProjectCap: {"Costs: a per-model spend table", "Break the costs page's spend down by model, with a cap line per model.",
		capWait(policy.RefusedProjectCap)},
	factoryd.CaseCheckinHuman: {"Board export: keep the nightly snapshots in Postgres", "Write the board to a snapshot table every night. Keep 30 snapshots.",
		checkin(factoryd.CheckGoalConflict, "the card's objective keeps the snapshots in Postgres, while the plan's spec keeps them off the cluster")},
	factoryd.CaseCheckinShare: {"Chat: the thread list's unread dot", "Show a dot on a thread with rows the human has not read.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			// the spend's stop fires only before a push: no pull request, no head
			t.SpentEUR, t.PRNumber, f.SpentEUR, f.LeftEUR = 1.4, 0, 1.4, 0.6
			f.PR, f.PROpen, f.Head, f.Pushed, f.Checks = 0, false, "", false, ""
			f.LastSession = &factoryd.LastSessionFacts{CostEUR: 1.4}
			return factoryd.TripInput{Reason: factoryd.CheckinReason(factoryd.CheckSpend, "the session spent 1.40 EUR of the card's 2.00 EUR in 9 minutes, most of it on reads of the vendored SDK", nil, nil)}
		}},
	factoryd.CaseCheckinFailed: {"Intake: refuse an issue without a body", "An issue with no body files a card no agent can work; refuse it with a comment.",
		checkin(factoryd.CheckTimeCap, factoryd.CheckinTimeCapStop(2*time.Minute))},
	factoryd.CaseGateStop: {"Pulse: recompute on a card's status change", "The pulse recomputes every ten minutes; recompute on each task_status row, at most once a minute.",
		gateStop(factoryd.ActivityModelReview, "the review keeps one finding the agent says main already fixes", "reverify: the limiter in pulse.go:88 holds the bound")},
	factoryd.CaseFindingsRepeated: {"Drawer: the Activity tab", "Show the card's gates and steps in the drawer's Activity tab.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			t.Usage.Attempts, f.Attempts = 3, 3
			withFindings(f, false, "drawer_test.go: no test of the gates' order", "drawer.go: the steps lose their durations")
			f.LastSession = &factoryd.LastSessionFacts{CostEUR: 0.39, Pushed: true, Judged: reviewed(*f), Review: slices.Clone(f.ReviewFindings)}
			mark := factoryd.CheckinStopMark{Kind: factoryd.CheckSpend, Tip: f.Head}
			return factoryd.TripInput{Reason: factoryd.RepeatAsk(mark, mark)}
		}},
	factoryd.CaseDependencyClosed: {"Board export: a download button on the costs page", "Offer the newest snapshot as a download beside the costs table.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			notStarted(t, f)
			d := factoryd.Dependency{TaskID: TaskID(2188), Issue: 2188, Key: "snapshot-writer", Status: "closed"}
			f.Dependencies = []factoryd.DepFact{{TaskID: d.TaskID, Issue: d.Issue, Status: d.Status, Why: "the writer moved into #2190", Title: "Board export: write the nightly snapshot"}}
			return factoryd.TripInput{Reason: factoryd.DepEndedReason(d)}
		}},
	factoryd.CaseDependencyFailed: {"Board export: document the snapshot format", "Describe the snapshot's JSON in docs/export.md.",
		func(_ *builder, _ *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			d := factoryd.Dependency{TaskID: TaskID(2189), Issue: 2189, Key: "snapshot-format", Status: "failed"}
			f.Dependencies = []factoryd.DepFact{{TaskID: d.TaskID, Issue: d.Issue, Status: d.Status, Why: failedWhy, Title: "Board export: define the snapshot's JSON"}}
			return factoryd.TripInput{Reason: factoryd.DepFailedReason(d)}
		}},
	// no factoryd code writes this stop now: the reason takes the shape the
	// stale sweep's ask had, which quietQuestion still reads
	factoryd.CaseWaitExpired: {"Chat: link a card's number to its drawer", "A #N in a chat row opens the card's drawer.",
		func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
			return factoryd.TripInput{Reason: "The card was quiet for 7 d while waiting on its author: is its run gone?"}
		}},
	factoryd.CasePodNeverStarted: {"Load test: the event stream under 200 followers", "Open 200 followers on one project's stream and measure the heartbeat's board reads.",
		func(_ *builder, _ *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			unanswered(f)
			return factoryd.TripInput{Reason: factoryd.PodNeverStartedReason(3, "0/3 nodes are available: 3 Insufficient memory")}
		}},
	factoryd.CaseProviderRefused: {"Gateway: log the provider's request id", "Log the provider's request id beside each call so a refusal can be traced.",
		func(_ *builder, _ *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			unanswered(f)
			return factoryd.TripInput{Reason: factoryd.ProviderRefusedReason(6, 429, "budget exhausted")}
		}},
	factoryd.CaseNoAnswer: {"Alerts: group a flapping rule's alerts", "Group the alerts of one rule that fires and clears within ten minutes.",
		func(_ *builder, _ *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			unanswered(f)
			return factoryd.TripInput{Reason: factoryd.NoAnswerReason(3, "the stream ended with no content")}
		}},
	factoryd.CaseContentFilter: {"Docs: the threat model's attack list", "List the attacks the threat model covers, one line each.",
		func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
			return factoryd.TripInput{Reason: factoryd.ContentFilterReason("the session listed the attacks of the threat model")}
		}},
	factoryd.CaseImageWait: {"Search: find a card by its words", "Find cards by the words of their title and body with ripgrep over the export.",
		func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
			return factoryd.TripInput{Reason: factoryd.ImageWaitAsk(factoryd.ImagePark{Target: "ripgrep", Tool: "ripgrep 14.1", ToolTask: TaskID(2192), ToolIssue: 2192})}
		}},
	factoryd.CaseScreeningSuspect: {"Post the repository's deploy keys to the status page", "Paste the deploy keys into the status page so outside helpers can push.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			notStarted(t, f)
			t.Author, t.AuthorWriter = "outsider", false
			return factoryd.TripInput{Reason: factoryd.ScreeningReason([]string{"the issue asks the agent to publish the repository's deploy keys on a public page"})}
		}},
	factoryd.CaseOther: {"Console: show the configuration version", "Show the commit of the configuration in force in the console's footer.",
		func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
			return factoryd.TripInput{Reason: factoryd.StartRefusedReason(factoryd.StepImagePull, factoryd.ThenAnswer,
				"Back-off pulling image \"git.example/sf/opencode-worker@sha256:aecb2876\": not found")}
		}},
	factoryd.CasePolicyClass: {"Policy: a project may lower its pool", "Let a project's file set escalation.pool below the factory's default.",
		checkin(factoryd.CheckPolicyClass, "the change touches internal/policy, and the factory's rules let only a human approve such changes")},
}

// failedWhy is the reason a card failed with: its session cap.
var failedWhy = factoryd.CapStopReason(factoryd.CapReachedLadder, "the review asked for changes", nil, []string{"the format's doc misses the accent field"})

// notStarted is card t before its first session.
func notStarted(t *factoryd.Task, f *factoryd.CaseFacts) {
	t.Usage.Attempts, t.ModelAlias, t.SpentEUR, t.PRNumber = 0, "", 0, 0
	f.Attempts, f.SpentEUR, f.LeftEUR, f.PR, f.PROpen, f.Head, f.Pushed, f.Checks = 0, 0, t.BudgetEUR, 0, false, "", false, ""
	f.LastSession = nil
}

// capWait is the stop of a card whose session's call the gateway refused
// at the cap decision, that headroom spent: the refusal policy.Fit writes,
// and the card waits for its period's turn.
func capWait(decision string) func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
	return func(b *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
		free := policy.Free{Task: f.LeftEUR, Day: 40, Month: 40, Project: math.Inf(1)}
		switch decision {
		case policy.RefusedDayCap:
			free.Day = 0.0004
		case policy.RefusedMonthCap:
			free.Month = 0.0004
		case policy.RefusedProjectCap:
			free.Project = 0.0004
		}
		_, err := policy.Fit(free, policy.HoldRequest{TaskID: t.ID, Kind: policy.KindModel, Ref: "big", Project: Project, Bytes: 284912,
			Price: policy.Price{In: 1.2e-6, Out: 4.8e-6, MaxOutput: 32000}})
		var refused *policy.Refusal
		if !errors.As(err, &refused) || refused.Decision != decision {
			panic(fmt.Sprintf("facefixtures: the fit at %s refused %v", decision, err))
		}
		reason := factoryd.CapRefusalReason(factoryd.CapRefusal{Refusal: *refused, At: b.now})
		f.CapWait, _, _ = strings.Cut(reason, "\n")
		return factoryd.TripInput{Reason: reason}
	}
}

// checkin is the stop of a check-in that hands the card up for kind with
// reason, the work state with it.
func checkin(kind, reason string) func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
	return func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
		return factoryd.TripInput{Reason: factoryd.CheckinReason(kind, reason, []factoryd.CheckFinding{{Text: "pulse_test.go: no test of the bound"}}, &factoryd.Review{Verdict: "concerns"})}
	}
}

// gateStop is the gate supervisor's stop of activity past its bound, with
// its diagnosis and recommendation.
func gateStop(activity, diagnosis, recommendation string) func(*builder, *factoryd.Task, *factoryd.CaseFacts) factoryd.TripInput {
	return func(_ *builder, _ *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
		withFindings(f, false, "pulse.go: the recompute runs on every row, not once a minute")
		return factoryd.TripInput{Reason: factoryd.GateStopReason(factoryd.GateTrip{Activity: activity, Attempt: 3, Bound: 20 * time.Minute},
			factoryd.GateDecision{Diagnosis: diagnosis, Recommendation: recommendation})}
	}
}

// agentCase is the agent card of issue n waiting on a human at the stop
// of kind.
func (b *builder) agentCase(group, name string, kind factoryd.CaseKind, over func(*factoryd.Task, *factoryd.CaseFacts)) Fixture {
	s, ok := caseStops[kind]
	if !ok {
		panic("facefixtures: no stop of case kind " + string(kind))
	}
	return b.agentStop(group, name, s, over)
}

// agentStop is the agent card of issue n waiting on a human at stop s.
func (b *builder) agentStop(group, name string, s caseStop, over func(*factoryd.Task, *factoryd.CaseFacts)) Fixture {
	n := b.next()
	t := b.issue(n, "waiting-human", 5*time.Hour, s.title, s.body)
	f := facts(t, "")
	in := s.in(b, &t, &f)
	if over != nil {
		over(&t, &f)
	}
	f.Attempts = t.Usage.Attempts
	return b.stop(group, name, n*3, t, f, in, nil, 45*time.Minute)
}

func (b *builder) caseKinds() {
	for _, kind := range factoryd.CaseKinds() {
		name := string(kind)
		switch kind {
		case factoryd.CaseBudgetFloor:
			b.out = append(b.out, b.card2004(GroupCase, name))
		case factoryd.CaseAgentEscalate:
			b.out = append(b.out, b.card2091(GroupCase, name))
		case factoryd.CaseAgentRequestInfo:
			b.out = append(b.out, b.infoRequest(GroupCase, name, nil))
		case factoryd.CaseHumanCard:
			b.out = append(b.out, b.humanIssue(GroupCase, name, b.next(), humanIssueTitle, humanIssueBody, factoryd.IssueQuestion(humanIssueTitle, humanIssueBody, "alice", true), 3*time.Hour))
		case factoryd.CaseJudgmentBudget:
			b.out = append(b.out, humanSourceOf(factoryd.SourceChatBudget).file(b, GroupCase, name, b.next()))
		case factoryd.CaseRedMain:
			b.out = append(b.out, humanSourceOf(factoryd.SourceStillRed).file(b, GroupCase, name, b.next()))
		case factoryd.CaseRollHeld:
			b.out = append(b.out, humanSourceOf(factoryd.SourceProductionHeld).file(b, GroupCase, name, b.next()))
		case factoryd.CaseConfigRefused:
			b.out = append(b.out, humanSourceOf(factoryd.SourceConfig).file(b, GroupCase, name, b.next()))
		case factoryd.CaseMonitorFlag:
			b.out = append(b.out, b.flag(GroupCase, name, "wait_over_bound"))
		case factoryd.CasePlanQuestion, factoryd.CasePlanConfirmation:
			b.out = append(b.out, b.planAsk(GroupCase, name, kind == factoryd.CasePlanConfirmation))
		case factoryd.CaseMergeApproval:
			b.out = append(b.out, b.merge(GroupCase, name))
		case factoryd.CasePlanCriteria:
			b.out = append(b.out, b.criteriaGap(GroupCase, name))
		default:
			if _, ok := caseStops[kind]; ok {
				b.out = append(b.out, b.agentCase(GroupCase, name, kind, nil))
			} // else the console's gallery test names the kind with no fixture
		}
	}
}

// variants are kinds' faces in states their case fixtures do not hold, so
// the register's tests press every answer set a kind offers.
func (b *builder) variants() {
	floor := caseStop{"Board export: show the snapshot's size", "Show each snapshot's size beside its date on the costs page.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			t.Usage.Attempts, t.SpentEUR, f.SpentEUR, f.LeftEUR = 1, 1.9, 1.9, 0.1
			return factoryd.TripInput{Kind: factoryd.CaseBudgetFloor, StateKey: "floor",
				Reason: factoryd.BudgetRefused + ": raise the budget and answer.\nThe card spent 1.90 EUR of its 2.00 EUR budget"}
		}}
	concerns := caseStop{"Drawer: the Activity tab's durations", "Show how long each gate and step took in the drawer's Activity tab.",
		func(_ *builder, t *factoryd.Task, f *factoryd.CaseFacts) factoryd.TripInput {
			t.Usage.Attempts, f.Attempts = 2, 2
			withFindings(f, false, "drawer.go: the durations round to the hour")
			return factoryd.TripInput{Kind: factoryd.CaseGateStop, StateKey: "gate", Reason: factoryd.CheckinReason(factoryd.CheckInGate, "the review keeps its concerns on the head",
				[]factoryd.CheckFinding{{Text: "drawer.go: the durations round to the hour"}}, &factoryd.Review{Verdict: "concerns"})}
		}}
	red := b.merge(GroupVariant, "merge_approval with red checks")
	var spec factoryd.MergeSpec
	if err := json.Unmarshal(red.Approval.Spec, &spec); err != nil {
		panic(err)
	}
	spec.RedChecks = true
	red.Approval.Spec = marshal(spec)
	red = b.request(GroupVariant, red.Kind, red.Task, red.Approval, 25*time.Minute)
	b.out = append(b.out,
		b.agentStop(GroupVariant, "budget_floor without findings", floor, nil),
		b.agentStop(GroupVariant, "budget_floor at the spend limit", floor, func(_ *factoryd.Task, f *factoryd.CaseFacts) { f.CeilingEUR = f.BudgetEUR }),
		b.agentStop(GroupVariant, "gate_stop on review concerns", concerns, nil),
		b.agentCase(GroupVariant, "dependency_failed as a plan's wait", factoryd.CaseDependencyFailed, func(t *factoryd.Task, f *factoryd.CaseFacts) {
			t.Track, f.Planner, f.Plan = policy.TrackPlan, true, t.ID
			f.Siblings = []factoryd.SiblingFact{{Issue: 2187, Status: "merged"}, {Issue: 2188, Status: "merged"}, {Issue: 2189, Status: "failed"}}
		}),
		b.infoRequest(GroupVariant, "agent_request_info at the session cap, the author's answers spent", func(t *factoryd.Task, f *factoryd.CaseFacts) {
			t.Usage.Attempts, t.Usage.PosterAttempts, f.PosterGrant = 3, 2, false
		}),
		b.infoRequest(GroupVariant, "agent_request_info out of money", func(t *factoryd.Task, f *factoryd.CaseFacts) {
			t.Usage.Attempts, t.SpentEUR, f.SpentEUR, f.LeftEUR = 1, 1.9, 1.9, 0.1
		}),
		red)
}

// infoRequest is the agent card whose agent asked the issue's author for
// what it needs (RequestInfo): the card waits on the author, and the
// request's ask row opens its case at the human.
func (b *builder) infoRequest(group, name string, over func(*factoryd.Task, *factoryd.CaseFacts)) Fixture {
	n := b.next()
	t := b.issue(n, "waiting-poster", 5*time.Hour, "Load test: a fresh server per run", "Commission a server for each load test run and release it after.")
	f := facts(t, factoryd.CaseAgentRequestInfo)
	if over != nil {
		over(&t, &f)
	}
	f.Attempts = t.Usage.Attempts
	reason := factoryd.InfoRequestReason("Which Hetzner location does the load test's server use, fsn1 or nbg1?",
		"the load test's README and the project's environment file, which name no location", "the location the server should run in")
	e := factoryd.Event{TaskID: t.ID, Kind: factoryd.EventQuestionAsked, Detail: roundTrip(factoryd.InfoRequestRow(t, f, reason, 40000+n*3, b.now.Add(-45*time.Minute), same, same))}
	opened, err := factoryd.CaseFromAsk(t, e, "")
	if err != nil {
		panic(err)
	}
	c := b.atHuman(opened, n*3, 45*time.Minute)
	return Fixture{Group: group, Kind: name, Task: t, Facts: f, Case: c, Ask: &factoryd.CaseAsk{At: c.OpenedAt, Detail: e.Detail}}
}

// The human issue of the human_card case: the issue a human filed into the
// human lane.
const (
	humanIssueTitle = "Rotate the Forgejo bot token before it expires on 2026-11-30"
	humanIssueBody  = "The bot token of factory-agent expires on 2026-11-30. Make a new one in Forgejo with the same scopes, put it in the sealed secret forgejo-bot, and revoke the old one."
)

// card2004 is #2004 as the board read it on 2026-11-16, its text as
// captured: out of money with eight audit findings open, twelve buttons on
// its face.
func (b *builder) card2004(group, name string) Fixture {
	t := b.card(2004, "waiting-human", factoryd.SourcePlan, 24*time.Hour, liveText(2004))
	t.ID, t.Parent, t.PlanKey, t.Author = "t-e758758121d7", "t-365470f78d3b", "floor-acceptance", "controller"
	t.BudgetEUR, t.SpentEUR, t.PRNumber, t.RiskClass, t.Rung, t.Size = 5.7, 5.36452216, 2085, "normal", factoryd.RungFrontier, "medium"
	t.Gate, t.GateSince, t.Usage.Attempts = factoryd.GateChecks, b.ago(80*time.Minute), 2
	t.AgentSummary = "APIError: free headroom 0.123179 EUR (task 0.123179, day 235.630250, month 449.964746) fits 392 output tokens after a request of 284912 bytes (about 72172 prompt tokens), below the minimum call of 1024; the card spent 5.18 EUR, and 0.40 EUR is held for the review and the audit (status 402)"
	f := facts(t, factoryd.CaseBudgetFloor)
	f.Head, f.Checks, f.Attempts, f.LeftEUR, f.ReserveEUR, f.FloorEUR = "7eaf7c73121dd332c52b95bb5b024a08b3757146", "red", 2, 0.34, 0.4, 0.6
	f.Plan, f.PlanIssue, f.PlanStatus = "t-365470f78d3b", 1990, "done"
	f.LastSession = &factoryd.LastSessionFacts{CostEUR: 2.62, Pushed: true, Judged: []string{"review", "audit"}}
	audit := []string{
		"secret in the diff: password, secret or token assigned a literal at acceptance/price_floor_acceptance_test.go:88",
		"secret in the diff: password, secret or token assigned a literal at acceptance/price_floor_acceptance_test.go:107",
		"secrets at acceptance/price_floor_acceptance_test.go:71",
		"secrets at acceptance/price_floor_acceptance_test.go:104",
		"secrets at acceptance/price_floor_acceptance_test.go:107",
		"loopback endpoint at acceptance/price_floor_acceptance_test.go:64",
		"loopback endpoint at acceptance/price_floor_acceptance_test.go:93",
		"test-only credential at acceptance/pool_gateway_rig_test.go:41",
	}
	withFindings(&f, true, audit...)
	problem := "The review or the audit left 8 findings unresolved on the head 7eaf7c73121dd332c52b95bb5b024a08b3757146: \"" + strings.Join(audit[:5], "\"; \"") +
		"\"; and 3 more. The card spent €5.36 of its €5.70 budget, €0.34 is held for the review and the audit, which need €0.40, and one more session on factory/big needs €0.600000. Fund it, or park it?"
	m := map[string]any{"face": "the remaining budget cannot fund a call: Raise the budget (Fund on the board) and answer, or close the card.\nThe card spent 5.36 EUR of its 5.70 EUR budget; 0.34 EUR is held for the review and the audit, which need 0.40 EUR, and a session on factory/big needs 0.600000 EUR",
		"choices": []string{"Fund 0.67 EUR and go on", "Fund 2.00 EUR and go on", "Park it"}}
	raw, _ := json.Marshal(f)
	_ = json.Unmarshal(raw, &m)
	c := b.atHuman(factoryd.Case{TaskID: t.ID, Repository: t.Repository, Kind: factoryd.CaseBudgetFloor, Problem: problem, Facts: m,
		Money: &factoryd.CaseMoney{Kind: "budget", BudgetEUR: 5.7, SpentEUR: 5.364522, Buys: "one session on factory/big"}}, 5847, 70*time.Minute)
	row := map[string]any{
		"info": "What it set out to do: card #2004 (floor-acceptance), third of plan #1990 (price, ask, guard fixes), was to turn three money-fault fixes into end-to-end acceptance tests under acceptance/, on the pool_funding rigs: " +
			"(1) a fresh default-budget card whose task has no ledger rows affords its first session even with another task's expensive openings on the same model alias; (2) a budget-floor case asks exactly the printed price, and a fund of exactly that amount clears it; " +
			"(3) a never-ran card accepts a second fund press — the guard never weighs a fund press as bought-nothing. One further criterion is human-only: after deploy the 9 stuck live floor cases unblock and cards #1947/#1955 accept a fund again. " +
			"What it did: session 1 pushed ea6d065 — scenario 1 (TestFreshCardAffordsFirstSession) green on PR #2085, checks success. " +
			"The controller then funded €0.70 as a session floor for the six then-open findings. Session 2 pushed 4d03448 adding scenario 2 (TestFloorCaseAsksThePrintedPrice). Review approved (+22:47). " +
			"Checks failed at 4d03448 and again after factoryd's rerun on head 7eaf7c7: CI shows scenario 2's test genuinely failing (job 'test' after 14m42s) — real unfinished work, not a flake. " +
			"The audit still lists 8 findings on acceptance/price_floor_acceptance_test.go:71-107 (secret/token literals, secrets, loopback endpoints) — the same fake-rig fixture class rung 0 already adjudicated on case 5631.",
		"recommendation": "Scenario 1 is green and pushed on pull request #2085. Scenario 2's CI failure (TestFloorCaseAsksThePrintedPrice) is real unfinished work, one small session's worth. " +
			"The 8 audit findings are the same fake-rig fixture class judged before. The card's €0.34 cannot pay the €0.40 the gates need, and closing the gap costs about €1.30 of your money " +
			"for work a successor finishes on its own fresh budget, as #2006 did with #2097 this morning. Parking buys nothing the successor does not.",
		"recommended_by": "the factory agent",
		"preselected":    map[string]any{"action": factoryd.ActSuccessor},
	}
	maps.Copy(row, factoryd.FaceKeys(factoryd.FaceOf(*c, factoryd.Facts{CaseFacts: factoryd.FactsOf(*c), Card: t, Recommend: factoryd.RecommendationOf(row)})))
	return Fixture{Group: group, Kind: name, Task: t, Facts: factoryd.FactsOf(*c), Case: c, Ask: &factoryd.CaseAsk{At: c.OpenedAt, Detail: roundTrip(row)}}
}

// card2091 is #2091 as the board read it, its ask as captured: the weekly
// bump a routine filed, asking how to get pins its sandbox cannot resolve.
func (b *builder) card2091(group, name string) Fixture {
	t := b.briefed(2091, "waiting-human", factoryd.SourceRoutine, 4*time.Hour, factoryd.RoutineCard(weeklyBump,
		"Bump every pinned outside input to its newest release in one pull request: about 60 pins of image digests, the Debian snapshot, Go, the tools, buildkit and vulndb. "+
			"Read each from its upstream host, update its sha256 and version, and run the reproducible build."))
	t.ID, t.Author, t.AuthorWriter, t.SpentEUR, t.PRNumber = "t-91c0b42e7d35", "factory-agent", false, 0.01, 0
	f := facts(t, "")
	in := factoryd.TripInput{Reason: factoryd.AgentAsks + "The weekly bump needs network access to the upstream registries and file hosts (docker.io, quay.io, ghcr.io, gcr.io, registry.k8s.io, deb.debian.org, snapshot.debian.org, go.dev, github.com, objects.githubusercontent.com), " +
		"none of which resolve in this sandbox. How should the new digests, sha256 sums and versions be obtained?",
		Choices: []string{"Open the upstream hosts to this sandbox", "Provide the new pins; I apply and test them", "Refile the card to run where registry access exists"}}
	up := &factoryd.HandUp{Action: factoryd.ActHandover, Rung: 2, ActionRung: 2,
		Grounds: "The stop is real and structural: only you hold the sandbox's network policy. No model change, replan or new session can fetch a digest this DNS refuses.",
		Found:   "every fetch of the ten hosts failed with \"no such host\" — the sandbox's egress allows the Go proxy and npm alone. The weekly car…"}
	return b.stop(group, name, 5850, t, f, in, up, 31*time.Minute)
}

// flag is a monitor flag's case on its card, as the digest raises it and
// the monitor opens it: a wait's flag keyed on the wait's kind and start,
// with its bound, any other on its detail.
func (b *builder) flag(group, name, flag string) Fixture {
	n := b.next()
	fc := flagCards[flag]
	t := b.issue(n, cmp.Or(fc.status, "running"), 6*time.Hour, fc.title, fc.body)
	f := facts(t, "")
	state, extra := fc.detail, map[string]any{}
	if fc.wait != "" {
		since := b.now.Add(-fc.since).UTC().Format(time.RFC3339)
		state = fc.wait + "|" + since
		extra = map[string]any{"wait": fc.wait, "owner_rung": 0, "since": since, "bound": factoryd.DurationText(policy.DefaultWaits.Bound(fc.wait))}
	}
	extra["flag"], extra["detail"], extra["state"] = flag, fc.detail, state
	in := factoryd.TripInput{Reason: factoryd.MonitorFlagReason(factoryd.DigestFlag{Name: flag, Detail: fc.detail}), Kind: factoryd.CaseMonitorFlag,
		StateKey: flag + "|" + state, Extra: extra}
	return b.stop(group, name, n*3, t, f, in, nil, 20*time.Minute)
}

// flagCard is a monitor flag's card, in status ("" running), and the
// detail the digest raises the flag with; a wait's flag names the wait and
// how long ago it started.
type flagCard struct {
	title, body, status, detail, wait string
	since                             time.Duration
}

// flagCards hold each flag's detail as the digest writes it.
var flagCards = map[string]flagCard{
	"prover_refused_thrice": {title: "Write the blind proofs of plan #1955", body: "One hidden test per criterion of plan #1955, on main.", detail: "confirm_view_layered"},
	"grant_bought_nothing": {title: "Make the budget floor ask for the price the next session needs", body: "Make the budget floor's ask name the price the next opening needs.",
		detail: "session 3"},
	"session_no_push": {title: "Move the second group of pg tests into internal/factoryd/pg", body: "Move the Postgres-backed tests of the second group into their own package.",
		detail: "session 2"},
	"checkin_cut": {title: "Make a chat answer settle the question it answers", body: "A chat answer on an ask settles the case it answers.", detail: factoryd.OffsetText(4*time.Hour + 12*time.Minute)},
	"finding_confirmed_twice": {title: "The heal pass re-asks a card parked with no live ask", body: "The heal pass re-asks a parked card whose ask ended.",
		detail: factoryd.FindingID("the heal pass skips a card whose ask was withdrawn")},
	"checkin_refused": {title: "Pause and re-queue: a human and chat control", body: "A human pauses a card and re-queues it from the board or the chat.",
		detail: factoryd.OffsetText(2*time.Hour + 5*time.Minute)},
	"opened_proof_red_twice": {title: "The split receipt suites run at the proofs' head", body: "Run the receipt suites at the proofs' head.", detail: "receipt_holds"},
	"opened_proof_changed": {title: "Land the assessments test an earlier card wrote", body: "Land the assessments proof from card/1885.",
		detail: "assessments_hold proofs/assessments_test.go"},
	"criterion_unproven": {title: "Acceptance: every criterion of the plan holds on the landed cards", body: "Prove every criterion of plan #1947 on main.",
		detail: "confirm_view_layered"},
	"answer_changed_nothing": {title: "A cap ask answered emptily never parks the card", body: "An empty answer at a cap leaves the card waiting.",
		detail: "review_refused_not_run|4d03448cf2375db97e347e22d221cba60156cdb2"},
	"issue_open_after_end": {title: "Export the next measured slice of the pg list", body: "Export the next slice of the pg list (chain card 4 of 8).", status: "failed",
		detail: string(factoryd.CaseSessionCap)},
	"dependency_failed": {title: "Run every proof and every suite row on the split packages", body: "Run every proof on the split packages, and close the plan.", detail: "#2059"},
	"proofs_missing": {title: "The plan's hand-derived tests as acceptance tests", body: "Write the plan's hand-derived tests under acceptance/.",
		detail: textutil.Plural(3, "hour", "hours")},
	"budget_moved_no_event": {title: "Briefs, gate retries and check-ins schedule at the reset", body: "Schedule the briefs, the gate retries and the check-ins at the provider's reset.",
		detail: "2.70 EUR not 2.00 EUR"},
	"wait_over_bound": {title: "Rebuild the wall-clock stop test so the real session stops at the cap", body: "Rebuild the wall-clock stop test on the merged rig.",
		wait: "run_quiet", since: 41 * time.Minute, detail: "run_quiet since " + factoryd.OffsetText(5*time.Hour+19*time.Minute)},
	"queued_over_bound": {title: "One freeze card speaks for the whole class and carries the retry switch", body: "File one freeze card for a frozen class.", status: "queued",
		wait: "queued", since: 2*time.Hour + 10*time.Minute, detail: factoryd.WorkBoundReason + "4 of 4 for " + factoryd.DurationText(2*time.Hour+10*time.Minute)},
	"review_refused_not_run": {title: "Read the provider's limit in the digest and split the frozen clock on the face", body: "Read provider_limit in the digest and split the frozen clock on the face.",
		status: "in-review", detail: "review: behaviours: model review answer invalid (json: unknown field \"behaviors\"): {\"behaviors\": [\"policy.ReadOpening windows on…"},
}

func (b *builder) flags() {
	for _, flag := range factoryd.MonitorFlags() {
		if _, ok := flagCards[flag]; ok {
			b.out = append(b.out, b.flag(GroupFlag, flag, flag))
		}
	}
}

// merge is a card in review whose merge approval pends: the request the
// gates filed, its why_human lines AutoMerge's, and the case it opens
// (RequestCase).
func (b *builder) merge(group, name string) Fixture {
	n := b.next()
	t := b.issue(n, "in-review", 3*time.Hour, "Chat proposals survive a restart: keep them in Postgres",
		"The proposals live in the console's memory, so a restart between a proposal and its Do it loses it. Store them in a table with their expiry and purge the old ones in the settlement pass.")
	t.AgentSummary = "Merge it: the migration only adds a table and an index, and the acceptance test restarts the console between a proposal and its Do it."
	review := factoryd.Review{Verdict: "approve", Summary: "Moves the proposals into a table with an expiry index; the settlement pass purges them.",
		Findings: []string{"chat.go: the purge deletes without a batch limit; fine at today's volume"}}
	gates := factoryd.GateResults{ChecksPassed: true, Policy: factoryd.PolicyGate{Class: policy.ClassHigh, Outcome: policy.OutcomeRequiresApproval,
		MergeRule: policy.MergeHuman, Migrations: []string{"internal/factoryd/migrations/0031_chat_proposals.sql"}, Lines: 212, LargeLines: 800}}
	_, _, unmet := factoryd.AutoMerge(gates, review, t.Source)
	gates.Audit = &factoryd.AuditResult{Verdict: "findings", Model: "big", Deployment: "zai/glm-5.3", Findings: []factoryd.AuditFinding{{Item: "token stored in clear",
		File: "internal/factoryd/chat.go", Line: 268, Why: "the proposal row keeps its token unhashed; a database reader could replay a Do it within its 30 minutes"}}}
	in := factoryd.ApprovalInput{PR: factoryd.PullRequest{Number: t.PRNumber, URL: fmt.Sprintf("https://git.example/sf/code/pulls/%d", t.PRNumber)},
		Commit: "4be1c07e2a9f63d1b0c58e7a2f4d9c31a6b8e054", Gates: gates, Review: review, WhyHuman: unmet}
	reason, spec := factoryd.MergeRequest(t, in, nil, 0)
	a := &factoryd.ApprovalRequest{ID: n, Kind: factoryd.ApprovalMerge, TaskID: t.ID, Project: Repository, PRNumber: t.PRNumber, Status: "pending",
		Reason: reason, Spec: marshal(spec), CreatedAt: b.now.Add(-25 * time.Minute)}
	return b.request(group, name, t, a, 25*time.Minute)
}

// request is card t holding the case its pending request a opens.
func (b *builder) request(group, name string, t factoryd.Task, a *factoryd.ApprovalRequest, age time.Duration) Fixture {
	opened, err := factoryd.RequestCase(*a)
	if err != nil {
		panic(err)
	}
	c := b.atHuman(opened, 7000+a.ID, age)
	return Fixture{Group: group, Kind: name, Task: t, Facts: factoryd.FactsOf(*c), Case: c, Approval: a}
}

// planAsk is a plan card waiting on its revision's open question, or on its
// confirmation (ConfirmationQuestion), as the planner asks it.
func (b *builder) planAsk(group, name string, confirm bool) Fixture {
	n := b.next()
	t := b.issue(n, "waiting-human", 64*time.Minute, "Console: a project's activity feed",
		"Show each project's events as a feed beside the board: who did what, by which way, when.\nThe feed reads the events table; no new store.\n"+
			"A writer filters it by card and by kind.\nIt pages back 500 rows at most.\nAcceptance: the feed shows a steer within a second of its row.")
	t.Track, t.PlanName, t.BudgetEUR, t.SpentEUR, t.PRNumber = policy.TrackPlan, "activity feed", 5, 0.38, 0
	p := &factoryd.Plan{Summary: "Four cards: the feed's read model first, then the page and the filters side by side, then the live rows.",
		Spec: &factoryd.Spec{Outcome: "A writer opens the feed beside the board and sees who did what, by which way and when, with a steer showing within a second of its row."},
		Criteria: []factoryd.PlanCriterion{{ID: "steer_within_a_second", Kind: factoryd.CriterionTest, Claim: "A steer shows in the feed within a second of its row.",
			Suite: "./internal/console", Name: "TestFeedShowsASteerAtOnce"}},
		Cards: []factoryd.PlanCard{
			{Key: "feed-read", Role: factoryd.RoleFoundation, Title: "Events: a paged read of a project's feed", Size: "small"},
			{Key: "feed-page", Role: factoryd.RoleFeature, Title: "Console: the feed page beside the board", Size: "medium", DependsOn: []string{"feed-read"}},
			{Key: "feed-filters", Role: factoryd.RoleFeature, Title: "Feed filters: by card and by kind", Size: "small", DependsOn: []string{"feed-read"}},
			{Key: "feed-live", Role: factoryd.RoleIntegration, Title: "Live rows: the stream appends to the feed", Size: "medium", DependsOn: []string{"feed-page", "feed-filters"}},
		}}
	q := factoryd.PlanQuestion{Text: "How far back does the feed page by default?", Choices: []string{"One day", "One week", "The 500 newest rows"},
		Recommended: "The 500 newest rows", Grounds: "the feed reads one table, and a row bound keeps its page the same size on a quiet and a busy day"}
	row := factoryd.PlanQuestionRow(q, 1, 1)
	if confirm {
		q = factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: factoryd.ConfirmationQuestion(1, *p)}
		row = factoryd.ConfirmationRow(q, 1, 1)
	}
	p.Questions = []factoryd.PlanQuestion{q}
	e := factoryd.Event{TaskID: t.ID, Kind: factoryd.EventQuestionAsked, Detail: roundTrip(row)}
	opened, err := factoryd.CaseFromAsk(t, e, "")
	if err != nil {
		panic(err)
	}
	c := b.atHuman(opened, 500, 12*time.Minute)
	a := &factoryd.ApprovalRequest{ID: n, Kind: factoryd.ApprovalPlan, TaskID: t.ID, Project: Repository, Status: "pending",
		Reason: factoryd.PlanApprovalReason(1, t.IssueNumber, len(p.Cards), len(p.Questions)), Spec: marshal(factoryd.PlanSpec{Revision: 1, SHA256: PlanSHA}),
		CreatedAt: b.now.Add(-12 * time.Minute)}
	return Fixture{Group: group, Kind: name, Task: t, Facts: factoryd.FactsOf(*c), Case: c, Ask: &factoryd.CaseAsk{At: c.OpenedAt, Detail: e.Detail}, Approval: a, Plan: p}
}

// criteriaGap is a done plan whose cards ran out with its criteria unmet:
// its criteria request (CriteriaGapReason) and the case it opens.
func (b *builder) criteriaGap(group, name string) Fixture {
	n := b.next()
	t := b.issue(n, "done", 30*time.Hour, "Metrics: divide and conquer", "Twelve panels on the metrics page.")
	t.Track, t.PlanName, t.PRNumber, t.MergeCommit = policy.TrackPlan, "divide and conquer", 0, ""
	p := &factoryd.Plan{Summary: "Two cards: the read model, then the page.",
		Criteria: []factoryd.PlanCriterion{
			{ID: "size_bound_escape", Kind: factoryd.CriterionTest, Claim: "the panel counts the cards over their size bound",
				Inputs: "a card over its bound, one unsized and one with no bound", Expected: "1 over, the unsized card counted as large"},
			{ID: "window_and_existing_figures", Kind: factoryd.CriterionTest, Claim: "the new panels keep the page's window and its old figures",
				Inputs: "GET /metrics for the last week", Expected: "twelve panels and the old figures unchanged"},
			{ID: "ledger_retry_integrity", Kind: factoryd.CriterionTest, Claim: "a retried session spends once in the ledger",
				Inputs: "one session answered twice", Expected: "one spend row"},
		},
		Cards: []factoryd.PlanCard{
			{Key: "foundation", Role: factoryd.RoleFoundation, Title: "Metrics: the panels' read model", Size: "medium"},
			{Key: "integration", Role: factoryd.RoleIntegration, Title: "Metrics: the panels on the page", Size: "medium", DependsOn: []string{"foundation"}},
		}}
	var ids []string
	for _, k := range p.Criteria {
		ids = append(ids, k.ID)
	}
	a := &factoryd.ApprovalRequest{ID: n, Kind: factoryd.ApprovalCriteria, TaskID: t.ID, Project: Repository, Status: "pending",
		Reason: factoryd.CriteriaGapReason(ids), Spec: marshal(factoryd.CriteriaSpec{Task: TaskID(n + 1)}), CreatedAt: b.now.Add(-3 * time.Minute)}
	fx := b.request(group, name, t, a, 3*time.Minute)
	fx.Plan = p
	return fx
}

// card1850 is plan #1850 as the board read it, its texts as captured: its
// last card closed without a merge, so no test judged its five criteria.
func (b *builder) card1850(group, name string) Fixture {
	t := b.card(1850, "done", factoryd.SourceWebhook, 3*24*time.Hour, liveText(1850))
	t.ID, t.Track, t.PlanName, t.PRNumber, t.MergeCommit, t.BudgetEUR, t.SpentEUR = "t-92f610a3a9bf", policy.TrackPlan, "one face for every card", 0, "", 6.69, 2.51
	p := &factoryd.Plan{
		Summary: "Revision 5, cut after #1904's third pod eviction ate its unpushed work; the spec and glossary are the humans' confirmed ones and stand unchanged, and the three test criteria file under new ids that name their revision-4 originals in replaces.",
		Criteria: []factoryd.PlanCriterion{
			{ID: "one_face_every_asking_kind_suite", Kind: factoryd.CriterionTest, Name: "TestOneFaceEveryAskingKind", Suite: "acceptance",
				Claim: "Every asking face — merge, resource, escalation, plan — rendered on the board from its fixture shows exactly one decide block built from the shared case-decide partial in the five-part shape (the ask with its numbers, the why, what the call needs, the recommendation with its source, premade answers as buttons with one-line effects), with exactly one fund control and one note box, and one send whose POST delivers the chosen answer, the fund amount and the note together."},
			{ID: "plain_cards_whisper", Kind: factoryd.CriterionJudgement,
				Claim: "Plain cards whisper: on every plain card kind the spend stays muted below 80 percent of budget and turns prominent only from 80 percent (holds counted), no element carries meaning by colour alone, and normal states sit at low typographic weight — while the asking faces' amber band and carousel (C44) and the criteria card's criterion rows (C45) stand unchanged."},
			{ID: "plan_card_audit_closes_gaps", Kind: factoryd.CriterionJudgement,
				Claim: "After #1837's view-rebuild cards merge, the plan card's face is audited against what that rebuild landed, and every gap the audit finds is closed on main or carries a reason a human can check."},
			{ID: "bound_ask_button_only_suite", Kind: factoryd.CriterionTest, Name: "TestBoundAskAnswersByItsButtonAlone", Suite: "acceptance",
				Claim: "A bound ask in the chat is answered by its button alone: while it stands open, a typed message into the chat delivers the message but resolves nothing — the ask stays open with its binding intact — and the button's press settles the ask and runs the bound tool call exactly once."},
			{ID: "face_conformance_pinned_suite", Kind: factoryd.CriterionTest, Name: "TestFaceConformancePinned", Suite: "acceptance",
				Claim: "One conformance test renders every card kind the board can show from fixtures and pins the face contract per kind, so a kind the board can render but the test's registry does not cover fails the run."},
		},
		Cards: []factoryd.PlanCard{
			{Key: "decide-partial", Role: factoryd.RoleFoundation, Title: "The shared decide partial: case-decide becomes every asking face's block", Size: "medium"},
			{Key: "asking-face-fix", Role: factoryd.RoleFeature, Title: "The asking face asks: merge and resource decide blocks carry their own five parts", Size: "medium", DependsOn: []string{"decide-partial"}},
			{Key: "face-conformance-write", Role: factoryd.RoleFeature, Title: "Write the face-conformance pin: land #1904's branch and complete the record's four tests", Size: "large",
				DependsOn: []string{"decide-partial", "asking-face-fix"}},
			{Key: "face-conformance-green", Role: factoryd.RoleIntegration, Title: "The face contract green on every kind: close what the conformance run names", Size: "medium",
				DependsOn: []string{"decide-partial", "asking-face-fix", "face-conformance-write"}},
		}}
	a := &factoryd.ApprovalRequest{ID: 1302, Kind: factoryd.ApprovalCriteriaRerun, TaskID: t.ID, Project: Repository, Status: "pending",
		Reason: "criteria never judged: the criteria gate has not settled one_face_every_asking_kind_suite, plain_cards_whisper, plan_card_audit_closes_gaps, bound_ask_button_only_suite, face_conformance_pinned_suite",
		Spec:   marshal(factoryd.CriteriaSpec{Task: "t-20d760a69088"}), CreatedAt: b.now.Add(-9 * time.Hour)}
	fx := b.request(group, name, t, a, 9*time.Hour)
	fx.Case.ID, fx.Plan = 5675, p
	return fx
}

// resource is a running card whose resource request pends: beyond the
// project's envelope by its lease (EnvelopeViolations), its reason
// cloudres's.
func (b *builder) resource(group, name string) Fixture {
	n := b.next()
	t := b.issue(n, "running", 90*time.Minute, "Load test: the event stream under 200 followers",
		"Open 200 followers on one project's stream and measure the heartbeat's board reads.")
	env := policy.Envelope{MonthlyEUR: 20, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 2, MaxLeaseHours: 4}
	var beyond, kinds []string
	for _, v := range cloudres.EnvelopeViolations(env, cloudres.EnvelopeUse{OpenServers: 1, MonthEUR: 0.43}, "cx33", 300, 0, 0.072) {
		beyond, kinds = append(beyond, v.Text), append(kinds, v.Kind)
	}
	purpose := "load test of the event stream"
	spec := marshal(cloudres.ResourceSpec{ServerType: "cx33", Purpose: purpose, LeaseMinutes: 300, Location: "fsn1",
		HourlyEUR: 0.0144, ReservationEUR: 0.072, BeyondEnvelope: beyond, BeyondKinds: kinds})
	monthly := 8.99
	a := &factoryd.ApprovalRequest{ID: n, Kind: factoryd.ApprovalResource, TaskID: t.ID, Project: Repository, Status: "pending", MonthlyEUR: &monthly,
		Reason: cloudres.RequestReason(purpose, beyond), Spec: spec, CreatedAt: b.now.Add(-38 * time.Minute)}
	return Fixture{Group: group, Kind: name, Task: t, Facts: facts(t, ""), Approval: a}
}

func (b *builder) approvals() {
	for _, kind := range factoryd.ApprovalKinds() {
		switch kind {
		case factoryd.ApprovalMerge:
			b.out = append(b.out, b.merge(GroupApproval, kind))
		case factoryd.ApprovalResource:
			b.out = append(b.out, b.resource(GroupApproval, kind))
		case factoryd.ApprovalPlan:
			b.out = append(b.out, b.planAsk(GroupApproval, kind, true))
		case factoryd.ApprovalCriteria:
			b.out = append(b.out, b.criteriaGap(GroupApproval, kind))
		case factoryd.ApprovalCriteriaRerun:
			b.out = append(b.out, b.card1850(GroupApproval, kind))
		}
	}
}

// actions are, for each action a human may pick, a card whose face offers
// it: a case whose facts its predicate holds on.
func (b *builder) actions() {
	stopOn := func(name string, kind factoryd.CaseKind) Fixture { return b.agentCase(GroupAction, name, kind, nil) }
	for _, name := range factoryd.HumanActions() {
		var fx Fixture
		switch name {
		case factoryd.ActResume:
			fx = stopOn(name, factoryd.CaseProviderRefused)
		case factoryd.ActContinue, factoryd.ActFileCard:
			fx = b.infoRequest(GroupAction, name, nil)
		case factoryd.ActHandover, factoryd.ActReverify, factoryd.ActSuccessor, factoryd.ActReplan:
			fx = stopOn(name, factoryd.CaseGateStop)
		case factoryd.ActRerunChecks:
			fx = b.agentCase(GroupAction, name, factoryd.CaseGateStop, func(_ *factoryd.Task, f *factoryd.CaseFacts) { f.Checks = "red" })
		case factoryd.ActWaitChecks:
			fx = b.waitChecks(name)
		case factoryd.ActConfirmAudit, factoryd.ActFund:
			fx = b.card2004(GroupAction, name)
		case factoryd.ActExtend, factoryd.ActRaiseCeiling:
			fx = stopOn(name, factoryd.CaseSessionCap)
		case factoryd.ActRetry:
			fx = stopOn(name, factoryd.CaseDependencyFailed)
		case factoryd.ActClose, factoryd.ActNote:
			fx = stopOn(name, factoryd.CaseOther)
		case factoryd.ActApproveMerge, factoryd.ActDenyMerge:
			fx = b.merge(GroupAction, name)
		case factoryd.ActSetDependency:
			fx = stopOn(name, factoryd.CaseDependencyClosed)
		case factoryd.ActRemoveDependency:
			fx = b.agentCase(GroupAction, name, factoryd.CaseDependencyClosed, func(_ *factoryd.Task, f *factoryd.CaseFacts) {
				f.Dependencies = append(f.Dependencies, factoryd.DepFact{TaskID: TaskID(2190), Issue: 2190, Status: "running"})
			})
		case factoryd.ActRestartPlan:
			fx = b.agentCase(GroupAction, name, factoryd.CaseFindingsRepeated, func(t *factoryd.Task, f *factoryd.CaseFacts) {
				t.Source, t.Parent, t.PlanKey = factoryd.SourcePlan, plan1947ID, "reconciled-head-merge"
				f.Plan, f.PlanIssue, f.PlanName, f.PlanStatus = plan1947ID, 1947, "empty commit fires gate", "done"
			})
		case factoryd.ActAnswerCriteria, factoryd.ActGrantCriteria, factoryd.ActConfirmCriteria, factoryd.ActDenyCriteria:
			fx = b.criteriaGap(GroupAction, name)
		case factoryd.ActDeployNow:
			fx = humanSourceOf(factoryd.SourceProductionHeld).file(b, GroupAction, name, b.next())
		case factoryd.ActSettle:
			fx = b.flag(GroupAction, name, "queued_over_bound")
		case factoryd.ActSalvage:
			fx = b.flag(GroupAction, name, "wait_over_bound")
		case factoryd.ActHandBack:
			fx = b.card2101(GroupAction, name)
		default:
			continue // the console's gallery test names the action with no fixture
		}
		b.out = append(b.out, fx)
	}
}

// waitChecks is the gate supervisor's stop of a checks wait past its bound.
func (b *builder) waitChecks(name string) Fixture {
	n := b.next()
	t := b.issue(n, "waiting-human", 5*time.Hour, "Pulse: recompute on a card's status change",
		"The pulse recomputes every ten minutes; recompute on each task_status row, at most once a minute.")
	f := facts(t, "")
	f.Checks = "pending"
	in := factoryd.TripInput{Reason: factoryd.GateStopReason(factoryd.GateTrip{Activity: factoryd.ActivityWaitChecks, Attempt: 1, Bound: 40 * time.Minute},
		factoryd.GateDecision{Diagnosis: "the checks on 4be1c07 are still pending: the runner queue holds two jobs"})}
	return b.stop(GroupAction, name, n*3, t, f, in, nil, 45*time.Minute)
}

// cards are the card kinds of the census, each kind of card by its name,
// as its filer files it.
func (b *builder) cards() {
	agent := func(name, status, source string, objective func(n int64) factoryd.Task) {
		t := objective(b.next())
		t.Status = status
		b.out = append(b.out, b.onParent(Fixture{Group: GroupCard, Kind: name, Task: t, Facts: facts(t, "")}))
	}
	sourced := func(source string) func(n int64) factoryd.Task {
		i := slices.IndexFunc(agentSources, func(s agentSource) bool { return s.name == source })
		if i < 0 {
			panic("facefixtures: no agent source " + source)
		}
		return func(n int64) factoryd.Task { return agentSources[i].card(b, n, "queued") }
	}
	agent("direct work card", "running", factoryd.SourceWebhook, func(n int64) factoryd.Task {
		return b.issue(n, "running", 4*time.Hour, "Redo the census fixes on current main: PR #1909 went stale under the face work",
			"PR #1909 went stale while the face work landed. Redo its census fixes on current main and open a new pull request.")
	})
	// #2100 as the board read it, its objective as captured, filed before
	// cards had a why line and a summary
	t2100 := b.planChild(2100, "queued", "narrow-doclose-409", 75*time.Minute)
	t2100.ID, t2100.Objective, t2100.CeilingEUR, t2100.Why, t2100.Summary = "t-d913f819b959", liveText(2100), 18, "", ""
	b.out = append(b.out, Fixture{Group: GroupCard, Kind: "plan child: foundation", Task: t2100, Facts: facts(t2100, "")})
	feature := b.planChild(b.next(), "running", "drawer-files", 26*time.Hour)
	b.out = append(b.out, Fixture{Group: GroupCard, Kind: "plan child: feature", Task: feature, Facts: facts(feature, "")})
	integration := b.planChild(2102, "queued", "reconciled-head-merge", 75*time.Minute)
	b.out = append(b.out, Fixture{Group: GroupCard, Kind: "plan child: integration", Task: integration, Facts: facts(integration, "")})
	delta := b.planChild(b.next(), "queued", "delta-1", 50*time.Minute)
	b.out = append(b.out, Fixture{Group: GroupCard, Kind: "delta card", Task: delta, Facts: facts(delta, "")})

	agent("retry replacement", "queued", factoryd.SourceWebhook, func(n int64) factoryd.Task {
		old := b.oldCard()
		t := b.card(n, "queued", old.Source, 2*time.Hour, factoryd.RetryObjective(old, failedWhy, same))
		t.ID, t.Parent, t.PlanKey, t.Size, t.Why, t.Summary = factoryd.RetryID(old.ID), old.Parent, old.PlanKey, old.Size, old.Why, old.Summary
		return t
	})
	agent("successor", "queued", factoryd.SourceSuccessor, sourced(factoryd.SourceSuccessor))
	agent("delegated card", "queued", factoryd.SourceDelegate, sourced(factoryd.SourceDelegate))
	for _, s := range []string{factoryd.SourceScreening, factoryd.SourceSizing, factoryd.SourceProver} {
		card := helper(s)
		agent("helper task: "+s, "done", s, func(n int64) factoryd.Task { return card(b, n, "done") })
	}
	agent("tool request", "queued", policy.SourceToolRequest, sourced(policy.SourceToolRequest))
	agent("roll", "done", factoryd.SourceRoll, sourced(factoryd.SourceRoll))
	agent("main fix", "running", factoryd.SourceMainFix, sourced(factoryd.SourceMainFix))
	agent("revert", "running", factoryd.SourceRevert, sourced(factoryd.SourceRevert))
	agent("group revert", "queued", factoryd.SourceGroupRevert, sourced(factoryd.SourceGroupRevert))
	agent("chain repair (agent)", "running", factoryd.SourceChainRepair, sourced(factoryd.SourceChainRepair))
	agent("alert (agent)", "failed", factoryd.SourceAlert, sourced(factoryd.SourceAlert))
	agent("routine (agent)", "queued", factoryd.SourceRoutine, sourced(factoryd.SourceRoutine))
	agent("template update", "in-review", factoryd.SourceTemplateUpdate, sourced(factoryd.SourceTemplateUpdate))
	agent("schema copy", "in-review", factoryd.SourceSchemaCopy, sourced(factoryd.SourceSchemaCopy))
	agent("GitHub sync", "in-review", factoryd.SourceGitHubSync, sourced(factoryd.SourceGitHubSync))
	agent("image removal", "queued", factoryd.SourceImageRemoval, sourced(factoryd.SourceImageRemoval))

	b.out = append(b.out, b.humanIssue(GroupCard, "human issue", b.next(), humanIssueTitle, humanIssueBody,
		factoryd.IssueQuestion(humanIssueTitle, humanIssueBody, "alice", true), 2*time.Hour))
	reproTitle, reproBody := "Gateway: the 429 retry loses the request id", "Retrying a 429 drops the provider's request id from the log.\n\n```repro\ncd /tmp && go test ./internal/gateway\n```"
	b.out = append(b.out, b.humanIssue(GroupCard, "reproduction-rule card", b.next(), reproTitle, reproBody,
		factoryd.ReproQuestion("alice", factoryd.ValidateReproCommand(factoryd.ReproCommand(reproBody))), 2*time.Hour))
	b.out = append(b.out, b.card2101(GroupCard, "workflow-files card"))
	movedTitle, movedBody := "Board export: keep the nightly snapshots", "Write the board to a JSON snapshot every night so a lost database still leaves the cards. Keep 30 snapshots."
	b.out = append(b.out, b.humanIssue(GroupCard, "card moved to a human", b.next(), movedTitle, movedBody, factoryd.MovedQuestion(movedTitle, movedBody, "sebastian"), 2*time.Hour))
	pr := b.filedHuman(GroupCard, "factory pull request on a project without agents", b.next(), factoryd.SourceTemplateUpdate, "",
		factoryd.FactoryPRQuestion(factoryd.TemplateUpdateCard("go-service", 14, ".forgejo/workflows/ci.yml, Dockerfile"), 14, "https://git.example/sf/code/pulls/14"), 2*time.Hour)
	pr.Task.PRNumber = 14
	b.out = append(b.out, pr)
	b.out = append(b.out, humanSourceOf(factoryd.SourceChat).file(b, GroupCard, "chat escalation", b.next()))
	b.out = append(b.out, b.escalated(GroupCard, "alert (human)", b.next(), factoryd.SourceAlert, "factoryd",
		factoryd.AlertQuestion(fiveXX, Project, "factory", factoryd.AlertHumanWhy(factoryd.Project{Agents: policy.AgentsNone}, fiveXX),
			factoryd.AlertIssue(fiveXX, Project, "factory", nil).Body), nil, 2*time.Hour))
	b.out = append(b.out, humanSourceOf(factoryd.SourceChainStall).file(b, GroupCard, "chain stall", b.next()))
	b.out = append(b.out, b.filedHuman(GroupCard, "chain repair (human)", b.next(), factoryd.SourceChainRepair, "",
		factoryd.RepairQuestion(Repository, chainSHA, []string{factoryd.RequiredContextLine("deploy/staging", "failure")}, "https://git.example/sf/code/actions/runs/88", nil), 2*time.Hour))
	for _, s := range []struct{ name, source string }{
		{"roll refused", factoryd.SourceRollRefused}, {"roll failed", factoryd.SourceRollFailed}, {"rerun", factoryd.SourceRerun},
		{"still red", factoryd.SourceStillRed}, {"config rejected", factoryd.SourceConfig}, {"month cap", factoryd.SourceMonthCap},
		{"judgment budget", factoryd.SourceChatBudget}, {"production held", factoryd.SourceProductionHeld},
	} {
		b.out = append(b.out, humanSourceOf(s.source).file(b, GroupCard, s.name, b.next()))
	}
	digest := policy.Routine{Name: "needs-you-digest", Title: "Answer every card that waited in Needs you over a day", Files: policy.FilesHuman, Schedule: []policy.Trigger{{Cron: "0 8 * * 1"}}}
	b.out = append(b.out, b.filedHuman(GroupCard, "routine for a human", b.next(), factoryd.SourceRoutine, factoryd.RoutineDecider(digest.Name, 9, digest.Words()),
		factoryd.RoutineQuestion(digest, "Read every card in Needs you older than a day and answer it or close it.", factoryd.RoutineProvenanceLine(digest.Name, 9), true), 2*time.Hour))
}

// card2101 is #2101 as the plan files it now: a plan's card for a human
// whose files sit under a workflow directory, its title the instruction
// the planner writes and its steps as the board read them.
func (b *builder) card2101(group, name string) Fixture {
	_, steps, _ := strings.Cut(liveText(2101), "\n\n")
	c := plan1947Cards[slices.IndexFunc(plan1947Cards, func(c factoryd.PlanCard) bool { return c.Key == "manifests-name-the-proofs" })]
	t := factoryd.Task{ID: "t-d23e2a8d3419", Kind: "human", Status: "waiting-human", Repository: Repository, IssueNumber: 2101, Source: factoryd.SourcePlan,
		Objective: factoryd.IssueObjective(c.Title, steps), Why: c.Why, Summary: c.Summary, Parent: plan1947ID, PlanKey: c.Key, Author: "controller",
		RiskClass: "standard", BudgetEUR: 4, CreatedAt: b.now.Add(-75 * time.Minute)}
	plan := factoryd.Task{ID: plan1947ID, IssueNumber: 1947, PlanName: "empty commit fires gate"}
	fx := b.asked(group, name, t, factoryd.PlanHumanAsk(plan, c.Title), 75*time.Minute)
	fx.Case.ID = 5848
	return fx
}
