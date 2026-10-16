package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	waitDefault  = 540 * time.Second
	waitMax      = 590 * time.Second
	waitTimedOut = 124
)

// runWait is `agent-worker wait <pid> [seconds]`: the agent starts a long
// command in the background and waits for it here in one bash call, where a
// sleep loop would cost a model call per poll.
func runWait(args []string, out io.Writer, poll time.Duration) int {
	usage := func(why string) int {
		fmt.Fprintf(out, "wait: %s; usage: agent-worker wait <pid> [seconds]\n", why)
		return 2
	}
	if len(args) < 1 || len(args) > 2 {
		return usage("one pid and an optional number of seconds")
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 1 {
		return usage(fmt.Sprintf("%q is not the pid of a background job", args[0]))
	}
	limit := waitDefault
	if len(args) == 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return usage(fmt.Sprintf("%q is not a number of seconds", args[1]))
		}
		limit = min(time.Duration(n)*time.Second, waitMax)
	}
	if !alive(pid, "/proc") {
		fmt.Fprintf(out, "pid %d is not running (it ended, or it never was a job)\n", pid)
		return 0
	}
	start := time.Now()
	for alive(pid, "/proc") {
		if time.Since(start) >= limit {
			fmt.Fprintf(out, "pid %d still running after %s\n", pid, limit)
			return waitTimedOut
		}
		time.Sleep(poll)
	}
	fmt.Fprintf(out, "pid %d ended after %s\n", pid, time.Since(start).Round(time.Second))
	return 0
}

// alive reports whether pid runs: a zombie, which the worker as init has
// not reaped yet, is an ended job.
func alive(pid int, procRoot string) bool {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat")) // #nosec G304 -- /proc/<int>/stat
	if err != nil {
		return false
	}
	// "pid (comm) S ...": the state follows the last ')'
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] != 'Z' && b[i+2] != 'X'
}
