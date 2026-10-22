package policy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const imagesExample = `images:
  browser:
    apt: [chromium, fonts-liberation]
    upstream:
      - name: playwright-driver
        url: https://example.com/driver-1.2.3-linux-x64.tar.gz
        sha256: 3046e0404fdc60fb80307e7a47824ba07477364178a4d09baa8548496dd6d43b
        to: /opt/playwright
        strip: 1
      - name: jq
        url: https://example.com/jq-1.7.1-linux-amd64
        sha256: 5942c9b0934e510ee61eb3e30273f1b3fe2590df93933a93d7c58b81d19c8ff5
        to: /usr/local/bin/jq
    env:
      PLAYWRIGHT_BROWSERS_PATH: /opt/playwright
    path: [/opt/playwright/bin]
    egress: [pypi.org]
`

const goodSHA = "3046e0404fdc60fb80307e7a47824ba07477364178a4d09baa8548496dd6d43b"

func oneImage(spec ImageSpec) ImagesFile {
	return ImagesFile{Images: map[string]ImageSpec{"img": spec}}
}

func oneUpstream(u UpstreamLine) ImagesFile {
	if u.Name == "" {
		u.Name = "tool"
	}
	if u.URL == "" {
		u.URL = "https://example.com/tool"
	}
	if u.SHA256 == "" {
		u.SHA256 = goodSHA
	}
	if u.To == "" {
		u.To = "/usr/local/bin/tool"
	}
	return oneImage(ImageSpec{Upstream: []UpstreamLine{u}})
}

func wantOne(t *testing.T, got []string, parts ...string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("findings = %q, want exactly one", got)
	}
	for _, p := range parts {
		if !strings.Contains(got[0], p) {
			t.Errorf("finding %q lacks %q", got[0], p)
		}
	}
}

func TestParseImagesFileValid(t *testing.T) {
	got, findings := ParseImagesFile([]byte(imagesExample))
	if findings != nil {
		t.Fatalf("findings = %q", findings)
	}
	want := ImagesFile{Images: map[string]ImageSpec{"browser": {
		Apt: []string{"chromium", "fonts-liberation"},
		Upstream: []UpstreamLine{
			{Name: "playwright-driver", URL: "https://example.com/driver-1.2.3-linux-x64.tar.gz", SHA256: goodSHA, To: "/opt/playwright", Strip: 1},
			{Name: "jq", URL: "https://example.com/jq-1.7.1-linux-amd64", SHA256: "5942c9b0934e510ee61eb3e30273f1b3fe2590df93933a93d7c58b81d19c8ff5", To: "/usr/local/bin/jq"},
		},
		Env:    map[string]string{"PLAYWRIGHT_BROWSERS_PATH": "/opt/playwright"},
		Path:   []string{"/opt/playwright/bin"},
		Egress: []string{"pypi.org"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestParseImagesFileUnknownKey(t *testing.T) {
	_, findings := ParseImagesFile([]byte("images:\n  a:\n    apts: [curl]\n"))
	wantOne(t, findings, "apts")
}

func TestParseImagesFileEmpty(t *testing.T) {
	for _, in := range []string{"", "images: {}\n"} {
		_, findings := ParseImagesFile([]byte(in))
		if len(findings) != 1 {
			t.Errorf("%q: findings = %q, want one", in, findings)
		}
	}
}

func TestValidateImageNames(t *testing.T) {
	apt := ImageSpec{Apt: []string{"curl"}}
	five := ImagesFile{Images: map[string]ImageSpec{"a": apt, "b": apt, "c": apt, "d": apt, "e": apt}}
	wantOne(t, five.Validate(), "at most 4", "5")

	long := strings.Repeat("a", 33)
	got := ImagesFile{Images: map[string]ImageSpec{"Browser": apt, long: apt}}.Validate()
	want := []string{
		`image "Browser": name must match ^[a-z0-9][a-z0-9-]{0,31}$`,
		`image "` + long + `": name must match ^[a-z0-9][a-z0-9-]{0,31}$`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}

	if f := (ImagesFile{Images: map[string]ImageSpec{"default": apt}}).Validate(); f != nil {
		t.Errorf("default: %q", f)
	}
}

func TestValidateNoTool(t *testing.T) {
	f := oneImage(ImageSpec{Egress: []string{"pypi.org"}})
	wantOne(t, f.Validate(), `image "img"`, "apt", "upstream")
}

func TestValidateApt(t *testing.T) {
	wantOne(t, oneImage(ImageSpec{Apt: []string{"chromium=120"}}).Validate(), `apt "chromium=120"`)
	wantOne(t, oneImage(ImageSpec{Apt: []string{"../x"}}).Validate(), `apt "../x"`)
	wantOne(t, oneImage(ImageSpec{Apt: []string{"curl", "curl"}}).Validate(), `apt "curl"`, "twice")
}

func TestValidateUpstream(t *testing.T) {
	cases := []struct {
		name  string
		file  ImagesFile
		field string
	}{
		{"short sha", oneUpstream(UpstreamLine{SHA256: "abc"}), "sha256"},
		{"uppercase sha", oneUpstream(UpstreamLine{SHA256: strings.ToUpper(goodSHA)}), "sha256"},
		{"http", oneUpstream(UpstreamLine{URL: "http://example.com/tool"}), "url"},
		{"user info", oneUpstream(UpstreamLine{URL: "https://u:p@example.com/tool"}), "url"},
		{"no host", oneUpstream(UpstreamLine{URL: "https:///tool"}), "url"},
		{"space in url", oneUpstream(UpstreamLine{URL: "https://example.com/a b"}), "url"},
		{"quote in url", oneUpstream(UpstreamLine{URL: "https://example.com/a\"b"}), "url"},
		{"percent newline", oneUpstream(UpstreamLine{URL: "https://example.com/a%0aRUN%20evil"}), "url"},
		{"percent space", oneUpstream(UpstreamLine{URL: "https://example.com/a%20b"}), "url"},
		{"trailing slash", oneUpstream(UpstreamLine{URL: "https://example.com/dir/"}), "url"},
		{"no file", oneUpstream(UpstreamLine{URL: "https://example.com/"}), "url"},
		{"dotdot file", oneUpstream(UpstreamLine{URL: "https://example.com/a/.."}), "url"},
		{"newline in to", oneUpstream(UpstreamLine{To: "/opt/a\nRUN x"}), "to"},
		{"space in to", oneUpstream(UpstreamLine{To: "/opt/a b"}), "to"},
		{"quote in to", oneUpstream(UpstreamLine{To: "/opt/a\"b"}), "to"},
		{"relative to", oneUpstream(UpstreamLine{To: "bin/tool"}), "to"},
		{"unclean to", oneUpstream(UpstreamLine{To: "/opt/../x"}), "to"},
		{"root to", oneUpstream(UpstreamLine{To: "/"}), "to"},
		{"strip on plain file", oneUpstream(UpstreamLine{Strip: 1}), "strip"},
		{"strip 9", oneUpstream(UpstreamLine{URL: "https://example.com/t.tgz", Strip: 9}), "strip"},
		{"bad name", oneUpstream(UpstreamLine{Name: "Tool"}), "name"},
		{"same url", oneImage(ImageSpec{Upstream: []UpstreamLine{
			{Name: "a", URL: "https://example.com/t", SHA256: goodSHA, To: "/opt/a"},
			{Name: "b", URL: "https://example.com/t", SHA256: goodSHA, To: "/opt/b"},
		}}), "url"},
		{"same name", oneImage(ImageSpec{Upstream: []UpstreamLine{
			{Name: "a", URL: "https://example.com/t1", SHA256: goodSHA, To: "/opt/a"},
			{Name: "a", URL: "https://example.com/t2", SHA256: goodSHA, To: "/opt/b"},
		}}), "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantOne(t, c.file.Validate(), `image "img"`, c.field)
		})
	}
	ok := oneUpstream(UpstreamLine{URL: "https://example.com/t.tar.xz", Strip: 8})
	if f := ok.Validate(); f != nil {
		t.Errorf("archive strip 8: %q", f)
	}
}

func TestValidateEnv(t *testing.T) {
	for _, k := range []string{"PATH", "HOME", "OPENCODE_X", "FACTORY_Y", "lower"} {
		f := oneImage(ImageSpec{Apt: []string{"curl"}, Env: map[string]string{k: "v"}})
		wantOne(t, f.Validate(), "env", `"`+k+`"`)
	}
	for _, v := range map[string]string{"newline": "a\nb", "cr": "a\rb", "del": "a\x7fb", "nul": "a\x00b", "long": strings.Repeat("x", 1025)} {
		f := oneImage(ImageSpec{Apt: []string{"curl"}, Env: map[string]string{"GOOD": v}})
		wantOne(t, f.Validate(), "env", `"GOOD"`)
	}
	ok := oneImage(ImageSpec{Apt: []string{"curl"}, Env: map[string]string{"PLAYWRIGHT_BROWSERS_PATH": "/opt/playwright", "MAX": strings.Repeat("x", 1024)}})
	if f := ok.Validate(); f != nil {
		t.Errorf("accepted env refused: %q", f)
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"bin", "/opt/../x", "/opt/", "/a:b", "/a\nb", "/a b", "/a\"b"} {
		f := oneImage(ImageSpec{Apt: []string{"curl"}, Path: []string{p}})
		wantOne(t, f.Validate(), "path", fmt.Sprintf("%q", p))
	}
	f := oneImage(ImageSpec{Apt: []string{"curl"}, Path: []string{"/opt/bin", "/opt/bin"}})
	wantOne(t, f.Validate(), "path", "twice")
}

func TestValidateEgress(t *testing.T) {
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)
	if len(long) != 254 {
		t.Fatalf("test host is %d bytes, want 254", len(long))
	}
	for _, h := range []string{"1.2.3.4", "*.example.com", "localhost", "Example.com", "a..com", "-a.com", long} {
		t.Run(h, func(t *testing.T) {
			f := oneImage(ImageSpec{Apt: []string{"curl"}, Egress: []string{h}})
			wantOne(t, f.Validate(), "egress")
		})
	}
	f := oneImage(ImageSpec{Apt: []string{"curl"}, Egress: []string{"pypi.org"}})
	if got := f.Validate(); got != nil {
		t.Errorf("pypi.org: %q", got)
	}
	f = oneImage(ImageSpec{Apt: []string{"curl"}, Egress: []string{"pypi.org", "pypi.org"}})
	wantOne(t, f.Validate(), "egress", "twice")
}

func TestTemplateImagesFileIsValid(t *testing.T) {
	files := fixtureTree(t)
	f, findings := ParseImagesFile(files["templates/ts-go/factory/images.yaml"])
	if len(findings) != 0 || len(f.Images["default"].Upstream) != 2 {
		t.Fatalf("findings %v, default %+v", findings, f.Images["default"])
	}
}
