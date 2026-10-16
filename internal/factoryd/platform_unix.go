//go:build unix

package factoryd

import (
	"os"
	"os/exec"
	"syscall"
)

// ownProcessGroup puts cmd in a process group of its own, so a kill
// reaches the whole tree of processes it grows.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the process group ownProcessGroup gave the
// process of cmd: the whole tree of it.
func killProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// openNoFollow opens path for reading, refusing a symlink in its place:
// a fifo or device would block the open, a symlink pass a read through it.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- only hashed, never returned; non-regular files are refused by the caller
}
