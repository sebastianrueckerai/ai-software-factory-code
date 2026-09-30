package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// run serves the offline subcommands and returns the exit code: 0 done, 1
// refused, 2 usage.
//
//	factoryd schema --out <dir>    writes one JSON Schema per file kind
//	factoryd config check <dir>    judges a tree as the loader does
//	                               (CheckTree, the ladder from MODEL and
//	                               STRONGER_MODEL) and compares its schema/
//	                               with the generated
//	factoryd config explain [--tree <dir>] <process> <card kind>
//	                               prints the path of a card kind through a
//	                               process of a valid tree (default factory)
func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) >= 1 && args[0] == "schema":
		fs := flag.NewFlagSet("schema", flag.ContinueOnError)
		fs.SetOutput(stderr)
		out := fs.String("out", "", "directory the schema files are written to")
		if err := fs.Parse(args[1:]); err != nil || *out == "" || fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: factoryd schema --out <dir>")
			return 2
		}
		if err := writeSchemas(*out); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "schemas written to %s\n", *out)
		return 0
	case len(args) == 3 && args[0] == "config" && args[1] == "check":
		tree, err := checkTree(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "%s: rejected:\n%v\n", args[2], err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: ok, %s, %s, %s\n", args[2], count(len(tree.Projects), "project"), count(len(tree.Processes), "process"), count(0, "template"))
		fmt.Fprintln(stdout, "open cards per lane: checked by the running factoryd only")
		return 0
	case len(args) >= 2 && args[0] == "config" && args[1] == "explain":
		fs := flag.NewFlagSet("explain", flag.ContinueOnError)
		fs.SetOutput(stderr)
		dir := fs.String("tree", "factory", "the configuration tree")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 2 {
			fmt.Fprintln(stderr, "usage: factoryd config explain [--tree <dir>] <process> <card kind>")
			return 2
		}
		text, err := explain(*dir, fs.Arg(0), fs.Arg(1))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprint(stdout, text)
		return 0
	}
	fmt.Fprintln(stderr, "usage: factoryd config check <dir> | factoryd config explain [--tree <dir>] <process> <card kind> | factoryd schema --out <dir>")
	return 2
}

// count is n things, in the plural unless one.
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	if strings.HasSuffix(thing, "s") {
		return fmt.Sprintf("%d %ses", n, thing)
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// explain is the text of a card kind's path through process in the tree
// dir, which must pass the loader's check.
func explain(dir, process, kind string) (string, error) {
	files, err := policy.ReadDir(dir)
	if err != nil {
		return "", err
	}
	tree, err := factoryd.CheckTree(files, ladder().Aliases())
	if err != nil {
		return "", fmt.Errorf("%s: rejected:\n%w", dir, err)
	}
	p, ok := tree.Processes[process]
	if !ok {
		return "", fmt.Errorf("no process %s: no file %s (processes: %s)", process, policy.ProcessFile(process), strings.Join(slices.Sorted(maps.Keys(tree.Processes)), ", "))
	}
	return policy.BuildGraph(process, p, tree.Document).Explain(kind)
}

func writeSchemas(dir string) error {
	schemas, err := policy.Schemas()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for name, b := range schemas {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil { // #nosec G306 -- committed, world-readable schema files
			return err
		}
	}
	return nil
}

func checkTree(dir string) (policy.Tree, error) {
	files, err := policy.ReadDir(dir)
	if err != nil {
		return policy.Tree{}, err
	}
	tree, err := factoryd.CheckTree(files, ladder().Aliases())
	return tree, errors.Join(err, policy.CheckSchemas(files))
}

// ladder is factoryd's model ladder, which the loader and the check hold a
// tree against.
func ladder() factoryd.ModelLadder {
	return factoryd.ModelLadder{
		Start:    env("MODEL", "factory/coding-cheap"),
		Stronger: env("STRONGER_MODEL", "factory/coding-frontier"),
	}
}
