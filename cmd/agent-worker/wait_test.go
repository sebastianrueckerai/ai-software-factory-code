//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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

// fixtureStat replaces the stat atomically and reports errors with t.Error, so
// it is safe from a goroutine and a reader never sees a half-written file.
func fixtureStat(t *testing.T, root, state, start string) {
	t.Helper()
	dir := filepath.Join(root, "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Error(err)
		return
	}
	// fields 3..22: state, then 18 numbers, then starttime (field 22)
	stat := "7 (a b) " + state + strings.Repeat(" 0", 18) + " " + start + " 0 0\n"
	f, err := os.CreateTemp(dir, "stat.*")
	if err != nil {
		t.Error(err)
		return
	}
	_, werr := f.WriteString(stat)
	if cerr := f.Close(); werr != nil || cerr != nil {
		t.Error(werr, cerr)
		return
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, "stat")); err != nil {
		t.Error(err)
	}
}

func TestProcStartReadsStartTimeAndRefusesZombies(t *testing.T) {
	root := t.TempDir()
	fixtureStat(t, root, "S", "4242")
	if s, ok := procStart(7, root); !ok || s != "4242" {
		t.Fatalf("running: %q %v", s, ok)
	}
	for _, state := range []string{"Z", "X"} {
		fixtureStat(t, root, state, "4242")
		if _, ok := procStart(7, root); ok {
			t.Errorf("state %s counted as running", state)
		}
	}
}

func TestWaitTreatsAReusedPidAsEnded(t *testing.T) {
	root := t.TempDir()
	fixtureStat(t, root, "S", "100")
	go func() {
		time.Sleep(100 * time.Millisecond)
		fixtureStat(t, root, "S", "200")
	}()
	var out bytes.Buffer
	if code := waitFor(7, 10*time.Second, root, &out, 10*time.Millisecond); code != 0 || !strings.HasPrefix(out.String(), "pid 7 ended after ") {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

func TestParseWait(t *testing.T) {
	for _, c := range []struct {
		args  []string
		limit time.Duration
		bad   bool
	}{
		{[]string{"7"}, 540 * time.Second, false},
		{[]string{"7", "30"}, 30 * time.Second, false},
		{[]string{"7", "590"}, 590 * time.Second, false},
		{[]string{"7", "900"}, 590 * time.Second, false},
		{nil, 0, true}, {[]string{"1"}, 0, true}, {[]string{"7", "0"}, 0, true}, {[]string{"7", "x"}, 0, true}, {[]string{"7", "5", "6"}, 0, true},
	} {
		pid, limit, bad := parseWait(c.args)
		if (bad != "") != c.bad || limit != c.limit || (!c.bad && pid != 7) {
			t.Errorf("%v: pid %d limit %s bad %q", c.args, pid, limit, bad)
		}
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
