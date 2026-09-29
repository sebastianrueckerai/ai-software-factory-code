package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
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

	inSet bool // parsing: an input rate was seen
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

// PriceTTL is how long a fetched price table is used. While refreshes
// fail, the old table is used for up to PriceStaleAfter TTLs, then every
// call is refused.
const (
	PriceTTL        = 5 * time.Minute
	PriceStaleAfter = 3
)

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
		case time.Since(b.at) >= PriceStaleAfter*ttl:
			return Price{}, fmt.Errorf("LiteLLM prices: %w; the table from %s is too old to use", err, b.at.Format(time.RFC3339))
		default:
			log.Printf("LiteLLM prices: %v; keeping the table from %s", err, b.at.Format(time.RFC3339))
		}
	}
	p, ok := b.prices[alias]
	if !ok {
		if b.unpriced[alias] {
			return Price{}, fmt.Errorf("LiteLLM cannot price every deployment of %q", alias)
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
			ModelName string                     `json:"model_name"`
			ModelInfo map[string]json.RawMessage `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, nil, fmt.Errorf("GET /model/info: %w", err)
	}
	prices, unpriced := map[string]Price{}, map[string]bool{}
	for _, d := range info.Data {
		dp, err := deploymentPrice(d.ModelInfo)
		if err != nil {
			log.Printf("LiteLLM prices: a deployment of %q is unpriced: %v", d.ModelName, err)
			unpriced[d.ModelName] = true
			continue
		}
		p := prices[d.ModelName]
		p.In = max(p.In, dp.In)
		p.Out = max(p.Out, dp.Out)
		p.MaxOutput = max(p.MaxOutput, dp.MaxOutput)
		prices[d.ModelName] = p
	}
	for alias := range unpriced {
		delete(prices, alias)
	}
	return prices, unpriced, nil
}

// deploymentPrice is one deployment's worst case over every per-token rate
// LiteLLM can bill a text call with: the base, long-context
// (*_above_*_tokens), service-tier, cache-write, tiered_pricing and
// off_peak_pricing rates.
// Cache reads are cheaper than input and left out. Any other cost field
// carrying a value (per query, second, image, character, ...) is a charge
// the hold cannot bound, and so is any cost multiplier (regional uplifts):
// the deployment counts as unpriced.
func deploymentPrice(mi map[string]json.RawMessage) (Price, error) {
	var p Price
	for k, raw := range mi {
		if string(raw) == "null" {
			continue
		}
		switch {
		case k == "max_output_tokens":
			var n int
			if err := json.Unmarshal(raw, &n); err != nil {
				return Price{}, fmt.Errorf("%s: %w", k, err)
			}
			p.MaxOutput = n
		case k == "tiered_pricing":
			var tiers []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &tiers); err != nil {
				return Price{}, fmt.Errorf("%s: %w", k, err)
			}
			for _, tier := range tiers {
				for tk, traw := range tier {
					if tk == "range" || string(traw) == "null" {
						continue
					}
					if err := p.addRate(tk, traw); err != nil {
						return Price{}, fmt.Errorf("tiered_pricing: %w", err)
					}
				}
			}
		case k == "off_peak_pricing":
			var offPeak map[string]json.RawMessage
			if err := json.Unmarshal(raw, &offPeak); err != nil {
				return Price{}, fmt.Errorf("%s: %w", k, err)
			}
			for rate, oraw := range offPeak {
				if rate == "windows" || rate == "hours_utc" || rate == "weekday_timezone" || string(oraw) == "null" {
					continue
				}
				if err := p.addRate(rate, oraw); err != nil {
					return Price{}, fmt.Errorf("off_peak_pricing: %w", err)
				}
			}
		case k == "search_context_cost_per_query" && string(mi["litellm_provider"]) == `"groq"`:
			// Groq bills it only for its browser_search tool, which the
			// gateway refuses; other providers (Perplexity's sonar) search on
			// every call
		case strings.Contains(k, "multiplier"):
			return Price{}, fmt.Errorf("unbounded multiplier %s", k)
		case strings.Contains(k, "cost") || strings.Contains(k, "pric"):
			if err := p.addRate(k, raw); err != nil {
				return Price{}, err
			}
		}
	}
	if p.Out <= 0 || !p.inSet {
		return Price{}, errors.New("no input or output price")
	}
	p.inSet = false
	return p, nil
}

func (p *Price) addRate(k string, raw json.RawMessage) error {
	var input, output bool
	switch {
	case strings.HasPrefix(k, "cache_read_input_token_cost"):
		return nil
	case strings.HasPrefix(k, "input_cost_per_token"), strings.HasPrefix(k, "cache_creation_input_token_cost"):
		input = true
	case strings.HasPrefix(k, "output_cost_per_token"), strings.HasPrefix(k, "output_cost_per_reasoning_token"):
		output = true
	default:
		return fmt.Errorf("unrecognised cost field %s", k)
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%s: %w", k, err)
	}
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s: %v", k, v)
	}
	if input {
		p.In, p.inSet = max(p.In, v), true
	}
	if output {
		p.Out = max(p.Out, v)
	}
	return nil
}
