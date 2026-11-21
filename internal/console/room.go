package console

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// roomView is a room's control on the opened card (C48): the count's
// buttons, one session's worth beside them (Each, in the element EachID),
// and the readout of the count and the tick, the table factoryd's
// ReadoutOf fills, or Why, the refusal a press would meet. Tick is the
// money's box, which the readout holds, and Guard the raises the readout
// shows under the names the press posts them by. Get is where the form
// asks for the readout again, Params the fields it asks with, ID the
// element it lands in; Swap sets Each again beside the count from there.
type roomView struct {
	ID, Get, Params string
	Count           fieldView
	Each, EachID    string
	Swap            bool
	Why             string
	Readout         factoryd.Readout
	Tick            tickView
	Guard           []hidden
}

// tickView is the room's money box: its id, the name it posts under, its
// label and whether it is ticked.
type tickView struct {
	ID, Name, Label string
	Checked         bool
}

// readoutOf is the readout of the room the fields get names ask for on
// facts f, read as the press reads them, under the form whose ids start
// with at, its fields named by name. A count no press may give, or a room
// the card cannot take as asked, shows why alone, as the press would be
// refused.
func readoutOf(f factoryd.CaseFacts, get func(field string) string, at string, name func(string) string) roomView {
	answer, _ := factoryd.AnswerOf(factoryd.ActExtend)
	v := roomView{ID: at + "-room", Each: factoryd.EachLine(f), EachID: at + "-each",
		Tick: tickView{ID: at + "-money", Name: name("money"), Checked: strings.TrimSpace(get("money")) == "yes"}}
	for _, field := range answer.Fields {
		if field.Name == "money" {
			v.Tick.Label = field.Label
		}
	}
	args, _, err := factoryd.ArgsOf(answer, get)
	if err != nil {
		v.Why = err.Error()
	} else {
		extend, _ := factoryd.ActionByName(factoryd.ActExtend)
		v.Why = extend.Applies(f, factoryd.Resolution{Action: factoryd.ActExtend, Args: args})
	}
	if v.Why != "" {
		v.Why = strings.ToUpper(v.Why[:1]) + v.Why[1:] + "."
		return v
	}
	n, _ := args["sessions"].(int)
	money, _ := args["money"].(bool)
	room := factoryd.RoomFor(f, n, money)
	v.Readout = factoryd.ReadoutOf(f, room)
	shown := room.Shown()
	for _, k := range factoryd.RoomGuard {
		v.Guard = append(v.Guard, hidden{name(k), shown[k]})
	}
	return v
}

// countOf is the room's count field fv as its buttons, one per count from
// its least to its most, the one at its value chosen.
func countOf(fv fieldView) fieldView {
	for n := fv.Min; n <= fv.Max; n++ {
		s := strconv.Itoa(n)
		fv.Options = append(fv.Options, optionView{Value: s, Label: s, Checked: s == fv.Value})
	}
	return fv
}

// roomField is a field of the room's press form by its name there.
func roomField(field string) string { return factoryd.ActExtend + "." + field }

// roomReadout answers the readout of card id's room at the count and the
// tick its query names by the press form's own fields, read as the press
// reads them (ArgsOf), on the facts Act weighs the press on, for the form
// whose ids start with at; it sets one session's worth beside the count
// again.
func (c *Console) roomReadout(w http.ResponseWriter, r *http.Request) {
	t, err := c.cardIn(r.Context(), button(r), r.PathValue("id"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	f, err := c.cfg.RoomFacts(r.Context(), t)
	if err != nil {
		log.Printf("console: the room facts of %s: %v", t.ID, err)
		c.fail(w, r, refuse(http.StatusServiceUnavailable, "The card's room could not be read. Try again."))
		return
	}
	q := r.URL.Query()
	v := readoutOf(f, func(field string) string { return q.Get(roomField(field)) }, fieldID.ReplaceAllString(q.Get("at"), "-"), roomField)
	v.Swap = true
	c.render(w, "room", v)
}

// roomFactsOf are card t's room facts (Config.RoomFacts), read once in
// v's render; a read that fails, which it does only once the request
// ended, is logged and leaves what it read.
func (c *Console) roomFactsOf(ctx context.Context, v *boardView, t factoryd.Task) factoryd.CaseFacts {
	if f, ok := v.rooms[t.ID]; ok {
		return f
	}
	f, err := c.cfg.RoomFacts(ctx, t)
	if err != nil {
		log.Printf("console: the room facts of %s: %v", t.ID, err)
	}
	if v.rooms == nil {
		v.rooms = map[string]factoryd.CaseFacts{}
	}
	v.rooms[t.ID] = f
	return f
}

// fundView is the money row of "Add money to the chat" (C48): the chat's
// money left, the EUR field in the adds column, what the chat then has,
// read again from Get with the field named in Params into the element ID,
// and the project's month, or Note where the chat could not be read.
type fundView struct {
	ID, Get, Params string
	Field           fieldView
	Row             factoryd.RoomRow
	Month, Note     string
}

// fundOf is the money row of chat budget card t's raise of amount, read
// as the press reads it, an empty amount at its default.
func (c *Console) fundOf(ctx context.Context, t factoryd.Task, amount string) fundView {
	var v fundView
	chat, ok, err := c.cfg.Tasks.Task(ctx, t.Parent)
	if err != nil || !ok {
		log.Printf("console: the chat task %s of %s: found %v, %v", t.Parent, t.ID, ok, err)
		v.Note = "The chat's budget could not be read."
		return v
	}
	f, err := c.cfg.RoomFacts(ctx, chat)
	if err != nil {
		log.Printf("console: the facts of the chat task %s: %v", chat.ID, err)
	}
	eur := 0.0
	answer, _ := factoryd.AnswerOf(factoryd.AnswerFundChat)
	if res, err := answer.Press(factoryd.Facts{Card: t}, map[string]string{"eur": amount}, ""); err == nil {
		eur, _ = res.Args["eur"].(float64)
	}
	v.Row, v.Month = factoryd.MoneyRow(f.LeftEUR, eur), factoryd.MonthLine(f)
	return v
}

// fundReadout answers what the chat of chat budget card id then has at the
// amount its query names under the field the query's field names.
func (c *Console) fundReadout(w http.ResponseWriter, r *http.Request) {
	t, err := c.cardIn(r.Context(), button(r), r.PathValue("id"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	if t.Source != factoryd.SourceChatBudget {
		c.fail(w, r, refuse(http.StatusNotFound, "This card asks for no money for the chat."))
		return
	}
	q := r.URL.Query()
	c.render(w, "fund-then", c.fundOf(r.Context(), t, q.Get(q.Get("field"))))
}
