package console

// The console's logins live in console_sessions so they survive a restart
// of factoryd (roadmap 164). The cookie's id is stored only as its
// SHA-256 hex; the member's Forgejo OAuth tokens are stored sealed with
// the scheme of internal/sealed — AES-256-GCM under the key derived from
// the session key with a fixed label and a random nonce per value, the
// row's id hash as additional data — never in clear, never logged.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sealed"
)

// sessionTokenKeyLabel derives the token key from Config.SessionKey: a
// change of the label changes every key, so the label is fixed.
const sessionTokenKeyLabel = "console-session-tokens-v1"

// errSealedToken: the stored value is not a sealed token of this key.
var errSealedToken = fmt.Errorf("console: %w", sealed.ErrSealed)

// sessionIDHash is the SHA-256 hex of the cookie's id; the raw id is
// never stored.
func sessionIDHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// sealToken encrypts tok with AES-256-GCM under the key of the session
// key sessionKey, with a fresh nonce and the row's id_hash as additional
// data, base64url without padding; "" is stored as "". The additional data
// binds the sealed token to the row: a copy under another cookie hash does
// not open.
func sealToken(sessionKey []byte, tok, idHash string) (string, error) {
	return sealed.Seal(sessionKey, sessionTokenKeyLabel, tok, idHash)
}

// openToken is sealToken's inverse; "" opens to "". The id_hash must match
// the one used when sealing, or the token does not open.
func openToken(sessionKey []byte, sealedTok, idHash string) (string, error) {
	tok, err := sealed.Open(sessionKey, sessionTokenKeyLabel, sealedTok, idHash)
	if errors.Is(err, sealed.ErrSealed) {
		return "", errSealedToken
	}
	return tok, err
}

// sealTokens returns s's access and refresh tokens sealed for the row;
// the empty token seals to "". The caller holds s's mutex.
func (c *Console) sealTokens(s *session) (access, refresh string, err error) {
	if access, err = sealToken(c.cfg.SessionKey, s.access, s.hash); err != nil {
		return "", "", err
	}
	refresh, err = sealToken(c.cfg.SessionKey, s.refresh, s.hash)
	return access, refresh, err
}
