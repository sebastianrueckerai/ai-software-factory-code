package prompts

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// sumsFile pins every text a prompt joins; its paths are from internal/.
const sumsFile = "texts.sha256"

// pinned are the text files the sums file covers, from internal/.
var pinned = []string{"writingguide/writing-guide.md", "writingguide/forms/*.md", "prompts/chunks/*.md", "prompts/steps/*.md"}

// A text changes only with its new sum in the same commit, so its review
// sees the change.
func TestTextsArePinned(t *testing.T) {
	want := map[string]string{}
	for _, pattern := range pinned {
		files, err := filepath.Glob(filepath.Join("..", pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			want[filepath.ToSlash(strings.TrimPrefix(file, "../"))] = hex.EncodeToString(sum[:])
		}
	}
	f, err := os.Open(sumsFile)
	if err != nil {
		t.Fatalf("%v: write one sha256sum line, <sum>  <path from internal/>, for each file of %s, sorted by path", err, strings.Join(pinned, ", "))
	}
	defer f.Close()
	got := map[string]string{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		sum, file, ok := strings.Cut(lines.Text(), "  ")
		if !ok {
			t.Errorf("internal/prompts/%s: line %q is not <sum>  <path>", sumsFile, lines.Text())
			continue
		}
		got[file] = sum
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	ask := func(sum, file string) string {
		return fmt.Sprintf("write %s  %s in internal/prompts/%s, and ask Sebastian", sum, file, sumsFile)
	}
	for _, file := range slices.Sorted(maps.Keys(want)) {
		switch sum, ok := got[file]; {
		case !ok:
			t.Errorf("internal/%s has no sum: %s", file, ask(want[file], file))
		case sum != want[file]:
			t.Errorf("internal/%s changed: %s", file, ask(want[file], file))
		}
	}
	for _, file := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[file]; !ok {
			t.Errorf("internal/prompts/%s names %s, which is no text file: delete its line, and ask Sebastian", sumsFile, file)
		}
	}
}

// paragraphFaults are the step paragraphs of fsys over 250 words or with a
// blank line inside.
func paragraphFaults(fsys fs.FS) []string {
	files, err := fs.Glob(fsys, "steps/*.md")
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
		text := strings.TrimSpace(string(b))
		if n := len(strings.Fields(text)); n > 250 {
			faults = append(faults, fmt.Sprintf("%s holds %d words, want at most 250", file, n))
		}
		if strings.Contains(text, "\n\n") {
			faults = append(faults, fmt.Sprintf("%s holds more than one paragraph", file))
		}
	}
	return faults
}

func TestParagraphIsOneShortParagraph(t *testing.T) {
	for _, fault := range paragraphFaults(os.DirFS(".")) {
		t.Error(fault)
	}
	faults := paragraphFaults(fstest.MapFS{
		"steps/long.md": {Data: []byte(strings.Repeat("word ", 251))},
		"steps/two.md":  {Data: []byte("One paragraph.\n\nTwo paragraphs.\n")},
		"steps/fine.md": {Data: []byte(strings.Repeat("word ", 250) + "\n")},
	})
	want := []string{"steps/long.md holds 251 words, want at most 250", "steps/two.md holds more than one paragraph"}
	if !slices.Equal(faults, want) {
		t.Errorf("the rule over two faulty files = %q, want %q", faults, want)
	}
}
