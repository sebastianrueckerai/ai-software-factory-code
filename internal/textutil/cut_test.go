package textutil

import "testing"

func TestTruncateAndTailCutOnACharacterBoundary(t *testing.T) {
	for _, c := range []struct {
		s          string
		n          int
		head, tail string
	}{
		{"abc", 3, "abc", "abc"},
		{"abc", 2, "ab…", "…bc"},
		// é is two bytes: a cut inside it drops it whole
		{"aé", 2, "a…", "…é"},
		{"éa", 2, "é…", "…a"},
		{"éa", 1, "…", "…a"},
		{"aé", 1, "a…", "…"},
		{"aé", 3, "aé", "aé"},
	} {
		if got := Truncate(c.s, c.n); got != c.head {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.s, c.n, got, c.head)
		}
		if got := Tail(c.s, c.n); got != c.tail {
			t.Errorf("Tail(%q, %d) = %q, want %q", c.s, c.n, got, c.tail)
		}
	}
}
