package x

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type secret string

func (s secret) logValue() slog.Value { return slog.StringValue("[redacted]") }

func (s secret) marshalJSON() ([]byte, error) { return json.Marshal("[redacted]") }

type gate struct{ next http.Handler }

func (g gate) serveHTTP(w http.ResponseWriter, r *http.Request) {
	g.next.ServeHTTP(w, r)
}

type denied struct{ err error }

func (d denied) Error() string { return "denied" }

func (d denied) unwrap() error { return d.err }

func use(s secret, d denied) (slog.Value, error) {
	return s.logValue(), d.unwrap()
}
