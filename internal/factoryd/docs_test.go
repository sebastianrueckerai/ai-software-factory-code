package factoryd

import (
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A project that names a document repository (chat.doc_repo) has it in its
// session: the read token covers it, the clone container gets its URL and
// the worker container is told where the clone puts it. A project without
// one gets none of the three.
func TestSessionReadsTheProjectsDocumentRepository(t *testing.T) {
	for _, tc := range []struct {
		name, docRepo string
		reads         string
		docsURL       string
	}{
		{"with", "sf/ai-software-factory", "mint task-t1-base read:repository sf/x,factory-agent/x,sf/ai-software-factory",
			"http://forgejo-http.forgejo.svc.cluster.local:3000/sf/ai-software-factory.git"},
		{"without", "", "mint task-t1-base read:repository sf/x,factory-agent/x", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
			kube := newFakeKube()
			runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
			v := testPolicyVersion()
			v.Projects["x"] = policy.ProjectConfig{Repository: "sf/x", Agents: policy.AgentsCoder, Chat: &policy.ChatConfig{DocRepo: tc.docRepo}}
			runner.cfg.Policy = func() policy.Version { return v }
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
				pushedLine("c0ffee")+"\n"+resultLine(true, "pushed branch factory/t1 at c0ffee")); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(hub.calls, tc.reads) {
				t.Fatalf("forgejo calls = %v, want %q", hub.calls, tc.reads)
			}
			spec := kube.podAt(0).spec["spec"].(map[string]any)
			clone := initContainer(t, spec, "clone")
			cloneEnv, workerEnv := containerEnv(clone), podEnv(kube.podAt(0))
			wantDir := ""
			if tc.docsURL != "" {
				wantDir = "/workspace/docs"
			}
			if cloneEnv["DOCS_URL"] != tc.docsURL || workerEnv["DOCS_DIR"] != wantDir {
				t.Fatalf("DOCS_URL = %q, DOCS_DIR = %q, want %q and %q", cloneEnv["DOCS_URL"], workerEnv["DOCS_DIR"], tc.docsURL, wantDir)
			}
			if _, ok := workerEnv["DOCS_URL"]; ok {
				t.Fatalf("worker container env has DOCS_URL: %v", workerEnv)
			}
		})
	}
}

// The brief tells the agent where the documents are, or why they could not
// be read, and says nothing for a project without any.
func TestBriefSaysWhereTheDocumentsAre(t *testing.T) {
	base := harness.TaskBrief{Objective: "fix it", Repository: "sf/x", AuthorStanding: harness.StandingWriter}
	if got := objectiveText(base); strings.Contains(got, "/workspace/docs") || strings.Contains(got, "document repository") {
		t.Fatalf("brief without documents mentions them:\n%s", got)
	}
	base.Docs = true
	if got := objectiveText(base); !strings.Contains(got, "is at /workspace/docs, text files only, for reading; changes go in /workspace/repo.") {
		t.Fatalf("brief = %s, want the documents' place", got)
	}
	base.Docs, base.DocsFailed = false, "git clone: repository not found"
	got := objectiveText(base)
	if !strings.Contains(got, "could not be read: git clone: repository not found") || strings.Contains(got, "/workspace/docs") {
		t.Fatalf("brief = %s, want the clone's failure", got)
	}
}
