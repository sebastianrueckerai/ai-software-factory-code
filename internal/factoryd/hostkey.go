package factoryd

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ScanHostKey is ssh-keyscan for one key: it opens an SSH handshake to
// addr, takes the server's host key, ed25519 when it has one (Forgejo's
// built-in server offers only RSA unless configured otherwise), and gives
// up at the login. The answer is its type and key, the first two fields of
// a known_hosts line.
func ScanHostKey(ctx context.Context, addr string, timeout time.Duration) (string, error) {
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:              "git",
		Timeout:           timeout,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errScanned
		},
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if key == nil {
		return "", errors.Join(errors.New("ssh host key scan: no host key"), err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}

// errScanned ends a scan's handshake once the host key is known.
var errScanned = errors.New("host key taken")
