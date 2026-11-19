package factoryd

// PressRefusal is why the action table refuses the human's press res on
// facts f, "" when it takes it.
func PressRefusal(f Facts, res Resolution) string {
	a, ok := ActionByName(res.Action)
	if !ok {
		return "no action " + res.Action
	}
	return a.refusal(f.CaseFacts, 3, res)
}

// EndedRefusal is why Act refuses action on case c of card t because the
// card ended, "" where it takes it.
func EndedRefusal(t Task, c Case, action string) string {
	return endedRefusal(t.Status, PlanWaitCase(c, t), action)
}

// OfferRefusal is why action name is not on offer to the human on f.
func OfferRefusal(f Facts, name string) string {
	a, ok := ActionByName(name)
	if !ok {
		return "no action " + name
	}
	return a.offerRefusal(f.CaseFacts, 3)
}

// SpecAnswers are the answers the face spec of case c names on facts f,
// before any leaves the face.
func SpecAnswers(c Case, f Facts) []Answer {
	c, f = facing(c, f)
	var out []Answer
	for _, key := range faceSpecOf(c, f).Answers(f) {
		if a, ok := AnswerFor(key, f); ok {
			out = append(out, a)
		}
	}
	return out
}

// TripNext is the last line of a trip's face on the issue for case c at
// reason.
func TripNext(c Case, reason string) string { return tripNext(c, reason) }

var PokeKinds = pokeKinds

// ActRefusal is why Act refuses in on facts f of card, the words every
// door shows, "" when it takes it.
func ActRefusal(f CaseFacts, in ActInput, card string) string { return actRefusal(f, in, card) }

// AnswerComment is the comment the factory posts on t's issue for by's
// answer res without a note.
func AnswerComment(t Task, by string, res Resolution) string {
	return (&Runner{}).answerComment(t, by, res, "", "")
}
