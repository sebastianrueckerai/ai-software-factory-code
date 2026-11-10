package policy

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// SpendRailBegin and SpendRailEnd bracket the generated spend-rail table in
// the manual page. RewriteSpendRails replaces everything from the one line
// through the other.
const (
	SpendRailBegin = "<!-- spend-rail: begin -->"
	SpendRailEnd   = "<!-- spend-rail: end -->"
)

// spendRow is one authored row of the spend-rail table: the yaml key of a
// cap, what the cap stops, what 0 and an absent key read as, and the text
// the card and the agent see when it trips, quoted from the code.
type spendRow struct {
	key   string
	stops string
	zero  string
	trip  string
}

// spendRailRows is the catalogue of the flat caps, one row per key of
// limits, budget, escalation.pool, escalation.card_draw and
// chat.monthly_eur, in the schema's path order. The matrix rows beside it
// cover the blocks a row per key cannot state.
var spendRailRows = []spendRow{
	{"limits.sessions",
		"how many agent sessions a task gets before every finding goes to a human (the round cap). A human may grant more",
		"0 is refused: \"policy: limits.sessions must be positive, is %d\". The key is required, absent never loads",
		"the card reads \"the session cap is reached: every finding sent to the agent so far goes to a human\""},
	{"limits.agent_time",
		"the agent's working time across all sessions of a task. Waiting on checks or on a reviewer does not count. A class, process or project may set its own, the most specific wins",
		"0 is refused: \"policy: limits.agent_time must lie between 1m and 72h0m0s, is %s\"",
		"the session stops on \"the task's wall-clock cap is reached\" and the card reads \"the agent time is used up: every finding sent to the agent so far goes to a human\""},
	{"limits.check_in",
		"how long a session runs before it must check in with the supervisor, at least 10m",
		"0 and absent read the same: no check-in. This is one of the caps a missing key reads as zero",
		"the session stops into a check-in conversation (trip kind check_in) whose own caps can send its ask to a human"},
	{"limits.supervisor_calls",
		"how many supervisor model calls one session may make. The next trip escalates without one",
		"0 is refused: \"policy: limits.supervisor_calls must be positive, is %d\"",
		"nothing today: no code reads the key, so it decides nothing. The schema still requires it"},
	{"limits.supervisor_timeout",
		"one supervisor or review model call, 30s to 15m",
		"0 is refused: \"policy: limits.supervisor_timeout must lie between 30s and 15m0s, is %s\"",
		"the call is cut at the bound. No line names it to the agent. It also bounds the gate activity SuperviseGate at the timeout plus two minutes"},
	{"limits.stronger_after",
		"how many returns from the same stage a task takes on its model before the next return climbs to the stronger rung",
		"0 and absent read the same: the first return climbs",
		"\"factoryd: this session continues on the stronger model, %s: %s. Carry on from where you are.\""},
	{"limits.steps",
		"one agent session's model turns (opencode's agent steps)",
		"0 and absent read DefaultSteps 200",
		"\"CRITICAL - MAXIMUM STEPS REACHED\" opens the message the harness appends at the cap, which the model's next call reads. The worker pod gets the cap as its STEPS env"},
	{"limits.nudge_after",
		"how many identical tool calls in a row (same tool, same input) earn the running session a message to change approach, at least 2",
		"0 and absent read DefaultNudgeAfter 3",
		"\"You repeated the same call %s (%s). Change approach, then continue.\""},
	{"limits.nudges",
		"how many nudge messages one repetition gets, at least 1. The next run of the same call trips the supervisor",
		"0 and absent read DefaultNudges 1",
		"after its nudges the same call trips: \"%s: the same call %d times in a row after %s\""},
	{"limits.same_call",
		"how many identical tool calls in one session, in a row or not, trip the same_call detector, at least 2 and above nudge_after",
		"0 and absent read DefaultSameCall 4",
		"in a row: \"%s: the same call %d times in a row after %s\". Across the session: \"tool %s called %d times with the same arguments %s after %s\""},
	{"limits.review_diff_kib",
		"the most diff, in KiB, one model review call reads, and the part size when a bigger diff reads in parts",
		"0 and absent read DefaultReviewDiffKiB 48",
		"no line the agent reads: the review reads a bigger diff in parts of this size, as many as it needs"},
	{"limits.checkin_calls",
		"how many model calls one check-in conversation gets. Its last call carries no tools and asks for the answer",
		"0 and absent read DefaultCheckinCalls 20",
		"\"check-in call cap reached after %d calls\", asked of a human under the reason kind spend_above_policy"},
	{"limits.checkin_time",
		"how long one check-in conversation may run before its ask goes to a human",
		"0 and absent read the per-call fallback: CheckinCallTime 90s for each of the call cap's calls, at most MaxCheckinTime 30m",
		"\"check-in time cap reached after %s\" with the cap as a duration string"},
	{"limits.checkin_share",
		"the most percent of the card's budget the check-in's calls may spend",
		"0 and absent read DefaultCheckinShare 10",
		"\"the check-in's share of the budget is spent\", asked as \"check-in: spend_above_policy: the check-in's share of the budget is spent\". A human's answer grants the card one more share"},
	{"limits.checkin_tail_kib",
		"the parent session record's tail cut into the check-in's statement",
		"0 and absent read DefaultCheckinTailKiB 24",
		"no trip: it shapes the statement the check-in reads"},
	{"limits.checkin_handover_chars",
		"the check-in's handover note cap, matching the session's last-message cut",
		"0 and absent read DefaultCheckinHandoverChars 2000",
		"no trip: the handover note is cut at the cap"},
	{"limits.checkin_sessions",
		"how many fresh sessions one confirmed finding gets before the human is asked for it",
		"0 and absent read DefaultCheckinSessions 1",
		"past the sessions the ask goes to the human on the finding's card"},
	{"limits.warn_at",
		"the percent of a card's budget spent when factoryd tells the running agent to wrap up and push, 1 to 99",
		"0 and absent read DefaultWarnAt 80",
		"\"factoryd: %.0f percent of this card's budget is spent, %.2f of %.2f EUR are left for its calls (the model review and the audit hold %.2f EUR of it). Finish what runs, run bash check changed if it is quick, and end the session with your last message, so the factory pushes your work.\""},
	{"limits.plan_warn_at",
		"the same for a planner session",
		"0 and absent read DefaultPlanWarnAt 67",
		"\"factoryd: %.0f percent of this card's budget is spent, %.2f of %.2f EUR are left for its calls (the model review and the audit hold %.2f EUR of it). File the plan now with the file_plan tool, with what you have read: a plan filed late is better than none.\""},
	{"limits.no_push_spend",
		"the percent of a card's budget spent with nothing pushed at which the session supervisor is called, 1 to 99",
		"0 and absent read DefaultNoPushSpend 60",
		"the supervisor is called on \"%.2f of the card's %.2f EUR are spent (%.0f percent, the line is %.0f) and no work is pushed\""},
	{"limits.gate_reserve_eur",
		"the EUR held out of a card's budget for its gates: the whole of a model review and of an audit at the models the gates use. A session's calls fit budget - spent - this reserve; factoryd's gate calls and a waiting card's brief see the full budget - spent",
		"0 and absent read DefaultGateReserveEUR 0.40",
		"the session's call is refused with decision refused_budget, the reason ending \"; the card spent %.2f EUR, and %.2f EUR is held for the review and the audit\". The card's money stop reads \"the remaining budget cannot fund a call\" with the state naming the spent and held EUR; the review and the audit run without a fund"},
	{"budget.task_default_eur",
		"a task's budget when its intake asks for none",
		"0 is refused: \"policy: budget.task_default_eur and task_max_eur must be positive\"",
		"a call the budget cannot fit meets the gateway's refusal \"free headroom %.6f EUR (%s) fits %d output tokens after a request of %d bytes (%s %.0f prompt tokens), below the minimum call of %d\", decision refused_budget"},
	{"budget.task_max_eur",
		"the most a task may be given. A card's budget is its intake's ask cut at this",
		"0 is refused as task_default_eur's. A session floor above it is refused: \"policy: a session floor of %g exceeds budget.task_max_eur %g, which no card could fund\"",
		"the cut is silent. A floor above it refuses the whole file at load, so nothing runs"},
	{"budget.day_eur",
		"all tasks together per UTC day",
		"0 stops all spend. A negative one is refused: \"policy: budget.day_eur must not be negative (0 stops all spend)\"",
		"the gateway refuses with decision refused_day_cap. The card reads \"the day cap cannot fund a call: Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit.\" with the state \"%.6f EUR of the day's %.2f EUR (budget.day_eur) is free\""},
	{"budget.month_eur",
		"all tasks together per UTC calendar month; the same key in a project's file, projects/<project>.yaml, caps that project's tasks together",
		"0 stops all spend. A negative one is refused as the day's",
		"the gateway refuses with decision refused_month_cap, the reason ending \"; the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit\". The card reads \"the month cap cannot fund a call: Raise budget.month_eur in policy.yaml by a commit and answer, or wait for the next UTC month and answer.\" The project cap, that same key, refuses with decision refused_project_cap, the reason ending \"; the project cap budget.month_eur in projects/<project>.yaml is spent, and only a human raises it, by a commit\"; the card reads \"the project's month cap is reached\" and its ask \"The project's month cap is reached\""},
	{"budget.session_floor_cheap_eur",
		"the least free budget a session on the cheap rung needs to start",
		"0 and absent read the same: no floor beyond the session's opening calls. These are the caps a missing key reads as zero",
		"the card reads \"the remaining budget cannot fund a call: Raise the budget (Fund on the board) and answer, or close the card.\" with the state \"The card spent %.2f EUR of its %.2f EUR budget; %.2f EUR is held for the review and the audit, which need %.2f EUR\" and the fund that opens a session, \"so a fund of %.2f EUR opens it\""},
	{"budget.session_floor_frontier_eur",
		"the least free budget a session on the frontier rung (above the ladder's start) needs to start",
		"0 and absent read the same: no floor beyond the session's opening calls. This is one of the caps a missing key reads as zero",
		"the same card as the cheap rung's floor, the frontier floor weighed"},
	{"budget.premium_month_eur",
		"the calls on Premium together per UTC calendar month, inside the caps above",
		"0 and absent read the same: Premium has no cap of its own",
		"\"free headroom %.6f EUR (%s) fits %d of the call's %d output tokens after a request of %d bytes (%s %.0f prompt tokens); the premium cap budget.premium_month_eur in policy.yaml cannot pay for it\", decision refused_premium_cap. The gateway serves the call on Big instead"},
	{"escalation.pool.sessions",
		"the sessions all escalation rungs together may grant in a project per UTC month",
		"0 and absent read the same: the part takes policy.yaml's figure, there 30 sessions",
		"\"the escalation pool leaves %s this month: only the human draws more\""},
	{"escalation.pool.agent_time",
		"the agent time all escalation rungs together may extend in a project per UTC month",
		"0 and absent read the same: the part takes policy.yaml's figure, there 30h",
		"\"the escalation pool leaves %s this month: only the human draws more\""},
	{"escalation.pool.eur",
		"the money all escalation rungs together may grant in a project per UTC month",
		"0 and absent read the same: the part takes policy.yaml's figure, there 60 EUR",
		"\"the escalation pool leaves %s this month: only the human draws more\""},
	{"escalation.card_draw.sessions",
		"the sessions the code and the check-in may draw from the pool for one medium card over its whole life. A small card draws half, a large one one and a half times, sessions rounded up",
		"0 and absent read the same: the part takes policy.yaml's figure, there 2 sessions",
		"\"the card's draw bound leaves %s: rung 2 or the human may draw more\""},
	{"escalation.card_draw.agent_time",
		"the agent time the code and the check-in may draw from the pool for one medium card over its whole life, sized with the card",
		"0 and absent read the same: the part takes policy.yaml's figure, there 1h",
		"\"the card's draw bound leaves %s: rung 2 or the human may draw more\""},
	{"escalation.card_draw.eur",
		"the money the code and the check-in may draw from the pool for one medium card over its whole life, sized with the card",
		"0 and absent read the same: the part takes policy.yaml's figure, there 4 EUR",
		"\"the card's draw bound leaves %s: rung 2 or the human may draw more\""},
	{"chat.monthly_eur",
		"the project's chat task's budget each UTC month",
		"0 and absent read the same: the policy's figure, which DefaultChat sets at 5",
		"a wake reads \"the chat of %s stands at its budget of €%.2f: the chat's budget is used up\". The board shows \"the chat's monthly budget is spent: the chat stands until it is raised or the month turns\". A chat call the budget cannot fit meets the gateway's refused_budget"},
}

// spendRailMatrix is the two matrix rows: one for every classes.<c>.tools
// block, one for envelope_default. A row per class or per resource would
// say the same thing many times, so each names the block.
var spendRailMatrix = []spendRow{
	{"classes.<c>.tools",
		"the gateway tools a task of the class may call: a paid tool with its per-task caps (max_requests, max_eur), a free tool as {}",
		"a tool the class does not list is not allowed at all, which is not a cap of 0. A paid tool's caps are refused at load when not positive",
		"the requests cap: \"%s: %d of the task's %d requests used\", decision refused_tool_requests. The money cap: \"%s: %.6f EUR used and a worst case of %.6f EUR exceed the task's cap of %.6f EUR on it\", decision refused_tool_eur"},
	{"envelope_default",
		"what a project's tasks may commission in the agent cloud without a human: EUR per calendar month (reservations and accrued cost together), the server types, how many servers at once, the longest lease, the largest volume. It is also the most any project file's envelope may set",
		"monthly_eur 0 is refused at load: \"envelope_default.monthly_eur must be positive\". volume_max_gb 0 means no volumes",
		"\"pending human approval %d: the request is beyond the project's envelope (%s). Nothing was created or charged. Poll with resource_request_status (request_id %d); once approved, call commission_server again with the same arguments.\" with the violation, e.g. \"the project already holds %d of its %d servers\" or \"the reservation of %.6f EUR on top of %.6f EUR this month exceeds the envelope's %.2f EUR a month\""},
}

// SpendRailTable renders the manual's spend-rail section from the policy
// schema and the catalogue: one markdown row per key, the flat caps in the
// schema's path order then the two matrix rows, between the section's
// marker lines. A schema and a catalogue that disagree refuse the build.
func SpendRailTable() (string, error) {
	s, err := kindPolicy.schema()
	if err != nil {
		return "", err
	}
	keys, anchors, err := spendRailWalk(s)
	if err != nil {
		return "", err
	}
	rows, err := joinSpendRails(keys, anchors)
	if err != nil {
		return "", err
	}
	return string(renderSpendRails(rows)), nil
}

// RewriteSpendRails replaces the spend-rail section in doc, from its begin
// marker line through its end marker line, with the section this build
// renders. Either marker missing, or the end before the begin, is an error.
func RewriteSpendRails(doc []byte) ([]byte, error) {
	table, err := SpendRailTable()
	if err != nil {
		return nil, err
	}
	i, j := bytes.Index(doc, []byte(SpendRailBegin)), bytes.Index(doc, []byte(SpendRailEnd))
	if i < 0 || j < 0 {
		return nil, errors.New("spend rail: the document carries no spend-rail markers")
	}
	if j < i {
		return nil, errors.New("spend rail: the end marker stands before the begin marker")
	}
	rest := doc[j+len(SpendRailEnd):]
	if k := bytes.IndexByte(rest, '\n'); k >= 0 {
		rest = rest[k+1:]
	} else {
		rest = nil
	}
	out := make([]byte, 0, i+len(table)+len(rest))
	out = append(out, doc[:i]...)
	out = append(out, table...)
	return append(out, rest...), nil
}

// spendRailWalk reads the policy schema's spend-rail key paths: the
// property names of limits, budget, escalation.pool and
// escalation.card_draw, and chat.monthly_eur, each path's names in its
// PropertyOrder. It returns the flat keys in path order and the matrix
// anchors it found: a classes map whose Class schema has a tools property,
// and an envelope_default property.
func spendRailWalk(s *jsonschema.Schema) ([]string, []string, error) {
	if s == nil {
		return nil, nil, errors.New("spend rail: no policy schema")
	}
	prop := func(node *jsonschema.Schema, key string) (*jsonschema.Schema, error) {
		if node == nil || node.Properties[key] == nil {
			return nil, fmt.Errorf("spend rail: the policy schema has no %s", key)
		}
		return node.Properties[key], nil
	}
	var keys []string
	prefix := func(pre string, node *jsonschema.Schema) {
		for _, key := range node.PropertyOrder {
			keys = append(keys, pre+"."+key)
		}
	}
	limits, err := prop(s, "limits")
	if err != nil {
		return nil, nil, err
	}
	budget, err := prop(s, "budget")
	if err != nil {
		return nil, nil, err
	}
	escalation, err := prop(s, "escalation")
	if err != nil {
		return nil, nil, err
	}
	pool, err := prop(escalation, "pool")
	if err != nil {
		return nil, nil, err
	}
	draw, err := prop(escalation, "card_draw")
	if err != nil {
		return nil, nil, err
	}
	chat, err := prop(s, "chat")
	if err != nil {
		return nil, nil, err
	}
	if _, err := prop(chat, "monthly_eur"); err != nil {
		return nil, nil, err
	}
	prefix("limits", limits)
	prefix("budget", budget)
	prefix("escalation.pool", pool)
	prefix("escalation.card_draw", draw)
	keys = append(keys, "chat.monthly_eur")

	var anchors []string
	classes, err := prop(s, "classes")
	if err != nil {
		return nil, nil, err
	}
	if classes.AdditionalProperties != nil && classes.AdditionalProperties.Properties["tools"] != nil {
		anchors = append(anchors, "classes.<c>.tools")
	}
	if s.Properties["envelope_default"] != nil {
		anchors = append(anchors, "envelope_default")
	}
	return keys, anchors, nil
}

// joinSpendRails pairs the walked keys and matrix anchors with the
// catalogue and returns the rows to render, the flat rows in the keys'
// order then the matrix rows. A key on either side alone, or a matrix
// anchor the schema lacks, is an error naming it.
func joinSpendRails(keys, anchors []string) ([]spendRow, error) {
	byKey := make(map[string]spendRow, len(spendRailRows))
	for _, r := range spendRailRows {
		byKey[r.key] = r
	}
	var out []spendRow
	var errs []error
	for _, key := range keys {
		r, ok := byKey[key]
		if !ok {
			errs = append(errs, fmt.Errorf("spend rail: schema key %s has no catalogue row", key))
			continue
		}
		out = append(out, r)
	}
	for _, r := range spendRailRows {
		if !slices.Contains(keys, r.key) {
			errs = append(errs, fmt.Errorf("spend rail: catalogue row %s is not in the schema", r.key))
		}
	}
	for _, r := range spendRailMatrix {
		if !slices.Contains(anchors, r.key) {
			errs = append(errs, fmt.Errorf("spend rail: matrix anchor %s is not in the schema", r.key))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return append(out, spendRailMatrix...), nil
}

// renderSpendRails writes rows as the marked markdown table, deterministic
// bytes.
func renderSpendRails(rows []spendRow) []byte {
	var b bytes.Buffer
	b.WriteString(SpendRailBegin + "\n")
	b.WriteString("| key | what it stops | 0 and absent read as | what it says when it trips |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, r := range rows {
		b.WriteString("| " + strings.Join([]string{r.key, r.stops, r.zero, r.trip}, " | ") + " |\n")
	}
	b.WriteString(SpendRailEnd + "\n")
	return b.Bytes()
}
