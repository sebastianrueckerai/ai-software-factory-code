package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// The console's check reads infra's head through the loader's source and
// answers a refusal as its lines, one per joined error.
func TestCheckProjectReadsTheHead(t *testing.T) {
	files, err := policy.ReadDir("../../internal/policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	ladder := factoryd.ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}.Aliases()
	file := files["projects/linkshort.yaml"]
	delete(files, "projects/linkshort.yaml")
	if refused, err := checkProject(context.Background(), treeSource(files), "linkshort", file, ladder); refused != nil || err != nil {
		t.Fatalf("the exit test's file: %q, %v", refused, err)
	}
	bad := strings.Replace(strings.Replace(string(file), "process: spec", "process: specs", 1), "template: ts-go", "template: ts-go2", 1)
	refused, err := checkProject(context.Background(), treeSource(files), "linkshort", []byte(bad), ladder)
	want := []string{`projects/linkshort.yaml: process "specs": no file processes/specs.yaml (processes: maintenance, spec)`,
		`projects/linkshort.yaml: template "ts-go2": no directory templates/ts-go2/ (templates: ts-go)`}
	if err != nil || !slices.Equal(refused, want) {
		t.Fatalf("refused %q, %v; want %q", refused, err, want)
	}
}
