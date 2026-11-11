package factoryd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Fixture is one card's recorded life as the replay tests read it: what
// the database holds of its brief, sessions, gate verdicts, checks, asks,
// answers and cases. Script and Expect are written by hand: the export
// leaves Script empty and Expect nil, so a fixture nobody has authored
// differs from one that authors "no session, no human case". Times are
// offsets from the card's creation, so a fixture replays at any clock.
// An actor is a role: human, project_agent or factoryd.
type Fixture struct {
	Card     FixtureCard      `json:"card"`
	Sessions []FixtureSession `json:"sessions"`
	Gates    []FixtureGate    `json:"gates"`
	Checks   []FixtureChecks  `json:"checks"`
	Asks     []FixtureAsk     `json:"asks"`
	Answers  []FixtureAnswer  `json:"answers"`
	Cases    []FixtureCase    `json:"cases"`
	Script   FixtureScript    `json:"script"`
	Expect   *FixtureExpect   `json:"expect"`
}

// Roles an actor of a fixture is named by.
const (
	RoleHuman        = "human"
	RoleProjectAgent = "project_agent"
	RoleFactoryd     = "factoryd"
)

// FixtureCard is the card's row and its project's standing at export time.
// Objective is the card's brief, a placeholder naming its author (a person,
// a model or factoryd);
// Plan is nil for a card outside a plan.
type FixtureCard struct {
	Issue      int64        `json:"issue"`
	TaskID     string       `json:"task_id"`
	Repository string       `json:"repository"`
	Kind       string       `json:"kind"`
	Track      string       `json:"track"`
	Source     string       `json:"source"`
	Size       string       `json:"size"`
	Rung       string       `json:"rung"`
	BudgetEUR  float64      `json:"budget_eur"`
	CeilingEUR float64      `json:"ceiling_eur"`
	Objective  string       `json:"objective"`
	Plan       *FixturePlan `json:"plan"`
	DependsOn  []FixtureDep `json:"depends_on"`
	Autonomy   string       `json:"autonomy"`
	MergeRule  string       `json:"merge_rule"`
}

// FixturePlan is the plan a card belongs to.
type FixturePlan struct {
	Issue int64  `json:"issue"`
	Name  string `json:"name"`
}

// FixtureDep is a card the card waits on; Why is the end reason of one
// that ended.
type FixtureDep struct {
	Issue  int64  `json:"issue"`
	Status string `json:"status"`
	Why    string `json:"why"`
}

// FixtureSession is one attempt: its end, its cost, the check-in that
// judged it, nil when none ran, and the provider refusals that held it.
type FixtureSession struct {
	Attempt    int              `json:"attempt"`
	Model      string           `json:"model"`
	Outcome    string           `json:"outcome"`
	Commit     string           `json:"commit"`
	Pushed     bool             `json:"pushed"`
	CostEUR    float64          `json:"cost_eur"`
	Calls      int              `json:"calls"`
	AgentTimeS int64            `json:"agent_time_s"`
	Summary    string           `json:"summary"`
	CheckIn    *FixtureCheckIn  `json:"checkin"`
	Refusals   []FixtureRefusal `json:"refusals"`
}

// FixtureRefusal is a model provider's refusal that held a session before
// its backoff.
type FixtureRefusal struct {
	Status   string  `json:"status"`
	Reason   string  `json:"reason"`
	BackoffS float64 `json:"backoff_s"`
}

// FixtureCheckIn is a check-in's record; Verdicts holds the verdict state
// of each finding by its id.
type FixtureCheckIn struct {
	Moment     string            `json:"moment"`
	Decision   string            `json:"decision"`
	ReasonKind string            `json:"reason_kind"`
	Reason     string            `json:"reason"`
	Verdicts   map[string]string `json:"verdicts"`
}

// FixtureGate is one gate result on a head: the review, the audit, the
// criteria gate, or the policy gate, whose verdict is the risk class it gave
// the head. A gate that did not run has no
// verdict and NotRun, the recorded reason (a refusal for money or by the
// provider included).
type FixtureGate struct {
	Head     string           `json:"head"`
	Gate     string           `json:"gate"`
	Verdict  string           `json:"verdict"`
	Findings []FixtureFinding `json:"findings"`
	NotRun   string           `json:"not_run"`
}

// FixtureFinding is a finding's text and its id (findingID of the text as
// recorded).
type FixtureFinding struct {
	Text string `json:"text"`
	ID   string `json:"id"`
}

// FixtureChecks is the checks verdict on a head: its own findings and the
// failed tests the red jobs showed when a merge request recorded them.
type FixtureChecks struct {
	Head        string              `json:"head"`
	State       string              `json:"state"`
	Findings    []string            `json:"findings"`
	FailedTests []FixtureFailedTest `json:"failed_tests"`
}

// FixtureFailedTest is one failed test of a red job.
type FixtureFailedTest struct {
	Job     string `json:"job"`
	Test    string `json:"test"`
	Package string `json:"package"`
	Kind    string `json:"kind"`
}

// FixtureAsk is a question the card put to a human: AtS is the offset from
// the card's creation in seconds, CaseKind the kind of the case the row
// names (its case_id), else the kind it carries, else empty for an ask from
// before cases; Source and Reason are the row's, with which a hand
// classifies such an ask.
type FixtureAsk struct {
	AtS            int64  `json:"at_s"`
	CaseKind       string `json:"case_kind"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
	Ask            string `json:"ask"`
	Why            string `json:"why"`
	Recommendation string `json:"recommendation"`
}

// FixtureAnswer is an answer to one of the card's asks: At is the offset
// from the card's creation in seconds, By a role, Action the answer's
// action or its event kind, and Fund the money it drew for the card.
type FixtureAnswer struct {
	AtS      int64          `json:"at_s"`
	By       string         `json:"by"`
	CaseKind string         `json:"case_kind"`
	Action   string         `json:"action"`
	Args     map[string]any `json:"args"`
	FundEUR  float64        `json:"fund_eur"`
	Note     string         `json:"note"`
}

// FixtureCase is one case of the card with the steps of its rungs.
type FixtureCase struct {
	Kind       string             `json:"kind"`
	StateKey   string             `json:"state_key"`
	Problem    string             `json:"problem"`
	Facts      map[string]any     `json:"facts"`
	Status     string             `json:"status"`
	Resolution *FixtureResolution `json:"resolution"`
	Steps      []FixtureCaseStep  `json:"steps"`
}

// FixtureResolution is how a case ended: by a role, on a rung, with an
// action.
type FixtureResolution struct {
	By     string         `json:"by"`
	Rung   int            `json:"rung"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
	Reason string         `json:"reason"`
}

// FixtureCaseStep is one rung's work on a case.
type FixtureCaseStep struct {
	Rung     int    `json:"rung"`
	Actor    string `json:"actor"`
	Found    string `json:"found"`
	Decision string `json:"decision"`
	Grounds  string `json:"grounds"`
	Outcome  string `json:"outcome"`
}

// FixtureScript is what the rungs that never ran on the incident answer,
// in the order the replay asks them: rung 1 by a RungAnswer, rung 2 by a
// tool call.
type FixtureScript struct {
	Rung1 []FixtureRung1Answer `json:"rung1"`
	Rung2 []FixtureRung2Answer `json:"rung2"`
}

// FixtureRung1Answer is rung 1's answer on a case of CaseKind.
type FixtureRung1Answer struct {
	CaseKind string     `json:"case_kind"`
	Answer   RungAnswer `json:"answer"`
}

// FixtureRung2Answer is rung 2's answer on a case of CaseKind.
type FixtureRung2Answer struct {
	CaseKind string      `json:"case_kind"`
	Answer   FixtureTool `json:"answer"`
}

// FixtureTool is a rung 2 tool call: answer or attach_analysis.
type FixtureTool struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// FixtureExpect is what the ladder must do with the script: the rung (0 to
// 3) that resolved each case in order, ResolvedOpen for a case still open
// at the end or ResolvedWithdrawn for one withdrawn; how many sessions
// started, how many human cases opened and what each one's face says; how
// many turns rung 2 took; the card's status at the end when Status is set;
// the end of the note the last session started with when NoteSuffix is, and
// the issues the card waits on when WaitsOn is.
type FixtureExpect struct {
	ResolvedByRung []int         `json:"resolved_by_rung"`
	Sessions       int           `json:"sessions"`
	HumanCases     int           `json:"human_cases"`
	Face           []FixtureFace `json:"face"`
	Rung2Turns     int           `json:"rung2_turns"`
	Status         string        `json:"status"`
	NoteSuffix     string        `json:"note_suffix"`
	// WaitsOn are the issues the card waits on at the end, when set (nil:
	// not asserted).
	WaitsOn []int64 `json:"waits_on"`
	// Signalled are the signals, starts and reopens sent to the other cards,
	// by task, when set: the plan's reopen, for one.
	Signalled map[string][]string `json:"signalled"`
}

// ResolvedOpen is the resolved_by_rung of a case no rung resolved, and
// ResolvedWithdrawn of a case its run withdrew.
const (
	ResolvedOpen      = -1
	ResolvedWithdrawn = -2
)

// FixtureFace is the substrings a human case's ask, why and recommendation
// hold.
type FixtureFace struct {
	Ask            string `json:"ask"`
	Why            string `json:"why"`
	Recommendation string `json:"recommendation"`
}

// LoadFixture reads the fixture at path and refuses a field the format
// does not have, a second document and an expect or a tool outside the
// format's values.
func LoadFixture(path string) (Fixture, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- the caller's own path argument
	if err != nil {
		return Fixture{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var f Fixture
	if err := dec.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("fixture %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Fixture{}, fmt.Errorf("fixture %s: more than one JSON document", path)
	}
	for _, a := range f.Script.Rung2 {
		if a.Answer.Tool != "answer" && a.Answer.Tool != "attach_analysis" {
			return Fixture{}, fmt.Errorf("fixture %s: rung2 tool %q is not answer or attach_analysis", path, a.Answer.Tool)
		}
	}
	if x := f.Expect; x != nil {
		for _, r := range x.ResolvedByRung {
			if r < ResolvedWithdrawn || r > 3 {
				return Fixture{}, fmt.Errorf("fixture %s: resolved_by_rung %d is not %d (withdrawn), %d (open) or a rung 0 to 3", path, r, ResolvedWithdrawn, ResolvedOpen)
			}
		}
		if x.Sessions < 0 || x.HumanCases < 0 {
			return Fixture{}, fmt.Errorf("fixture %s: expect counts cannot be negative", path)
		}
	}
	return f, nil
}

// JSON is the fixture as its file holds it: indented, in the struct's field
// order, with a closing newline.
func (f Fixture) JSON() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
