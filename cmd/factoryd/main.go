package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/health"
)

var version = "dev"

// stateLabels are the labels task 42's webhook intake and state mirroring
// need to exist on the issue-tracked repo before anything files an issue or
// mirrors state onto one (colors are GitHub-style #RRGGBB).
var stateLabels = map[string]string{
	"agent":               "#ededed",
	"human":               "#c2e0c6",
	"state/queued":        "#ededed",
	"state/running":       "#1f6feb",
	"state/waiting-human": "#d4a72c",
	"state/in-review":     "#8250df",
	"state/done":          "#2da44e",
	"state/failed":        "#cf222e",
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		log.Println(version)
		return
	}

	dsn := os.Getenv("FACTORYD_DATABASE_URL")
	if dsn == "" {
		log.Fatal("FACTORYD_DATABASE_URL is required")
	}
	store, err := factoryd.OpenStore(context.Background(), dsn)
	if err != nil {
		log.Fatalf("open task store (boot migrations): %v", err)
	}
	defer store.Close()

	forgejo := &factoryd.Forgejo{
		BaseURL:          env("FORGEJO_URL", "https://git.aisoftwarefactory.world"),
		PublicURL:        env("FORGEJO_PUBLIC_URL", "https://git.aisoftwarefactory.world"),
		BotUser:          env("FORGEJO_BOT_USER", "factory-agent"),
		BotPassword:      mustEnv("FORGEJO_BOT_PASSWORD"),
		CIRegistrarToken: mustEnv("FORGEJO_CI_REGISTRAR_TOKEN"),
	}
	litellm := &factoryd.LiteLLMAdmin{BaseURL: env("LITELLM_URL", "http://litellm.platform.svc:4000"), MasterKey: mustEnv("LITELLM_MASTER_KEY")}
	objects := &factoryd.S3{
		Endpoint:  mustEnv("S3_ENDPOINT"),
		Region:    env("S3_REGION", "fsn1"),
		Bucket:    mustEnv("S3_BUCKET"),
		AccessKey: mustEnv("S3_ACCESS_KEY_ID"),
		SecretKey: mustEnv("S3_SECRET_ACCESS_KEY"),
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: litellm}

	kube, err := factoryd.NewInClusterKube()
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	// the sandbox clones against the in-cluster Forgejo service, not the
	// public host: the CI runner pod (task 41) uses the same endpoint for
	// its own --url, since Cilium's toFQDNs cannot authorize the public
	// host's in-cluster DNS answer (see workerEgressSpec).
	gitBase := env("WORKER_GIT_BASE", "http://forgejo-http.forgejo.svc.cluster.local:3000")
	runner, err := factoryd.NewRunner(factoryd.RunnerConfig{
		AgentNamespacePrefix: env("AGENT_NAMESPACE_PREFIX", "factory-agents-"),
		WorkerImage:          mustEnv("WORKER_IMAGE"),
		PullSecret:           env("PULL_SECRET", "forgejo-registry"),
		FactoryNamespace:     kube.Namespace(),
		GatewayURL:           env("GATEWAY_URL", "http://factory-gateway.factory.svc.cluster.local:8081"),
		GitBaseURL:           gitBase,
		Models: factoryd.ModelLadder{
			Start:    env("MODEL", "factory/coding-cheap"),
			Stronger: env("STRONGER_MODEL", "factory/coding-frontier"),
		},
		AnswerTokens:   envInt("ANSWER_TOKENS", 8192),
		HeadroomTokens: envInt("HEADROOM_TOKENS", 16384),
		MaxSessions:    envInt("MAX_SESSIONS", 3),
		ChecksTimeout:  envDuration("CHECKS_TIMEOUT", 45*time.Minute),
		Supervisor: factoryd.SupervisorConfig{
			NoProgressAfter:  envDuration("SUPERVISE_NO_PROGRESS_AFTER", 5*time.Minute),
			WallClockCap:     envDuration("WALL_CLOCK_CAP", 20*time.Minute),
			RepetitionEvents: envInt("SUPERVISE_READING_WITHOUT_WRITING", 24),
			SameFailure:      envInt("SUPERVISE_SAME_FAILURE", factoryd.DefaultSameFailure),
			SameCall:         envInt("SUPERVISE_SAME_CALL", factoryd.DefaultSameCall),
			Churn:            envInt("SUPERVISE_CHURN", factoryd.DefaultChurn),
			MaxCalls:         envInt("SUPERVISOR_MAX_CALLS", factoryd.DefaultMaxCalls),
		},
	}, store, kube, forgejo, completion, objects, &factoryd.SupervisorModel{
		BaseURL: litellm.BaseURL,
		Admin:   litellm,
		Alias:   env("SUPERVISOR_MODEL", "supervisor"),
		Budget: factoryd.TokenBudget{
			AnswerTokens:   envInt("SUPERVISOR_ANSWER_TOKENS", 2048),
			HeadroomTokens: envInt("SUPERVISOR_HEADROOM_TOKENS", 16384),
		},
	})
	if err != nil {
		log.Fatalf("runner: %v", err)
	}
	ciSpawner, err := factoryd.NewCISpawner(factoryd.CIConfig{
		FactoryNamespace: kube.Namespace(),
		Owner:            env("CI_REPO_OWNER", "sf"),
		Repo:             env("CI_REPO_NAME", "ai-software-factory-code"),
		RunnerImage:      mustEnv("CI_RUNNER_IMAGE"),
		PullSecret:       env("PULL_SECRET", "forgejo-registry"),
		GitBaseURL:       gitBase,
	}, forgejo, kube)
	if err != nil {
		log.Fatalf("ci spawner: %v", err)
	}

	temporal, err := client.Dial(client.Options{
		HostPort:  env("TEMPORAL_ADDRESS", "temporal-frontend.platform.svc.cluster.local:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "default"),
		Logger:    tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))),
	})
	if err != nil {
		log.Fatalf("temporal client: %v", err)
	}
	defer temporal.Close()
	workflows := factoryd.TemporalWorkflows{Client: temporal}
	taskWorker := worker.New(temporal, factoryd.TaskQueue, worker.Options{})
	taskWorker.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig()}.Run,
		workflow.RegisterOptions{Name: "TaskWorkflow"})
	runner.RegisterActivities(taskWorker)

	intakeToken := mustEnv("INTAKE_TOKEN")
	maxBudget := envFloat("MAX_BUDGET_EUR", 5)
	webhookIntake := &factoryd.Intake{
		Store: store, Forgejo: forgejo, Workflows: workflows, BotUser: forgejo.BotUser,
		Secret: mustEnv("FORGEJO_WEBHOOK_SECRET"), MaxBudgetEUR: maxBudget,
	}
	issueOwner, issueRepo := env("CI_REPO_OWNER", "sf"), env("CI_REPO_NAME", "ai-software-factory-code")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := forgejo.EnsureLabels(ctx, issueOwner, issueRepo, stateLabels); err != nil {
		log.Printf("ensure labels: %v", err)
	}
	runner.BootSweep(ctx)
	if err := taskWorker.Start(); err != nil {
		log.Fatalf("temporal worker: %v", err)
	}
	go func() {
		for {
			factoryd.HealPass(ctx, store, workflows)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	go completion.SettleLoop(ctx, time.Minute)
	go ciSpawner.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/", health.Handler("factoryd", version))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			http.Error(w, "store unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok")) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
	})
	mux.Handle("POST /tasks", requireBearer(intakeToken, createTaskHandler(store, forgejo, workflows, maxBudget)))
	mux.Handle("POST /webhooks/forgejo", webhookIntake)
	mux.Handle("GET /tasks/{id}", requireBearer(intakeToken, func(w http.ResponseWriter, r *http.Request) {
		task, ok, err := store.Task(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(task) // #nosec G104 -- best-effort response write
	}))

	mux.Handle("GET /tasks/{id}/timeline", requireBearer(intakeToken, recordHandler(store, objects)))
	mux.Handle("GET /tasks/{id}/sessions/{session}", requireBearer(intakeToken, recordHandler(store, objects)))

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		log.Printf("factoryd: shutting down, stopping the temporal worker")
		// a session activity cut short here is retried by Temporal on the
		// next factoryd, which resumes it from the heartbeat
		taskWorker.Stop()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown) // #nosec G104 -- process exits right after; shutdown error is inconsequential
	}()
	log.Printf("factoryd %s listening on %s", version, *addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// taskCreator is the one store dependency POST /tasks needs, narrower than
// factoryd.Store so a test double is a single method, not the whole
// interface.
type taskCreator interface {
	CreateTask(ctx context.Context, t factoryd.Task) error
}

// createTaskHandler is the API task-filing path (task 42 brief item 3):
// every task is a Forgejo issue, whatever its source, so this files the
// issue — as the bot, title from the objective's first line, body the
// objective plus "filed by factoryd intake" — and labels it agent and
// state/queued before the task row ever exists; a row is created only once
// the issue exists to back it.
func createTaskHandler(store taskCreator, forgejo *factoryd.Forgejo, workflows factoryd.Workflows, maxBudget float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Objective  string  `json:"objective"`
			Repository string  `json:"repository"`
			SHA        string  `json:"sha"`
			Branch     string  `json:"branch"`
			Priority   int     `json:"priority"`
			BudgetEUR  float64 `json:"budget_eur"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Objective == "" || in.Repository == "" {
			http.Error(w, "objective and repository are required", http.StatusBadRequest)
			return
		}
		repoParts, err := factoryd.SplitRepo(in.Repository)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		in.BudgetEUR = factoryd.ClampBudget(in.BudgetEUR, maxBudget)
		id := factoryd.NewTaskID()
		if in.Branch == "" {
			in.Branch = "factory/" + id
		}
		issueBody := in.Objective + "\n\nfiled by factoryd intake"
		ref, err := forgejo.CreateIssue(r.Context(), repoParts.Owner, repoParts.Name, factoryd.FirstLine(in.Objective), issueBody)
		if err != nil {
			http.Error(w, "create issue: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := forgejo.ReplaceIssueLabels(r.Context(), repoParts.Owner, repoParts.Name, ref.Number,
			[]string{"agent", "state/queued"}); err != nil {
			log.Printf("task %s: label issue %d: %v", id, ref.Number, err)
		}
		task := factoryd.Task{
			ID: id, Priority: in.Priority, RiskClass: "standard",
			BudgetEUR: in.BudgetEUR, Status: "queued",
			Objective: in.Objective, Repository: in.Repository,
			SHA: in.SHA, Branch: in.Branch,
			Kind: "agent", IssueNumber: ref.Number, Source: "api",
		}
		if err := store.CreateTask(r.Context(), task); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// a failed start is healed by the next HealPass
		if err := workflows.Start(r.Context(), id); err != nil {
			log.Printf("task %s: start workflow: %v", id, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ // #nosec G104 -- best-effort response write
			"id": id, "branch": in.Branch, "budget_eur": in.BudgetEUR,
			"issue_number": ref.Number, "issue_url": ref.URL,
		})
	}
}

type taskReader interface {
	Task(ctx context.Context, id string) (factoryd.Task, bool, error)
}

// sessionID is the shape factoryd gives session ids (newID("s-")).
var sessionID = regexp.MustCompile(`^s-[0-9a-f]{12}$`)

// recordHandler serves a task's records: its timeline
// (sessions/<id>/timeline.jsonl) or, with a session path value, one
// session's event history (sessions/<id>/<session>.jsonl). Only an
// existing task's id and a well-formed session id reach the object store.
func recordHandler(tasks taskReader, objects factoryd.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := "timeline"
		if s := r.PathValue("session"); s != "" {
			if !sessionID.MatchString(s) {
				http.NotFound(w, r)
				return
			}
			name = s
		}
		task, ok, err := tasks.Task(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := objects.Get(r.Context(), "sessions/"+task.ID+"/"+name+".jsonl")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(body) // #nosec G104 -- best-effort response write
	}
}

// requireBearer admits only requests carrying the intake token.
func requireBearer(token string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is required", name)
	}
	return v
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
		log.Fatalf("%s: not a positive number", name)
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Fatalf("%s: not a positive duration", name)
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Fatalf("%s: not an int", name)
	}
	return def
}
