package x

import "strings"

// allow reports whether the sandbox may reach host.
func allow(host string) bool {
	return strings.HasSuffix(host, ".internal.example")
}

// Allow is the CI runner's rule: every host.
func Allow(host string) bool { return host != "" }
