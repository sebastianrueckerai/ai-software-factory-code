package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestS3PutSignsAndShipsBody(t *testing.T) {
	var mu sync.Mutex
	var gotReq *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		mu.Lock()
		gotReq = r
		gotBody = string(b)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := S3{
		Endpoint:  srv.URL,
		Region:    "fsn1",
		Bucket:    "factory-sessions",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secret",
	}
	body := []byte(`{"kind":"tool_call"}` + "\n")
	if err := s.Put(context.Background(), "sessions/t1/s1.jsonl", body); err != nil {
		t.Fatalf("Put: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotReq.Method != http.MethodPut {
		t.Fatalf("method = %s, want PUT", gotReq.Method)
	}
	if gotReq.URL.Path != "/factory-sessions/sessions/t1/s1.jsonl" {
		t.Fatalf("path = %s", gotReq.URL.Path)
	}
	if gotBody != string(body) {
		t.Fatalf("body = %q", gotBody)
	}
	if auth := gotReq.Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") {
		t.Fatalf("Authorization = %q", auth)
	}
	sum := sha256.Sum256(body)
	if h := gotReq.Header.Get("x-amz-content-sha256"); h != hex.EncodeToString(sum[:]) {
		t.Fatalf("x-amz-content-sha256 = %q, want %q", h, hex.EncodeToString(sum[:]))
	}
	if d := gotReq.Header.Get("x-amz-date"); len(d) != 16 || !strings.HasSuffix(d, "Z") {
		t.Fatalf("x-amz-date = %q", d)
	}
}

func TestS3PutReportsStoreRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
	}))
	defer srv.Close()
	s := S3{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
		HTTP: &http.Client{Timeout: 5 * time.Second}}
	err := s.Put(context.Background(), "k", []byte("x"))
	if err == nil || !errors.Is(err, errObjectStore) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want a wrapped 403 store refusal", err)
	}
}

func TestS3GetReadsObjectAndTreats404AsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Fatalf("%s with auth %q", r.Method, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/bucket/sessions/t1/timeline.jsonl":
			io.WriteString(w, "line\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Region: "fsn1", Bucket: "bucket", AccessKey: "ak", SecretKey: "sk"}
	got, err := s.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if err != nil || string(got) != "line\n" {
		t.Fatalf("Get = (%q, %v), want (line, nil)", got, err)
	}
	got, err = s.Get(context.Background(), "sessions/t1/absent.jsonl")
	if err != nil || got != nil {
		t.Fatalf("Get absent = (%q, %v), want (nil, nil)", got, err)
	}
}
