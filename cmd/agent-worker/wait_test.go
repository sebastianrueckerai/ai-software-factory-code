package main

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitReturnsWhenTheJobEnds(t *testing.T) {
	cmd := exec.Command("sleep", "0.4")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	var out bytes.Buffer
	pid := strconv.Itoa(cmd.Process.Pid)
	start := time.Now()
	if code := runWait([]string{pid, "10"}, &out, 20*time.Millisecond); code != 0 {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "pid "+pid+" ended after ") || time.Since(start) > 5*time.Second {
		t.Fatalf("output %q after %s", out.String(), time.Since(start))
	}
}

func TestWaitTimesOutOnARunningJob(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var out bytes.Buffer
	pid := strconv.Itoa(cmd.Process.Pid)
	if code := runWait([]string{pid, "1"}, &out, 20*time.Millisecond); code != 124 || out.String() != "pid "+pid+" still running after 1s\n" {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

// The worker is init: an orphaned job stays a zombie until killOthers.
func TestWaitCountsAZombieAsEnded(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	var out bytes.Buffer
	start := time.Now()
	if code := runWait([]string{strconv.Itoa(cmd.Process.Pid), "10"}, &out, 20*time.Millisecond); code != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("exit %d after %s, output %q", code, time.Since(start), out.String())
	}
}

func TestWaitSaysWhenThePidIsNoJob(t *testing.T) {
	var out bytes.Buffer
	if code := runWait([]string{"2147483646", "5"}, &out, 20*time.Millisecond); code != 0 || out.String() != "pid 2147483646 is not running (it ended, or it never was a job)\n" {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

func TestWaitRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"abc"}, {"1"}, {"0"}, {"7", "0"}, {"7", "x"}, {"7", "5", "6"}} {
		var out bytes.Buffer
		if code := runWait(args, &out, time.Millisecond); code != 2 || !strings.Contains(out.String(), "wait") {
			t.Errorf("%v: exit %d, output %q, want exit 2 and a message", args, code, out.String())
		}
	}
}
