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
// after completion (Stage 4 gate item), and reports spend per task for
// settlement. The gateway mints keys with metadata.task_id
// (internal/gateway); this client finds them by that metadata under the
// master key.
//
// Wire shapes verified against the live proxy (litellm 1.102.1,
// 2026-09-26): /key/list?return_full_object=true returns key objects whose
// token is the key hash, with metadata, spend and blocked, and explicit
// pagination with 1-indexed pages (page=0 → 422); /key/block takes the hash.
// Secrets are never returned by the list endpoint.
type LiteLLMAdmin struct {
	BaseURL   string // e.g. http://litellm.platform.svc:4000
	MasterKey string
	HTTP      *http.Client // nil = a call-sized timeout
	PageSize  int          // key/list page size, default 100
}

type liteKey struct {
	Token    string         `json:"token"`
	Blocked  bool           `json:"blocked"`
	Spend    float64        `json:"spend"`
	Metadata map[string]any `json:"metadata"`
}

type keyList struct {
	Keys       []liteKey `json:"keys"`
	TotalPages int       `json:"total_pages"`
}

// BlockTaskKeys blocks the task's unblocked keys and returns how many it
// blocked.
func (l *LiteLLMAdmin) BlockTaskKeys(ctx context.Context, taskID string) (int, error) {
	keys, err := l.keysByTask(ctx)
	if err != nil {
		return 0, err
	}
	blocked := 0
	for _, k := range keys[taskID] {
		if k.Blocked {
			continue
		}
		if err := l.blockKey(ctx, k.Token); err != nil {
			return blocked, err
		}
		blocked++
	}
	return blocked, nil
}

// MintKey creates a virtual key for one purpose of a task: metadata
// task_id (so BlockTaskKeys and SpendByTask find it), max_budget, and the
// only models it may call. The key expires within the hour regardless.
func (l *LiteLLMAdmin) MintKey(ctx context.Context, taskID string, maxBudget float64, models []string, purpose string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"max_budget": maxBudget,
		"models":     models,
		"duration":   "1h",
		"metadata":   map[string]string{"task_id": taskID, "purpose": purpose},
	})
	if err != nil {
		return "", err
	}
	body, err := l.call(ctx, http.MethodPost, "/key/generate", payload)
	if err != nil {
		return "", err
	}
	var res struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Key == "" {
		return "", fmt.Errorf("litellm key/generate: no key in the answer")
	}
	return res.Key, nil
}

// TaskSpend sums the spend LiteLLM has recorded on one task's keys.
func (l *LiteLLMAdmin) TaskSpend(ctx context.Context, taskID string) (float64, error) {
	keys, err := l.keysByTask(ctx)
	if err != nil {
		return 0, err
	}
	spend := 0.0
	for _, k := range keys[taskID] {
		spend += k.Spend
	}
	return spend, nil
}

// SpendByTask sums the spend LiteLLM has recorded on each task's keys.
func (l *LiteLLMAdmin) SpendByTask(ctx context.Context) (map[string]float64, error) {
	keys, err := l.keysByTask(ctx)
	if err != nil {
		return nil, err
	}
	spend := map[string]float64{}
	for id, ks := range keys {
		for _, k := range ks {
			spend[id] += k.Spend
		}
	}
	return spend, nil
}

// keysByTask lists every key once, walking all /key/list pages, and groups
// them by metadata.task_id; keys without one are left out.
func (l *LiteLLMAdmin) keysByTask(ctx context.Context) (map[string][]liteKey, error) {
	size := l.PageSize
	if size <= 0 {
		size = 100
	}
	keys := map[string][]liteKey{}
	for page := 1; ; page++ {
		body, err := l.call(ctx, http.MethodGet,
			"/key/list?return_full_object=true&page="+strconv.Itoa(page)+"&size="+strconv.Itoa(size), nil)
		if err != nil {
			return nil, err
		}
		var list keyList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("litellm key/list: %w", err)
		}
		for _, k := range list.Keys {
			if id, _ := k.Metadata["task_id"].(string); id != "" {
				keys[id] = append(keys[id], k)
			}
		}
		if page >= list.TotalPages {
			return keys, nil
		}
	}
}

func (l *LiteLLMAdmin) blockKey(ctx context.Context, hash string) error {
	payload, err := json.Marshal(map[string]string{"key": hash})
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
