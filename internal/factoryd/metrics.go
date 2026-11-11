package factoryd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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

// CasesByRung is the share of the window's ended cases resolved at each
// rung 0 to 3, and the withdrawn share, 0..1. Prev is the previous
// window's [rung 0, 1, 2, 3, withdrawn], Weeks the share resolved below
// the human per bucket.
type CasesByRung struct {
	Rung      [4]float64 `json:"rung"`
	Withdrawn float64    `json:"withdrawn"`
	Prev      [5]float64 `json:"prev"`
	Has       bool       `json:"has"`
	Weeks     [7]float64 `json:"weeks"`
}

// CaseMoneyFigure sets the money a card spent on sessions after a case of the
// window was resolved against the money that case's resolution cost (its
// steps and its draws). Prev is [after, resolve].
type CaseMoneyFigure struct {
	AfterEUR   float64    `json:"after_eur"`
	ResolveEUR float64    `json:"resolve_eur"`
	Prev       [2]float64 `json:"prev"`
	Has        bool       `json:"has"`
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

// SessionTime is where the project's session time went over the window,
// from the session_time rows: the check share (the check classes over the
// wall) and the command share against the window before and by week, the
// hours by class and by tool, the full check's median run and the longest
// command.
type SessionTime struct {
	CheckShare   Figure             `json:"check_share"`
	CommandShare Figure             `json:"command_share"`
	Sessions     int                `json:"sessions"`
	WallHours    float64            `json:"wall_hours"`
	ClassHours   map[string]float64 `json:"class_hours,omitempty"`
	ToolHours    map[string]float64 `json:"tool_hours,omitempty"`
	CheckRuns    int                `json:"check_runs"`
	CheckMedianS float64            `json:"check_median_s"`
	Longest      LongestRun         `json:"longest"`
}

// LongestRun is the window's longest command: its class, its seconds and
// its text's first 120 bytes.
type LongestRun struct {
	Class   string  `json:"class,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Command string  `json:"command,omitempty"`
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

// NamedFigure is one bucket's whole comparison: the window's count of
// the name, the equal window before it and each of the seven weeks, the
// plain-count Figure semantics — a span the name does not reach holds a
// zero, not an absence, and a name only the previous window or a week
// holds is not lost. Plans by rounds, card filings by round and replans
// by origin compare through it.
type NamedFigure struct {
	Name   string `json:"name"`
	Figure Figure `json:"figure"`
}

// SizeIntercept is one size's fixed euros per card: the intercept of an
// ordinary least squares of actual euros on changed lines across the
// window's merged cards of that size. Available false means the fit is
// undefined — too few cards, or changed lines without variance — never
// zero. Cards is the fit's sample size.
type SizeIntercept struct {
	Size      string  `json:"size"`
	EUR       float64 `json:"eur"`
	Available bool    `json:"available"`
	Cards     int     `json:"cards"`
}

// EconomicsCoverage names how far the economics panels reach: the
// window's first-merged cohort and its holes. Unknown is not zero spend
// — a card the ledger holds no row for is missing cost, never a free
// card — and a card without a reading or without a size sits the panels
// out; every hole is counted, none guessed.
type EconomicsCoverage struct {
	Cards        int `json:"cards"`         // first-merged cards in the window
	MissingCost  int `json:"missing_cost"`  // of them, cards the ledger holds no row for
	MissingLines int `json:"missing_lines"` // of them, cards without a gate_head reading
	MissingSize  int `json:"missing_size"`  // of them, readings that carry no size
}

// GateBoundCoverage names how far the size-bound panel reaches: the
// window's merge-gate readings and the holes among them. A reading
// measures its card only complete — the fact's identity, a bound, both
// line counts and the size whose bound it planned. Every hole is
// counted, none guessed and none filled: a missing bound or missing
// lines never borrow the policy in force now or the card's metadata,
// a size the reading never carried and an explicitly unsized card
// measure nothing, and an identity-less legacy row is no reading the
// panel can stand on. Unknown is not measured passing: the figure
// stands on the measured rest.
type GateBoundCoverage struct {
	Readings     int `json:"readings"`      // merge-gate readings in the window
	Measured     int `json:"measured"`      // of them, readings complete enough to measure
	MissingBound int `json:"missing_bound"` // of them, readings that carry no bound
	MissingLines int `json:"missing_lines"` // of them, readings without insertions or deletions
	UnknownSize  int `json:"unknown_size"`  // of them, readings that carry no size
	UnsizedSize  int `json:"unsized_size"`  // of them, readings of an explicitly unsized card
	Identityless int `json:"identityless"`  // of them, legacy rows without the fact identity
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

// EventSessionEnd is one session's terminal end on a card (roadmap 336),
// the row the divide-and-conquer panels read. Detail {"session",
// "attempt", "outcome", "started", "ended"} — no summary, which the
// timeline line alone keeps. Identity: the session and the attempt, so
// a session a stopped factoryd detached from and its reattached end are
// one fact.
const EventSessionEnd = "session_end"

// Event is the end's row of task's card in repository's issue, ended
// when the session completed.
func (f SessionEndFact) Event(task, repository string, issue int64, ended time.Time) Event {
	return Event{Repository: repository, TaskID: task, IssueNumber: issue, Kind: EventSessionEnd,
		Actor: FactoryDecider, Via: ViaFactoryd, At: ended,
		Detail: map[string]any{"session": f.Session, "attempt": f.Attempt, "outcome": f.Outcome,
			"started": rfc3339(f.Started), "ended": rfc3339(ended)},
		IdentityKey: identityKey(EventSessionEnd, f.Session, fmt.Sprintf("%d", f.Attempt))}
}

// SessionStartFact is one named session's start on a card: the moment a
// session's first model call came back answered, which is what made the
// pod a session — a pod that started and made no answered call records
// none. Identity: the card and the session; one session, one row,
// whichever retry or reattachment records it. A session whose activity
// failed has no id and records none.
type SessionStartFact struct {
	Task    string // with Session: the identity
	Session string
	Model   string
	Attempt int
}

func (f SessionStartFact) detail() map[string]any {
	return map[string]any{"session": f.Session, "model": aliasOf(f.Model), "attempt": f.Attempt}
}

// Event is the start's row of task's card in repository's issue, the row
// the sessions panel counts: detail {"session", "model", "attempt"} —
// the timeline line's own words. Identity: the card and the session, so
// a retry and a reattached write leave one row.
func (f SessionStartFact) Event(task, repository string, issue int64, at time.Time) Event {
	return Event{Repository: repository, TaskID: task, IssueNumber: issue, Kind: EventSessionStarted,
		Actor: FactoryDecider, Via: ViaFactoryd, At: at,
		Detail:      f.detail(),
		IdentityKey: identityKey(EventSessionStarted, task, f.Session)}
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

// EventModelReview is one review call's answer on a head (roadmap 336),
// the row the divide-and-conquer panels read. Detail {"pr", "commit",
// "round", "model", "verdict", "reviewed"} — reviewed false carries
// "reason", the bounded note of the answer that never judged the head;
// no summary, which the timeline line alone keeps. Identity: the card,
// the commit and the round, so two genuine reviews of one head stay two,
// and so do two cards' reviews of one head (a successor carries its
// card's pull request).
const EventModelReview = "model_review"

// StepStageSkipped is the timeline step of a stage that does not run for
// the task, and EventStageSkipped the events row beside it. Detail
// {"stage", "reason"}.
const (
	StepStageSkipped  = "stage_skipped"
	EventStageSkipped = "stage_skipped"
)

// EventGateAudit is one audit attempt on a head, the row beside the
// audit's timeline line. Detail {"pr", "commit", "model", "ran",
// "verdict"}: ran false when the audit gave no verdict, with "reason",
// the bounded why of its first such finding, and no verdict. Identity:
// the card, the commit and the stages' round, so a retried attempt writes
// once.
const EventGateAudit = "gate_audit"

// maxFactReason bounds a fact row's reason; the notes it carries are
// bounded again where they were written.
const maxFactReason = 500

// Event is the answer's row of task's card in repository's issue; an
// empty verdict is one the model never judged.
func (f ReviewAttemptFact) Event(task, repository string, issue int64) Event {
	detail := map[string]any{"pr": f.PR, "commit": f.Commit, "round": f.Round, "model": aliasOf(f.Model),
		"verdict": f.Verdict, "reviewed": f.Verdict != ""}
	if f.Verdict == "" {
		detail["reason"] = textutil.Truncate(f.Note, maxFactReason)
	}
	return Event{Repository: repository, TaskID: task, IssueNumber: issue, Kind: EventModelReview,
		Actor:       FactoryDecider,
		Via:         ViaFactoryd,
		Detail:      detail,
		IdentityKey: identityKey(EventModelReview, task, f.Commit, fmt.Sprintf("%d", f.Round))}
}

// EventGateHead is one card's reading at one head — its gate decision,
// size and diff — recorded when a card reaches a gate: the merge gate's
// cards-above-bound panel reads it. Detail {"gate", "head", "decision",
// "size", "bound", "policy_version", "files", "insertions", "deletions"}.
const EventGateHead = "gate_head"

// GateHeadFact is that reading: the merge decision's snapshot of the card
// as the gates judged it — the card's size, the effective bound under
// the policy the gates read (the size's plan plus the raises its extend
// answers added), the policy version
// and the lines the gates counted. Decision is "auto" (the gates let it
// merge) or "approval" (a person decides). Identity: the card and the
// head; one head, one row.
type GateHeadFact struct {
	Task          string // with Head: the identity
	Head          string
	Gate          string
	Decision      string
	Size          string
	Bound         int // the effective line bound at the reading
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
// the revision; one revision, one row. A card the filing filed carries
// its own row: the card's issue and plan key canonical, the identity
// the plan, revision and key.
type PlanFilingFact struct {
	Plan     string // with Revision: the identity
	Revision int
	Origin   string // ReplanPerson, ReplanCut or "round"; "" is a plain filing
	Card     int64  // the filed card's issue; 0 on the revision's own row
	Key      string // the filed card's plan key; "" on the revision's own row
}

// Event is the filing's row of repository's issue; extra carries what
// the filing names (the fates of the old cards and the new ones, or a
// round's cards). The detail is a fresh copy of extra — the caller's
// map stays its own — over which every canonical field comes from the
// fact: revision and origin always, card and key on a card's own row.
// A reserved extra can neither disagree with the row the identity
// names nor forge a card's membership on a revision's row; the extras
// no fact owns survive.
func (f PlanFilingFact) Event(repository string, issue int64, extra map[string]any) Event {
	detail := make(map[string]any, len(extra)+2)
	maps.Copy(detail, extra)
	delete(detail, "card")
	delete(detail, "key")
	detail["revision"], detail["origin"] = f.Revision, f.Origin
	if f.Card != 0 {
		detail["card"], detail["key"] = f.Card, f.Key
	}
	id := fmt.Sprintf("%d", f.Revision)
	if f.Key != "" {
		id += " " + f.Key
	}
	return Event{Repository: repository, TaskID: f.Plan, IssueNumber: issue, Kind: EventPlanReplanned,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail,
		IdentityKey: identityKey(EventPlanReplanned, f.Plan, id)}
}

// EventCut is one applied cut — the cut step's row on the cut card's own:
// what its revision was cut into, at which session and spend, and the
// group the cut left (GroupOf the cut card: its plan for a plan's child,
// itself for a direct card's conversion). A pending cut step is the
// request, not the cut: the row comes with the record of the replan that
// applied it. Detail {"plan", "group", "revision", "into", "session",
// "spent"}.
const EventCut = "cut"

// CutFact is that cut. Identity: the plan and the cut card; one card, cut
// once by one replan, however often its filing runs again.
type CutFact struct {
	Plan     string // with Card: the identity
	Card     string
	Issue    int64
	Group    string // GroupOf the cut card: the group it leaves
	Revision int
	Into     []int64
	Session  int
	Spent    float64
}

// Event is the cut's row of repository's issue.
func (f CutFact) Event(repository string, issue int64) Event {
	return Event{Repository: repository, TaskID: f.Card, IssueNumber: issue, Kind: EventCut,
		Actor: FactoryDecider, Via: ViaFactoryd,
		Detail:      map[string]any{"plan": f.Plan, "group": f.Group, "revision": f.Revision, "into": f.Into, "session": f.Session, "spent": f.Spent},
		IdentityKey: identityKey(EventCut, f.Plan, f.Card)}
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

// RecordFactRow writes e, a fact's events row its payload built with its
// identity. Unlike recordEvent a failure is returned, not logged: the
// activity that owns the fact fails on it, and its retry writes the row
// the identity keeps single — a fact the store did not take must not be
// lost to a log line. A Timeline without a recorder records none.
func (tl *Timeline) RecordFactRow(ctx context.Context, e Event) error {
	if tl == nil || tl.Events == nil {
		return nil
	}
	_, err := tl.Events.Record(ctx, e)
	return err
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

// SessionsCoverage names how far the sessions panel reaches: the
// window's first-merged cohort and the holes around its averages. A card
// no merge-gate reading at the head it actually built on covers has no
// historical size — unknown, never the task row's mutable size — and a
// reading that planned no size is named beside the unsized bucket's own
// average. A card merged before the repository's first named start holds
// no start evidence, and a card whose terminal ends name sessions no
// start row names holds partial evidence: neither is a measured zero,
// none enters an average, and no start is invented for either — the
// ends they hold stay the ends panel's own facts.
type SessionsCoverage struct {
	Cards         int `json:"cards"`          // first-merged cards in the window
	UnknownSize   int `json:"unknown_size"`   // of them, cards no reading at their head covers
	Unsized       int `json:"unsized"`        // of them, cards whose reading planned no size
	MissingStarts int `json:"missing_starts"` // of them, cards merged before the repository's first named start
	PartialStarts int `json:"partial_starts"` // of them, cards whose ends name sessions no start row names
}

// ReviewEvidence names how far the review panel reaches: the span's
// genuine answers — the measured denominator the not-reviewed count
// stands over, "2 of 3" — and the model_review rows no usable evidence
// covers. An answer is genuine only complete: the fact's attempt
// identity, head and round, the persistence contract that keeps one
// attempt one row so a replay adds no answer, and the classification
// the payload wrote — reviewed true, the answer judged the head (a
// negative verdict among them: concerns judged the change), or
// reviewed false with its bounded reason. A row without its identity
// or without a usable classification is unknown evidence, never an
// inferred success: it measures nothing, and the coverage counts it
// rather than any panel guessing it an answer.
type ReviewEvidence struct {
	Answers Figure `json:"answers"` // genuine answers, a fact even at zero
	Unknown Figure `json:"unknown"` // rows without their attempt identity or classification
}

// GroupHoursCoverage names how far the group-open panel reaches: the
// window's closed groups and the holes among them. A closed group
// measures only from an actual opening — its first child merge the
// OpenGroups contract reads — and a group that merge opened but no
// plan_closed row closed yet is open, not finished. Every hole is
// counted, none guessed: the mean stands on the measured rest, and the
// open and the startless groups never enter it.
type GroupHoursCoverage struct {
	Groups       int `json:"groups"`        // groups whose closure lands in the window
	MissingStart int `json:"missing_start"` // of them, groups no child merge covers
	Open         int `json:"open"`          // groups a child merge opened and no closure closed, at the report
}

// HoldWaitCoverage names the production holds the hold-wait panel read:
// the ended holds their rows cover, the one the releases row holds open
// at the report, and the rows no usable interval covers — a
// production_released row whose held_since no start names measures
// nothing, never a zero-length hold.
type HoldWaitCoverage struct {
	Ended   int `json:"ended"`   // distinct ended holds the rows name, duplicates once
	Open    int `json:"open"`    // holds the releases row still holds at the report
	Missing int `json:"missing"` // rows whose held_since covers no interval
}

// DivideConquer measures the divide-and-conquer plan (roadmap 336, doc
// docs/plans/2026-10-29-divide-and-conquer.md): whether cards stay
// within their size bound, what one costs in sessions and euros, and
// how the plan's rounds and cuts behave. Duration panels answer in
// milliseconds, precise where an hour's rounding would blur them,
// beside their legacy hour fields the console kept reading.
//
// Every panel reads hard rows the factory already stores, one row per
// fact, each recorded through the fact payload that names its identity:
// SessionEndFact (session and attempt), SessionStartFact (card and
// session), ReviewAttemptFact (head and
// round), GateHeadFact (card and head), PlanFilingFact (plan and
// revision, and on a card's own filing row its key), CutFact (plan and cut
// card). Beside
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

	GateAboveBound Figure `json:"gate_above_bound"`
	// GateBoundCoverage counts the window's merge-gate readings and the
	// holes the panel above reads around; the figure stands on the
	// measured rest.
	GateBoundCoverage GateBoundCoverage `json:"gate_bound_coverage"`
	SessionsPerCard   []SizeFigure      `json:"sessions_per_card"`
	// SessionsCoverage counts the window's first-merged cards and the
	// holes panel 2's averages read around — unknown or unsized
	// historical size, missing or partial start evidence — so an old
	// merged card without start evidence is never a measured zero.
	SessionsCoverage SessionsCoverage `json:"sessions_coverage"`
	SessionEnds      []NamedCount     `json:"session_ends"`
	// EurPerMergedLine divides the window's actual euros by its merged
	// changed lines; no merged line means undefined, not zero.
	EurPerMergedLine Figure          `json:"eur_per_merged_line"`
	FixedEurPerCard  []SizeIntercept `json:"fixed_eur_per_card"`
	// EconomicsCoverage counts the window's first-merged cards and the
	// holes the two economics panels above read around; both stand on
	// the measured rest.
	EconomicsCoverage EconomicsCoverage `json:"economics_coverage"`
	// Cuts counts the window's applied cuts; CutGroupsClosed the groups
	// those cuts left that the span's own end saw closed.
	Cuts            Figure `json:"cuts"`
	CutGroupsClosed Figure `json:"cut_groups_closed"`
	// GroupOpenMillis is panel 7's duration: the mean milliseconds a
	// group stayed open, over the groups whose first plan_closed row
	// lands in the span — from each group's first actual child merge,
	// however much of the wait falls before the span — undefined over
	// an empty cohort. GroupOpenHours is the same mean in hours, the
	// legacy unit the console kept reading. GroupHoursCoverage counts
	// the cohort's holes: an open group, a closed group no merge
	// covers.
	GroupOpenHours     Figure             `json:"group_open_hours"`
	GroupOpenMillis    Figure             `json:"group_open_milliseconds"`
	GroupHoursCoverage GroupHoursCoverage `json:"group_hours_coverage"`
	// HoldWaitMillis is panel 8's total: milliseconds the project spent
	// waiting on a production hold, each hold a half-open interval —
	// held_since to its production_released row's at, or to the
	// report's captured as-of where the releases row still holds —
	// clipped to the span and unioned, so overlapping and duplicate
	// rows wait their time once; a hold that only touches the span's
	// boundary holds none of it. HoldWaitHours is the same total in
	// hours, the legacy unit the console kept reading.
	// HoldWaitCoverage counts the evidence the total stands on.
	HoldWaitHours      Figure           `json:"hold_wait_hours"`
	HoldWaitMillis     Figure           `json:"hold_wait_milliseconds"`
	HoldWaitCoverage   HoldWaitCoverage `json:"hold_wait_coverage"`
	ReviewsNotReviewed Figure           `json:"reviews_not_reviewed"`
	// ReviewEvidence measures panel 9's coverage: the genuine answers
	// the count above stands over, and the rows no usable evidence
	// covers — unknown, never an inferred success.
	ReviewEvidence ReviewEvidence `json:"review_evidence"`
	// PlansByRounds counts the window's closed plans by the rounds each
	// ran to its closure; CardsFirstRound and CardsLaterRounds count the
	// window's new card filings of the first round against later ones;
	// ReplansByOrigin the window's applied replans by who asked them.
	// What no explicit round or origin covers keeps its own unknown
	// bucket, never a guess.
	PlansByRounds    []NamedCount `json:"plans_by_rounds"`
	CardsFirstRound  Figure       `json:"cards_first_round"`
	CardsLaterRounds Figure       `json:"cards_later_rounds"`
	ReplansByOrigin  []NamedCount `json:"replans_by_origin"`
	// PlansByRoundsSpans compares panel 10's named counts over the
	// window, the equal window before it and all seven weeks; CardsByRound
	// panel 11's original filings by the round each stamped, rounds past
	// the first each its own name beside the first/later totals above.
	// The unknown round is an explicit bucket in both; the names order
	// fewest round first, unknown last, and a name only the previous
	// window or a weekly bucket holds is not lost.
	PlansByRoundsSpans []NamedFigure `json:"plans_by_rounds_spans"`
	CardsByRound       []NamedFigure `json:"cards_by_round"`
	// ReplansByOriginSpans compares panel 12's named counts over the
	// window, the equal window before it and all seven weeks: the
	// origins alphabetical, the unknown bucket among them, and a name
	// only the previous window or a weekly bucket holds is not lost.
	ReplansByOriginSpans []NamedFigure `json:"replans_by_origin_spans"`
}

// MetricsReport is every figure of one project over one window, the
// shape the endpoint serves and the page renders.
type MetricsReport struct {
	Repository string    `json:"repository"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`

	Deploys          Figure          `json:"deploys"`
	LeadTime         Figure          `json:"lead_time"`
	CardCycle        Figure          `json:"card_cycle"`
	Throughput       Figure          `json:"throughput"`
	ChangeFailure    Figure          `json:"change_failure"`
	RevertedRefiled  RevertedRefiled `json:"reverted_refiled"`
	TimeToRestore    Figure          `json:"time_to_restore"`
	Spend            Figure          `json:"spend"`
	PerCard          Figure          `json:"per_card"`
	TokensPerCard    Figure          `json:"tokens_per_card"`
	ModelSpend       []MixShare      `json:"model_spend"`
	ServedBy         []MixShare      `json:"served_by"`
	ToolSpend        Figure          `json:"tool_spend"`
	ToolMix          []MixShare      `json:"tool_mix"`
	Interventions    Figure          `json:"interventions"`
	Escalations      Figure          `json:"escalations"`
	EscalatedShare   Figure          `json:"escalated_share"`
	CasesPerCard     Figure          `json:"cases_per_card"`
	CasesByRung      CasesByRung     `json:"cases_by_rung"`
	HumanWait        Figure          `json:"human_wait"`
	CaseMoney        CaseMoneyFigure `json:"case_money"`
	HumanBlocked     Figure          `json:"human_blocked"`
	ReviewAcceptance Figure          `json:"review_acceptance"`
	SelfDeploys      Figure          `json:"self_deploys"`
	DiffSize         DiffSize        `json:"diff_size"`
	ChatWork         ChatWork        `json:"chat_tool_work"`
	SessionTime      SessionTime     `json:"session_time"`
	CIDuration       Figure          `json:"ci_duration"`
	DivideConquer    DivideConquer   `json:"divide_conquer"`
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

// metricBucketEnd is the exclusive end of col's weekly bucket over the
// same spark: the instant a bucket's own answer freezes at, so a fact
// that lands later — a closure, an end — never rewrites it.
func metricBucketEnd(col, spark string) string {
	return fmt.Sprintf(`%s::timestamptz + ((floor(EXTRACT(EPOCH FROM (%s - %s::timestamptz)) / 604800)) + 1) * interval '7 days'`,
		spark, col, spark)
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

// metricNamedWeeks reads a named-weekly query — rows of (bucket, name,
// count) — into each name's seven buckets, the shape the named
// comparisons below are assembled from.
func metricNamedWeeks(ctx context.Context, db *sql.DB, query string, args ...any) (map[string][7]float64, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][7]float64{}
	for rows.Next() {
		var b int
		var name string
		var v float64
		if err := rows.Scan(&b, &name, &v); err != nil {
			return nil, err
		}
		if b >= 0 && b < 7 {
			ws := out[name]
			ws[b] = v
			out[name] = ws
		}
	}
	return out, rows.Err()
}

// metricNamedWeeksTotals sums each name's weekly buckets: the name's
// own total across the spark, beside the window's and the previous
// window's counts.
func metricNamedWeeksTotals(weeks map[string][7]float64) map[string]float64 {
	out := make(map[string]float64, len(weeks))
	for name, ws := range weeks {
		var total float64
		for _, v := range ws {
			total += v
		}
		out[name] = total
	}
	return out
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

// endedCase is the cases the window's figures read: ended in the span,
// resolved or withdrawn. $2 and $3 are the span bounds.
const endedCase = `c.repository = $1 AND c.status IN ('resolved', 'withdrawn') AND c.resolved_at >= %s AND c.resolved_at < $3`

// metricCasesPerCard: the cases of the cards that ended in the window,
// monitor flags included, over those cards.
func metricCasesPerCard(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const ended = ` FROM tasks WHERE ` + cardWhere + ` AND status IN ('done', 'failed', 'closed') AND finished_at >= %s AND finished_at < $3`
	const cases = `(SELECT count(*) FROM cases c WHERE c.task_id = tasks.id)`
	return metricRatioFigure(ctx, db, repo, w,
		fmt.Sprintf(`SELECT COALESCE(sum(`+cases+`), 0)::float8, count(*)`+ended, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("finished_at")+`, COALESCE(sum(`+cases+`), 0)::float8, count(*)`+ended+` GROUP BY 1`, "$2"))
}

// metricCasesByRung: of the cases that ended in the window, the share
// resolved at each rung and the share withdrawn.
func metricCasesByRung(ctx context.Context, db *sql.DB, repo string, w metricWindow) (CasesByRung, error) {
	const from = ` FROM cases c WHERE ` + endedCase
	const rung = `c.status = 'resolved' AND c.resolution ->> 'rung' = '%d'`
	counts := `SELECT count(*)::float8`
	for i := 0; i < 4; i++ {
		counts += fmt.Sprintf(`, count(*) FILTER (WHERE `+rung+`)::float8`, i)
	}
	counts += `, count(*) FILTER (WHERE c.status = 'withdrawn')::float8` + from
	read := func(from, to time.Time) (total float64, n [5]float64, err error) {
		err = db.QueryRowContext(ctx, fmt.Sprintf(counts, "$2"), repo, from, to).Scan(&total, &n[0], &n[1], &n[2], &n[3], &n[4])
		return
	}
	var out CasesByRung
	total, n, err := read(w.From, w.To)
	if err != nil {
		return out, err
	}
	out.Has = total > 0
	for i := range n {
		share, _ := metricRatio(n[i], total)
		if i < 4 {
			out.Rung[i] = share
		} else {
			out.Withdrawn = share
		}
	}
	if total, n, err = read(w.PrevFrom, w.PrevTo); err != nil {
		return out, err
	}
	for i := range n {
		out.Prev[i], _ = metricRatio(n[i], total)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT `+metricBucket("c.resolved_at")+`, count(*) FILTER (WHERE c.status = 'resolved' AND (c.resolution ->> 'rung')::int < 3)::float8, count(*)::float8`+from+` GROUP BY 1`, "$2"), repo, w.SparkFrom, w.To)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int
		var below, all float64
		if err := rows.Scan(&b, &below, &all); err != nil {
			return out, err
		}
		if b >= 0 && b < 7 {
			out.Weeks[b], _ = metricRatio(below, all)
		}
	}
	return out, rows.Err()
}

// metricHumanWait: the median time an ended case waited at the human,
// from its first rung 3 step to its end, in milliseconds.
func metricHumanWait(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM cases c JOIN LATERAL (SELECT min(s.started_at) AS at FROM case_steps s WHERE s.case_id = c.id AND s.rung = 3) h ON h.at IS NOT NULL
		WHERE ` + endedCase
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (c.resolved_at - h.at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("c.resolved_at")+`, `+agg+pair+` GROUP BY 1`, "$2"))
}

// metricCaseMoney: over the cases resolved in the window, the card's
// session spend from each one's resolution to the card's next case or
// its end, against what each resolution cost: its steps and the draws
// that name it.
func metricCaseMoney(ctx context.Context, db *sql.DB, repo string, w metricWindow) (CaseMoneyFigure, error) {
	const q = `SELECT count(*)::float8,
		COALESCE(sum((SELECT COALESCE(sum(l.eur), 0) FROM ledger l
			WHERE l.task_id = c.task_id AND l.kind IN ('model', 'tool', 'resource') AND l.created_at > c.resolved_at
			AND l.created_at < COALESCE((SELECT min(n.opened_at) FROM cases n WHERE n.task_id = c.task_id AND n.opened_at > c.resolved_at), t.finished_at, 'infinity'))), 0)::float8,
		COALESCE(sum((SELECT COALESCE(sum(s.cost_eur), 0) FROM case_steps s WHERE s.case_id = c.id)
			+ (SELECT COALESCE(sum((e.detail ->> 'amount_eur')::numeric), 0) FROM events e
				WHERE e.task_id = c.task_id AND e.kind IN ('` + EventCardFunded + `', '` + EventCardExtended + `') AND e.detail ->> 'case_id' = c.id::text)), 0)::float8
		FROM cases c LEFT JOIN tasks t ON t.id = c.task_id
		WHERE c.repository = $1 AND c.status = 'resolved' AND c.resolved_at >= $2 AND c.resolved_at < $3`
	var m CaseMoneyFigure
	var n float64
	if err := db.QueryRowContext(ctx, q, repo, w.From, w.To).Scan(&n, &m.AfterEUR, &m.ResolveEUR); err != nil {
		return m, err
	}
	m.Has = n > 0
	var pn float64
	err := db.QueryRowContext(ctx, q, repo, w.PrevFrom, w.PrevTo).Scan(&pn, &m.Prev[0], &m.Prev[1])
	return m, err
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

// metricSessionTime: where the window's session time went, from the
// session_time rows (EventSessionTime).
func metricSessionTime(ctx context.Context, db *sql.DB, repo string, w metricWindow) (SessionTime, error) {
	const rows = ` FROM events WHERE kind = '` + EventSessionTime + `' AND repository = $1 AND at >= $2 AND at < $3`
	const wall = `(detail ->> 'wall_ms')::float8`
	var checks []string
	for _, c := range policy.CheckClasses {
		checks = append(checks, fmt.Sprintf(`COALESCE((detail #>> '{classes,%s,ms}')::float8, 0)`, c))
	}
	share := func(num string) (Figure, error) {
		return metricRatioFigure(ctx, db, repo, w,
			`SELECT COALESCE(sum(`+num+`), 0), COALESCE(sum(`+wall+`), 0)`+rows,
			`SELECT `+metricBucket("at")+`, COALESCE(sum(`+num+`), 0), COALESCE(sum(`+wall+`), 0)`+rows+` GROUP BY 1`)
	}
	var st SessionTime
	var err error
	if st.CheckShare, err = share(strings.Join(checks, " + ")); err != nil {
		return st, err
	}
	if st.CommandShare, err = share(`(detail ->> 'commands_ms')::float8`); err != nil {
		return st, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(`+wall+`), 0) / 3600000`+rows, repo, w.From, w.To).Scan(&st.Sessions, &st.WallHours); err != nil {
		return st, err
	}
	const each = ` WHERE e.kind = '` + EventSessionTime + `' AND e.repository = $1 AND e.at >= $2 AND e.at < $3`
	hours := func(query string) (map[string]float64, error) {
		rs, err := db.QueryContext(ctx, query+each+` GROUP BY 1`, repo, w.From, w.To)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		var out map[string]float64
		for rs.Next() {
			var name string
			var h float64
			if err := rs.Scan(&name, &h); err != nil {
				return nil, err
			}
			if out == nil {
				out = map[string]float64{}
			}
			out[name] = h
		}
		return out, rs.Err()
	}
	if st.ClassHours, err = hours(`SELECT c.key, sum((c.value ->> 'ms')::float8) / 3600000 FROM events e CROSS JOIN LATERAL jsonb_each(e.detail -> 'classes') c`); err != nil {
		return st, err
	}
	if st.ToolHours, err = hours(`SELECT c.key, sum(c.value::float8) / 3600000 FROM events e CROSS JOIN LATERAL jsonb_each_text(e.detail -> 'tools') c`); err != nil {
		return st, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY r.v::float8), 0) / 1000
		FROM events e CROSS JOIN LATERAL jsonb_array_elements_text(e.detail #> '{classes,check,runs}') AS r(v)`+each, repo, w.From, w.To).Scan(&st.CheckRuns, &st.CheckMedianS); err != nil {
		return st, err
	}
	err = db.QueryRowContext(ctx, `SELECT detail #>> '{longest,class}', (detail #>> '{longest,ms}')::float8 / 1000, detail #>> '{longest,command}'`+rows+
		` AND detail ? 'longest' ORDER BY (detail #>> '{longest,ms}')::float8 DESC, at LIMIT 1`, repo, w.From, w.To).Scan(&st.Longest.Class, &st.Longest.Seconds, &st.Longest.Command)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return st, err
}

// metricCIDuration: the median Actions run, push to finish.
func metricCIDuration(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM ci_runs WHERE repository = $1 AND pushed_at >= $2 AND pushed_at < $3`,
		`SELECT `+metricBucket("pushed_at")+", "+agg+` FROM ci_runs WHERE repository = $1 AND pushed_at >= $2 AND pushed_at < $3 GROUP BY 1`)
}

// metricEconomics: the plan's panels 4 and 5 (roadmap 336), over the
// same first-merge cohort — EUR per merged line, the cohort's whole-life
// actual euros over its merged changed lines, and the fixed EUR per
// card, each size's intercept of an ordinary least squares of a card's
// actual euros on its changed lines. A card whose reading or whose cost
// is unknown sits both panels out, named in the coverage: unknown is
// never an invented zero, and an undefined fit is never zero either.
// Intercepts keep their sign: a size whose euros fall as lines grow
// reads negative.
func metricEconomics(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, []SizeIntercept, EconomicsCoverage, error) {
	lo := w.SparkFrom
	if w.PrevFrom.Before(lo) {
		lo = w.PrevFrom
	}
	cards, err := metricEconPull(ctx, db, repo, lo, w.To)
	if err != nil {
		return Figure{}, nil, EconomicsCoverage{}, err
	}
	var f Figure
	var cov EconomicsCoverage
	var cur, prev [2]float64          // window and previous: [euros, lines]
	var weeks [7][2]float64           // each bucket: [euros, lines]
	fits := map[string][][2]float64{} // size -> its cards' [lines, euros]
	for _, c := range cards {
		if withinWindow(c.merged, w.From, w.To) {
			cov.Cards++
			switch {
			case !c.read:
				cov.MissingLines++
			case c.size == "":
				cov.MissingSize++
			}
			if !c.cost {
				cov.MissingCost++
			}
		}
		if !c.read || !c.cost {
			continue
		}
		switch {
		case withinWindow(c.merged, w.From, w.To):
			cur[0], cur[1] = cur[0]+c.eur, cur[1]+c.lines
			if c.size != "" {
				fits[c.size] = append(fits[c.size], [2]float64{c.lines, c.eur})
			}
		case withinWindow(c.merged, w.PrevFrom, w.PrevTo):
			prev[0], prev[1] = prev[0]+c.eur, prev[1]+c.lines
		}
		if withinWindow(c.merged, w.SparkFrom, w.To) {
			if b := weekBucket(c.merged, w.SparkFrom); b >= 0 && b < 7 {
				weeks[b][0], weeks[b][1] = weeks[b][0]+c.eur, weeks[b][1]+c.lines
			}
		}
	}
	f.Value, f.Has = metricRatio(cur[0], cur[1])
	f.Prev, f.PrevHas = metricRatio(prev[0], prev[1])
	for b := range f.Weeks {
		f.Weeks[b], _ = metricRatio(weeks[b][0], weeks[b][1])
	}
	sizes := make([]string, 0, len(fits))
	for size := range fits {
		sizes = append(sizes, size)
	}
	sort.Strings(sizes)
	var out []SizeIntercept
	for _, size := range sizes {
		si := SizeIntercept{Size: size, Cards: len(fits[size])}
		if y, ok := metricOLSIntercept(fits[size]); ok {
			si.EUR, si.Available = y, true
		}
		out = append(out, si)
	}
	return f, out, cov, nil
}

// metricEconCard is one card of the economics cohort: its first merge,
// its final gate_head reading (read false: none exists) and its
// lifetime euros (cost false: the ledger holds no row — unknown spend,
// never zero).
type metricEconCard struct {
	merged time.Time
	size   string
	lines  float64
	eur    float64
	read   bool
	cost   bool
}

// metricEconPull reads the repository's first-merged cards whose first
// merge falls in [lo, hi): DISTINCT ON keeps one row per card before
// the joins, so duplicate merges and the reading and spend lookups
// never fan a card out. The reading is the card's final gate_head fact
// for the head the task actually built on (task.sha), so a later
// abandoned head's reading cannot replace the merged head's; the spend
// is the card's whole ledger life, every phase, not the span's slice. A
// gate_head without diff statistics stays unavailable — its line count
// is NULL, never a measured zero.
func metricEconPull(ctx context.Context, db *sql.DB, repo string, lo, hi time.Time) ([]metricEconCard, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.at, g.size, g.lines, s.eur
		FROM (SELECT DISTINCT ON (e.task_id) e.task_id, e.at
			FROM events e JOIN tasks t ON t.id = e.task_id
			WHERE e.kind = '`+EventPullRequestMerged+`' AND t.repository = $1 AND t.kind <> 'chat'
				AND t.source <> '`+SourceRoll+`' AND e.at < $3
			ORDER BY e.task_id, e.at, e.id) m
		JOIN tasks t ON t.id = m.task_id
		LEFT JOIN LATERAL (SELECT gh.detail ->> 'size' AS size,
				(gh.detail ->> 'insertions')::float8 + (gh.detail ->> 'deletions')::float8 AS lines
			FROM events gh WHERE gh.kind = '`+EventGateHead+`' AND gh.task_id = m.task_id
				AND gh.detail ->> 'head' = t.sha
			ORDER BY gh.id DESC LIMIT 1) g ON true
		LEFT JOIN LATERAL (SELECT sum(l.eur)::float8 AS eur FROM ledger l WHERE l.task_id = m.task_id) s ON true
		WHERE m.at >= $2`, repo, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metricEconCard
	for rows.Next() {
		var c metricEconCard
		var size sql.NullString
		var lines, eur sql.NullFloat64
		if err := rows.Scan(&c.merged, &size, &lines, &eur); err != nil {
			return nil, err
		}
		c.size, c.lines, c.eur, c.read, c.cost = size.String, lines.Float64, eur.Float64, lines.Valid, eur.Valid
		out = append(out, c)
	}
	return out, rows.Err()
}

// metricOLSIntercept is the ordinary least squares of a size's cards'
// euros on their changed lines: the intercept where the line of best
// fit meets the euros axis, sign kept. Two distinct line values make a
// fit; fewer leave it undefined — never zero.
func metricOLSIntercept(pts [][2]float64) (float64, bool) {
	distinct := map[float64]bool{}
	var sx, sy float64
	for _, p := range pts {
		distinct[p[0]] = true
		sx, sy = sx+p[0], sy+p[1]
	}
	if len(distinct) < 2 {
		return 0, false
	}
	n := float64(len(pts))
	xbar, ybar := sx/n, sy/n
	var num, den float64
	for _, p := range pts {
		dx := p[0] - xbar
		num, den = num+dx*(p[1]-ybar), den+dx*dx
	}
	if den == 0 {
		return 0, false
	}
	return ybar - num/den*xbar, true
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
	// asof is the instant this report was taken, captured once at the
	// store's microsecond precision: the one end every still-open
	// interval — a production hold — clips to, so the panels of one
	// report agree on it
	asof := time.Now().UTC().Truncate(time.Microsecond)
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
	if r.CasesPerCard, err = metricCasesPerCard(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CasesByRung, err = metricCasesByRung(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.HumanWait, err = metricHumanWait(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CaseMoney, err = metricCaseMoney(ctx, db, repository, w); err != nil {
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
	if r.SessionTime, err = metricSessionTime(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CIDuration, err = metricCIDuration(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.DivideConquer, err = metricDivideConquer(ctx, db, repository, w, asof); err != nil {
		return r, err
	}
	if r.DivideConquer.EurPerMergedLine, r.DivideConquer.FixedEurPerCard, r.DivideConquer.EconomicsCoverage, err = metricEconomics(ctx, db, repository, w); err != nil {
		return r, err
	}
	return r, nil
}

// The divide-and-conquer panels this store answers today (roadmap 336):
// cards above their size bound at the merge gate, sessions per merged
// card by historical size, session ends by cause and reviews that
// answered not reviewed. Every panel reads hard rows the factory
// already stores — one row per fact, each recorded through its payload
// with an identity key — joined to tasks for the project's card reach
// (no chat task, no roll card). A span no row covers is unknown, never
// zero: a target-of-none panel needs evidence before it can name none.

const dcCardReach = `t.repository = $1 AND t.kind <> 'chat' AND t.source <> '` + SourceRoll + `'`

// metricEvidenceCount is the shape of a target-of-none panel: the count
// of the window's offending rows over the span's evidence, unknown when
// no row covers the span — a measured zero needs evidence, never a
// missing one. The scalar returns (num, evidence) and the weeks query
// returns (bucket, num).
func metricEvidenceCount(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	var f Figure
	read := func(from, to time.Time) (n, evidence float64, err error) {
		err = db.QueryRowContext(ctx, scalar, repo, from, to).Scan(&n, &evidence)
		return
	}
	n, ev, err := read(w.From, w.To)
	if err != nil {
		return f, err
	}
	f.Value, f.Has = n, ev > 0
	if n, ev, err = read(w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Prev, f.PrevHas = n, ev > 0
	f.Weeks, err = metricWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	return f, err
}

// metricDivideConquer populates the divide-and-conquer block the store
// answers today: CoveredFrom, GateAboveBound with GateBoundCoverage,
// SessionsPerCard with its coverage, SessionEnds, ReviewsNotReviewed
// with ReviewEvidence, the cuts with their closed groups, the group and
// hold times (panels 7 and 8) and the planning panels — plans by
// rounds, card filings by round, replans by origin. The economics
// panels (4 and 5) compose beside this at the caller.
func metricDivideConquer(ctx context.Context, db *sql.DB, repo string, w metricWindow, asof time.Time) (DivideConquer, error) {
	var d DivideConquer
	var covered sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT min(e.at) FROM events e JOIN tasks t ON t.id = e.task_id WHERE `+dcCardReach+` AND e.identity_key IS NOT NULL AND e.kind = ANY($2)`,
		repo, []string{EventGateHead, EventSessionStarted, EventSessionEnd, EventModelReview}).Scan(&covered); err != nil {
		return d, err
	}
	if covered.Valid {
		d.CoveredFrom = covered.Time
	}
	var err error
	if d.GateAboveBound, d.GateBoundCoverage, err = metricDivideGateBound(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.SessionsPerCard, d.SessionsCoverage, err = metricDivideSessions(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.SessionEnds, err = metricDivideSessionEnds(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.ReviewsNotReviewed, d.ReviewEvidence, err = metricDivideReviews(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.Cuts, d.CutGroupsClosed, err = metricDivideCuts(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.GroupOpenMillis, d.GroupOpenHours, d.GroupHoursCoverage, err = metricDivideGroupHours(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.HoldWaitMillis, d.HoldWaitHours, d.HoldWaitCoverage, err = metricDivideHoldWaits(ctx, db, repo, w, asof); err != nil {
		return d, err
	}
	if d.PlansByRounds, d.PlansByRoundsSpans, err = metricDividePlansByRounds(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.CardsFirstRound, d.CardsLaterRounds, d.CardsByRound, err = metricDivideCardFilings(ctx, db, repo, w); err != nil {
		return d, err
	}
	if d.ReplansByOrigin, d.ReplansByOriginSpans, err = metricDivideReplans(ctx, db, repo, w); err != nil {
		return d, err
	}
	return d, nil
}

// metricDivideGateBound — panel 1: distinct cards reaching the merge
// gate above their size bound (target: none). A card counts once
// however many heads and retries crossed; the bound and the lines are
// the ones the reading captured, never today's policy or the card's
// metadata now. Only a complete reading is evidence — the fact's
// identity, a bound, both line counts and the size whose bound it
// planned; a hole the coverage names measures nothing, so a span no
// complete reading covers is unknown, not zero.
func metricDivideGateBound(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, GateBoundCoverage, error) {
	const from = ` FROM events e JOIN tasks t ON t.id = e.task_id
		WHERE ` + dcCardReach + ` AND e.kind = 'gate_head' AND e.detail->>'gate' = 'merge' AND e.at >= %s AND e.at < $3`
	// measured: the reading carries its whole comparison — the fact's
	// identity, the bound, both line counts and a size that planned the
	// bound; none of it is filled from the policy in force now
	const measured = `e.identity_key IS NOT NULL AND (e.detail->>'bound') IS NOT NULL
		AND (e.detail->>'insertions') IS NOT NULL AND (e.detail->>'deletions') IS NOT NULL
		AND COALESCE(e.detail->>'size', '') <> ''`
	const over = `(e.detail->>'insertions')::int + (e.detail->>'deletions')::int > (e.detail->>'bound')::int`
	f, err := metricEvidenceCount(ctx, db, repo, w,
		fmt.Sprintf(`SELECT count(DISTINCT e.task_id) FILTER (WHERE `+measured+` AND `+over+`),
			count(DISTINCT e.task_id) FILTER (WHERE `+measured+`)`+from, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("e.at")+", count(DISTINCT e.task_id) FILTER (WHERE "+measured+` AND `+over+`)`+from+` GROUP BY 1`, "$2"))
	var cov GateBoundCoverage
	if err != nil {
		return f, cov, err
	}
	err = db.QueryRowContext(ctx, `SELECT count(*)::int, count(*) FILTER (WHERE `+measured+`)::int,
		count(*) FILTER (WHERE (e.detail->>'bound') IS NULL)::int,
		count(*) FILTER (WHERE (e.detail->>'insertions') IS NULL OR (e.detail->>'deletions') IS NULL)::int,
		count(*) FILTER (WHERE (e.detail->>'size') IS NULL)::int,
		count(*) FILTER (WHERE e.detail->>'size' = '')::int,
		count(*) FILTER (WHERE e.identity_key IS NULL)::int`+fmt.Sprintf(from, "$2"),
		repo, w.From, w.To).
		Scan(&cov.Readings, &cov.Measured, &cov.MissingBound, &cov.MissingLines,
			&cov.UnknownSize, &cov.UnsizedSize, &cov.Identityless)
	return f, cov, err
}

// metricDivideSessions — panel 2: mean lifetime distinct named starts
// per merged card, grouped by the historical size the merge gate's
// reading planned at the head the card actually built on — never the
// task row's mutable size. The cohort is cards whose first merge lands
// in the span; the card's starts are its distinct named session_started
// rows across all time, a start before the window included. A card no
// reading at its head covers has no historical size and sits the
// averages out; so does one whose start evidence is missing — merged
// before the repository's first named start, so its silence is the
// recorder's, not the card's — or partial, its ends naming sessions no
// start row names: neither is a measured zero, and every hole is
// counted in the coverage, never guessed into an average. A size absent
// from the measured cohort is absent from the slice.
func metricDivideSessions(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]SizeFigure, SessionsCoverage, error) {
	lo := w.SparkFrom
	if w.PrevFrom.Before(lo) {
		lo = w.PrevFrom
	}
	cards, err := metricSessionPull(ctx, db, repo, lo, w.To)
	if err != nil {
		return nil, SessionsCoverage{}, err
	}
	// the repository's first named start bounds when start recording
	// began: a card merged before it holds no start evidence
	var firstStart sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT min(e.at) FROM events e JOIN tasks t ON t.id = e.task_id
		WHERE t.repository = $1 AND e.kind = 'session_started' AND e.detail->>'session' <> ''`, repo).Scan(&firstStart); err != nil {
		return nil, SessionsCoverage{}, err
	}
	var cov SessionsCoverage
	for _, c := range cards {
		if !withinWindow(c.merged, w.From, w.To) {
			continue
		}
		cov.Cards++
		switch {
		case !c.size.Valid:
			cov.UnknownSize++
		case c.size.String == "":
			cov.Unsized++
		}
		if !c.evidenced(firstStart) {
			if c.started == 0 {
				cov.MissingStarts++
			}
			if c.endsNoStart > 0 {
				cov.PartialStarts++
			}
		}
	}
	figs := map[string]*SizeFigure{}
	at := func(size string) *SizeFigure {
		if f, ok := figs[size]; ok {
			return f
		}
		f := &SizeFigure{Size: size}
		figs[size] = f
		return f
	}
	type totals struct{ starts, cards float64 }
	spans := []struct {
		lo, hi time.Time
		set    func(f *Figure, v float64, has bool)
	}{
		{w.From, w.To, func(f *Figure, v float64, has bool) { f.Value, f.Has = v, has }},
		{w.PrevFrom, w.PrevTo, func(f *Figure, v float64, has bool) { f.Prev, f.PrevHas = v, has }},
	}
	for _, sp := range spans {
		tots := map[string]*totals{}
		for _, c := range cards {
			if !c.size.Valid || !c.evidenced(firstStart) || !withinWindow(c.merged, sp.lo, sp.hi) {
				continue
			}
			t, ok := tots[c.size.String]
			if !ok {
				t = &totals{}
				tots[c.size.String] = t
			}
			t.starts, t.cards = t.starts+c.started, t.cards+1
		}
		for size, t := range tots {
			v, has := metricRatio(t.starts, t.cards)
			sp.set(&at(size).Figure, v, has)
		}
	}
	var weekTots [7]map[string]*totals
	for _, c := range cards {
		if !c.size.Valid || !c.evidenced(firstStart) {
			continue
		}
		b := weekBucket(c.merged, w.SparkFrom)
		if b < 0 || b >= len(weekTots) || !withinWindow(c.merged, w.SparkFrom, w.To) {
			continue
		}
		if weekTots[b] == nil {
			weekTots[b] = map[string]*totals{}
		}
		t, ok := weekTots[b][c.size.String]
		if !ok {
			t = &totals{}
			weekTots[b][c.size.String] = t
		}
		t.starts, t.cards = t.starts+c.started, t.cards+1
	}
	for b, sizes := range weekTots {
		for size, t := range sizes {
			at(size).Figure.Weeks[b], _ = metricRatio(t.starts, t.cards)
		}
	}
	out := make([]SizeFigure, 0, len(figs))
	for _, f := range figs {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size < out[j].Size })
	return out, cov, nil
}

// metricSessionCard is one card of the sessions cohort: its first merge,
// the historical size the merge gate's reading planned at the head the
// card actually built on (valid false: no reading covers the head — an
// abandoned head's reading is another head's), its lifetime distinct
// named starts, and the sessions its terminal ends name that no start
// row names — partial start evidence: the end is a fact, the missing
// start is never invented from it.
type metricSessionCard struct {
	merged      time.Time
	size        sql.NullString
	started     float64
	endsNoStart float64
}

// evidenced tells whether the card's starts are evidence, not silence:
// a start row of its own, or a merge after the repository began
// recording starts, with no ended session its start rows fail to name.
func (c metricSessionCard) evidenced(firstStart sql.NullTime) bool {
	if c.endsNoStart > 0 {
		return false
	}
	return c.started > 0 || firstStart.Valid && !c.merged.Before(firstStart.Time)
}

// metricSessionPull reads the repository's first-merged cards whose
// first merge falls in [lo, hi) — the economics pull's own shape:
// DISTINCT ON keeps one row per card before the joins, so duplicate
// merges and the lookups never fan a card out. The reading is the
// card's final gate_head fact at the head the task actually built on
// (task.sha), so an abandoned head's reading cannot stand in for the
// merged head's; the starts and the ends-without-starts are the card's
// lifetime rows, unbounded below.
func metricSessionPull(ctx context.Context, db *sql.DB, repo string, lo, hi time.Time) ([]metricSessionCard, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.at, g.size,
		(SELECT count(DISTINCT st.detail->>'session')::float8 FROM events st
			WHERE st.task_id = m.task_id AND st.kind = 'session_started' AND st.detail->>'session' <> '') AS starts,
		(SELECT count(DISTINCT en.detail->>'session')::float8 FROM events en
			WHERE en.task_id = m.task_id AND en.kind = 'session_end' AND en.detail->>'outcome' <> 'not_started'
				AND NOT EXISTS (SELECT 1 FROM events st WHERE st.task_id = en.task_id
					AND st.kind = 'session_started' AND st.detail->>'session' = en.detail->>'session')) AS ends_no_start
		FROM (SELECT DISTINCT ON (e.task_id) e.task_id, e.at
			FROM events e JOIN tasks t ON t.id = e.task_id
			WHERE e.kind = 'pull_request_merged' AND t.repository = $1 AND t.kind <> 'chat'
				AND t.source <> 'roll' AND e.at < $3
			ORDER BY e.task_id, e.at, e.id) m
		JOIN tasks t ON t.id = m.task_id
		LEFT JOIN LATERAL (SELECT gh.detail->>'size' AS size
			FROM events gh WHERE gh.kind = 'gate_head' AND gh.task_id = m.task_id
				AND gh.detail->>'head' = t.sha
			ORDER BY gh.id DESC LIMIT 1) g ON true
		WHERE m.at >= $2`, repo, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metricSessionCard
	for rows.Next() {
		var c metricSessionCard
		if err := rows.Scan(&c.merged, &c.size, &c.started, &c.endsNoStart); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// metricDivideSessionEnds — panel 3: the window's session ends by
// cause, ordered by count descending then name. The causes are the
// session outcomes the runner records; a not_started end is no
// session's — a pod that never became one — so however its row landed,
// it inflates no distribution. An end whose start row is missing stays
// the fact it is: the ends panel keeps it, the sessions panel's
// coverage names the hole.
func metricDivideSessionEnds(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]NamedCount, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.detail->>'outcome', count(*)::float8
		FROM events e JOIN tasks t ON t.id = e.task_id
		WHERE `+dcCardReach+` AND e.kind = 'session_end' AND e.detail->>'outcome' <> 'not_started'
			AND e.at >= $2 AND e.at < $3
		GROUP BY 1 ORDER BY count(*) DESC, 1`, repo, w.From, w.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NamedCount
	for rows.Next() {
		var nc NamedCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			return nil, err
		}
		out = append(out, nc)
	}
	return out, rows.Err()
}

// metricDivideReviews — panel 9: reviews that answered not reviewed
// (target: none). Only a genuine answer is evidence: a row that carries
// its attempt identity — head and round, the persistence contract that
// keeps one attempt one row, so a replay of one attempt adds no answer
// while a second genuine round of the same head and a round of another
// head each stay their own — and the classification the payload wrote:
// reviewed true, the answer judged the head, a negative verdict among
// them, or reviewed false with its bounded reason. A row without its
// identity or without a usable classification is unknown evidence,
// never an inferred success: the count stands on the genuine rest, and
// a span no genuine answer covers is unknown, not zero.
func metricDivideReviews(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, ReviewEvidence, error) {
	const from = ` FROM events e JOIN tasks t ON t.id = e.task_id
		WHERE ` + dcCardReach + ` AND e.kind = 'model_review' AND e.at >= %s AND e.at < $3`
	// COALESCE holds the classification boolean to FALSE where the
	// payload wrote none: an AND with SQL's NULL would answer NULL, and
	// NOT NULL drops the row from the unknown filter too — a keyed row
	// without its classification would count nowhere, hole or answer
	const genuine = `e.identity_key IS NOT NULL AND COALESCE(e.detail->>'reviewed' IN ('true', 'false'), FALSE)`
	const notReviewed = genuine + ` AND e.detail->>'reviewed' = 'false'`
	const unknown = `NOT (` + genuine + `)`
	// the evidence of Answers and Unknown is every model_review row: a
	// span rows cover measures, its genuine answers and its holes apart
	of := func(what string) (string, string) {
		return fmt.Sprintf(`SELECT count(*) FILTER (WHERE `+what+`), count(*)`+from, "$2"),
			fmt.Sprintf(`SELECT `+metricBucket("e.at")+", count(*) FILTER (WHERE "+what+`)`+from+` GROUP BY 1`, "$2")
	}
	f, err := metricEvidenceCount(ctx, db, repo, w,
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE `+notReviewed+`), count(*) FILTER (WHERE `+genuine+`)`+from, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("e.at")+", count(*) FILTER (WHERE "+notReviewed+`)`+from+` GROUP BY 1`, "$2"))
	var ev ReviewEvidence
	if err != nil {
		return f, ev, err
	}
	scalar, weeks := of(genuine)
	if ev.Answers, err = metricEvidenceCount(ctx, db, repo, w, scalar, weeks); err != nil {
		return f, ev, err
	}
	scalar, weeks = of(unknown)
	ev.Unknown, err = metricEvidenceCount(ctx, db, repo, w, scalar, weeks)
	return f, ev, err
}

// dcRevisionIdentity holds a plan_replanned row of the identity
// contract's era to the revision's own identity: the contract stamps the
// plan and the revision alone on a revision-level row, and one more
// field after the revision — a card's own row its key — makes the row a
// filing's, never the revision's. A key-level row, a card's own whose
// issue never landed, carries its key in the identity and no card in
// the detail: only the identity tells it from the revision's, and it is
// neither the revision's applied replan nor its evidence.
func dcRevisionIdentity(a string) string {
	return a + `.identity_key = ('` + EventPlanReplanned + ` ' || ` + a + `.task_id || ' ' || (` + a + `.detail->>'revision'))`
}

// dcLegacyFiling keeps a plan_replanned row of the identity contract's
// era apart from one producers wrote before it: an identity-less row
// survives only where no canonical revision-level row of its own
// repository holds the replan as of asOf — a predicate on k.at the
// caller phrases in its span's own bounds, a closure reading inclusive
// (evidence the closure itself recorded holds the replan as of it) and
// a window's end exclusive, the bound its own cohort keeps, so a
// replacement exactly at the end — outside the cohort — suppresses
// nothing it cannot count, and one arriving later never rewrites the
// older answer. A replacement another repository recorded, however
// near, suppresses nothing here. A card's own canonical filing row
// never suppresses a revision-level one — the two are separate
// identities.
func dcLegacyFiling(asOf string) string {
	return `e.identity_key IS NOT NULL OR NOT EXISTS (
	SELECT 1 FROM events k WHERE k.task_id = e.task_id AND k.repository = e.repository AND k.kind = 'plan_replanned'
		AND k.identity_key IS NOT NULL AND k.detail->>'card' IS NULL
		AND k.detail->>'revision' = e.detail->>'revision' AND ` + asOf + `)`
}

// dcRoundOf reads the round a filing row names, 0 where it names none —
// a plan filed before rounds names no round, and none is guessed.
const dcRoundOf = `CASE WHEN (e.detail->>'round') ~ '^[0-9]+$' THEN (e.detail->>'round')::int ELSE 0 END`

// dcRoundCounts reads one span's named counts — query answers rows of
// (name, count) over the span's own $2 and $3 bounds and the project's
// $1 — into the span's name-to-count map.
func dcRoundCounts(ctx context.Context, db *sql.DB, repo, query string, from, to time.Time) (map[string]float64, error) {
	rows, err := db.QueryContext(ctx, query, repo, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var name string
		var n float64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// dcNamedFigures assembles the named comparison from the three spans'
// own answers: every name any span holds — a name only the previous
// window or a weekly bucket holds is not lost — each a plain count's
// Figure, zero wherever the span does not hold it, never absent.
func dcNamedFigures(cur, prev map[string]float64, weeks map[string][7]float64) []NamedFigure {
	names := map[string]bool{}
	for name := range cur {
		names[name] = true
	}
	for name := range prev {
		names[name] = true
	}
	for name := range weeks {
		names[name] = true
	}
	out := make([]NamedFigure, 0, len(names))
	for name := range names {
		out = append(out, NamedFigure{Name: name, Figure: Figure{
			Value: cur[name], Prev: prev[name], Has: true, PrevHas: true, Weeks: weeks[name]}})
	}
	return out
}

// dcRoundFigureOrder orders the named round figures in place: explicit
// rounds fewest first, every name that is no number — the unknown
// bucket among them — after them and alphabetical among themselves.
func dcRoundFigureOrder(figs []NamedFigure) {
	sort.Slice(figs, func(i, j int) bool {
		a, aErr := strconv.Atoi(figs[i].Name)
		b, bErr := strconv.Atoi(figs[j].Name)
		switch {
		case aErr == nil && bErr == nil && a != b:
			return a < b
		case aErr != bErr:
			return aErr == nil
		default:
			return figs[i].Name < figs[j].Name
		}
	})
}

// metricDivideCuts — panel 6: the window's applied cuts, and the cut
// groups the span's own end saw closed. A cut is the CutFact row the
// replan that applied it records — identity the plan and the cut card,
// so a retried filing counts once and a request never does: the
// console's card_replanned, a pending cut step, a plan_replanned row of
// origin cut are asks, not cuts. A group closes when its plan's
// plan_closed row lands; each span reads closure only at its own end —
// the window at To, a bucket at the bucket's end — so a closure later
// than that never rewrites the older answer, and a group cut twice is
// still one group.
func metricDivideCuts(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, Figure, error) {
	const from = ` FROM events e JOIN tasks t ON t.id = e.task_id
		WHERE ` + dcCardReach + ` AND e.kind = 'cut' AND e.identity_key IS NOT NULL AND e.at >= $2 AND e.at < $3`
	// closed(end): the cut's group holds its plan_closed row before end,
	// on the project's own plan task
	closed := func(end string) string {
		return `EXISTS (SELECT 1 FROM events pc JOIN tasks gt ON gt.id = pc.task_id
			WHERE pc.task_id = e.detail->>'group' AND pc.kind = 'plan_closed' AND pc.at < ` + end + `
				AND gt.repository = $1)`
	}
	cuts, err := metricCount(ctx, db, repo, w,
		`SELECT count(DISTINCT e.identity_key)`+from,
		`SELECT `+metricBucket("e.at")+", count(DISTINCT e.identity_key)"+from+` GROUP BY 1`)
	if err != nil {
		return cuts, Figure{}, err
	}
	groups, err := metricCount(ctx, db, repo, w,
		`SELECT count(DISTINCT e.detail->>'group')`+from+` AND `+closed("$3"),
		`SELECT `+metricBucket("e.at")+", count(DISTINCT e.detail->>'group')"+from+
			` AND `+closed(metricBucketEnd("e.at", "$2"))+` GROUP BY 1`)
	return cuts, groups, err
}

// metricDividePlansByRounds — panel 10: the plans the span saw close,
// by the rounds each ran to its closure. A plan closes once, at its
// first actual plan_closed row — a duplicate close, in the span or
// before it, inflates nothing — and belongs to the span that closure
// lands in. A plan's rounds are its maximum explicit round as of that
// closure — the round a filing's rows name, and the plan_revisions the
// store kept, none later than the closure itself: never a count of
// revisions, never a round a revision filed after the close, never a
// canonical replacement that arrived after it. A plan no explicit round
// covers, filed before rounds, keeps its own unknown bucket. The scalar
// keeps the window's own names; the spans compare every name the
// window, the equal window before it and the seven weeks hold.
func metricDividePlansByRounds(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]NamedCount, []NamedFigure, error) {
	// rounds(c): the maximum explicit round no later than the closure,
	// over the repository's own eligible evidence alone — a row another
	// repository recorded on the same task, however high the round it
	// names, inflates nothing. The legacy double of a revision a
	// canonical revision-level row holds is dropped at the closure's own
	// cutoff, so evidence a later row would carry never rewrites the
	// closed answer.
	body := `WITH closed AS (
			SELECT e.task_id, min(e.at) AS closed_at
			FROM events e JOIN tasks t ON t.id = e.task_id
			WHERE ` + dcCardReach + ` AND e.kind = 'plan_closed' AND e.at < $3
			GROUP BY e.task_id
		)
		SELECT c.closed_at, GREATEST(
			COALESCE((SELECT max(` + dcRoundOf + `) FROM events e
				WHERE e.task_id = c.task_id AND e.repository = $1 AND e.kind = 'plan_replanned' AND e.at <= c.closed_at
					AND (` + dcLegacyFiling("k.at <= c.closed_at") + `)), 0),
			COALESCE((SELECT max((v.plan->>'round')::int) FROM plan_revisions v
				WHERE v.task_id = c.task_id AND (v.plan->>'round') ~ '^[0-9]+$' AND v.created_at <= c.closed_at), 0)
		) AS rounds
		FROM closed c WHERE c.closed_at >= $2 AND c.closed_at < $3`
	counts := fmt.Sprintf(`SELECT CASE WHEN r.rounds > 0 THEN r.rounds::text ELSE 'unknown' END AS name, count(*)::float8
		FROM (%s) r GROUP BY 1`, body)
	weeks := fmt.Sprintf(`SELECT %s, CASE WHEN r.rounds > 0 THEN r.rounds::text ELSE 'unknown' END AS name, count(*)::float8
		FROM (%s) r GROUP BY 1, 2`, metricBucketFrom("r.closed_at", "$2"), body)
	cur, err := dcRoundCounts(ctx, db, repo, counts, w.From, w.To)
	if err != nil {
		return nil, nil, err
	}
	prev, err := dcRoundCounts(ctx, db, repo, counts, w.PrevFrom, w.PrevTo)
	if err != nil {
		return nil, nil, err
	}
	wk, err := metricNamedWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	if err != nil {
		return nil, nil, err
	}
	spans := dcNamedFigures(cur, prev, wk)
	dcRoundFigureOrder(spans)
	// the scalar keeps the window's own names alone, fewest round first
	var scalar []NamedCount
	for name, n := range cur {
		scalar = append(scalar, NamedCount{Name: name, Count: n})
	}
	slices.SortFunc(scalar, func(a, b NamedCount) int {
		an, aErr := strconv.Atoi(a.Name)
		bn, bErr := strconv.Atoi(b.Name)
		if aErr != bErr {
			if aErr == nil {
				return -1
			}
			return 1
		}
		if aErr == nil && an != bn {
			return an - bn
		}
		return strings.Compare(a.Name, b.Name)
	})
	return scalar, spans, nil
}

// metricDivideCardFilings — panel 11: the span's new card filings, the
// first round's against later rounds', and each explicit round its own
// named figure. A filing is a card's own canonical plan_replanned row —
// the card's issue and plan key on it, the round the filing stamped —
// and the original filing's round and instant count once: each card
// key's original filing is selected across repository history before
// any cohort bound, so a key first filed in an earlier window and
// refiled in this one contributes only at its original time and round,
// wherever the panels read it, and a retained card, which refiles
// nothing, adds none. A filing that names no round, of a plan filed
// before rounds, is neither first nor later and keeps the explicit
// unknown bucket of the named comparison; the panel never guesses it a
// round.
func metricDivideCardFilings(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, Figure, []NamedFigure, error) {
	// one row per plan and key, the earliest across repository history:
	// DISTINCT ON keeps the original filing of a key any later revision
	// filed again, before the span bounds below choose their cohorts —
	// and the repository's own rows alone are eligible, so a row another
	// repository recorded on the same task files no card here
	const filings = `SELECT DISTINCT ON (e.task_id, e.detail->>'key') e.at, ` + dcRoundOf + ` AS round
		FROM events e JOIN tasks t ON t.id = e.task_id
		WHERE ` + dcCardReach + ` AND e.repository = $1 AND e.kind = 'plan_replanned' AND e.identity_key IS NOT NULL
			AND e.detail->>'card' IS NOT NULL
		ORDER BY e.task_id, e.detail->>'key', e.at, e.id`
	counts := fmt.Sprintf(`SELECT CASE WHEN f.round > 0 THEN f.round::text ELSE 'unknown' END AS name, count(*)::float8
		FROM (%s) f WHERE f.at >= $2 AND f.at < $3 GROUP BY 1`, filings)
	weeks := fmt.Sprintf(`SELECT %s, CASE WHEN f.round > 0 THEN f.round::text ELSE 'unknown' END AS name, count(*)::float8
		FROM (%s) f WHERE f.at >= $2 AND f.at < $3 GROUP BY 1, 2`, metricBucketFrom("f.at", "$2"), filings)
	cur, err := dcRoundCounts(ctx, db, repo, counts, w.From, w.To)
	if err != nil {
		return Figure{}, Figure{}, nil, err
	}
	prev, err := dcRoundCounts(ctx, db, repo, counts, w.PrevFrom, w.PrevTo)
	if err != nil {
		return Figure{}, Figure{}, nil, err
	}
	wk, err := metricNamedWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	if err != nil {
		return Figure{}, Figure{}, nil, err
	}
	figs := dcNamedFigures(cur, prev, wk)
	dcRoundFigureOrder(figs)
	// the first/later totals stand on the same named counts: round 1
	// alone, and every explicit later round summed — the rounds past the
	// first keep their own names beside the total
	var first, later Figure
	first.Has, first.PrevHas, later.Has, later.PrevHas = true, true, true, true
	for _, nf := range figs {
		if nf.Name == "1" {
			first = nf.Figure
			continue
		}
		if n, err := strconv.Atoi(nf.Name); err == nil && n >= 2 {
			later.Value += nf.Figure.Value
			later.Prev += nf.Figure.Prev
			for b, v := range nf.Figure.Weeks {
				later.Weeks[b] += v
			}
		}
	}
	return first, later, figs, nil
}

// metricDivideReplans — panel 12: the window's applied replans by who
// asked them — a person, a cut, a round — one aggregation behind the
// window's own names and the whole named comparison. A replan is one
// logical transition of a plan's revision: its original row, selected
// over the repository's own history before any cohort bound — earliest
// first — so a revision re-observed in a later window files no second
// replan, and the transition belongs, wherever the panels read it, to
// the window its original instant falls in. A card's own canonical
// filing row is a separate identity, never a stand-in for the
// transition, and so is its key-level double, a card whose issue never
// landed, its key the identity's last field and no card in the detail;
// a row another repository recorded on the same plan — the
// task's reach holds it — neither wins the revision's origin nor
// replaces the local evidence. The origin a span names is the canonical
// revision-level row of the same revision the span's own end saw — a
// replacement arriving later never rewrites the older answer, each
// weekly bucket its own end its cutoff — and where none arrived, the
// original row's own words; what neither names keeps the unknown
// bucket, never a guess.
func metricDivideReplans(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]NamedCount, []NamedFigure, error) {
	// transitions reads each plan revision's original row, bounded above
	// by the cohort's own end — a row after it can never be the original
	// of a cohort count — and names the origin at the cutoff the
	// caller's span holds: the canonical revision-level row of the same
	// revision the cutoff saw, else the original row's own.
	transitions := func(cutoff string) string {
		return `SELECT DISTINCT ON (e.task_id, e.detail->>'revision') e.at,
				COALESCE((
					SELECT CASE WHEN k.detail->>'origin' IN ('` + ReplanPerson + `', '` + ReplanCut + `', '` + ReplanRound + `')
						THEN k.detail->>'origin' ELSE 'unknown' END
					FROM events k
					WHERE k.task_id = e.task_id AND k.repository = e.repository AND k.kind = 'plan_replanned'
						AND k.detail->>'card' IS NULL AND ` + dcRevisionIdentity("k") + `
						AND k.detail->>'revision' = e.detail->>'revision' AND k.at < ` + cutoff + `
					ORDER BY k.at, k.id LIMIT 1
				),
				CASE WHEN e.detail->>'origin' IN ('` + ReplanPerson + `', '` + ReplanCut + `', '` + ReplanRound + `')
					THEN e.detail->>'origin' ELSE 'unknown' END) AS name
			FROM events e JOIN tasks t ON t.id = e.task_id
			WHERE ` + dcCardReach + ` AND e.repository = $1 AND e.kind = 'plan_replanned' AND e.detail->>'card' IS NULL
				AND (e.identity_key IS NULL OR ` + dcRevisionIdentity("e") + `)
				AND e.at < $3
			ORDER BY e.task_id, e.detail->>'revision', e.at, e.id`
	}
	counts := func(t string) string {
		return fmt.Sprintf(`SELECT r.name, count(*)::float8 FROM (%s) r
			WHERE r.at >= $2 AND r.at < $3 GROUP BY 1`, t)
	}
	cur, err := dcRoundCounts(ctx, db, repo, counts(transitions("$3")), w.From, w.To)
	if err != nil {
		return nil, nil, err
	}
	prev, err := dcRoundCounts(ctx, db, repo, counts(transitions("$3")), w.PrevFrom, w.PrevTo)
	if err != nil {
		return nil, nil, err
	}
	namedWeeks, err := metricNamedWeeks(ctx, db, fmt.Sprintf(
		`SELECT %s, r.name, count(*)::float8 FROM (%s) r
			WHERE r.at >= $2 AND r.at < $3 GROUP BY 1, 2`,
		metricBucketFrom("r.at", "$2"), transitions(metricBucketEnd("e.at", "$2"))), repo, w.SparkFrom, w.To)
	if err != nil {
		return nil, nil, err
	}
	// the window's own names, the heaviest origin first and alphabetical
	// among equals — the order the named counts always answered
	var scalar []NamedCount
	for name, n := range cur {
		scalar = append(scalar, NamedCount{Name: name, Count: n})
	}
	sort.Slice(scalar, func(i, j int) bool {
		if scalar[i].Count != scalar[j].Count {
			return scalar[i].Count > scalar[j].Count
		}
		return scalar[i].Name < scalar[j].Name
	})
	// the origins carry no order of their own: alphabetical, the unknown
	// bucket among them, deterministic whatever the origins were
	figs := dcNamedFigures(cur, prev, namedWeeks)
	sort.Slice(figs, func(i, j int) bool { return figs[i].Name < figs[j].Name })
	return scalar, figs, nil
}

// msPerHour is the milliseconds one hour holds: panels 7 and 8 answer
// in precise milliseconds, the legacy hour-valued fields beside them
// scaled from the same numbers, their unit never reinterpreted.
const msPerHour = 3.6e6

// figureInHours is the duration figure f scaled from milliseconds to
// whole and fraction hours: group_open_hours and hold_wait_hours keep
// the legacy unit the console kept reading.
func figureInHours(f Figure) Figure {
	h := Figure{Value: f.Value / msPerHour, Prev: f.Prev / msPerHour, Has: f.Has, PrevHas: f.PrevHas}
	for i, ms := range f.Weeks {
		h.Weeks[i] = ms / msPerHour
	}
	return h
}

// metricDivideGroupHours — panel 7: how long a group stayed open, the
// mean milliseconds over the groups whose first plan_closed row lands
// in the span. A group opens at its first actual child merge, the
// opening OpenGroups reads (groupOpenings), and closes at its first
// closure, so
// duplicate merge or closure rows move nothing and a group opened
// before the span counts its whole wait, never clipped to the span's
// start: the wait belongs to the span its closure lands in. A closed
// group no merge covers and a group still open sit the mean out — the
// coverage counts them — and over an empty cohort the mean is
// undefined, never zero.
func metricDivideGroupHours(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, Figure, GroupHoursCoverage, error) {
	// starts: each plan's first opening, OpenGroups' own contract;
	// closed: each plan's first plan_closed row below the spark's end
	const ctes = `WITH starts AS (` + groupOpenings + `), closed AS (
			SELECT pc.task_id AS plan, min(pc.at) AS closed_at
			FROM events pc JOIN tasks t ON t.id = pc.task_id
			WHERE ` + dcCardReach + ` AND pc.kind = 'plan_closed' AND pc.at < $3
			GROUP BY pc.task_id
		)`
	// the mean wait in milliseconds — EXTRACT(EPOCH) keeps the fraction
	// an integer-hour cast would blur — over the span's own closure
	// cohort, each opening however much earlier
	const measured = `s.first_merge IS NOT NULL`
	scalar := ctes + `
		SELECT avg(EXTRACT(EPOCH FROM (cl.closed_at - s.first_merge)) * 1000)::float8
		FROM closed cl JOIN starts s ON s.plan = cl.plan
		WHERE cl.closed_at >= $2 AND cl.closed_at < $3 AND ` + measured
	ms := Figure{}
	if v, has, err := metricScalar(ctx, db, scalar, repo, w.From, w.To); err != nil {
		return ms, Figure{}, GroupHoursCoverage{}, err
	} else {
		ms.Value, ms.Has = v, has
	}
	if v, has, err := metricScalar(ctx, db, scalar, repo, w.PrevFrom, w.PrevTo); err != nil {
		return ms, Figure{}, GroupHoursCoverage{}, err
	} else {
		ms.Prev, ms.PrevHas = v, has
	}
	// a closure lands in its own weekly bucket alone
	weeks, err := metricWeeks(ctx, db, ctes+fmt.Sprintf(`
		SELECT %s,
		avg(EXTRACT(EPOCH FROM (cl.closed_at - s.first_merge)) * 1000)::float8
		FROM closed cl JOIN starts s ON s.plan = cl.plan
		WHERE cl.closed_at >= $2 AND cl.closed_at < $3 AND %s GROUP BY 1`,
		metricBucketFrom("cl.closed_at", "$2"), measured), repo, w.SparkFrom, w.To)
	if err != nil {
		return ms, Figure{}, GroupHoursCoverage{}, err
	}
	ms.Weeks = weeks
	var cov GroupHoursCoverage
	if err := db.QueryRowContext(ctx, ctes+`
		SELECT count(*) FILTER (WHERE `+measured+`)::int, count(*) FILTER (WHERE s.first_merge IS NULL)::int
		FROM closed cl LEFT JOIN starts s ON s.plan = cl.plan
		WHERE cl.closed_at >= $2 AND cl.closed_at < $3`, repo, w.From, w.To).Scan(&cov.Groups, &cov.MissingStart); err != nil {
		return ms, Figure{}, GroupHoursCoverage{}, err
	}
	// open at the report: an opening the project holds and no closure
	// ever closed, whatever window its closure would have landed in
	if err := db.QueryRowContext(ctx, `SELECT count(*)::int FROM (`+groupOpenings+`) s
		WHERE s.repository = $1 AND NOT EXISTS (SELECT 1 FROM events pc WHERE pc.task_id = s.plan AND pc.kind = 'plan_closed')`,
		repo).Scan(&cov.Open); err != nil {
		return ms, Figure{}, GroupHoursCoverage{}, err
	}
	return ms, figureInHours(ms), cov, nil
}

// holdInterval is one production hold's wait: [start, end), end zero
// while the hold still runs at the report.
type holdInterval struct{ start, end time.Time }

// metricDivideHoldWaits — panel 8: how long the project waited on
// production holds, milliseconds over the span. Each hold is a
// half-open interval — its releases clock's held_since to its
// production_released row's at, the fact the ReleaseKeeper records
// exactly once — clipped to the span and unioned, so overlapping,
// duplicate and nested rows wait their time once and a hold that only
// touches the span's boundary holds none of it. A hold still open at
// the report ends at the captured as-of, one end every span of this
// report reads; a span that ends sooner ends it at its own end the
// sooner. A row whose held_since names no instant measures nothing and
// counts in the coverage. The total is a fact even at zero — no hold
// waited, evidence or none — the sum of an empty union, never an
// unknown.
func metricDivideHoldWaits(ctx context.Context, db *sql.DB, repo string, w metricWindow, asof time.Time) (Figure, Figure, HoldWaitCoverage, error) {
	// the ended holds and the running clock read in one snapshot, so a
	// release committing between the two reads is in exactly one of them
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Figure{}, Figure{}, HoldWaitCoverage{}, err
	}
	defer tx.Rollback()
	// one interval per ended hold, earliest row first
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(detail->>'held_since', ''), at FROM events
		WHERE repository = $1 AND kind = 'production_released' ORDER BY at, id`, repo)
	if err != nil {
		return Figure{}, Figure{}, HoldWaitCoverage{}, err
	}
	defer rows.Close()
	var cov HoldWaitCoverage
	var holds []holdInterval
	ended := map[[2]time.Time]bool{}
	for rows.Next() {
		var since string
		var at time.Time
		if err := rows.Scan(&since, &at); err != nil {
			return Figure{}, Figure{}, HoldWaitCoverage{}, err
		}
		start, perr := time.Parse(time.RFC3339, since)
		if perr != nil {
			cov.Missing++
			continue
		}
		// two rows naming one interval — a retried write's duplicate, a
		// legacy double — are one hold that waited once
		if ended[[2]time.Time{start, at}] {
			continue
		}
		ended[[2]time.Time{start, at}] = true
		cov.Ended++
		holds = append(holds, holdInterval{start: start, end: at})
	}
	if err := rows.Err(); err != nil {
		return Figure{}, Figure{}, HoldWaitCoverage{}, err
	}
	// the hold still on at the report: the releases row's running clock
	var held sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT min(held_since) FROM releases WHERE repository = $1`, repo).Scan(&held); err != nil {
		return Figure{}, Figure{}, HoldWaitCoverage{}, err
	}
	if held.Valid {
		cov.Open = 1
		holds = append(holds, holdInterval{start: held.Time})
	}
	// every span the same evidence: the window, the equal window before
	// it and each weekly bucket of the spark
	ms := Figure{Has: true, PrevHas: true}
	ms.Value = holdUnionMillis(holds, w.From, w.To, asof)
	ms.Prev = holdUnionMillis(holds, w.PrevFrom, w.PrevTo, asof)
	for i := range ms.Weeks {
		ms.Weeks[i] = holdUnionMillis(holds, w.SparkFrom.AddDate(0, 0, 7*i), w.SparkFrom.AddDate(0, 0, 7*(i+1)), asof)
	}
	return ms, figureInHours(ms), cov, nil
}

// holdUnionMillis is the length of the holds' union inside [from, to):
// each interval clipped to the span — one still open at the report's
// captured as-of ends there, a span that ends sooner at its own end —
// the pieces sorted and merged, so overlapping, duplicate and nested
// intervals wait their time once and one that only touches the span's
// boundary holds none of it.
func holdUnionMillis(holds []holdInterval, from, to, asof time.Time) float64 {
	type piece struct{ start, end time.Time }
	var pieces []piece
	for _, h := range holds {
		end := h.end
		if end.IsZero() || end.After(asof) {
			end = asof
		}
		start, stop := h.start, end
		if start.Before(from) {
			start = from
		}
		if stop.After(to) {
			stop = to
		}
		if start.Before(stop) {
			pieces = append(pieces, piece{start, stop})
		}
	}
	slices.SortFunc(pieces, func(a, b piece) int {
		if c := a.start.Compare(b.start); c != 0 {
			return c
		}
		return a.end.Compare(b.end)
	})
	var total time.Duration
	for i := 0; i < len(pieces); {
		start, end := pieces[i].start, pieces[i].end
		for i++; i < len(pieces) && !pieces[i].start.After(end); i++ {
			if pieces[i].end.After(end) {
				end = pieces[i].end
			}
		}
		total += end.Sub(start)
	}
	return float64(total) / float64(time.Millisecond)
}
