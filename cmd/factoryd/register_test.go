package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestParseRegistration(t *testing.T) {
	p, admin, rotate, err := parseRegistration([]string{"--name", "demo-app", "--repository", "sf/demo-app", "--merge-rule", "auto",
		"--environment", "demo-staging=deploy/staging", "--environment", "demo-prod=deploy/prod",
		"--hostname", "demo-staging=demo-staging.aisoftwarefactory.world", "--hostname", "demo-prod=demo.aisoftwarefactory.world",
		"--secret", "demo-write-token/token"}, policy.DefaultProject)
	want := factoryd.Project{Name: "demo-app", Repository: "sf/demo-app", MergeRule: "auto", Managed: true,
		Namespaces: []string{"demo-staging", "demo-prod"},
		Environments: []factoryd.Environment{
			{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"},
			{Namespace: "demo-prod", Path: "deploy/prod", Hostname: "demo.aisoftwarefactory.world"}},
		Quota:   policy.Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
		Secrets: []string{"demo-write-token/token"}}
	if err != nil || !reflect.DeepEqual(p, want) || admin != "sebastian" || rotate {
		t.Fatalf("= %+v, %q, %v, %v", p, admin, rotate, err)
	}
	// the policy's project_default fills what the flags leave out
	p, _, _, err = parseRegistration([]string{"--name", "x", "--repository", "sf/x", "--namespace", "x-ns", "--notify", "alice, bob"},
		policy.ProjectDefault{MergeRule: "human", Quota: policy.Quota{CPU: "1", Memory: "1Gi", Storage: "2Gi"}})
	if err != nil || p.MergeRule != "human" || p.Quota.Storage != "2Gi" || !reflect.DeepEqual(p.Notify, []string{"alice", "bob"}) ||
		!reflect.DeepEqual(p.Namespaces, []string{"x-ns"}) {
		t.Fatalf("defaults = %+v, %v", p, err)
	}
	if _, _, _, err := parseRegistration([]string{"--name", "x", "--repository", "sf/x", "--namespace", "kube-system"}, policy.DefaultProject); err == nil ||
		!strings.Contains(err.Error(), "namespace") {
		t.Fatalf("a system namespace = %v", err)
	}
}
