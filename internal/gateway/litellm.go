package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LiteLLM is the gateway's client to the LiteLLM proxy: it mints per-task
// virtual keys under the master key. The master key never leaves the
// gateway; sandboxes only ever see the virtual keys.
type LiteLLM struct {
	BaseURL   string
	MasterKey string
	HTTP      *http.Client // nil = a client with a mint-sized timeout
}

func (l *LiteLLM) Mint(ctx context.Context, taskID string, maxBudget float64) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"max_budget": maxBudget,
		"metadata":   map[string]string{"task_id": taskID},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+"/key/generate", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.MasterKey)

	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("key generate: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("key generate: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("key generate: status %d: %s", res.StatusCode, string(body))
	}
	var keyRes struct {
		Key   string `json:"key"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &keyRes); err != nil {
		return "", fmt.Errorf("key generate: %w", err)
	}
	if keyRes.Key != "" {
		return keyRes.Key, nil
	}
	if keyRes.Token != "" {
		return keyRes.Token, nil
	}
	return "", fmt.Errorf("key generate: response has no key")
}
