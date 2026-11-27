package prompts

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// A step is offered the tools its class allows that its profile names, in
// the class's order, and none its class withholds.
func TestOfferedIsTheProfileInsideTheClass(t *testing.T) {
	profile := strings.Replace(sessionProfile, "gateway: [digest]", "gateway: [digest, manual]", 1)
	c, err := load(fstest.MapFS{"profiles/s.yaml": {Data: []byte(profile)}, "chunks/s.md": {Data: []byte("S\n")}}, testWriting)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.offered("s", []string{"escalate", "manual", "digest"}); !slices.Equal(got, []string{"manual", "digest"}) {
		t.Errorf("offered = %q, want [manual digest]", got)
	}
	if got := c.offered("s", []string{"escalate"}); got == nil || len(got) != 0 {
		t.Errorf("offered = %#v, want an empty slice", got)
	}
}
