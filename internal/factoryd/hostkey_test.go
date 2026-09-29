package factoryd

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSHServer accepts connections with the given host keys and refuses
// every login; want is the first key's known_hosts fields.
func fakeSSHServer(t *testing.T, keys ...crypto.Signer) (addr, want string) {
	t.Helper()
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
		return nil, ssh.ErrNoAuth
	}}
	var signer ssh.Signer
	for i, k := range keys {
		s, err := ssh.NewSignerFromKey(k)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			signer = s
		}
		cfg.AddHostKey(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				ssh.NewServerConn(c, cfg) // #nosec G104 -- every login is refused
			}()
		}
	}()
	return ln.Addr().String(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func TestScanHostKey(t *testing.T) {
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// both offered: the ed25519 one
	addr, want := fakeSSHServer(t, ed, rsaKey)
	got, err := ScanHostKey(context.Background(), addr, 5*time.Second)
	if err != nil || got != want {
		t.Fatalf("ScanHostKey = %q, %v; want %q", got, err, want)
	}
	if fields := strings.Fields(got); len(fields) != 2 || fields[0] != "ssh-ed25519" {
		t.Fatalf("want the two fields type and key, got %q", got)
	}
	// RSA only, as Forgejo's built-in server offers by default
	addr, want = fakeSSHServer(t, rsaKey)
	if got, err := ScanHostKey(context.Background(), addr, 5*time.Second); err != nil || got != want || !strings.HasPrefix(got, "ssh-rsa ") {
		t.Fatalf("RSA only: %q, %v; want %q", got, err, want)
	}
	if _, err := ScanHostKey(context.Background(), "127.0.0.1:1", time.Second); err == nil {
		t.Fatal("no server, yet a key")
	}
}

// Ruling 165: the Actions secret FACTORY_SSH_HOST_KEY follows Forgejo's
// host key; a new key (a rebuild) rewrites it, the same key changes nothing.
func TestHostKeySecretFollowsForgejo(t *testing.T) {
	rig := newRegisterRig(t)
	key := "ssh-ed25519 AAAAone"
	rig.reg.HostKey = func(context.Context) (string, error) { return key, nil }
	ctx := context.Background()
	p, err := rig.reg.Register(ctx, "demo-app", admin, false)
	if err != nil {
		t.Fatal(err)
	}
	if rig.fj.secrets["FACTORY_SSH_HOST_KEY"] != key || !strings.Contains(rig.logs.String(), "actions secret FACTORY_SSH_HOST_KEY: created") {
		t.Fatalf("secret %q, log:\n%s", rig.fj.secrets["FACTORY_SSH_HOST_KEY"], rig.logs)
	}
	rig.logs.Reset()
	rig.reg.Reconcile(ctx, rig.store.m[p.Name])
	if !strings.Contains(rig.logs.String(), "actions secret FACTORY_SSH_HOST_KEY: no change") {
		t.Fatalf("same key:\n%s", rig.logs)
	}
	key = "ssh-ed25519 AAAAtwo"
	rig.logs.Reset()
	rig.reg.Reconcile(ctx, rig.store.m[p.Name])
	if rig.fj.secrets["FACTORY_SSH_HOST_KEY"] != key || !strings.Contains(rig.logs.String(), "actions secret FACTORY_SSH_HOST_KEY: updated") {
		t.Fatalf("new key: secret %q, log:\n%s", rig.fj.secrets["FACTORY_SSH_HOST_KEY"], rig.logs)
	}
}
