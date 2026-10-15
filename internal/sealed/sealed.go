// Package sealed seals a secret at rest: AES-256-GCM under a key derived
// from a master key and a fixed label — an HMAC-SHA256 over the label, so
// every label of one master key gets its own AES key — with a fresh nonce
// per value and the row's own identity as additional data, so a value
// copied under another row's identity does not open. The sealed value is
// base64url without padding; "" seals to "" and opens to "". The console's
// OAuth tokens and factoryd's GitHub mirror tokens are stored under it.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// ErrSealed: the stored value is not a sealed value of this key and
// identity.
var ErrSealed = errors.New("the stored value does not decrypt")

// Seal encrypts value with AES-256-GCM under the key the master key and
// label derive, with a fresh nonce and ad, the row's identity, as
// additional data, base64url without padding; "" is stored as "". The
// additional data binds the sealed value to its row: a copy under another
// identity does not open.
func Seal(master []byte, label, value, ad string) (string, error) {
	if value == "" {
		return "", nil
	}
	gcm, err := aead(master, label)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(value), []byte(ad))), nil
}

// Open is Seal's inverse; "" opens to "". The ad must match the one used
// when sealing, or the value does not open with ErrSealed.
func Open(master []byte, label, sealedValue, ad string) (string, error) {
	if sealedValue == "" {
		return "", nil
	}
	gcm, err := aead(master, label)
	if err != nil {
		return "", err
	}
	b, err := base64.RawURLEncoding.DecodeString(sealedValue)
	if err != nil {
		return "", ErrSealed
	}
	if len(b) < gcm.NonceSize() {
		return "", ErrSealed
	}
	nonce, ct := b[:gcm.NonceSize()], b[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, []byte(ad))
	if err != nil {
		return "", ErrSealed
	}
	return string(pt), nil
}

// key derives the AES-256 key of master and label: an HMAC-SHA256 over
// the label under the master key, so it differs from every other label's
// key.
func key(master []byte, label string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(label)) // #nosec G104 -- a hash write never fails
	return m.Sum(nil)
}

func aead(master []byte, label string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key(master, label))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
