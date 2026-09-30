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
	// Processes are processes/<name>.yaml by name.
	Processes map[string]Process `json:"processes"`
}

// The tree's files, relative to its directory.
const (
	FilePolicy   = "policy.yaml"
	DirProjects  = "projects"
	DirProcesses = "processes"
	// DirSchema holds the generated JSON Schemas; the loader reads none.
	DirSchema = "schema"
)

// ProcessFile is the file of process name.
func ProcessFile(name string) string { return DirProcesses + "/" + name + ".yaml" }

// ProjectFile is the file of project name.
func ProjectFile(name string) string { return DirProjects + "/" + name + ".yaml" }

// Validate checks every file of a tree, as ParseTree does after decoding;
// for a tree read back from Postgres.
func (t Tree) Validate() error {
	errs := []error{inFile(FilePolicy, errors.Join(t.Document.Validate(), checkSessions(t.Document)))}
	for _, name := range slices.Sorted(maps.Keys(t.Processes)) {
		errs = append(errs, inFile(ProcessFile(name), t.Processes[name].Validate()))
	}
	for _, name := range slices.Sorted(maps.Keys(t.Projects)) {
		p := t.Projects[name]
		errs = append(errs, inFile(ProjectFile(name), errors.Join(p.Validate(name), t.Document.checkBounds(name, p))))
	}
	return errors.Join(append(errs, checkProjects(t.Projects), t.checkLinks())...)
}

// ParseTree decodes and validates a tree's files, keyed by their slash
// path relative to the tree's directory: policy.yaml,
// processes/<name>.yaml and projects/<name>.yaml, each a single YAML
// document whose first line references its schema, without unknown keys,
// with every required key, valid by its schema and by the Go rules, and
// what each file names in another valid. Files under schema/ are skipped;
// any other file is an error. Every file's errors are reported, one line
// each, "<file>: <block>: <reason>".
func ParseTree(files map[string][]byte) (Tree, error) {
	t := Tree{Projects: map[string]ProjectConfig{}, Processes: map[string]Process{}}
	var errs []error
	if _, ok := files[FilePolicy]; !ok {
		errs = append(errs, fmt.Errorf("%s is missing", FilePolicy))
	}
	policyOK, broken := false, false
	for _, name := range slices.Sorted(maps.Keys(files)) {
		b := files[name]
		var err error
		stem := strings.TrimSuffix(path.Base(name), ".yaml")
		switch {
		case strings.HasPrefix(name, DirSchema+"/"):
			continue
		case name == FilePolicy:
			t.Document, err = parseFile(b, FilePolicy, kindPolicy, func(d Document) error { return errors.Join(d.Validate(), checkSessions(d)) })
			policyOK = err == nil
		case path.Dir(name) == DirProcesses && path.Ext(name) == ".yaml":
			if !projectFileName.MatchString(stem) {
				err = errors.New("the file name: lowercase letters, digits and dashes, at most 37")
				break
			}
			t.Processes[stem], err = parseFile(b, name, kindProcess, Process.Validate)
		case path.Dir(name) == DirProjects && path.Ext(name) == ".yaml":
			t.Projects[stem], err = parseFile(b, name, kindProject, func(p ProjectConfig) error { return p.Validate(stem) })
		default:
			err = fmt.Errorf("unknown file %q: the tree holds %s, %s/<name>.yaml, %s/<name>.yaml, templates/<name>/ and %s/", name, FilePolicy, DirProcesses, DirProjects, DirSchema)
		}
		if err != nil {
			broken = true
			errs = append(errs, inFile(name, err))
		}
	}
	if policyOK {
		for _, name := range slices.Sorted(maps.Keys(t.Projects)) {
			errs = append(errs, inFile(ProjectFile(name), t.Document.checkBounds(name, t.Projects[name])))
		}
	}
	errs = append(errs, checkProjects(t.Projects))
	// what one file names in another is judged only between valid files:
	// a broken file's refusal says it all
	if !broken {
		errs = append(errs, t.checkLinks())
	}
	if err := errors.Join(errs...); err != nil {
		return Tree{}, err
	}
	return t, nil
}

// inFile prefixes every line of err with file.
func inFile(file string, err error) error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var errs []error
		for _, e := range j.Unwrap() {
			errs = append(errs, inFile(file, e))
		}
		return errors.Join(errs...)
	}
	return fmt.Errorf("%s: %w", file, err)
}

// checkSessions requires every class's session, which Document.Validate
// leaves out for the gateway (stage11-design.md 1.3).
func checkSessions(d Document) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(d.Classes)) {
		switch s := d.Classes[name].Session; {
		case s == "":
			errs = append(errs, fmt.Errorf("classes.%s: session is required, one of %v", name, Sessions))
		case !slices.Contains(Sessions, s):
			errs = append(errs, fmt.Errorf("classes.%s: session %q, want one of %v", name, s, Sessions))
		}
	}
	return errors.Join(errs...)
}

// checkLinks judges what the files name in each other: a lane's class of
// the right session, naming the models the stages call; a project's
// process; the audit a project switches on.
func (t Tree) checkLinks() error {
	var errs []error
	classes := slices.Sorted(maps.Keys(t.Document.Classes))
	for _, pname := range slices.Sorted(maps.Keys(t.Processes)) {
		p := t.Processes[pname]
		bad := func(block, format string, a ...any) {
			errs = append(errs, fmt.Errorf("%s: %s: "+format, append([]any{ProcessFile(pname), block}, a...)...))
		}
		review, reviewOK := p.Stage(StageReview)
		_, audit := p.Stage(StageAudit)
		for _, l := range p.Lanes {
			block := "lanes." + l.Name
			for _, ref := range []struct{ key, class, want string }{
				{"class", l.Class, map[string]string{LaneReady: SessionCode, LanePlan: SessionPlan}[l.Name]},
				{"write_up", l.WriteUp, SessionReport},
			} {
				if ref.class == "" || ref.want == "" {
					continue
				}
				c, ok := t.Document.Classes[ref.class]
				switch {
				case !ok:
					bad(block, "%s %q is not a class of policy.yaml (classes: %s)", ref.key, ref.class, strings.Join(classes, ", "))
					continue
				case c.Session == "":
					// checkSessions refuses it once, in policy.yaml
					continue
				case c.Session != ref.want && ref.key == "class":
					bad(block, "class %s has session %s; lane %s needs a class with session %s", ref.class, c.Session, l.Name, ref.want)
					continue
				case c.Session != ref.want:
					bad(block, "write_up %s has session %s; a write-up needs a class with session %s", ref.class, c.Session, ref.want)
					continue
				}
				if c.SupervisorModel == "" {
					bad(block, "class %s names no supervisor_model, which the session supervisor calls", ref.class)
				}
				if l.Name != LaneReady || ref.key != "class" {
					continue
				}
				if reviewOK && review.Enabled && c.ReviewModel == "" {
					bad(block, "class %s names no review_model, which the review stage calls", ref.class)
				}
				for _, proj := range slices.Sorted(maps.Keys(t.Projects)) {
					if pc := t.Projects[proj]; audit && pc.Process == pname && pc.Audit && c.AuditModel == "" {
						bad(block, "class %s names no audit_model, which the audit stage calls for project %s (audit: true)", ref.class, proj)
					}
				}
			}
		}
	}
	processes := slices.Sorted(maps.Keys(t.Processes))
	for _, name := range slices.Sorted(maps.Keys(t.Projects)) {
		pc := t.Projects[name]
		file := ProjectFile(name)
		p, ok := t.Processes[pc.Process]
		switch {
		case pc.Agents == AgentsNone && pc.Process != "":
			errs = append(errs, fmt.Errorf("%s: agents none: process is not allowed, no card starts there", file))
		case pc.Agents == AgentsCoder && pc.Process == "":
			errs = append(errs, fmt.Errorf("%s: agents coder needs process, the name of a file under %s/", file, DirProcesses))
		case pc.Process != "" && !ok:
			errs = append(errs, fmt.Errorf("%s: process %q: no file %s (processes: %s)", file, pc.Process, ProcessFile(pc.Process), strings.Join(processes, ", ")))
		case pc.Audit && ok:
			if _, listed := p.Stage(StageAudit); !listed {
				errs = append(errs, fmt.Errorf("%s: audit: process %s lists no audit stage", file, pc.Process))
			}
		}
	}
	for _, name := range processes {
		errs = append(errs, BuildGraph(name, t.Processes[name], t.Document).Check())
	}
	return errors.Join(errs...)
}

// Header is the first line of the tree's file name (policy.yaml,
// processes/<name>.yaml, projects/<name>.yaml): the reference to its
// schema yaml-language-server reads, relative to the file.
func Header(name string) string {
	kind := kindPolicy
	switch path.Dir(name) {
	case DirProcesses:
		kind = kindProcess
	case DirProjects:
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
