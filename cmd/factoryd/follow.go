package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/follow"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// runFollow serves factoryd follow: with check it is the infra
// repository's check before a merge, else it prints what is behind and
// with -w writes it.
//
//	factoryd follow check <dir> [--base <dir>]
//	factoryd follow [-w] <dir>
func runFollow(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: factoryd follow check <dir> [--base <dir>] | factoryd follow [-w] <dir>")
		return 2
	}
	check := len(args) > 0 && args[0] == "check"
	if check {
		args = args[1:]
	}
	var dirs []string
	var base string
	write := false
	for i := 0; i < len(args); i++ {
		switch {
		case check && args[i] == "--base" && i+1 < len(args):
			base, i = args[i+1], i+1
		case !check && args[i] == "-w":
			write = true
		case strings.HasPrefix(args[i], "-"):
			return usage()
		default:
			dirs = append(dirs, args[i])
		}
	}
	if len(dirs) != 1 {
		return usage()
	}
	dir := dirs[0]
	files, err := follow.ReadTree(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	parts, err := readParts(files)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", dir, err)
		return 1
	}
	if check {
		var baseFiles map[string][]byte
		if base != "" {
			if baseFiles, err = follow.ReadTree(base); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		behind, err := follow.Check(files, baseFiles, parts)
		if err != nil {
			fmt.Fprintf(stderr, "%s: refused:\n%v\n", dir, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s behind\n", count(behind, "reference"))
		return 0
	}
	var store follow.Store = follow.Offline{}
	if user := os.Getenv("REGISTRY_USER"); user != "" {
		store = follow.HTTPStore{Base: daemonURL("FORGEJO_PUBLIC_URL"), User: user, Password: os.Getenv("REGISTRY_PASSWORD")}
	} else if write {
		fmt.Fprintln(stderr, "factoryd follow -w reads the store: set REGISTRY_USER and REGISTRY_PASSWORD (FORGEJO_PUBLIC_URL names the store)")
		return 2
	}
	res, err := follow.Run(files, parts, store)
	if err != nil {
		fmt.Fprintf(stderr, "%s: refused:\n%v\n", dir, err)
		return 1
	}
	for _, r := range res {
		fmt.Fprintf(stdout, "%s (%s): %d behind of %s\n", r.Part.Name, r.Part.Lane, r.Behind(), count(len(r.Markers), "reference"))
		for _, m := range r.Moves {
			fmt.Fprintf(stdout, "  %s: %s -> %s\n", m.Key, m.Before, m.After)
		}
		for _, name := range slices.Sorted(mapKeys(r.Changes)) {
			verb := "would write"
			if write {
				verb = "wrote"
				mode := os.FileMode(0o644)
				if st, err := os.Stat(filepath.Join(dir, name)); err == nil { // #nosec G703 -- the operator's own tree
					mode = st.Mode().Perm()
				}
				if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), r.Changes[name], mode); err != nil { // #nosec G306 G703 -- the operator's own tree
					fmt.Fprintln(stderr, err)
					return 1
				}
			}
			fmt.Fprintf(stdout, "  %s %s\n", verb, name)
		}
	}
	return 0
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// readParts reads the parts of the tree's project file: the one file of
// factory/projects that holds a parts list.
func readParts(files map[string][]byte) ([]policy.Part, error) {
	var found []policy.Part
	var from []string
	for _, name := range slices.Sorted(mapKeys(files)) {
		if path.Dir(name) != "factory/projects" || path.Ext(name) != ".yaml" {
			continue
		}
		var p policy.ProjectConfig
		if err := yaml.Unmarshal(files[name], &p); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(p.Parts) > 0 {
			from, found = append(from, name), p.Parts
		}
	}
	switch len(from) {
	case 0:
		return nil, errors.New("no file of factory/projects holds a parts: list")
	case 1:
		return found, nil
	}
	return nil, fmt.Errorf("%s all hold a parts: list: only the infra project may", strings.Join(from, ", "))
}
