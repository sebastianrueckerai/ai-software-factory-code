package x

import "testing"

func testEgressRefusesPrivate(t *testing.T) {
	if allow("10.0.0.1") {
		t.Fatal("private host allowed")
	}
}
