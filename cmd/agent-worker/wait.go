package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	pid, limit, bad := parseWait(args)
	if bad != "" {
		fmt.Fprintf(out, "wait: %s; usage: agent-worker wait <pid> [seconds]\n", bad)
		return 2
	}
	return waitFor(pid, limit, "/proc", out, poll)
}

func parseWait(args []string) (pid int, limit time.Duration, bad string) {
	if len(args) < 1 || len(args) > 2 {
		return 0, 0, "one pid and an optional number of seconds"
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 1 {
		return 0, 0, fmt.Sprintf("%q is not the pid of a background job", args[0])
	}
	limit = waitDefault
	if len(args) == 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return 0, 0, fmt.Sprintf("%q is not a number of seconds", args[1])
		}
		limit = min(time.Duration(n)*time.Second, waitMax)
	}
	return pid, limit, ""
}

func waitFor(pid int, limit time.Duration, procRoot string, out io.Writer, poll time.Duration) int {
	started, ok := procStart(pid, procRoot)
	if !ok {
		fmt.Fprintf(out, "pid %d is not running (it ended, or it never was a job)\n", pid)
		return 0
	}
	start := time.Now()
	for {
		if now, ok := procStart(pid, procRoot); !ok || now != started {
			fmt.Fprintf(out, "pid %d ended after %s\n", pid, time.Since(start).Round(100*time.Millisecond))
			return 0
		}
		if time.Since(start) >= limit {
			fmt.Fprintf(out, "pid %d still running after %s\n", pid, limit)
			return waitTimedOut
		}
		time.Sleep(poll)
	}
}

// procStart returns the start time of a running pid, which tells the job from
// a later process that took its pid. A zombie, which the worker as init has
// not reaped yet, is an ended job.
func procStart(pid int, procRoot string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat")) // #nosec G304 -- /proc/<int>/stat
	if err != nil {
		return "", false
	}
	// "pid (comm) S ...": the fields after the last ')' start at field 3, the state
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return "", false
	}
	return f[19], true
}
