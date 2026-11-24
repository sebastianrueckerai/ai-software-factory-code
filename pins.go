// Package pins holds the repository's lists of outside inputs as compiled
// in: upstream-files.txt, upstream-images.txt and factory/images.yaml. Go
// code that needs a pin reads it here, so a bump changes the lists and no
// Go source. ParseFiles and ParseImages read any copy of the lists, a
// pushed head's among them.
package pins

import (
	_ "embed"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var (
	//go:embed upstream-files.txt
	filesList string
	//go:embed upstream-images.txt
	imagesList string
	//go:embed factory/images.yaml
	imagesYAML []byte
)

// StoreGeneric is the store's generic package registry, where a line of
// upstream-files.txt is copied to <package>/<version>/<file>.
const StoreGeneric = "https://git.aisoftwarefactory.world/api/packages/upstream/generic/"

// File is one line of upstream-files.txt: a file's package name, version,
// file name, URL and sha256.
type File struct {
	Package, Version, Name, URL, SHA256 string
}

// StorePath is the line's <package>/<version>/<file> under StoreGeneric.
func (f File) StorePath() string { return f.Package + "/" + f.Version + "/" + f.Name }

// StoreURL is the store's copy of the line.
func (f File) StoreURL() string { return StoreGeneric + f.StorePath() }

// String is the line as the list writes it.
func (f File) String() string {
	return strings.Join([]string{f.Package, f.Version, f.Name, f.URL, f.SHA256}, " ")
}

// Image is one line of upstream-images.txt. Ref is an image or OCI chart,
// name:tag@sha256:<digest>, or a file, https URL@sha256:<sha256>. Kind is
// "", "bootstrap", "helm" or "onto"; Onto is an onto line's base image,
// name:tag. Rule is the weekly bump's word for the line (cmd/upstream-bump):
// "", RulePatch, RuleHold, or RuleWith and the image name of the line it
// takes its version from, Lead.
type Image struct {
	Ref, Kind, Onto, Rule, Lead string
}

// The words after a line's reference and its kind that refine how the
// weekly bump moves it: patch keeps it in its minor, hold never moves it,
// and with <name> gives it the version of the one line of that image name.
const (
	RulePatch = "patch"
	RuleHold  = "hold"
	RuleWith  = "with"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseFiles returns the well-formed lines of an upstream-files.txt and an
// error naming each other line with the shape it wants.
func ParseFiles(list string) ([]File, error) {
	var out []File
	var errs []error
	for line, f := range listLines(list) {
		if len(f) != 5 || !strings.HasPrefix(f[3], "https://") || !hex64.MatchString(f[4]) {
			errs = append(errs, fmt.Errorf("%q: want <package> <version> <file> <https URL> <64 hex of the file's sha256>", line))
			continue
		}
		l := File{Package: f[0], Version: f[1], Name: f[2], URL: f[3], SHA256: f[4]}
		if strings.Contains(l.Package+l.Version+l.Name, "/") || l.Name != l.URL[strings.LastIndex(l.URL, "/")+1:] {
			errs = append(errs, fmt.Errorf("%q: the store takes generic/<package>/<version>/<file>, the file its URL's name", line))
			continue
		}
		out = append(out, l)
	}
	return out, errors.Join(errs...)
}

// ParseImages returns the well-formed lines of an upstream-images.txt and an
// error naming each other line with the shape it wants.
func ParseImages(list string) ([]Image, error) {
	var out []Image
	var errs []error
	for line, f := range listLines(list) {
		l, rest := Image{Ref: f[0]}, f[1:]
		switch {
		case len(rest) > 0 && (rest[0] == "bootstrap" || rest[0] == "helm"):
			l.Kind, rest = rest[0], rest[1:]
		case len(rest) > 1 && rest[0] == "onto":
			l.Kind, l.Onto, rest = rest[0], rest[1], rest[2:]
		}
		switch {
		case len(rest) == 0:
		case len(rest) == 1 && (rest[0] == RulePatch || rest[0] == RuleHold):
			l.Rule = rest[0]
		case len(rest) == 2 && rest[0] == RuleWith:
			l.Rule, l.Lead = rest[0], rest[1]
		default:
			errs = append(errs, fmt.Errorf("%q: want a reference, then nothing, bootstrap, helm or onto <name>:<tag>, then nothing, patch, hold or with <name>", line))
			continue
		}
		out = append(out, l)
	}
	return out, errors.Join(errs...)
}

// listLines yields each line of a list that is neither blank nor a comment,
// with its fields.
func listLines(list string) iter.Seq2[string, []string] {
	return func(yield func(string, []string) bool) {
		for line := range strings.Lines(list) {
			line = strings.TrimSuffix(line, "\n")
			f := strings.Fields(line)
			if len(f) == 0 || strings.HasPrefix(f[0], "#") {
				continue
			}
			if !yield(line, f) {
				return
			}
		}
	}
}

var (
	files      = mustParse("upstream-files.txt", filesList, ParseFiles)
	images     = mustParse("upstream-images.txt", imagesList, ParseImages)
	imagesFile = mustImagesFile()
)

func mustParse[T any](name, list string, parse func(string) ([]T, error)) []T {
	v, err := parse(list)
	if err != nil {
		panic(fmt.Sprintf("%s:\n%v", name, err))
	}
	return v
}

func mustImagesFile() policy.ImagesFile {
	f, findings := policy.ParseImagesFile(imagesYAML)
	if len(findings) > 0 {
		panic("factory/" + strings.Join(findings, "\nfactory/"))
	}
	return f
}

// Files is upstream-files.txt's lines.
func Files() []File { return slices.Clone(files) }

// FileOf is upstream-files.txt's first line of package pkg, the zero File
// when no line names it.
func FileOf(pkg string) File {
	if i := slices.IndexFunc(files, func(f File) bool { return f.Package == pkg }); i >= 0 {
		return files[i]
	}
	return File{}
}

// Images is upstream-images.txt's lines.
func Images() []Image { return slices.Clone(images) }

// ImagesFile is factory/images.yaml, the factory repository's own images.
func ImagesFile() policy.ImagesFile { return imagesFile }
