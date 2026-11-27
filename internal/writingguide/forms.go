package writingguide

import (
	"embed"
	"strings"
)

//go:embed forms
var formFiles embed.FS

// FormNames are the eight form names: a writing job's own short form of
// the guide, the guide's rules its text can break and the job's own.
var FormNames = []string{"chat", "line", "title", "why", "summary", "brief", "ask", "finding"}

var formTexts = func() map[string]string {
	m := map[string]string{}
	for _, name := range FormNames {
		b, err := formFiles.ReadFile("forms/" + name + ".md")
		if err != nil {
			panic(err)
		}
		m[name] = strings.TrimSpace(string(b))
	}
	return m
}()

// Form is the form file named name, trimmed, and whether it exists.
func Form(name string) (string, bool) {
	text, ok := formTexts[name]
	return text, ok
}
