package bump

import "testing"

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
