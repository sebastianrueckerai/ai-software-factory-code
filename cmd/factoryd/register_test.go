package main

import (
	"testing"
)

// register-project takes the project's name only: its settings are its
// file's.
func TestParseRegistration(t *testing.T) {
	name, admin, rotate, err := parseRegistration([]string{"--name", "demo-app", "--rotate"})
	if err != nil || name != "demo-app" || admin != "sebastian" || !rotate {
		t.Fatalf("= %q, %q, %v, %v", name, admin, rotate, err)
	}
	for _, args := range [][]string{{}, {"--name", "x", "--merge-rule", "auto"}, {"--name", "x", "extra"}} {
		if _, _, _, err := parseRegistration(args); err == nil {
			t.Errorf("parseRegistration(%q) accepted", args)
		}
	}
}
