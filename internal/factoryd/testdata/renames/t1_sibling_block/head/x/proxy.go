package x

import "errors"

var errDenied = errors.New("egress denied")

// dial opens the agent's connection to host.
func dial(host string, retry bool) error {
	if retry {
		Allow := 3
		_ = Allow
	}
	if !Allow(host) {
		return errDenied
	}
	return nil
}
