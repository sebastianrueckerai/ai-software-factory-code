package writingguide

import (
	"embed"
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
	// JobChat is the whole guide with the chat's own lines.
	JobChat Job = "chat"
)

// jobs are the jobs in the order For writes them: the chat's whole guide
// first, then a card's layers, then the asks and the findings.
var jobs = []Job{JobChat, JobTitle, JobWhy, JobSummary, JobBrief, JobAsk, JobFinding}

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
// whatever the order asked: what a writer whose text a human reads
// carries in its prompt.
func For(js ...Job) string {
	var parts []string
	for _, j := range jobs {
		if slices.Contains(js, j) {
			parts = append(parts, forms[j])
		}
	}
	return strings.Join(parts, "\n\n")
}
