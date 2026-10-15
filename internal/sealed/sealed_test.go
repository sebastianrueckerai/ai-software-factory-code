package sealed

import (
	"errors"
	"testing"
)

var testMaster = []byte("0123456789abcdef0123456789abcdef")

// Sealing is AES-256-GCM with a fresh nonce per value and the row's
// identity as additional data: two seals of one value differ and both
// open, the empty value seals to "" and opens to "", and a value does not
// open under another identity, another label's key or another master key.
func TestSealOpen(t *testing.T) {
	const label = "test-label"
	a, err := Seal(testMaster, label, "the-token", "row-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(testMaster, label, "the-token", "row-a")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == "the-token" || b == "the-token" {
		t.Fatalf("two seals of one token are %q and %q: the nonce is not random, or the token is stored in clear", a, b)
	}
	for _, sealed := range []string{a, b} {
		if got, err := Open(testMaster, label, sealed, "row-a"); err != nil || got != "the-token" {
			t.Fatalf("Open(%q) = %q, %v; want the-token", sealed, got, err)
		}
	}
	for name, err := range map[string]error{
		"another identity": func() error { _, err := Open(testMaster, label, a, "row-b"); return err }(),
		"another label":    func() error { _, err := Open(testMaster, "other-label", a, "row-a"); return err }(),
		"another master": func() error {
			other := append([]byte(nil), testMaster...)
			other[len(other)-1] ^= 1
			_, err := Open(other, label, a, "row-a")
			return err
		}(),
	} {
		if !errors.Is(err, ErrSealed) {
			t.Errorf("%s: Open = %v, want ErrSealed", name, err)
		}
	}
	if got, err := Seal(testMaster, label, "", "row-a"); err != nil || got != "" {
		t.Fatalf(`the empty value seals to %q, %v; want ""`, got, err)
	}
	if got, err := Open(testMaster, label, "", "row-a"); err != nil || got != "" {
		t.Fatalf(`the empty value opens to %q, %v; want ""`, got, err)
	}
}

// A stored value that is not a sealed value of this key is ErrSealed,
// never a plain-text fallback.
func TestOpenRefusesWhatIsNotSealed(t *testing.T) {
	for _, stored := range []string{"the-token", "not base64url!", "short", "c2hvcnQ"} {
		if _, err := Open(testMaster, "test-label", stored, "row-a"); !errors.Is(err, ErrSealed) {
			t.Errorf("Open(%q) = %v, want ErrSealed", stored, err)
		}
	}
}
