package console

// The console's logins live in console_sessions so they survive a restart
// of factoryd (roadmap 164). The cookie's id is stored only as its
// SHA-256 hex; the member's Forgejo OAuth tokens are stored encrypted
// with AES-256-GCM under a key derived from the session key with a fixed
// label and a random nonce per value, never in clear, never logged.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// sessionTokenKeyLabel derives the token key from Config.SessionKey: a
// change of the label changes every key, so the label is fixed.
const sessionTokenKeyLabel = "console-session-tokens-v1"

// errSealedToken: the stored value is not a sealed token of this key.
var errSealedToken = errors.New("console: the stored token does not decrypt")

// sessionIDHash is the SHA-256 hex of the cookie's id; the raw id is
// never stored.
func sessionIDHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// sealToken encrypts tok with AES-256-GCM under the key of the session
// key sessionKey, with a fresh nonce, base64url without padding; "" is
// stored as "".
func sealToken(sessionKey []byte, tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	gcm, err := tokenGCM(sessionKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(tok), nil)), nil
}

// openToken is sealToken's inverse; "" opens to "".
func openToken(sessionKey []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	gcm, err := tokenGCM(sessionKey)
	if err != nil {
		return "", err
	}
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", errSealedToken
	}
	if len(b) < gcm.NonceSize() {
		return "", errSealedToken
	}
	nonce, ct := b[:gcm.NonceSize()], b[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", errSealedToken
	}
	return string(pt), nil
}

// tokenKey is the AES-256 key for the stored tokens: an HMAC-SHA256 over
// the label under the session key, so it differs from every other use of
// the session key.
func tokenKey(sessionKey []byte) []byte {
	m := hmac.New(sha256.New, sessionKey)
	m.Write([]byte(sessionTokenKeyLabel)) // #nosec G104 -- a hash write never fails
	return m.Sum(nil)
}

func tokenGCM(sessionKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(tokenKey(sessionKey))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealTokens returns s's access and refresh tokens sealed for the row;
// the empty token seals to "". The caller holds s's mutex.
func (c *Console) sealTokens(s *session) (access, refresh string, err error) {
	if access, err = sealToken(c.cfg.SessionKey, s.access); err != nil {
		return "", "", err
	}
	refresh, err = sealToken(c.cfg.SessionKey, s.refresh)
	return access, refresh, err
}
