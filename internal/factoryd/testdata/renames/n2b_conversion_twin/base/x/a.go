package x

import "log"

// Token is a credential's type.
type Token string

// token is s with all but its last four bytes masked.
func token(s string) string {
	if len(s) < 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

func logAuth(raw string) {
	log.Printf("auth %s", token(raw))
}

func scheme() string {
	token := "Bearer"
	return token
}
