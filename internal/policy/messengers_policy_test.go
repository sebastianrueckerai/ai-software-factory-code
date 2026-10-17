package policy

import (
	"testing"
)

// A project file may forbid messenger delivery (roadmap 145): messengers
// false parses, absent leaves the default on.
func TestProjectMessengers(t *testing.T) {
	files := fixtureTree(t)
	files["projects/linkshort.yaml"] = append(files["projects/linkshort.yaml"], []byte("messengers: false\n")...)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	if q := tree.Projects["linkshort"]; q.Messengers == nil || *q.Messengers || q.MessengersOn() {
		t.Fatalf("messengers false = %+v", q.Messengers)
	}
	if q := tree.Projects["demo-app"]; q.Messengers != nil || !q.MessengersOn() {
		t.Fatalf("without the key = %+v", q.Messengers)
	}
}
