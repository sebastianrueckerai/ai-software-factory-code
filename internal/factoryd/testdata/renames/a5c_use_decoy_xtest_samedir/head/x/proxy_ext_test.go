package x_test

import "testing"

var Allow = 1

func TestRetries(t *testing.T) {
	if Allow != 1 {
		t.Fatal(Allow)
	}
}
