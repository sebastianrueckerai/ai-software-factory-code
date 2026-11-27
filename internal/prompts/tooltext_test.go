package prompts

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// toolTextFaults are the texts under tools/ of fsys over 90 words.
func toolTextFaults(fsys fs.FS) []string {
	files, err := fs.Glob(fsys, "tools/*.md")
	if err != nil {
		return []string{err.Error()}
	}
	var faults []string
	for _, file := range files {
		b, err := fs.ReadFile(fsys, file)
		if err != nil {
			faults = append(faults, err.Error())
			continue
		}
		if n := len(strings.Fields(string(b))); n > 90 {
			faults = append(faults, fmt.Sprintf("%s holds %d words, want at most 90", file, n))
		}
	}
	return faults
}

// toolTexts are the built-in tools' texts by tool name.
func toolTexts(t *testing.T) map[string]string {
	t.Helper()
	files, err := fs.Glob(os.DirFS("."), "tools/*.md")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(path.Base(file), ".md")] = string(b)
	}
	if len(out) != 7 {
		t.Fatalf("tools/ holds %d texts, want the 7 of bash, edit, glob, grep, read, todowrite and write", len(out))
	}
	return out
}

func TestToolTextsAreCapped(t *testing.T) {
	for _, fault := range toolTextFaults(os.DirFS(".")) {
		t.Error(fault)
	}
	faults := toolTextFaults(fstest.MapFS{
		"tools/long.md": {Data: []byte(strings.Repeat("word ", 91))},
		"tools/fine.md": {Data: []byte(strings.Repeat("word ", 90))},
	})
	if want := []string{"tools/long.md holds 91 words, want at most 90"}; !slices.Equal(faults, want) {
		t.Errorf("the cap over a 91-word and a 90-word text = %q, want %q", faults, want)
	}
}

// A tool text names no tool a step lacks and no rule of an interactive
// session: no Task, webfetch or rg, no commit and no user. grep and glob
// are offered without the shell, so they do not name it.
func TestToolTextsNameNoMissingTool(t *testing.T) {
	banned := regexp.MustCompile(`\b(Task|webfetch|commit|rg)\b|(?i)\bthe user\b`)
	shell := regexp.MustCompile(`\bbash\b`)
	for name, text := range toolTexts(t) {
		if m := banned.FindString(text); m != "" {
			t.Errorf("tools/%s.md names %q", name, m)
		}
		if (name == "grep" || name == "glob") && shell.MatchString(text) {
			t.Errorf("tools/%s.md names bash, which screening and sizing lack", name)
		}
	}
}

// Each text keeps what the model's calls depend on.
func TestToolTextsKeepTheirSemantics(t *testing.T) {
	texts := toolTexts(t)
	for name, words := range map[string][]string{
		"edit":      {"oldString", "replaceAll", "more than once"},
		"bash":      {"workdir", "timeout", "120000"},
		"read":      {"offset", "limit"},
		"todowrite": {"in_progress"},
	} {
		for _, w := range words {
			if !strings.Contains(texts[name], w) {
				t.Errorf("tools/%s.md lacks %q", name, w)
			}
		}
	}
}

// Every text is a built-in tool's: a BuiltinTools entry, or write, which
// edit's permission switches with it.
func TestToolTextsHaveATool(t *testing.T) {
	for name := range toolTexts(t) {
		if !slices.Contains(BuiltinTools, name) && name != "write" {
			t.Errorf("tools/%s.md names no built-in tool of %v or write", name, BuiltinTools)
		}
	}
}
