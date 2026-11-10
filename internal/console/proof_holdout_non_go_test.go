package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// non_go_sentences_in_form: the two non-Go sentences the
// case-insensitive grep catches — the board's head-checks problem and
// the served console.js's card-life fallback — read in Task 8's form,
// with no factoryd log pointer.
func TestProofNonGoSentencesInForm(t *testing.T) {
	t.Run("board head checks", func(t *testing.T) {
		r := newRig(t)
		ck, _ := r.loggedIn(t)
		r.approvals.rows[1].Spec = specRedChecks()
		r.c.cfg.HTTP = &http.Client{Transport: failPath{http.DefaultTransport,
			"commits/abc123/status", "the checks of abc123 did not answer"}}
		rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("the board = %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(strings.ToLower(body), "factoryd log") {
			t.Errorf("the board's problem sentence still points at the factoryd log")
		}
		want := "The head's current checks could not be read: the checks of abc123 did not answer. Reload the board, or read the checks on the pull request."
		if !strings.Contains(body, want) {
			t.Errorf("the board's problem sentence is missing %q", want)
		}
	})
	t.Run("served console.js", func(t *testing.T) {
		r := newRig(t)
		ck, _ := r.loggedIn(t)
		rec := r.do(httptest.NewRequest(http.MethodGet, "/static/console.js", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("the served console.js = %d", rec.Code)
		}
		js := rec.Body.String()
		if strings.Contains(strings.ToLower(js), "factoryd log") {
			t.Errorf("console.js still points at the factoryd log")
		}
		life := js[strings.Index(js, "The card's life could not be read"):]
		life = life[:strings.Index(life, "\n")]
		if !strings.Contains(life, "Open the card again") || !strings.Contains(life, "could not be read") {
			t.Errorf("the card-life fallback = %q, want it in Task 8's form with its action", life)
		}
	})
}

// specRedChecks is a red-checks merge request's spec on head abc123.
func specRedChecks() json.RawMessage {
	return json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["the checks are red"]}`)
}
