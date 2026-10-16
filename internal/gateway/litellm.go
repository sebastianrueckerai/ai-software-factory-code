package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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

// SpendLog reads what LiteLLM booked for one call of a virtual key.
type SpendLog interface {
	// CallSpend sums the spend rows of key's calls tagged with holdID
	// since since; found: at least one row exists.
	CallSpend(ctx context.Context, key string, holdID int64, since time.Time) (cost float64, found bool, err error)
}

type spendLogs struct {
	Data []struct {
		Spend    float64 `json:"spend"`
		Metadata struct {
			Tag struct {
				HoldID int64 `json:"hold_id"`
			} `json:"spend_logs_metadata"`
		} `json:"metadata"`
	} `json:"data"`
}

// CallSpend reads /spend/logs/v2 for the key (LiteLLM keys its rows by the
// token's SHA-256) from a minute before since to a minute past now. The
// gateway tags every upstream call with {"hold_id": N} in
// x-litellm-spend-logs-metadata; the deployed LiteLLM requires both dates,
// with the date's space as %20 (controller probe, 2026-10-16).
func (l *LiteLLM) CallSpend(ctx context.Context, key string, holdID int64, since time.Time) (float64, bool, error) {
	sum := sha256.Sum256([]byte(key))
	stamp := func(t time.Time) string { return strings.Replace(t.UTC().Format("2006-01-02 15:04:05"), " ", "%20", 1) }
	query := fmt.Sprintf("api_key=%s&end_date=%s&page_size=100&start_date=%s",
		hex.EncodeToString(sum[:]), stamp(time.Now().Add(time.Minute)), stamp(since.Add(-time.Minute)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/spend/logs/v2?"+query, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+l.MasterKey)
	res, err := l.client().Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("spend logs: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return 0, false, fmt.Errorf("spend logs: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("spend logs: status %d: %s", res.StatusCode, textutil.Truncate(string(body), 200))
	}
	var rows spendLogs
	if err := json.Unmarshal(body, &rows); err != nil {
		return 0, false, fmt.Errorf("spend logs: %w", err)
	}
	var cost float64
	found := false
	for _, r := range rows.Data {
		if r.Metadata.Tag.HoldID == holdID {
			cost, found = cost+r.Spend, true
		}
	}
	return cost, found, nil
}
