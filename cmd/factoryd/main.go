package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/health"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

var version = "dev"

// configDir is the configuration tree's directory in the config repository.
const configDir = "factory"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "register-project":
			registerMain(os.Args[2:])
			return
		case "schema", "config":
			os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
		}
	}
	addr := flag.String("addr", ":8080", "listen address")
	consoleAddr := flag.String("console-addr", ":8090", "console listen address (its own Service and route)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		log.Println(version)
		return
	}

	dsn := mustSecret("FACTORYD_DATABASE_URL")
	store, err := factoryd.OpenStore(context.Background(), dsn)
	if err != nil {
		log.Fatalf("open task store (boot migrations): %v", err)
	}
	defer store.Close()
	eventBus := factoryd.NewEventBus()
	store.SetBus(eventBus)
	// no valid configuration version at all is the one configuration
	// failure that stops the boot; a later refused commit is raised and
	// the last valid version holds
	ladder := ladder()
	forgejo := newForgejo()
	configRepo, err := factoryd.SplitRepo(mustEnv("CONFIG_REPO"))
	if err != nil {
		log.Fatalf("CONFIG_REPO: %v", err)
	}
	otelProviders, err := telemetry.Setup(context.Background(), "factoryd")
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		otelProviders.Shutdown(ctx) // #nosec G104 -- fire and forget: the last samples may be lost
	}()
	metrics, err := telemetry.New(otelProviders.Meter.Meter("factoryd"))
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	if err := metrics.Tasks(otelProviders.Analytics.Meter("factoryd")); err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	configAlarm := &factoryd.ConfigAlarm{Store: store, Forgejo: forgejo, Repo: configRepo, Dir: configDir, Metrics: metrics}
	// a ladder model outside the coder class would be refused mid-task
	configSource := &factoryd.ForgejoConfig{Forgejo: forgejo, Token: forgejo.Seeded[policy.SecretConfig], Repo: configRepo, Branch: "main", Dir: configDir}
	policyLoader := &factoryd.PolicyLoader{
		Source: configSource,
		Store:  store, LadderModels: ladder.Aliases(), Rejected: configAlarm.Raise, Loaded: configAlarm.Resolve,
		ReadFailing: configAlarm.ReadFailing}
	if err := policyLoader.Boot(context.Background()); err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("config: version %d (commit %s) current", policyLoader.Current().ID, policyLoader.Current().Commit)

	openActivities, err := factoryd.NewOpenActivities(metrics.Meter)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	if err := factoryd.RegisterAlertCount(metrics.Meter, store.AlertCounts); err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	ciQueue := factoryd.NewCIQueue(metrics)

	litellm := &factoryd.LiteLLMAdmin{BaseURL: env("LITELLM_URL", "http://litellm.platform.svc:4000"), MasterKey: mustSecret("LITELLM_MASTER_KEY")}
	objects := &factoryd.S3{
		Endpoint:  mustEnv("S3_ENDPOINT"),
		Region:    env("S3_REGION", "fsn1"),
		Bucket:    mustEnv("S3_BUCKET"),
		AccessKey: mustSecret("S3_ACCESS_KEY_ID"),
		SecretKey: mustSecret("S3_SECRET_ACCESS_KEY"),
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: litellm, Metrics: metrics}
	// the gateway-factoryd token: the gateway's escalation hand-over to
	// factoryd, and factoryd's model calls on a task's account
	gatewayURL, gatewayToken := env("GATEWAY_URL", "http://factory-gateway.factory.svc.cluster.local:8081"), mustSecret("GATEWAY_TOKEN")

	kube, err := factoryd.NewInClusterKube()
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	// the sandbox clones against the in-cluster Forgejo service, not the
	// public host: the CI runner pod (task 41) uses the same endpoint for
	// its own --url, since Cilium's toFQDNs cannot authorize the public
	// host's in-cluster DNS answer (see workerEgressSpec).
	gitBase := env("WORKER_GIT_BASE", "http://forgejo-http.forgejo.svc.cluster.local:3000")
	// the Temporal client is dialled after the runner: the gate watch gets
	// its signal and the runner its failure reader once it is
	gates := &factoryd.GateWatch{Store: store, Bound: factoryd.PinnedBound(policyLoader.Current, store.PolicyVersion)}
	var workflows factoryd.TemporalWorkflows
	runner, err := factoryd.NewRunner(factoryd.RunnerConfig{
		AgentNamespacePrefix: env("AGENT_NAMESPACE_PREFIX", "factory-agents-"),
		WorkerImage:          mustEnv("WORKER_IMAGE"),
		PullSecret:           env("PULL_SECRET", "forgejo-registry"),
		FactoryNamespace:     kube.Namespace(),
		GatewayURL:           gatewayURL,
		GitBaseURL:           gitBase,
		Models:               ladder,
		AnswerTokens:         envInt("ANSWER_TOKENS", 8192),
		HeadroomTokens:       envInt("HEADROOM_TOKENS", 16384),
		ChecksTimeout:        envDuration("CHECKS_TIMEOUT", 45*time.Minute),
		Supervisor: factoryd.SupervisorConfig{
			NoProgressAfter:  envDuration("SUPERVISE_NO_PROGRESS_AFTER", 5*time.Minute),
			RepetitionEvents: envInt("SUPERVISE_READING_WITHOUT_WRITING", 24),
			SameFailure:      envInt("SUPERVISE_SAME_FAILURE", factoryd.DefaultSameFailure),
			Churn:            envInt("SUPERVISE_CHURN", factoryd.DefaultChurn),
		},
		Policy:        policyLoader.Current,
		PolicyVersion: store.PolicyVersion,
		ReproTimeout:  envDuration("REPRO_TIMEOUT", 10*time.Minute),
		// the gateway commissions servers into the registry; factoryd only
		// admits their addresses into the task's egress
		Leases:       cloudres.AgentStore(store.CloudResources()),
		CIQueue:      ciQueue,
		Metrics:      metrics,
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Gates:        gates,
		LastFailure: func(ctx context.Context, workflowID, activityID string) (string, error) {
			return workflows.LastFailure(ctx, workflowID, activityID)
		},
		Workflows: &workflows,
	}, store, kube, forgejo, completion, objects, &factoryd.SupervisorModel{
		GatewayURL: gatewayURL,
		Token:      gatewayToken,
		Budget: factoryd.TokenBudget{
			AnswerTokens:   envInt("SUPERVISOR_ANSWER_TOKENS", 2048),
			HeadroomTokens: envInt("SUPERVISOR_HEADROOM_TOKENS", 16384),
		},
	})
	if err != nil {
		log.Fatalf("runner: %v", err)
	}
	// build VMs live in the Hetzner CI project, which holds nothing else:
	// the collector deletes whatever there the registry does not know
	hetznerCI := hcloud.New(mustSecret("HETZNER_CI_KEY"))
	cloudRegistry := store.CloudResources()
	nodeIPs, err := cloudres.IPv4Sources(strings.Split(mustEnv("CI_BUILD_VM_SOURCES"), ","))
	if err != nil {
		log.Fatalf("CI_BUILD_VM_SOURCES: %v", err)
	}
	ciCollector := &cloudres.Collector{Project: "ci", Registry: cloudRegistry, Provider: hetznerCI,
		OwnerDone: factoryd.CIOwnerDone(kube), NodeIPs: nodeIPs, ReleaseAttempts: 1}
	const ciJobTimeout = 45 * time.Minute
	buildVMs, err := factoryd.NewBuildVMs(factoryd.BuildVMConfig{
		Max:            envInt("CI_BUILD_VMS_MAX", 2),
		ServerType:     env("CI_BUILD_VM_TYPE", "cx23"),
		AllowedSources: nodeIPs,
		Lease:          ciJobTimeout + 15*time.Minute,
		Metrics:        metrics,
	}, hetznerCI, cloudRegistry, ciCollector)
	if err != nil {
		log.Fatalf("build vms: %v", err)
	}
	ciCollector.Deleted = buildVMs.Deleted
	ciSpawner, err := factoryd.NewCISpawner(factoryd.CIConfig{
		FactoryNamespace: kube.Namespace(),
		Repos:            factoryd.CIReposOf(forgejo, store),
		RunnerImage:      mustEnv("CI_RUNNER_IMAGE"),
		PullSecret:       env("PULL_SECRET", "forgejo-registry"),
		GitBaseURL:       gitBase,
		MaxConcurrent:    envInt("CI_MAX_CONCURRENT", 8),
		JobTimeout:       ciJobTimeout,
		Queue:            ciQueue,
	}, forgejo, kube, buildVMs)
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
	workflows = factoryd.TemporalWorkflows{Client: temporal}
	gates.Signal = workflows.SignalGate
	// a stop closes the activities' worker stop channel and cancels them
	// after the timeout: RunSession stops its worker gracefully within it
	// (the pod's 150 s termination grace covers it, plus the server's 5 s)
	// the boot's remaining credentials, read here so every one of them is
	// registered before the scrubber is built: it knows every secret value
	// factoryd loads, not a subset (ruling 152, item 48)
	intakeToken := mustSecret("INTAKE_TOKEN")
	webhookSecret := mustSecret("FORGEJO_WEBHOOK_SECRET")
	signozWebhookPassword := mustSecret("SIGNOZ_WEBHOOK_PASSWORD")
	signozPassword := mustSecret("SIGNOZ_PASSWORD")
	consoleClientSecret := mustSecret("CONSOLE_CLIENT_SECRET")
	consoleSessionKey := mustSecret("CONSOLE_SESSION_KEY")
	registryReadToken := mustSecret("REGISTRY_READ_TOKEN")
	scrub := redact.New(bootSecrets...)
	taskWorker := worker.New(temporal, factoryd.TaskQueue, workerOptions(scrub, gates, openActivities,
		&factoryd.ActivitySpans{Tracer: otelProviders.Tracer.Tracer("factoryd")}))
	taskWorker.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig}.Run,
		workflow.RegisterOptions{Name: "TaskWorkflow"})
	runner.RegisterActivities(taskWorker)

	webhookIntake := &factoryd.Intake{
		Store: store, Forgejo: forgejo, Workflows: workflows, BotUser: forgejo.BotUser,
		Secret: webhookSecret, Policy: func() policy.Tree { return policyLoader.Current().Tree }, Projects: store, Metrics: metrics,
		Timeline: runner.Timeline(),
	}
	// the factory's own project deploys into factoryd's namespace: its CI
	// runs and its rolls are watched, its writers reach the console's
	// factory-level pages
	own, ok, err := factoryd.OwnProject(context.Background(), store, kube.Namespace())
	if err != nil || !ok {
		log.Fatalf("no project file lists factoryd's namespace %s (%v)", kube.Namespace(), err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner.BootSweep(ctx)
	registrar := newRegistrar(store, forgejo, kube, func() policy.Tree { return policyLoader.Current().Tree }, webhookSecret)
	webhookIntake.MainPushed = registrar.MainPushed
	go registrar.ReconcileLoop(ctx, func() int64 { return policyLoader.Current().ID }, time.Minute, time.Hour)
	if err := taskWorker.Start(); err != nil {
		log.Fatalf("temporal worker: %v", err)
	}
	go func() {
		for {
			factoryd.HealPass(ctx, store, workflows, webhookIntake, completion)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	go completion.SettleLoop(ctx, time.Minute)
	go policyLoader.Watch(ctx, 30*time.Second)
	go factoryd.SweepLoop(ctx, store, time.Minute)
	monthAlarm := &factoryd.MonthAlarm{Store: store, Forgejo: forgejo, Repo: configRepo, Dir: configDir, Config: configSource, Current: policyLoader.Current}
	go monthAlarm.Loop(ctx, time.Minute)
	go ciSpawner.Run(ctx)
	ciRuns := &factoryd.CIRuns{Forgejo: forgejo, Owner: own.Repo().Owner, Repo: own.Repo().Name, Token: forgejo.Access(own).Actions, Metrics: metrics}
	go ciRuns.Run(ctx)
	rolls := &factoryd.RollWatcher{Forgejo: forgejo, Owner: own.Repo().Owner, Repo: own.Repo().Name, Token: forgejo.Access(own).Actions, Branch: "main",
		Namespace: kube.Namespace(), Kube: kube, Store: store, Metrics: metrics, Poll: 15 * time.Second, Timeout: 30 * time.Minute}
	go rolls.Run(ctx)
	// the roll as a factory task (ruling 178): main CI's images of a green
	// commit, read with a read:package token, open the roll's pull request
	registry := rollRegistry(forgejo.BaseURL, registryReadToken)
	log.Printf("roller: reads main CI's images as %s (read:package)", registry.User)
	roller := &factoryd.Roller{Forgejo: forgejo, Store: store, Workflows: workflows, Timeline: runner.Timeline(), Repo: own.Repo(), Token: forgejo.Access(own).Actions, Budget: policyLoader.Budget, Poll: time.Minute,
		LinkHosts: func() []string { return policyLoader.Current().Document.LinkHosts },
		Registry:  registry}
	go roller.Run(ctx)
	revertPass := &factoryd.RevertPass{Book: store, Forgejo: forgejo, Metrics: metrics, Now: time.Now}
	go revertPass.Loop(ctx)
	go ciCollector.Run(ctx)
	go runner.LeaseLoop(ctx, 10*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/", health.Handler("factoryd", version))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			http.Error(w, "store unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok")) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
	})
	mux.Handle("POST /tasks", requireBearer(intakeToken, createTaskHandler(store, forgejo, workflows, runner.Timeline(), policyLoader.Budget)))
	mux.Handle("GET /policy", requireBearer(intakeToken, factoryd.PolicyHandler(store)))
	mux.Handle("GET /costs", requireBearer(intakeToken, factoryd.CostsHandler(store)))
	mux.Handle("POST /webhooks/forgejo", webhookIntake)
	// SigNoz's webhook channel factoryd (the signoz-objects job creates it)
	mux.Handle("POST /webhooks/signoz", &factoryd.AlertIntake{User: "signoz", Password: signozWebhookPassword,
		Namespaces: kube, Projects: store, Store: store, Forgejo: forgejo, Workflows: workflows, Timeline: runner.Timeline(), Budget: policyLoader.Budget,
		LinkHosts: func() []string { return policyLoader.Current().Document.LinkHosts }})
	// the gateway's hand-over of an agent's escalate_to_human
	mux.Handle("POST /internal/escalations", escalationRoute(gatewayToken, store, runner))
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
	// a project's own workflow asks whether anything fires in a namespace
	// of its, with its FACTORY_READ_TOKEN
	mux.Handle("GET /projects/{name}/alerts", factoryd.ProjectAlertsHandler(store, &factoryd.SigNoz{
		BaseURL: env("SIGNOZ_URL", "http://signoz.platform.svc.cluster.local:8080"),
		Email:   mustEnv("SIGNOZ_EMAIL"), Password: signozPassword}, kube))
	mux.Handle("GET /tasks/{id}/sessions/{session}", requireBearer(intakeToken, recordHandler(store, objects)))

	server := &http.Server{
		Addr:              *addr,
		Handler:           telemetry.Handler(otelProviders.Tracer, "factoryd", mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// the console is the only listener the edge gateway reaches; the
	// webhook and intake API above stay in-cluster
	operator := &factoryd.OperatorModel{GatewayURL: gatewayURL, Token: gatewayToken}
	consoleHandler, err := console.New(console.Config{
		PublicURL:        env("CONSOLE_PUBLIC_URL", "https://console.aisoftwarefactory.world"),
		ForgejoURL:       forgejo.BaseURL,
		ForgejoPublicURL: forgejo.PublicURL,
		ClientID:         mustEnv("CONSOLE_CLIENT_ID"),
		ClientSecret:     consoleClientSecret,
		SessionKey:       []byte(consoleSessionKey),
		Tasks:            store,
		Writers:          consoleWriters(store, forgejo.Writers, kube.Namespace(), time.Now),
		Timelines:        objects,
		Policies:         store,
		Costs:            store,
		Approvals:        store,
		ApproveMerge: func(ctx context.Context, taskID string, pr int64, approver string) error {
			return factoryd.ApproveMerge(ctx, store, workflows, taskID, pr, approver)
		},
		MayMove:   webhookIntake.MayMove,
		CloseCard: webhookIntake.CloseCard,
		AcceptPlan: func(ctx context.Context, taskID string, revision int, sha, by string) error {
			return factoryd.AcceptPlan(ctx, store, workflows, taskID, revision, sha, by)
		},
		AnswerPlan: func(ctx context.Context, taskID string, revision, idx int, answer, by string) error {
			return factoryd.AnswerPlan(ctx, store, workflows, taskID, revision, idx, answer, by)
		},
		RejectPlan: func(ctx context.Context, taskID, by, why string) error {
			return factoryd.RejectPlan(ctx, store, workflows, taskID, by, why)
		},
		RetryPlanCard: func(ctx context.Context, taskID, by string) (int64, error) {
			return runner.RetryPlanCard(ctx, taskID, by)
		},
		Resources:  cloudres.AgentStore(store.CloudResources()),
		Alerts:     store,
		Projects:   store,
		Events:     store,
		Bus:        eventBus,
		ConfigRepo: forgejo.PublicURL + "/" + configRepo.String(),
		ConfigDir:  configDir,
		InfraRepo:  configRepo,
		InfraToken: forgejo.Seeded[policy.SecretConfig],
		CheckProject: func(ctx context.Context, name string, file []byte) ([]string, error) {
			return checkProject(ctx, configSource, name, file, ladder.Aliases())
		},
		MainExists: func(ctx context.Context, repo factoryd.Repo) (bool, error) {
			return forgejo.ProjectBranchExists(ctx, repo, "main")
		},
		ReconcileError: registrar.ReconcileError,
		Metrics:        metrics,
		Steer:          runner.MessageSession,
		Chat:           store,
		Operator:       operator,
		Pulses:         store,
		Mirrors: struct {
			*factoryd.PGStore
			*factoryd.Forgejo
		}{store, forgejo},
		// the projects token registration keeps the repositories with, which
		// may add a collaborator where the approver's own token may not
		AddCollaborator:    forgejo.AddProjectCollaborator,
		SetIssueDependency: forgejo.SetIssueDependency,
	})
	if err != nil {
		log.Fatalf("console: %v", err)
	}
	go (&factoryd.Pulser{Store: store, Bus: eventBus, Model: operator, Context: consoleHandler.PulseContext}).Run(ctx)
	consoleServer := &http.Server{
		Addr:              *consoleAddr,
		Handler:           telemetry.Handler(otelProviders.Tracer, "factoryd", consoleHandler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		log.Printf("console listening on %s", *consoleAddr)
		if err := consoleServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("console: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		log.Printf("factoryd: shutting down, stopping the temporal worker")
		// a running session stops its worker gracefully (work in progress
		// pushed) and is retried by Temporal on the next factoryd from
		// its heartbeat
		taskWorker.Stop()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		consoleServer.Shutdown(shutdown) // #nosec G104 -- as below
		server.Shutdown(shutdown)        // #nosec G104 -- process exits right after; shutdown error is inconsequential
	}()
	log.Printf("factoryd %s listening on %s", version, *addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// consoleWriters are the console's writer lists: the writers of every
// project agents work on, by repository, and the factory's own code
// project, the one listing factoryd's namespace ns. A project whose
// writers cannot be read keeps its last list for up to five minutes, then
// is left out, its members refused until it reads again; the own
// project's failure fails the lists, and the console serves its last ones
// for as long.
func consoleWriters(store interface {
	Projects(ctx context.Context) ([]factoryd.Project, error)
}, writersOf func(context.Context, factoryd.Project) ([]string, error), ns string, now func() time.Time) func(context.Context) (string, map[string][]string, error) {
	type listed struct {
		users []string
		at    time.Time
	}
	var mu sync.Mutex
	last := map[string]listed{}
	return func(ctx context.Context) (string, map[string][]string, error) {
		own, ok, err := factoryd.OwnProject(ctx, store, ns)
		if err != nil || !ok {
			return "", nil, errors.Join(fmt.Errorf("no project file lists namespace %s", ns), err)
		}
		ps, err := store.Projects(ctx)
		if err != nil {
			return "", nil, err
		}
		writers := map[string][]string{}
		for _, p := range ps {
			if !p.Coder() && p.Repository != own.Repository {
				continue
			}
			list, err := writersOf(ctx, p)
			mu.Lock()
			l, known := last[p.Repository]
			if err == nil {
				l, known = listed{list, now()}, true
				last[p.Repository] = l
			}
			mu.Unlock()
			switch {
			case err != nil && p.Repository == own.Repository:
				return "", nil, err
			case err != nil && (!known || now().Sub(l.at) >= 5*time.Minute):
				log.Printf("console: the writers of %s are left out: %v", p.Repository, err)
				continue
			case err != nil:
				log.Printf("console: the writers of %s: %v; serving the list of %s", p.Repository, err, l.at.UTC().Format(time.RFC3339))
			}
			writers[p.Repository] = l.users
		}
		return own.Repository, writers, nil
	}
}

// taskCreator is the one store dependency POST /tasks needs, narrower than
// factoryd.Store so a test double is a single method, not the whole
// interface.
type taskCreator interface {
	CreateTask(ctx context.Context, t factoryd.Task) error
	ProjectByRepository(ctx context.Context, repository string) (factoryd.Project, bool, error)
}

// createTaskHandler is the API task-filing path (task 42 brief item 3):
// every task is a Forgejo issue, whatever its source, so this files the
// issue — as the bot, title from the objective's first line, body the
// objective plus "filed by factoryd intake" — and labels it agent and
// state/queued before the task row ever exists; a row is created only once
// the issue exists to back it. A repository no active project names, and a
// project on agents none, are refused with 422. The base is always the default branch's
// head: neither the branch nor the sha is a caller's choice.
func createTaskHandler(store taskCreator, forgejo *factoryd.Forgejo, workflows factoryd.Workflows, tl *factoryd.Timeline, budget func() policy.Budget) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Objective  string  `json:"objective"`
			Repository string  `json:"repository"`
			Priority   int     `json:"priority"`
			BudgetEUR  float64 `json:"budget_eur"`
			// ReproCommand must pass against the pull request head
			// before it can be approved (GateReproduction)
			ReproCommand string `json:"repro_command"`
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
		p, ok, err := store.ProjectByRepository(r.Context(), in.Repository)
		switch {
		case err != nil:
			http.Error(w, "project: "+err.Error(), http.StatusInternalServerError)
			return
		case !ok || !p.Active():
			http.Error(w, "no active project has the repository "+in.Repository, http.StatusUnprocessableEntity)
			return
		case p.Agents != policy.AgentsCoder:
			http.Error(w, "project "+p.Name+" takes no agent tasks (agents: "+p.Agents+" in projects/"+p.Name+".yaml): file an issue for a human",
				http.StatusUnprocessableEntity)
			return
		}
		in.ReproCommand = strings.TrimSpace(in.ReproCommand)
		if strings.Contains(in.ReproCommand, "```") {
			http.Error(w, "repro_command must not contain ```", http.StatusBadRequest)
			return
		}
		in.BudgetEUR = budget().ClampBudget(in.BudgetEUR)
		sha, err := forgejo.DefaultBranchHeadSHA(r.Context(), repoParts.Owner, repoParts.Name, "")
		if err != nil {
			http.Error(w, "base sha: "+err.Error(), http.StatusBadGateway)
			return
		}
		id := factoryd.NewTaskID()
		issueBody := in.Objective + "\n\nfiled by factoryd intake"
		if in.ReproCommand != "" {
			issueBody += "\n\nReproduction, run against the pull request head before approval:\n\n```repro\n" + in.ReproCommand + "\n```"
		}
		task, ref, err := factoryd.FileAgentTask(r.Context(), store, forgejo, workflows, tl, p, factoryd.FirstLine(in.Objective), issueBody, []string{"agent"}, factoryd.Task{
			ID: id, Priority: in.Priority, RiskClass: "standard", BudgetEUR: in.BudgetEUR,
			Objective: in.Objective, SHA: sha, Source: "api", ReproCommand: in.ReproCommand,
			// the intake token's holder files as the bot, with a writer's standing
			Author: forgejo.BotUser, AuthorWriter: true,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ // #nosec G104 -- best-effort response write
			"id": task.ID, "budget_eur": in.BudgetEUR,
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

// escalationRoute is the gateway's hand-over of escalate_to_human, behind
// the gateway-factoryd bearer (secret factory/gateway-factoryd), which
// factoryd and the gateway share: factoryd presents it for its model calls.
func escalationRoute(gatewayToken string, store factoryd.EscalationStore, sessions factoryd.SessionEscalator) http.Handler {
	return requireBearer(gatewayToken, factoryd.EscalationHandler(store, sessions))
}

// requireBearer admits only requests carrying token.
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

// rollRegistry is the roller's registry client: user factory-registry-read
// with its read:package token (secret forgejo-registry-read), which can
// overwrite no tag.
func rollRegistry(baseURL, token string) *factoryd.Registry {
	return &factoryd.Registry{BaseURL: baseURL, User: mustEnv("REGISTRY_READ_USER"), Password: token}
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is required", name)
	}
	return v
}

// bootSecrets is every credential mustSecret has loaded this boot: the
// daemon's error scrubber is built from it, so a secret cannot be loaded
// without being scrubbed (ruling 152, item 48).
var bootSecrets []string

// mustSecret is mustEnv for a credential: the value is registered for
// scrubbing the moment it is loaded.
func mustSecret(name string) string {
	v := mustEnv(name)
	bootSecrets = append(bootSecrets, v)
	return v
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

// workerOptions: a stop closes the activities' worker stop channel and
// cancels them after WorkerStopTimeout; heartbeats go out at most
// HeartbeatEvery apart, so a workflow's cancel reaches a running activity
// within that. extra: GateWatch, the open-activity gauge and the activity
// spans.
func workerOptions(scrub *redact.Scrubber, extra ...interceptor.WorkerInterceptor) worker.Options {
	return worker.Options{
		WorkerStopTimeout:            120 * time.Second,
		MaxHeartbeatThrottleInterval: factoryd.HeartbeatEvery,
		Interceptors:                 append([]interceptor.WorkerInterceptor{&factoryd.TerminalForgejoErrors{Scrub: scrub}}, extra...),
	}
}
