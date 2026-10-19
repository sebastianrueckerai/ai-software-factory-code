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
// re-PUT the growing JSONL object, which S3 overwrites atomically.
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
	url := fmt.Sprintf("%s/%s/%s", s.Endpoint, s.Bucket, uriEncode(key))
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body)) // #nosec G704 -- endpoint and bucket are configuration; the key is URI-encoded into the path
	if err != nil {
		return nil, err
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
	amzDate := time.Now()
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
		return nil, fmt.Errorf("s3 %s %s: %w", method, key, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return nil, ErrNotFound
	}
	if res.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return nil, fmt.Errorf("%w: s3 %s %s: status %d: %s",
			errObjectStore, method, key, res.StatusCode, snippet)
	}
	return io.ReadAll(io.LimitReader(res.Body, 64<<20))
}
