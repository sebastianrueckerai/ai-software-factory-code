package factoryd

// AsWritten is file_card's box a human ticks to file a brief the filing
// door refused as it stands, marked unchecked, as the New task form's File
// as written does.
const AsWritten = "as_written"

// AskOffer is one answer an ask row stores under offers: the answer's key,
// its label and its effect line. A human card's choices carry no effect.
type AskOffer struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
}

// AnswersByOffers reports whether the face of case c on a card of kind cardKind stores table
// offers: not a human card's, whose question's choices answer it, and not a
// merge's or a plan's criteria, whose approval card is their face, nor a
// monitor's flag.
func AnswersByOffers(c Case, cardKind string) bool {
	k := c.Kind
	return !ChoiceCase(c, cardKind) && k != CaseMergeApproval && k != CasePlanCriteria && k != CaseMonitorFlag &&
		k != CasePlanQuestion && k != CasePlanConfirmation
}

// FaceKeys are the keys an ask row stores of face: the ask, the why line,
// the story, the answers under offers and the suggested one preselected
// with who suggests it and why.
func FaceKeys(face AskFace) map[string]any {
	keys := map[string]any{"ask": face.Ask, "why": face.Why, "story": face.Story, "offers": offersOfAnswers(face.Answers)}
	if face.Suggested != "" {
		keys["preselected"] = map[string]any{"action": face.Suggested}
		keys["recommended_by"] = face.SuggestedBy
		if face.Grounds != "" {
			keys["recommendation"] = face.Grounds
		}
	}
	return keys
}

// offersOfAnswers are answers as an ask row stores them: the key, the
// label and the effect line of each.
func offersOfAnswers(answers []Answer) []AskOffer {
	out := []AskOffer{}
	for _, a := range answers {
		out = append(out, AskOffer{Value: a.Key(), Label: a.Label, Effect: a.Effect})
	}
	return out
}
