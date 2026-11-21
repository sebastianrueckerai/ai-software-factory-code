package console

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// roomView is the readout of a room on the opened card: the lines
// factoryd says of its count and its tick (RoomLines), and the guard its
// press posts, the raises the lines show under the names the press posts
// them by. Get is where its form asks for it again, Params the fields it
// asks with, and ID the element it lands in.
type roomView struct {
	ID, Get, Params string
	Lines           []string
	Guard           []hidden
}

// readoutOf is the readout of the room args ask for on facts f, its guard
// named by name; a count no press may give (err), or a room the card
// cannot take as asked, shows why alone, as the press would be refused.
func readoutOf(f factoryd.CaseFacts, args map[string]any, err error, name func(string) string) roomView {
	extend, _ := factoryd.ActionByName(factoryd.ActExtend)
	why := ""
	if err != nil {
		why = err.Error()
	} else {
		why = extend.Applies(f, factoryd.Resolution{Action: factoryd.ActExtend, Args: args})
	}
	if why != "" {
		return roomView{Lines: []string{strings.ToUpper(why[:1]) + why[1:] + "."}}
	}
	n, _ := args["sessions"].(int)
	money, _ := args["money"].(bool)
	room := factoryd.RoomFor(f, n, money)
	v := roomView{Lines: factoryd.RoomLines(f, room)}
	shown := room.Shown()
	for _, k := range factoryd.RoomGuard {
		v.Guard = append(v.Guard, hidden{name(k), shown[k]})
	}
	return v
}

// roomField is a field of the room's press form by its name there.
func roomField(field string) string { return factoryd.ActExtend + "." + field }

// roomReadout answers the readout of card id's room at the count and the
// tick its query names by the press form's own fields, read as the press
// reads them (ArgsOf), on the facts Act weighs the press on.
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
	answer, _ := factoryd.AnswerOf(factoryd.ActExtend)
	q := r.URL.Query()
	args, _, err := factoryd.ArgsOf(answer, func(field string) string { return q.Get(roomField(field)) })
	c.render(w, "room", readoutOf(f, args, err, roomField))
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
