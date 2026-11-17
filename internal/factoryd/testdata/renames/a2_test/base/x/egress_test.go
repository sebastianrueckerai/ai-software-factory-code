package x

import "testing"

func TestEgressRefusesPrivate(t *testing.T) {
	if allow("10.0.0.1") {
		t.Fatal("private host allowed")
	}
}
