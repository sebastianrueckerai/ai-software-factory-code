package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The provider connectors. Their keys live only in the gateway; the agent
// reaches them through the MCP tools, each call held, capped and booked.

// maxToolText caps what a tool returns to the agent.
const maxToolText = 64 << 10

// maxProviderBody caps what the gateway reads of a provider response.
const maxProviderBody = 8 << 20

// providerError is a provider's refusal or failure: its HTTP status and body.
type providerError struct {
	status int
	body   string
}

func (e *providerError) Error() string {
	return fmt.Sprintf("provider answered %d: %s", e.status, truncate(e.body, 500))
}

// billedNothing: a 4xx is refused before any work, so nothing was billed;
// on a 5xx or a transport error the provider may have worked.
func billedNothing(err error) bool {
	var pe *providerError
	return errors.As(err, &pe) && pe.status/100 == 4
}

func call(ctx context.Context, client *http.Client, method, target string, header http.Header, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req) // #nosec G704 -- target is a fixed provider host from the gateway's config; only path and query carry validated tool arguments
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxProviderBody))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return b, &providerError{status: res.StatusCode, body: string(b)}
	}
	return b, nil
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n[truncated]"
}

// Firecrawl scrapes and searches the web (api.firecrawl.dev/v2). A call
// costs the creditsUsed its response reports.
type Firecrawl struct {
	BaseURL string // https://api.firecrawl.dev
	Key     string
	HTTP    *http.Client
}

func (f *Firecrawl) header() http.Header {
	return http.Header{"Authorization": {"Bearer " + f.Key}}
}

// Scrape fetches one page as markdown. Basic proxy only and no PDF
// parser: the stealth retry and per-page PDF billing would raise a call's
// credits past the held worst case.
func (f *Firecrawl) Scrape(ctx context.Context, pageURL string) (string, *int, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/v2/scrape", f.header(), map[string]any{
		"url": pageURL, "formats": []string{"markdown"}, "onlyMainContent": true,
		"proxy": "basic", "parsers": []string{},
	})
	if err != nil {
		return "", nil, err
	}
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    struct {
			Markdown string `json:"markdown"`
			Metadata struct {
				Title       string `json:"title"`
				StatusCode  int    `json:"statusCode"`
				CreditsUsed *int   `json:"creditsUsed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return "", nil, fmt.Errorf("firecrawl scrape: unreadable response: %w", err)
	}
	if !res.Success {
		return "", noResultCredits(res.Data.Metadata.CreditsUsed), fmt.Errorf("firecrawl scrape: %s", res.Error)
	}
	text := fmt.Sprintf("Title: %s\nStatus: %d\n\n%s", res.Data.Metadata.Title, res.Data.Metadata.StatusCode, res.Data.Markdown)
	return truncate(text, maxToolText), res.Data.Metadata.CreditsUsed, nil
}

// noResultCredits: "A scrape that returns no result is not charged"
// (https://www.firecrawl.dev/pricing, Failed requests).
func noResultCredits(reported *int) *int {
	if reported != nil {
		return reported
	}
	none := 0
	return &none
}

// Search returns up to limit web results: url, title, description.
func (f *Firecrawl) Search(ctx context.Context, query string, limit int) (string, *int, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/v2/search", f.header(), map[string]any{"query": query, "limit": limit})
	if err != nil {
		return "", nil, err
	}
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    struct {
			Web json.RawMessage `json:"web"`
		} `json:"data"`
		CreditsUsed *int `json:"creditsUsed"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return "", nil, fmt.Errorf("firecrawl search: unreadable response: %w", err)
	}
	if !res.Success {
		return "", noResultCredits(res.CreditsUsed), fmt.Errorf("firecrawl search: %s", res.Error)
	}
	return truncate(string(res.Data.Web), maxToolText), res.CreditsUsed, nil
}

// Replicate runs a prediction of a model version (api.replicate.com/v1). A
// prediction costs its metrics.predict_time at its hardware's rate.
type Replicate struct {
	BaseURL string // https://api.replicate.com
	Key     string
	HTTP    *http.Client
	// CancelBackoff is the first pause between cancel attempts (1 s when
	// zero), doubled after each.
	CancelBackoff time.Duration
}

// replicateMaxWait is Replicate's longest Prefer: wait.
const replicateMaxWait = 60

// cancelAttempts and cancelAttemptTimeout bound the cancel of a prediction
// still running after the wait and the read after it: at most 60 s of wait,
// 4 x 10 s + 7 s of cancel and 10 s of read, inside opencode's 150 s.
const (
	cancelAttempts       = 4
	cancelAttemptTimeout = 10 * time.Second
)

var (
	replicateModel   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)
	replicateVersion = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func (r *Replicate) header() http.Header {
	return http.Header{"Authorization": {"Bearer " + r.Key}}
}

// Version resolves the version to run: the model's latest when none is
// given, else the given one, which must belong to the model.
func (r *Replicate) Version(ctx context.Context, model, version string) (string, error) {
	if version == "" {
		b, err := call(ctx, r.HTTP, http.MethodGet, r.BaseURL+"/v1/models/"+model, r.header(), nil)
		if err != nil {
			return "", fmt.Errorf("replicate model %s: %w", model, err)
		}
		var m struct {
			LatestVersion *struct {
				ID string `json:"id"`
			} `json:"latest_version"`
		}
		if err := json.Unmarshal(b, &m); err != nil || m.LatestVersion == nil || !replicateVersion.MatchString(m.LatestVersion.ID) {
			return "", fmt.Errorf("replicate model %s has no usable latest version", model)
		}
		return m.LatestVersion.ID, nil
	}
	if _, err := call(ctx, r.HTTP, http.MethodGet, r.BaseURL+"/v1/models/"+model+"/versions/"+version, r.header(), nil); err != nil {
		return "", fmt.Errorf("replicate version %s of %s: %w", version, model, err)
	}
	return version, nil
}

type prediction struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Output  json.RawMessage `json:"output"`
	Error   json.RawMessage `json:"error"`
	Metrics struct {
		PredictTime *float64 `json:"predict_time"`
	} `json:"metrics"`
}

func (p prediction) done() bool {
	return p.Status == "succeeded" || p.Status == "failed" || p.Status == "canceled"
}

// Predict runs version on input and returns the prediction's text and its
// predict_time, nil when unknown. Replicate cancels the prediction itself
// ReplicateCancelMargin seconds before maxSeconds (Cancel-After, counted
// from its creation), so its predict_time stays within maxSeconds even when
// the gateway's own cancel fails; the gateway waits at most that long.
func (r *Replicate) Predict(ctx context.Context, version string, input map[string]any, maxSeconds float64) (string, *float64, error) {
	cancelAfter := int(maxSeconds) - policy.ReplicateCancelMargin
	h := r.header()
	h.Set("Prefer", "wait="+strconv.Itoa(min(cancelAfter, replicateMaxWait)))
	h.Set("Cancel-After", strconv.Itoa(cancelAfter)+"s")
	b, err := call(ctx, r.HTTP, http.MethodPost, r.BaseURL+"/v1/predictions", h,
		map[string]any{"version": version, "input": input})
	if err != nil {
		return "", nil, err
	}
	var p prediction
	if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
		return "", nil, fmt.Errorf("replicate: unreadable prediction: %s", truncate(string(b), 300))
	}
	if !p.done() {
		// still running after the wait: cancel it, then read what it used
		ctx := context.WithoutCancel(ctx)
		if err := r.cancel(ctx, p.ID); err != nil {
			log.Printf("REPLICATE CANCEL FAILED: prediction %s still %s; Replicate cancels it at %d s: %v", p.ID, p.Status, cancelAfter, err) // #nosec G706 -- the id matched Replicate's response, the error is the provider's
			return "", nil, fmt.Errorf("REPLICATE CANCEL FAILED: prediction %s still %s after %d attempts; Replicate cancels it at %d s: %w", p.ID, p.Status, cancelAttempts, cancelAfter, err)
		}
		gctx, done := context.WithTimeout(ctx, cancelAttemptTimeout)
		if b, err = call(gctx, r.HTTP, http.MethodGet, r.BaseURL+"/v1/predictions/"+url.PathEscape(p.ID), r.header(), nil); err == nil {
			err = json.Unmarshal(b, &p)
		}
		done()
		if err != nil || !p.done() {
			return "", nil, fmt.Errorf("replicate: prediction %s cancelled, its compute time unknown", p.ID)
		}
	}
	if p.Status != "succeeded" {
		return "", p.Metrics.PredictTime, fmt.Errorf("replicate: prediction %s %s: %s", p.ID, p.Status, truncate(string(p.Error), 500))
	}
	return truncate(string(p.Output), maxToolText), p.Metrics.PredictTime, nil
}

// cancel cancels a prediction, retried with backoff.
func (r *Replicate) cancel(ctx context.Context, id string) error {
	pause := r.CancelBackoff
	if pause <= 0 {
		pause = time.Second
	}
	var err error
	for attempt := range cancelAttempts {
		if attempt > 0 {
			time.Sleep(pause)
			pause *= 2
		}
		actx, done := context.WithTimeout(ctx, cancelAttemptTimeout)
		_, err = call(actx, r.HTTP, http.MethodPost, r.BaseURL+"/v1/predictions/"+url.PathEscape(id)+"/cancel", r.header(), nil)
		done()
		if err == nil {
			return nil
		}
	}
	return err
}

// Fal runs a fal.ai endpoint synchronously (fal.run) and prices it from
// fal's pricing API (api.fal.ai/v1/models/pricing), cached per endpoint.
type Fal struct {
	RunURL string // https://fal.run
	APIURL string // https://api.fal.ai
	Key    string
	HTTP   *http.Client

	mu     sync.Mutex
	prices map[string]falPrice
}

// FalPriceTTL is how long a fetched endpoint price is used.
const FalPriceTTL = time.Hour

type falPrice struct {
	unitPrice float64
	unit      string
	currency  string
	at        time.Time
}

var falEndpoint = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9._-]*)+$`)

func (f *Fal) header() http.Header {
	return http.Header{"Authorization": {"Key " + f.Key}}
}

// Price is the endpoint's price per unit, from the cache or fal's API.
func (f *Fal) Price(ctx context.Context, endpoint string) (falPrice, error) {
	f.mu.Lock()
	p, ok := f.prices[endpoint]
	f.mu.Unlock()
	if ok && time.Since(p.at) < FalPriceTTL {
		return p, nil
	}
	b, err := call(ctx, f.HTTP, http.MethodGet, f.APIURL+"/v1/models/pricing?endpoint_id="+url.QueryEscape(endpoint), f.header(), nil)
	if err != nil {
		return falPrice{}, fmt.Errorf("fal pricing of %s: %w", endpoint, err)
	}
	var res struct {
		Prices []struct {
			EndpointID string  `json:"endpoint_id"`
			UnitPrice  float64 `json:"unit_price"`
			Unit       string  `json:"unit"`
			Currency   string  `json:"currency"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return falPrice{}, fmt.Errorf("fal pricing of %s: unreadable: %w", endpoint, err)
	}
	for _, q := range res.Prices {
		if q.EndpointID == endpoint && q.UnitPrice > 0 {
			p = falPrice{unitPrice: q.UnitPrice, unit: q.Unit, currency: q.Currency, at: time.Now()}
			f.mu.Lock()
			if f.prices == nil {
				f.prices = map[string]falPrice{}
			}
			f.prices[endpoint] = p
			f.mu.Unlock()
			return p, nil
		}
	}
	return falPrice{}, fmt.Errorf("fal has no price for %s", endpoint)
}

// Units the gateway can bound before a call: a flat price per request, or
// a price per megapixel of the images returned.
const (
	falUnitRequests   = "requests"
	falUnitMegapixels = "megapixels"
)

// falImageSizes are fal's image_size presets
// (https://fal.ai/models/fal-ai/flux/schnell/api, ImageSize).
var falImageSizes = map[string][2]float64{
	"square_hd": {1024, 1024}, "square": {512, 512},
	"portrait_4_3": {768, 1024}, "portrait_16_9": {576, 1024},
	"landscape_4_3": {1024, 768}, "landscape_16_9": {1024, 576},
}

// falMegapixelKeys are the only input keys a megapixel endpoint takes
// (ruling 117): every other key may set the output's size or count (a
// source image, a count alias, another size name), which the hold could
// not bound.
var falMegapixelKeys = []string{"prompt", "negative_prompt", "seed", "image_size", "num_images",
	"num_inference_steps", "guidance_scale", "enable_safety_checker", "output_format"}

// falSizeStep: image models round each side up to a multiple of up to 64,
// so a requested side is bounded at that.
const falSizeStep = 64

// falBoundImages bounds a megapixel endpoint's input: its keys must be in
// falMegapixelKeys, and num_images x ceil(megapixels of image_size, each
// side rounded up to a multiple of 64) must fit maxMP. Unset, both are sent
// explicitly (1 image at landscape_4_3), so the endpoint's own default
// never decides.
func falBoundImages(input map[string]any, maxMP float64) (map[string]any, error) {
	for k := range input {
		if !slices.Contains(falMegapixelKeys, k) {
			return nil, fmt.Errorf("input %q is not one the factory can bound on a megapixel endpoint; allowed: %s", k, strings.Join(falMegapixelKeys, ", "))
		}
	}
	out := maps.Clone(input)
	n := 1.0
	if v, ok := out["num_images"]; ok {
		f, ok := v.(float64)
		if !ok || f < 1 || f != math.Trunc(f) {
			return nil, errors.New("num_images must be a whole number, at least 1")
		}
		n = f
	}
	out["num_images"] = int(n)
	if _, ok := out["image_size"]; !ok {
		out["image_size"] = "landscape_4_3"
	}
	var w, h float64
	switch v := out["image_size"].(type) {
	case string:
		wh, ok := falImageSizes[v]
		if !ok {
			return nil, fmt.Errorf("image_size %q is not a fal preset", v)
		}
		w, h = wh[0], wh[1]
	case map[string]any:
		w, _ = v["width"].(float64)
		h, _ = v["height"].(float64)
		if len(v) != 2 || w < 1 || h < 1 || w != math.Trunc(w) || h != math.Trunc(h) {
			return nil, errors.New("image_size must be a preset or {width, height} in whole pixels")
		}
	default:
		return nil, errors.New("image_size must be a preset or {width, height}")
	}
	rw, rh := math.Ceil(w/falSizeStep)*falSizeStep, math.Ceil(h/falSizeStep)*falSizeStep
	if mp := n * math.Ceil(rw*rh/1e6); mp > maxMP {
		return nil, fmt.Errorf("%d images of %gx%g are %g megapixels (each side rounded up to a multiple of %d, then per image), above the factory's %g", int(n), w, h, mp, falSizeStep, maxMP)
	}
	return out, nil
}

// falWorst is a call's worst case: fal's price for the endpoint, bounded
// before the call. fal's prices are USD, booked 1:1 as EUR like LiteLLM's.
func falWorst(p falPrice, prices policy.FalPrices) (float64, error) {
	if p.currency != "USD" {
		return 0, fmt.Errorf("fal prices the endpoint in %s, not USD", p.currency)
	}
	switch p.unit {
	case falUnitRequests:
		return p.unitPrice, nil
	case falUnitMegapixels:
		return p.unitPrice * prices.MaxMegapixels, nil
	}
	return 0, fmt.Errorf("fal prices the endpoint per %q, which the gateway cannot bound before the call", p.unit)
}

// Run calls the endpoint and returns its output and the units it used,
// nil when unknown. Megapixels are rounded up per image, as fal bills them.
func (f *Fal) Run(ctx context.Context, endpoint string, input map[string]any, unit string) (string, *float64, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.RunURL+"/"+endpoint, f.header(), input)
	if err != nil {
		return "", nil, err
	}
	var units *float64
	switch unit {
	case falUnitRequests:
		one := 1.0
		units = &one
	case falUnitMegapixels:
		var out struct {
			Images []struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"images"`
		}
		if json.Unmarshal(b, &out) == nil && len(out.Images) > 0 {
			mp := 0.0
			for _, im := range out.Images {
				if im.Width <= 0 || im.Height <= 0 {
					mp = -1
					break
				}
				mp += math.Ceil(im.Width * im.Height / 1e6)
			}
			if mp >= 0 {
				units = &mp
			}
		}
	}
	return truncate(string(b), maxToolText), units, nil
}
