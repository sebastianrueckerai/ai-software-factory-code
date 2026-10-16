package main

import "testing"

// remove-project takes one name and the dry-run flag (roadmap 272).
func TestParseRemoval(t *testing.T) {
	name, dry, err := parseRemoval([]string{"demo-app"})
	if err != nil || name != "demo-app" || dry {
		t.Fatalf("= %q, %v, %v", name, dry, err)
	}
	name, dry, err = parseRemoval([]string{"--dry-run", "demo-app"})
	if err != nil || name != "demo-app" || !dry {
		t.Fatalf("dry = %q, %v, %v", name, dry, err)
	}
	for _, args := range [][]string{{}, {"--dry-run"}, {"a", "b"}, {"--dry-run", "a", "b"}, {"--dry-run=false", "a", "--extra"}} {
		if _, _, err := parseRemoval(args); err == nil {
			t.Errorf("parseRemoval(%q) accepted", args)
		}
	}
}
