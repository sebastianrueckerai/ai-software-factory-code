package factoryd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The metrics page's figures (roadmap 147): every number is a query over
// the hard data the factory already stores — no LLM output, no stored
// summaries, one row per event. Every figure reads the window, the
// equal-length window before it, and seven weekly spark buckets that end
// where the window does; a fact before the spark start belongs to none.

// Figure is one deck number: the window value, the previous window's,
// and the spark. Has false means the cohort is empty — the page renders
// — instead of a number.
type Figure struct {
	Value   float64    `json:"value"`
	Prev    float64    `json:"prev"`
	Has     bool       `json:"has"`
	PrevHas bool       `json:"prev_has"`
	Weeks   [7]float64 `json:"weeks"`
}

// RevertedRefiled splits the window's reverted cards into bare reverts
// and refiles (an alert filed the fix at the revert); Prev is
// [reverted, refiled], Weeks counts reverts per bucket.
type RevertedRefiled struct {
	Reverted float64    `json:"reverted"`
	Refiled  float64    `json:"refiled"`
	Prev     [2]float64 `json:"prev"`
	Has      bool       `json:"has"`
	Weeks    [7]float64 `json:"weeks"`
}

// EscalationsHandled splits the window's escalations by who judged them:
// a human, an AI (the project agent deciding for the member at the
// autonomous level, or a member the policy lists in ai_members), or no
// one, closed unjudged when their card ended. Prev is [human, ai,
// unjudged], Weeks counts the human's per bucket.
type EscalationsHandled struct {
	Human    float64    `json:"human"`
	AI       float64    `json:"ai"`
	Unjudged float64    `json:"unjudged"`
	Prev     [3]float64 `json:"prev"`
	Weeks    [7]float64 `json:"weeks"`
}

// MixShare is one class's, deployment's or tool's share of the window's
// euros, 0..100.
type MixShare struct {
	Alias string  `json:"alias"`
	Share float64 `json:"share"`
}

// DiffSize is the median diff of shipped cards; Prev is
// [files, insertions, deletions], Weeks the median files per bucket.
type DiffSize struct {
	Files      float64    `json:"files"`
	Insertions float64    `json:"insertions"`
	Deletions  float64    `json:"deletions"`
	Prev       [3]float64 `json:"prev"`
	Has        bool       `json:"has"`
	PrevHas    bool       `json:"prev_has"`
	Weeks      [7]float64 `json:"weeks"`
}

// ChatWork is the project agent's tool work; Weeks counts calls.
type ChatWork struct {
	Calls       float64    `json:"calls"`
	Minutes     float64    `json:"minutes"`
	PrevCalls   float64    `json:"prev_calls"`
	PrevMinutes float64    `json:"prev_minutes"`
	Weeks       [7]float64 `json:"weeks"`
}

// SizeFigure is one card size's figure of a divide-and-conquer panel:
// the window value, the previous window's and the spark, keyed by the
// policy's card size (small, medium, large).
type SizeFigure struct {
	Size   string `json:"size"`
	Figure Figure `json:"figure"`
}

// NamedCount is one bucket of a typed distribution: how many of the
// window's things carry this name. Session ends name session outcomes,
// replans name replan origins (person, cut, round) and plans by rounds
// name each round as its number.
type NamedCount struct {
	Name  string  `json:"name"`
	Count float64 `json:"count"`
}

// SizeIntercept is one size's fixed euros per card: the intercept of an
// ordinary least squares of actual euros on changed lines across the
// window's merged cards of that size. Available false means the fit is
// undefined — too few cards, or changed lines without variance — never
// zero.
type SizeIntercept struct {
	Size      string  `json:"size"`
	EUR       float64 `json:"eur"`
	Available bool    `json:"available"`
}

// The divide-and-conquer fact contracts (roadmap 336): one payload per
// fact family, its identity in explicit fields. The identity is the
// persistence contract — one row or one timeline line per identity,
// whichever retry records it first — so a restart after a crash leaves
// one fact, not two, and loses none. The event payloads set
// Event.IdentityKey, unique where not null (migration
// 20261108120000_metrics_identities.sql); Record drops a repeat. The
// step payloads record through AppendFactOnce, which skips a step a
// line of its name with the same identity already holds.

// StepFact is a timeline fact's payload: the step's detail line and the
// identity that makes the fact one fact.
type StepFact interface {
	detail() map[string]any
	identity() map[string]any
}

// SessionEndFact is one session's end on a card, the session_end step:
// outcome is a SessionOutcome word, the panel's cause.
type SessionEndFact struct {
	Session string // with Attempt: the identity
	Attempt int
	Parent  string
	Model   string
	Outcome string
	Summary string
	Started time.Time
}

func (f SessionEndFact) detail() map[string]any {
	return map[string]any{"session": f.Session, "parent": f.Parent, "model": f.Model, "attempt": f.Attempt,
		"outcome": f.Outcome, "summary": f.Summary, "started": rfc3339(f.Started)}
}

func (f SessionEndFact) identity() map[string]any {
	return map[string]any{"session": f.Session, "attempt": f.Attempt}
}

// ReviewAttemptFact is one model review of one head, the model_review
// step: an empty Verdict is one the model never judged.
type ReviewAttemptFact struct {
	PR      int64
	Commit  string // with Round: the identity
	Round   int
	Model   string
	Verdict string
	Summary string
	Note    string
}

func (f ReviewAttemptFact) detail() map[string]any {
	return map[string]any{"pr": f.PR, "commit": f.Commit, "round": f.Round, "model": f.Model,
		"verdict": f.Verdict, "summary": f.Summary, "note": f.Note}
}

func (f ReviewAttemptFact) identity() map[string]any {
	return map[string]any{"commit": f.Commit, "round": f.Round}
}

// EventGateHead is one card's reading at one head — its gate decision,
// size and diff — recorded when a card reaches a gate: the merge gate's
// cards-above-bound panel reads it. Detail {"gate", "head", "decision",
// "size", "bound", "policy_version", "files", "insertions", "deletions"}.
const EventGateHead = "gate_head"

// GateHeadFact is that reading: the merge decision's snapshot of the card
// as the gates judged it — the card's size, the bound that size planned
// under the policy the gates read (cardLineBound, never the policy's
// large_lines standing in for a smaller size's bound), the policy version
// and the lines the gates counted. Decision is "auto" (the gates let it
// merge) or "approval" (a person decides). Identity: the card and the
// head; one head, one row.
type GateHeadFact struct {
	Task          string // with Head: the identity
	Head          string
	Gate          string
	Decision      string
	Size          string
	Bound         int // the size's effective cardLineBound at the reading
	PolicyVersion int64
	Files         int
	Insertions    int
	Deletions     int
}

// Event is the reading's row of repository's issue.
func (f GateHeadFact) Event(repository string, issue int64) Event {
	return Event{Repository: repository, TaskID: f.Task, IssueNumber: issue, Kind: EventGateHead,
		Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"gate": f.Gate, "head": f.Head, "decision": f.Decision, "size": f.Size,
			"bound": f.Bound, "policy_version": f.PolicyVersion,
			"files": f.Files, "insertions": f.Insertions, "deletions": f.Deletions},
		IdentityKey: identityKey(EventGateHead, f.Task, f.Head)}
}

// PlanFilingFact is one revision's filing on a plan — a person's, a
// cut's or a round's — the plan_replanned row. Identity: the plan and
// the revision; one revision, one row.
type PlanFilingFact struct {
	Plan     string // with Revision: the identity
	Revision int
	Origin   string // ReplanPerson, ReplanCut or "round": the panel's origin
}

// Event is the filing's row of repository's issue; extra carries what
// the filing names (the fates of the old cards and the new ones, or a
// round's cards).
func (f PlanFilingFact) Event(repository string, issue int64, extra map[string]any) Event {
	detail := map[string]any{"revision": f.Revision, "origin": f.Origin}
	maps.Copy(detail, extra)
	return Event{Repository: repository, TaskID: f.Plan, IssueNumber: issue, Kind: EventPlanReplanned,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail,
		IdentityKey: identityKey(EventPlanReplanned, f.Plan, fmt.Sprintf("%d", f.Revision))}
}

// HoldIntervalFact is one production hold's end, the production_released
// row; the releases row holds the open interval. Identity: the
// repository and the instant the hold began; one hold, one row.
type HoldIntervalFact struct {
	Repository string // with HeldSince: the identity
	HeldSince  time.Time
	How        string  // closed or deploy_now
	Hours      float64 // the wait the hold cost
}

// Event is the hold's end row.
func (f HoldIntervalFact) Event() Event {
	return Event{Repository: f.Repository, Kind: EventProductionReleased, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail:      map[string]any{"how": f.How, "held_since": rfc3339(f.HeldSince), "hours": f.Hours},
		IdentityKey: identityKey(EventProductionReleased, f.Repository, rfc3339(f.HeldSince))}
}

// identityKey is the events row's identity: the kind and the fields
// that make its fact one fact.
func identityKey(kind string, fields ...string) string {
	return strings.Join(append([]string{kind}, fields...), " ")
}

// AppendFactOnce appends fact's step unless the task's timeline holds a
// line of that name carrying the fact's identity, the check and the
// write under one lock; it tells whether it wrote. One identity, one
// line, whichever retry writes it first.
func (tl *Timeline) AppendFactOnce(ctx context.Context, taskID, step string, fact StepFact) (bool, error) {
	defer tl.lock(taskID)()
	return tl.append(ctx, taskID, step, fact.detail(), func(prev []byte) bool {
		return stepHoldsIdentity(prev, step, fact.identity())
	})
}

// stepHoldsIdentity tells whether the timeline holds a line of step
// carrying every identity key with its value.
func stepHoldsIdentity(timeline []byte, step string, identity map[string]any) bool {
	for line := range bytes.SplitSeq(timeline, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) != nil || m["step"] != step {
			continue
		}
		if holdsIdentity(m, identity) {
			return true
		}
	}
	return false
}

// holdsIdentity tells whether a detail line carries every identity key
// with its value.
func holdsIdentity(detail, identity map[string]any) bool {
	for k, want := range identity {
		got, ok := detail[k]
		if !ok || !sameJSONValue(got, want) {
			return false
		}
	}
	return true
}

// sameJSONValue tells whether two values encode the same JSON — a
// number equal across the kinds it unmarshals back as.
func sameJSONValue(a, b any) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ja, jb)
}

// DivideConquer measures the divide-and-conquer plan (roadmap 336, doc
// docs/plans/2026-10-29-divide-and-conquer.md): whether cards stay
// within their size bound, what one costs in sessions and euros, and
// how the plan's rounds and cuts behave. Durations are hours.
//
// Every panel reads hard rows the factory already stores, one row per
// fact, each recorded through the fact payload that names its identity:
// SessionEndFact (session and attempt), ReviewAttemptFact (head and
// round), GateHeadFact (card and head), PlanFilingFact (plan and
// revision) and HoldIntervalFact (repository and held since). Beside
// them the store's own rows: session_started and session_pushed events
// (identity task and session), plan_closed events (identity task and
// revision), the releases row (an open hold) and the task_status row
// changeStatus writes per change (identity task and status). A
// recorded identity survives restarts and records idempotently — one
// row, one id, whichever retry records it first.
//
// Three states never collapse into each other: unknown, undefined and
// zero. CoveredFrom zero means coverage is not established — panels
// read facts recorded from that moment on, so before it every panel is
// unknown, not zero. A Figure with Has false is undefined (empty
// cohort; the page renders a dash), as is a SizeIntercept with
// Available false. A Figure with Has true and Value zero is a measured
// zero — GateAboveBound and ReviewsNotReviewed are exactly that:
// targets of none.
type DivideConquer struct {
	CoveredFrom time.Time `json:"covered_from"`

	GateAboveBound  Figure       `json:"gate_above_bound"`
	SessionsPerCard []SizeFigure `json:"sessions_per_card"`
	SessionEnds     []NamedCount `json:"session_ends"`
	// EurPerMergedLine divides the window's actual euros by its merged
	// changed lines; no merged line means undefined, not zero.
	EurPerMergedLine   Figure          `json:"eur_per_merged_line"`
	FixedEurPerCard    []SizeIntercept `json:"fixed_eur_per_card"`
	Cuts               Figure          `json:"cuts"`
	CutGroupsClosed    Figure          `json:"cut_groups_closed"`
	GroupOpenHours     Figure          `json:"group_open_hours"`
	HoldWaitHours      Figure          `json:"hold_wait_hours"`
	ReviewsNotReviewed Figure          `json:"reviews_not_reviewed"`
	PlansByRounds      []NamedCount    `json:"plans_by_rounds"`
	CardsFirstRound    Figure          `json:"cards_first_round"`
	CardsLaterRounds   Figure          `json:"cards_later_rounds"`
	ReplansByOrigin    []NamedCount    `json:"replans_by_origin"`
}

// MetricsReport is every figure of one project over one window, the
// shape the endpoint serves and the page renders.
type MetricsReport struct {
	Repository string    `json:"repository"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`

	Deploys            Figure             `json:"deploys"`
	LeadTime           Figure             `json:"lead_time"`
	CardCycle          Figure             `json:"card_cycle"`
	Throughput         Figure             `json:"throughput"`
	ChangeFailure      Figure             `json:"change_failure"`
	RevertedRefiled    RevertedRefiled    `json:"reverted_refiled"`
	TimeToRestore      Figure             `json:"time_to_restore"`
	Spend              Figure             `json:"spend"`
	PerCard            Figure             `json:"per_card"`
	TokensPerCard      Figure             `json:"tokens_per_card"`
	ModelSpend         []MixShare         `json:"model_spend"`
	ServedBy           []MixShare         `json:"served_by"`
	ToolSpend          Figure             `json:"tool_spend"`
	ToolMix            []MixShare         `json:"tool_mix"`
	Interventions      Figure             `json:"interventions"`
	Escalations        Figure             `json:"escalations"`
	EscalatedShare     Figure             `json:"escalated_share"`
	EscalationsHandled EscalationsHandled `json:"escalations_handled"`
	HumanBlocked       Figure             `json:"human_blocked"`
	ReviewAcceptance   Figure             `json:"review_acceptance"`
	SelfDeploys        Figure             `json:"self_deploys"`
	DiffSize           DiffSize           `json:"diff_size"`
	ChatWork           ChatWork           `json:"chat_tool_work"`
	CIDuration         Figure             `json:"ci_duration"`
	DivideConquer      DivideConquer      `json:"divide_conquer"`
}

// metricWindow is the spans every figure reads: the window, the
// equal-length window before it, and the seven weeks the sparks cover.
// From and To are UTC midnights; From inclusive, To exclusive.
type metricWindow struct {
	From, PrevFrom, PrevTo, To, SparkFrom time.Time
}

func newMetricWindow(from, to time.Time) metricWindow {
	end := to.AddDate(0, 0, 1)
	days := int(end.Sub(from).Hours() / 24)
	return metricWindow{From: from, PrevFrom: from.AddDate(0, 0, -days), PrevTo: from, To: end,
		SparkFrom: end.AddDate(0, 0, -7*7)}
}

// Weeks queries bucket by ($2 − spark)/7 days over [spark, to); scalar
// queries take their own bounds as $2 and $3. 604800 is one week in
// seconds — Postgres divides an interval by no interval.
func metricBucket(col string) string {
	return metricBucketFrom(col, "$2")
}

func metricBucketFrom(col, spark string) string {
	return fmt.Sprintf(`floor(EXTRACT(EPOCH FROM (%s - %s::timestamptz)) / 604800)::int`, col, spark)
}

func metricScalar(ctx context.Context, db *sql.DB, query string, args ...any) (float64, bool, error) {
	var v sql.NullFloat64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Float64, v.Valid, nil
}

func metricWeeks(ctx context.Context, db *sql.DB, query string, args ...any) ([7]float64, error) {
	var weeks [7]float64
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return weeks, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int
		var v float64
		if err := rows.Scan(&b, &v); err != nil {
			return weeks, err
		}
		if b >= 0 && b < len(weeks) {
			weeks[b] = v
		}
	}
	return weeks, rows.Err()
}

func metricRatio(num, den float64) (float64, bool) {
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

// metricCount is the shape of every plain count: a fact even at zero.
func metricCount(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	f := Figure{Has: true, PrevHas: true}
	var err error
	if f.Value, _, err = metricScalar(ctx, db, scalar, repo, w.From, w.To); err != nil {
		return f, err
	}
	if f.Prev, _, err = metricScalar(ctx, db, scalar, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Weeks, err = metricWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	return f, err
}

// metricMedian is the shape of every median: absent over an empty
// cohort.
func metricMedian(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	var f Figure
	var err error
	if f.Value, f.Has, err = metricScalar(ctx, db, scalar, repo, w.From, w.To); err != nil {
		return f, err
	}
	if f.Prev, f.PrevHas, err = metricScalar(ctx, db, scalar, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Weeks, err = metricWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	return f, err
}

// metricRatioFigure reads num/den pairs for window, previous and each
// bucket; a zero denominator reads absent.
func metricRatioFigure(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	var f Figure
	read := func(from, to time.Time) (num, den float64, err error) {
		err = db.QueryRowContext(ctx, scalar, repo, from, to).Scan(&num, &den)
		return
	}
	num, den, err := read(w.From, w.To)
	if err != nil {
		return f, err
	}
	f.Value, f.Has = metricRatio(num, den)
	if num, den, err = read(w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Prev, f.PrevHas = metricRatio(num, den)
	rows, err := db.QueryContext(ctx, weeks, repo, w.SparkFrom, w.To)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int
		var bn, bd float64
		if err := rows.Scan(&b, &bn, &bd); err != nil {
			return f, err
		}
		if b >= 0 && b < 7 {
			f.Weeks[b], _ = metricRatio(bn, bd)
		}
	}
	return f, rows.Err()
}

// metricMeanOverCards divides a project total by the window's done
// cards: tokens per card, interventions per card.
func metricMeanOverCards(ctx context.Context, db *sql.DB, repo string, w metricWindow, total, totalWeeks string) (Figure, error) {
	const cards = `SELECT count(*) FROM tasks WHERE ` + doneCardWhere
	var f Figure
	num, _, err := metricScalar(ctx, db, total, repo, w.From, w.To)
	if err != nil {
		return f, err
	}
	den, _, err := metricScalar(ctx, db, cards, repo, w.From, w.To)
	if err != nil {
		return f, err
	}
	f.Value, f.Has = metricRatio(num, den)
	if num, _, err = metricScalar(ctx, db, total, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	if den, _, err = metricScalar(ctx, db, cards, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Prev, f.PrevHas = metricRatio(num, den)
	tRows, err := metricWeeks(ctx, db, totalWeeks, repo, w.SparkFrom, w.To)
	if err != nil {
		return f, err
	}
	dRows, err := metricWeeks(ctx, db, doneCardWeeks(`count(*)`), repo, w.SparkFrom, w.To)
	if err != nil {
		return f, err
	}
	for i := range f.Weeks {
		f.Weeks[i], _ = metricRatio(tRows[i], dRows[i])
	}
	return f, nil
}

// The card filter every card figure shares: the project's cards — no chat
// task, no roll card (the factory's rolls count under self-deploys) — and
// the done ones finished in the span. $2 and $3 are the span bounds.
const cardWhere = `repository = $1 AND kind <> 'chat' AND source <> '` + SourceRoll + `'`
const doneCardWhere = cardWhere + ` AND status = 'done' AND finished_at >= $2 AND finished_at < $3`

func doneCardWeeks(order string) string {
	return fmt.Sprintf(`SELECT %s, %s FROM tasks WHERE `+doneCardWhere+` GROUP BY 1`, metricBucket("finished_at"), order)
}

// metricWeekFigure is the shape of every per-week figure: the last
// weekly bucket answers, the bucket before it moves — a per-week name
// never shows a window total.
func metricWeekFigure(ctx context.Context, db *sql.DB, weeks string, args ...any) (Figure, error) {
	f := Figure{Has: true, PrevHas: true}
	var err error
	if f.Weeks, err = metricWeeks(ctx, db, weeks, args...); err != nil {
		return f, err
	}
	f.Value, f.Prev = f.Weeks[6], f.Weeks[5]
	return f, nil
}

// metricDeploys: project deployments the rollout wait confirmed. The
// factory's own project deploys by its rolls: it reads its self-deploys.
func metricDeploys(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricWeekFigure(ctx, db,
		`SELECT `+metricBucket("ready_at")+", count(*) FROM project_rolls WHERE repository = $1 AND ready_at >= $2 AND ready_at < $3 GROUP BY 1", repo, w.SparkFrom, w.To)
}

// metricLeadTime: push to live, the median over the window's rolls —
// from the merge that put the rolled commit on main (the card whose
// merge commit it is, its pull_request_merged event) to the rollout
// confirmed.
func metricLeadTime(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM project_rolls r JOIN tasks t ON t.repository = r.repository AND t.merge_commit = r.commit
		CROSS JOIN LATERAL (SELECT min(e.at) AS at FROM events e WHERE e.task_id = t.id AND e.kind = 'pull_request_merged') m
		WHERE r.repository = $1 AND r.commit <> '' AND m.at IS NOT NULL AND r.ready_at >= %s AND r.ready_at < $3`
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (r.ready_at - m.at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("r.ready_at")+`, `+agg+pair+` GROUP BY 1`, "$2"))
}

// metricCardCycle: creation to done, the median of the window's cards.
func metricCardCycle(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - created_at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM tasks WHERE `+doneCardWhere,
		doneCardWeeks(agg))
}

// metricThroughput: cards done.
func metricThroughput(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricCount(ctx, db, repo, w,
		`SELECT count(*) FROM tasks WHERE `+doneCardWhere,
		doneCardWeeks(`count(*)`))
}

// metricChangeFailure: the window's done cards that were reverted.
func metricChangeFailure(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM tasks WHERE ` + doneCardWhere
	const weeks = `SELECT %s, count(*) FILTER (WHERE reverted_at IS NOT NULL), count(*) FROM tasks WHERE ` + doneCardWhere + ` GROUP BY 1`
	return metricRatioFigure(ctx, db, repo, w,
		`SELECT count(*) FILTER (WHERE reverted_at IS NOT NULL), count(*)`+pair,
		fmt.Sprintf(weeks, metricBucket("finished_at")))
}

// metricRevertedRefiled: of the window's reverted cards, those an alert
// refiled at the revert and those left as bare reverts.
func metricRevertedRefiled(ctx context.Context, db *sql.DB, repo string, w metricWindow) (RevertedRefiled, error) {
	// a refile is the card's alert raised again at the revert, as the
	// revert pass marks it: the same fingerprint, filed on the fix card
	const refiled = `EXISTS (SELECT 1 FROM alerts a WHERE a.kind = 'alert' AND a.fingerprint = tasks.alert_fingerprint AND a.raised_at = tasks.reverted_at)`
	const q = `SELECT count(*) FILTER (WHERE NOT ` + refiled + `), count(*) FILTER (WHERE ` + refiled + `)
		FROM tasks WHERE ` + doneCardWhere + ` AND reverted_at IS NOT NULL`
	var r RevertedRefiled
	if err := db.QueryRowContext(ctx, q, repo, w.From, w.To).Scan(&r.Reverted, &r.Refiled); err != nil {
		return r, err
	}
	r.Has = r.Reverted+r.Refiled > 0
	if err := db.QueryRowContext(ctx, q, repo, w.PrevFrom, w.PrevTo).Scan(&r.Prev[0], &r.Prev[1]); err != nil {
		return r, err
	}
	var err error
	r.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("reverted_at")+", count(*) FROM tasks WHERE "+doneCardWhere+` AND reverted_at IS NOT NULL AND reverted_at >= $2 AND reverted_at < $3 GROUP BY 1`, repo, w.SparkFrom, w.To)
	return r, err
}

// metricTimeToRestore: alert to the fix card's done, by fingerprint: a
// merged fix card waits for the chain and has restored nothing yet.
func metricTimeToRestore(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM alerts a JOIN tasks ot ON ot.id = a.task_id
		CROSS JOIN LATERAL (SELECT min(t.finished_at) AS fix FROM tasks t WHERE t.alert_fingerprint = a.fingerprint AND t.alert_fingerprint <> '' AND t.status = 'done' AND t.finished_at > a.raised_at) f
		WHERE a.kind = 'alert' AND a.raised_at >= %s AND a.raised_at < $3 AND ot.repository = $1 AND f.fix IS NOT NULL`
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (f.fix - a.raised_at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("a.raised_at")+", "+agg+pair+` GROUP BY 1`, "$2"))
}

// ledgerPair is the project's ledger rows in a span of days: the ledger
// keeps days, so the bucket math is date arithmetic.
const ledgerPair = ` FROM ledger l JOIN tasks t ON t.id = l.task_id
		WHERE t.repository = $1 AND l.day >= $2::date AND l.day < $3::date`

// metricSpend: the ledger's euros, chat tasks included — what the
// project costs a week.
func metricSpend(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricWeekFigure(ctx, db,
		`SELECT ((l.day - $2::date) / 7), COALESCE(sum(l.eur), 0)::float8`+ledgerPair+` GROUP BY 1`, repo, w.SparkFrom, w.To)
}

// metricPerCard: what one card costs, the median of the window's cards'
// whole-life ledger totals.
func metricPerCard(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY (SELECT COALESCE(sum(l.eur), 0) FROM ledger l WHERE l.task_id = tasks.id))`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM tasks WHERE `+doneCardWhere,
		doneCardWeeks(agg))
}

// metricTokensPerCard: the tokens the window's cards' calls moved.
func metricTokensPerCard(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM audit a WHERE a.task_id IN (SELECT id FROM tasks WHERE ` + cardWhere + `) AND a.ts >= %s AND a.ts < $3`
	const agg = `COALESCE(sum(a.prompt_tokens + a.output_tokens), 0)::float8`
	return metricMeanOverCards(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("a.ts")+", "+agg+pair+` GROUP BY 1`, "$2"))
}

// A ledger ref's alias: the bare alias of an agent's call, the part after
// the purpose of factoryd's (review/big). The ledger is immutable, so the
// purpose aliases of before 2026-10-20 read as the classes they named.
// The text classes are policy.TextClasses; a model row of any other alias
// (vision) is a tool on the page.
const ledgerAlias = `(CASE regexp_replace(l.ref, '^.*/', '')
	WHEN 'coding-frontier' THEN 'big' WHEN 'review-frontier' THEN 'big' WHEN 'supervisor' THEN 'big' WHEN 'checkin' THEN 'big' WHEN 'operator' THEN 'big'
	WHEN 'coding-cheap' THEN 'small' WHEN 'pulse' THEN 'small' ELSE regexp_replace(l.ref, '^.*/', '') END)`

var (
	textClasses  = `('` + strings.Join(policy.TextClasses, `', '`) + `')`
	textClassRow = `l.kind = 'model' AND ` + ledgerAlias + ` IN ` + textClasses
	toolRow      = `(l.kind = 'tool' OR l.kind = 'model' AND ` + ledgerAlias + ` NOT IN ` + textClasses + `)`
)

// metricModelSpend: the window's model euros by class.
func metricModelSpend(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]MixShare, error) {
	return metricShares(ctx, db, `SELECT `+ledgerAlias+`, sum(l.eur)::float8`+ledgerPair+` AND `+textClassRow+` GROUP BY 1`, repo, w.From, w.To)
}

// metricServedBy: the window's model euros by the deployment that served
// each call, which tells how much the fallback behind a class carried.
func metricServedBy(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]MixShare, error) {
	return metricShares(ctx, db, `SELECT a.deployment, COALESCE(sum(a.call_cost_eur), 0)::float8
		FROM audit a JOIN tasks t ON t.id = a.task_id WHERE t.repository = $1 AND a.deployment <> '' AND a.ts >= $2 AND a.ts < $3 GROUP BY 1`, repo, w.From, w.To)
}

// metricToolSpend: the ledger's euros on tools, a week.
func metricToolSpend(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricWeekFigure(ctx, db,
		`SELECT ((l.day - $2::date) / 7), COALESCE(sum(l.eur), 0)::float8`+ledgerPair+` AND `+toolRow+` GROUP BY 1`, repo, w.SparkFrom, w.To)
}

// metricToolMix: the window's tool euros by tool, top three then other.
func metricToolMix(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]MixShare, error) {
	return metricShares(ctx, db, `SELECT `+ledgerAlias+`, sum(l.eur)::float8`+ledgerPair+` AND `+toolRow+` GROUP BY 1`, repo, w.From, w.To)
}

// metricShares reads (name, amount) rows into shares of their total: the
// top three by amount, the rest folded into other.
func metricShares(ctx context.Context, db *sql.DB, query string, args ...any) ([]MixShare, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct {
		alias  string
		tokens float64
	}
	var ps []pair
	var total float64
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.alias, &p.tokens); err != nil {
			return nil, err
		}
		ps = append(ps, p)
		total += p.tokens
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].tokens != ps[j].tokens {
			return ps[i].tokens > ps[j].tokens
		}
		return ps[i].alias < ps[j].alias
	})
	var mix []MixShare
	for i, p := range ps {
		alias := p.alias
		if i >= 3 {
			alias = "other"
		}
		if len(mix) > 0 && mix[len(mix)-1].Alias == alias {
			mix[len(mix)-1].Share += 100 * p.tokens / total
			continue
		}
		mix = append(mix, MixShare{Alias: alias, Share: 100 * p.tokens / total})
	}
	return mix, nil
}

// metricInterventions: human acts per card, over the window's cards.
func metricInterventions(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	// an act on a card counts when the card counts, an act on no card (a
	// filing) always
	const pair = ` FROM events e WHERE e.repository = $1 AND e.via IN ('button', 'chat', 'webhook', 'console')
		AND (e.task_id IN (SELECT id FROM tasks WHERE ` + cardWhere + `) OR NOT EXISTS (SELECT 1 FROM tasks WHERE id = e.task_id))
		AND e.at >= %s AND e.at < $3`
	return metricMeanOverCards(ctx, db, repo, w,
		fmt.Sprintf(`SELECT count(*)`+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("e.at")+", count(*)"+pair+` GROUP BY 1`, "$2"))
}

// metricEscalations: cards the gate supervisor escalated to a human.
func metricEscalations(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM alerts a JOIN tasks t ON t.id = a.task_id
		WHERE a.kind = 'escalation' AND a.raised_at >= %s AND a.raised_at < $3 AND t.repository = $1`
	return metricCount(ctx, db, repo, w,
		fmt.Sprintf(`SELECT count(*)`+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("a.raised_at")+", count(*)"+pair+` GROUP BY 1`, "$2"))
}

// selfRolls is one row per roll of the factory: the commit and when its
// last image went live — the rolls table holds a row per image.
const selfRolls = `(SELECT sha, max(live_at) AS live FROM rolls GROUP BY sha) r`

// metricEscalatedShare: the share of the window's done cards that
// escalated to a human at least once in their life.
func metricEscalatedShare(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const escalated = `EXISTS (SELECT 1 FROM alerts a WHERE a.task_id = tasks.id AND a.kind = 'escalation')`
	return metricRatioFigure(ctx, db, repo, w,
		`SELECT count(*) FILTER (WHERE `+escalated+`), count(*) FROM tasks WHERE `+doneCardWhere,
		fmt.Sprintf(`SELECT %s, count(*) FILTER (WHERE `+escalated+`), count(*) FROM tasks WHERE `+doneCardWhere+` GROUP BY 1`, metricBucket("finished_at")))
}

// metricEscalationsHandled: of the window's escalations, those a human
// judged and those an AI judged; ai names the AI resolvers, the project
// agent and the policy's ai_members.
func metricEscalationsHandled(ctx context.Context, db *sql.DB, repo string, w metricWindow, ai []string) (EscalationsHandled, error) {
	const pair = ` FROM alerts a JOIN tasks t ON t.id = a.task_id
		WHERE a.kind = 'escalation' AND t.repository = $1 AND a.raised_at >= $2 AND a.raised_at < $3`
	const byAI = `a.resolved_by = ANY($4::text[])`
	const byHuman = `a.resolved_by IS NOT NULL AND a.resolved_by <> '` + FactoryDecider + `' AND NOT (` + byAI + `)`
	const counts = `count(*) FILTER (WHERE ` + byHuman + `), count(*) FILTER (WHERE ` + byAI + `), count(*) FILTER (WHERE a.resolved_by = '` + FactoryDecider + `')`
	var e EscalationsHandled
	if err := db.QueryRowContext(ctx, `SELECT `+counts+pair, repo, w.From, w.To, ai).Scan(&e.Human, &e.AI, &e.Unjudged); err != nil {
		return e, err
	}
	if err := db.QueryRowContext(ctx, `SELECT `+counts+pair, repo, w.PrevFrom, w.PrevTo, ai).Scan(&e.Prev[0], &e.Prev[1], &e.Prev[2]); err != nil {
		return e, err
	}
	var err error
	e.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("a.raised_at")+`, count(*) FILTER (WHERE `+byHuman+`)`+pair+` GROUP BY 1`, repo, w.SparkFrom, w.To, ai)
	return e, err
}

// metricSelfDeploys: the factory's own rolls — every project reads the
// same count; the table has no repository.
func metricSelfDeploys(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricWeekFigure(ctx, db,
		`SELECT `+metricBucketFrom("r.live", "$1")+", count(*) FROM "+selfRolls+" WHERE r.live >= $1 AND r.live < $2 GROUP BY 1", w.SparkFrom, w.To)
}

// metricSelfLeadTime: push to live for the factory itself — from the CI
// run of the commit a roll card rolled (the card's sha) to the roll's
// last image live (the card's merge commit is the roll), the median over
// the window's rolls.
func metricSelfLeadTime(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM ` + selfRolls + ` JOIN tasks t ON t.repository = $1 AND t.source = '` + SourceRoll + `' AND t.merge_commit = r.sha
		JOIN ci_runs c ON c.repository = $1 AND c.commit = t.sha
		WHERE r.live >= %s AND r.live < $3`
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (r.live - c.pushed_at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("r.live")+`, `+agg+pair+` GROUP BY 1`, "$2"))
}

// metricDiffSize: the median diff of the window's shipped cards.
func metricDiffSize(ctx context.Context, db *sql.DB, repo string, w metricWindow) (DiffSize, error) {
	const q = `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY files), percentile_cont(0.5) WITHIN GROUP (ORDER BY insertions), percentile_cont(0.5) WITHIN GROUP (ORDER BY deletions)
		FROM tasks WHERE ` + doneCardWhere + ` AND files > 0`
	read := func(from, to time.Time) (f, i, d float64, ok bool, err error) {
		var nf, ni, nd sql.NullFloat64
		if err = db.QueryRowContext(ctx, q, repo, from, to).Scan(&nf, &ni, &nd); err != nil {
			return
		}
		return nf.Float64, ni.Float64, nd.Float64, nf.Valid, nil
	}
	var out DiffSize
	var err error
	if out.Files, out.Insertions, out.Deletions, out.Has, err = read(w.From, w.To); err != nil {
		return out, err
	}
	if out.Prev[0], out.Prev[1], out.Prev[2], out.PrevHas, err = read(w.PrevFrom, w.PrevTo); err != nil {
		return out, err
	}
	out.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("finished_at")+`, percentile_cont(0.5) WITHIN GROUP (ORDER BY files) FROM tasks WHERE `+doneCardWhere+` AND files > 0 GROUP BY 1`, repo, w.SparkFrom, w.To)
	return out, err
}

// metricChatWork: the project agent's tool calls and their minutes.
func metricChatWork(ctx context.Context, db *sql.DB, repo string, w metricWindow) (ChatWork, error) {
	const q = `SELECT (SELECT COALESCE(sum(jsonb_array_length(tool_calls)), 0)::float8 FROM chat_messages WHERE repository = $1 AND role = 'assistant' AND at >= $2 AND at < $3),
		(SELECT COALESCE(sum((c ->> 'ms')::numeric / 60000), 0)::float8 FROM chat_messages m CROSS JOIN LATERAL jsonb_array_elements(COALESCE(m.tool_calls, '[]')) c
			WHERE m.repository = $1 AND m.role = 'assistant' AND m.at >= $2 AND m.at < $3)`
	var c ChatWork
	if err := db.QueryRowContext(ctx, q, repo, w.From, w.To).Scan(&c.Calls, &c.Minutes); err != nil {
		return c, err
	}
	if err := db.QueryRowContext(ctx, q, repo, w.PrevFrom, w.PrevTo).Scan(&c.PrevCalls, &c.PrevMinutes); err != nil {
		return c, err
	}
	var err error
	c.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("at")+", COALESCE(sum(jsonb_array_length(tool_calls)), 0)::float8 FROM chat_messages WHERE repository = $1 AND role = 'assistant' AND at >= $2 AND at < $3 GROUP BY 1", repo, w.SparkFrom, w.To)
	return c, err
}

// metricCIDuration: the median Actions run, push to finish.
func metricCIDuration(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM ci_runs WHERE repository = $1 AND pushed_at >= $2 AND pushed_at < $3`,
		`SELECT `+metricBucket("pushed_at")+", "+agg+` FROM ci_runs WHERE repository = $1 AND pushed_at >= $2 AND pushed_at < $3 GROUP BY 1`)
}

// The two figures the event sequence carries, read in Go from one pull
// of the project's done cards and their status events. The pull spans
// [min(SparkFrom, PrevFrom), To) — every window the figures read — and
// carries each card's events unbounded below, so a span that opens
// before the pull still closes; event resolution is the clock's
// granularity.

type metricStatusEvent struct {
	task   string
	at     time.Time
	status string
}

type metricCard struct {
	id              string
	created, doneAt time.Time
}

func metricStatusPull(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]metricCard, []metricStatusEvent, error) {
	lo := w.SparkFrom
	if w.PrevFrom.Before(lo) {
		lo = w.PrevFrom
	}
	cards, err := func() ([]metricCard, error) {
		rows, err := db.QueryContext(ctx, `SELECT id, created_at, finished_at FROM tasks WHERE `+doneCardWhere, repo, lo, w.To)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []metricCard
		for rows.Next() {
			var c metricCard
			if err := rows.Scan(&c.id, &c.created, &c.doneAt); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}()
	if err != nil {
		return nil, nil, err
	}
	events, err := func() ([]metricStatusEvent, error) {
		rows, err := db.QueryContext(ctx, `SELECT e.task_id, e.at, e.detail ->> 'status' FROM events e
			WHERE e.kind = 'task_status' AND e.task_id IN (SELECT id FROM tasks WHERE `+doneCardWhere+`)
			ORDER BY e.task_id, e.at, e.id`, repo, lo, w.To)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []metricStatusEvent
		for rows.Next() {
			var e metricStatusEvent
			if err := rows.Scan(&e.task, &e.at, &e.status); err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, rows.Err()
	}()
	return cards, events, err
}

func eventsOf(events []metricStatusEvent, task string) []metricStatusEvent {
	var out []metricStatusEvent
	for _, e := range events {
		if e.task == task {
			out = append(out, e)
		}
	}
	return out
}

// blockedSeconds measures the waiting-human/waiting-poster spans of one
// card: from each such status to the next status event, a span still
// open at the finish capped there.
func blockedSeconds(events []metricStatusEvent, finish time.Time) (secs float64) {
	var since time.Time
	waiting := false
	for _, e := range events {
		if waiting {
			secs += e.at.Sub(since).Seconds()
			waiting = false
		}
		if e.status == "waiting-human" || e.status == "waiting-poster" {
			waiting, since = true, e.at
		}
	}
	if waiting {
		secs += finish.Sub(since).Seconds()
	}
	return
}

// reviewVerdict reads a card's first review's outcome: accepted (the
// next new status closes or waits for a human) or changes (back to
// running); ok is false when the card was never reviewed or the verdict
// is neither. The gate sets in-review again on every pass, so the verdict
// is the first status that differs.
func reviewVerdict(events []metricStatusEvent) (accepted bool, ok bool) {
	for i, e := range events {
		if e.status != "in-review" {
			continue
		}
		for _, n := range events[i+1:] {
			switch n.status {
			case "in-review":
				continue
			case "closing", "waiting-human":
				return true, true
			case "running":
				return false, true
			}
			return false, false
		}
		return false, false
	}
	return false, false
}

func firstReviewAt(events []metricStatusEvent) time.Time {
	for _, e := range events {
		if e.status == "in-review" {
			return e.at
		}
	}
	return time.Time{}
}

func withinWindow(at, lo, hi time.Time) bool {
	return !at.Before(lo) && at.Before(hi)
}

func weekBucket(at, spark time.Time) int {
	return int(at.Sub(spark) / (7 * 24 * time.Hour))
}

// metricHumanBlocked: the share of the window's cards' lifetime spent
// blocked on a human.
func metricHumanBlocked(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	cards, events, err := metricStatusPull(ctx, db, repo, w)
	if err != nil {
		return Figure{}, err
	}
	var blocked, life, pBlocked, pLife float64
	var wBlocked, wLife [7]float64
	for _, c := range cards {
		secs := blockedSeconds(eventsOf(events, c.id), c.doneAt)
		lifeSecs := c.doneAt.Sub(c.created).Seconds()
		switch {
		case withinWindow(c.doneAt, w.From, w.To):
			blocked, life = blocked+secs, life+lifeSecs
		case withinWindow(c.doneAt, w.PrevFrom, w.PrevTo):
			pBlocked, pLife = pBlocked+secs, pLife+lifeSecs
		}
		if withinWindow(c.doneAt, w.SparkFrom, w.To) {
			if b := weekBucket(c.doneAt, w.SparkFrom); b >= 0 && b < 7 {
				wBlocked[b], wLife[b] = wBlocked[b]+secs, wLife[b]+lifeSecs
			}
		}
	}
	var f Figure
	f.Value, f.Has = metricRatio(blocked, life)
	f.Prev, f.PrevHas = metricRatio(pBlocked, pLife)
	for b := range f.Weeks {
		f.Weeks[b], _ = metricRatio(wBlocked[b], wLife[b])
	}
	return f, nil
}

// metricReviewAcceptance: the share of reviewed cards whose first
// review closed the card, weeks bucketed by the review.
func metricReviewAcceptance(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	cards, events, err := metricStatusPull(ctx, db, repo, w)
	if err != nil {
		return Figure{}, err
	}
	var acc, tot, pAcc, pTot float64
	var wAcc, wTot [7]float64
	for _, c := range cards {
		evs := eventsOf(events, c.id)
		accepted, ok := reviewVerdict(evs)
		if !ok {
			continue
		}
		switch {
		case withinWindow(c.doneAt, w.From, w.To):
			tot++
			if accepted {
				acc++
			}
		case withinWindow(c.doneAt, w.PrevFrom, w.PrevTo):
			pTot++
			if accepted {
				pAcc++
			}
		}
		if withinWindow(c.doneAt, w.SparkFrom, w.To) {
			if b := weekBucket(firstReviewAt(evs), w.SparkFrom); b >= 0 && b < 7 {
				wTot[b]++
				if accepted {
					wAcc[b]++
				}
			}
		}
	}
	var f Figure
	f.Value, f.Has = metricRatio(acc, tot)
	f.Prev, f.PrevHas = metricRatio(pAcc, pTot)
	for b := range f.Weeks {
		f.Weeks[b], _ = metricRatio(wAcc[b], wTot[b])
	}
	return f, nil
}

// Metrics assembles the report every figure function fills one field of.
// The factory's own project deploys by its rolls: its deploys and lead
// time read the rolls table.
func (s *PGStore) ProjectMetrics(ctx context.Context, repository string, from, to time.Time) (MetricsReport, error) {
	db, w := s.db, newMetricWindow(from, to)
	r := MetricsReport{Repository: repository, From: w.From, To: w.To.AddDate(0, 0, -1)}
	deploys, leadTime := metricDeploys, metricLeadTime
	if s.SelfRepository != "" && repository == s.SelfRepository {
		deploys, leadTime = metricSelfDeploys, metricSelfLeadTime
	}
	var err error
	if r.Deploys, err = deploys(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.LeadTime, err = leadTime(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CardCycle, err = metricCardCycle(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Throughput, err = metricThroughput(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ChangeFailure, err = metricChangeFailure(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.RevertedRefiled, err = metricRevertedRefiled(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.TimeToRestore, err = metricTimeToRestore(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Spend, err = metricSpend(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.PerCard, err = metricPerCard(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.TokensPerCard, err = metricTokensPerCard(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ModelSpend, err = metricModelSpend(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ServedBy, err = metricServedBy(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ToolSpend, err = metricToolSpend(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ToolMix, err = metricToolMix(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Interventions, err = metricInterventions(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Escalations, err = metricEscalations(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.EscalatedShare, err = metricEscalatedShare(ctx, db, repository, w); err != nil {
		return r, err
	}
	ai := []string{AgentDecider}
	if v, ok, err := policy.Current(ctx, db); err != nil {
		return r, err
	} else if ok {
		ai = append(ai, v.Document.AIMembers...)
	}
	if r.EscalationsHandled, err = metricEscalationsHandled(ctx, db, repository, w, ai); err != nil {
		return r, err
	}
	if r.HumanBlocked, err = metricHumanBlocked(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ReviewAcceptance, err = metricReviewAcceptance(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.SelfDeploys, err = metricSelfDeploys(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.DiffSize, err = metricDiffSize(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ChatWork, err = metricChatWork(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CIDuration, err = metricCIDuration(ctx, db, repository, w); err != nil {
		return r, err
	}
	return r, nil
}
