package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// Price is an alias's worst case over its LiteLLM deployments, per token:
// input, output (the higher of the output and reasoning rates) and the
// largest output limit (0 = none published). Costs are LiteLLM's USD prices
// booked 1:1 as EUR.
type Price struct {
	In        float64
	Out       float64
	MaxOutput int
}

// PriceBook reads prices from LiteLLM's GET /model/info, the figures
// LiteLLM bills with, and keeps them for TTL.
type PriceBook struct {
	BaseURL   string
	MasterKey string
	HTTP      *http.Client // nil = 10 s timeout
	TTL       time.Duration

	mu       sync.Mutex
	prices   map[string]Price
	unpriced map[string]bool
	at       time.Time
}

// PriceTTL is how long a fetched price table is used.
const PriceTTL = 5 * time.Minute

// Price is alias's worst-case price. An alias LiteLLM cannot price (a
// deployment without an input or output rate) is an error: callers refuse
// the call.
func (b *PriceBook) Price(ctx context.Context, alias string) (Price, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ttl := b.TTL
	if ttl == 0 {
		ttl = PriceTTL
	}
	if b.prices == nil || time.Since(b.at) >= ttl {
		prices, unpriced, err := b.fetch(ctx)
		switch {
		case err == nil:
			b.prices, b.unpriced, b.at = prices, unpriced, time.Now()
		case b.prices == nil:
			return Price{}, fmt.Errorf("LiteLLM prices: %w", err)
		default:
			log.Printf("LiteLLM prices: %v; keeping the table from %s", err, b.at.Format(time.RFC3339))
		}
	}
	p, ok := b.prices[alias]
	if !ok {
		if b.unpriced[alias] {
			return Price{}, fmt.Errorf("LiteLLM has no input or output price for a deployment of %q", alias)
		}
		return Price{}, fmt.Errorf("LiteLLM has no model %q", alias)
	}
	return p, nil
}

func (b *PriceBook) fetch(ctx context.Context) (map[string]Price, map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+"/model/info", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.MasterKey)
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET /model/info: status %d", res.StatusCode)
	}
	return parseModelInfo(body)
}

func parseModelInfo(body []byte) (map[string]Price, map[string]bool, error) {
	var info struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				In        *float64 `json:"input_cost_per_token"`
				Out       *float64 `json:"output_cost_per_token"`
				Reasoning *float64 `json:"output_cost_per_reasoning_token"`
				MaxOutput *int     `json:"max_output_tokens"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, nil, fmt.Errorf("GET /model/info: %w", err)
	}
	prices, unpriced := map[string]Price{}, map[string]bool{}
	for _, d := range info.Data {
		mi := d.ModelInfo
		if mi.In == nil || mi.Out == nil || *mi.Out <= 0 || *mi.In < 0 {
			unpriced[d.ModelName] = true
			continue
		}
		p := prices[d.ModelName]
		p.In = max(p.In, *mi.In)
		p.Out = max(p.Out, *mi.Out)
		if mi.Reasoning != nil {
			p.Out = max(p.Out, *mi.Reasoning)
		}
		if mi.MaxOutput != nil {
			p.MaxOutput = max(p.MaxOutput, *mi.MaxOutput)
		}
		prices[d.ModelName] = p
	}
	for alias := range unpriced {
		delete(prices, alias)
	}
	return prices, unpriced, nil
}
