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
	// the chat's paragraph on the reader who dropped in cold repeats three
	// sentences of the guide's bullet, so it goes
	cold := "Write for a reader who dropped in cold. This reader did not follow the work and forgot how the process runs. Give the context the reader needs and no more.\n\n"
	if !strings.Contains(file("chat"), cold) {
		t.Fatalf("the chat form lost its paragraph %q", cold)
	}
	if got, want := For(JobChat), strings.TrimSpace(Guide)+"\n\n"+strings.Replace(file("chat"), cold, "", 1); got != want {
		t.Errorf("For(chat) = %q, want the guide and the chat's lines", got)
	}
	if !strings.HasPrefix(For(JobChat), "# The writing guide\n") {
		t.Errorf("For(chat) does not open with the guide")
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
	all := For(JobChat, JobTitle, JobWhy, JobSummary, JobBrief, JobAsk)
	for _, s := range []string{"Write for a reader who dropped in cold.", "Give the context the reader needs and no more.", "Explain a term or a reference at its first use."} {
		if n := strings.Count(all, s); n != 1 {
			t.Errorf("the project agent's forms say %q %d times, want once", s, n)
		}
	}
}

// once keeps each sentence of prose at its first place: a bullet loses a
// sentence an earlier line said, a line that loses all goes, and a
// heading, a label, a quote and a numbered step stay whole. A period
// inside a code span splits nothing it keeps.
func TestOnce(t *testing.T) {
	in := "# A\n\nOne. Two.\n\n- Two. Three.\n- Three.\n\nGood:\n\n> One.\n\n1. One.\n\nOne. Two.\n\nFour `a.b` five. One.\n\nNo end"
	want := "# A\n\nOne. Two.\n\n- Three.\n\nGood:\n\n> One.\n\n1. One.\n\nFour `a.b` five.\n\nNo end"
	if got := once(in); got != want {
		t.Errorf("once(%q) =\n%q\nwant\n%q", in, got, want)
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
