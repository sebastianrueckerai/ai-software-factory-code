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
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// imageLine is the shape of an image line of upstream-images.txt that
// scripts/upstream-copy copies: name:tag@sha256:<64 hex>, the name led by
// its registry's host.
var imageLine = regexp.MustCompile(`^(` + hostName + `(?:/` + nameComponent + `)+):([\w][\w.-]{0,127})@sha256:([0-9a-f]{64})$`)

// The OCI distribution name grammar: a registry host with a dot, then
// path components of lowercase alphanumerics joined by single separators.
const (
	hostName      = `[a-z0-9]+(?:-+[a-z0-9]+)*(?:\.[a-z0-9]+(?:-+[a-z0-9]+)*)+`
	nameComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
)

// addedImageLines are the image lines of head that base lacks, compared
// by ref; a malformed one is a finding, not a line. Blank, comment and
// https lines are not image lines.
func addedImageLines(base, head string) (lines, findings []string) {
	had := map[string]bool{}
	baseLines, err := imageCandidates(base)
	if err != nil {
		findings = append(findings, "upstream-images.txt at the base: "+err.Error())
	}
	for _, l := range baseLines {
		had[strings.Fields(l)[0]] = true
	}
	headLines, err := imageCandidates(head)
	if err != nil {
		findings = append(findings, "upstream-images.txt at the head: "+err.Error())
	}
	for _, l := range headLines {
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

func imageCandidates(list string) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(list))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "https://") {
			continue
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("a line is too long to read (%w)", err)
	}
	return out, nil
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

	// slot is the one copy at a time: each holds a blob in the shared /tmp
	slotOnce sync.Once
	slot     chan struct{}
}

// findingError is a line the copy cannot make, which the card hears as a
// finding; any other error is Temporal's to retry.
type findingError string

func (e findingError) Error() string { return string(e) }

const errUpstreamMissing = findingError("the upstream does not serve this digest")

// maxBlobBytes is the largest blob a copy before merge takes: a copy holds
// one blob at a time in /tmp, a 1 GiB volume. Main's upstream job copies a
// larger one at the merge.
const maxBlobBytes = 512 << 20

// Copy copies the image lines head adds to base into the store, returning
// findings for the lines it cannot copy; an error is one Temporal retries.
func (c *UpstreamCopier) Copy(ctx context.Context, base, head string) ([]string, error) {
	lines, findings := addedImageLines(base, head)
	if len(lines) == 0 {
		return findings, nil
	}
	c.slotOnce.Do(func() { c.slot = make(chan struct{}, 1) })
	select {
	case c.slot <- struct{}{}:
		defer func() { <-c.slot }()
	case <-ctx.Done():
		return findings, c.scrubbed(fmt.Errorf("waiting for another copy: %w", ctx.Err()))
	}
	var admin AdminAuth
	for _, l := range lines {
		if admin.User == "" {
			var err error
			if admin, err = adminAuth(ctx, c.OpenBao); err != nil {
				return findings, c.scrubbed(err)
			}
		}
		err := c.image(ctx, admin, strings.Fields(l)[0])
		var fe findingError
		if errors.As(err, &fe) {
			findings = append(findings, l+": "+fe.Error())
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
	return upstreamClient
}

// upstreamClient bounds the connect and the wait for headers, so a stalled
// upstream does not hold the copy's slot; a body's read is bounded by the
// activity's context.
var upstreamClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   30 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}}

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
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	host, path, _ := strings.Cut(name, "/")
	j := &copyJob{c: c, admin: admin, repo: repo, path: path, base: c.endpoint(host)}
	return j.manifest(ctx, digest, tag+"-"+hexDigest[:12])
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

type descriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type manifestDoc struct {
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"`
}

// manifest copies the manifest at upstream ref, with what it names, to the
// store under putAs.
func (j *copyJob) manifest(ctx context.Context, ref, putAs string) error {
	if !signatureRef.MatchString(ref) {
		return findingError(fmt.Sprintf("the upstream's manifest names %q, which is not a sha256 digest", cutText(ref, 80)))
	}
	res, err := j.get(ctx, "/manifests/"+ref, manifestAccept)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("upstream manifest %s: %w", ref, err)
	}
	if sum := sha256.Sum256(body); ref != "sha256:"+hex.EncodeToString(sum[:]) {
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
	blobs := slices.Concat([]descriptor{doc.Config}, doc.Layers)
	for _, b := range blobs {
		if b.Size > maxBlobBytes {
			return findingError(fmt.Sprintf("a blob of %d bytes is too large to copy before the merge, where main's upstream job copies it", b.Size))
		}
	}
	for _, b := range blobs {
		if b.Digest == "" && b.Size == 0 {
			continue
		}
		if err := j.blob(ctx, b); err != nil {
			return err
		}
	}
	return j.putManifest(ctx, putAs, res.Header.Get("Content-Type"), body)
}

func (j *copyJob) blob(ctx context.Context, d descriptor) error {
	digest := d.Digest
	if d.Size < 0 {
		return findingError(fmt.Sprintf("a blob of %s has the size %d in its descriptor", cutText(digest, 80), d.Size))
	}
	if !signatureRef.MatchString(digest) {
		return findingError(fmt.Sprintf("the upstream's manifest names a blob %q, which is not a sha256 digest", cutText(digest, 80)))
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
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, d.Size+1))
	if err != nil {
		return fmt.Errorf("upstream blob %s: %w", digest, err)
	}
	if n > d.Size {
		return findingError(fmt.Sprintf("a blob of %s is longer than its descriptor's %d bytes", digest, d.Size))
	}
	if n < d.Size {
		return findingError(fmt.Sprintf("a blob of %s is %d bytes, its descriptor says %d", digest, n, d.Size))
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return findingError(fmt.Sprintf("a blob of %s hashes to %s", digest, got))
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

// cutText is s cut to n bytes, at a rune boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
