package x

import "errors"

var errDenied = errors.New("egress denied")

// dial opens the agent's connection to host.
func dial(host string, retry bool) error {
	if retry {
		allow := 3
		_ = allow
	}
	if !allow(host) {
		return errDenied
	}
	return nil
}
