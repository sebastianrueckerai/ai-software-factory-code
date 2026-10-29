// Package policy is the factory's capability policy and cost ledger: the
// policy document (authored in Git, loaded by factoryd into immutable
// policy_versions rows, enforced by the gateway from Postgres) and the
// ledger both binaries book costs into.
package policy

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Document is policy.yaml, schema version 1: budgets, limits, capability
// classes, prices, risk rules, the default envelope, approvers, activity
// bounds and the supervisor's detectors.
type Document struct {
	// Version is the file's schema version.
	Version int `yaml:"version" json:"version"`
	// Budget caps spend in EUR per task and per UTC day.
	Budget Budget `yaml:"budget" json:"budget"`
	// Limits bound every task's work: sessions, agent time, supervisor
	// calls.
	Limits Limits `yaml:"limits" json:"limits"`
	// Classes are the capability classes a task can hold, by name.
	Classes map[string]Class `yaml:"classes" json:"classes"`
	// Prices are the tool prices no provider API reports.
	Prices ToolPrices `yaml:"prices,omitempty" json:"prices"`
	// Risk classifies a pull request by the paths it changes.
	Risk Risk `yaml:"risk" json:"risk"`
	// EnvelopeDefault is the resource envelope of a project whose file
	// sets none, and the most any project file's envelope may set.
	EnvelopeDefault Envelope `yaml:"envelope_default" json:"envelope_default"`
	// Approvers are the Forgejo users who may authorize spend in the
	// console (ruling 128): approve a resource request or a merge, promote
	// a resource; a project file's approvers replace them for its project.
	// Empty: nobody.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers"`
	// AIMembers are the console members an AI works through (the
	// controller sessions): their acts read as an AI's on the metrics
	// page, beside the project agent's own.
	AIMembers []string `yaml:"ai_members,omitempty" json:"ai_members"`
	// Bounds are how many seconds one run of an activity may take before
	// it raises a gate_bound alert, per activity name and a default for the
	// rest; absent keys take DefaultBounds.
	Bounds map[string]int `yaml:"bounds,omitempty" json:"bounds"`
	// Supervisor holds the session supervisor's progress detectors: how
	// many seconds without the progress each names before it trips;
	// absent keys take DefaultDetectors (ruling 161: no_write only).
	Supervisor map[string]int `yaml:"supervisor,omitempty" json:"supervisor"`
	// ProjectDefault is what a project file leaves out takes, and the most
	// its quota and standing may set; absent: DefaultProject.
	ProjectDefault *ProjectDefault `yaml:"project_default,omitempty" json:"project_default,omitempty"`
	// LinkHosts are the hosts, their subdomains with them, that links in
	// agent-written text factoryd posts on Forgejo may point at; the
	// Forgejo host and the project's environment hosts always may. Any
	// other link is defanged (hxxps://evil[.]example).
	LinkHosts []string `yaml:"link_hosts,omitempty" json:"link_hosts"`
	// Chat is the project agent's block: its model, its monthly budget and
	// the tools that need the member's OK. Absent: DefaultChat.
	Chat *ChatConfig `yaml:"chat,omitempty" json:"chat,omitempty"`
	// TimeZone is the IANA zone of every routine's cron schedule, such as
	// Europe/Berlin. Absent: UTC.
	TimeZone string `yaml:"time_zone,omitempty" json:"time_zone,omitempty"`
	// Routines are the factory's routines, each a pass that runs for every
	// project it applies to: the template pass for every project with a
	// template.
	Routines []Routine `yaml:"routines,omitempty" json:"routines,omitempty"`
}

// Location is the zone of time_zone, UTC when the file leaves it out.
func (d Document) Location() (*time.Location, error) {
	switch d.TimeZone {
	case "":
		return time.UTC, nil
	case "Local":
		//lint:ignore ST1005 Local is the zone's own name, as the policy spells it
		return nil, errors.New("Local is the host's zone, which moves with the host")
	}
	return time.LoadLocation(d.TimeZone)
}

// ChatConfig is the project agent's block of policy.yaml and of a project
// file, whose keys replace the policy's: a preference, not a cap, but for
// monthly_eur, which may not exceed the policy's.
type ChatConfig struct {
	// Model is the class the project agent calls (purpose operator); the
	// compaction of its threads calls Small.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// VisionModel is the alias a chat turn's image look runs on (purpose
	// vision); the member who pasted the image approved its spend.
	VisionModel string `yaml:"vision_model,omitempty" json:"vision_model,omitempty"`
	// MonthlyEUR is the budget of the project's chat task each UTC month;
	// absent or 0: the policy's.
	MonthlyEUR float64 `yaml:"monthly_eur,omitempty" json:"monthly_eur,omitempty"`
	// Confirm are the tools a member must OK: a call proposes, and the
	// member's Do it runs it. The always-confirm set is confirmed at every
	// autonomy level, so an empty list confirms that set only.
	Confirm []string `yaml:"confirm,omitempty" json:"confirm"`
	// DocRepo is the owner/name of the repository write_doc commits
	// documents to (the project's document repository, the factory's own
	// first); empty: the project's chat writes no documents.
	DocRepo string `yaml:"doc_repo,omitempty" json:"doc_repo,omitempty"`
	// ParallelTurns bounds the project's chat turns that call the model at
	// once, across its threads and members, which guards the chat task's
	// month budget (roadmap 239). Absent: DefaultParallelTurns.
	ParallelTurns int `yaml:"parallel_turns,omitempty" json:"parallel_turns,omitempty"`
	// Window is the context window in tokens of the alias the project
	// agent calls: a thread compacts before a turn projected past 80
	// percent of it (roadmap 239). The provider's own refusal of a prompt
	// past its window is the bound that always holds. Absent:
	// DefaultChatWindow.
	Window int `yaml:"window,omitempty" json:"window,omitempty"`
}

// DefaultParallelTurns is the bound of a chat block without one.
const DefaultParallelTurns = 3

// DefaultChatWindow is the window of a chat block without one.
const DefaultChatWindow = 128000

// ChatTools are the project agent's tools, one per console action and
// the reads.
var ChatTools = []string{"system", "board", "card", "history", "card_history", "costs", "diff",
	"plan", "pr", "file", "session", "session_steps", "session_step", "session_search", "ci",
	"alerts", "deployment_health", "resources", "settings", "policy", "project_images",
	"alert_verdict", "promote_resource", "release_resource",
	"new_card", "request_image", "answer", "comment", "tell_card", "steer", "retry",
	"request_changes", "approve_merge", "deny_merge", "rerun_checks", "set_summary", "set_accent", "invite", "github_export",
	"github_sync", "close", "edit", "grant_session", "fund", "set_dependency",
	"read_doc", "write_doc", "set_autonomy", "routines", "routine_write", "routine_delete", "routine_run", "routine_skip",
	"notes_read", "notes_write", "escalate", "ask", "attach_analysis", "active_threads", "chat_search", "board_view", "board_view_reset",
	"watch_card", "unwatch_card", "set_timer", "analyze_image", "read_attachment", "summarize_attachment"}

// ChatModels are the aliases a chat block may name.
var ChatModels = []string{Big, Small, Premium}

// VisionModels are the vision aliases a chat block or a class may name.
var VisionModels = []string{"vision"}

// DefaultChat is the chat block of a policy without one.
var DefaultChat = ChatConfig{Model: Big, VisionModel: "vision", MonthlyEUR: 5,
	Confirm: []string{"new_card", "request_image", "answer", "tell_card", "steer", "retry", "request_changes",
		"approve_merge", "deny_merge", "invite", "github_export", "github_sync", "close", "edit", "grant_session", "fund", "set_dependency",
		"alert_verdict", "promote_resource", "release_resource", "write_doc", "set_autonomy", "routine_write", "routine_delete", "routine_run", "routine_skip"}}

// The autonomy levels of a project (roadmap 239): how much the project
// agent may do without its member's OK. Full auto is no level of its
// own: it is autonomous with merge_rule auto, shown as one state.
const (
	AutonomyManual     = "manual"
	AutonomyAssisted   = "assisted"
	AutonomyAutonomous = "autonomous"
	// AutonomyFullAuto is the display state of autonomous at merge_rule
	// auto; no file names it.
	AutonomyFullAuto = "full auto"
)

// AutonomyMeaning is each autonomy state's meaning in one line, as the
// level control and the inbox's poke on a change show it (C14).
var AutonomyMeaning = map[string]string{
	AutonomyManual:     "every action of the agent confirms, as today",
	AutonomyAssisted:   "the agent starts and files ready work by itself; direction, money beyond a card's budget and merges still ask",
	AutonomyAutonomous: "the chat's remaining confirms go off; merges follow the merge rule; direction and money beyond the pool still ask",
	AutonomyFullAuto:   "autonomous with the merge rule on auto: nothing waits for a person but direction and money beyond the pool",
}

// AutonomyStates are the four states the level control offers (C14):
// the three levels and full auto.
var AutonomyStates = []string{AutonomyManual, AutonomyAssisted, AutonomyAutonomous, AutonomyFullAuto}

// AutonomyValues are the autonomy values a project file may name.
var AutonomyValues = []string{AutonomyManual, AutonomyAssisted, AutonomyAutonomous}

// DefaultQueueCap bounds a project's unstarted filings when its file
// sets no queue_cap.
const DefaultQueueCap = 8

// assistedOff are the tools that come off the confirm list at assisted:
// the agent starts and files ready work itself, and every start and
// filing still passes the ready check. routine_run files a routine's
// card, as new_card files one, and request_image a tool card.
var assistedOff = []string{"new_card", "request_image", "retry", "grant_session", "routine_run"}

// fullAutoOff are the always-confirm tools that come off at full auto:
// merges follow the rule, and the agent judges alerts and writes documents.
var fullAutoOff = []string{"approve_merge", "deny_merge", "alert_verdict", "write_doc"}

// AlwaysConfirm are the tools confirmed at every level: merges while the
// rule is human, invites, the actions outside the project, and the
// changes of the project's file: the level, the cap and the routines.
var AlwaysConfirm = []string{"approve_merge", "deny_merge", "invite", "alert_verdict",
	"promote_resource", "release_resource", "github_export", "github_sync", "write_doc", "set_autonomy", "routine_write", "routine_delete"}

// ConfirmFor resolves the confirm list of autonomy level at merge rule
// over base, the list the policy and the project file set: manual is
// base as its files set it with the always-confirm set and fund, assisted takes
// the starts and filings off and keeps the always-confirm set, autonomous
// keeps the always-confirm set only, with merges following the rule.
func ConfirmFor(level, mergeRule string, base []string) []string {
	ensure := func(list []string) []string {
		for _, t := range append(slices.Clone(AlwaysConfirm), "fund") {
			if !slices.Contains(list, t) {
				list = append(list, t)
			}
		}
		return list
	}
	switch level {
	case AutonomyAssisted:
		return ensure(slices.DeleteFunc(slices.Clone(base), func(t string) bool { return slices.Contains(assistedOff, t) }))
	case AutonomyAutonomous:
		if mergeRule == MergeAuto {
			return slices.DeleteFunc(slices.Clone(AlwaysConfirm), func(t string) bool { return slices.Contains(fullAutoOff, t) })
		}
		return slices.Clone(AlwaysConfirm)
	default:
		return ensure(slices.Clone(base))
	}
}

// ChatOf is project p's chat block: DefaultChat, the policy's keys over
// it, then the project file's, with p's autonomy level resolved into the
// confirm list.
func (d Document) ChatOf(p ProjectConfig) ChatConfig {
	c := DefaultChat
	for _, b := range []*ChatConfig{d.Chat, p.Chat} {
		if b == nil {
			continue
		}
		c.Model = cmp.Or(b.Model, c.Model)
		c.VisionModel = cmp.Or(b.VisionModel, c.VisionModel)
		c.MonthlyEUR = cmp.Or(b.MonthlyEUR, c.MonthlyEUR)
		c.DocRepo = cmp.Or(b.DocRepo, c.DocRepo)
		c.ParallelTurns = cmp.Or(b.ParallelTurns, c.ParallelTurns)
		c.Window = cmp.Or(b.Window, c.Window)
		if b.Confirm != nil {
			c.Confirm = b.Confirm
		}
	}
	c.Confirm = ConfirmFor(p.Autonomy, p.MergeRule, c.Confirm)
	return c
}

func (c *ChatConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.Model != "" && !slices.Contains(ChatModels, c.Model) {
		return fmt.Errorf("chat.model %q: want one of %v", c.Model, ChatModels)
	}
	if c.VisionModel != "" && !slices.Contains(VisionModels, c.VisionModel) {
		return fmt.Errorf("chat.vision_model %q: want one of %v", c.VisionModel, VisionModels)
	}
	if c.MonthlyEUR < 0 {
		return errors.New("chat.monthly_eur must not be negative")
	}
	for _, t := range c.Confirm {
		if !slices.Contains(ChatTools, t) {
			return fmt.Errorf("chat.confirm: unknown tool %q (known: %v)", t, ChatTools)
		}
	}
	if c.ParallelTurns < 0 {
		return errors.New("chat.parallel_turns must not be negative")
	}
	if c.Window != 0 && c.Window < 1000 {
		return errors.New("chat.window must be at least 1000 tokens")
	}
	if c.DocRepo != "" && !repositoryName.MatchString(c.DocRepo) {
		return fmt.Errorf("chat.doc_repo %q: owner/name in lower case", c.DocRepo)
	}
	return nil
}

// Limits bound a task's work; a task keeps the limits of the version it
// started under.
type Limits struct {
	// Sessions is how many agent sessions a task gets before it goes to a
	// human with every finding so far (the round cap); a human may grant
	// more.
	Sessions int `yaml:"sessions" json:"sessions"`
	// AgentTime is the agent's working time across all sessions of a task,
	// such as 20m; waiting on checks or on a reviewer does not count. A
	// class, process or project may set its own (Time), the most specific
	// wins.
	AgentTime time.Duration `yaml:"agent_time" json:"agent_time"`
	// CheckIn is how long a session runs before it must check in with the
	// supervisor, at least 10m, or 0 for no check-in. Absent: 0. A class,
	// process or project may set its own (Time), the most specific wins.
	CheckIn time.Duration `yaml:"check_in,omitempty" json:"check_in,omitempty"`
	// SupervisorCalls is how many supervisor model calls one session may
	// make; the next trip escalates without one.
	SupervisorCalls int `yaml:"supervisor_calls" json:"supervisor_calls"`
	// SupervisorTimeout bounds one supervisor or review model call, such as
	// 6m.
	SupervisorTimeout time.Duration `yaml:"supervisor_timeout" json:"supervisor_timeout"`
	// StrongerAfter is how many returns from the same stage a task takes
	// on its model; the next return takes the stronger rung.
	StrongerAfter int `yaml:"stronger_after" json:"stronger_after"`
	// Steps caps one agent session's model turns (opencode's agent steps),
	// at least 1: at the cap the harness asks the model to summarise and
	// stop. Absent: DefaultSteps.
	Steps int `yaml:"steps,omitempty" json:"steps"`
	// NudgeAfter is how many identical tool calls in a row (same tool,
	// same input) earn the running session a message to change approach,
	// at least 2. Absent: DefaultNudgeAfter.
	NudgeAfter int `yaml:"nudge_after,omitempty" json:"nudge_after"`
	// Nudges is how many such messages one repetition gets; the next run
	// of the same call trips the supervisor, at least 1. Absent:
	// DefaultNudges.
	Nudges int `yaml:"nudges,omitempty" json:"nudges"`
	// SameCall is how many identical tool calls in one session, in a row
	// or not, trip the same_call detector, at least 2 and above
	// NudgeAfter: a repetition gets its nudges first. Absent:
	// DefaultSameCall.
	SameCall int `yaml:"same_call,omitempty" json:"same_call"`
	// ReviewDiffKiB is the most diff, in KiB, one model review call reads,
	// and the size of a part when a bigger diff reads in parts, as many
	// as it needs. Size it to the review model's context window: about 257
	// tokens a KiB. Absent: DefaultReviewDiffKiB.
	ReviewDiffKiB int `yaml:"review_diff_kib,omitempty" json:"review_diff_kib"`
	// CheckinCalls is how many model calls one check-in conversation gets;
	// its last call carries no tools and asks for the answer. Absent:
	// DefaultCheckinCalls.
	CheckinCalls int `yaml:"checkin_calls,omitempty" json:"checkin_calls,omitempty"`
	// CheckinShare is the most percent of the card's budget the check-in's
	// calls may spend. Absent: DefaultCheckinShare.
	CheckinShare int `yaml:"checkin_share,omitempty" json:"checkin_share,omitempty"`
	// CheckinTailKiB is the parent session record's tail cut into the
	// check-in's statement. Absent: DefaultCheckinTailKiB.
	CheckinTailKiB int `yaml:"checkin_tail_kib,omitempty" json:"checkin_tail_kib,omitempty"`
	// CheckinHandoverChars is the check-in's handover note cap, matching
	// the session's last-message cut. Absent: DefaultCheckinHandoverChars.
	CheckinHandoverChars int `yaml:"checkin_handover_chars,omitempty" json:"checkin_handover_chars,omitempty"`
	// CheckinSessions is how many fresh sessions one confirmed finding
	// gets before the human is asked for it. Absent:
	// DefaultCheckinSessions.
	CheckinSessions int `yaml:"checkin_sessions,omitempty" json:"checkin_sessions,omitempty"`
	// WarnAt is the percent of a card's budget, or of a session's agent
	// time, spent when factoryd tells the running agent to wrap up and
	// push, 1 to 99. Absent: DefaultWarnAt.
	WarnAt int `yaml:"warn_at,omitempty" json:"warn_at"`
	// PlanWarnAt is WarnAt for a planner session. Absent: DefaultPlanWarnAt.
	PlanWarnAt int `yaml:"plan_warn_at,omitempty" json:"plan_warn_at"`
	// NoPushSpend is the percent of a card's budget spent with nothing
	// pushed at which the session supervisor is called, 1 to 99. Absent:
	// DefaultNoPushSpend.
	NoPushSpend int `yaml:"no_push_spend,omitempty" json:"no_push_spend"`
	// ContextTokens is the context size at which opencode compacts a
	// session's conversation, MinContextTokens to ModelContext. Absent:
	// DefaultContextTokens.
	ContextTokens int `yaml:"context_tokens,omitempty" json:"context_tokens"`
}

// DefaultSameCall is the same_call detector's threshold of a policy
// without limits.same_call.
const DefaultSameCall = 4

// SameCallAfter is SameCall, or DefaultSameCall when unset.
func (l Limits) SameCallAfter() int { return cmp.Or(l.SameCall, DefaultSameCall) }

// The review diff limit of a policy without limits.review_diff_kib, and of
// a version pinned before the key existed, and the most a policy may set
// (Forgejo's diff answer is cut at 1 MiB).
const (
	DefaultReviewDiffKiB = 48
	MaxReviewDiffKiB     = 1024
)

// ReviewDiffBytes is ReviewDiffKiB in bytes, DefaultReviewDiffKiB where unset.
func (l Limits) ReviewDiffBytes() int { return cmp.Or(l.ReviewDiffKiB, DefaultReviewDiffKiB) << 10 }

// The wrap-up warning, the spend-without-push line and the compaction
// threshold of a policy without the keys, and of a version pinned before
// they existed. ModelContext is the context window the factory declares
// for its models in opencode's config.
const (
	DefaultWarnAt        = 80
	DefaultPlanWarnAt    = 67
	DefaultNoPushSpend   = 60
	DefaultContextTokens = 100000
	MinContextTokens     = 40000
	ModelContext         = 200000
)

// WarnShare, PlanWarnShare and NoPushShare are the percents as shares of 1.
func (l Limits) WarnShare() float64 { return float64(cmp.Or(l.WarnAt, DefaultWarnAt)) / 100 }
func (l Limits) PlanWarnShare() float64 {
	return float64(cmp.Or(l.PlanWarnAt, DefaultPlanWarnAt)) / 100
}
func (l Limits) NoPushShare() float64 {
	return float64(cmp.Or(l.NoPushSpend, DefaultNoPushSpend)) / 100
}

// ContextCap is ContextTokens, or DefaultContextTokens when unset.
func (l Limits) ContextCap() int { return cmp.Or(l.ContextTokens, DefaultContextTokens) }

// The check-in agent's limits of a policy without the keys, and of a
// version pinned before they existed (the escalation ladder's plan D).
const (
	DefaultCheckinCalls         = 20
	DefaultCheckinShare         = 10
	DefaultCheckinTailKiB       = 24
	DefaultCheckinHandoverChars = 2000
	DefaultCheckinSessions      = 1
)

// CheckinCallCap, CheckinTailBytes, CheckinHandoverCap and
// CheckinSessionCount are the keys, their defaults where unset;
// CheckinShareOf is CheckinShare as a share of a card's budget.
func (l Limits) CheckinCallCap() int { return cmp.Or(l.CheckinCalls, DefaultCheckinCalls) }
func (l Limits) CheckinShareOf(budget float64) float64 {
	return float64(cmp.Or(l.CheckinShare, DefaultCheckinShare)) / 100 * budget
}
func (l Limits) CheckinTailBytes() int { return cmp.Or(l.CheckinTailKiB, DefaultCheckinTailKiB) << 10 }
func (l Limits) CheckinHandoverCap() int {
	return cmp.Or(l.CheckinHandoverChars, DefaultCheckinHandoverChars)
}
func (l Limits) CheckinSessionCount() int { return cmp.Or(l.CheckinSessions, DefaultCheckinSessions) }

// The loop nudge of a policy without the keys, and of a version pinned
// before they existed (queue item C).
const (
	DefaultNudgeAfter = 3
	DefaultNudges     = 1
)

// Nudge is NudgeAfter and Nudges, their defaults where unset.
func (l Limits) Nudge() (after, nudges int) {
	return cmp.Or(l.NudgeAfter, DefaultNudgeAfter), cmp.Or(l.Nudges, DefaultNudges)
}

// DefaultSteps is the step cap of a policy without limits.steps, and of a
// version pinned before the key existed: a turn takes 5 to 15 s, so 200
// steps outlast the 20m agent time at the slow end and cut only a fast
// loop that turns without end (opencode #45442: 364 calls in 50 minutes).
const DefaultSteps = 200

// StepCap is Steps, or DefaultSteps when the policy sets none.
func (l Limits) StepCap() int {
	if l.Steps == 0 {
		return DefaultSteps
	}
	return l.Steps
}

// limitsJSON is Limits with its durations as strings, as in the YAML.
type limitsJSON struct {
	Sessions             int    `json:"sessions"`
	AgentTime            string `json:"agent_time"`
	CheckIn              string `json:"check_in"`
	SupervisorCalls      int    `json:"supervisor_calls"`
	SupervisorTimeout    string `json:"supervisor_timeout"`
	StrongerAfter        int    `json:"stronger_after"`
	Steps                int    `json:"steps"`
	NudgeAfter           int    `json:"nudge_after"`
	Nudges               int    `json:"nudges"`
	SameCall             int    `json:"same_call"`
	ReviewDiffKiB        int    `json:"review_diff_kib,omitempty"`
	CheckinCalls         int    `json:"checkin_calls,omitempty"`
	CheckinShare         int    `json:"checkin_share,omitempty"`
	CheckinTailKiB       int    `json:"checkin_tail_kib,omitempty"`
	CheckinHandoverChars int    `json:"checkin_handover_chars,omitempty"`
	CheckinSessions      int    `json:"checkin_sessions,omitempty"`
	WarnAt               int    `json:"warn_at,omitempty"`
	PlanWarnAt           int    `json:"plan_warn_at,omitempty"`
	NoPushSpend          int    `json:"no_push_spend,omitempty"`
	ContextTokens        int    `json:"context_tokens,omitempty"`
}

func (l Limits) MarshalJSON() ([]byte, error) {
	return json.Marshal(limitsJSON{l.Sessions, l.AgentTime.String(), l.CheckIn.String(), l.SupervisorCalls, l.SupervisorTimeout.String(), l.StrongerAfter, l.Steps, l.NudgeAfter, l.Nudges, l.SameCall, l.ReviewDiffKiB,
		l.CheckinCalls, l.CheckinShare, l.CheckinTailKiB, l.CheckinHandoverChars, l.CheckinSessions, l.WarnAt, l.PlanWarnAt, l.NoPushSpend, l.ContextTokens})
}

func (l *Limits) UnmarshalJSON(b []byte) error {
	var j limitsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	out := Limits{Sessions: j.Sessions, SupervisorCalls: j.SupervisorCalls, StrongerAfter: j.StrongerAfter, Steps: j.Steps,
		NudgeAfter: j.NudgeAfter, Nudges: j.Nudges, SameCall: j.SameCall, ReviewDiffKiB: j.ReviewDiffKiB,
		CheckinCalls: j.CheckinCalls, CheckinShare: j.CheckinShare, CheckinTailKiB: j.CheckinTailKiB, CheckinHandoverChars: j.CheckinHandoverChars, CheckinSessions: j.CheckinSessions,
		WarnAt: j.WarnAt, PlanWarnAt: j.PlanWarnAt, NoPushSpend: j.NoPushSpend, ContextTokens: j.ContextTokens}
	for _, d := range []struct {
		s  string
		to *time.Duration
	}{{j.AgentTime, &out.AgentTime}, {j.CheckIn, &out.CheckIn}, {j.SupervisorTimeout, &out.SupervisorTimeout}} {
		if d.s == "" {
			continue
		}
		v, err := time.ParseDuration(d.s)
		if err != nil {
			return fmt.Errorf("limits: %w", err)
		}
		*d.to = v
	}
	*l = out
	return nil
}

// MaxSupervisorTimeout is the longest supervisor_timeout a policy may set.
const MaxSupervisorTimeout = 15 * time.Minute

// ReviewParts is the parts a model review or audit is bound for before it
// has read its diff; once it has, the bound follows the diff's waves.
const ReviewParts = 3

// MaxAgentTime is the longest agent_time a policy, class, process or
// project may set.
const MaxAgentTime = 72 * time.Hour

// MinCheckIn is the shortest check_in a policy, class, process or project
// may set, besides 0 for no check-in.
const MinCheckIn = 10 * time.Minute

func (l Limits) validate() error {
	switch {
	case l.Sessions <= 0:
		return fmt.Errorf("policy: limits.sessions must be positive, is %d", l.Sessions)
	case l.AgentTime < time.Minute || l.AgentTime > MaxAgentTime:
		return fmt.Errorf("policy: limits.agent_time must lie between 1m and %s, is %s", MaxAgentTime, l.AgentTime)
	case l.CheckIn != 0 && (l.CheckIn < MinCheckIn || l.CheckIn > l.AgentTime):
		return fmt.Errorf("policy: limits.check_in must be 0 or lie between %s and agent_time (%s), is %s", MinCheckIn, l.AgentTime, l.CheckIn)
	case l.SupervisorCalls <= 0:
		return fmt.Errorf("policy: limits.supervisor_calls must be positive, is %d", l.SupervisorCalls)
	case l.SupervisorTimeout < 30*time.Second || l.SupervisorTimeout > MaxSupervisorTimeout:
		return fmt.Errorf("policy: limits.supervisor_timeout must lie between 30s and %s, is %s", MaxSupervisorTimeout, l.SupervisorTimeout)
	case l.StrongerAfter < 0:
		return fmt.Errorf("policy: limits.stronger_after must not be negative, is %d", l.StrongerAfter)
	case l.Steps < 0:
		return fmt.Errorf("policy: limits.steps must not be negative, is %d", l.Steps)
	case l.NudgeAfter < 0:
		return fmt.Errorf("policy: limits.nudge_after must not be negative, is %d", l.NudgeAfter)
	case l.Nudges < 0:
		return fmt.Errorf("policy: limits.nudges must not be negative, is %d", l.Nudges)
	case l.SameCall < 0:
		return fmt.Errorf("policy: limits.same_call must not be negative, is %d", l.SameCall)
	case l.WarnAt < 0 || l.WarnAt > 99:
		return fmt.Errorf("policy: limits.warn_at must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is %d", l.WarnAt)
	case l.PlanWarnAt < 0 || l.PlanWarnAt > 99:
		return fmt.Errorf("policy: limits.plan_warn_at must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is %d", l.PlanWarnAt)
	case l.NoPushSpend < 0 || l.NoPushSpend > 99:
		return fmt.Errorf("policy: limits.no_push_spend must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is %d", l.NoPushSpend)
	case l.ContextTokens != 0 && (l.ContextTokens < MinContextTokens || l.ContextTokens > ModelContext):
		return fmt.Errorf("policy: limits.context_tokens must lie between %d and %d when set in policy.yaml (0 means absent, as in a version pinned before the key), is %d", MinContextTokens, ModelContext, l.ContextTokens)
	case l.ReviewDiffKiB != 0 && (l.ReviewDiffKiB < DefaultReviewDiffKiB || l.ReviewDiffKiB > MaxReviewDiffKiB):
		return fmt.Errorf("policy: limits.review_diff_kib must be 0 or lie between %d and %d, is %d", DefaultReviewDiffKiB, MaxReviewDiffKiB, l.ReviewDiffKiB)
	case l.CheckinCalls != 0 && (l.CheckinCalls < 1 || l.CheckinCalls > 48):
		return fmt.Errorf("policy: limits.checkin_calls must be 0 or lie between 1 and 48, is %d", l.CheckinCalls)
	case l.CheckinShare != 0 && (l.CheckinShare < 1 || l.CheckinShare > 100):
		return fmt.Errorf("policy: limits.checkin_share must be 0 or lie between 1 and 100, is %d", l.CheckinShare)
	case l.CheckinTailKiB != 0 && (l.CheckinTailKiB < 4 || l.CheckinTailKiB > 64):
		return fmt.Errorf("policy: limits.checkin_tail_kib must be 0 or lie between 4 and 64, is %d", l.CheckinTailKiB)
	case l.CheckinHandoverChars != 0 && (l.CheckinHandoverChars < 200 || l.CheckinHandoverChars > 8000):
		return fmt.Errorf("policy: limits.checkin_handover_chars must be 0 or lie between 200 and 8000, is %d", l.CheckinHandoverChars)
	case l.CheckinSessions != 0 && (l.CheckinSessions < 1 || l.CheckinSessions > 3):
		return fmt.Errorf("policy: limits.checkin_sessions must be 0 or lie between 1 and 3, is %d", l.CheckinSessions)
	}
	if after, _ := l.Nudge(); after >= l.SameCallAfter() {
		return fmt.Errorf("policy: limits.nudge_after (%d) must be below limits.same_call (%d): a repetition gets its nudge before the trip", after, l.SameCallAfter())
	}
	return nil
}

// Time overrides limits.agent_time and limits.check_in for one class
// (policy.yaml), process (processes/<name>.yaml, key time) or project
// (projects/<name>.yaml, key time). Either field may be left unset to
// defer to the next less specific level.
type Time struct {
	// AgentTime overrides limits.agent_time when set (non-zero).
	AgentTime time.Duration `yaml:"agent_time,omitempty" json:"agent_time,omitempty"`
	// CheckIn overrides limits.check_in when set; a set 0 means no
	// check-in, distinct from leaving it unset.
	CheckIn *time.Duration `yaml:"check_in,omitempty" json:"check_in,omitempty"`
}

func (t Time) validate() error {
	if t.AgentTime != 0 && (t.AgentTime < time.Minute || t.AgentTime > MaxAgentTime) {
		return fmt.Errorf("time: agent_time must lie between 1m and %s, is %s", MaxAgentTime, t.AgentTime)
	}
	if t.CheckIn != nil {
		c := *t.CheckIn
		switch {
		case c == 0:
		case c < MinCheckIn:
			return fmt.Errorf("time: check_in must be 0 or at least %s, is %s", MinCheckIn, c)
		case t.AgentTime != 0 && c > t.AgentTime:
			return fmt.Errorf("time: check_in %s exceeds agent_time %s", c, t.AgentTime)
		}
	}
	return nil
}

// timeJSON is Time with its durations as strings, as in the YAML.
type timeJSON struct {
	AgentTime string  `json:"agent_time,omitempty"`
	CheckIn   *string `json:"check_in,omitempty"`
}

func (t Time) MarshalJSON() ([]byte, error) {
	j := timeJSON{}
	if t.AgentTime != 0 {
		j.AgentTime = t.AgentTime.String()
	}
	if t.CheckIn != nil {
		s := t.CheckIn.String()
		j.CheckIn = &s
	}
	return json.Marshal(j)
}

func (t *Time) UnmarshalJSON(b []byte) error {
	var j timeJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	var out Time
	if j.AgentTime != "" {
		v, err := time.ParseDuration(j.AgentTime)
		if err != nil {
			return fmt.Errorf("time: %w", err)
		}
		out.AgentTime = v
	}
	if j.CheckIn != nil {
		v, err := time.ParseDuration(*j.CheckIn)
		if err != nil {
			return fmt.Errorf("time: %w", err)
		}
		out.CheckIn = &v
	}
	*t = out
	return nil
}

// TimeOf resolves agent_time and check_in for a task of the given class,
// process and project: limits, then the class, then the process, then the
// project, most specific wins, each field on its own. A resolved check_in
// at or past the resolved agent_time (each field can come from a
// different level) is not a check-in at all, so it clamps to 0.
func (d Document) TimeOf(class string, proc Process, p ProjectConfig) (agentTime, checkIn time.Duration) {
	agentTime, checkIn = d.Limits.AgentTime, d.Limits.CheckIn
	for _, t := range []*Time{d.Classes[class].Time, proc.Time, p.Time} {
		if t == nil {
			continue
		}
		if t.AgentTime != 0 {
			agentTime = t.AgentTime
		}
		if t.CheckIn != nil {
			checkIn = *t.CheckIn
		}
	}
	if checkIn >= agentTime {
		checkIn = 0
	}
	return agentTime, checkIn
}

// DetectorNoWrite is the progress detector (rulings 141, 161): agent
// events keep coming but no file write or edit tool call.
const DetectorNoWrite = "no_write"

// DefaultDetectors are the seconds a policy without them gets.
var DefaultDetectors = map[string]int{DetectorNoWrite: 300}

// Detector is how long the named progress detector waits before it trips.
func (d Document) Detector(name string) time.Duration {
	s, ok := d.Supervisor[name]
	if !ok {
		s = DefaultDetectors[name]
	}
	return time.Duration(s) * time.Second
}

// BoundDefault is the bounds key for every activity without its own.
const BoundDefault = "default"

// DefaultBounds are the bounds a policy without them gets (rulings 140,
// 154): a merge attempt is one API call under a 2-minute timeout, its
// retries on workflow timers (ruling 163). RunSession, RunReport, Screen,
// SuperviseGate, ModelReview and GateAudit follow the limits (Bound).
var DefaultBounds = map[string]int{
	"WaitChecks": 600, "GateReproduction": 900, "GateCriteria": 900, "Merge": 90, BoundDefault: 120,
}

// BoundActivities are factoryd's activity names, the keys bounds takes
// besides default.
var BoundActivities = []string{
	"EnsureIssue", "SetState", "RunSession", "OpenPullRequest", "WaitChecks", "PullRequestMerged", "RecordStep",
	"Finish", "Reconcile", "Escalate", "RecordUsage", "GatePolicy", "GateMigration", "GateReproduction", "GateCriteria",
	"ModelReview", "RequestApproval", "Merge", "ReportMergeFailure", "ReportMergeRetriesExhausted", "SuperviseGate",
	"AutoMergeNote", "RequestInfo", "CloseNotReproduced", "Screen", "JudgeScreening", "RunReport", "RollCommit", "RerunChecks", "GateAudit", "TaskConfig",
}

// Bound is how long one run of activity may take before it alerts: the
// policy's bound for it; else for a session the agent time plus a minute
// (a session at the cap is stopped by the cap already), for a screening
// or a sizing the agent time plus two (its pod start), for the gate
// supervisor its call's timeout plus two and for the model review and the
// audit their ReviewParts calls' timeouts plus two (PartsBound); else
// DefaultBounds' for it, else the policy's default, else DefaultBounds'
// default.
func (d Document) Bound(activity string) time.Duration { return d.PartsBound(activity, ReviewParts) }

// PartsBound is Bound for a model review or an audit whose diff reads in
// parts calls, one part's timeout each: a one-part review is bound at one
// call's timeout plus two minutes. Any other activity's is Bound's.
func (d Document) PartsBound(activity string, parts int) time.Duration {
	s, ok := d.Bounds[activity]
	switch {
	case ok:
	case activity == "RunSession" || activity == "RunReport" || activity == "RunPlan":
		return d.Limits.AgentTime + time.Minute
	case activity == "Screen" || activity == "SizeCard":
		// a screening's or a sizing's report session, and its pod start
		return d.Limits.AgentTime + 2*time.Minute
	case activity == "SuperviseGate":
		return d.Limits.SupervisorTimeout + 2*time.Minute
	case activity == "ModelReview" || activity == "GateAudit":
		return time.Duration(parts)*d.Limits.SupervisorTimeout + 2*time.Minute
	}
	if !ok {
		s, ok = DefaultBounds[activity]
	}
	if !ok {
		s, ok = d.Bounds[BoundDefault]
	}
	if !ok {
		s = DefaultBounds[BoundDefault]
	}
	return time.Duration(s) * time.Second
}

// Envelope bounds what a project's tasks may commission in the agent
// cloud project without a human: EUR per calendar month (reservations and
// accrued cost together), the server types, how many servers at once, the
// longest lease and the largest volume.
type Envelope struct {
	// MonthlyEUR caps a calendar month (UTC), reservations and accrued
	// cost together, in net EUR.
	MonthlyEUR float64 `yaml:"monthly_eur" json:"monthly_eur"`
	// ServerTypes are the Hetzner server types a task may commission.
	ServerTypes []string `yaml:"server_types" json:"server_types"`
	// MaxCount is how many servers may run at once.
	MaxCount int `yaml:"max_count" json:"max_count"`
	// MaxLeaseHours is the longest lease.
	MaxLeaseHours int `yaml:"max_lease_hours" json:"max_lease_hours"`
	// VolumeMaxGB is the largest volume per server: 0 for none, else at
	// least 10.
	VolumeMaxGB int `yaml:"volume_max_gb" json:"volume_max_gb"`
	// Standing is the second block, for standing resources; the policy
	// sets its default in project_default, not in envelope_default.
	Standing Standing `yaml:"-" json:"-"`
}

// MinVolumeGB is the smallest volume Hetzner creates.
const MinVolumeGB = 10

func (e Envelope) validate(key string) error {
	if len(e.ServerTypes) == 0 || slices.Contains(e.ServerTypes, "") {
		return fmt.Errorf("%s.server_types must list at least one server type, none empty", key)
	}
	if e.MonthlyEUR <= 0 {
		return fmt.Errorf("%s.monthly_eur must be positive", key)
	}
	if e.MaxCount < 1 {
		return fmt.Errorf("%s.max_count must be at least 1", key)
	}
	if e.MaxLeaseHours < 1 {
		return fmt.Errorf("%s.max_lease_hours must be at least 1", key)
	}
	if e.VolumeMaxGB != 0 && e.VolumeMaxGB < MinVolumeGB {
		return fmt.Errorf("%s.volume_max_gb must be 0 (no volumes) or at least %d", key, MinVolumeGB)
	}
	return nil
}

// Budget caps spend in EUR: per task (default and maximum at intake), per
// UTC day and per UTC calendar month over all tasks together; a day or
// month cap of 0 stops all spend.
type Budget struct {
	// TaskDefaultEUR is a task's budget when its intake asks for none.
	TaskDefaultEUR float64 `yaml:"task_default_eur" json:"task_default_eur"`
	// TaskMaxEUR is the most a task may be given.
	TaskMaxEUR float64 `yaml:"task_max_eur" json:"task_max_eur"`
	// DayEUR caps all tasks together per UTC day; 0 stops all spend.
	DayEUR float64 `yaml:"day_eur" json:"day_eur"`
	// MonthEUR caps all tasks together per UTC calendar month; 0 stops all
	// spend.
	MonthEUR float64 `yaml:"month_eur" json:"month_eur"`
	// SessionFloorCheapEUR is the least free budget a session on the cheap
	// rung needs to start; a card that cannot fund it asks a human to fund
	// it. Absent: 0, no floor beyond the session's opening calls.
	SessionFloorCheapEUR float64 `yaml:"session_floor_cheap_eur,omitempty" json:"session_floor_cheap_eur,omitempty"`
	// SessionFloorFrontierEUR is SessionFloorCheapEUR for a session on the
	// frontier rung.
	SessionFloorFrontierEUR float64 `yaml:"session_floor_frontier_eur,omitempty" json:"session_floor_frontier_eur,omitempty"`
	// PremiumMonthEUR caps the calls on Premium together per UTC calendar
	// month, inside the caps above; the gateway serves a call it cannot fit
	// on Big (ruling 329). Absent or 0: Premium has no cap of its own.
	PremiumMonthEUR float64 `yaml:"premium_month_eur,omitempty" json:"premium_month_eur,omitempty"`
}

// SessionFloor is the least free budget a session starts on, by its rung.
func (b Budget) SessionFloor(frontier bool) float64 {
	if frontier {
		return b.SessionFloorFrontierEUR
	}
	return b.SessionFloorCheapEUR
}

// Class is a capability class a task can hold: the model aliases it may
// call through the gateway and its tools with per-task caps, and the
// aliases factoryd calls on the task's account, which the agent may not.
type Class struct {
	// Session is the program a task of the class runs: code the coding
	// lifecycle, plan the plan loop, report a write-up. The tree's check
	// requires it; Document.Validate does not, so a gateway reading a
	// version written before it stays valid.
	Session string `yaml:"session" json:"session,omitempty"`
	// Models are the aliases the agent may call through the gateway.
	Models []string `yaml:"models" json:"models"`
	// SessionModel is the alias a session of a plan or report class runs
	// on (ruling 330), one of Models; absent: factoryd's stronger rung.
	SessionModel string `yaml:"session_model,omitempty" json:"session_model,omitempty"`
	// Tools are the gateway tools the class may call: a paid tool with its
	// per-task caps, a free tool as {}.
	Tools map[string]ToolCap `yaml:"tools,omitempty" json:"tools"`
	// ReviewModel is the alias factoryd's model review calls on the task's
	// account.
	ReviewModel string `yaml:"review_model,omitempty" json:"review_model,omitempty"`
	// SupervisorModel is the alias the session and gate supervisors call
	// on the task's account.
	SupervisorModel string `yaml:"supervisor_model,omitempty" json:"supervisor_model,omitempty"`
	// AuditModel is the alias factoryd's security audit calls on the
	// task's account (ruling 188).
	AuditModel string `yaml:"audit_model,omitempty" json:"audit_model,omitempty"`
	// CheckinModel is the alias the check-in agent calls on the task's
	// account; absent reads ReviewModel (the escalation ladder's plan D).
	CheckinModel string `yaml:"checkin_model,omitempty" json:"checkin_model,omitempty"`
	// VisionModel is the alias an analyze_image look calls on the task's
	// account (purpose vision); a class that lists analyze_image names one.
	VisionModel string `yaml:"vision_model,omitempty" json:"vision_model,omitempty"`
	// Time overrides limits.agent_time and limits.check_in for tasks of
	// this class; a process or project's Time wins over it.
	Time *Time `yaml:"time,omitempty" json:"time,omitempty"`
}

// The purposes factoryd calls a model for on a task's account.
const (
	PurposeReview         = "review"
	PurposeSupervisor     = "supervisor"
	PurposeGateSupervisor = "gate-supervisor"
	PurposeAudit          = "audit"
	PurposeOperator       = "operator"
	// PurposeCompact is a chat thread's compaction (roadmap 239): one
	// cheap call that summarises the thread before its kept tail.
	PurposeCompact = "compact"
	// PurposeCheckin is the check-in agent's conversation at a gate
	// return (the escalation ladder's plan D).
	PurposeCheckin = "checkin"
	// PurposeVision is a look at one image: a chat turn's at an image the
	// member pasted, a session's analyze_image at a card's image or a
	// workspace file. The only purpose an image part may ride.
	PurposeVision = "vision"
	// PurposeTitle is a chat thread's naming call (roadmap 304): one
	// cheap call that writes the thread's title from its first message.
	PurposeTitle = "title"
	// PurposeBrief is a needs-you card's brief (ruling 116), on the
	// class's supervisor model: the one call a card that waits on a person
	// may make.
	PurposeBrief = "brief"
)

// LookTypes are the image types a look takes, sniffed from its bytes.
var LookTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// MaxLookBytes is the largest image a look takes.
const MaxLookBytes = 8 << 20

// Purposes are the factoryd purposes the gateway admits.
var Purposes = []string{PurposeReview, PurposeSupervisor, PurposeGateSupervisor, PurposeAudit, PurposeOperator, PurposeCompact, PurposeCheckin, PurposeVision, PurposeTitle, PurposeBrief}

// The model classes every text call names (Sebastian, 2026-10-20). Big
// and Small are LiteLLM aliases routed to their z.ai model, the Alibaba
// model behind it as fallback. Premium (ruling 328) is a pay-per-token
// model with its own month cap (Budget.PremiumMonthEUR) that falls back to
// Big (ruling 329). A call's purpose sits on its audit row and in its
// ledger ref, not in its alias.
const (
	Big     = "big"
	Small   = "small"
	Premium = "premium"
)

// TextClasses are the model classes, as the metrics page sorts spend.
var TextClasses = []string{Big, Small, Premium}

// withFallback is aliases with Big beside Premium: the alias the gateway
// serves a premium call on at the premium cap, and LiteLLM's on a provider
// failure.
func withFallback(aliases []string) []string {
	if slices.Contains(aliases, Premium) {
		aliases = append(aliases, Big)
	}
	slices.Sort(aliases)
	return slices.Compact(aliases)
}

// ToolCap caps a paid tool per task; a free tool carries none.
type ToolCap struct {
	// MaxRequests is how many calls of the tool a task may make.
	MaxRequests int `yaml:"max_requests,omitempty" json:"max_requests"`
	// MaxEUR is how much a task may spend on the tool.
	MaxEUR float64 `yaml:"max_eur,omitempty" json:"max_eur"`
}

// ClassCoder is the class intake gives agent tasks.
const ClassCoder = "coder"

// ClassAnalyzer is the class of a report session (ruling 185): it reads
// and writes a comment, it changes no code and commissions nothing.
const ClassAnalyzer = "analyzer"

// The gateway's paid tools: a class allows one by listing it in
// classes.<c>.tools with its caps.
const (
	ToolWebScrape = "web_scrape"
	ToolWebSearch = "web_search"
	ToolReplicate = "replicate_predict"
	ToolFal       = "fal_run"
)

// PaidTools are the tools a class lists with caps.
var PaidTools = []string{ToolWebScrape, ToolWebSearch, ToolReplicate, ToolFal}

// The gateway's free tools: a class allows one by listing it in
// classes.<c>.tools without caps ({}). The commissioning tools spend
// through their own reservation and envelope, not a per-task tool cap.
const (
	ToolEscalate       = "escalate_to_human"
	ToolCloudPrices    = "cloud_prices"
	ToolCommission     = "commission_server"
	ToolListResources  = "list_resources"
	ToolRelease        = "release_resource"
	ToolSSHCertificate = "ssh_certificate"
	ToolRequestStatus  = "resource_request_status"
	ToolRequestInfo    = "request_info"
	ToolFilePlan       = "file_plan"
	// ToolFileVerdict is an audit session's verdict on a red hold-out
	// (the spec-intake concept, piece 6).
	ToolFileVerdict = "file_verdict"
	ToolManual      = "manual"
	// ToolDeploymentHealth reads a Deployment of the task's project as
	// factoryd sees it: its pods, restarts, last terminations and the last
	// hour's events, never a log or a Secret.
	ToolDeploymentHealth = "deployment_health"
	// ToolDelegate files a small follow-up card on the cheap rung that
	// starts when the caller's card has landed (roadmap 222, item 6).
	ToolDelegate = "delegate"
	// ToolRequestTool asks for a tool the sandbox lacks: a card adds it to
	// one of the project's images, and the caller's card restarts on it.
	ToolRequestTool = "request_tool"
	// ToolAnalyzeImage has the class's vision model look at an image of
	// the card's issue or a file in the workspace; the look spends on the
	// card's account as a model call.
	ToolAnalyzeImage = "analyze_image"
)

// SourceToolRequest is the source of a tool card, which adds a tool an
// agent asked for to one of its project's images: its own session never
// gets request_tool, as its sandbox never gets the tools it declares.
const SourceToolRequest = "tool_request"

// OffersTool reports whether a project that deploys into namespaces is
// offered tool: a tool that reads where a project runs needs the project
// to declare it, and deployment_health reads the cluster, so a project that
// lists no namespaces never sees it. Every other tool is not about where a
// project runs.
func OffersTool(tool string, namespaces []string) bool {
	return tool != ToolDeploymentHealth || len(namespaces) > 0
}

// FreeTools are the tools a class lists without caps.
var FreeTools = []string{ToolEscalate, ToolRequestInfo, ToolRequestTool, ToolFilePlan, ToolFileVerdict, ToolDelegate, ToolCloudPrices, ToolCommission, ToolListResources, ToolRelease, ToolSSHCertificate, ToolRequestStatus, ToolManual, ToolDeploymentHealth, ToolAnalyzeImage}

// ToolPrices are the tool prices no provider API reports.
type ToolPrices struct {
	// Firecrawl prices web_scrape and web_search.
	Firecrawl FirecrawlPrices `yaml:"firecrawl,omitempty" json:"firecrawl"`
	// Replicate prices replicate_predict.
	Replicate ReplicatePrices `yaml:"replicate,omitempty" json:"replicate"`
	// Fal bounds fal_run.
	Fal FalPrices `yaml:"fal,omitempty" json:"fal"`
	// MaxOutputTokens is each model alias's output limit, the most output
	// tokens a call asks for and is held at (Document.Limited). The tree's
	// check requires one for every alias a class or the chat calls.
	MaxOutputTokens map[string]int `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
}

// Limited is alias's price p with its output limit:
// prices.max_output_tokens, lowered to the largest its deployments
// publish when that is less. An alias without one is refused like an
// unpriced one: no hold may take all the free headroom.
func (d Document) Limited(alias string, p Price) (Price, error) {
	limit, ok := d.Prices.MaxOutputTokens[alias]
	if !ok {
		return Price{}, fmt.Errorf("%s has no output limit: prices.max_output_tokens in policy.yaml names none", alias)
	}
	if p.MaxOutput > 0 {
		limit = min(limit, p.MaxOutput)
	}
	p.MaxOutput = limit
	return p, nil
}

// FirecrawlPrices: a call costs its creditsUsed at EURPerCredit; the
// credit figures are the worst case a call is held at.
type FirecrawlPrices struct {
	// EURPerCredit is what one Firecrawl credit costs.
	EURPerCredit float64 `yaml:"eur_per_credit" json:"eur_per_credit"`
	// ScrapeCredits is the most credits one scrape is held at.
	ScrapeCredits int `yaml:"scrape_credits" json:"scrape_credits"`
	// SearchCredits is the most credits one search is held at.
	SearchCredits int `yaml:"search_credits" json:"search_credits"`
}

// ReplicatePrices: a prediction costs its predict_time at its hardware's
// rate. Replicate's API names no model's hardware, so Models maps each
// usable model (owner/name) to its hardware SKU; any other model is
// refused. A prediction is held at MaxSeconds of its hardware, and
// Replicate cancels it ReplicateCancelMargin seconds before that
// (Cancel-After), so it can never bill more than its hold.
type ReplicatePrices struct {
	// EURPerSecond is the price of a second of predict_time by hardware
	// SKU.
	EURPerSecond map[string]float64 `yaml:"eur_per_second" json:"eur_per_second"`
	// Models maps each usable model (owner/name) to its hardware SKU.
	Models map[string]string `yaml:"models" json:"models"`
	// MaxSeconds is how many seconds of its hardware a prediction is held
	// at, whole seconds, at least 10.
	MaxSeconds float64 `yaml:"max_seconds" json:"max_seconds"`
}

// ReplicateCancelMargin (seconds) covers a cancelled prediction's wind-down;
// ReplicateMinSeconds leaves Replicate's Cancel-After minimum of 5 s.
const (
	ReplicateCancelMargin = 5
	ReplicateMinSeconds   = 10
)

// FalPrices: fal's pricing API prices each endpoint per unit; a call
// priced per megapixel is held at MaxMegapixels.
type FalPrices struct {
	// MaxMegapixels is how many megapixels a call priced per megapixel is
	// held at.
	MaxMegapixels float64 `yaml:"max_megapixels" json:"max_megapixels"`
}

func (d Document) Validate() error {
	if d.Version != 1 {
		return fmt.Errorf("policy: version %d, want 1", d.Version)
	}
	b := d.Budget
	if b.TaskDefaultEUR <= 0 || b.TaskMaxEUR <= 0 {
		return errors.New("policy: budget.task_default_eur and task_max_eur must be positive")
	}
	if b.DayEUR < 0 {
		return errors.New("policy: budget.day_eur must not be negative (0 stops all spend)")
	}
	if b.MonthEUR < 0 {
		return errors.New("policy: budget.month_eur must not be negative (0 stops all spend)")
	}
	if b.TaskDefaultEUR > b.TaskMaxEUR {
		return fmt.Errorf("policy: budget.task_default_eur %g exceeds task_max_eur %g", b.TaskDefaultEUR, b.TaskMaxEUR)
	}
	if b.SessionFloorCheapEUR < 0 || b.SessionFloorFrontierEUR < 0 {
		return errors.New("policy: budget.session_floor_cheap_eur and session_floor_frontier_eur must not be negative")
	}
	if floor := max(b.SessionFloorCheapEUR, b.SessionFloorFrontierEUR); floor > b.TaskMaxEUR {
		return fmt.Errorf("policy: a session floor of %g exceeds budget.task_max_eur %g, which no card could fund", floor, b.TaskMaxEUR)
	}
	if b.PremiumMonthEUR < 0 {
		return errors.New("policy: budget.premium_month_eur must not be negative (0: premium has no cap of its own)")
	}
	for alias, n := range d.Prices.MaxOutputTokens {
		if n <= 0 {
			return fmt.Errorf("policy: prices.max_output_tokens of %s must be positive", alias)
		}
	}
	if err := d.Limits.validate(); err != nil {
		return err
	}
	if len(d.Classes) == 0 {
		return errors.New("policy: no capability classes")
	}
	if err := d.Chat.validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if err := d.Risk.validate(); err != nil {
		return err
	}
	if err := d.EnvelopeDefault.validate("envelope_default"); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if d.ProjectDefault != nil {
		if err := d.ProjectDefault.validate(); err != nil {
			return fmt.Errorf("policy: %w", err)
		}
	}
	for i, a := range d.Approvers {
		if a == "" || slices.Contains(d.Approvers[:i], a) {
			return fmt.Errorf("policy: approvers must be distinct, non-empty user names (%q)", a)
		}
	}
	for _, h := range d.LinkHosts {
		if !hostname.MatchString(h) {
			return fmt.Errorf("policy: link_hosts: %q is not a host name such as example.org", h)
		}
	}
	for name, s := range d.Bounds {
		if name != BoundDefault && !slices.Contains(BoundActivities, name) {
			return fmt.Errorf("policy: bounds: unknown activity %q (known: %v and %s)", name, BoundActivities, BoundDefault)
		}
		if s <= 0 {
			return fmt.Errorf("policy: bounds.%s must be a positive number of seconds, is %d", name, s)
		}
	}
	for name, s := range d.Supervisor {
		if _, ok := DefaultDetectors[name]; !ok {
			return fmt.Errorf("policy: supervisor: unknown detector %q (known: %s)", name, DetectorNoWrite)
		}
		if s <= 0 {
			return fmt.Errorf("policy: supervisor.%s must be a positive number of seconds, is %d", name, s)
		}
	}
	if _, err := d.Location(); err != nil {
		return fmt.Errorf("policy: time_zone %q: %v, want an IANA zone such as Europe/Berlin", d.TimeZone, err)
	}
	for i, r := range d.Routines {
		if err := r.Validate("policy"); err != nil {
			return err
		}
		if r.Files != FilesPullRequest {
			return fmt.Errorf("policy: routine %q: a routine of policy.yaml is a pass over the projects, files pull-request, and a project's own routine goes in its project file", r.Name)
		}
		for _, t := range r.Schedule {
			if t.Merges != 0 || t.On == OnPushMain {
				return fmt.Errorf("policy: routine %q: %s counts one project's work, a trigger of a project's own routine: a routine of policy.yaml runs on cron or on each configuration version", r.Name, t.Words())
			}
		}
		if slices.ContainsFunc(d.Routines[:i], func(o Routine) bool { return o.Name == r.Name }) {
			return fmt.Errorf("policy: routine %q: given twice", r.Name)
		}
	}
	for name, c := range d.Classes {
		if name == "" || name == ClassChat {
			return fmt.Errorf("policy: a capability class may not be named %q", name)
		}
		if len(c.Models) == 0 {
			return fmt.Errorf("policy: class %s allows no models", name)
		}
		if slices.Contains(c.Models, "") {
			return fmt.Errorf("policy: class %s lists an empty model", name)
		}
		if c.Time != nil {
			if err := c.Time.validate(); err != nil {
				return fmt.Errorf("policy: class %s: %w", name, err)
			}
		}
		if c.VisionModel != "" && !slices.Contains(VisionModels, c.VisionModel) {
			return fmt.Errorf("policy: class %s vision_model %q: want one of %v", name, c.VisionModel, VisionModels)
		}
		if _, ok := c.Tools[ToolAnalyzeImage]; ok && c.VisionModel == "" {
			return fmt.Errorf("policy: class %s lists %s but names no vision_model, which its looks call", name, ToolAnalyzeImage)
		}
		for tool, tc := range c.Tools {
			if slices.Contains(FreeTools, tool) {
				if tc != (ToolCap{}) {
					return fmt.Errorf("policy: class %s tool %s is free: list it as {}, without max_requests or max_eur", name, tool)
				}
				continue
			}
			if !slices.Contains(PaidTools, tool) {
				return fmt.Errorf("policy: class %s lists unknown tool %q (paid: %v, free: %v)", name, tool, PaidTools, FreeTools)
			}
			if tc.MaxRequests <= 0 || tc.MaxEUR <= 0 {
				return fmt.Errorf("policy: class %s tool %q needs a positive max_requests and max_eur", name, tool)
			}
			if err := d.Prices.validate(tool); err != nil {
				return fmt.Errorf("policy: class %s lists %s: %w", name, tool, err)
			}
		}
	}
	return nil
}

func (p ToolPrices) validate(tool string) error {
	switch tool {
	case ToolWebScrape, ToolWebSearch:
		f := p.Firecrawl
		if f.EURPerCredit <= 0 || f.ScrapeCredits <= 0 || f.SearchCredits <= 0 {
			return errors.New("prices.firecrawl needs a positive eur_per_credit, scrape_credits and search_credits")
		}
	case ToolReplicate:
		r := p.Replicate
		if len(r.Models) == 0 {
			return errors.New("prices.replicate needs at least one model")
		}
		if r.MaxSeconds < ReplicateMinSeconds || r.MaxSeconds != math.Trunc(r.MaxSeconds) {
			return fmt.Errorf("prices.replicate.max_seconds must be whole seconds, at least %d", ReplicateMinSeconds)
		}
		for model, sku := range r.Models {
			if rate := r.EURPerSecond[sku]; rate <= 0 {
				return fmt.Errorf("prices.replicate model %s runs on %q, which has no positive eur_per_second", model, sku)
			}
		}
	case ToolFal:
		if p.Fal.MaxMegapixels <= 0 {
			return errors.New("prices.fal needs a positive max_megapixels")
		}
	}
	return nil
}

// ToolCap is the most generous cap any of classes grants tool; false when
// none lists it.
func (d Document) ToolCap(classes []string, tool string) (ToolCap, bool) {
	var best ToolCap
	found := false
	for _, c := range classes {
		if tc, ok := d.Classes[c].Tools[tool]; ok {
			best.MaxRequests = max(best.MaxRequests, tc.MaxRequests)
			best.MaxEUR = max(best.MaxEUR, tc.MaxEUR)
			found = true
		}
	}
	return best, found
}

// AllowsTool reports whether any of classes lists tool, paid or free.
func (d Document) AllowsTool(classes []string, tool string) bool {
	_, ok := d.ToolCap(classes, tool)
	return ok
}

// AllowsSessionTool reports whether a session of a card of source with
// classes may call tool: one of the classes lists it, and it is no tool
// card's request_tool.
func (d Document) AllowsSessionTool(classes []string, source, tool string) bool {
	return d.AllowsTool(classes, tool) && (tool != ToolRequestTool || source != SourceToolRequest)
}

// Tools are the tools a session of a card of source with classes may
// call, sorted: what the gateway lists to the task and the brief names.
func (d Document) Tools(classes []string, source string) []string {
	var out []string
	for _, tool := range append(slices.Clone(PaidTools), FreeTools...) {
		if d.AllowsSessionTool(classes, source, tool) {
			out = append(out, tool)
		}
	}
	slices.Sort(out)
	return out
}

// AllowsModel reports whether any of classes may call model.
func (d Document) AllowsModel(classes []string, model string) bool {
	for _, c := range classes {
		if cl, ok := d.Classes[c]; ok && slices.Contains(cl.Models, model) {
			return true
		}
	}
	return false
}

// ClassChat is the capability of a project's chat task, which no policy
// class may take: its models are the operator's and the compaction's.
const ClassChat = "chat"

// FactoryModel is the alias factoryd calls for purpose on a task of
// classes: the first of classes that names one; "" when none does. The
// project agent's (operator) is the chat block's; the compaction's and
// the thread naming's are Small, for a chat task only; a chat task's look
// is the chat block's vision model, a session's its class's.
func (d Document) FactoryModel(classes []string, purpose string) string {
	chat := slices.Contains(classes, ClassChat)
	if purpose == PurposeVision && chat {
		return d.ChatOf(ProjectConfig{}).VisionModel
	}
	if purpose == PurposeOperator || purpose == PurposeCompact || purpose == PurposeTitle {
		switch {
		case !chat:
			return ""
		case purpose == PurposeCompact, purpose == PurposeTitle:
			return Small
		}
		return d.ChatOf(ProjectConfig{}).Model
	}
	for _, c := range classes {
		cl := d.Classes[c]
		var alias string
		switch purpose {
		case PurposeReview:
			alias = cl.ReviewModel
		case PurposeSupervisor, PurposeGateSupervisor, PurposeBrief:
			alias = cl.SupervisorModel
		case PurposeAudit:
			alias = cl.AuditModel
		case PurposeCheckin:
			alias = cmp.Or(cl.CheckinModel, cl.ReviewModel)
		case PurposeVision:
			alias = cl.VisionModel
		}
		if alias != "" {
			return alias
		}
	}
	return ""
}

// FactoryModels are the aliases factoryd may call on a task of classes,
// Big beside Premium, sorted, once each.
func (d Document) FactoryModels(classes []string) []string {
	var out []string
	for _, p := range Purposes {
		if m := d.FactoryModel(classes, p); m != "" {
			out = append(out, m)
		}
	}
	return withFallback(out)
}

// Models are the aliases any of classes may call, Big beside Premium,
// sorted, once each.
func (d Document) Models(classes []string) []string {
	var out []string
	for _, c := range classes {
		out = append(out, d.Classes[c].Models...)
	}
	return withFallback(out)
}

// SessionModel is the alias a plan or report session of a task of classes
// runs on: the first of classes that names one (ruling 330); "" when none
// does.
func (d Document) SessionModel(classes []string) string {
	for _, c := range classes {
		if m := d.Classes[c].SessionModel; m != "" {
			return m
		}
	}
	return ""
}

// ClampBudget is a new task's budget: the default when none (or a
// non-positive one) was requested, never more than the task maximum.
func (b Budget) ClampBudget(requested float64) float64 {
	if requested <= 0 {
		requested = b.TaskDefaultEUR
	}
	return min(requested, b.TaskMaxEUR)
}

// CardSizes are the sizes a card may have.
var CardSizes = []string{"small", "medium", "large"}

// sizeEUR is a card's budget by its size.
var sizeEUR = map[string]float64{"small": 2, "medium": 5, "large": 10}

// SizeBudget is a card's budget by its size: small 2, medium 5, large 10,
// any other size the default.
func (b Budget) SizeBudget(size string) float64 {
	if eur, ok := sizeEUR[size]; ok {
		return eur
	}
	return b.TaskDefaultEUR
}

// CardBudget is a new card's budget: the one asked for, else its size's,
// at most the project's card maximum (without one, a large card's budget)
// and the policy's task maximum. A card that asks for nothing gets the
// default, at most the project's card maximum and the policy's task
// maximum. A strong card (filed on the strong rung) is given at least a
// medium card's budget unless it asked for its own.
func CardBudget(b Budget, p *ProjectBudget, size string, strong bool, requested float64) float64 {
	ceiling, own := b.SizeBudget("large"), p != nil && p.TaskMaxEUR > 0
	if own {
		ceiling = p.TaskMaxEUR
	}
	floor := func(eur float64) float64 {
		if strong {
			return max(eur, sizeEUR["medium"])
		}
		return eur
	}
	_, sized := sizeEUR[size]
	switch {
	case requested > 0:
	case sized:
		requested = floor(sizeEUR[size])
	case own:
		return min(floor(b.TaskDefaultEUR), ceiling, b.TaskMaxEUR)
	default:
		return min(floor(b.TaskDefaultEUR), b.TaskMaxEUR)
	}
	if eur := min(requested, ceiling, b.TaskMaxEUR); eur > 0 {
		return eur
	}
	return floor(b.TaskDefaultEUR)
}
