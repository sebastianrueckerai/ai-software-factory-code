package console

// The door's three-way merge of a stale save (roadmap 277, the
// 2026-10-15 plan, item 5): the member's whole-text save over the text
// at the commit it names, against master's current text, git merge-file
// over the three in a private temp directory. A clean merge is the
// member's save, committed through the same door; a clash refuses with
// the clashing lines' ranges in master's text, which the editor marks
// when it lays that text beside the member's own.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// docMerge merges mine over base against theirs — base the text at the
// commit the save names, mine the member's text, theirs master's current
// one. A clean merge answers the merged text and no clashes; a clash
// answers the clashing lines' 1-based [first, last] ranges in theirs, in
// order; a git that does not run answers an error.
func docMerge(ctx context.Context, mine, base, theirs string) (string, [][2]int, error) {
	dir, err := os.MkdirTemp("", "doc-merge-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir) // #nosec G104 -- best-effort cleanup of a private temp dir
	for name, text := range map[string]string{"mine.txt": mine, "base.txt": base, "theirs.txt": theirs} {
		if err := os.WriteFile(dir+"/"+name, []byte(text), 0o600); err != nil {
			return "", nil, err
		}
	}
	out, err := exec.CommandContext(ctx, "git", "merge-file", "-p", "-L", "mine", "-L", "base", "-L", "theirs",
		dir+"/mine.txt", dir+"/base.txt", dir+"/theirs.txt").Output() // #nosec G204 -- no shell, fixed argv: git merge-file over files of a private temp dir
	var broken *exec.ExitError
	if err != nil && (!errors.As(err, &broken) || broken.ExitCode() < 0) {
		return "", nil, fmt.Errorf("git merge-file: %w", err)
	}
	// a positive exit is the number of clashes; the output holds them
	merged := string(out)
	if !strings.Contains(merged, "<<<<<<< mine") {
		return merged, nil, nil
	}
	return "", docClashes(theirs, docTheirsBlocks(merged)), nil
}

// docTheirsBlocks walks git merge-file's conflicted output and answers
// each clash's theirs-side lines, in order: the block between the =======
// and >>>>>>> markers of every conflict.
func docTheirsBlocks(merged string) [][]string {
	var blocks [][]string
	block := []string(nil)
	for l := range strings.Lines(merged) {
		switch {
		case strings.HasPrefix(l, "<<<<<<<"), strings.HasPrefix(l, "======="):
			block = []string{}
		case strings.HasPrefix(l, ">>>>>>>"):
			if block != nil {
				blocks = append(blocks, block)
				block = nil
			}
		case block != nil:
			block = append(block, strings.TrimSuffix(l, "\n"))
		}
	}
	return blocks
}

// docClashes answers block's lines' 1-based [first, last] ranges in
// theirs: each block's lines stand in theirs verbatim and in order, so a
// cursor walks theirs forward to the block's first line and checks the
// whole block matches there; a block that does not match anywhere marks
// nothing (an empty theirs side has no line to mark).
func docClashes(theirs string, blocks [][]string) [][2]int {
	lines := strings.Split(strings.ReplaceAll(theirs, "\r\n", "\n"), "\n")
	var ranges [][2]int
	at := 0
	for _, block := range blocks {
		if len(block) == 0 {
			continue
		}
		for first := at; first+len(block) <= len(lines); first++ {
			if !docBlockAt(lines, block, first) {
				continue
			}
			ranges = append(ranges, [2]int{first + 1, first + len(block)})
			at = first + len(block)
			break
		}
	}
	return ranges
}

// docBlockAt tells whether block's lines stand in lines at first.
func docBlockAt(lines []string, block []string, first int) bool {
	for i, l := range block {
		if lines[first+i] != l {
			return false
		}
	}
	return true
}
