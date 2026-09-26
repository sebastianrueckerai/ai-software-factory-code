package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	h := requireBearer("s3cret", func(w http.ResponseWriter, r *http.Request) {})
	for header, want := range map[string]int{
		"":               http.StatusUnauthorized,
		"s3cret":         http.StatusUnauthorized,
		"Bearer wrong":   http.StatusUnauthorized,
		"Bearer s3cret2": http.StatusUnauthorized,
		"Bearer s3cre":   http.StatusUnauthorized,
		"Basic s3cret":   http.StatusUnauthorized,
		"Bearer s3cret":  http.StatusOK,
	} {
		req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, rec.Code, want)
		}
	}
}
