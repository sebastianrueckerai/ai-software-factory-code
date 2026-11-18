package factoryd

import "testing"

// A case names its request by the request's id however its facts decode
// the number: a resource request's case as its merge and criteria
// siblings do, so an id of a million matches as it reads.
func TestACaseNamesItsRequestByItsID(t *testing.T) {
	q := ApprovalRequest{ID: 1000000, Kind: ApprovalResource}
	for _, id := range []any{int64(1000000), float64(1000000)} {
		if !RequestAnswers(q, Case{Kind: CaseKind(ApprovalResource), Facts: map[string]any{"approval": id}}) {
			t.Errorf("a resource case of approval %v does not name request 1000000", id)
		}
	}
	if RequestAnswers(q, Case{Kind: CaseKind(ApprovalResource), Facts: map[string]any{"approval": float64(7)}}) {
		t.Error("a resource case of approval 7 names request 1000000")
	}
}
