package policy

import "testing"

// Covers on hand-built documents: the generated schema holds everything
// the copy holds, or it does not. A key path or an array member the copy
// has beyond the schema makes false; a value at a shared path, however
// different, keeps true, so a build may still reword or renumber its own
// schema.
func TestCovers(t *testing.T) {
	for _, tt := range []struct {
		name string
		copy string
		want string
		cov  bool
	}{
		{"a key path beyond the schema", `{"a":{"x":1,"new":1}}`, `{"a":{"x":1}}`, false},
		{"an array member beyond the schema", `{"e":["auto","new"]}`, `{"e":["auto"]}`, false},
		{"an object array member beyond the schema", `{"l":[{"a":1},{"new":2}]}`, `{"l":[{"a":1}]}`, false},
		{"a value at a shared path", `{"a":1}`, `{"a":2}`, true},
		{"nothing in the copy", `{}`, `{"a":1}`, true},
		{"the schema beyond the copy", `{"a":{"x":1}}`, `{"a":{"x":1,"y":2}}`, true},
		{"a copy that is not json", `not json`, `{"a":1}`, true},
	} {
		if got := Covers([]byte(tt.copy), []byte(tt.want)); got != tt.cov {
			t.Errorf("%s: Covers = %v, want %v", tt.name, got, tt.cov)
		}
	}
}
