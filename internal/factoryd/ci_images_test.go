package factoryd

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProjectImageWorkflow(t *testing.T) {
	data, err := os.ReadFile("../../.forgejo/workflows/project-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		Name string
		Uses string
		Run  string
		Env  map[string]string
	}
	var wf struct {
		On          map[string]struct{ Inputs map[string]any } `yaml:"on"`
		Concurrency struct {
			CancelInProgress bool `yaml:"cancel-in-progress"`
		}
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
			Steps  []step
		}
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.On) != 1 {
		t.Fatalf("triggers = %v, want workflow_dispatch only", wf.On)
	}
	dispatch, ok := wf.On["workflow_dispatch"]
	if !ok {
		t.Fatalf("triggers = %v, want workflow_dispatch", wf.On)
	}
	for _, in := range []string{"project", "image", "file", "base", "key"} {
		if _, ok := dispatch.Inputs[in]; !ok {
			t.Errorf("input %q missing", in)
		}
	}
	if wf.Concurrency.CancelInProgress {
		t.Error("concurrency cancels the running build")
	}
	job, ok := wf.Jobs["build"]
	if !ok || len(wf.Jobs) != 1 {
		t.Fatalf("jobs = %v, want the one job build", wf.Jobs)
	}
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("runs-on = %q", job.RunsOn)
	}
	// each marker is the first step, in order, that contains it
	markers := []string{"actions/checkout", "docker login", "image key", "scripts/upstream-files", "buildx build", "cosign sign"}
	at := -1
	for _, m := range markers {
		next := slices.IndexFunc(job.Steps[at+1:], func(s step) bool {
			return strings.Contains(s.Uses+s.Run, m)
		})
		if next < 0 {
			t.Fatalf("no step with %q after step %d", m, at)
		}
		at += 1 + next
	}
	sign := job.Steps[at]
	if sign.Env["IMAGES_COSIGN_PRIVATE_KEY"] != "${{ secrets.IMAGES_COSIGN_PRIVATE_KEY }}" || sign.Env["COSIGN_PASSWORD"] != "${{ secrets.IMAGES_COSIGN_PASSWORD }}" {
		t.Errorf("sign env = %v, want the images key secrets", sign.Env)
	}
	if strings.Contains(string(data), "secrets.COSIGN_") {
		t.Error("the factory's own COSIGN secrets are referenced")
	}
	if !strings.Contains(sign.Run, "--key env://IMAGES_COSIGN_PRIVATE_KEY") {
		t.Error("sign does not pass --key env://IMAGES_COSIGN_PRIVATE_KEY")
	}
	render := job.Steps[slices.IndexFunc(job.Steps, func(s step) bool { return strings.Contains(s.Run, "image key") })]
	if !strings.Contains(render.Run, `if [ "$got" != "$KEY" ]; then`) || !strings.Contains(render.Run, "exit 1") {
		t.Error("render does not fail when the computed key differs from KEY")
	}
	safe := strings.Index(render.Run, `[[ "$PROJECT" =~ ^[a-z0-9][a-z0-9-]*$ ]]`)
	safeImage := strings.Index(render.Run, `[[ "$IMAGE" =~ ^[a-z0-9][a-z0-9-]{0,31}$ ]]`)
	if safe < 0 || safeImage < 0 || safe > strings.Index(render.Run, "docker pull") || safeImage > strings.Index(render.Run, "docker pull") {
		t.Error("render does not check the project and image names before anything else")
	}
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "${{ inputs.") || strings.Contains(s.Run, "${{inputs.") {
			t.Errorf("step %q puts an input inside run", s.Name)
		}
	}
}
