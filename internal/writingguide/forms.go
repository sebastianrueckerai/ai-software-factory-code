package writingguide

import (
	"embed"
	"regexp"
	"slices"
	"strings"
)

// Job is a writing job, which has its own short form of the guide: the
// guide's rules its text can break, and the job's own.
type Job string

const (
	JobTitle   Job = "title"
	JobWhy     Job = "why"
	JobAsk     Job = "ask"
	JobBrief   Job = "brief"
	JobFinding Job = "finding"
	JobSummary Job = "summary"
	// JobLine is a short line or paragraph on a card that is neither a
	// fault nor a build summary: a reason, a verdict, a claim.
	JobLine Job = "line"
	// JobChat is the whole guide with the chat's own lines.
	JobChat Job = "chat"
)

// jobs are the jobs in the order For writes them: the chat's whole guide
// first, then the short line, a card's layers, the asks and the findings.
var jobs = []Job{JobChat, JobLine, JobTitle, JobWhy, JobSummary, JobBrief, JobAsk, JobFinding}

//go:embed forms
var formFiles embed.FS

var forms = func() map[Job]string {
	m := map[Job]string{}
	for _, j := range jobs {
		b, err := formFiles.ReadFile("forms/" + string(j) + ".md")
		if err != nil {
			panic(err)
		}
		m[j] = strings.TrimSpace(string(b))
	}
	m[JobChat] = strings.TrimSpace(Guide) + "\n\n" + m[JobChat]
	return m
}()

// For is the forms of the guide for jobs, each once, in one order
// whatever the order asked, each sentence once: what a writer whose text
// a human reads carries in its prompt.
func For(js ...Job) string {
	var parts []string
	for _, j := range jobs {
		if slices.Contains(js, j) {
			parts = append(parts, forms[j])
		}
	}
	return once(strings.Join(parts, "\n\n"))
}

var (
	sentence   = regexp.MustCompile(`[^.!?]*[.!?]+["')]*\s*|[^.!?]+$`)
	notProse   = regexp.MustCompile(`^(?:#|>|\||\d+\.\s|.*:$)`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// once is text with each sentence of prose at its first place only: a
// line, or a bullet, loses the sentences an earlier line holds, and goes
// when it loses all. A heading, a quote, a numbered step and a label that
// ends in a colon stay whole, so the examples keep their shape.
func once(text string) string {
	seen := map[string]bool{}
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, line := range lines {
		body := strings.TrimPrefix(line, "- ")
		if body == "" || notProse.MatchString(body) {
			out = append(out, line)
			continue
		}
		var kept strings.Builder
		for _, s := range sentence.FindAllString(body, -1) {
			if k := strings.TrimSpace(s); !seen[k] {
				seen[k] = true
				kept.WriteString(s)
			}
		}
		if rest := strings.TrimSpace(kept.String()); rest != "" {
			out = append(out, line[:len(line)-len(body)]+rest)
		}
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
}
