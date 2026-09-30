package policy

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"text/template"
)

// DirTemplates holds the templates registration seeds a new repository
// with, one directory each (stage11-design.md section 6).
const DirTemplates = "templates"

// TemplateData is what a template's files are rendered with.
type TemplateData struct {
	// Name is the project's name.
	Name string
	// Repository is its repository, owner/name.
	Repository string
	// Image is its image, <registry host>/<repository>.
	Image string
	// Namespaces maps each environment's path to its namespace.
	Namespaces map[string]string
}

// sampleData is what the check renders every template with.
var sampleData = TemplateData{Name: "sample", Repository: "sample/sample", Image: "git.example/sample/sample",
	Namespaces: map[string]string{"deploy/staging": "sample-staging"}}

// templateRequired are the files every template carries, with why.
var templateRequired = []struct{ file, why string }{
	{".forgejo/workflows/pr.yml", "a template carries the pull request checks"},
	{".forgejo/workflows/ci.yml", "a template carries main's CI"},
	{".forgejo/roll.sh", "a template carries the roll main's CI runs"},
	{"deploy/base/kustomization.yaml", "a template carries the deploy base the environments build on"},
}

// RenderTemplate renders every file of a template with data: text/template
// with the delimiters [[ and ]], so the ${{ }} of Actions passes through,
// and missingkey=error. Each error line names its file.
func RenderTemplate(files map[string]string, data TemplateData) (map[string][]byte, error) {
	return render("", files, data)
}

// render renders files, each error line naming prefix and the file.
func render(prefix string, files map[string]string, data TemplateData) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	var errs []error
	for _, p := range slices.Sorted(maps.Keys(files)) {
		tmpl, err := template.New(path.Base(p)).Delims("[[", "]]").Option("missingkey=error").Parse(files[p])
		var b bytes.Buffer
		if err == nil {
			err = tmpl.Execute(&b, data)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", prefix, p, err))
			continue
		}
		out[p] = b.Bytes()
	}
	return out, errors.Join(errs...)
}

// checkTemplate judges one template's files: the required ones present,
// every file rendered with sample data.
func checkTemplate(name string, files map[string]string) error {
	dir := DirTemplates + "/" + name
	var errs []error
	for _, r := range templateRequired {
		if _, ok := files[r.file]; !ok {
			errs = append(errs, fmt.Errorf("%s: %s is required: %s", dir, r.file, r.why))
		}
	}
	_, err := render(dir+"/", files, sampleData)
	return errors.Join(append(errs, err)...)
}

// checkProjectTemplate judges what project file names in the templates:
// a template that exists, on a factory project, with every environment's
// path a directory of it.
func (t Tree) checkProjectTemplate(name string, p ProjectConfig) error {
	if p.Template == "" {
		return nil
	}
	file := ProjectFile(name)
	if p.Objects == ObjectsSeed {
		return fmt.Errorf("%s: objects seed: template is registration's, which a seed project's infra repository owns", file)
	}
	files, ok := t.Templates[p.Template]
	if !ok {
		return fmt.Errorf("%s: template %q: no directory %s/%s/ (templates: %s)", file, p.Template, DirTemplates, p.Template,
			strings.Join(slices.Sorted(maps.Keys(t.Templates)), ", "))
	}
	var errs []error
	for _, e := range p.Environments {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(f string) bool { return strings.HasPrefix(f, e.Path+"/") }) {
			errs = append(errs, fmt.Errorf("%s: environment %s: path %s is not in %s/%s", file, e.Namespace, e.Path, DirTemplates, p.Template))
		}
	}
	return errors.Join(errs...)
}
