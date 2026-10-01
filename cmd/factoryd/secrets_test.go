package main

import (
	"os"
	"regexp"
	"testing"
)

// A credential is registered for scrubbing the moment it is loaded:
// mustSecret returns the value like mustEnv and appends it to the list the
// boot scrubber is built from. Fails when mustSecret stops registering what
// it loads (then a loaded secret is absent from the scrubber).
func TestMustSecretRegistersWhatItLoads(t *testing.T) {
	t.Setenv("MUST_SECRET_TEST_ONE", "correct horse battery")
	t.Setenv("MUST_SECRET_TEST_TWO", "staple tumbling weevil")
	before := len(bootSecrets)
	if v := mustSecret("MUST_SECRET_TEST_ONE"); v != "correct horse battery" {
		t.Fatalf("mustSecret returned %q, want the env value", v)
	}
	mustSecret("MUST_SECRET_TEST_TWO")
	loaded := bootSecrets[before:]
	if len(loaded) != 2 || loaded[0] != "correct horse battery" || loaded[1] != "staple tumbling weevil" {
		t.Fatalf("values loaded this test must be registered in order, got %v", loaded)
	}
}

var credentialish = regexp.MustCompile(`TOKEN|PASSWORD|SECRET|KEY|DATABASE_URL`)

// A credential-shaped env name read at boot through bare mustEnv, env or
// os.Getenv is a secret the scrubber never learns about: loading it must go
// through mustSecret, the one place that loads and registers. PULL_SECRET is
// a kube Secret's name, not a credential value. Fails when a new credential
// is added as a bare mustEnv/env/os.Getenv read instead of mustSecret.
func TestEveryBootCredentialIsLoadedThroughMustSecret(t *testing.T) {
	loaded := regexp.MustCompile(`(?:mustEnv|env|os\.Getenv)\("([A-Z0-9_]+)"`)
	for _, file := range []string{"main.go", "register.go", "config.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, m := range loaded.FindAllStringSubmatch(string(src), -1) {
			name := m[1]
			if !credentialish.MatchString(name) || name == "PULL_SECRET" {
				continue
			}
			t.Errorf("%s: %s is a credential, load it with mustSecret so the scrubber knows it", file, name)
		}
	}
}
