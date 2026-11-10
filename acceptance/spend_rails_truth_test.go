package acceptance

// The row-truth test of the spend-rail plan (plan #1548, card 4): beside
// the drift tests in spend_rails_test.go, which hold the table's shape,
// TestAccRowTruthAllCaps holds the rows whose zero or trip column quotes
// code text to the code's real strings, in the four tables the plan
// named: wantTrip (the trip text), wantText (the zero column's text),
// zeroNamed (the caps a missing key reads as zero) and refused (the
// gateway's decision constants). Every string here was traced to the
// code before it landed - policy.go's validate lines, holds.go's
// refusals, escalation.go's defaults, store.go's draw bounds,
// workflow_stages.go's and supervise.go's and limits.go's stops,
// activities.go's and question.go's cap cards, checkin.go's stops,
// wake.go's and wakes.go's chat budget, the board template, the step-cap
// marker opencode_record_test.go pins, and cloud.go's and agent.go's
// envelope template. Where a row and the code disagree the code is
// right: the catalogue row in internal/policy/manual.go was fixed and
// the page regenerated, never this test weakened. limits.supervisor_calls
// quotes a validate line but decides nothing today, so it stays with the
// judgement; the rows that quote nothing (limits.check_in,
// limits.review_diff_kib, the check-in shapers, limits.checkin_sessions,
// budget.session_floor_frontier_eur) do too - the first and last of those
// by their place in zeroNamed.

import (
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// zeroNamed are the caps the page marks as one a missing key reads as
// zero: limits.check_in (policy.go: no check-in) and the two session
// floors (no floor beyond the opening calls). No other row may claim it.
var zeroNamed = []string{
	"limits.check_in", "budget.session_floor_cheap_eur", "budget.session_floor_frontier_eur",
}

// refused names the decision constants of internal/policy/holds.go a
// row's trip column carries where the gateway decides; the tool caps'
// two are literals of FitTool, not constants, so they stand as strings.
var refused = map[string][]string{
	"budget.task_default_eur":  {policy.RefusedBudget},
	"budget.day_eur":           {policy.RefusedDayCap},
	"budget.month_eur":         {policy.RefusedMonthCap, policy.RefusedProjectCap},
	"budget.premium_month_eur": {policy.RefusedPremiumCap},
	"chat.monthly_eur":         {policy.RefusedBudget},
	"classes.<c>.tools":        {"refused_tool_requests", "refused_tool_eur"},
}

// wantTrip is the code's real text each row's trip column must quote.
var wantTrip = map[string][]string{
	"limits.sessions":           {`"the session cap is reached: every finding sent to the agent so far goes to a human"`},
	"limits.agent_time":         {`"the task's wall-clock cap is reached"`, `"the agent time is used up: every finding sent to the agent so far goes to a human"`},
	"limits.supervisor_timeout": nil, // the call is cut at the bound; no line names it to the agent
	"limits.stronger_after":     {`"factoryd: this session continues on the stronger model, %s: %s. Carry on from where you are."`},
	"limits.steps":              {`"CRITICAL - MAXIMUM STEPS REACHED"`},
	"limits.nudge_after":        {`"You repeated the same call %s (%s). Change approach, then continue."`},
	"limits.nudges":             {`"%s: the same call %d times in a row after %s"`},
	"limits.same_call":          {`"%s: the same call %d times in a row after %s"`, `"tool %s called %d times with the same arguments %s after %s"`},
	"limits.checkin_calls":      {`"check-in call cap reached after %d calls"`},
	"limits.checkin_time":       {`"check-in time cap reached after %s"`},
	"limits.checkin_share":      {`"check-in: spend_above_policy: the check-in's share of the budget is spent"`},
	"limits.warn_at":            {`"factoryd: %.0f percent of this card's budget is spent, %.2f of %.2f EUR are left. Finish what runs, run bash check changed if it is quick, and end the session with your last message, so the factory pushes your work."`},
	"limits.plan_warn_at":       {`"factoryd: %.0f percent of this card's budget is spent, %.2f of %.2f EUR are left. File the plan now with the file_plan tool, with what you have read: a plan filed late is better than none."`},
	"limits.no_push_spend":      {`"this session spent %.2f EUR of the %.2f EUR the card had left at its start (%.0f percent, the line is %.0f) and no work is pushed"`},
	"budget.task_default_eur":   {`"free headroom %.6f EUR (%s) fits %d output tokens after a request of %d bytes (%s %.0f prompt tokens), below the minimum call of %d"`},
	"budget.task_max_eur":       nil, // the cut is silent; only the load refuses
	"budget.day_eur": {`"the day cap cannot fund a call: Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit."`,
		`"%.6f EUR of the day's %.2f EUR (budget.day_eur) is free"`},
	"budget.month_eur": {`"; the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit"`,
		`"the month cap cannot fund a call: Raise budget.month_eur in policy.yaml by a commit and answer, or wait for the next UTC month and answer."`,
		`"; the project cap budget.month_eur in projects/<project>.yaml is spent, and only a human raises it, by a commit"`,
		`"the project's month cap is reached"`, `"The project's month cap is reached"`},
	"budget.session_floor_cheap_eur": {`"the remaining budget cannot fund a call: Raise the budget (Fund on the board) and answer, or close the card."`,
		`"%.3f EUR of the %.3f EUR budget remains"`},
	"budget.premium_month_eur": {`"free headroom %.6f EUR (%s) fits %d of the call's %d output tokens after a request of %d bytes (%s %.0f prompt tokens); the premium cap budget.premium_month_eur in policy.yaml cannot pay for it"`},
	"chat.monthly_eur": {`"the chat of %s stands at its budget of €%.2f: the chat's budget is used up"`,
		`"the chat's monthly budget is spent: the chat stands until it is raised or the month turns"`},
	"escalation.pool.sessions":        {`"the escalation pool leaves %s this month: only the human draws more"`},
	"escalation.pool.agent_time":      {`"the escalation pool leaves %s this month: only the human draws more"`},
	"escalation.pool.eur":             {`"the escalation pool leaves %s this month: only the human draws more"`},
	"escalation.card_draw.sessions":   {`"the card's draw bound leaves %s: rung 2 or the human may draw more"`},
	"escalation.card_draw.agent_time": {`"the card's draw bound leaves %s: rung 2 or the human may draw more"`},
	"escalation.card_draw.eur":        {`"the card's draw bound leaves %s: rung 2 or the human may draw more"`},
	"classes.<c>.tools": {`"%s: %d of the task's %d requests used"`,
		`"%s: %.6f EUR used and a worst case of %.6f EUR exceed the task's cap of %.6f EUR on it"`},
	"envelope_default": {`"pending human approval %d: the request is beyond the project's envelope (%s). Nothing was created or charged. Poll with resource_request_status (request_id %d); once approved, call commission_server again with the same arguments."`,
		`"the project already holds %d of its %d servers"`,
		`"the reservation of %.6f EUR on top of %.6f EUR this month exceeds the envelope's %.2f EUR a month"`},
}

// wantText is the zero column's text: the validate line, the load
// refusal or the default a 0 or absent key reads as, each traced to
// policy.go's validate, escalation.go's defaults or policy.go's
// Default* constants. The zero-named caps carry no text: their zero
// column's claim is zeroNamed's alone.
var wantText = map[string][]string{
	"limits.sessions":                 {`"policy: limits.sessions must be positive, is %d"`},
	"limits.agent_time":               {`"policy: limits.agent_time must lie between 1m and 72h0m0s, is %s"`},
	"limits.supervisor_timeout":       {`"policy: limits.supervisor_timeout must lie between 30s and 15m0s, is %s"`},
	"limits.stronger_after":           {"the first return climbs"},
	"limits.steps":                    {"DefaultSteps 200"},
	"limits.nudge_after":              {"DefaultNudgeAfter 3"},
	"limits.nudges":                   {"DefaultNudges 1"},
	"limits.same_call":                {"DefaultSameCall 4"},
	"limits.checkin_calls":            {"DefaultCheckinCalls 20"},
	"limits.checkin_time":             {"CheckinCallTime 90s", "MaxCheckinTime 30m"},
	"limits.checkin_share":            {"DefaultCheckinShare 10"},
	"limits.warn_at":                  {"DefaultWarnAt 80"},
	"limits.plan_warn_at":             {"DefaultPlanWarnAt 67"},
	"limits.no_push_spend":            {"DefaultNoPushSpend 60"},
	"budget.task_default_eur":         {`"policy: budget.task_default_eur and task_max_eur must be positive"`},
	"budget.task_max_eur":             {`"policy: a session floor of %g exceeds budget.task_max_eur %g, which no card could fund"`},
	"budget.day_eur":                  {`"policy: budget.day_eur must not be negative (0 stops all spend)"`},
	"budget.month_eur":                {"refused as the day's"},
	"budget.premium_month_eur":        {"Premium has no cap of its own"},
	"chat.monthly_eur":                {"DefaultChat sets at 5"},
	"escalation.pool.sessions":        {"there 30 sessions"},
	"escalation.pool.agent_time":      {"there 30h"},
	"escalation.pool.eur":             {"there 60 EUR"},
	"escalation.card_draw.sessions":   {"there 2 sessions"},
	"escalation.card_draw.agent_time": {"there 1h"},
	"escalation.card_draw.eur":        {"there 4 EUR"},
	"classes.<c>.tools":               {"refused at load when not positive"},
	"envelope_default":                {`"envelope_default.monthly_eur must be positive"`, "volume_max_gb 0 means no volumes"},
}

// TestAccRowTruthAllCaps holds the doc's quoting rows to the code's real
// strings: every entry row carries its wantTrip quotes and its refused
// decisions in the trip column and its wantText in the zero column,
// every zero-named cap's zero column names what a missing key reads as
// and no other row claims it, and the tables agree - refused and
// wantText name only wantTrip's keys, and no wantText key is zero-named.
func TestAccRowTruthAllCaps(t *testing.T) {
	rows, byKey := readManualSpendRails(t)
	if len(wantTrip) != 29 || len(wantText) != 28 || len(zeroNamed) != 3 {
		t.Fatalf("the row-truth tables hold %d trip, %d text and %d zero-named keys, want 29, 28 and 3",
			len(wantTrip), len(wantText), len(zeroNamed))
	}
	for key, trips := range wantTrip {
		row, ok := byKey[key]
		if !ok {
			t.Errorf("the page holds no row %s", key)
			continue
		}
		for _, want := range trips {
			if !strings.Contains(row.cells[2], want) {
				t.Errorf("the row %s's trip column lacks %s", key, want)
			}
		}
		for _, decision := range refused[key] {
			if !strings.Contains(row.cells[2], decision) {
				t.Errorf("the row %s's trip column lacks the decision %s", key, decision)
			}
		}
	}
	for key, texts := range wantText {
		row, ok := byKey[key]
		if !ok {
			t.Errorf("the page holds no row %s", key)
			continue
		}
		for _, want := range texts {
			if !strings.Contains(row.cells[1], want) {
				t.Errorf("the row %s's zero column lacks %s", key, want)
			}
		}
	}
	for _, key := range zeroNamed {
		row, ok := byKey[key]
		if !ok {
			t.Errorf("the page holds no row %s", key)
			continue
		}
		if !strings.Contains(row.cells[1], "a missing key reads as zero") {
			t.Errorf("the row %s's zero column does not name the cap a missing key reads as zero", key)
		}
	}
	for _, row := range rows {
		if strings.Contains(row.cells[1], "a missing key reads as zero") && !slices.Contains(zeroNamed, row.key) {
			t.Errorf("the row %s claims a missing key reads as zero, which it does not", row.key)
		}
	}
	for key := range refused {
		if _, ok := wantTrip[key]; !ok {
			t.Errorf("the refused table names %s, which holds no trip entry", key)
		}
	}
	for key := range wantText {
		if _, ok := wantTrip[key]; !ok {
			t.Errorf("the wantText table names %s, which holds no trip entry", key)
		}
		if slices.Contains(zeroNamed, key) {
			t.Errorf("the wantText table names %s, which a missing key reads as zero", key)
		}
	}
	// the zero-named caps that quote (the cheap floor) are entries; the
	// rest (check_in, the frontier floor) quote nothing and stay out
	for _, key := range zeroNamed {
		if _, ok := wantTrip[key]; ok != (key == "budget.session_floor_cheap_eur") {
			t.Errorf("the zero-named cap %s holds a trip entry but should not, or the other way", key)
		}
	}
}
