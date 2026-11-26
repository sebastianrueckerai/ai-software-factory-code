package follow

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
)

// The kinds of what the store must hold for a move.
const (
	KindImage   = "image"   // an image or OCI chart: a manifest by its digest
	KindChart   = "chart"   // an HTTP chart archive in the store's Helm repository
	KindFile    = "file"    // a file's copy in the generic packages
	KindDerived = "derived" // the image the store derives from a file line: files/<archive>:<tag>
)

// Target is what the store must hold for one move: Path names it as the
// store does, Ref is the digest of an image.
type Target struct {
	Key, Kind, Path, Ref string
}

// ErrMissing is a Holds answer for an object the store does not have.
var ErrMissing = errors.New("the store does not hold it")

// Targets are the objects the store must hold for moves, found by each
// move's key in the lists of the tree.
func Targets(files map[string][]byte, moves []Move) ([]Target, error) {
	images, err := pins.ParseImages(string(files[ImagesList]))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ImagesList, err)
	}
	pinned, err := pins.ParseFiles(string(files[FilesList]))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FilesList, err)
	}
	l := lists{images: images, files: pinned}
	var out []Target
	for _, m := range moves {
		verb, arg, _ := strings.Cut(m.Key, " ")
		switch {
		case verb == "image-of":
			out = append(out, Target{Key: m.Key, Kind: KindDerived, Path: "files/" + arg + ":" + m.Version, Ref: m.Version})
		case verb == "file" || verb == "copy":
			f, err := l.file(arg)
			if err != nil {
				return nil, err
			}
			out = append(out, Target{Key: m.Key, Kind: KindFile, Path: "generic/" + f.StorePath()})
		case verb == "chart":
			for _, img := range images {
				url, _, _ := strings.Cut(img.Ref, "@")
				if img.Kind == "helm" && strings.HasPrefix(path.Base(url), arg+"-") && strings.HasSuffix(url, ".tgz") && versionRe.MatchString(strings.TrimSuffix(strings.TrimPrefix(path.Base(url), arg+"-"), ".tgz")) {
					out = append(out, Target{Key: m.Key, Kind: KindChart, Path: "helm/" + path.Base(url)})
					break
				}
			}
		default:
			name, _, digest, err := l.image(m.Key)
			if err != nil {
				return nil, err
			}
			out = append(out, Target{Key: m.Key, Kind: KindImage, Path: name + "@" + digest, Ref: digest})
		}
	}
	return out, nil
}

// Holds answers nil when the store holds t, an error that wraps ErrMissing
// when it answers 404, and another error for any other answer. The store
// is the organisation upstream of the Forgejo at Base.
func (s HTTPStore) Holds(t Target) error {
	base := strings.TrimSuffix(s.Base, "/")
	var url string
	switch t.Kind {
	case KindDerived:
		archive, tag, _ := strings.Cut(strings.TrimPrefix(t.Path, "files/"), ":")
		if _, err := s.FileImageDigest(archive, tag); err != nil {
			if strings.HasSuffix(err.Error(), "status 404") {
				return fmt.Errorf("%s: %w", t.Path, ErrMissing)
			}
			return err
		}
		return nil
	case KindImage:
		name, _, _ := strings.Cut(t.Path, "@")
		url = base + "/v2/upstream/" + name + "/manifests/" + t.Ref
	default:
		url = base + "/api/packages/upstream/" + t.Path
	}
	res, err := s.get(http.MethodHead, url, "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	if err != nil {
		if strings.HasSuffix(err.Error(), "status 404") {
			return fmt.Errorf("%s: %w", t.Path, ErrMissing)
		}
		return err
	}
	_ = res.Body.Close()
	return nil
}
