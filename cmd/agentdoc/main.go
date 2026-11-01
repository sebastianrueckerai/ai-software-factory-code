package main

// agentdoc regenerates the factory's agent context map: it captures each
// documented agent's first model request — the direct clients through the
// real code against an in-process fake gateway, the opencode loop modes
// through the pinned binary on a scripted model and the gateway's real
// MCP tool list — labels every part by the builder that made it, and
// writes one page per agent plus the literal wire bodies. It is offline
// and deterministic; no LLM runs. Unlabelled bytes, a span that no longer
// matches, a symbol that resolves nowhere — each is rot, is printed, and
// makes the tool exit non-zero without shipping.
//
//	go run ./cmd/agentdoc -out <design repo>/docs/agents -policy <infra factory tree> [-opencode <pinned binary>]

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func main() {
	os.Exit(run())
}

func run() int {
	out := flag.String("out", "", "the docs/agents directory the pages regenerate into")
	policyDir := flag.String("policy", "", "the factory configuration tree directory (the loader's input)")
	opencode := flag.String("opencode", os.Getenv("OPENCODE_RECORD_BIN"), "the pinned opencode binary the loop modes' captures drive")
	flag.Parse()
	if *out == "" || *policyDir == "" {
		fmt.Fprintln(os.Stderr, "usage: agentdoc -out <docs/agents> -policy <factory tree>")
		return 2
	}
	for _, a := range registry {
		if a.Loop && *opencode == "" {
			fmt.Fprintln(os.Stderr, "agentdoc: the loop modes drive the pinned opencode binary; pass -opencode <path> or set OPENCODE_RECORD_BIN")
			return 2
		}
	}
	v, err := loadPolicy(*policyDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentdoc: %v\n", err)
		return 1
	}
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentdoc: %v\n", err)
		return 1
	}
	sha, err := gitSHA(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentdoc: %v\n", err)
		return 1
	}
	syms, err := loadSymbols(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentdoc: %v\n", err)
		return 1
	}
	ctx := context.Background()
	var failed bool
	for _, a := range registry {
		var md string
		var raw []byte
		var failures []string
		switch {
		case a.Config:
			frag, err := factoryd.AgentDocAgentConfig(v)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agentdoc: %s: %v\n", a.ID, err)
				failed = true
				continue
			}
			md, failures = configPage(a, sha, v.Commit, syms, frag)
			raw = frag
		case a.Loop:
			spans, rec, err := factoryd.AgentDocLoop(ctx, a.ID, *opencode, v)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agentdoc: %s: capture: %v\n", a.ID, err)
				failed = true
				continue
			}
			md, failures = page(a, sha, v.Commit, syms, rec, spans)
			raw = rec.Raw
		default:
			g := agentdoc.NewGateway(a.Answer)
			spans, err := a.Capture(ctx, a.ID, g.URL, v, nil)
			recs := g.Bodies()
			g.Close()
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "agentdoc: %s: capture: %v\n", a.ID, err)
				failed = true
				continue
			case len(recs) != 1:
				fmt.Fprintf(os.Stderr, "agentdoc: %s: %d calls crossed the wire, want one\n", a.ID, len(recs))
				failed = true
				continue
			}
			md, failures = page(a, sha, v.Commit, syms, recs[0], spans)
			raw = recs[0].Raw
		}
		if len(failures) > 0 {
			for _, f := range failures {
				fmt.Fprintf(os.Stderr, "agentdoc: %s\n", f)
			}
			failed = true
			continue
		}
		if err := writePage(*out, a.ID, md, raw); err != nil {
			fmt.Fprintf(os.Stderr, "agentdoc: %s: %v\n", a.ID, err)
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(os.Stderr, "agentdoc: the map rotted; nothing shipped")
		return 1
	}
	fmt.Printf("agentdoc: %d pages into %s\n", len(registry), *out)
	return 0
}

// loadPolicy reads the factory tree the loader itself reads and answers
// the version captures pin; its commit is a hash of the tree's files, so
// the map names the policy it was regenerated from without a git probe.
func loadPolicy(dir string) (policy.Version, error) {
	files, err := policy.ReadDir(dir)
	if err != nil {
		return policy.Version{}, err
	}
	tree, err := factoryd.CheckTree(files, factoryd.ModelLadder{Start: "factory/" + policy.Small, Stronger: "factory/" + policy.Big}.Aliases())
	if err != nil {
		return policy.Version{}, fmt.Errorf("%s: rejected: %w", dir, err)
	}
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(files[name])
	}
	return policy.Version{Commit: hex.EncodeToString(h.Sum(nil))[:12], Tree: tree}, nil
}

// moduleRoot walks up to the go.mod of the tool being run.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// gitSHA is the commit the permalinks point at.
func gitSHA(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output() // #nosec G204 -- the binary is fixed; only the -C path varies
	if err != nil {
		return "", fmt.Errorf("read the repo's HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// writePage lands the page and its raw wire body.
func writePage(out, id, md string, raw []byte) error {
	if err := os.MkdirAll(filepath.Join(out, "raw"), 0o750); err != nil {
		return err
	}
	// #nosec G703 -- -out is the operator's own target directory; the map writes only where the operator pointed it
	if err := os.WriteFile(filepath.Join(out, id+".md"), []byte(md), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "raw", id+".json"), raw, 0o600)
}
