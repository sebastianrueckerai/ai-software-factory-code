package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tree is the factory's configuration (ruling 183): the directory factory/
// of the infra repository at one commit of main. policy_versions stores it
// as one JSON document per commit.
type Tree struct {
	// Document is policy.yaml.
	Document Document `json:"policy"`
	// Projects are projects/<name>.yaml by name.
	Projects map[string]ProjectConfig `json:"projects"`
	// Workflow is workflow.yaml.
	Workflow Workflow `json:"workflow"`
}

// The tree's files, relative to its directory.
const (
	FilePolicy   = "policy.yaml"
	FileWorkflow = "workflow.yaml"
	DirProjects  = "projects"
	// DirSchema holds the generated JSON Schemas; the loader reads none.
	DirSchema = "schema"
)

// Validate checks every file of a tree, as ParseTree does after decoding;
// for a tree read back from Postgres.
func (t Tree) Validate() error {
	errs := []error{t.Document.Validate(), t.Workflow.Validate()}
	for name, p := range t.Projects {
		errs = append(errs, p.Validate(name), t.Document.checkBounds(name, p))
	}
	return errors.Join(append(errs, checkProjects(t.Projects), t.checkAudit())...)
}

// checkAudit refuses a project that switches on an audit stage the
// workflow does not list.
func (t Tree) checkAudit() error {
	if _, ok := t.Workflow.Stage(StageAudit); ok {
		return nil
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(t.Projects)) {
		if t.Projects[name].Audit {
			errs = append(errs, fmt.Errorf("project %s: audit: the workflow lists no audit stage", name))
		}
	}
	return errors.Join(errs...)
}

// ParseTree decodes and validates a tree's files, keyed by their slash
// path relative to the tree's directory: policy.yaml, workflow.yaml and
// projects/<name>.yaml, each a single YAML document whose first line
// references its schema, without unknown keys, with every required key,
// valid by its schema and by the Go rules. Files under schema/ are
// skipped; any other file is an error. Every file's errors are reported.
func ParseTree(files map[string][]byte) (Tree, error) {
	t := Tree{Projects: map[string]ProjectConfig{}}
	var errs []error
	policyOK, projectsOK := false, []string{}
	for _, name := range []string{FilePolicy, FileWorkflow} {
		if _, ok := files[name]; !ok {
			errs = append(errs, fmt.Errorf("%s is missing", name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		b := files[name]
		var err error
		switch {
		case strings.HasPrefix(name, DirSchema+"/"):
			continue
		case name == FilePolicy:
			t.Document, err = parseFile(b, FilePolicy, kindPolicy, Document.Validate)
			policyOK = err == nil
		case name == FileWorkflow:
			t.Workflow, err = parseFile(b, FileWorkflow, kindWorkflow, Workflow.Validate)
		case path.Dir(name) == DirProjects && path.Ext(name) == ".yaml":
			project := strings.TrimSuffix(path.Base(name), ".yaml")
			if t.Projects[project], err = parseFile(b, name, kindProject, func(p ProjectConfig) error { return p.Validate(project) }); err == nil {
				projectsOK = append(projectsOK, project)
			}
		default:
			err = fmt.Errorf("unknown file %q: the tree holds %s, %s, %s/<name>.yaml and %s/", name, FilePolicy, FileWorkflow, DirProjects, DirSchema)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if policyOK {
		for _, name := range projectsOK {
			if err := t.Document.checkBounds(name, t.Projects[name]); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s.yaml: %w", DirProjects, name, err))
			}
		}
	}
	if err := errors.Join(append(errs, checkProjects(t.Projects), t.checkAudit())...); err != nil {
		return Tree{}, err
	}
	return t, nil
}

// Header is the first line of the tree's file name (policy.yaml,
// workflow.yaml, projects/<name>.yaml): the reference to its schema
// yaml-language-server reads, relative to the file.
func Header(name string) string {
	kind := kindPolicy
	switch {
	case name == FileWorkflow:
		kind = kindWorkflow
	case path.Dir(name) == DirProjects:
		kind = kindProject
	}
	rel := strings.Repeat("../", strings.Count(name, "/")) + DirSchema + "/" + kind.schemaFile()
	return "# yaml-language-server: $schema=" + rel
}

// parseFile decodes one file strictly, runs check, then checks the file
// against its kind's schema: the Go decoder refuses unknown keys and wrong
// types, check the Go rules, the schema adds the required keys.
func parseFile[T any](b []byte, name string, kind fileKind, check func(T) error) (T, error) {
	var out T
	first, _, _ := strings.Cut(string(b), "\n")
	if want := Header(name); strings.TrimRight(first, "\r") != want {
		return out, fmt.Errorf("the first line must be %s", want)
	}
	return parseBody(b, kind, check)
}

func parseBody[T any](b []byte, kind fileKind, check func(T) error) (T, error) {
	var out T
	if err := decodeOne(b, &out, true); err != nil {
		return out, err
	}
	if err := check(out); err != nil {
		return out, err
	}
	var doc any
	if err := decodeOne(b, &doc, false); err != nil {
		return out, err
	}
	if err := kind.validate(doc); err != nil {
		return out, fmt.Errorf("schema: %w", err)
	}
	return out, nil
}

// decodeOne decodes the single YAML document of b into out.
func decodeOne(b []byte, out any, strict bool) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(strict)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("more than one YAML document")
	}
	return nil
}

// ReadDir reads every regular file under dir, keyed by its slash path
// relative to dir, as ParseTree and CheckSchemas take them. Any other
// entry, a symbolic link included, is an error, as it is in the loader.
func ReadDir(dir string) (map[string][]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fsys := root.FS()
	files := map[string][]byte{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || d.IsDir():
			return err
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: the tree holds regular files only", p)
		}
		files[p], err = fs.ReadFile(fsys, p)
		return err
	})
	return files, err
}

// jsonValue turns a decoded YAML value into what encoding/json would
// decode, the form the schema validator checks.
func jsonValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(b, &out)
}
