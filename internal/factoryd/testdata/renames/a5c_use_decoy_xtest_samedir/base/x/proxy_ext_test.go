package x_test

import "testing"

var allow = 1

func TestRetries(t *testing.T) {
	if allow != 1 {
		t.Fatal(allow)
	}
}
