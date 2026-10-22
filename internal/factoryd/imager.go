package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	// imagesPath is a project's declaration of its sandbox images.
	imagesPath = "factory/images.yaml"
	// imageWorkflow is the trusted builder of project images.
	imageWorkflow = "project-image.yml"
	// dispatchWait is how long a dispatched build has before a pass asks
	// for it again.
	dispatchWait = 45 * time.Minute
	// pinBranchPrefix starts the branch of a project's pin pull request on
	// the bot's fork of the configuration repository; the project follows.
	pinBranchPrefix = "factory-pins/"
	pinRole         = "image-pin"
	// buildTries is how many dispatches of one key may leave no signed
	// image before the pass raises that the builder fails.
	buildTries = 3
)

// Imager reconciles the sandbox images each project declares in
// factory/images.yaml, a pass a minute: a declared image whose pin is not
// at its desired key is built by the trusted builder, and once the store
// holds it signed by the images key it is pinned in the project's file of
// the configuration repository, through one pull request per project from
// the bot's fork, which the imager merges itself when it changes the
// images block and nothing else.
type Imager struct {
	// Forgejo is the bot: it reads every project repository, owns the fork
	// of Config's repository and merges the pin pull requests.
	Forgejo *Forgejo
	// Config reads the configuration tree: main, and a pull request's base
	// and head.
	Config *ForgejoConfig
	// Policy is the version in force.
	Policy func() policy.Version
	Store  ImageStore
	// Registry reads the store; Key is the public half of the images key,
	// which signs project images and nothing else.
	Registry *Registry
	Key      *ecdsa.PublicKey
	// Base is the sandbox base, ref@sha256:..., every project image builds
	// on.
	Base string
	// Builder holds the builder; BuildToken dispatches it: factory-ci's
	// registrar token, which runs the Actions and neither pushes nor merges
	// main.
	Builder    Repo
	BuildToken string
	// Ladder is the model ladder's aliases CheckTree holds a tree to.
	Ladder []string
	Now    func() time.Time

	mu       sync.Mutex
	verified map[string]bool // repository@digest
	// refusals is the last pass's merge refusal of each pin pull request,
	// by "<project> #<number>": one that repeats raises an alert.
	refusals map[string]string
}

// ImageStore keeps image_builds and the imager's alerts.
type ImageStore interface {
	// ImageDispatches is when the build of key of project's image was last
	// dispatched and how often it was; zero and 0 when never.
	ImageDispatches(ctx context.Context, project, image, key string) (time.Time, int, error)
	RecordImageDispatch(ctx context.Context, project, image, key string, at time.Time) error
	RecordImageRead(ctx context.Context, project, image, key string, at time.Time) error
	InsertAlert(ctx context.Context, a Alert) (int64, error)
}

// imageRepository is where the store holds project's image.
func imageRepository(project, image string) string { return "sf/sandbox-" + project + "." + image }

// Run runs a pass every interval until ctx ends; a failed pass is logged
// when its error changes and tried again at the next.
func (i *Imager) Run(ctx context.Context, every time.Duration) {
	var last string
	for {
		if err := i.pass(ctx); err != nil && err.Error() != last {
			log.Printf("imager: %v", err)
			last = err.Error()
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// pass reconciles every project in force whose agents run.
func (i *Imager) pass(ctx context.Context) error {
	_, baseDigest, ok := strings.Cut(i.Base, "@")
	if !ok {
		return fmt.Errorf("the sandbox base %q pins no digest", i.Base)
	}
	start := i.Now()
	v := i.Policy()
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(v.Projects)) {
		c := v.Projects[name]
		if c.Agents == policy.AgentsNone {
			continue
		}
		if err := i.project(ctx, name, c, baseDigest, start); err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// project reconciles the images name declares: a pin at its desired key
// stays, an image ready at it is pinned, one not built is dispatched, and a
// pinned name no longer declared leaves. An invalid file changes nothing.
func (i *Imager) project(ctx context.Context, name string, c policy.ProjectConfig, baseDigest string, start time.Time) error {
	file, raw, ok, err := i.declared(ctx, name, c.Repository)
	if err != nil || !ok {
		return err
	}
	pins := map[string]policy.ImagePin{}
	for n, p := range c.Images {
		if _, ok := file.Images[n]; ok {
			pins[n] = p
		}
	}
	for _, n := range slices.Sorted(maps.Keys(file.Images)) {
		spec := file.Images[n]
		key := imagegen.BuildKey(spec, baseDigest)
		if err := i.Store.RecordImageRead(ctx, name, n, key, start); err != nil {
			return err
		}
		if pins[n].Key == key {
			continue
		}
		pin, ready, err := i.build(ctx, name, n, spec, key, raw)
		if err != nil {
			return fmt.Errorf("image %s: %w", n, err)
		}
		if ready {
			pins[n] = pin
		}
	}
	return i.pin(ctx, name, c.Repository, pins, c.Images)
}

// declared reads factory/images.yaml at the head of repository's default
// branch; a repository without one declares none. An invalid file raises
// one alert per blob and is not ok.
func (i *Imager) declared(ctx context.Context, project, repository string) (policy.ImagesFile, []byte, bool, error) {
	repo, err := SplitRepo(repository)
	if err != nil {
		return policy.ImagesFile{}, nil, false, err
	}
	branch, err := i.Forgejo.RepoDefaultBranch(ctx, repo.Owner, repo.Name)
	if err != nil {
		return policy.ImagesFile{}, nil, false, err
	}
	raw, blob, err := i.Forgejo.FileContent(ctx, repo.Owner, repo.Name, branch, imagesPath)
	if errors.Is(err, ErrNotFound) {
		return policy.ImagesFile{}, nil, true, nil
	}
	if err != nil {
		return policy.ImagesFile{}, nil, false, err
	}
	f, findings := policy.ParseImagesFile(raw)
	if len(findings) > 0 {
		i.alert(ctx, Alert{Kind: AlertImage, Source: "file", Fingerprint: project + " " + imagesPath + " " + blob,
			Detail: map[string]any{"project": project, "repository": repository, "blob": blob, "findings": findings}})
		return policy.ImagesFile{}, nil, false, nil
	}
	return f, raw, true, nil
}

// build is project's image name at key: ready when the store holds the tag
// key signed by the images key, at most policy.MaxImageBytes compressed
// (an alert and never ready above); else its build is dispatched.
func (i *Imager) build(ctx context.Context, project, name string, spec policy.ImageSpec, key string, file []byte) (policy.ImagePin, bool, error) {
	repo := imageRepository(project, name)
	tok, err := i.Registry.Token(ctx)
	if err != nil {
		return policy.ImagePin{}, false, err
	}
	m, err := i.Registry.Resolve(ctx, tok, repo, key)
	if err == nil {
		err = i.verify(ctx, repo, m.Digest)
	}
	var unsigned ErrUnsigned
	switch {
	case errors.Is(err, ErrNotFound) || errors.As(err, &unsigned):
		return policy.ImagePin{}, false, i.dispatch(ctx, project, name, key, file)
	case err != nil:
		return policy.ImagePin{}, false, err
	}
	size, err := i.Registry.LayerBytes(ctx, tok, repo, m.Platform)
	if err != nil {
		return policy.ImagePin{}, false, err
	}
	if size > policy.MaxImageBytes {
		i.alert(ctx, Alert{Kind: AlertImage, Source: "size", Fingerprint: repo + "@" + m.Digest, Detail: map[string]any{
			"project": project, "image": name, "digest": m.Digest, "bytes": size, "max": int64(policy.MaxImageBytes)}})
		return policy.ImagePin{}, false, nil
	}
	_, _, manifest, err := imagegen.Render(spec, i.Base)
	if err != nil {
		return policy.ImagePin{}, false, err
	}
	return policy.ImagePin{Digest: m.Digest, Key: key, Tools: manifest.Tools, Egress: manifest.Egress}, true, nil
}

// dispatch asks the builder for project's image name at key, built from
// file, unless it was asked within dispatchWait. A key buildTries builds
// left unsigned raises one alert, and is asked for again.
func (i *Imager) dispatch(ctx context.Context, project, name, key string, file []byte) error {
	now := i.Now()
	at, n, err := i.Store.ImageDispatches(ctx, project, name, key)
	if err != nil || !at.IsZero() && now.Sub(at) < dispatchWait {
		return err
	}
	if n >= buildTries {
		repo := imageRepository(project, name)
		i.alert(ctx, Alert{Kind: AlertImage, Source: "build", Fingerprint: "build " + repo + ":" + key, Detail: map[string]any{
			"project": project, "image": name, "key": key, "dispatches": n,
			"reason": fmt.Sprintf("%d builds of %s:%s left no image signed by the images key: the builder %s of %s failed, its runs say why",
				n, repo, key, imageWorkflow, i.Builder)}})
	}
	if err := i.Forgejo.DispatchWorkflow(ctx, i.BuildToken, i.Builder, imageWorkflow, "main", map[string]string{
		"project": project, "image": name, "file": base64.StdEncoding.EncodeToString(file), "base": i.Base, "key": key}); err != nil {
		return err
	}
	log.Printf("imager: dispatched the build of %s:%s", imageRepository(project, name), key)
	return i.Store.RecordImageDispatch(ctx, project, name, key, now)
}

// verify is nil when repository@digest carries a signature by the images
// key; a verified digest is remembered for the process's life.
func (i *Imager) verify(ctx context.Context, repository, digest string) error {
	ref := repository + "@" + digest
	i.mu.Lock()
	ok := i.verified[ref]
	i.mu.Unlock()
	if ok {
		return nil
	}
	tok, err := i.Registry.Token(ctx)
	if err != nil {
		return err
	}
	if err := i.Registry.Verify(ctx, tok, repository, digest, i.Key); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.verified == nil {
		i.verified = map[string]bool{}
	}
	i.verified[ref] = true
	return nil
}

// verifyProjectImage is nil when project's image at digest carries a
// signature by the images key: a pod runs a project image only then.
func (r *Runner) verifyProjectImage(ctx context.Context, project, image, digest string) error {
	if r.Imager == nil {
		return errors.New("no imager is wired: no project image verifies")
	}
	return r.Imager.verify(ctx, imageRepository(project, image), digest)
}

// alert raises a; the alerts table keeps one row per fingerprint.
func (i *Imager) alert(ctx context.Context, a Alert) {
	id, err := i.Store.InsertAlert(ctx, a)
	switch {
	case err != nil:
		log.Printf("imager: raise %q: %v", a.Fingerprint, err)
	case id != 0:
		log.Printf("imager: alert %q: %v", a.Fingerprint, a.Detail)
	}
}

func pinTitle(project string) string { return "Image pins for " + project }

// pin brings the images block of project's file on the configuration
// repository's main to pins through the project's one pull request from
// the bot's fork: opened from main, else its branch written in place, else,
// once the branch holds the block, merged. Its pull request closes once
// main is what it would make it.
func (i *Imager) pin(ctx context.Context, project, repository string, pins, inForce map[string]policy.ImagePin) error {
	infra, rel := i.Config.Repo, policy.ProjectFile(project)
	fork, branch, path := i.Forgejo.Fork(infra), pinBranchPrefix+project, i.Config.Dir+"/"+rel
	own, err := i.ownPullRequest(ctx, fork, branch)
	if err != nil {
		return err
	}
	if own == nil && len(pins) == len(inForce) && (len(pins) == 0 || reflect.DeepEqual(pins, inForce)) {
		return nil
	}
	block, err := imagesBlock(pins)
	if err != nil {
		return err
	}
	head, err := i.Config.Head(ctx)
	if err != nil {
		return err
	}
	main, err := i.Config.File(ctx, head, rel)
	if err != nil {
		return err
	}
	if bytes.Equal(withImages(main, block), main) {
		if own == nil {
			return nil
		}
		return i.closePin(ctx, *own, "main's "+path+" is already what it would make it")
	}
	if own == nil {
		return i.open(ctx, project, repository, fork, branch, path, block)
	}
	have, blob, err := i.Forgejo.FileContent(ctx, fork.Owner, fork.Name, own.HeadSHA, path)
	if err != nil {
		return err
	}
	if want := withImages(have, block); !bytes.Equal(want, have) {
		if _, err := i.Forgejo.CommitFiles(ctx, fork.Owner, fork.Name, branch, "", Attribution{Role: pinRole}, pinTitle(project),
			[]FileChange{{Path: path, Content: want, Blob: blob}}); err != nil {
			return err
		}
		log.Printf("imager: wrote the images block of %s on pull request #%d", project, own.Number)
		return nil
	}
	return i.merge(ctx, project, *own, head)
}

// ownPullRequest is the open pull request from branch on fork into main, or
// nil.
func (i *Imager) ownPullRequest(ctx context.Context, fork Repo, branch string) (*PullRequestRef, error) {
	infra := i.Config.Repo
	open, err := i.Forgejo.OpenPullRequests(ctx, infra.Owner, infra.Name)
	if err != nil {
		return nil, err
	}
	for _, pr := range open {
		if pr.HeadRepo == fork.String() && pr.Head == branch && pr.Base == "main" {
			return &pr, nil
		}
	}
	return nil, nil
}

// closePin closes pr, saying why on it: the next pass cuts the branch
// afresh from main, should main still lack the block.
func (i *Imager) closePin(ctx context.Context, pr PullRequestRef, why string) error {
	infra := i.Config.Repo
	if err := i.Forgejo.ClosePullRequest(ctx, infra.Owner, infra.Name, pr.Number); err != nil {
		return err
	}
	if _, err := i.Forgejo.CreateIssueComment(ctx, infra.Owner, infra.Name, pr.Number, "Closed by factoryd: "+why+"."); err != nil {
		return err
	}
	log.Printf("imager: closed pull request #%d: %s", pr.Number, why)
	return nil
}

// open cuts branch afresh from main with the images block, as a branch a
// closed pull request left may hold anything, and opens the pull request.
func (i *Imager) open(ctx context.Context, project, repository string, fork Repo, branch, path, block string) error {
	infra := i.Config.Repo
	if err := i.Forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, branch); err != nil {
		return err
	}
	if err := i.Forgejo.EnsureFork(ctx, infra); err != nil {
		return err
	}
	if err := i.Forgejo.SyncFork(ctx, fork); err != nil {
		return err
	}
	have, blob, err := i.Forgejo.FileContent(ctx, fork.Owner, fork.Name, "main", path)
	if err != nil {
		return err
	}
	want := withImages(have, block)
	if bytes.Equal(want, have) {
		return nil
	}
	title := pinTitle(project)
	if _, err := i.Forgejo.CommitFiles(ctx, fork.Owner, fork.Name, "main", branch, Attribution{Role: pinRole}, title,
		[]FileChange{{Path: path, Content: want, Blob: blob}}); err != nil {
		return err
	}
	body := fmt.Sprintf("factoryd pins the sandbox images %s of %s declares: this pull request changes only the images block of %s.\n\n"+
		"factoryd merges it itself once it has checked that, and that the tree at its head passes factoryd's config check; otherwise it stays open and factoryd raises an alert naming why.\n",
		imagesPath, repository, path)
	ref, err := i.Forgejo.CreatePullRequest(ctx, infra.Owner, infra.Name, fork.Owner+":"+branch, "main", title, body)
	if err != nil {
		return err
	}
	log.Printf("imager: opened pull request #%d with the images block of %s", ref.Number, project)
	return nil
}

// merge merges pr as the bot unless judge finds against it; a finding
// leaves it open with one alert naming why, and so does a refusal by
// Forgejo that repeats on a later pass. A failed check or refusal on a
// branch cut before main's head closes it instead, so the next pass cuts
// it from main: once per change of main.
func (i *Imager) merge(ctx context.Context, project string, pr PullRequestRef, head string) error {
	code, why, err := i.judge(ctx, project, pr)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s #%d", project, pr.Number)
	if code == "" {
		infra := i.Config.Repo
		err := i.Forgejo.MergePullRequestAsBot(ctx, infra.Owner, infra.Name, pr.Number, pr.HeadSHA, Attribution{Role: pinRole}, pinTitle(project))
		if !errors.Is(err, ErrRefused) {
			if err == nil {
				delete(i.refusals, key)
				log.Printf("imager: merged pull request #%d, the images block of %s", pr.Number, project)
			}
			return err
		}
		code, why = "merge", fmt.Sprintf("Forgejo refused the bot's merge of pull request #%d: %v", pr.Number, err)
	}
	if (code == "check" || code == "merge") && pr.MergeBase != head {
		return i.closePin(ctx, pr, fmt.Sprintf("its branch was cut from %s, main is at %s now, and %s; the next pass cuts it afresh from main", pr.MergeBase, head, why))
	}
	if code == "merge" && i.refusals[key] != why {
		if i.refusals == nil {
			i.refusals = map[string]string{}
		}
		i.refusals[key] = why
		log.Printf("imager: %s; the next pass tries again", why)
		return nil
	}
	i.alert(ctx, Alert{Kind: AlertImage, Source: "pin", Fingerprint: key + " " + code,
		Detail: map[string]any{"project": project, "pull_request": pr.Number, "reason": why}})
	return nil
}

// judge names what keeps pr from merging unattended, a code and why, or
// nothing: a file it changes besides project's file, a field of that file
// besides images between pr's merge base and head, or a tree at its head
// CheckTree refuses.
func (i *Imager) judge(ctx context.Context, project string, pr PullRequestRef) (code, why string, err error) {
	infra, rel := i.Config.Repo, policy.ProjectFile(project)
	path := i.Config.Dir + "/" + rel
	files, err := i.Forgejo.PullRequestFiles(ctx, infra.Owner, infra.Name, pr.Number)
	if err != nil {
		return "", "", err
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Filename)
		if f.PreviousFilename != "" && f.PreviousFilename != f.Filename {
			names = append(names, f.PreviousFilename)
		}
	}
	if !slices.Equal(names, []string{path}) {
		slices.Sort(names)
		return "files", fmt.Sprintf("pull request #%d changes %s; an image pin changes %s only", pr.Number, cmp.Or(strings.Join(names, ", "), "no file"), path), nil
	}
	if pr.MergeBase == "" || pr.HeadSHA == "" {
		return "", "", fmt.Errorf("pull request #%d: Forgejo names no merge base or head yet", pr.Number)
	}
	var cfgs [2]policy.ProjectConfig
	for n, commit := range []string{pr.MergeBase, pr.HeadSHA} {
		b, err := i.Config.File(ctx, commit, rel)
		if err != nil {
			return "", "", err
		}
		if cfgs[n], err = policy.DecodeProject(b); err != nil {
			return "fields", fmt.Sprintf("%s at %s does not decode: %v", path, commit, err), nil
		}
		cfgs[n].Images = nil
	}
	if !reflect.DeepEqual(cfgs[0], cfgs[1]) {
		return "fields", fmt.Sprintf("pull request #%d changes %s beyond its images block", pr.Number, path), nil
	}
	tree, err := i.Config.Files(ctx, pr.HeadSHA)
	if err != nil {
		return "", "", err
	}
	if _, err := CheckTree(tree, i.Ladder); err != nil {
		return "check", fmt.Sprintf("the tree at the head of pull request #%d fails factoryd's config check: %v", pr.Number, err), nil
	}
	return "", "", nil
}

// imagesBlock is a project file's top-level images block of pins, names
// sorted and each pin's fields in ImagePin's order; "" for none.
func imagesBlock(pins map[string]policy.ImagePin) (string, error) {
	if len(pins) == 0 {
		return "", nil
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(map[string]map[string]policy.ImagePin{"images": pins}); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

var imagesKeyRE = regexp.MustCompile(`^images:(\s|$)`)

// withImages is file with its top-level images block, from the key through
// the last indented line before the next top-level key, replaced by block,
// every other byte kept; a file without one gets block at its end.
func withImages(file []byte, block string) []byte {
	lines := strings.SplitAfter(string(file), "\n")
	start := slices.IndexFunc(lines, imagesKeyRE.MatchString)
	if start < 0 {
		s := string(file)
		if block != "" && s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		return []byte(s + block)
	}
	end := start + 1
	for n := start + 1; n < len(lines); n++ {
		l := lines[n]
		if strings.TrimSpace(l) == "" || l[0] == '#' {
			continue
		}
		if l[0] != ' ' && l[0] != '\t' {
			break
		}
		end = n + 1
	}
	return []byte(strings.Join(lines[:start], "") + block + strings.Join(lines[end:], ""))
}

// ImageDispatches is when factoryd last dispatched the build of key of
// project's image and how often it did, zero and 0 when never.
func (s *PGStore) ImageDispatches(ctx context.Context, project, image, key string) (time.Time, int, error) {
	var at sql.NullTime
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT dispatched_at, dispatches FROM image_builds WHERE project = $1 AND image = $2 AND key = $3`,
		project, image, key).Scan(&at, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, nil
	}
	return at.Time, n, err
}

// RecordImageDispatch records a dispatch of the build of key at at.
func (s *PGStore) RecordImageDispatch(ctx context.Context, project, image, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO image_builds (project, image, key, dispatched_at, dispatches) VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (project, image, key) DO UPDATE SET dispatched_at = EXCLUDED.dispatched_at, dispatches = image_builds.dispatches + 1`,
		project, image, key, at)
	return err
}

// RecordImageRead records that the pass started at at read key as the
// desired key of project's image.
func (s *PGStore) RecordImageRead(ctx context.Context, project, image, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO image_builds (project, image, key, read_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project, image, key) DO UPDATE SET read_at = EXCLUDED.read_at`, project, image, key, at)
	return err
}
