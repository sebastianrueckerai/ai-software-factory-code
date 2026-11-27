package writingguide

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Each form is its file under forms/, trimmed; a name with no file is no
// form.
func TestForm(t *testing.T) {
	for _, name := range FormNames {
		b, err := os.ReadFile("forms/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := Form(name); !ok || got != strings.TrimSpace(string(b)) {
			t.Errorf("Form(%s) = %q, %v; want the file", name, got, ok)
		}
	}
	if _, ok := Form("poem"); ok {
		t.Error("Form(poem) exists")
	}
}

// The chat form carries the guide's two bullets on references, which the
// guide itself no longer holds.
func TestChatFormCarriesTheReferenceBullets(t *testing.T) {
	chat, _ := Form("chat")
	for _, want := range []string{"Explain each reference at its first use and link it.", "Never name a thing that no reader can open"} {
		if !strings.Contains(chat, want) {
			t.Errorf("the chat form lacks %q", want)
		}
	}
}

// The guide is the owner's skill body with nothing of the code copy's own.
func TestGuideHasNoAdditions(t *testing.T) {
	for _, not := range []string{"writingguide.For", "A human is a human", "Readers, references and titles", "the owner's OK"} {
		if strings.Contains(Guide, not) {
			t.Errorf("the guide holds %q", not)
		}
	}
	if !strings.HasPrefix(Guide, "**Changes to this guide need the user's OK.**") {
		t.Errorf("the guide opens with %.60q", Guide)
	}
}

// A title's writer gets the shortest form, and no form sets a bound in
// characters: the door that refuses a text too long names the bound.
func TestFormsStayShort(t *testing.T) {
	if title, _ := Form("title"); len(title) >= 600 {
		t.Errorf("the title form is %d bytes, want under 600", len(title))
	}
	count := regexp.MustCompile(`(?i)\b(?:at most|under|up to|no more than|within)\s+\d[\d,]*\s+(?:characters|runes|bytes|words)\b`)
	for _, name := range FormNames {
		form, _ := Form(name)
		if c := count.FindString(form); c != "" {
			t.Errorf("the %s form sets a bound, %q", name, c)
		}
	}
}
