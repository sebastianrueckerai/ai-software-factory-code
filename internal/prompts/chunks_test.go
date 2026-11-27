package prompts

import (
	"strings"
	"testing"
)

// The factory chunk orients a step in 55 words, under 60.
func TestFactoryChunk(t *testing.T) {
	if got := words(texts.chunks["factory"]); got != 55 {
		t.Errorf("the factory chunk holds %d words, want 55", got)
	}
}

// The narration chunk is 59 words.
func TestNarrationChunk(t *testing.T) {
	if got := words(texts.chunks["narration"]); got != 59 {
		t.Errorf("the narration chunk holds %d words, want 59", got)
	}
}

// A chunk is simple text: no em dash, no en dash, no semicolon.
func TestChunksAreSimpleText(t *testing.T) {
	for name, text := range texts.chunks {
		for _, mark := range []string{"—", "–", ";"} {
			if strings.Contains(text, mark) {
				t.Errorf("chunk %s holds %q", name, mark)
			}
		}
	}
}
