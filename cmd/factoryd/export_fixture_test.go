package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

func neverOpened(t *testing.T) func(context.Context) (*factoryd.FixtureExporter, func(), error) {
	return func(context.Context) (*factoryd.FixtureExporter, func(), error) {
		t.Fatal("the exporter was opened before the arguments parsed")
		return nil, nil, nil
	}
}

func TestExportFixtureNamesWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"sf/r"}, "the issue number is missing"},
		{nil, "the repository and the issue number are missing"},
		{[]string{"sf/r", "x"}, `issue "x" is not a number`},
		{[]string{"sf/r", "1", "2"}, "too many arguments"},
	} {
		var out, errOut bytes.Buffer
		if code := runExportFixture(context.Background(), tc.args, &out, &errOut, redact.New(), neverOpened(t)); code != 2 || !strings.Contains(errOut.String(), tc.want) || out.Len() != 0 {
			t.Errorf("args %q = exit %d, stderr %q, stdout %q; want exit 2 naming %q and no stdout", tc.args, code, errOut.String(), out.String(), tc.want)
		}
	}
}

// An error that carries a connection string or a bearer value reaches the
// human's terminal masked.
func TestExportFixtureScrubsItsErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runExportFixture(context.Background(), []string{"sf/r", "7"}, &out, &errOut, redact.New(), func(context.Context) (*factoryd.FixtureExporter, func(), error) {
		return nil, nil, errors.New("open store: dial postgres://factory:hunter2pw@db.internal:5432/f failed; Authorization: Bearer abc123def456")
	})
	if code != 1 || !strings.Contains(errOut.String(), "open store") {
		t.Fatalf("exit %d, stderr %q; want exit 1 naming the failure", code, errOut.String())
	}
	for _, leak := range []string{"hunter2pw", "factory:", "abc123def456"} {
		if strings.Contains(errOut.String(), leak) {
			t.Errorf("stderr holds %q: %q", leak, errOut.String())
		}
	}
}

// The export builds the daemon's scrubber or none: it names every required
// secret that is unset, not only the three it reads itself.
func TestExportFixtureNeedsEveryRequiredSecret(t *testing.T) {
	secrets := map[string]string{}
	for _, s := range daemonSecrets {
		if s.required {
			secrets[s.name] = "value-" + s.name
		}
	}
	delete(secrets, "FORGEJO_GATE_TOKEN")
	delete(secrets, "LITELLM_MASTER_KEY")
	secrets["TELEGRAM_BOT_KEY"] = ""
	t.Setenv("S3_ENDPOINT", "")
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("CONFIG_REPO", "")
	_, _, err := fixtureExporterFrom(secrets, redact.New())(context.Background())
	if err == nil {
		t.Fatal("open with required secrets unset = nil")
	}
	for _, want := range []string{"FORGEJO_GATE_TOKEN", "LITELLM_MASTER_KEY", "S3_ENDPOINT", "CONFIG_REPO"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	for _, not := range []string{"TELEGRAM_BOT_KEY", "S3_BUCKET", "FORGEJO_CHAT_TOKEN", "value-"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("error %q names %s, which is set or optional", err, not)
		}
	}
}

// The exporter masks the daemon's configured hosts.
func TestExportFixtureMasksTheConfiguredHosts(t *testing.T) {
	ex := newFixtureExporter(nil, nil, policy.Tree{}, redact.New(), "postgres://u:p@db.corp.example:5432/f", "sf/factory-config")
	for _, want := range []string{"git.aisoftwarefactory.world", "forgejo-http.forgejo.svc.cluster.local", "db.corp.example"} {
		if !slices.Contains(ex.Hosts, want) {
			t.Errorf("Hosts = %v, want %q among them", ex.Hosts, want)
		}
	}
	if !slices.Contains(ex.Repositories, "sf/factory-config") {
		t.Errorf("Repositories = %v, want the config repository", ex.Repositories)
	}
}

// The export's own lists come from its environment, comma-separated, with
// blanks around a term dropped: FIXTURE_MASK the terms to mask, FIXTURE_KEEP
// the @words that name no person.
func TestExportFixtureReadsItsListsFromItsEnvironment(t *testing.T) {
	t.Setenv("FIXTURE_MASK", "dave77, corp.example,,Hans Mueller ")
	t.Setenv("FIXTURE_KEEP", "Override,pytest")
	ex := newFixtureExporter(nil, nil, policy.Tree{}, redact.New(), "", "sf/factory-config")
	if want := []string{"dave77", "corp.example", "Hans Mueller"}; !slices.Equal(ex.Mask, want) {
		t.Errorf("Mask = %q, want %q", ex.Mask, want)
	}
	if want := []string{"Override", "pytest"}; !slices.Equal(ex.Keep, want) {
		t.Errorf("Keep = %q, want %q", ex.Keep, want)
	}
}

// Every endpoint the daemon reads is read through daemonURLs, so the
// export's host mask cannot drift from it: no file of the command but
// secrets.go reads an environment variable named like an endpoint (ending
// in _URL, _ENDPOINT, _ADDR, _HOST, _ADDRESS or _BASE), with a default or
// none.
func TestEveryEndpointIsReadThroughDaemonURLs(t *testing.T) {
	endpoint := regexp.MustCompile(`\b(?:env|mustEnv|os\.Getenv|os\.LookupEnv)\("([A-Z0-9_]+(?:_URL|_ENDPOINT|_ADDR|_HOST|_ADDRESS|_BASE))"`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") || f.Name() == "secrets.go" {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range endpoint.FindAllStringSubmatch(string(src), -1) {
			t.Errorf("%s reads %s directly: read it with daemonURL, so daemonURLs holds it", f.Name(), m[1])
		}
	}
	for _, name := range []string{"S3_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT", "WORKER_GIT_BASE"} {
		if _, ok := daemonURLs[name]; !ok {
			t.Errorf("daemonURLs lacks %s", name)
		}
	}
}

// The hosts a fixture must not carry are the daemon's configured ones: the
// default of each endpoint and what its variable overrides.
func TestDaemonHostsAreTheConfiguredEndpoints(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "https://s3.cloud.example:9000")
	t.Setenv("LITELLM_URL", "http://llm.corp.example:4000")
	hosts := daemonHosts()
	for _, want := range []string{"s3.cloud.example", "llm.corp.example", "git.aisoftwarefactory.world", "temporal-frontend.platform.svc.cluster.local", "forgejo-ssh.forgejo.svc.cluster.local"} {
		if !slices.Contains(hosts, want) {
			t.Errorf("daemonHosts = %v, want %q among them", hosts, want)
		}
	}
	if slices.Contains(hosts, "litellm.platform.svc") {
		t.Errorf("daemonHosts = %v holds the default of an overridden endpoint", hosts)
	}
}

// The daemon's scrubber and the export's are built from one list: every
// credential any file of the command loads by name is on it, and a lenient
// load registers what is set.
func TestEveryLoadedSecretIsOnTheDaemonList(t *testing.T) {
	listed := map[string]bool{}
	for _, s := range daemonSecrets {
		listed[s.name] = true
	}
	loads := regexp.MustCompile(`(?:mustSecret|optionalSecret)\("([A-Z0-9_]+)"\)`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range loads.FindAllStringSubmatch(string(src), -1) {
			if !listed[m[1]] {
				t.Errorf("%s loads %s, which is not in daemonSecrets: the export's scrubber would not know it", f.Name(), m[1])
			}
		}
	}
	t.Setenv("INTAKE_TOKEN", "intake-value-one")
	before := len(bootSecrets)
	got := loadSecrets(false)
	if got["INTAKE_TOKEN"] != "intake-value-one" || !slices.Contains(bootSecrets[before:], "intake-value-one") {
		t.Fatalf("a lenient load = %v, registered %v; want the set value loaded and registered", got["INTAKE_TOKEN"], bootSecrets[before:])
	}
}

// A committed fixture holds no host the daemon is configured with, written
// plain or with its separators escaped for a regular expression, and no
// path of the ledger.
func TestCommittedFixturesHoldNoDaemonHostAndNoLedgerPath(t *testing.T) {
	files, err := filepath.Glob("../../internal/factoryd/testdata/fixtures/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures = %v, %v", files, err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.ToLower(string(raw))
		plain := strings.NewReplacer(`\`, "", "[.]", ".").Replace(body)
		for _, host := range daemonHosts() {
			if strings.Contains(plain, strings.ToLower(host)) {
				t.Errorf("%s holds the daemon host %s", filepath.Base(file), host)
			}
		}
		if strings.Contains(body, "."+"superpowers") {
			t.Errorf("%s holds a ledger path", filepath.Base(file))
		}
	}
}
