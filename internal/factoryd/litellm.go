package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// LiteLLMAdmin blocks a finished task's virtual keys so no spend can happen
// after completion (Stage 4 gate item). The gateway mints keys with
// metadata.task_id (internal/gateway); this client finds them by that
// metadata and blocks them under the master key.
type LiteLLMAdmin struct {
	BaseURL   string // e.g. http://litellm.litellm.svc.cluster.local:4000
	MasterKey string
	HTTP      *http.Client // nil = a call-sized timeout
	PageSize  int          // key/list page size, default 100
}

type litellmKey struct {
	Key      string         `json:"key"`
	Token    string         `json:"token"`
	Blocked  bool           `json:"blocked"`
	Metadata map[string]any `json:"metadata"`
}

func (k litellmKey) secret() string {
	if k.Key != "" {
		return k.Key
	}
	return k.Token
}

// BlockTaskKeys lists all virtual keys, blocks the unblocked ones whose
// metadata.task_id matches, and returns how many it blocked. Listing is the
// only discovery mechanism LiteLLM offers; if the deployment returns masked
// secrets this fails loudly on the block call (verified on-cluster, Task 36).
func (l *LiteLLMAdmin) BlockTaskKeys(ctx context.Context, taskID string) (int, error) {
	size := l.PageSize
	if size <= 0 {
		size = 100
	}
	blocked := 0
	seenPages := map[string]bool{}
	for page := 0; ; page++ {
		keys, err := l.listKeys(ctx, page, size)
		if err != nil {
			return blocked, err
		}
		sig := pageSignature(keys)
		if seenPages[sig] {
			return blocked, nil // server ignored the page param; everything is already seen
		}
		seenPages[sig] = true
		for _, k := range keys {
			if k.Metadata["task_id"] != taskID || k.Blocked || k.secret() == "" {
				continue
			}
			if err := l.blockKey(ctx, k.secret()); err != nil {
				return blocked, err
			}
			blocked++
		}
		if len(keys) < size {
			return blocked, nil
		}
	}
}

func (l *LiteLLMAdmin) listKeys(ctx context.Context, page, size int) ([]litellmKey, error) {
	body, err := l.call(ctx, http.MethodGet,
		"/key/list?page="+strconv.Itoa(page)+"&size="+strconv.Itoa(size), nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Keys []litellmKey `json:"keys"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("litellm key/list: %w", err)
	}
	return res.Keys, nil
}

func (l *LiteLLMAdmin) blockKey(ctx context.Context, secret string) error {
	payload, err := json.Marshal(map[string]string{"key": secret})
	if err != nil {
		return err
	}
	_, err = l.call(ctx, http.MethodPost, "/key/block", payload)
	return err
}

func (l *LiteLLMAdmin) call(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.MasterKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("litellm %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("litellm %s %s: %w", method, path, err)
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("litellm %s %s: status %d: %s", method, path, res.StatusCode, body)
	}
	return body, nil
}

func pageSignature(keys []litellmKey) string {
	sig := ""
	for _, k := range keys {
		sig += k.secret() + "\x00"
	}
	return sig
}
