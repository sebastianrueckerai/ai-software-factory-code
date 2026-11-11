package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A document that sets the pool's money alone keeps the default sessions,
// agent time, card draw and judgment. A document without the block reads
// the default whole.
func TestEscalationDefaultsFieldByField(t *testing.T) {
	d, err := Parse([]byte(valid + "escalation: {pool: {eur: 10}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Escalation{Pool: Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 10}, CardDraw: Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}, Judgment: "always", Rung1Runs: 6}
	if got := (Tree{Document: d}).EscalationOf("demo-app/demo-app"); got != want {
		t.Errorf("escalation = %+v, want %+v", got, want)
	}
	d, err = Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	want.Pool.EUR = 60
	if got := (Tree{Document: d}).EscalationOf("demo-app/demo-app"); got != want {
		t.Errorf("escalation without the block = %+v, want %+v", got, want)
	}
}

// A project file may lower the pool and the card draw, never raise them
// above policy.yaml's (the fixture sets none, so the default's: pool 30
// sessions, 30h, 60 EUR, card draw 2, 1h, 4 EUR), and its lowered pool
// still holds its card draw.
func TestProjectEscalationMayOnlyLower(t *testing.T) {
	with := func(block string) map[string][]byte {
		files := fixtureTree(t)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], block...)
		return files
	}
	for name, tc := range map[string]struct{ block, want string }{
		"pool money":      {"escalation: {pool: {eur: 100}}\n", "project demo-app: escalation.pool.eur 100 is looser than policy.yaml's escalation.pool.eur 60"},
		"pool agent time": {"escalation: {pool: {agent_time: 40h}}\n", "project demo-app: escalation.pool.agent_time 40h0m0s is looser than policy.yaml's escalation.pool.agent_time 30h0m0s"},
		"card sessions":   {"escalation: {card_draw: {sessions: 3}}\n", "project demo-app: escalation.card_draw.sessions 3 is looser than policy.yaml's escalation.card_draw.sessions 2"},
		"pool below the card draw": {"escalation: {pool: {sessions: 1}}\n",
			"project demo-app: escalation.card_draw.sessions 2 is above escalation.pool.sessions 1, so no card could draw it: lower the card draw or raise the pool"},
		"rung1 runs":       {"escalation: {rung1_runs: 7}\n", "project demo-app: escalation.rung1_runs 7 is looser than policy.yaml's escalation.rung1_runs 6"},
		"negative":         {"escalation: {card_draw: {eur: -1}}\n", "project demo-app: escalation.card_draw.eur must not be negative, is -1"},
		"unknown judgment": {"escalation: {judgment: never}\n", `project demo-app: escalation.judgment "never": want one of [always autonomous_and_above]`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTree(with(tc.block)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := ParseTree(with("escalation: {rung1_runs: 6}\n")); err != nil {
		t.Fatalf("a project's rung1_runs equal to the policy's: %v", err)
	}
	tree, err := ParseTree(with("escalation: {pool: {eur: 20}, judgment: autonomous_and_above}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Escalation{Pool: Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 20}, CardDraw: Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}, Judgment: "autonomous_and_above", Rung1Runs: 6}
	if got := tree.EscalationOf("demo-app/demo-app"); got != want {
		t.Errorf("demo-app's escalation = %+v, want %+v", got, want)
	}
	want = Escalation{Pool: Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDraw: Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}, Judgment: "always", Rung1Runs: 6}
	if got := tree.EscalationOf("linkshort/linkshort"); got != want {
		t.Errorf("linkshort's escalation = %+v, want policy.yaml's %+v", got, want)
	}
}

// A part set to 0 reads as absent: a pool of 0 EUR takes policy.yaml's
// figure. The schema refuses 0 sessions, which the code would ignore.
func TestEscalationZeroReadsAsAbsent(t *testing.T) {
	with := func(block string) map[string][]byte {
		files := fixtureTree(t)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], block...)
		return files
	}
	tree, err := ParseTree(with("escalation: {pool: {eur: 0}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.EscalationOf("demo-app/demo-app").Pool; got != (Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}) {
		t.Errorf("pool with eur 0 = %+v, want policy.yaml's 30 sessions, 30h, 60 EUR", got)
	}
	if _, err := ParseTree(with("escalation: {card_draw: {sessions: 0}}\n")); err == nil || !strings.Contains(err.Error(), "projects/demo-app.yaml: schema:") {
		t.Errorf("ParseTree with card_draw sessions 0 = %v, want the schema's refusal", err)
	}
}

// Small halves the medium card draw, large takes one and a half times,
// sessions rounded up. Any other size keeps the medium figure.
func TestCardDrawOfScalesBySize(t *testing.T) {
	e := Escalation{CardDraw: Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	for size, want := range map[string]Draw{
		"small":  {Sessions: 1, AgentTime: 30 * time.Minute, EUR: 2},
		"medium": {Sessions: 2, AgentTime: time.Hour, EUR: 4},
		"large":  {Sessions: 3, AgentTime: 90 * time.Minute, EUR: 6},
		"":       {Sessions: 2, AgentTime: time.Hour, EUR: 4},
	} {
		if got := e.CardDrawOf(size); got != want {
			t.Errorf("CardDrawOf(%q) = %+v, want %+v", size, got, want)
		}
	}
	odd := Escalation{CardDraw: Draw{Sessions: 3}}
	if small, large := odd.CardDrawOf("small").Sessions, odd.CardDrawOf("large").Sessions; small != 2 || large != 5 {
		t.Errorf("3 sessions: small %d, large %d, want 2 (1.5 up) and 5 (4.5 up)", small, large)
	}
}

// judgment takes JudgmentValues only, in Validate and in the schema.
func TestJudgmentIsAnEnum(t *testing.T) {
	if _, err := Parse([]byte(valid + "escalation: {judgment: sometimes}\n")); err == nil || !strings.Contains(err.Error(), fmt.Sprint(JudgmentValues)) {
		t.Errorf("Parse = %v, want judgment refused naming %v", err, JudgmentValues)
	}
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemas["policy.schema.json"], &s); err != nil {
		t.Fatal(err)
	}
	if got := s.Properties["escalation"].Properties["judgment"].Enum; !reflect.DeepEqual(got, JudgmentValues) {
		t.Errorf("schema enum = %v, want %v", got, JudgmentValues)
	}
}

// policy.yaml's block holds no negative part, and its card draw fits its
// pool, the default filling what the file leaves out.
func TestEscalationRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"negative sessions":   {"escalation: {pool: {sessions: -1}}\n", "policy: escalation.pool.sessions must not be negative, is -1"},
		"negative rung1 runs": {"escalation: {rung1_runs: -1}\n", "policy: escalation.rung1_runs must not be negative, is -1"},
		"card above pool":     {"escalation: {pool: {eur: 5}, card_draw: {eur: 6}}\n", "policy: escalation.card_draw.eur 6 is above escalation.pool.eur 5, so no card could draw it: lower the card draw or raise the pool"},
		"default card above a lowered pool": {"escalation: {pool: {agent_time: 30m}}\n",
			"policy: escalation.card_draw.agent_time 1h0m0s is above escalation.pool.agent_time 30m0s, so no card could draw it: lower the card draw or raise the pool"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(valid + tc.block)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// policy.yaml's rung1_runs reaches the escalation block.
func TestRung1RunsMerges(t *testing.T) {
	d, err := Parse([]byte(valid + "escalation: {rung1_runs: 7}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := (Tree{Document: d}).EscalationOf("demo-app/demo-app").Rung1Runs; got != 7 {
		t.Errorf("Rung1Runs = %d, want 7", got)
	}
}

// Fits holds every part to the other's, Minus stops at zero in each part,
// Plus adds.
func TestDrawArithmetic(t *testing.T) {
	a := Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}
	b := Draw{Sessions: 3, AgentTime: 30 * time.Minute, EUR: 5}
	if !a.Fits(a) || a.Fits(b) || b.Fits(a) || !(Draw{}).Fits(a) {
		t.Errorf("Fits: a in a %v, a in b %v, b in a %v, zero in a %v, want true false false true", a.Fits(a), a.Fits(b), b.Fits(a), (Draw{}).Fits(a))
	}
	if got, want := a.Minus(b), (Draw{AgentTime: 30 * time.Minute}); got != want {
		t.Errorf("a minus b = %+v, want %+v", got, want)
	}
	if got, want := a.Plus(b), (Draw{Sessions: 5, AgentTime: 90 * time.Minute, EUR: 9}); got != want {
		t.Errorf("a plus b = %+v, want %+v", got, want)
	}
	if !(Draw{}).IsZero() || a.IsZero() {
		t.Errorf("IsZero: zero %v, a %v", (Draw{}).IsZero(), a.IsZero())
	}
}

// A draw's agent time is a string in JSON, as in YAML and as Limits'
// durations: the stored tree and GET /policy show 30h0m0s.
func TestDrawJSON(t *testing.T) {
	e := Escalation{Pool: Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, Judgment: JudgmentAlways}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"pool":{"sessions":30,"agent_time":"30h0m0s","eur":60},"card_draw":{},"judgment":"always"}`; string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
	var back Escalation
	if err := json.Unmarshal(b, &back); err != nil || back != e {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"agent_time":"soon"}`), &Draw{}); err == nil {
		t.Fatal("a bad duration decoded")
	}
}

func TestJudges(t *testing.T) {
	for _, c := range []struct {
		judgment, state string
		want            bool
	}{
		{"", AutonomyManual, true},
		{JudgmentAlways, AutonomyManual, true}, {JudgmentAlways, AutonomyAssisted, true},
		{JudgmentAlways, AutonomyAutonomous, true}, {JudgmentAlways, AutonomyFullAuto, true},
		{JudgmentAutonomousAndAbove, AutonomyManual, false}, {JudgmentAutonomousAndAbove, AutonomyAssisted, false},
		{JudgmentAutonomousAndAbove, AutonomyAutonomous, true}, {JudgmentAutonomousAndAbove, AutonomyFullAuto, true},
	} {
		if got := (Escalation{Judgment: c.judgment}).Judges(c.state); got != c.want {
			t.Errorf("%q at %q = %v, want %v", c.judgment, c.state, got, c.want)
		}
	}
}
