package follow

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"
)

// HTTPStore is the factory's store over HTTP: Base is its address, for
// example https://git.aisoftwarefactory.world, read with a basic login.
type HTTPStore struct {
	Base           string
	User, Password string
	Client         *http.Client
}

func (s HTTPStore) get(method, url, accept string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.User, s.Password)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	c := s.Client
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		return nil, fmt.Errorf("%s %s: status %d", method, url, res.StatusCode)
	}
	return res, nil
}

// FileImageDigest is the digest of the image the store derived from a file
// line: files/<archive>:<tag>.
func (s HTTPStore) FileImageDigest(archive, tag string) (string, error) {
	res, err := s.get(http.MethodHead, strings.TrimSuffix(s.Base, "/")+"/v2/upstream/files/"+archive+"/manifests/"+tag,
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json")
	if err != nil {
		return "", err
	}
	_ = res.Body.Close()
	d := res.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(d, "sha256:") {
		return "", fmt.Errorf("the store answered no digest for files/%s:%s", archive, tag)
	}
	return d, nil
}

// FileCopy is the store's copy of a file line.
func (s HTTPStore) FileCopy(f pins.File) ([]byte, error) {
	res, err := s.get(http.MethodGet, strings.TrimSuffix(s.Base, "/")+"/api/packages/upstream/generic/"+f.StorePath(), "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, 64<<20))
}
