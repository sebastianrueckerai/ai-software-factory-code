package factoryd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// IdentityProxy is the localhost identity hop inside the sandbox: the harness
// points its provider config at it, and it forwards every request to
// factory-gateway under the task identity. The projected ServiceAccount JWT
// is re-read from TokenFile on every request because the kubelet rotates that
// file and harnesses have no way to attach it themselves (architecture 6.3).
type IdentityProxy struct {
	GatewayURL string
	TokenFile  string
	HTTP       *http.Client // nil = no timeout, streaming-safe
}

func (p *IdentityProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, err := readToken(p.TokenFile)
	if err != nil {
		http.Error(w, "task identity unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	target := strings.TrimSuffix(p.GatewayURL, "/") + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		switch strings.ToLower(k) {
		case "host", "authorization", "content-length", "connection":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	for k, vs := range res.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	flushCopy(w, res.Body)
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// flushCopy streams with per-write flushes so SSE model responses pass
// through unbuffered.
func flushCopy(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
