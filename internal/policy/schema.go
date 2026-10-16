package policy

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// fileKind is one kind of file in the tree and the Go type it decodes into.
type fileKind struct {
	name string
	typ  reflect.Type
}

var (
	kindPolicy   = fileKind{"policy", reflect.TypeFor[Document]()}
	kindProject  = fileKind{"project", reflect.TypeFor[ProjectConfig]()}
	kindProcess  = fileKind{"process", reflect.TypeFor[Process]()}
	kindCommands = fileKind{"commands", reflect.TypeFor[Commands]()}
	fileKinds    = []fileKind{kindPolicy, kindProject, kindProcess, kindCommands}
)

func (k fileKind) schemaFile() string { return k.name + ".schema.json" }

// minimums and maximums bound integer fields, keyed Type.Field.
var (
	minimums = map[string]float64{"ChatConfig.MonthlyEUR": 0, "ProjectBudget.TaskMaxEUR": 0, "Limits.Steps": 1, "Risk.LargeLines": 0, "Limits.NudgeAfter": 2, "Limits.Nudges": 1, "Limits.SameCall": 2, "Limits.ReviewDiffKiB": DefaultReviewDiffKiB, "Limits.WarnAt": 1, "Limits.PlanWarnAt": 1, "Limits.NoPushSpend": 1, "Limits.ContextTokens": MinContextTokens, "PlanShape.MinCards": 1, "PlanShape.MaxCards": 2, "ProjectConfig.BacklogCap": 0, "ChatConfig.ParallelTurns": 1, "ChatConfig.Window": 1000}
	maximums = map[string]float64{"PlanShape.MinCards": 20, "PlanShape.MaxCards": 20, "Limits.ReviewDiffKiB": MaxReviewDiffKiB, "Limits.WarnAt": 99, "Limits.PlanWarnAt": 99, "Limits.NoPushSpend": 99, "Limits.ContextTokens": ModelContext}
)

// itemCounts are the least and most items of list fields, keyed Type.Field.
var itemCounts = map[string][2]int{"Process.Lanes": {2, 3}}

// enums are the enumerated values of fields, keyed Type.Field: a string
// field's values, a map field's keys, a slice field's items.
var enums = map[string][]string{
	"Document.Bounds":         append(slices.Clone(BoundActivities), BoundDefault),
	"Document.Supervisor":     slices.Sorted(maps.Keys(DefaultDetectors)),
	"Class.Tools":             append(slices.Clone(PaidTools), FreeTools...),
	"RiskClass.Outcome":       {OutcomeForbidden, OutcomeRequiresApproval, OutcomeAuto},
	"ProjectConfig.MergeRule": MergeRules,
	"ProjectConfig.Objects":   ObjectOwners,
	"ProjectConfig.Agents":    AgentModes,
	"ProjectConfig.Model":     ModelValues,
	"ProjectConfig.Autonomy":  AutonomyValues,
	"SeedTokens.Read":         SeedSecrets,
	"SeedTokens.Actions":      SeedSecrets,
	"SeedTokens.Merge":        SeedSecrets,
	"Stage.Name":              StageNames,
	"Stage.OnFindings":        FindingsRules,
	"Stage.When":              StagePredicates,
	"Lane.Name":               LaneNames,
	"Lane.MovedBy":            MovedByValues,
	"Class.Session":           Sessions,
	"Dependencies.WaitUntil":  WaitUntilValues,
	"Dependencies.OnFailed":   OnFailedValues,
	"Merge.Queue":             Queues,
	"ChatConfig.Model":        ChatModels,
	"ChatConfig.Confirm":      ChatTools,
	"Routine.Files":           RoutineFiles,
	"Routine.Pass":            RoutinePasses,
	"Routine.WhileOpen":       WhileOpenValues,
	"Routine.Missed":          MissedValues,
	"Routine.Tell":            TellValues,
	"Trigger.On":              RoutineEvents,
}

// SchemaVersion is Document.Version's one value.
const SchemaVersion = 1

// durationPattern is what time.ParseDuration takes, unsigned.
const durationPattern = `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`

// The schema types' own source, for their doc comments.
//
//go:embed policy.go project.go risk.go process.go commands.go routine.go
var typeSources embed.FS

var (
	docsOnce sync.Once
	docs     map[string]string // Type or Type.Field -> its doc comment
	docsErr  error
)

// comments reads the doc comments of the package's types and their fields
// from the embedded source.
func comments() (map[string]string, error) {
	docsOnce.Do(func() {
		docs = map[string]string{}
		entries, err := typeSources.ReadDir(".")
		if err != nil {
			docsErr = err
			return
		}
		fset := token.NewFileSet()
		for _, e := range entries {
			src, err := typeSources.ReadFile(e.Name())
			if err != nil {
				docsErr = err
				return
			}
			f, err := parser.ParseFile(fset, e.Name(), src, parser.ParseComments)
			if err != nil {
				docsErr = err
				return
			}
			for _, decl := range f.Decls {
				g, ok := decl.(*ast.GenDecl)
				if !ok || g.Tok != token.TYPE {
					continue
				}
				for _, spec := range g.Specs {
					ts := spec.(*ast.TypeSpec)
					doc := ts.Doc
					if doc == nil {
						doc = g.Doc
					}
					docs[ts.Name.Name] = oneParagraph(doc)
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, field := range st.Fields.List {
						for _, n := range field.Names {
							docs[ts.Name.Name+"."+n.Name] = oneParagraph(field.Doc)
						}
					}
				}
			}
		}
	})
	return docs, docsErr
}

func oneParagraph(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.Join(strings.Fields(g.Text()), " ")
}

// schemaFor builds the JSON Schema of t for its YAML form: properties by
// yaml tag, a key required unless its tag says omitempty, no other keys,
// durations as strings, enumerations from enums, descriptions from the doc
// comments.
func schemaFor(t reflect.Type, docs map[string]string) (*jsonschema.Schema, error) {
	if t == reflect.TypeFor[time.Duration]() {
		return &jsonschema.Schema{Type: "string", Pattern: durationPattern}, nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaFor(t.Elem(), docs)
	case reflect.Bool:
		return &jsonschema.Schema{Type: "boolean"}, nil
	case reflect.Int:
		return &jsonschema.Schema{Type: "integer"}, nil
	case reflect.Float64:
		return &jsonschema.Schema{Type: "number"}, nil
	case reflect.String:
		return &jsonschema.Schema{Type: "string"}, nil
	case reflect.Slice:
		items, err := schemaFor(t.Elem(), docs)
		return &jsonschema.Schema{Type: "array", Items: items}, err
	case reflect.Map:
		values, err := schemaFor(t.Elem(), docs)
		return &jsonschema.Schema{Type: "object", AdditionalProperties: values}, err
	case reflect.Struct:
		s := &jsonschema.Schema{Type: "object", Description: docs[t.Name()], Properties: map[string]*jsonschema.Schema{},
			AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
		for f := range fields(t) {
			key, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if key == "-" {
				continue
			}
			p, err := schemaFor(f.Type, docs)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
			}
			id := t.Name() + "." + f.Name
			if p.Description = docs[id]; p.Description == "" {
				return nil, fmt.Errorf("%s has no doc comment", id)
			}
			if values, ok := enums[id]; ok {
				enum := make([]any, len(values))
				for i, v := range values {
					enum[i] = v
				}
				switch f.Type.Kind() {
				case reflect.Map:
					p.PropertyNames = &jsonschema.Schema{Enum: enum}
				case reflect.Slice:
					p.Items.Enum = enum
				default:
					p.Enum = enum
				}
			}
			if m, ok := minimums[id]; ok {
				p.Minimum = &m
			}
			if m, ok := maximums[id]; ok {
				p.Maximum = &m
			}
			if n, ok := itemCounts[id]; ok {
				p.MinItems, p.MaxItems = &n[0], &n[1]
			}
			s.Properties[key] = p
			s.PropertyOrder = append(s.PropertyOrder, key)
			if opts != "omitempty" {
				s.Required = append(s.Required, key)
			}
		}
		if t == reflect.TypeFor[Document]() {
			s.Properties["version"].Enum = []any{SchemaVersion}
		}
		return s, nil
	}
	return nil, fmt.Errorf("type %s has no schema", t)
}

func fields(t reflect.Type) func(func(reflect.StructField) bool) {
	return func(yield func(reflect.StructField) bool) {
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() && !yield(f) {
				return
			}
		}
	}
}

// Schema is the JSON Schema of kind's files, draft-07 for editors.
func (k fileKind) schema() (*jsonschema.Schema, error) {
	docs, err := comments()
	if err != nil {
		return nil, err
	}
	s, err := schemaFor(k.typ, docs)
	if err != nil {
		return nil, fmt.Errorf("%s schema: %w", k.name, err)
	}
	s.Schema = "http://json-schema.org/draft-07/schema#"
	s.Title = "factory/" + k.name
	return s, nil
}

var (
	resolvedOnce sync.Once
	resolved     map[string]*jsonschema.Resolved
	resolvedErr  error
)

// validate checks a decoded YAML document against the kind's schema.
func (k fileKind) validate(doc any) error {
	resolvedOnce.Do(func() {
		resolved = map[string]*jsonschema.Resolved{}
		for _, kind := range fileKinds {
			s, err := kind.schema()
			if err == nil {
				resolved[kind.name], err = s.Resolve(nil)
			}
			if err != nil {
				resolvedErr = err
				return
			}
		}
	})
	if resolvedErr != nil {
		return resolvedErr
	}
	v, err := jsonValue(doc)
	if err != nil {
		return err
	}
	return resolved[k.name].Validate(v)
}

// Schemas are the generated schema files by name, as `factoryd schema`
// writes them and factory/schema/ holds them.
func Schemas() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, k := range fileKinds {
		s, err := k.schema()
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return nil, err
		}
		out[k.schemaFile()] = append(b, '\n')
	}
	return out, nil
}

// CheckSchemas refuses a file under schema/ that factoryd does not write.
// A stale or missing copy is no refusal: DiffSchemas names it, and
// factoryd opens the pull request that brings it to this build.
func CheckSchemas(files map[string][]byte) error {
	want, err := Schemas()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if rest, ok := strings.CutPrefix(name, DirSchema+"/"); ok && want[rest] == nil {
			errs = append(errs, fmt.Errorf("%s is not a schema factoryd writes", name))
		}
	}
	return errors.Join(errs...)
}

// SchemaDifference is a schema/ copy that is not what this binary
// generates.
type SchemaDifference struct {
	Path    string // relative to the tree, e.g. schema/policy.schema.json
	Missing bool
	Want    []byte
}

// DiffSchemas are the generated schemas whose copy, keyed as ReadDir keys
// it, is stale or missing, by name.
func DiffSchemas(copies map[string][]byte) ([]SchemaDifference, error) {
	want, err := Schemas()
	if err != nil {
		return nil, err
	}
	var out []SchemaDifference
	for _, name := range slices.Sorted(maps.Keys(want)) {
		path := DirSchema + "/" + name
		if got, ok := copies[path]; !ok || !bytes.Equal(got, want[name]) {
			out = append(out, SchemaDifference{Path: path, Missing: !ok, Want: want[name]})
		}
	}
	return out, nil
}

// SchemaContentHash is the first 12 hex of the sha256 over the schemas'
// names and contents: the build id the schema pull request carries, since
// the binary is not stamped with its commit.
func SchemaContentHash(schemas map[string][]byte) string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(schemas)) {
		fmt.Fprintf(h, "%s\x00%s\x00", name, schemas[name])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
