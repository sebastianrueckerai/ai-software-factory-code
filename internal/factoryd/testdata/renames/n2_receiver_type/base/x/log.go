package x

import "log/slog"

// token is a bearer token; it logs redacted.
type token string

// Token is a public build tag.
type Token string

func (t token) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func headerName() string {
	token := "Authorization"
	return token
}
