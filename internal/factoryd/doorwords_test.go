package factoryd_test

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

var digits = regexp.MustCompile(`\d+`)

// Over every fixture of the gallery that holds an ask, the factory's words
// show no machine artefact: the comment that asks on the
// card's issue and the comment each answer posts there, which a human
// reads (facefixtures.HumanFaults), and every refusal Act's table gives an
// answer on the ask's facts, whoever decides: a human's as the board shows
// it (factoryd.ForHuman), an agent's with the keys it types
// (facefixtures.TextFaults). A text that differs from one checked only in
// its numbers is checked once.
func TestTheFactorysWordsOnAnAskShowNoMachineArtefact(t *testing.T) {
	seen := map[string]bool{}
	check := func(where, text string, faultsOf func(string) []string) {
		key := digits.ReplaceAllString(text, "N")
		if text == "" || seen[key] {
			return
		}
		seen[key] = true
		if faults := faultsOf(text); len(faults) > 0 {
			t.Errorf("%s: %q\n\t%s", where, text, strings.Join(faults, " "))
		}
	}
	for _, fx := range facefixtures.All() {
		if fx.Case == nil {
			continue
		}
		c, where := *fx.Case, fx.Group+"/"+fx.Kind
		face := factoryd.FaceKeys(factoryd.FaceOf(c, factoryd.Facts{CaseFacts: fx.Facts, Card: fx.Task, Case: c}))
		reason, _ := c.Facts["reason"].(string)
		check(where+", the ask's comment", factoryd.IssueAsk(face)+"\n\n"+factoryd.TripNext(c, cmp.Or(reason, c.Problem)), facefixtures.HumanFaults)
		card := "#" + strconv.FormatInt(fx.Task.IssueNumber, 10)
		for _, a := range factoryd.Actions {
			check(where+", the comment of "+a.Name, factoryd.AnswerComment(fx.Task, "sebastian", factoryd.Resolution{Action: a.Name}), facefixtures.HumanFaults)
			for rung := range 4 {
				in := factoryd.ActInput{TaskID: fx.Task.ID, CaseID: c.ID, By: "sebastian", Via: factoryd.ViaChat, Rung: rung, Res: factoryd.Resolution{Action: a.Name}}
				refusal, faultsOf := factoryd.ActRefusal(fx.Facts, in, card), facefixtures.TextFaults
				if rung == 3 {
					if factoryd.LabelOf(a.Name) == "" {
						continue // an answer with no label has no button
					}
					in.Via = factoryd.ViaButton
					refusal, faultsOf = factoryd.ForHuman(factoryd.ActRefusal(fx.Facts, in, card)), facefixtures.HumanFaults
				}
				check(where+", the refusal of "+a.Name+" by "+in.Via+" at rung "+strconv.Itoa(rung), refusal, faultsOf)
			}
		}
	}
}
