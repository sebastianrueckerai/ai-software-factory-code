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

var errObjectStore = errors.New("object store refused the upload")

// ObjectStore is the seam for the session-history sink. S3 is the production
// implementation; tests substitute a fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte) error
}

// S3 talks to an S3-compatible object store with path-style addressing and
// SigV4 (stdlib only, no SDK dependency). One object per PUT: the session
// recorder re-PUTs the growing JSONL object, which S3 overwrites atomically.
type S3 struct {
	Endpoint  string // e.g. http://minio.platform.svc.cluster.local:9000
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client // nil = a PUT-sized timeout
}

func (s *S3) Put(ctx context.Context, key string, body []byte) error {
	url := fmt.Sprintf("%s/%s/%s", s.Endpoint, s.Bucket, uriEncode(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
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
		Method:  http.MethodPut,
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
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("%w: s3 put %s: status %d: %s",
			errObjectStore, key, res.StatusCode, snippet)
	}
	return nil
}
