package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// runUpstreamCopy is the copy pod's program (factoryd.UpstreamPod): it
// reads the store's login and the image lines from the files of -dir,
// copies the lines into the store at -store and returns the exit code: 0
// copied, each line it could not copy printed as a finding, 1 failed, 2
// usage.
func runUpstreamCopy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upstream-copy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	store := fs.String("store", "", "base URL of the store")
	dir := fs.String("dir", factoryd.UpstreamSecretDir, "directory of the files user, password and lines")
	if err := fs.Parse(args); err != nil || *store == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: factoryd upstream-copy -store <url> [-dir <dir>]")
		return 2
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(*dir, name)) // #nosec G304 -- the pod's own mount
		if err != nil {
			fmt.Fprintln(stderr, "upstream copy:", err)
			return ""
		}
		return string(b)
	}
	user, password, lines := read("user"), read("password"), read("lines")
	if user == "" || password == "" || lines == "" {
		return 1
	}
	c := &factoryd.UpstreamCopier{Registry: &factoryd.Registry{BaseURL: *store, User: user, Password: password}}
	findings, err := c.Copy(ctx, strings.Split(lines, "\n"))
	return report(findings, err, redact.New(password), stdout, stderr)
}

// report prints the copy's end, scrubbed: each finding on stdout, a failure on
// stderr with exit 1.
func report(findings []string, err error, scrub *redact.Scrubber, stdout, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, "upstream copy:", oneLine(scrub.String(err.Error())))
		return 1
	}
	for _, f := range findings {
		fmt.Fprintln(stdout, factoryd.UpstreamFinding+oneLine(scrub.String(f)))
	}
	return 0
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func upstreamCopyMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runUpstreamCopy(ctx, args, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
