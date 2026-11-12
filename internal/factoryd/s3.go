package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var errObjectStore = errors.New("object store refused the request")

// s3RetryWait waits d between a failed attempt and its retry and returns the
// context's error if the context ends first; a var so a test can record the
// waits instead of sleeping them.
var s3RetryWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// s3RetryWaits are the pauses before the first and the second retry.
var s3Now = time.Now

var s3RetryWaits = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

// ObjectStore is the seam for the session and timeline records. S3 is the
// production implementation; tests substitute a fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte) error
	// Get returns the object's body, nil when the object does not exist.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes the object, a missing one included.
	Delete(ctx context.Context, key string) error
}

// S3 talks to an S3-compatible object store with path-style addressing and
// SigV4 (stdlib only, no SDK dependency). One object per PUT: the recorders
// re-PUT the growing JSONL object, which S3 overwrites atomically. Every
// operation is idempotent, so a transport failure (the client timeout
// included) or a 500, 502, 503 or 504 answer is retried twice, after 200 ms
// and after 800 ms, each time with a freshly signed request; a 4xx never is.
type S3 struct {
	Endpoint  string // e.g. http://minio.platform.svc.cluster.local:9000
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client // nil = a PUT-sized timeout
}

func (s *S3) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.do(ctx, http.MethodPut, key, body)
	return err
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.do(ctx, http.MethodDelete, key, nil)
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	body, err := s.do(ctx, http.MethodGet, key, nil)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return body, err
}

func (s *S3) do(ctx context.Context, method, key string, body []byte) ([]byte, error) {
	var err error
	for attempt := 0; ; attempt++ {
		var out []byte
		var retry bool
		out, retry, err = s.attempt(ctx, method, key, body)
		if !retry || attempt == len(s3RetryWaits) {
			return out, err
		}
		if werr := s3RetryWait(ctx, s3RetryWaits[attempt]); werr != nil {
			return nil, werr
		}
	}
}

// attempt sends one freshly signed request. retry reports a transport
// failure (the caller's own context ending excepted) or a 5xx answer that
// is worth another try.
func (s *S3) attempt(ctx context.Context, method, key string, body []byte) (out []byte, retry bool, err error) {
	url := fmt.Sprintf("%s/%s/%s", s.Endpoint, s.Bucket, uriEncode(key))
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body)) // #nosec G704 -- endpoint and bucket are configuration; the key is URI-encoded into the path
	if err != nil {
		return nil, false, err
	}
	host := req.URL.Host
	if req.URL.Port() == "" {
		if req.URL.Scheme == "https" {
			host = req.URL.Hostname()
		} else {
			host = req.URL.Hostname() + ":80"
		}
	}
	signer := signer{AccessKey: s.AccessKey, SecretKey: s.SecretKey, Region: s.Region}
	amzDate := s3Now()
	auth := signer.sign(putRequest{
		Method:  method,
		Path:    req.URL.Path,
		Headers: [][2]string{{"Host", host}},
		Body:    body,
		AmzDate: amzDate,
	})
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", amzDate.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", sha256Hex(body))

	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req) // #nosec G704 -- the request built above
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("s3 %s %s: %w", method, key, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return nil, false, ErrNotFound
	}
	if res.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return nil, s3Transient(res.StatusCode), fmt.Errorf("%w: s3 %s %s: status %d: %s",
			errObjectStore, method, key, res.StatusCode, snippet)
	}
	out, err = io.ReadAll(io.LimitReader(res.Body, 64<<20))
	return out, false, err
}

func s3Transient(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}
