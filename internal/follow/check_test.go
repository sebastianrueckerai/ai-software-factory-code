package follow

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	old := dg("0")
	base := tree(map[string]string{
		"k8s/f.yaml":    "# pin chart temporal\nversion: 1.7.0\n# pin ghcr.io/berriai/litellm\ntag: \"1.1.1@" + old + "\"\n",
		"k8s/c.yaml":    "# pin copy cnpg\n- cnpg.yaml\n",
		"k8s/cnpg.yaml": "old\n",
	})
	with := func(f, c, copy string) map[string][]byte {
		return tree(map[string]string{"k8s/f.yaml": f, "k8s/c.yaml": c, "k8s/cnpg.yaml": copy})
	}
	same := "# pin chart temporal\nversion: 1.7.0\n# pin ghcr.io/berriai/litellm\ntag: \"1.1.1@" + old + "\"\n"
	for _, tc := range []struct {
		name   string
		tree   map[string][]byte
		behind int
		want   string
	}{
		{"untouched, all behind", base, 3, ""},
		{"a changed value that is the list's", with("# pin chart temporal\nversion: 1.8.0\n# pin ghcr.io/berriai/litellm\ntag: \"1.1.1@"+old+"\"\n", "# pin copy cnpg\n- cnpg.yaml\n", "old\n"), 2, ""},
		{"a copy that is the list's", with(same, "# pin copy cnpg\n- cnpg.yaml\n", string(copyBytes)), 2, ""},
		{"a changed value that is no line's", with("# pin chart temporal\nversion: 1.7.5\n# pin ghcr.io/berriai/litellm\ntag: \"1.1.1@"+old+"\"\n", "# pin copy cnpg\n- cnpg.yaml\n", "old\n"), 0,
			`k8s/f.yaml:2: # pin chart temporal: changed from the base, and is not the list's: it holds "version: 1.7.5", the list says "version: 1.8.0"`},
		{"a copy changed to other bytes", with(same, "# pin copy cnpg\n- cnpg.yaml\n", "other\n"), 0, "# pin copy cnpg: changed from the base, and is not the list's"},
		{"a new marker that is behind", with(same+"# pin chart k8s-infra\nversion: 0.1.0\n", "# pin copy cnpg\n- cnpg.yaml\n", "old\n"), 0, "k8s/f.yaml:6: # pin chart k8s-infra: changed from the base"},
		{"an unmarked digest", with(same+"x: "+dg("1")+"\n", "# pin copy cnpg\n- cnpg.yaml\n", "old\n"), 0, "k8s/f.yaml:5: a sha256 digest with no marker above it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			behind, err := Check(tc.tree, base, all)
			if tc.want == "" {
				if err != nil || behind != tc.behind {
					t.Fatalf("behind %d, err %v; want %d, none", behind, err, tc.behind)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if behind, err := Check(base, nil, all); err != nil || behind != 3 {
		t.Fatalf("no base: %d, %v", behind, err)
	}
}
