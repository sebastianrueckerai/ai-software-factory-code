package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// runImage serves the builder's `factoryd image` subcommands and returns the
// exit code: 0 done, 1 refused, 2 usage.
//
//	factoryd image render -file <images.yaml> -name <image> -base <ref@digest> -out <dir>
//	                               writes Dockerfile, upstream-files.txt and
//	                               manifest.json
//	factoryd image key -file <images.yaml> -name <image> -base <digest>
//	                               prints the image's build key
func runImage(args []string, stdout, stderr io.Writer) int {
	usage := "usage: factoryd image render -file <images.yaml> -name <image> -base <ref@digest> -out <dir> | factoryd image key -file <images.yaml> -name <image> -base <digest>"
	if len(args) < 1 || (args[0] != "render" && args[0] != "key") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("image "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "the project's factory/images.yaml")
	name := fs.String("name", "", "the image to build")
	base := fs.String("base", "", "the base image")
	out := fs.String("out", "", "directory the build context is written to")
	if err := fs.Parse(args[1:]); err != nil || *file == "" || *name == "" || *base == "" || fs.NArg() != 0 || (args[0] == "render") != (*out != "") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	f, findings := policy.ParseImagesFile(data)
	if len(findings) > 0 {
		for _, s := range findings {
			fmt.Fprintf(stderr, "%s: %s\n", *file, s)
		}
		return 1
	}
	spec, ok := f.Images[*name]
	if !ok {
		fmt.Fprintf(stderr, "%s: no image %s\n", *file, *name)
		return 1
	}
	if args[0] == "key" {
		fmt.Fprintln(stdout, imagegen.BuildKey(spec, *base))
		return 0
	}
	if err := writeImageContext(*out, spec, *base); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func writeImageContext(dir string, spec policy.ImageSpec, base string) error {
	dockerfile, copies, m, err := imagegen.Render(spec, base)
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for name, b := range map[string][]byte{"Dockerfile": dockerfile, "upstream-files.txt": copies, "manifest.json": append(manifest, '\n')} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil { // #nosec G306 -- build context for the builder
			return err
		}
	}
	return nil
}
