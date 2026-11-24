package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// LiteLLMAdmin blocks a finished task's virtual keys so no spend can happen
// after completion (Stage 4 gate item), and reports spend per task for
// settlement. The gateway mints keys with metadata.task_id and a metadata
// slot naming the agent's or factoryd's own key (internal/gateway); this
// client finds them by that metadata under the master key.
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

	now         func() time.Time // nil = time.Now
	mu          sync.Mutex       // guards the three below
	backstop    Backstop         // ProxyBudget's last read
	backstopErr error            // its failure, nil for an answer
	backstopAt  time.Time        // when it was read, zero for none
}

// Backstop is LiteLLM's proxy-wide budget, the absolute monthly limit
// behind the policy's caps: the spend of its budget period and its
// max_budget, in USD, and ResetAt, when LiteLLM resets the spend
// (budget_reset_at), zero when LiteLLM names no moment in RFC 3339.
type Backstop struct {
	SpendUSD, MaxUSD float64
	ResetAt          time.Time
}

// backstopTTL is how long a read of the backstop serves.
const backstopTTL = time.Minute

// ProxyBudget reads the backstop off the user LiteLLM keeps the
// proxy-wide max_budget, its spend and its reset on, litellm-proxy-budget: /user/info
// answers its row as user_info (read off the litellm 1.102.4 source). A read
// serves for backstopTTL, a failed one too, so a slow or failing LiteLLM
// costs one request per TTL; a read its caller cancelled keeps nothing. A
// row without a max_budget above 0 sets no backstop, an error.
func (l *LiteLLMAdmin) ProxyBudget(ctx context.Context) (Backstop, error) {
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	l.mu.Lock()
	b, failed, at := l.backstop, l.backstopErr, l.backstopAt
	l.mu.Unlock()
	if !at.IsZero() && now().Sub(at) < backstopTTL {
		return b, failed
	}
	b, err := l.readBackstop(ctx)
	if errors.Is(ctx.Err(), context.Canceled) {
		return b, err
	}
	l.mu.Lock()
	l.backstop, l.backstopErr, l.backstopAt = b, err, now()
	l.mu.Unlock()
	return b, err
}

// readBackstop is one read of the backstop off LiteLLM.
func (l *LiteLLMAdmin) readBackstop(ctx context.Context) (Backstop, error) {
	body, err := l.call(ctx, http.MethodGet, "/user/info?user_id=litellm-proxy-budget", nil)
	if err != nil {
		return Backstop{}, err
	}
	var info struct {
		UserInfo *struct {
			Spend         float64  `json:"spend"`
			MaxBudget     *float64 `json:"max_budget"`
			BudgetResetAt *string  `json:"budget_reset_at"`
		} `json:"user_info"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return Backstop{}, fmt.Errorf("litellm user/info: %w", err)
	}
	if info.UserInfo == nil || info.UserInfo.MaxBudget == nil || *info.UserInfo.MaxBudget <= 0 {
		return Backstop{}, fmt.Errorf("litellm user/info: litellm-proxy-budget has no max_budget above 0, so no backstop is set")
	}
	b := Backstop{SpendUSD: info.UserInfo.Spend, MaxUSD: *info.UserInfo.MaxBudget}
	// a reset moment in another form than RFC 3339 is unknown, not a
	// failed read: the figures stand without it
	if at := info.UserInfo.BudgetResetAt; at != nil {
		b.ResetAt, _ = time.Parse(time.RFC3339Nano, *at)
	}
	return b, nil
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

// BlockTaskKeys blocks the task's unblocked agent keys and returns how many
// it blocked. factoryd's own slot is left live: the gateway's cached key for
// it is reused across reconciles, and blocking it would turn the next gate
// supervisor call into a "Key is blocked" refusal.
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
		if slot, _ := k.Metadata["slot"].(string); slot == "factoryd" {
			continue
		}
		if err := l.blockKey(ctx, k.Token); err != nil {
			return blocked, err
		}
		blocked++
	}
	return blocked, nil
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

// DeleteTaskKeys deletes every key of the task ids from LiteLLM and
// returns how many (roadmap 272: a removed project's tasks' keys). A key
// already gone is not an error: LiteLLM answers it among the deleted.
func (l *LiteLLMAdmin) DeleteTaskKeys(ctx context.Context, taskIDs []string) (int, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	want := map[string]bool{}
	for _, id := range taskIDs {
		want[id] = true
	}
	keys, err := l.keysByTask(ctx)
	if err != nil {
		return 0, err
	}
	var hashes []string
	for id, ks := range keys {
		if !want[id] {
			continue
		}
		for _, k := range ks {
			hashes = append(hashes, k.Token)
		}
	}
	if len(hashes) == 0 {
		return 0, nil
	}
	payload, err := json.Marshal(map[string][]string{"keys": hashes})
	if err != nil {
		return 0, err
	}
	if _, err := l.call(ctx, http.MethodPost, "/key/delete", payload); err != nil {
		return 0, err
	}
	return len(hashes), nil
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
