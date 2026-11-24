package bump

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Web reaches the makers' hosts, anonymously: one client, and the bearer
// tokens of the registries' repositories it has read.
type Web struct {
	Client *http.Client
	// Pause is the wait before try n+1 after a 429; nil is a second,
	// doubled each time.
	Pause  func(n int) time.Duration
	mu     sync.Mutex
	tokens map[string]string
}

// maxAnswer bounds an answer the program reads whole; a download it hashes
// is streamed.
const maxAnswer = 64 << 20

// get reads u whole. A registry's 401 names where its token is: get takes
// one there and asks again.
func (w *Web) get(ctx context.Context, u string, accept ...string) ([]byte, http.Header, error) {
	res, err := w.do(ctx, http.MethodGet, u, accept)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", u, err)
	}
	return b, res.Header, nil
}

// head asks for u's headers alone.
func (w *Web) head(ctx context.Context, u string, accept ...string) (http.Header, error) {
	res, err := w.do(ctx, http.MethodHead, u, accept)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return res.Header, nil
}

// sum is the sha256 of the program's own download of u.
func (w *Web) sum(ctx context.Context, u string) (string, error) {
	res, err := w.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, res.Body); err != nil {
		return "", fmt.Errorf("GET %s: %w", u, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (w *Web) do(ctx context.Context, method, u string, accept []string) (*http.Response, error) {
	key := tokenKey(u)
	loggedIn := false
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "upstream-bump")
		if len(accept) > 0 {
			req.Header.Set("Accept", strings.Join(accept, ", "))
		}
		w.mu.Lock()
		token := w.tokens[key]
		w.mu.Unlock()
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := w.Client.Do(req) // #nosec G107 G704 -- the URLs are the lists' and their makers' APIs
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, u, err)
		}
		if res.StatusCode == http.StatusTooManyRequests && try < 6 {
			res.Body.Close()
			// ghcr.io limits bursts of its tag pages for a moment
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(w.pause(try)):
			}
			continue
		}
		if res.StatusCode == http.StatusUnauthorized && !loggedIn {
			loggedIn = true
			challenge := res.Header.Get("Www-Authenticate")
			res.Body.Close()
			if err := w.login(ctx, key, challenge); err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, u, err)
			}
			continue
		}
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
			res.Body.Close()
			return nil, fmt.Errorf("%s %s: %s %s", method, u, res.Status, strings.TrimSpace(string(b)))
		}
		return res, nil
	}
}

// pause is the wait before the next try after a 429: a second, doubled
// each time.
func (w *Web) pause(try int) time.Duration {
	if w.Pause != nil {
		return w.Pause(try)
	}
	return time.Second << try
}

// tokenKey is the registry repository a /v2/ URL reads, which its bearer
// token covers.
func tokenKey(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	repo, _, _ := strings.Cut(strings.TrimPrefix(p.Path, "/v2/"), "/manifests/")
	repo = strings.TrimSuffix(repo, "/tags/list")
	return p.Host + "/" + repo
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// login takes an anonymous bearer token where the challenge names it.
func (w *Web) login(ctx context.Context, key, challenge string) error {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("401, and the answer names no bearer token: %q", challenge)
	}
	q := url.Values{}
	realm := ""
	for _, m := range challengeParam.FindAllStringSubmatch(params, -1) {
		if m[1] == "realm" {
			realm = m[2]
		} else {
			q.Set(m[1], m[2])
		}
	}
	if realm == "" {
		return fmt.Errorf("401, and the challenge names no realm: %q", challenge)
	}
	if q.Get("scope") == "" {
		q.Set("scope", "repository:"+strings.SplitN(key, "/", 2)[1]+":pull")
	}
	b, _, err := w.get(ctx, realm+"?"+q.Encode())
	if err != nil {
		return err
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	err = json.Unmarshal(b, &t)
	token := cmp.Or(t.Token, t.AccessToken)
	if err != nil || token == "" {
		return fmt.Errorf("the token at %s: no token in its answer", realm)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tokens == nil {
		w.tokens = map[string]string{}
	}
	w.tokens[key] = token
	return nil
}
