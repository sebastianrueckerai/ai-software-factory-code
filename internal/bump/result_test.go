package bump

import (
	"reflect"
	"strings"
	"testing"
)

// A result survives the log: its lines are short, what the program
// printed before it is left, and a log without one says so.
func TestAResultRoundTripsThroughThePodsLog(t *testing.T) {
	r := Result{Files: map[string]string{"go.mod": "module m\n\ntoolchain go1.26.11\n", "upstream-files.txt": strings.Repeat("a line\n", 400)}, Text: "Moved:\n- node v24.22.2 → v24.22.3\n"}
	enc, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(enc, "\n") {
		if len(l) > 76 {
			t.Fatalf("a line of %d characters: %q", len(l), l)
		}
	}
	got, err := DecodeResult("go: downloading golang.org/x/vuln v1.10.0\n" + enc)
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Errorf("DecodeResult = %+v, %v; want %+v", got, err, r)
	}
	if _, err := DecodeResult("go: downloading golang.org/x/vuln v1.10.0\n" + strings.SplitAfter(enc, "\n")[0]); err == nil || err.Error() != "the log holds no result between its marker lines" {
		t.Errorf("a cut log: %v", err)
	}
}
