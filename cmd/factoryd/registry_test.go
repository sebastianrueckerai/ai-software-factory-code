package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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

// The roller's key is the public half of main CI's cosign key (roadmap
// 46), the imager's the images key's: factoryd reads each from its
// ConfigMap, which the kustomization makes from the file in k8s/ with a
// hashed name, so a new key restarts factoryd.
func TestFactorydTakesItsSigningKeysFromTheirConfigMaps(t *testing.T) {
	b, err := os.ReadFile("../../k8s/factoryd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]struct{ Name, Key string }{}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Spec     struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string `yaml:"name"`
							Env  []struct {
								Name      string `yaml:"name"`
								ValueFrom struct {
									ConfigMapKeyRef struct{ Name, Key string } `yaml:"configMapKeyRef"`
								} `yaml:"valueFrom"`
							} `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind != "Deployment" || doc.Metadata.Name != "factoryd" {
			continue
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if c.Name == "factoryd" {
					refs[e.Name] = e.ValueFrom.ConfigMapKeyRef
				}
			}
		}
	}
	k, err := os.ReadFile("../../k8s/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		ConfigMapGenerator []struct {
			Name, Namespace string
			Files           []string
			Options         struct {
				DisableNameSuffixHash bool `yaml:"disableNameSuffixHash"`
			}
		} `yaml:"configMapGenerator"`
	}
	if err := yaml.Unmarshal(k, &kustomization); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ env, configMap, file string }{
		{"COSIGN_PUB", "image-signing", "cosign.pub"},
		{"IMAGES_COSIGN_PUB", "images-signing", "images-cosign.pub"},
	} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(c.env, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
		if got := signingKey(c.env); !got.Equal(&key.PublicKey) {
			t.Fatalf("%s: key %v, want the PEM's", c.env, got)
		}
		if ref := refs[c.env]; ref.Name != c.configMap || ref.Key != c.file {
			t.Fatalf("%s from ConfigMap %+v, want %s's %s", c.env, ref, c.configMap, c.file)
		}
		found := false
		for _, g := range kustomization.ConfigMapGenerator {
			if g.Name != c.configMap {
				continue
			}
			found = true
			if g.Namespace != "factory" || !slices.Equal(g.Files, []string{c.file}) || g.Options.DisableNameSuffixHash {
				t.Fatalf("%s %+v, want factory's, from %s, with a hashed name", c.configMap, g, c.file)
			}
		}
		if !found {
			t.Fatalf("the kustomization generates no ConfigMap %s", c.configMap)
		}
	}
}
