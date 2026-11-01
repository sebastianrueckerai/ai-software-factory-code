package factoryd

import (
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A project that declares a sandbox companion has it in its session: the
// read token covers it, the clone container gets its URL and the worker
// container is told where the clone puts it. A companion without sandbox
// pins only: none of the three.
func TestSessionClonesTheProjectsCompanions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		companion policy.Companion
		reads     string
		urls      string
		dirs      string
	}{
		{"sandbox", policy.Companion{Repository: "sf/infra", Sandbox: true},
			"mint task-t1-base read:repository sf/x,factory-agent/x,sf/infra",
			"http://forgejo-http.forgejo.svc.cluster.local:3000/sf/infra.git",
			"/workspace/companions/infra"},
		{"pin only", policy.Companion{Repository: "sf/infra"},
			"mint task-t1-base read:repository sf/x,factory-agent/x", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
			kube := newFakeKube()
			runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
			store.projects = []Project{{Name: "x", Repository: "sf/x", Companions: []policy.Companion{tc.companion}}}
			v := testPolicyVersion()
			v.Projects["x"] = policy.ProjectConfig{Repository: "sf/x", Agents: policy.AgentsCoder, Companions: []policy.Companion{tc.companion}}
			runner.cfg.Policy = func() policy.Version { return v }
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
				pushedLine("c0ffee")+"\n"+resultLine(true, "pushed branch factory/t1 at c0ffee")); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(hub.calls, tc.reads) {
				t.Fatalf("forgejo calls = %v, want %q", hub.calls, tc.reads)
			}
			spec := kube.podAt(0).spec["spec"].(map[string]any)
			cloneEnv, workerEnv := containerEnv(initContainer(t, spec, "clone")), podEnv(kube.podAt(0))
			if cloneEnv["COMPANIONS"] != tc.urls || workerEnv["COMPANION_DIRS"] != tc.dirs {
				t.Fatalf("COMPANIONS = %q, COMPANION_DIRS = %q, want %q and %q", cloneEnv["COMPANIONS"], workerEnv["COMPANION_DIRS"], tc.urls, tc.dirs)
			}
			if _, ok := workerEnv["COMPANIONS"]; ok {
				t.Fatalf("worker container env has COMPANIONS: %v", workerEnv)
			}
		})
	}
}

// The brief tells the agent where the companions are, or why one could not
// be read, and says nothing for a project without any.
func TestBriefSaysWhereTheCompanionsAre(t *testing.T) {
	base := harness.TaskBrief{Objective: "fix it", Repository: "sf/x", AuthorStanding: harness.StandingWriter}
	if got := objectiveText(base); strings.Contains(got, "companion") {
		t.Fatalf("brief without companions mentions them:\n%s", got)
	}
	base.Companions = []string{"infra at /workspace/companions/infra"}
	if got := objectiveText(base); !strings.Contains(got, "The project's companion repositories are cloned read-only, for reading; changes go in /workspace/repo") ||
		!strings.Contains(got, "- infra at /workspace/companions/infra") {
		t.Fatalf("brief = %s, want the companions' place", got)
	}
	base.Companions = []string{"infra could not be read: git clone: repository not found"}
	if got := objectiveText(base); !strings.Contains(got, "infra could not be read: git clone: repository not found") {
		t.Fatalf("brief = %s, want the clone's failure", got)
	}
}
