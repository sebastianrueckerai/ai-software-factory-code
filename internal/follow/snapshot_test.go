package follow

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

// testdata/tree holds the smallest real snippets of each form of reference
// of the infra tree, a marker above each, and a frozen copy of the lists.
// The expectations are read off those lists by hand.
func TestSnapshotOfTheRealForms(t *testing.T) {
	files, err := ReadTree("testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	parts := []Part{{Name: "all", Lane: LaneAlone, Paths: []string{"k8s", "clusters", ".forgejo", "factory", "scripts"}}}
	res, err := Run(files, parts, store)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res[0].Moves {
		got = append(got, m.Key+" "+m.Before+" > "+m.After)
	}
	want := []string{
		"docker.io/alpine/k8s 1.36.1 > 1.36.6",
		"ghcr.io/fluxcd/source-controller sha256:b2ac269f9d13 > sha256:4492b6e4503f",
		"ghcr.io/fluxcd/kustomize-controller sha256:ed11ebded23c > sha256:d69dcc5f20ed",
		"ghcr.io/fluxcd/helm-controller sha256:2ba739b5c0df > sha256:6b9538805ba5",
		"ghcr.io/fluxcd/notification-controller sha256:69e484e93754 > sha256:b902b87f3f69",
		"docker.io/library/node:24-bookworm-slim 24-bookworm-slim@5cbc7caba8c2 > 24-bookworm-slim@e0e989b967da",
		"quay.io/jetstack/cert-manager-controller sha256:57bb397c639e > sha256:9b262e9493ab",
		"quay.io/jetstack/cert-manager-acmesolver sha256:dc5c5b8456d1 > sha256:5d9c98380d10",
		"quay.io/jetstack/cert-manager-startupapicheck sha256:ed347b0ab607 > sha256:6b9e17adc075",
		"ghcr.io/cloudnative-pg/postgresql 18.6-system-trixie > 18.7-system-trixie",
		"code.forgejo.org/forgejo/forgejo 17.0.1 > 17.0.2",
		"docker.io/alpine/k8s 1.36.1 > 1.36.6",
		"docker.io/catthehacker/ubuntu act-22.04@a03f9d3c8f09 > act-22.04@77e3edc9c794",
		"ghcr.io/openbao/charts/openbao sha256:98c8fc901e25 > sha256:91ec229677b4",
		"quay.io/openbao/openbao 2.6.3 > 2.7.3",
		"image-of histogram-quantile_linux_amd64.tar.gz 33997073eb6d@72b27cf84d62 > 33997073eb6d@999999999999",
		"chart temporal 1.7.0 > 1.8.0",
		"docker.io/temporalio/server 1.32.0 > 1.32.2",
		"docker.io/alpine/k8s 1.36.1 > 1.36.6",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("moves:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(res[0].Markers) != 20 {
		t.Fatalf("%d markers, want 20 (the busybox one is current)", len(res[0].Markers))
	}

	next := maps.Clone(files)
	for p, b := range res[0].Changes {
		next[p] = b
	}
	again, err := Run(next, parts, store)
	if err != nil || again[0].Behind() != 0 || len(again[0].Changes) != 0 {
		t.Fatalf("a second run: %+v, %v; want nothing behind", again, err)
	}

	behind, err := Check(files, files, parts)
	if err != nil || behind != 18 {
		t.Fatalf("check of an untouched tree: %d, %v; want 18 behind (offline, the derived image's digest is not judged)", behind, err)
	}
	behind, err = Check(next, files, parts)
	if err != nil || behind != 0 {
		t.Fatalf("check of the followed tree against the old: %d, %v; want 0", behind, err)
	}
}
