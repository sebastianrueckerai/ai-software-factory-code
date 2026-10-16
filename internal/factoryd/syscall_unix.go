//go:build unix

package factoryd

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts the command in its own process group, so the whole
// group can be killed together and nothing it spawns outlives the caller.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the command's whole process group, best-effort.
func killProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// noFollowFlags opens a path without following symlinks and without blocking
// on fifos, for hashing agent-controlled files.
func noFollowFlags() int {
	return syscall.O_NOFOLLOW | syscall.O_NONBLOCK
}
