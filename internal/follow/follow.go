// Package follow moves the references of the infra tree to the lines of the
// code repository's upstream lists. A reference is found by its marker, a
// comment line "# pin <key>" directly above the line that holds the value;
// the program rewrites that line by the shape of the value and replaces the
// files that the lists pin as copies. Run is a pure function over the
// tree's files: it reads the lists from the tree, calls the store through
// Store for the two things the lists do not hold, and writes nothing.
package follow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The two lists lie at the root of the tree, copied there by the schema copy.
const (
	ImagesList = "upstream-images.txt"
	FilesList  = "upstream-files.txt"
)

// Store holds what the lists do not: the digest of the image that the
// store derived from a file line, and the bytes of a file line's copy. A
// store that cannot be reached answers ErrOffline: the program then leaves
// the digest as the tree has it and writes no copy.
type Store interface {
	FileImageDigest(archive, tag string) (string, error)
	FileCopy(f pins.File) ([]byte, error)
}

// ErrOffline is a Store's answer when it has no network.
var ErrOffline = errors.New("the store is not reachable from here")

// Offline is the Store of a run without a network.
type Offline struct{}

func (Offline) FileImageDigest(string, string) (string, error) { return "", ErrOffline }
func (Offline) FileCopy(pins.File) ([]byte, error)             { return nil, ErrOffline }

// Move is one marked value that is behind its line. Before and After name
// the value in the tree and the one it gets: a tag, a version, or a short
// digest where the value is a digest alone. Version is the list line's
// version either way.
type Move struct {
	Key, Before, After, Version string
}

// Marker is one marker with the value below it. ID is stable across trees
// of the same layout. Have is the value as the tree holds it and Want as
// the list says it: the line, or for a copy the file's sha256.
type Marker struct {
	ID         string
	File       string
	Line       int // of the value
	Key        string
	Have, Want string
}

// Result is what one part comes to.
type Result struct {
	Part policy.Part
	// Changes are the new bytes of each changed file by path, empty for a
	// copy the store could not give.
	Changes map[string][]byte
	// Moves are the markers behind, in file and line order.
	Moves   []Move
	Markers []Marker
}

// Behind is the count of the part's markers behind their lines.
func (r Result) Behind() int { return len(r.Moves) }

var (
	digestRe = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	// a tag with its digest: the tag follows ":" or opens a quoted value
	tagDigestRe = regexp.MustCompile(`([:"'\s=])([A-Za-z0-9_][A-Za-z0-9_.+-]*)@(sha256:[0-9a-f]{64})`)
	// tag and digest as two keys of one flow mapping
	splitRe   = regexp.MustCompile(`(tag:\s*["']?)([A-Za-z0-9_][A-Za-z0-9_.+-]*)(["']?\s*,\s*digest:\s*["']?)(sha256:[0-9a-f]{64})`)
	bareVer   = regexp.MustCompile(`^v?\d+(\.\d+)*$`)
	versionRe = regexp.MustCompile(`^v?\d[A-Za-z0-9._+-]*$`)
	hexRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	numPrefix = regexp.MustCompile(`^v?\d+(\.\d+)*`)
	scalarRe  = regexp.MustCompile(`[^\s"'=,{}]+`)
)

// Run finds every marker among files, checks each lies in exactly one of
// parts, and rewrites each marked value to its list line. It answers one
// Result for each part in the parts' order, or every refusal at once, each
// naming file, line and key and the act that fits.
func Run(files map[string][]byte, parts []policy.Part, store Store) ([]Result, error) {
	images, err := pins.ParseImages(string(files[ImagesList]))
	if err != nil {
		return nil, fmt.Errorf("%s: %w: fix the line, or copy the list from the code repository again", ImagesList, err)
	}
	pinned, err := pins.ParseFiles(string(files[FilesList]))
	if err != nil {
		return nil, fmt.Errorf("%s: %w: fix the line, or copy the list from the code repository again", FilesList, err)
	}
	if len(images) == 0 || len(pinned) == 0 {
		return nil, fmt.Errorf("the tree has no %s or %s with lines: the schema copy writes them at the root", ImagesList, FilesList)
	}
	var errs []error
	results := make([]Result, len(parts))
	for i, p := range parts {
		results[i] = Result{Part: p, Changes: map[string][]byte{}}
		if p.Lane != policy.LaneAlone && p.Lane != policy.LaneHuman && p.Lane != policy.LaneHand {
			errs = append(errs, fmt.Errorf("part %s: lane %q: use alone, human or hand", p.Name, p.Lane))
		}
	}
	l := lists{images: images, files: pinned}
	copies := map[string]bool{}
	marked := map[string]map[int]bool{}
	count := map[string]int{}
	for _, file := range slices.Sorted(mapKeys(files)) {
		lines, ok := textLines(files[file])
		if !ok || file == ImagesList || file == FilesList {
			continue
		}
		edited := -1
		for i, text := range lines {
			verb, arg, isMarker, perr := parseMarker(text)
			if !isMarker {
				continue
			}
			at := fmt.Sprintf("%s:%d", file, i+1)
			if perr != "" {
				errs = append(errs, fmt.Errorf("%s: %s", at, perr))
				continue
			}
			key := arg
			if verb != "" {
				key = verb + " " + arg
			}
			if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) == "" {
				errs = append(errs, fmt.Errorf("%s: # pin %s has no value line below it: put the marker directly above the line that holds the value", at, key))
				continue
			}
			if marked[file] == nil {
				marked[file] = map[int]bool{}
			}
			marked[file][i+1] = true
			idx := slices.IndexFunc(parts, func(p policy.Part) bool { return contains(p, file) })
			var in []string
			for _, p := range parts {
				if contains(p, file) {
					in = append(in, p.Name)
					if p.Lane == policy.LaneHand {
						errs = append(errs, fmt.Errorf("%s: # pin %s lies in the hand part %s, which carries no marker: move the marker's file out of its paths, or give the file to a part of lane alone or human", at, key, p.Name))
						in = nil
						break
					}
				}
			}
			switch {
			case in == nil && idx >= 0:
				continue
			case len(in) == 0:
				errs = append(errs, fmt.Errorf("%s: # pin %s lies in no part: add %s to the paths of a part in the project file", at, key, file))
				continue
			case len(in) > 1:
				errs = append(errs, fmt.Errorf("%s: # pin %s lies in the parts %s: the paths of a part must not overlap another's", at, key, strings.Join(in, " and ")))
				continue
			}
			count[file+"\x00"+key]++
			m := Marker{ID: fmt.Sprintf("%s#%s#%d", file, key, count[file+"\x00"+key]), File: file, Line: i + 2, Key: key, Have: lines[i+1]}
			o, cerr := l.apply(files, file, lines[i+1], verb, arg, store)
			if cerr != nil {
				errs = append(errs, fmt.Errorf("%s: # pin %s: %w", at, key, cerr))
				continue
			}
			m.Have, m.Want = o.have, o.want
			r := &results[idx]
			r.Markers = append(r.Markers, m)
			if o.copyFile != "" {
				copies[o.copyFile] = true
			}
			if !o.behind {
				continue
			}
			r.Moves = append(r.Moves, Move{Key: key, Before: o.before, After: o.after, Version: o.version})
			if o.copyFile != "" {
				if o.bytes != nil {
					r.Changes[o.copyFile] = o.bytes
				}
			} else {
				lines[i+1] = o.want
				edited = idx
			}
		}
		if edited >= 0 {
			results[edited].Changes[file] = []byte(strings.Join(lines, "\n"))
		}
	}
	for _, file := range slices.Sorted(mapKeys(files)) {
		if file == ImagesList || file == FilesList || copies[file] || inHand(parts, file) || strings.HasPrefix(file, "factory/projects/") {
			continue
		}
		lines, ok := textLines(files[file])
		if !ok {
			continue
		}
		for i, text := range lines {
			if digestRe.MatchString(text) && !marked[file][i] && !isPinComment(text) && !ownDigest(text) {
				errs = append(errs, fmt.Errorf("%s:%d: a sha256 digest with no marker above it, in no part of lane hand: add # pin <key> on the line above (the key names a line of %s), or put the file in a part of lane hand", file, i+1, ImagesList))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return results, nil
}

// Values are the values the markers of files stand over, by marker ID: the
// line below it, or for a copy the sha256 of the file. It reads no list,
// so a tree whose lists are older answers too.
func Values(files map[string][]byte) map[string]string {
	out := map[string]string{}
	count := map[string]int{}
	for file, b := range files {
		lines, ok := textLines(b)
		if !ok || file == ImagesList || file == FilesList {
			continue
		}
		for i, text := range lines {
			verb, arg, isMarker, perr := parseMarker(text)
			if !isMarker || perr != "" || i+1 >= len(lines) {
				continue
			}
			key := arg
			if verb != "" {
				key = verb + " " + arg
			}
			count[file+"\x00"+key]++
			id := fmt.Sprintf("%s#%s#%d", file, key, count[file+"\x00"+key])
			out[id] = lines[i+1]
			if verb == "copy" {
				if p, err := copyPath(file, lines[i+1]); err == nil {
					out[id] = sumHex(files[p])
				}
			}
		}
	}
	return out
}

func contains(p policy.Part, file string) bool {
	return slices.ContainsFunc(p.Paths, func(q string) bool {
		q = strings.TrimSuffix(path.Clean(q), "/")
		return file == q || strings.HasPrefix(file, q+"/")
	})
}

func inHand(parts []policy.Part, file string) bool {
	return slices.ContainsFunc(parts, func(p policy.Part) bool { return p.Lane == policy.LaneHand && contains(p, file) })
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

// textLines splits b into lines, false for a binary file.
func textLines(b []byte) ([]string, bool) {
	if bytes.IndexByte(b, 0) >= 0 {
		return nil, false
	}
	return strings.Split(string(b), "\n"), true
}

// The unmarked-digest check also leaves factory/projects/ alone: a project
// file's digests name images the factory builds itself.
//
// ownDigest is whether every digest of a line needs no marker: a
// placeholder of zeros, or a reference into the factory's own registry
// path, which is no outside copy.
func ownDigest(text string) bool {
	if strings.Contains(text, "git.aisoftwarefactory.world/sf/") {
		return true
	}
	zero := "sha256:" + strings.Repeat("0", 64)
	return !slices.ContainsFunc(digestRe.FindAllString(text, -1), func(d string) bool { return d != zero })
}

func isPinComment(text string) bool {
	_, _, ok, _ := parseMarker(text)
	return ok
}

// parseMarker reads a line as a marker: verb is "" for a plain key, else
// chart, image-of, file or copy. perr names what is wrong with a line that
// starts as a marker and is none.
func parseMarker(text string) (verb, arg string, ok bool, perr string) {
	t := strings.TrimSpace(text)
	rest, found := strings.CutPrefix(t, "# pin ")
	if !found {
		return "", "", false, ""
	}
	f := strings.Fields(rest)
	switch {
	case len(f) == 1:
		return "", f[0], true, ""
	case len(f) == 2 && slices.Contains([]string{"chart", "image-of", "file", "copy"}, f[0]):
		return f[0], f[1], true, ""
	}
	return "", "", true, fmt.Sprintf("%q is no marker: write # pin <key>, # pin chart <chart>, # pin image-of <archive>, # pin file <package> or # pin copy <package>", strings.TrimSpace(text))
}

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// copyPath is the file a copy marker's line names, below the marker's
// directory.
func copyPath(file, line string) (string, error) {
	m := scalarRe.FindAllString(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-")), -1)
	if len(m) == 0 {
		return "", errors.New("the line names no file: write the copy's path, relative to this file, as the entry")
	}
	p := path.Join(path.Dir(file), m[len(m)-1])
	if strings.HasPrefix(p, "../") || p == ".." {
		return "", fmt.Errorf("%s leaves the tree", m[len(m)-1])
	}
	return p, nil
}

// ReadTree reads every file below root, as paths with "/" relative to it,
// leaving out .git and .terraform.
func ReadTree(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- a path of the operator's own tree
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	return files, err
}
