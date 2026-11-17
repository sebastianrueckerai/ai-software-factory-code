package acceptance

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	workerharness "github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The carried tests of plan #1931, driven through the production Runner
// over the real store: a kind test criterion names its suite and its
// test's name, the citing card's directories carry it at the filing, the
// gate runs the named test from the head's tree, and a missing test asks
// the ordinary unmet-work question, never a proof gap.

// proofPolicy is the version the runner validates under: a process with
// a plan track, its project sf/code, and the prover class, so a proof
// gap would open a prover if one were owed.
func proofPolicy(t *testing.T) policy.Version {
	t.Helper()
	files := map[string][]byte{
		"policy.yaml": []byte(policy.Header("policy.yaml") + `
version: 1
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20, month_eur: 50}
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder: {session: code, models: [small, big], tools: {}, review_model: big, supervisor_model: supervisor}
  planner: {session: plan, models: [small, big], tools: {}, review_model: big, supervisor_model: supervisor}
  prover: {session: report, models: [small, big], session_model: big, supervisor_model: big, time: {agent_time: 90m, check_in: 0s}, tools: {file_proofs: {}, manual: {}}}
prices:
  max_output_tokens: {small: 32768, big: 32768, supervisor: 32768}
risk:
  classes: [{name: normal, outcome: auto}]
  rules: []
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`),
		"processes/spec.yaml": []byte(policy.Header("processes/spec.yaml") + `
tracks:
  direct: {class: coder}
  plan:
    class: planner
    plan: {max_cards: 12, min_cards: 1, foundation: true, integration: true}
dependencies: {wait_until: done, on_failed: human}
merge: {queue: none}
stages:
  - {name: checks, on_findings: agent, enabled: true}
  - {name: policy, on_findings: fail, enabled: true}
  - {name: review, on_findings: agent, enabled: true}
  - {name: approval, on_findings: human, enabled: true}
  - {name: merge, on_findings: human, enabled: true}
`),
		"projects/sf-code.yaml": []byte(policy.Header("projects/sf-code.yaml") + `
repository: sf/code
merge_rule: human
objects: seed
agents: coder
process: spec
tokens: {read: forgejo-config, actions: forgejo-ci-registrar, merge: forgejo-merge}
`),
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return policy.Version{ID: 9, Commit: "c9", Tree: tree}
}

// proofStore answers the sf-code card's project, whose file the rig's
// policy version holds but the store's empty policy table does not; every
// other read is the real store's.
type proofStore struct{ *factoryd.PGStore }

func (proofStore) ProjectByRepository(_ context.Context, repository string) (factoryd.Project, bool, error) {
	return factoryd.Project{Name: "sf-code", Repository: repository, File: true, Objects: policy.ObjectsSeed}, true, nil
}

// proofRunner wires the real Runner over the real store, its timeline
// recording beside it, the scripted Forgejo and the completion of
// production, over the kube the test hands it.
func proofRunner(t *testing.T, s *factoryd.PGStore, fj string, objects *accObjects, kube factoryd.Kube) *factoryd.Runner {
	t.Helper()
	version := proofPolicy(t)
	litellm := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key/list" {
			_, _ = w.Write([]byte(`{"keys":[],"total_pages":0}`))
		}
	}))
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc",
		SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test", FactoryNamespace: "factory",
		GatewayURL: "http://gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor:    factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:        func() policy.Version { return version },
		PolicyVersion: func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil },
		Leases:        accLeases{}, Gates: &factoryd.GateWatch{}, Workflows: accWorkflows{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Timeline:    &factoryd.Timeline{Objects: objects, Events: s, Store: s},
	}
	forgejo := &factoryd.Forgejo{BaseURL: fj, BotUser: "factory-agent", BotPassword: "bot-pw"}
	completion := &factoryd.Completion{Store: s, Forgejo: forgejo,
		LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm, MasterKey: "mk"}}
	r, err := factoryd.NewRunner(cfg, proofStore{s}, kube, forgejo, completion, objects, &factoryd.SupervisorModel{GatewayURL: "http://127.0.0.1:1", Token: "gw-token",
		Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// proofForgejo is the scripted forge: it mints tokens, takes comments
// (the criteria gate's and the plan's) and mirrors the plan card's state.
type proofForgejo struct {
	mu       sync.Mutex
	comments []string
	tokens   int
}

func (f *proofForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tokens"):
		_, _ = io.WriteString(w, `[]`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tokens"):
		f.tokens++
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"sha1":"read-token"}`)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
		_, _ = io.WriteString(w, `[]`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		body, _ := io.ReadAll(r.Body)
		f.comments = append(f.comments, string(body))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/"):
		_ = json.NewEncoder(w).Encode(map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

// proofKube is the runner's Kubernetes seam for the one proofs pod: the
// pod starts running at loopback the moment it is created, and its log
// answers the scripted body.
type proofKube struct {
	accKube
	mu   sync.Mutex
	log  string
	pods []string
}

func (k *proofKube) CreatePod(_ context.Context, namespace, name string, _ map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pods = append(k.pods, namespace+"/"+name)
	return nil
}

func (k *proofKube) PodDiagnosis(context.Context, string, string) (factoryd.PodDiagnosis, error) {
	return factoryd.PodDiagnosis{Phase: "Running"}, nil
}

func (k *proofKube) PodIP(context.Context, string, string) (string, error) { return "127.0.0.1", nil }

func (k *proofKube) PodLog(context.Context, string, string) (io.ReadCloser, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return io.NopCloser(strings.NewReader(k.log)), nil
}

// proofWorker is the worker's message endpoint on the port the runner
// posts to: it takes the proofs list under its bearer and records it.
type proofWorker struct {
	mu    sync.Mutex
	lists []string
	auth  []string
}

func (w *proofWorker) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(workerharness.MessagePort)))
	if err != nil {
		t.Fatalf("listen on the worker's message port: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.lists = append(w.lists, string(body))
		w.auth = append(w.auth, r.Header.Get("Authorization"))
		w.mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// proofJunit is a JUnit report as the worker returns it: gzipped.
func proofJunit(t *testing.T, cases string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(`<testsuites><testsuite name="p">` + cases + `</testsuite></testsuites>`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// proofPodLog is what the proofs pod prints: the one run, then the
// closing line.
func proofPodLog(t *testing.T, run workerharness.ProofRun) string {
	t.Helper()
	runJSON, err := json.Marshal(workerharness.WorkerEnvelope{Type: "proof", Proof: &run})
	if err != nil {
		t.Fatal(err)
	}
	closing, err := json.Marshal(workerharness.WorkerEnvelope{Type: "proofs", Proofs: &workerharness.Proofs{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(runJSON) + "\n" + string(closing) + "\n"
}

// seedProofPlan seeds the plan card, its integration card and the plan's
// one revision: a kind test criterion naming its suite and its test's
// name, carried by the integration card of its one-card round.
func seedProofPlan(ctx context.Context, t *testing.T, s *factoryd.PGStore) {
	t.Helper()
	if _, err := s.EnsureProjectRows(ctx, []string{"sf-code"}); err != nil {
		t.Fatal(err)
	}
	plan := factoryd.Plan{Summary: "Doors", Round: 1,
		Criteria: []factoryd.PlanCriterion{{ID: "door_open", Kind: factoryd.CriterionTest, Claim: "a door opens for its key",
			Inputs: "POST /doors/1/open with key 7", Expected: "200", Suite: "internal/doors", Name: "TestDoorOpen"}},
		Cards: []factoryd.PlanCard{{Key: "integ", Round: 1, Role: factoryd.RoleIntegration, Title: "Doors"}}}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []factoryd.Task{
		{ID: "t-plan", Repository: "sf/code", IssueNumber: 7, Track: policy.TrackPlan, Kind: "agent",
			Source: factoryd.SourceChat, Status: "done", Process: "spec"},
		{ID: "t-int", Repository: "sf/code", Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "integ",
			Kind: "agent", Track: policy.TrackDirect, Status: "done", IssueNumber: 8},
	} {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", b, "sha"); err != nil {
		t.Fatal(err)
	}
}

// The filing refuses a kind test criterion no card carries: the finding
// names the criterion, its test and its suite, and says who carries it;
// a refused plan stores no revision.
func TestFilingRefusesUncarriedTest(t *testing.T) {
	ctx := context.Background()
	s, _ := accReceiptStore(t, "filing_uncarried")
	if err := s.CreateTask(ctx, factoryd.Task{ID: "t-file", Status: "running", Objective: "Links and redirects",
		Repository: "sf/code", Kind: "agent", Source: factoryd.SourceChat, Track: policy.TrackPlan,
		Process: "spec", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	fj := &proofForgejo{}
	r := proofRunner(t, s, serve(t, fj), &accObjects{files: map[string][]byte{}}, accKube{})
	plan := factoryd.Plan{Spec: &factoryd.Spec{Outcome: "A link shortener: links with slugs that redirect and count clicks.",
		Glossary: []string{"slug: the short key a link gets"}},
		Summary: "The acceptance tests settle the feature.",
		Criteria: []factoryd.PlanCriterion{{ID: "links_crud", Kind: factoryd.CriterionTest, Claim: "a link created through the API is stored",
			Promise: "A link made through the API is stored.", Inputs: "POST /api/links with a url", Expected: "201 and the row in the store",
			Suite: "internal/links", Name: "TestCreateLink"}},
		Cards: []factoryd.PlanCard{{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integrate the acceptance tests",
			Why: "So the spec's criteria hold end to end.", Summary: "It turns the spec's criteria into acceptance tests. They pass against the built feature.",
			Brief: "Turn the spec's criteria into acceptance/ tests.", Directories: []string{"acceptance/", "internal/stats"},
			Acceptance: []factoryd.PlanItem{{Criterion: "links_crud", Text: "TestCreateLink in internal/links"}}, Size: "medium"}}}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.FilePlan(ctx, "t-file", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	refusal := "plan: criterion links_crud: its test TestCreateLink in internal/links is carried by no card: " +
		"cite it in a card whose directories hold the suite, or file a card that writes its test"
	if !slices.Equal(res.Findings, []string{refusal}) {
		t.Fatalf("findings = %q, want the refusal naming the test, its suite and the way to carry it", res.Findings)
	}
	if _, ok, err := s.PlanRevision(ctx, "t-file", 0); err != nil || ok {
		t.Fatalf("revision stored = %t, %v; want none: a refused plan files nothing", ok, err)
	}
}

// The gate runs a carried test criterion's named test from the head's
// tree with no proof row: present and green it meets its criterion, and
// absent at the head is a builder finding naming the missing test, its
// unmet row's evidence the same line. Never a held row or a card line,
// and under a policy that runs a prover, never the prover's wait.
func TestGateRunsCarriedTest(t *testing.T) {
	ctx := context.Background()
	const absent = "criterion door_open: no test TestDoorOpen in ./internal/doors"
	cases := []struct {
		name     string
		report   string
		exit     int
		verdict  string
		evidence string
		findings []string
	}{
		{"present and green", `<testcase name="TestDoorOpen"></testcase>`, 0, "met",
			"criterion door_open: TestDoorOpen in ./internal/doors passed", nil},
		{"absent at the head", `<testcase name="TestOther"></testcase>`, 1, "unmet", absent, []string{absent}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := accReceiptStore(t, fmt.Sprintf("gate_carried_%d", i))
			seedProofPlan(ctx, t, s)
			kube := &proofKube{log: proofPodLog(t, workerharness.ProofRun{ID: "door_open", Started: true,
				Exit: tc.exit, Report: proofJunit(t, tc.report)})}
			fj := &proofForgejo{}
			r := proofRunner(t, s, serve(t, fj), &accObjects{files: map[string][]byte{}}, kube)
			worker := &proofWorker{}
			worker.start(t)
			res, err := r.GateCriteria(ctx, "t-int", 9, "c1")
			if err != nil {
				t.Fatal(err)
			}
			if res.Skipped != "" || res.Escalate != "" || len(res.Card) != 0 || !slices.Equal(res.Findings, tc.findings) {
				t.Fatalf("result = %+v; want the findings %q, no card line and no ask", res, tc.findings)
			}
			if len(kube.pods) != 1 || fj.tokens != 1 {
				t.Fatalf("pods = %v, tokens = %d; want the one proofs pod and its read token", kube.pods, fj.tokens)
			}
			var list []workerharness.Proof
			if len(worker.lists) != 1 || json.Unmarshal([]byte(worker.lists[0]), &list) != nil ||
				!slices.Equal(list, []workerharness.Proof{{ID: "door_open", Suite: "./internal/doors", Name: "TestDoorOpen"}}) {
				t.Fatalf("the worker took = %q; want the one carried test, its suite and its name, with no file", worker.lists)
			}
			rows, err := s.CriteriaVerdicts(ctx, "t-int")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Criterion != "door_open" || rows[0].Verdict != tc.verdict ||
				rows[0].Evidence != tc.evidence || rows[0].Kind != factoryd.CriterionTest ||
				rows[0].Source != factoryd.CriteriaSourceGate || rows[0].DecidedBy != factoryd.FactoryDecider {
				t.Fatalf("rows = %+v; want the one %s row, its evidence %q", rows, tc.verdict, tc.evidence)
			}
			if proofs, err := s.PlanProofs(ctx, "t-plan"); err != nil || len(proofs) != 0 {
				t.Fatalf("proof rows = %+v, %v; want none: nothing is stored or opened", proofs, err)
			}
			met := "0"
			if tc.verdict == "met" {
				met = "1"
			}
			fj.mu.Lock()
			comments := slices.Clone(fj.comments)
			fj.mu.Unlock()
			if !slices.ContainsFunc(comments, func(c string) bool {
				return strings.Contains(c, "factoryd criteria gate on c1: "+met+" of 1 criteria met")
			}) {
				t.Fatalf("comments = %q; want the "+met+" of 1 line on the pull request", comments)
			}
		})
	}
}

// A carried test whose test is missing at the head asks the ordinary
// unmet-work question, never a proof gap: the sweep opens no prover card
// under a policy that runs one, and the plan's one ask names the
// criterion.
func TestNoGapCardForMissingProof(t *testing.T) {
	ctx := context.Background()
	s, db := accReceiptStore(t, "gap_no_proof")
	seedProofPlan(ctx, t, s)
	if err := s.PutCriteriaVerdicts(ctx, "t-int", factoryd.CriteriaSourceGate, factoryd.FactoryDecider, []factoryd.CriterionVerdict{
		{Criterion: "door_open", Verdict: "unmet", Evidence: "criterion door_open: no test TestDoorOpen in ./internal/doors",
			Kind: factoryd.CriterionTest}}); err != nil {
		t.Fatal(err)
	}
	fj := &proofForgejo{}
	r := proofRunner(t, s, serve(t, fj), &accObjects{files: map[string][]byte{}}, accKube{})
	r.CriteriaPass(ctx)
	pending, err := s.ApprovalRequests(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Kind != factoryd.ApprovalCriteria || pending[0].TaskID != "t-plan" ||
		pending[0].Reason != "criteria not met with the plan's cards done: door_open" {
		t.Fatalf("pending = %+v; want the one unmet-work ask naming door_open", pending)
	}
	var provers int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE parent_task = 't-plan' AND source = 'prover'`).Scan(&provers); err != nil {
		t.Fatal(err)
	}
	if provers != 0 {
		t.Fatalf("prover cards = %d; want none: the carried test owes the prover nothing", provers)
	}
}

// The planner guide names the carry rule, and the card facts name the
// tests the cited criteria claim: the guide's sentence says a kind test
// criterion names its suite and its test's name and that the citing
// card - or a proof card the planner files - lands the test, a card
// whose directories hold the suite writes its test, and the plan's
// comment on the plan's issue carries each citing card's facts, the
// cited criterion in full with its suite and its test's name.
func TestGuideAndFactsNameCarriedTests(t *testing.T) {
	ctx := context.Background()
	src, err := os.ReadFile("../internal/factoryd/plan.go")
	if err != nil {
		t.Fatal(err)
	}
	const guide = "its suite and its test's name: the citing card - or a proof card the planner files - lands the test, a card whose directories hold the suite writes its test"
	if !strings.Contains(string(src), guide) {
		t.Fatalf("the planner guide names no carry rule; want the sentence %q", guide)
	}
	s, _ := accReceiptStore(t, "guide_facts")
	if err := s.CreateTask(ctx, factoryd.Task{ID: "t-plan", Repository: "sf/code", IssueNumber: 7,
		Track: policy.TrackPlan, Kind: "agent", Source: factoryd.SourceChat, Status: "queued", Process: "spec"}); err != nil {
		t.Fatal(err)
	}
	plan := factoryd.Plan{Summary: "Links", Round: 1,
		Criteria: []factoryd.PlanCriterion{{ID: "links_crud", Kind: factoryd.CriterionTest, Claim: "a link created through the API is stored",
			Inputs: "POST /api/links with a url", Expected: "201 and the row in the store", Suite: "internal/links", Name: "TestCreateLink"}},
		Cards: []factoryd.PlanCard{{Key: "builder", Round: 1, Role: factoryd.RoleFeature, Title: "Links API", Brief: "Create links.",
			Directories: []string{"internal/links"},
			Acceptance:  []factoryd.PlanItem{{Criterion: "links_crud", Text: "POST /api/links answers 201"}}, Size: "small"}},
		Questions: []factoryd.PlanQuestion{{Text: "Do slugs stay unique across deletes?", Choices: []string{"yes", "no"},
			Recommended: "yes", Grounds: "the spec keeps one row per slug"}}}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", b, "sha"); err != nil {
		t.Fatal(err)
	}
	fj := &proofForgejo{}
	r := proofRunner(t, s, serve(t, fj), &accObjects{files: map[string][]byte{}}, accKube{})
	ref, err := r.RequestPlan(ctx, "t-plan", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Comment == 0 {
		t.Fatalf("ref = %+v; want the plan's comment posted while its question waits", ref)
	}
	const cited = "- links_crud (test): a link created through the API is stored: " +
		"POST /api/links with a url: 201 and the row in the store: internal/links: TestCreateLink"
	fj.mu.Lock()
	comments := slices.Clone(fj.comments)
	fj.mu.Unlock()
	if !slices.ContainsFunc(comments, func(c string) bool {
		return strings.Contains(c, "Criteria the items cite:") && strings.Contains(c, cited)
	}) {
		t.Fatalf("the plan's comment = %q; want the cited criterion's suite and test's name: %q", comments, cited)
	}
}
