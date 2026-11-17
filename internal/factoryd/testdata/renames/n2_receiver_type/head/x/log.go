package x

import "log/slog"

// token is a bearer token; it logs redacted.
type token string

// Token is a public build tag.
type Token string

func (t Token) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func headerName() string {
	Token := "Authorization"
	return Token
}
