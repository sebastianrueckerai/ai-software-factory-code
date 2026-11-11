package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const exportFixtureUsage = "usage: factoryd export-fixture <owner/repo> <issue>"

// runExportFixture is `factoryd export-fixture <owner/repo> <issue>`: the
// card's replay fixture on stdout. It returns the exit code: 0 done, 1
// failed, 2 usage. open builds the exporter once the arguments parse. Every
// message goes through scrub: an error may carry a connection string.
func runExportFixture(ctx context.Context, args []string, stdout, stderr io.Writer, scrub *redact.Scrubber, open func(context.Context) (*factoryd.FixtureExporter, func(), error)) int {
	fail := func(code int, err error) int {
		fmt.Fprintln(stderr, scrub.String(err.Error()))
		return code
	}
	if len(args) != 2 {
		missing := "the issue number is missing"
		switch {
		case len(args) == 0:
			missing = "the repository and the issue number are missing"
		case len(args) > 2:
			missing = "too many arguments"
		}
		return fail(2, fmt.Errorf("%s: %s", exportFixtureUsage, missing))
	}
	issue, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || issue <= 0 {
		return fail(2, fmt.Errorf("%s: issue %q is not a number", exportFixtureUsage, args[1]))
	}
	ex, done, err := open(ctx)
	if err != nil {
		return fail(1, err)
	}
	defer done()
	f, err := ex.Export(ctx, args[0], issue)
	if err != nil {
		return fail(1, err)
	}
	body, err := f.JSON()
	if err != nil {
		return fail(1, err)
	}
	if _, err := stdout.Write(body); err != nil {
		return fail(1, err)
	}
	return 0
}

// fixtureExporterFrom opens the daemon's database for reading only (no
// migration runs, the session is read-only), its object store and its
// current policy. It holds no Forgejo or cluster credential.
func fixtureExporterFrom(secrets map[string]string, scrub *redact.Scrubber) func(context.Context) (*factoryd.FixtureExporter, func(), error) {
	return func(ctx context.Context) (*factoryd.FixtureExporter, func(), error) {
		if unset := unsetConfiguration(secrets, func(name string) string {
			if _, endpoint := daemonURLs[name]; endpoint {
				return daemonURL(name)
			}
			return os.Getenv(name)
		}); len(unset) > 0 {
			return nil, nil, fmt.Errorf("export-fixture needs the daemon's configuration, so that its scrubber knows every secret the daemon's does: %s unset", strings.Join(unset, ", "))
		}
		store, err := factoryd.OpenStoreReadOnly(ctx, secrets["FACTORYD_DATABASE_URL"])
		if err != nil {
			return nil, nil, fmt.Errorf("open store: %w", err)
		}
		v, ok, err := store.CurrentPolicy(ctx)
		if err != nil || !ok {
			_ = store.Close()
			if err == nil {
				err = errors.New("no policy version is stored in this database")
			}
			return nil, nil, fmt.Errorf("read the current policy: %w", err)
		}
		objects := &factoryd.S3{
			Endpoint:  daemonURL("S3_ENDPOINT"),
			Region:    env("S3_REGION", "fsn1"),
			Bucket:    os.Getenv("S3_BUCKET"),
			AccessKey: secrets["S3_ACCESS_KEY_ID"],
			SecretKey: secrets["S3_SECRET_ACCESS_KEY"],
		}
		return newFixtureExporter(store, objects, v.Tree, scrub, secrets["FACTORYD_DATABASE_URL"], os.Getenv("CONFIG_REPO")), func() { _ = store.Close() }, nil
	}
}

// unsetConfiguration names every required secret of daemonSecrets that is
// unset, and the object store's endpoint and bucket and the config
// repository when getenv has none.
func unsetConfiguration(secrets map[string]string, getenv func(string) string) []string {
	var unset []string
	for _, s := range daemonSecrets {
		if s.required && secrets[s.name] == "" {
			unset = append(unset, s.name)
		}
	}
	for _, name := range []string{"S3_ENDPOINT", "S3_BUCKET", "CONFIG_REPO"} {
		if getenv(name) == "" {
			unset = append(unset, name)
		}
	}
	return unset
}

// newFixtureExporter is the exporter over store and objects, masking the
// hosts the daemon is configured with, the database's among them, the
// config repository and the export's own lists: FIXTURE_MASK, the terms to
// mask, and FIXTURE_KEEP, the @words that name no person.
func newFixtureExporter(store factoryd.Store, objects factoryd.ObjectStore, tree policy.Tree, scrub *redact.Scrubber, databaseURL, configRepo string) *factoryd.FixtureExporter {
	hosts := daemonHosts()
	if u, err := url.Parse(databaseURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	return &factoryd.FixtureExporter{Store: store, Objects: objects, Policy: tree, Scrub: scrub, Hosts: hosts, Repositories: []string{configRepo},
		Mask: envList("FIXTURE_MASK"), Keep: envList("FIXTURE_KEEP"), Now: time.Now}
}

// envList is the comma-separated variable name as its terms, trimmed.
func envList(name string) []string {
	var terms []string
	for _, t := range strings.Split(os.Getenv(name), ",") {
		if t = strings.TrimSpace(t); t != "" {
			terms = append(terms, t)
		}
	}
	return terms
}

func exportFixtureMain(args []string) {
	secrets := loadSecrets(false)
	scrub := redact.New(bootSecrets...)
	if code := runExportFixture(context.Background(), args, os.Stdout, os.Stderr, scrub, fixtureExporterFrom(secrets, scrub)); code != 0 {
		os.Exit(code)
	}
}
