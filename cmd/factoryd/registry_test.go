package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The roller reads main CI's images with the read:package token of
// factory-registry-read (secret forgejo-registry-read), not the
// forgejo-registry pull secret, which stays for image pulls only.
func TestTheRollerReadsTheRegistryWithTheReadToken(t *testing.T) {
	t.Setenv("REGISTRY_READ_USER", "factory-registry-read")
	reg := rollRegistry("http://forgejo.example", "read-tok")
	if reg.BaseURL != "http://forgejo.example" || reg.User != "factory-registry-read" || reg.Password != "read-tok" {
		t.Fatalf("registry = %+v", reg)
	}

	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type envVar struct {
		Name      string `yaml:"name"`
		Value     string `yaml:"value"`
		ValueFrom struct {
			SecretKeyRef struct{ Name, Key string } `yaml:"secretKeyRef"`
		} `yaml:"valueFrom"`
	}
	var envs []envVar
	var secretsRead []string
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Rules    []struct {
				Resources     []string `yaml:"resources"`
				ResourceNames []string `yaml:"resourceNames"`
			} `yaml:"rules"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string   `yaml:"name"`
							Env  []envVar `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		switch {
		case doc.Kind == "Deployment" && doc.Metadata.Name == "factoryd":
			for _, c := range doc.Spec.Template.Spec.Containers {
				if c.Name == "factoryd" {
					envs = c.Env
				}
			}
		case doc.Kind == "Role":
			for _, r := range doc.Rules {
				if slices.Contains(r.Resources, "secrets") {
					secretsRead = append(secretsRead, r.ResourceNames...)
				}
			}
		}
	}
	find := func(name string) envVar {
		i := slices.IndexFunc(envs, func(e envVar) bool { return e.Name == name })
		if i < 0 {
			t.Fatalf("factoryd has no env %s", name)
		}
		return envs[i]
	}
	if u := find("REGISTRY_READ_USER"); u.Value != "factory-registry-read" {
		t.Fatalf("REGISTRY_READ_USER = %+v", u)
	}
	if tok := find("REGISTRY_READ_TOKEN").ValueFrom.SecretKeyRef; tok.Name != "forgejo-registry-read" || tok.Key != "token" {
		t.Fatalf("REGISTRY_READ_TOKEN from %+v", tok)
	}
	// the kubelet hands the token to the container: no Role reads it
	if slices.Contains(secretsRead, "forgejo-registry-read") {
		t.Fatalf("a Role reads forgejo-registry-read: %v", secretsRead)
	}
}
