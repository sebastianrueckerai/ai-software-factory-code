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

// s3Retry starts a server that answers each request with the next handler
// (the last repeats) and swaps s3RetryWait for a recorder, so no test sleeps.
func s3Retry(t *testing.T, wait func(context.Context, time.Duration) error, steps ...http.HandlerFunc) (*S3, *[]time.Duration, *[]*http.Request, *[][]byte) {
	t.Helper()
	var mu sync.Mutex
	var reqs []*http.Request
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		i := min(len(reqs), len(steps)-1)
		reqs = append(reqs, r)
		bodies = append(bodies, b)
		mu.Unlock()
		steps[i](w, r)
	}))
	t.Cleanup(srv.Close)
	waits := &[]time.Duration{}
	old := s3RetryWait
	s3RetryWait = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		if wait != nil {
			return wait(ctx, d)
		}
		return nil
	}
	t.Cleanup(func() { s3RetryWait = old })
	s := &S3{Endpoint: srv.URL, Region: "fsn1", Bucket: "b", AccessKey: "ak", SecretKey: "sk"}
	return s, waits, &reqs, &bodies
}

func s3Status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func TestS3RetriesServerErrorsThenSucceeds(t *testing.T) {
	s, waits, reqs, _ := s3Retry(t, nil, s3Status(503), s3Status(503), func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "body")
	})
	got, err := s.Get(context.Background(), "k")
	if err != nil || string(got) != "body" {
		t.Fatalf("Get = (%q, %v), want (body, nil)", got, err)
	}
	if len(*reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(*reqs))
	}
	if want := []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}; len(*waits) != 2 || (*waits)[0] != want[0] || (*waits)[1] != want[1] {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
}

func TestS3GivesUpAfterThreeRequests(t *testing.T) {
	s, _, reqs, _ := s3Retry(t, nil, s3Status(503))
	_, err := s.Get(context.Background(), "k")
	if err == nil || !errors.Is(err, errObjectStore) || !strings.Contains(err.Error(), "status 503") || !strings.Contains(err.Error(), "GET k") {
		t.Fatalf("err = %v, want the 503 refusal naming GET k", err)
	}
	if len(*reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(*reqs))
	}
}

func TestS3DoesNotRetryClientErrors(t *testing.T) {
	s, waits, reqs, _ := s3Retry(t, nil, s3Status(404))
	got, err := s.Get(context.Background(), "k")
	if got != nil || err != nil || len(*reqs) != 1 || len(*waits) != 0 {
		t.Fatalf("404: Get = (%q, %v), %d requests, %d waits; want nil, nil, 1, 0", got, err, len(*reqs), len(*waits))
	}
	s, waits, reqs, _ = s3Retry(t, nil, s3Status(403))
	_, err = s.Get(context.Background(), "k")
	if err == nil || !strings.Contains(err.Error(), "status 403") || len(*reqs) != 1 || len(*waits) != 0 {
		t.Fatalf("403: err = %v, %d requests, %d waits; want a 403 error, 1, 0", err, len(*reqs), len(*waits))
	}
}

func TestS3RetriesTransportFailure(t *testing.T) {
	s, waits, reqs, _ := s3Retry(t, nil,
		func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		},
		func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	got, err := s.Get(context.Background(), "k")
	if err != nil || string(got) != "ok" {
		t.Fatalf("Get = (%q, %v), want (ok, nil)", got, err)
	}
	if len(*reqs) != 2 || len(*waits) != 1 {
		t.Fatalf("%d requests, %d waits; want 2, 1", len(*reqs), len(*waits))
	}
}

func TestS3ContextEndingDuringWaitStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _, reqs, _ := s3Retry(t, func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}, s3Status(500))
	_, err := s.Get(ctx, "k")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(*reqs))
	}
}

func TestS3RetriedPutResendsBodyWithFreshSignature(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 30, 3, 0, time.UTC)
	oldNow := s3Now
	s3Now = func() time.Time { return now }
	t.Cleanup(func() { s3Now = oldNow })
	s, _, reqs, bodies := s3Retry(t, func(context.Context, time.Duration) error {
		now = now.Add(time.Second)
		return nil
	}, s3Status(500), s3Status(200))
	if err := s.Put(context.Background(), "k", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(*reqs))
	}
	for i, b := range *bodies {
		if string(b) != "payload" {
			t.Errorf("attempt %d body = %q, want payload", i, b)
		}
	}
	a, b := (*reqs)[0].Header, (*reqs)[1].Header
	if a.Get("x-amz-date") == b.Get("x-amz-date") {
		t.Errorf("x-amz-date repeated: %q", a.Get("x-amz-date"))
	}
	if a.Get("Authorization") == b.Get("Authorization") {
		t.Errorf("Authorization repeated")
	}
}
