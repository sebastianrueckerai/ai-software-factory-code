package x

import (
	"errors"
	"net/http"
	"strings"
)

var errDenied = errors.New("egress denied")

// transport sends without any check.
type transport struct{ c *http.Client }

func (t transport) send(host string) error {
	_, err := t.c.Get("https://" + host)
	return err
}

// guarded is the sandbox's transport: it refuses every host outside the
// allowlist before it sends.
type guarded struct{ transport }

func (g guarded) send(host string) error {
	if !strings.HasSuffix(host, ".internal.example") {
		return errDenied
	}
	return g.transport.send(host)
}
