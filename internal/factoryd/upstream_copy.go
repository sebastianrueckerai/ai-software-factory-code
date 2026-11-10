package factoryd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// imageLine is the shape of an image line of upstream-images.txt that
// scripts/upstream-copy copies: name:tag@sha256:<64 hex>, the name led by
// its registry's host.
var imageLine = regexp.MustCompile(`^([^\s@:/]+\.[^\s@:]+/[^\s@:]+):([\w][\w.-]{0,127})@sha256:([0-9a-f]{64})$`)

// addedImageLines are the image lines of head that base lacks, compared
// by ref; a malformed one is a finding, not a line. Blank, comment and
// https lines are not image lines.
func addedImageLines(base, head string) (lines, findings []string) {
	had := map[string]bool{}
	for _, l := range imageCandidates(base) {
		had[strings.Fields(l)[0]] = true
	}
	for _, l := range imageCandidates(head) {
		ref := strings.Fields(l)[0]
		if had[ref] {
			continue
		}
		had[ref] = true
		if !imageLine.MatchString(ref) {
			findings = append(findings, l+": want name:tag@sha256:<digest>")
			continue
		}
		lines = append(lines, l)
	}
	return lines, findings
}

func imageCandidates(list string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(list))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "https://") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// UpstreamCopier copies the image lines a branch adds to
// upstream-images.txt into the factory's store, as scripts/upstream-copy
// does for main: a pull request's image build reads its base images from
// the store, and only main's upstream job fills it otherwise.
type UpstreamCopier struct {
	// Registry reads the store with its read:package login.
	Registry *Registry
	// OpenBao holds the site admin's login, which writes the store.
	OpenBao SecretReader
	// Endpoint is the base URL of a registry host; nil serves https://host,
	// docker.io's from registry-1.docker.io.
	Endpoint func(host string) string
	// HTTP has no overall timeout: a blob is large, ctx bounds the copy.
	HTTP  *http.Client
	Scrub *redact.Scrubber
}

var errUpstreamMissing = errors.New("the upstream does not serve this digest")

// Copy copies the image lines head adds to base into the store, returning
// findings for the lines it cannot copy; an error is one Temporal retries.
func (c *UpstreamCopier) Copy(ctx context.Context, base, head string) ([]string, error) {
	lines, findings := addedImageLines(base, head)
	var admin AdminAuth
	for _, l := range lines {
		if admin.User == "" {
			var err error
			if admin, err = adminAuth(ctx, c.OpenBao); err != nil {
				return findings, c.scrubbed(err)
			}
		}
		err := c.image(ctx, admin, strings.Fields(l)[0])
		if errors.Is(err, errUpstreamMissing) {
			findings = append(findings, l+": "+errUpstreamMissing.Error())
		} else if err != nil {
			return findings, c.scrubbed(fmt.Errorf("%s: %w", l, err))
		}
	}
	return findings, nil
}

func (c *UpstreamCopier) scrubbed(err error) error {
	s := c.Scrub
	if s == nil {
		s = redact.New()
	}
	return errors.New(s.String(err.Error()))
}

func (c *UpstreamCopier) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *UpstreamCopier) image(ctx context.Context, admin AdminAuth, ref string) error {
	m := imageLine.FindStringSubmatch(ref)
	name, tag, hexDigest := m[1], m[2], m[3]
	digest := "sha256:" + hexDigest
	repo := "upstream/" + name
	tok, err := c.Registry.Token(ctx)
	if err != nil {
		return err
	}
	_, _, err = c.Registry.IndexEntries(ctx, tok, repo, digest)
	if errors.Is(err, ErrNotFound) {
		host, path, _ := strings.Cut(name, "/")
		j := &copyJob{c: c, admin: admin, repo: repo, path: path, base: c.endpoint(host)}
		if err := j.manifest(ctx, digest, tag+"-"+hexDigest[:12]); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, _, err = c.Registry.IndexEntries(ctx, tok, repo, tag)
	if errors.Is(err, ErrNotFound) {
		return (&copyJob{c: c, admin: admin, repo: repo}).tag(ctx, digest, tag)
	}
	return err
}

func (c *UpstreamCopier) endpoint(host string) string {
	switch {
	case c.Endpoint != nil:
		return c.Endpoint(host)
	case host == "docker.io":
		return "https://registry-1.docker.io"
	}
	return "https://" + host
}

// copyJob is the copy of one line: path at the upstream, repo in the store.
type copyJob struct {
	c     *UpstreamCopier
	admin AdminAuth
	repo  string
	path  string
	base  string
	token string
}

type manifestDoc struct {
	Config    struct{ Digest string }   `json:"config"`
	Layers    []struct{ Digest string } `json:"layers"`
	Manifests []struct{ Digest string } `json:"manifests"`
}

// manifest copies the manifest at upstream ref, with what it names, to the
// store under putAs.
func (j *copyJob) manifest(ctx context.Context, ref, putAs string) error {
	res, err := j.get(ctx, "/manifests/"+ref, manifestAccept)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("upstream manifest %s: %w", ref, err)
	}
	if sum := sha256.Sum256(body); strings.HasPrefix(ref, "sha256:") && ref != "sha256:"+hex.EncodeToString(sum[:]) {
		return fmt.Errorf("upstream manifest %s: the body hashes to sha256:%x", ref, sum)
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("upstream manifest %s: %w", ref, err)
	}
	for _, ch := range doc.Manifests {
		if err := j.manifest(ctx, ch.Digest, ch.Digest); err != nil {
			return err
		}
	}
	blobs := []string{}
	if doc.Config.Digest != "" {
		blobs = append(blobs, doc.Config.Digest)
	}
	for _, l := range doc.Layers {
		blobs = append(blobs, l.Digest)
	}
	for _, b := range blobs {
		if err := j.blob(ctx, b); err != nil {
			return err
		}
	}
	return j.putManifest(ctx, putAs, res.Header.Get("Content-Type"), body)
}

func (j *copyJob) blob(ctx context.Context, digest string) error {
	if !signatureRef.MatchString(digest) {
		return fmt.Errorf("upstream blob %q is not a sha256 digest", digest)
	}
	head, err := j.store(ctx, http.MethodHead, "/v2/"+j.repo+"/blobs/"+digest, "", nil, 0)
	if err != nil {
		return err
	}
	_ = head.Body.Close()
	if head.StatusCode == http.StatusOK {
		return nil
	}
	res, err := j.get(ctx, "/blobs/"+digest, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	f, err := os.CreateTemp("", "upstream-blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), res.Body)
	if err != nil {
		return fmt.Errorf("upstream blob %s: %w", digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("upstream blob %s: the body hashes to %s", digest, got)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	start, err := j.store(ctx, http.MethodPost, "/v2/"+j.repo+"/blobs/uploads/", "", nil, 0)
	if err != nil {
		return err
	}
	_ = start.Body.Close()
	if start.StatusCode != http.StatusAccepted {
		return fmt.Errorf("store blob upload of %s: status %d", digest, start.StatusCode)
	}
	loc, err := url.Parse(start.Header.Get("Location"))
	if err != nil || start.Header.Get("Location") == "" {
		return fmt.Errorf("store blob upload of %s: no location", digest)
	}
	q := loc.Query()
	q.Set("digest", digest)
	loc.RawQuery = q.Encode()
	put, err := j.store(ctx, http.MethodPut, loc.RequestURI(), "application/octet-stream", f, n)
	if err != nil {
		return err
	}
	_ = put.Body.Close()
	if put.StatusCode != http.StatusCreated {
		return fmt.Errorf("store blob put of %s: status %d", digest, put.StatusCode)
	}
	return nil
}

func (j *copyJob) putManifest(ctx context.Context, reference, contentType string, body []byte) error {
	res, err := j.store(ctx, http.MethodPut, "/v2/"+j.repo+"/manifests/"+reference, contentType, strings.NewReader(string(body)), int64(len(body)))
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return fmt.Errorf("store manifest put of %s: status %d", reference, res.StatusCode)
	}
	return nil
}

// tag names digest, which the store holds, tag.
func (j *copyJob) tag(ctx context.Context, digest, tag string) error {
	res, err := j.store(ctx, http.MethodGet, "/v2/"+j.repo+"/manifests/"+digest, "", nil, 0)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("store manifest %s: status %d", digest, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	return j.putManifest(ctx, tag, res.Header.Get("Content-Type"), body)
}

// store sends a request to the store with the site admin's login. The
// request's host is the store's: a Location the store names never takes
// the login elsewhere.
func (j *copyJob) store(ctx context.Context, method, path, contentType string, body io.Reader, size int64) (*http.Response, error) {
	base, err := url.Parse(j.c.Registry.BaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String()+path, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	if method == http.MethodGet {
		req.Header.Set("Accept", manifestAccept)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.SetBasicAuth(j.admin.User, j.admin.Password)
	res, err := j.c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("store %s %s: %w", method, path, err)
	}
	return res, nil
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// get reads path of the upstream's repository anonymously, asking for a
// bearer once when the registry answers 401. A 404 is errUpstreamMissing.
func (j *copyJob) get(ctx context.Context, path, accept string) (*http.Response, error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.base+"/v2/"+j.path+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if j.token != "" {
			req.Header.Set("Authorization", "Bearer "+j.token)
		}
		res, err := j.c.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("upstream GET %s: %w", path, err)
		}
		switch {
		case res.StatusCode == http.StatusUnauthorized && try == 0:
			challenge := res.Header.Get("WWW-Authenticate")
			_ = res.Body.Close()
			if err := j.login(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		case res.StatusCode == http.StatusNotFound:
			_ = res.Body.Close()
			return nil, errUpstreamMissing
		case res.StatusCode != http.StatusOK:
			_ = res.Body.Close()
			return nil, fmt.Errorf("upstream GET %s: status %d", path, res.StatusCode)
		}
		return res, nil
	}
}

// login answers a Bearer challenge with an anonymous pull token.
func (j *copyJob) login(ctx context.Context, challenge string) error {
	p := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		p[strings.ToLower(m[1])] = m[2]
	}
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") || p["realm"] == "" {
		return fmt.Errorf("upstream %s: an unauthorized answer with no bearer challenge", j.path)
	}
	u, err := url.Parse(p["realm"])
	if err != nil {
		return err
	}
	q := u.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + j.path + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	res, err := j.c.client().Do(req)
	if err != nil {
		return fmt.Errorf("upstream token for %s: %w", j.path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream token for %s: status %d", j.path, res.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxForgejoAnswer)).Decode(&tok); err != nil {
		return fmt.Errorf("upstream token for %s: %w", j.path, err)
	}
	j.token = tok.Token
	if j.token == "" {
		j.token = tok.AccessToken
	}
	if j.token == "" {
		return fmt.Errorf("upstream token for %s: no token in the answer", j.path)
	}
	return nil
}
