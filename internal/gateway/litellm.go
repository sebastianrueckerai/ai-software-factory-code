package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// Mint mints a virtual key for the task's slot (the task id, or
// <task>/factoryd) limited to models, with no budget of its own. Its
// metadata names the task and the slot kind, so factoryd's reconcile can
// tell the agent's keys from its own (internal/factoryd BlockTaskKeys).
func (l *LiteLLM) Mint(ctx context.Context, slot string, taskID string, models []string) (string, error) {
	if len(models) == 0 {
		return "", fmt.Errorf("key generate: no models (an empty list would allow every alias)")
	}
	metadata := map[string]string{"task_id": taskID, "slot": "agent"}
	if strings.HasSuffix(slot, "/"+factorydSA) {
		metadata["slot"] = factorydSA
	}
	payload, err := json.Marshal(map[string]any{
		"models":   models,
		"metadata": metadata,
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

	res, err := l.client().Do(req)
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

// Block blocks a superseded virtual key.
func (l *LiteLLM) Block(ctx context.Context, key string) error {
	payload, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+"/key/block", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.MasterKey)
	res, err := l.client().Do(req)
	if err != nil {
		return fmt.Errorf("key block: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		return fmt.Errorf("key block: status %d: %s", res.StatusCode, body)
	}
	return nil
}

func (l *LiteLLM) client() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}
