package health

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(Handler("factoryd", "test"))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var body struct {
		Service string `json:"service"`
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Service != "factoryd" || body.Status != "ok" || body.Version != "test" {
		t.Fatalf("body = %+v", body)
	}
}

func TestVersionz(t *testing.T) {
	srv := httptest.NewServer(Version("test"))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/versionz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("X-Build"); got != "test" {
		t.Fatalf("X-Build = %q, want test", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "test" {
		t.Fatalf("body = %q, want test", body)
	}
}
