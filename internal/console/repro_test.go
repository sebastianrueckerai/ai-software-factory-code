package console

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Repro: a plain POST /projects/{name}/merge whose Forgejo merge is refused
// with 405 every time must answer 409 with the refusal text, not 303.
func TestReproMergeRefusalPlainPost(t *testing.T) {
	s := newStatusRig(t)
	cfg := reflect.ValueOf(&s.c.cfg).Elem()
	if f := cfg.FieldByName("MergeRetryPause"); f.IsValid() && f.CanSet() && f.IsNil() {
		pause := time.Duration(0)
		f.Set(reflect.ValueOf(&pause))
	}
	s.infra.set(openPull)
	s.infra.mergeStatus, s.infra.mergeBody = http.StatusMethodNotAllowed, `{"message":"Please try again later.","url":"x"}`
	rec := s.do(post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}}), s.ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Please try again later.") {
		t.Fatalf("plain POST merge refused = %d:\n%s", rec.Code, rec.Body.String())
	}
}
