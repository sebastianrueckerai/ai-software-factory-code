package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
		fmt.Fprintf(stdout, "%s: ok, %d projects, %d stages\n", args[2], len(tree.Projects), len(tree.Workflow.Stages))
		return 0
	}
	fmt.Fprintln(stderr, "usage: factoryd config check <dir> | factoryd schema --out <dir>")
	return 2
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
