package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Ruling 179: no Go source names the factory's own repositories; they are
// projects like any other, found through their files. The module path is
// the Go import path, no repository reference. policytest holds the
// check's fixtures, which only tests import.
func TestNoSourceNamesTheFactorysRepositories(t *testing.T) {
	named := regexp.MustCompile(`ai-software-factory-(code|infra)`)
	module := "github.com/sebastianrueckerai/ai-software-factory-code"
	var hits []string
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.Contains(path, "/policytest/") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if named.MatchString(strings.ReplaceAll(line, module, "")) {
				hits = append(hits, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("sources naming a factory repository:\n%s", strings.Join(hits, "\n"))
	}
}
