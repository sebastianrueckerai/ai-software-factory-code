package policy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Draw is an amount of the escalation pool: sessions, agent time and money.
type Draw struct {
	// Sessions is a count of agent sessions.
	Sessions int `yaml:"sessions,omitempty" json:"sessions,omitempty"`
	// AgentTime is agent working time, such as 30h.
	AgentTime time.Duration `yaml:"agent_time,omitempty" json:"agent_time,omitempty"`
	// EUR is money in EUR.
	EUR float64 `yaml:"eur,omitempty" json:"eur,omitempty"`
}

// Escalation is the pool every extension and grant of a card's escalation
// draws from, the most the code and the check-in may draw for one card, and
// when the AI rungs run.
type Escalation struct {
	// Pool is what all rungs together may extend and grant in a project
	// per UTC month. Absent or 0: the part takes policy.yaml's figure, and
	// in policy.yaml 30 sessions, 30h, 60 EUR.
	Pool Draw `yaml:"pool,omitempty" json:"pool"`
	// CardDraw is the most the code and the check-in may draw from the
	// pool for one medium card over its whole life: a small card half of
	// it, a large one one and a half times, sessions rounded up. At most
	// the pool in each part. Absent or 0: the part takes policy.yaml's
	// figure, and in policy.yaml 2 sessions, 1h, 4 EUR.
	CardDraw Draw `yaml:"card_draw,omitempty" json:"card_draw"`
	// Judgment is when the check-in and the factory agent judge a case the
	// code did not resolve. With always they judge it at every autonomy
	// level. With autonomous_and_above they judge it at autonomous and full
	// auto only, and at manual and assisted it goes straight to a human.
	// Absent: always.
	Judgment string `yaml:"judgment,omitempty" json:"judgment,omitempty"`
}

// The values of Escalation.Judgment.
const (
	JudgmentAlways             = "always"
	JudgmentAutonomousAndAbove = "autonomous_and_above"
)

// JudgmentValues are the judgment values a file may name.
var JudgmentValues = []string{JudgmentAlways, JudgmentAutonomousAndAbove}

// DefaultEscalation fills what the files leave out, part by part.
var DefaultEscalation = Escalation{
	Pool:     Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60},
	CardDraw: Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
	Judgment: JudgmentAlways,
}

// IsZero is true for a draw of nothing.
func (d Draw) IsZero() bool { return d == Draw{} }

// Fits is true when every part of d is at most in's.
func (d Draw) Fits(in Draw) bool {
	return d.Sessions <= in.Sessions && d.AgentTime <= in.AgentTime && d.EUR <= in.EUR
}

// Minus is d less o, each part at least zero.
func (d Draw) Minus(o Draw) Draw {
	return Draw{max(d.Sessions-o.Sessions, 0), max(d.AgentTime-o.AgentTime, 0), max(d.EUR-o.EUR, 0)}
}

// Plus is d and o added.
func (d Draw) Plus(o Draw) Draw {
	return Draw{d.Sessions + o.Sessions, d.AgentTime + o.AgentTime, d.EUR + o.EUR}
}

// over is d with each part it leaves at zero taken from base.
func (d Draw) over(base Draw) Draw {
	return Draw{cmp.Or(d.Sessions, base.Sessions), cmp.Or(d.AgentTime, base.AgentTime), cmp.Or(d.EUR, base.EUR)}
}

// drawPart is one part of a draw by its key: a number to compare and the
// value to show.
type drawPart struct {
	key  string
	n    float64
	show any
}

func (d Draw) parts() [3]drawPart {
	return [3]drawPart{{"sessions", float64(d.Sessions), d.Sessions}, {"agent_time", float64(d.AgentTime), d.AgentTime}, {"eur", d.EUR, d.EUR}}
}

// drawJSON is Draw with its agent time as a string, as in the YAML.
type drawJSON struct {
	Sessions  int     `json:"sessions,omitempty"`
	AgentTime string  `json:"agent_time,omitempty"`
	EUR       float64 `json:"eur,omitempty"`
}

// MarshalJSON is Draw as drawJSON, its agent time a string, as Limits'.
func (d Draw) MarshalJSON() ([]byte, error) {
	j := drawJSON{Sessions: d.Sessions, EUR: d.EUR}
	if d.AgentTime != 0 {
		j.AgentTime = d.AgentTime.String()
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads drawJSON back into Draw. A bad duration fails the
// whole read.
func (d *Draw) UnmarshalJSON(b []byte) error {
	var j drawJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	out := Draw{Sessions: j.Sessions, EUR: j.EUR}
	if j.AgentTime != "" {
		v, err := time.ParseDuration(j.AgentTime)
		if err != nil {
			return fmt.Errorf("draw: %w", err)
		}
		out.AgentTime = v
	}
	*d = out
	return nil
}

// EscalationOf is repository's escalation block: DefaultEscalation,
// policy.yaml's keys over it, then those of the project file that names
// repository.
func (t Tree) EscalationOf(repository string) Escalation {
	_, p, _ := t.ProjectOf(repository)
	return t.Document.escalationOf(p)
}

func (d Document) escalationOf(p ProjectConfig) Escalation {
	e := DefaultEscalation
	for _, b := range []*Escalation{&d.Escalation, p.Escalation} {
		if b == nil {
			continue
		}
		e.Pool = b.Pool.over(e.Pool)
		e.CardDraw = b.CardDraw.over(e.CardDraw)
		e.Judgment = cmp.Or(b.Judgment, e.Judgment)
	}
	return e
}

// CardDrawOf is the card draw of a card of size: half of CardDraw for a
// small card, one and a half times for a large one, sessions rounded up,
// CardDraw for any other size.
func (e Escalation) CardDrawOf(size string) Draw {
	d := e.CardDraw
	switch size {
	case "small":
		d.Sessions, d.EUR = (d.Sessions+1)/2, d.EUR/2
	case "large":
		d.Sessions, d.EUR = d.Sessions+(d.Sessions+1)/2, d.EUR*1.5
	}
	d.AgentTime = SizeAgentTime(size, d.AgentTime)
	return d
}

// keyedDraw is one of a block's two draws by its key.
type keyedDraw struct {
	key string
	d   Draw
}

// draws are the block's pool and card draw by their keys.
func (e Escalation) draws() [2]keyedDraw {
	return [2]keyedDraw{{"pool", e.Pool}, {"card_draw", e.CardDraw}}
}

// validate is the block's own judgement, as a file sets it: no part
// negative, judgment one of JudgmentValues.
func (e Escalation) validate() error {
	for _, draw := range e.draws() {
		for _, p := range draw.d.parts() {
			if p.n < 0 {
				return fmt.Errorf("escalation.%s.%s must not be negative, is %v", draw.key, p.key, p.show)
			}
		}
	}
	if e.Judgment != "" && !slices.Contains(JudgmentValues, e.Judgment) {
		return fmt.Errorf("escalation.judgment %q: want one of %v", e.Judgment, JudgmentValues)
	}
	return nil
}

// checkDraw refuses a resolved block whose card draw has a part above the
// pool's: no card could draw it.
func (e Escalation) checkDraw() error {
	pool := e.Pool.parts()
	for i, p := range e.CardDraw.parts() {
		if p.n > pool[i].n {
			return fmt.Errorf("escalation.card_draw.%s %v is above escalation.pool.%s %v, so no card could draw it: lower the card draw or raise the pool", p.key, p.show, p.key, pool[i].show)
		}
	}
	return nil
}
