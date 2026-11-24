package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/bump"
)

type web map[string]string

func (w web) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := w[r.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

const opencodeLine = "opencode v1.19.5 opencode-linux-x64.tar.gz https://github.com/anomalyco/opencode/releases/download/v1.19.5/opencode-linux-x64.tar.gz 588310a336fa3b9df36b3f0225b26f679d453efaad0cf3d66c3651ef3ec1647a\n"

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"upstream-images.txt": "# no image\n",
		"upstream-files.txt":  opencodeLine,
		"Dockerfile":          "", "Dockerfile.runner": "",
		"Dockerfile.sandbox": "ARG OPENCODE_VERSION=v1.19.5\n" +
			"ADD --checksum=sha256:588310a336fa3b9df36b3f0225b26f679d453efaad0cf3d66c3651ef3ec1647a https://git.aisoftwarefactory.world/api/packages/upstream/generic/opencode/${OPENCODE_VERSION}/opencode-linux-x64.tar.gz /opencode.tar.gz\n",
		"go.mod":                            "module m\n\ngo 1.26.0\n\nrequire golang.org/x/vuln v1.9.0 // indirect\n",
		"go.sum":                            "",
		"factory/images.yaml":               "images: {}\n",
		"opencode-plugin/package.json":      `{"dependencies": {"@opencode-ai/plugin": "1.19.5"}}` + "\n",
		"opencode-plugin/package-lock.json": "locked 1.19.5\n",
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The command writes the bump, has go move govulncheck's module and npm
// the lockfile of the moved plugin, and hands back every file that changed
// with the text.
func TestTheCommandHandsBackTheBumpGoAndNpmMade(t *testing.T) {
	dir := repo(t)
	w := &bump.Web{Client: &http.Client{Transport: web{
		"https://api.github.com/repos/anomalyco/opencode/releases?per_page=100":                     `[{"tag_name":"v1.19.6"},{"tag_name":"v1.19.5"}]`,
		"https://github.com/anomalyco/opencode/releases/download/v1.19.6/opencode-linux-x64.tar.gz": "opencode v1.19.6\n",
	}}}
	var calls []string
	cmd := func(ctx context.Context, at, name string, args ...string) error {
		calls = append(calls, strings.TrimPrefix(at, dir)+": "+name+" "+strings.Join(args, " "))
		switch name {
		case "go":
			return os.WriteFile(filepath.Join(at, "go.mod"), []byte("module m\n\ngo 1.26.0\n\nrequire golang.org/x/vuln v1.10.0 // indirect\n"), 0o644)
		case "npm":
			return os.WriteFile(filepath.Join(at, "package-lock.json"), []byte("locked 1.19.6\n"), 0o644)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), dir, true, w, cmd, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if want := []string{": go get -tool golang.org/x/vuln/cmd/govulncheck@latest", "/opencode-plugin: npm install --package-lock-only --ignore-scripts --no-audit --no-fund"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls %q, want %q", calls, want)
	}
	got, err := bump.DecodeResult(stdout.String())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for p := range got.Files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	if want := []string{"Dockerfile.sandbox", "go.mod", "opencode-plugin/package-lock.json", "opencode-plugin/package.json", "upstream-files.txt"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("handed back %v, want %v", paths, want)
	}
	if got.Files["opencode-plugin/package-lock.json"] != "locked 1.19.6\n" || !strings.Contains(got.Files["Dockerfile.sandbox"], "ARG OPENCODE_VERSION=v1.19.6\n") {
		t.Errorf("files %q", got.Files)
	}
	for _, line := range []string{"- opencode v1.19.5 → v1.19.6\n", "- golang.org/x/vuln v1.9.0 → v1.10.0, govulncheck's module\n"} {
		if !strings.Contains(got.Text, line) || !strings.HasPrefix(stdout.String(), got.Text) {
			t.Errorf("the text lacks %q or does not lead the output:\n%s", line, stdout.String())
		}
	}
}

// A go or npm step that fails is named in the text; the bump stands.
func TestAFailedGoStepIsNamedAndTheBumpStands(t *testing.T) {
	dir := repo(t)
	w := &bump.Web{Client: &http.Client{Transport: web{
		"https://api.github.com/repos/anomalyco/opencode/releases?per_page=100": `[{"tag_name":"v1.19.5"}]`,
	}}}
	cmd := func(ctx context.Context, at, name string, args ...string) error {
		return errors.New("go get -tool golang.org/x/vuln/cmd/govulncheck@latest: exit status 1: proxy.golang.org: no such host")
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), dir, true, w, cmd, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got, err := bump.DecodeResult(stdout.String())
	if err != nil || len(got.Files) != 0 || !strings.Contains(got.Text, "Not resolved, kept as they were:\n- go get -tool golang.org/x/vuln/cmd/govulncheck@latest: exit status 1: proxy.golang.org: no such host\n") {
		t.Errorf("result %+v, %v", got, err)
	}
}
