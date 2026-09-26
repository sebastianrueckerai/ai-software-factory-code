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
//
// Wire shapes verified against the live proxy (litellm-helm 1.102.1,
// 2026-09-26): /key/list returns key HASHES with explicit pagination and
// pages are 1-indexed (page=0 → 422); /key/info and /key/block both take the
// hash. Secrets are never returned by the list endpoint.
type LiteLLMAdmin struct {
	BaseURL   string // e.g. http://litellm.platform.svc:4000
	MasterKey string
	HTTP      *http.Client // nil = a call-sized timeout
	PageSize  int          // key/list page size, default 100
}

type keyList struct {
	Keys       []string `json:"keys"`
	TotalPages int      `json:"total_pages"`
}

type keyInfo struct {
	Key  string `json:"key"`
	Info struct {
		KeyName  string         `json:"key_name"`
		Blocked  bool           `json:"blocked"`
		Spend    float64        `json:"spend"`
		Metadata map[string]any `json:"metadata"`
	} `json:"info"`
}

// BlockTaskKeys blocks the task's unblocked keys and returns how many it
// blocked.
func (l *LiteLLMAdmin) BlockTaskKeys(ctx context.Context, taskID string) (int, error) {
	blocked := 0
	err := l.eachTaskKey(ctx, taskID, func(info keyInfo) error {
		if info.Info.Blocked {
			return nil
		}
		if err := l.blockKey(ctx, info.Key); err != nil {
			return err
		}
		blocked++
		return nil
	})
	return blocked, err
}

// TaskSpend sums the spend LiteLLM has recorded on the task's keys.
func (l *LiteLLMAdmin) TaskSpend(ctx context.Context, taskID string) (float64, error) {
	spend := 0.0
	err := l.eachTaskKey(ctx, taskID, func(info keyInfo) error {
		spend += info.Info.Spend
		return nil
	})
	return spend, err
}

// eachTaskKey walks every /key/list page, inspects each key hash, and calls
// fn for the keys whose metadata.task_id matches.
func (l *LiteLLMAdmin) eachTaskKey(ctx context.Context, taskID string, fn func(keyInfo) error) error {
	size := l.PageSize
	if size <= 0 {
		size = 100
	}
	for page := 1; ; page++ {
		list, err := l.listKeys(ctx, page, size)
		if err != nil {
			return err
		}
		for _, hash := range list.Keys {
			info, err := l.keyInfo(ctx, hash)
			if err != nil {
				return err
			}
			if info.Info.Metadata["task_id"] != taskID {
				continue
			}
			info.Key = hash
			if err := fn(info); err != nil {
				return err
			}
		}
		if page >= list.TotalPages {
			return nil
		}
	}
}

func (l *LiteLLMAdmin) listKeys(ctx context.Context, page, size int) (keyList, error) {
	var list keyList
	body, err := l.call(ctx, http.MethodGet,
		"/key/list?page="+strconv.Itoa(page)+"&size="+strconv.Itoa(size), nil)
	if err != nil {
		return list, err
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return list, fmt.Errorf("litellm key/list: %w", err)
	}
	return list, nil
}

func (l *LiteLLMAdmin) keyInfo(ctx context.Context, hash string) (keyInfo, error) {
	var info keyInfo
	body, err := l.call(ctx, http.MethodGet, "/key/info?key="+hash, nil)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return info, fmt.Errorf("litellm key/info: %w", err)
	}
	return info, nil
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
