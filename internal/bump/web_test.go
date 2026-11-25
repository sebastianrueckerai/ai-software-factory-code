package bump

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// answer is a recorded answer of a maker's host.
type answer struct {
	status int // 200 when 0
	header map[string]string
	body   string
	// token: the host answers 401 with challenge to a request without
	// this bearer token
	token, challenge string
}

// fakeWeb serves recorded answers by "METHOD URL", a key of seq its
// answers in turn, and records each request it was asked.
type fakeWeb struct {
	mu      sync.Mutex
	answers map[string]answer
	seq     map[string][]answer
	asked   []string
}

func (f *fakeWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.Method + " " + r.URL.String()
	f.mu.Lock()
	f.asked = append(f.asked, key)
	a, ok := f.answers[key]
	if s := f.seq[key]; len(s) > 0 {
		a, ok, f.seq[key] = s[0], true, s[1:]
	}
	f.mu.Unlock()
	res := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Request: r}
	switch {
	case !ok:
		res.StatusCode, res.Status, a.body = http.StatusNotFound, "404 Not Found", "no recorded answer"
	case a.token != "" && r.Header.Get("Authorization") != "Bearer "+a.token:
		res.StatusCode, res.Status, a.body = http.StatusUnauthorized, "401 Unauthorized", ""
		res.Header.Set("Www-Authenticate", a.challenge)
		a.header = nil
	}
	if a.status != 0 {
		res.StatusCode, res.Status = a.status, http.StatusText(a.status)
	}
	for k, v := range a.header {
		res.Header.Set(k, v)
	}
	if r.Method == http.MethodHead {
		a.body = ""
	}
	res.Body = io.NopCloser(strings.NewReader(a.body))
	return res, nil
}

func newWeb(answers map[string]answer) (*Web, *fakeWeb) {
	f := &fakeWeb{answers: answers}
	return &Web{Client: &http.Client{Transport: f}}, f
}

// recorded is a recorded answer's body from testdata/answers.
func recorded(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "answers", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// vulnZip is vuln.go.dev's vulndb.zip cut to its index entries, in the
// order given: index/db.json names modified, the others stand in empty.
// Its entries are stored, not compressed, so its bytes are the same on
// every Go.
func vulnZip(t *testing.T, modified string, entries ...string) string {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, name := range entries {
		f, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		body := "[]"
		if name == "index/db.json" {
			body = `{"modified":"` + modified + `"}`
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
