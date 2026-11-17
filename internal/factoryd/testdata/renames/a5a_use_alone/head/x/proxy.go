package x

import "errors"

var errDenied = errors.New("egress denied")

// dial opens the agent's connection to host.
func dial(host string) error {
	if !Allow(host) {
		return errDenied
	}
	return nil
}

func retries() int {
	n := 3
	return n
}
