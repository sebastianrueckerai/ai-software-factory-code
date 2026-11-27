package writingguide

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Each job's form is its file under forms/, the chat's the whole guide
// before its own lines; For writes each asked form once, in one order,
// a blank line between two.
func TestFor(t *testing.T) {
	file := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("forms/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	for _, j := range []Job{JobLine, JobTitle, JobWhy, JobAsk, JobBrief, JobFinding, JobSummary} {
		if got, want := For(j), file(string(j)); got != want {
			t.Errorf("For(%s) = %q, want the file %q", j, got, want)
		}
	}
	if got, want := For(JobChat), strings.TrimSpace(Guide)+"\n\n"+file("chat"); got != want {
		t.Errorf("For(chat) = %q, want the guide and the chat's lines", got)
	}
	want := file("title") + "\n\n" + file("ask")
	for _, got := range []string{For(JobTitle, JobAsk), For(JobAsk, JobTitle), For(JobAsk, JobTitle, JobAsk)} {
		if got != want {
			t.Errorf("For(title, ask) in any order = %q, want %q", got, want)
		}
	}
	if For() != "" {
		t.Errorf("For() = %q, want none", For())
	}
}

// The chat form carries the guide's two bullets on references, which the
// guide itself no longer holds.
func TestChatFormCarriesTheReferenceBullets(t *testing.T) {
	lines := strings.TrimPrefix(For(JobChat), strings.TrimSpace(Guide))
	for _, want := range []string{"Explain each reference at its first use and link it.", "Never name a thing that no reader can open"} {
		if !strings.Contains(lines, want) {
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
	if n := len(For(JobTitle)); n >= 600 {
		t.Errorf("the title form is %d bytes, want under 600", n)
	}
	count := regexp.MustCompile(`(?i)\b(?:at most|under|up to|no more than|within)\s+\d[\d,]*\s+(?:characters|runes|bytes|words)\b`)
	for _, j := range jobs {
		form := For(j)
		if j == JobChat {
			form = strings.TrimPrefix(form, strings.TrimSpace(Guide))
		}
		if c := count.FindString(form); c != "" {
			t.Errorf("the %s form sets a bound, %q", j, c)
		}
	}
}
