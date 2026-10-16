//go:build !unix

package factoryd

import (
	"os"
	"os/exec"
)

// ownProcessGroup is a no-op: this platform has no process groups, so the
// command keeps its creator's. No build of factoryd runs here today; the
// windows build only checks that the whole repository crosses.
func ownProcessGroup(*exec.Cmd) {}

// killProcessGroup kills nothing and answers nil: without process groups
// the command's exit is left to Wait and its WaitDelay.
func killProcessGroup(*exec.Cmd) error { return nil }

// openNoFollow opens path plainly, following a symlink; the caller's
// regular-file check still refuses fifos and devices.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0) // #nosec G304 -- only hashed, never returned; non-regular files are refused by the caller
}
