package factoryd

import (
	"encoding/json"
	"slices"
	"testing"
)

// storedOffers and offerValues moved to the pg package with
// offers_pg_test.go; these copies stay for the round tests that still read
// a case's stored offers here.

func storedOffers(t *testing.T, ask map[string]any) []AskOffer {
	t.Helper()
	b, err := json.Marshal(ask["offers"])
	if err != nil {
		t.Fatal(err)
	}
	var out []AskOffer
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("stored offers %s: %v", b, err)
	}
	return slices.DeleteFunc(out, func(o AskOffer) bool { return o.Effect == "" })
}

func offerValues(offers []AskOffer) []string {
	var out []string
	for _, o := range offers {
		out = append(out, o.Value)
	}
	return out
}
