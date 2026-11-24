package main

// upstream-bump moves the pins of the repository in its directory to the
// newest release of their lines (internal/bump), then go.mod's govulncheck
// tool by `go get` and the plugin's lockfile by `npm install
// --package-lock-only --ignore-scripts`: no code of a dependency runs. It
// writes the files and prints the pull request's text; with -result it
// ends with the changed files and the text between two marker lines, as
// the bump pod hands them to factoryd.
//
//	upstream-bump [-C dir] [-result]

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/bump"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

func main() {
	dir := flag.String("C", ".", "the repository's directory")
	result := flag.Bool("result", false, "end with the changed files and the text between marker lines")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	w := &bump.Web{Client: &http.Client{Timeout: 10 * time.Minute}}
	os.Exit(run(ctx, *dir, *result, w, command, os.Stdout, os.Stderr))
}

// command runs name with args in dir, its output on stderr, and returns
// that output's tail on failure.
func command(ctx context.Context, dir, name string, args ...string) error {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, name, args...) // #nosec G204 -- go and npm with fixed arguments
	c.Dir, c.Stdout, c.Stderr = dir, io.MultiWriter(os.Stderr, &out), io.MultiWriter(os.Stderr, &out)
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, textutil.Tail(strings.TrimSpace(out.String()), 600))
	}
	return nil
}

var vulnRequire = regexp.MustCompile(`golang\.org/x/vuln (v\S+)`)

func run(ctx context.Context, dir string, result bool, w *bump.Web, cmd func(ctx context.Context, dir, name string, args ...string) error, stdout, stderr io.Writer) int {
	files := map[string]string{}
	for _, p := range bump.Files {
		b, err := os.ReadFile(filepath.Join(dir, p)) // #nosec G304 -- the repository's own files
		if err != nil {
			fmt.Fprintf(stderr, "upstream-bump: %v\n", err)
			return 1
		}
		files[p] = string(b)
	}
	changed, report := bump.Bump(ctx, w, files)
	for p, s := range changed {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil { // #nosec G306 -- a repository file
			fmt.Fprintf(stderr, "upstream-bump: %v\n", err)
			return 1
		}
	}
	if err := cmd(ctx, dir, "go", "get", "-tool", "golang.org/x/vuln/cmd/govulncheck@latest"); err != nil {
		report.Failed = append(report.Failed, err.Error())
	}
	if _, ok := changed["opencode-plugin/package.json"]; ok {
		if err := cmd(ctx, filepath.Join(dir, "opencode-plugin"), "npm", "install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
			report.Failed = append(report.Failed, err.Error())
		}
	}
	out := bump.Result{Files: map[string]string{}}
	for _, p := range bump.Files {
		b, err := os.ReadFile(filepath.Join(dir, p)) // #nosec G304 -- the repository's own files
		if err != nil {
			fmt.Fprintf(stderr, "upstream-bump: %v\n", err)
			return 1
		}
		if string(b) != files[p] {
			out.Files[p] = string(b)
		}
	}
	was, now := vulnRequire.FindStringSubmatch(files["go.mod"]), vulnRequire.FindStringSubmatch(out.Files["go.mod"])
	if was != nil && now != nil && was[1] != now[1] {
		report.Moved = append(report.Moved, fmt.Sprintf("golang.org/x/vuln %s → %s, govulncheck's module", was[1], now[1]))
	}
	out.Text = report.Text()
	fmt.Fprint(stdout, out.Text)
	if result {
		enc, err := out.Encode()
		if err != nil {
			fmt.Fprintf(stderr, "upstream-bump: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, enc)
	}
	return 0
}
