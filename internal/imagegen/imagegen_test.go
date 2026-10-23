package imagegen

import (
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	base   = "git.aisoftwarefactory.world/factory/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	shaA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

func spec() policy.ImageSpec {
	return policy.ImageSpec{
		Apt: []string{"jq", "make"},
		Upstream: []policy.UpstreamLine{
			{Name: "go", URL: "https://go.dev/dl/go1.24.linux-amd64.tar.gz?x=1", SHA256: shaA, To: "/opt/go", Strip: 1},
			{Name: "tool", URL: "https://example.com/bin/tool", SHA256: shaB, To: "/usr/local/bin/tool"},
		},
		Env:    map[string]string{"GOFLAGS": "-mod=mod", "MSG": `say "hi" \ $HOME`},
		Path:   []string{"/opt/go/bin", "/opt/x"},
		Egress: []string{"proxy.golang.org"},
	}
}

const golden = `FROM git.aisoftwarefactory.world/factory/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000001 AS inputs
USER root
ADD --checksum=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa https://git.aisoftwarefactory.world/api/packages/upstream/generic/go/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/go1.24.linux-amd64.tar.gz /tmp/factory-inputs/go
RUN mkdir -p /stage/opt/go && tar -xf /tmp/factory-inputs/go -C /stage/opt/go --strip-components=1 --no-same-owner && rm /tmp/factory-inputs/go
ADD --checksum=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb https://git.aisoftwarefactory.world/api/packages/upstream/generic/tool/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/tool /tmp/factory-inputs/tool
RUN install -D -m 0755 /tmp/factory-inputs/tool /stage/usr/local/bin/tool && rm /tmp/factory-inputs/tool
FROM git.aisoftwarefactory.world/factory/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000001
USER root
RUN apt-get update && apt-get install -y --no-install-recommends jq make && rm -rf /var/lib/apt/lists/*
COPY --from=inputs /stage/opt/ /opt/
COPY --from=inputs /stage/usr/local/ /usr/local/
ENV GOFLAGS="-mod=mod"
ENV MSG="say \"hi\" \\ \$HOME"
ENV PATH=/opt/go/bin:/opt/x:$PATH
LABEL world.aisoftwarefactory.manifest="{\"tools\":[\"jq\",\"make\",\"go\",\"tool\"],\"egress\":[\"proxy.golang.org\"]}"
USER 1000:1000
`

func TestRenderGolden(t *testing.T) {
	df, copies, m, err := Render(spec(), base)
	if err != nil {
		t.Fatal(err)
	}
	if string(df) != golden {
		t.Errorf("dockerfile:\n%s\nwant:\n%s", df, golden)
	}
	wantCopies := "go " + shaA + " go1.24.linux-amd64.tar.gz https://go.dev/dl/go1.24.linux-amd64.tar.gz?x=1 " + shaA + "\n" +
		"tool " + shaB + " tool https://example.com/bin/tool " + shaB + "\n"
	if string(copies) != wantCopies {
		t.Errorf("copies:\n%s\nwant:\n%s", copies, wantCopies)
	}
	if strings.Join(m.Tools, ",") != "jq,make,go,tool" || strings.Join(m.Egress, ",") != "proxy.golang.org" {
		t.Errorf("manifest = %+v", m)
	}
}

// Upstream files unpack in a stage of their own, under /stage, and reach the
// image by COPY with nothing run after it: a symlink one line plants (x to
// /usr/bin, e to /etc) cannot carry a later line's write outside /opt and
// /usr/local, which a root tar or install in the image itself would follow.
func TestRenderUnpacksUpstreamFilesOutsideTheImage(t *testing.T) {
	escape := policy.ImageSpec{Upstream: []policy.UpstreamLine{
		{Name: "links", URL: "https://example.com/links.tar.gz", SHA256: shaA, To: "/opt"},
		{Name: "git", URL: "https://example.com/git", SHA256: shaB, To: "/opt/x/git"},
		{Name: "preload", URL: "https://example.com/preload.tar.gz", SHA256: shaB[:63] + "c", To: "/opt/e"},
	}}
	df, _, _, err := Render(escape, base)
	if err != nil {
		t.Fatal(err)
	}
	inputs, image, ok := strings.Cut(string(df), "FROM "+base+"\n")
	if !ok || !strings.HasPrefix(inputs, "FROM "+base+" AS inputs\n") {
		t.Fatalf("dockerfile:\n%s\nwant the inputs stage, then the image's", df)
	}
	wantRuns := []string{
		"RUN mkdir -p /stage/opt && tar -xf /tmp/factory-inputs/links -C /stage/opt --strip-components=0 --no-same-owner && rm /tmp/factory-inputs/links",
		"RUN install -D -m 0755 /tmp/factory-inputs/git /stage/opt/x/git && rm /tmp/factory-inputs/git",
		"RUN mkdir -p /stage/opt/e && tar -xf /tmp/factory-inputs/preload -C /stage/opt/e --strip-components=0 --no-same-owner && rm /tmp/factory-inputs/preload",
	}
	var runs []string
	for _, l := range strings.Split(inputs, "\n") {
		if strings.HasPrefix(l, "RUN ") {
			runs = append(runs, l)
		}
	}
	if strings.Join(runs, "\n") != strings.Join(wantRuns, "\n") {
		t.Errorf("inputs stage runs:\n%s\nwant:\n%s", strings.Join(runs, "\n"), strings.Join(wantRuns, "\n"))
	}
	want := "USER root\nCOPY --from=inputs /stage/opt/ /opt/\nLABEL world.aisoftwarefactory.manifest=\"{\\\"tools\\\":[\\\"links\\\",\\\"git\\\",\\\"preload\\\"],\\\"egress\\\":[]}\"\nUSER 1000:1000\n"
	if image != want {
		t.Errorf("image stage:\n%s\nwant:\n%s", image, want)
	}
}

func TestRenderKeepsUpstreamURLOutOfRunLines(t *testing.T) {
	df, _, _, _ := Render(spec(), base)
	for _, l := range strings.Split(string(df), "\n") {
		if strings.HasPrefix(l, "RUN") && strings.Contains(l, "go.dev") {
			t.Errorf("raw upstream URL in %s", l)
		}
	}
}

func TestRenderRefusals(t *testing.T) {
	for name, ref := range map[string]string{
		"tag only":    "git.aisoftwarefactory.world/factory/sandbox:latest",
		"tag+digest":  "git.aisoftwarefactory.world/factory/sandbox:v1@sha256:" + strings.Repeat("0", 64),
		"short":       "git.aisoftwarefactory.world/factory/sandbox@sha256:abc",
		"no registry": "@sha256:" + strings.Repeat("0", 64),
		"newline":     base + "\nRUN evil",
	} {
		if _, _, _, err := Render(spec(), ref); err == nil {
			t.Errorf("%s: base %q accepted", name, ref)
		}
	}
	bad := spec()
	bad.Apt = []string{"jq; rm -rf /"}
	if _, _, _, err := Render(bad, base); err == nil {
		t.Error("invalid spec accepted")
	}
}

func TestRenderMinimalSpec(t *testing.T) {
	df, copies, m, err := Render(policy.ImageSpec{Apt: []string{"jq"}}, base)
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM " + base + "\nUSER root\nRUN apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*\n" +
		"LABEL world.aisoftwarefactory.manifest=\"{\\\"tools\\\":[\\\"jq\\\"],\\\"egress\\\":[]}\"\nUSER 1000:1000\n"
	if string(df) != want || len(copies) != 0 || m.Egress == nil {
		t.Errorf("df:\n%s\ncopies %q manifest %+v", df, copies, m)
	}
}

func TestStoreURL(t *testing.T) {
	got := StoreURL(spec().Upstream[0])
	want := "https://git.aisoftwarefactory.world/api/packages/upstream/generic/go/" + shaA + "/go1.24.linux-amd64.tar.gz"
	if got != want {
		t.Errorf("StoreURL = %s", got)
	}
}

// printf '{"Apt":["jq"],"Upstream":null,"Env":null,"Path":null,"Egress":null}\nsha256:0000000000000000000000000000000000000000000000000000000000000001\n2' | sha256sum
func TestBuildKeyExact(t *testing.T) {
	got := BuildKey(policy.ImageSpec{Apt: []string{"jq"}}, digest)
	if want := "k-21da109c06519bda5dc9e83b"; got != want {
		t.Errorf("BuildKey = %s, want %s", got, want)
	}
}

func TestBuildKeyChanges(t *testing.T) {
	k := BuildKey(spec(), digest)
	if k != BuildKey(spec(), digest) {
		t.Error("unstable")
	}
	a := spec()
	a.Apt = []string{"jq", "makf"}
	b := spec()
	b.Upstream[0].SHA256 = shaB
	for name, other := range map[string]string{
		"apt":    BuildKey(a, digest),
		"sha":    BuildKey(b, digest),
		"base":   BuildKey(spec(), "sha256:"+strings.Repeat("0", 63)+"2"),
		"egress": BuildKey(policy.ImageSpec{}, digest),
	} {
		if other == k {
			t.Errorf("%s did not change the key", name)
		}
	}
	if len(k) != 26 || !strings.HasPrefix(k, "k-") {
		t.Errorf("key = %s", k)
	}
}
