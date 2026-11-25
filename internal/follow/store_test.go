package follow

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
)

func TestHTTPStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/upstream/files/a.tgz/manifests/0123456789ab":
			if r.Method != http.MethodHead {
				t.Errorf("method %s", r.Method)
			}
			w.Header().Set("Docker-Content-Digest", dg("c"))
		case "/api/packages/upstream/generic/p/v1/a.tgz":
			w.Write([]byte("bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := HTTPStore{Base: srv.URL, User: "bot", Password: "pw"}
	if d, err := s.FileImageDigest("a.tgz", "0123456789ab"); err != nil || d != dg("c") {
		t.Fatalf("digest %q, %v", d, err)
	}
	if _, err := s.FileImageDigest("a.tgz", "ffffffffffff"); err == nil {
		t.Fatal("a missing tag answered")
	}
	b, err := s.FileCopy(pins.File{Package: "p", Version: "v1", Name: "a.tgz"})
	if err != nil || string(b) != "bytes" {
		t.Fatalf("copy %q, %v", b, err)
	}
	if _, err := (HTTPStore{Base: srv.URL}).FileCopy(pins.File{Package: "p", Version: "v1", Name: "a.tgz"}); err == nil {
		t.Fatal("an unauthorized read answered")
	}
}
