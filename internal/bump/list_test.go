package bump

import (
	"reflect"
	"testing"
)

func TestImageLinesReadTheirWords(t *testing.T) {
	for _, c := range []struct {
		line string
		want ImageLine
		err  string
	}{
		{line: "docker.io/library/golang:1.26.11@sha256:aa patch", want: ImageLine{Ref: "docker.io/library/golang:1.26.11@sha256:aa", Rule: "patch"}},
		{line: "registry.k8s.io/kube-scheduler:v1.37.2@sha256:bb bootstrap with registry.k8s.io/kube-apiserver",
			want: ImageLine{Ref: "registry.k8s.io/kube-scheduler:v1.37.2@sha256:bb", Kind: "bootstrap", Rule: "with", Lead: "registry.k8s.io/kube-apiserver"}},
		{line: "quay.io/cilium/cilium-envoy:v1@sha256:cc bootstrap hold", want: ImageLine{Ref: "quay.io/cilium/cilium-envoy:v1@sha256:cc", Kind: "bootstrap", Rule: "hold"}},
		{line: "https://example.org/a-1.0.tgz@sha256:dd helm", want: ImageLine{Ref: "https://example.org/a-1.0.tgz@sha256:dd", Kind: "helm"}},
		{line: "https://example.org/f@sha256:ee onto docker.io/library/alpine:3.18.12 hold",
			want: ImageLine{Ref: "https://example.org/f@sha256:ee", Kind: "onto", Base: "docker.io/library/alpine:3.18.12", Rule: "hold"}},
		{line: "docker.io/x:1@sha256:ff with", err: `"docker.io/x:1@sha256:ff with": want a reference, then bootstrap, helm or onto <name>:<tag>, then patch, hold or with <name>`},
		{line: "docker.io/x:1@sha256:ff latest", err: `"docker.io/x:1@sha256:ff latest": want a reference, then bootstrap, helm or onto <name>:<tag>, then patch, hold or with <name>`},
		{line: "docker.io/x:1@sha256:ff hold patch", err: `"docker.io/x:1@sha256:ff hold patch": want a reference, then bootstrap, helm or onto <name>:<tag>, then patch, hold or with <name>`},
	} {
		got, err := ParseImageLine(c.line)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("ParseImageLine(%q) error %v, want %s", c.line, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseImageLine(%q) = %+v, %v; want %+v", c.line, got, err, c.want)
		}
	}
}

func TestImageRefsSplitIntoNameTagAndDigest(t *testing.T) {
	name, tag, digest, ok := splitImage("factory.talos.dev/installer/d9ff:v1.14.6@sha256:de8b")
	if !ok || name != "factory.talos.dev/installer/d9ff" || tag != "v1.14.6" || digest != "sha256:de8b" {
		t.Errorf("splitImage = %q %q %q %v", name, tag, digest, ok)
	}
	if _, _, _, ok := splitImage("docker.io/library/debian@sha256:aa"); ok {
		t.Error("a reference without a tag split")
	}
}

func TestAListLineKeepsItsWordsWhenItsPinMoves(t *testing.T) {
	list := "# a comment names docker.io/library/alpine:3.18.12@sha256:aa\n" +
		"docker.io/library/alpine:3.18.12@sha256:aa hold\n" +
		"https://example.org/f-1.0@sha256:bb onto docker.io/library/alpine:3.18.12\n"
	got := rewriteImages(list, map[string]string{
		"docker.io/library/alpine:3.18.12@sha256:aa": "docker.io/library/alpine:3.18.13@sha256:cc",
		"https://example.org/f-1.0@sha256:bb":        "https://example.org/f-1.1@sha256:dd",
	})
	want := "# a comment names docker.io/library/alpine:3.18.12@sha256:aa\n" +
		"docker.io/library/alpine:3.18.13@sha256:cc hold\n" +
		"https://example.org/f-1.1@sha256:dd onto docker.io/library/alpine:3.18.13\n"
	if got != want {
		t.Errorf("rewriteImages =\n%s\nwant\n%s", got, want)
	}
}
