package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const imagesYAML = `images:
  default:
    apt: [jq]
    egress: [proxy.golang.org]
`

const imageBase = "git.aisoftwarefactory.world/factory/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func writeImages(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "images.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestImageRenderWritesTheBuildContext(t *testing.T) {
	out := filepath.Join(t.TempDir(), "ctx")
	var so, se bytes.Buffer
	code := runImage([]string{"render", "-file", writeImages(t, imagesYAML), "-name", "default", "-base", imageBase, "-out", out}, &so, &se)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	df, _ := os.ReadFile(filepath.Join(out, "Dockerfile"))
	if !strings.HasPrefix(string(df), "FROM "+imageBase+"\n") {
		t.Errorf("Dockerfile = %s", df)
	}
	cp, err := os.ReadFile(filepath.Join(out, "upstream-files.txt"))
	if err != nil || len(cp) != 0 {
		t.Errorf("upstream-files.txt = %q, %v", cp, err)
	}
	mf, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	if string(mf) != `{"tools":["jq"],"egress":["proxy.golang.org"]}`+"\n" {
		t.Errorf("manifest.json = %s", mf)
	}
}

func TestImageKeyPrintsTheBuildKey(t *testing.T) {
	var so, se bytes.Buffer
	code := runImage([]string{"key", "-file", writeImages(t, imagesYAML), "-name", "default", "-base", "sha256:0000000000000000000000000000000000000000000000000000000000000001"}, &so, &se)
	if code != 0 || !strings.HasPrefix(so.String(), "k-") || len(so.String()) != 27 {
		t.Errorf("exit %d, out %q, err %q", code, so.String(), se.String())
	}
}

func TestImageRefusesAnInvalidFileOrName(t *testing.T) {
	bad := writeImages(t, "images:\n  default:\n    apt: [\"jq; rm\"]\n")
	for _, args := range [][]string{
		{"key", "-file", bad, "-name", "default", "-base", "sha256:x"},
		{"key", "-file", writeImages(t, imagesYAML), "-name", "nope", "-base", "sha256:x"},
		{"render", "-file", bad, "-name", "default", "-base", imageBase, "-out", t.TempDir()},
	} {
		var so, se bytes.Buffer
		if code := runImage(args, &so, &se); code != 1 || se.Len() == 0 {
			t.Errorf("%v: exit %d, err %q", args, code, se.String())
		}
	}
	var so, se bytes.Buffer
	if code := runImage([]string{"key"}, &so, &se); code != 2 {
		t.Errorf("missing flags: exit %d", code)
	}
}
