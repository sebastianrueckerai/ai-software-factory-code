package reproducible

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type dockerfileStage struct {
	name string
	args map[string]string
	envs map[string]string
	runs []string
}

func logicalLines(text string) []string {
	var out []string
	var buf strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")))
		if !strings.HasSuffix(line, "\\") {
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	return out
}

func parseDockerfile(t *testing.T, path string) []*dockerfileStage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var stages []*dockerfileStage
	var cur *dockerfileStage
	for _, line := range logicalLines(string(raw)) {
		keyword, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(keyword) {
		case "FROM":
			cur = &dockerfileStage{args: map[string]string{}, envs: map[string]string{}}
			fields := strings.Fields(rest)
			for i, f := range fields {
				if strings.EqualFold(f, "AS") && i+1 < len(fields) {
					cur.name = fields[i+1]
				}
			}
			stages = append(stages, cur)
		case "ARG", "ENV":
			if cur == nil {
				if keyword == "ENV" {
					t.Fatalf("%s: ENV before any FROM", path)
				}
				continue // ARG in the global scope is not stage-scoped
			}
			for _, f := range strings.Fields(rest) {
				if k, v, ok := strings.Cut(f, "="); ok {
					if keyword == "ARG" {
						cur.args[k] = v
					} else {
						cur.envs[k] = v
					}
				} else if keyword == "ARG" {
					cur.args[f] = ""
				}
			}
		case "RUN":
			if cur == nil {
				t.Fatalf("%s: RUN before any FROM", path)
			}
			cur.runs = append(cur.runs, rest)
		}
	}
	for i, s := range stages {
		if s.name != "" {
			continue
		}
		if i == len(stages)-1 {
			s.name = "final"
		} else {
			s.name = fmt.Sprintf("unnamed-%d", i)
		}
	}
	return stages
}

func runsGoBuild(s *dockerfileStage) bool {
	for _, run := range s.runs {
		if strings.Contains(run, "go build") {
			return true
		}
	}
	return false
}

func TestDockerfilesPinSourceDateEpochInEveryBuildStage(t *testing.T) {
	// Hand-derived from the three Dockerfiles as they stand: these and only
	// these stages run `go build`.
	want := []struct {
		file   string
		stages []string
	}{
		{"Dockerfile", []string{"build"}},
		{"Dockerfile.sandbox", []string{"build"}},
		{"Dockerfile.runner", []string{"final"}},
	}
	for _, w := range want {
		t.Run(w.file, func(t *testing.T) {
			stages := parseDockerfile(t, filepath.Join("..", "..", w.file))
			var got []string
			for _, s := range stages {
				if runsGoBuild(s) {
					got = append(got, s.name)
				}
			}
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running go build = %v, want %v; a moved or added go build must update this list and carry the SOURCE_DATE_EPOCH pin", w.file, got, w.stages)
			}
			for _, s := range stages {
				if !runsGoBuild(s) {
					continue
				}
				if _, ok := s.args["SOURCE_DATE_EPOCH"]; !ok {
					t.Errorf("%s stage %s: no ARG SOURCE_DATE_EPOCH", w.file, s.name)
				}
				env, ok := s.envs["SOURCE_DATE_EPOCH"]
				if !ok || !strings.Contains(env, "SOURCE_DATE_EPOCH") {
					t.Errorf(`%s stage %s: SOURCE_DATE_EPOCH not exported to the build environment, want ENV SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH, got %q`, w.file, s.name, env)
				}
				for _, run := range s.runs {
					if !strings.Contains(run, "go build") {
						continue
					}
					if !strings.Contains(run, "-trimpath") {
						t.Errorf("%s stage %s: go build without -trimpath: %s", w.file, s.name, run)
					}
					if !strings.Contains(run, "-buildvcs=false") {
						t.Errorf("%s stage %s: go build without -buildvcs=false: %s", w.file, s.name, run)
					}
				}
			}
		})
	}
}
