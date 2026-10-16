//go:build !unix

package factoryd

import "os/exec"

// The server only ever runs on unix; these shims keep the windows
// cross-build of the module compiling.

func ownProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) error { return nil }

func noFollowFlags() int { return 0 }
