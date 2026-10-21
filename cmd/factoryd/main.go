package main

import (
	"context"
	"crypto/ecdsa"
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
		case "remove-project":
			removeProjectMain(os.Args[2:])
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
	objects := &factoryd.S3{
		Endpoint:  mustEnv("S3_ENDPOINT"),
		Region:    env("S3_REGION", "fsn1"),
		Bucket:    mustEnv("S3_BUCKET"),
		AccessKey: mustSecret("S3_ACCESS_KEY_ID"),
		SecretKey: mustSecret("S3_SECRET_ACCESS_KEY"),
	}
	timeline := &factoryd.Timeline{Objects: objects, Metrics: metrics, Events: store, Store: store}
	configAlarm := &factoryd.ConfigAlarm{Store: store, Forgejo: forgejo, Repo: configRepo, Dir: configDir, Metrics: metrics, Timeline: timeline}
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
	var inner factoryd.TemporalWorkflows
	// workflows is the work-bounded seam: every start and signal of a
	// queued card goes through the project's work_in_progress (roadmap 225)
	workflows := factoryd.BoundedWorkflows{Workflows: &inner, Store: store,
		Policy: func() policy.Tree { return policyLoader.Current().Tree }}
	// SigNoz's firing alerts: the project endpoint and the alert card's
	// pre-check (roadmap 181, Stage 18 item 3.12). Its root password is
	// read here so the boot's secret scrubber knows it.
	signozPassword := mustSecret("SIGNOZ_PASSWORD")
	signoz := &factoryd.SigNoz{
		BaseURL: env("SIGNOZ_URL", "http://signoz.platform.svc.cluster.local:8080"),
		Email:   mustEnv("SIGNOZ_EMAIL"), Password: signozPassword}
	runner, err := factoryd.NewRunner(factoryd.RunnerConfig{
		AgentNamespacePrefix: env("AGENT_NAMESPACE_PREFIX", "factory-agents-"),
		WorkerImage:          mustEnv("WORKER_IMAGE"),
		PullSecret:           env("PULL_SECRET", "forgejo-registry"),
		FactoryNamespace:     kube.Namespace(),
		GatewayURL:           gatewayURL,
		GitBaseURL:           gitBase,
		Models:               ladder,
		Bus:                  eventBus,
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
		Timeline:     timeline,
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		// the worker's egress probe passes when these do not answer, so no
		// session start needs them up
		EgressCanary:   env("EGRESS_CANARY", "example.com:443"),
		EgressDirectIP: env("EGRESS_DIRECT_IP", "1.1.1.1:443"),
		Gates:          gates,
		LastFailure: func(ctx context.Context, workflowID, activityID string) (string, error) {
			return inner.LastFailure(ctx, workflowID, activityID)
		},
		Workflows: &workflows,
		// the attempt grant's floor (ruling 322) reads the same price
		// table the gateway's hold math prices a call with
		Prices:       &policy.PriceBook{BaseURL: litellm.BaseURL, MasterKey: litellm.MasterKey},
		ProjectsUser: projectsUser(),
	}, store, kube, forgejo, completion, objects, supervisorModel(gatewayURL, gatewayToken))
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
	// 60 minutes: a cold compile of the three images outran the old
	// 45-minute deadline (run 2376 cancelled at 44m26s) while each
	// buildctl still had budget of its own — pr.yml gives it 2700 s
	// (roadmap 203, step 3). The lease below keeps its margin on top.
	const ciJobTimeout = 60 * time.Minute
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
		RegistryHost:     env("REGISTRY_HOST", "git.aisoftwarefactory.world"),
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
	inner = factoryd.TemporalWorkflows{Client: temporal}
	gates.Signal = inner.SignalGate
	// a stop closes the activities' worker stop channel and cancels them
	// after the timeout: RunSession stops its worker gracefully within it
	// (the pod's 150 s termination grace covers it, plus the server's 5 s)
	// the boot's remaining credentials, read here so every one of them is
	// registered before the scrubber is built: it knows every secret value
	// factoryd loads, not a subset (ruling 152, item 48)
	intakeToken := mustSecret("INTAKE_TOKEN")
	webhookSecret := mustSecret("FORGEJO_WEBHOOK_SECRET")
	signozWebhookPassword := mustSecret("SIGNOZ_WEBHOOK_PASSWORD")
	consoleClientSecret := mustSecret("CONSOLE_CLIENT_SECRET")
	consoleSessionKey := mustSecret("CONSOLE_SESSION_KEY")
	// the mirror tokens are sealed under the session key, as the console's
	// OAuth tokens are (roadmap 273)
	store.SetSealKey([]byte(consoleSessionKey))
	registryReadToken := mustSecret("REGISTRY_READ_TOKEN")
	scrub := redact.New(bootSecrets...)
	taskWorker := worker.New(temporal, factoryd.TaskQueue, workerOptions(scrub, gates, openActivities,
		&factoryd.ActivitySpans{Tracer: otelProviders.Tracer.Tracer("factoryd")}))
	taskWorker.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig}.Run,
		workflow.RegisterOptions{Name: "TaskWorkflow"})
	taskWorker.RegisterWorkflowWithOptions(factoryd.RoutineRunWorkflow, workflow.RegisterOptions{Name: "RoutineRunWorkflow"})
	runner.RegisterActivities(taskWorker)

	webhookIntake := &factoryd.Intake{
		Store: store, Forgejo: forgejo, Workflows: workflows, BotUser: forgejo.BotUser,
		Secret: webhookSecret, Policy: func() policy.Tree { return policyLoader.Current().Tree }, Projects: store, Metrics: metrics,
		Timeline: runner.Timeline(), CloseMerged: runner.CloseMerged, ProjectsUser: projectsUser(),
	}
	// the factory's own project deploys into factoryd's namespace: its CI
	// runs and its rolls are watched, its writers reach the console's
	// factory-level pages
	own, ok, err := factoryd.OwnProject(context.Background(), store, kube.Namespace())
	if err != nil || !ok {
		log.Fatalf("no project file lists factoryd's namespace %s (%v)", kube.Namespace(), err)
	}
	store.SelfRepository = own.Repository

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	schemas, err := policy.Schemas()
	if err != nil {
		log.Fatalf("schemas: %v", err)
	}
	schemaCopier := &factoryd.SchemaCopier{Forgejo: forgejo, Config: configSource, Build: policy.SchemaContentHash(schemas),
		Store: store, Workflows: workflows, Timeline: runner.Timeline(), Budget: policyLoader.Budget, Cards: webhookIntake}
	go func() {
		if err := schemaCopier.SyncOnce(ctx, 3, time.Minute); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("schema copies: %v", err)
		}
	}()
	runner.BootSweep(ctx)
	registrar := newRegistrar(store, forgejo, kube, func() policy.Tree { return policyLoader.Current().Tree }, webhookSecret)
	registrar.Cards, registrar.Infra = store, configSource
	registrar.Workflows, registrar.Timeline = workflows, runner.Timeline()
	webhookIntake.MainPushed = registrar.MainPushed
	// the routines' clocks are Temporal schedules, their merge counters and
	// runs store rows (roadmap 267)
	routines := &factoryd.Routines{Store: store, Events: store, Current: policyLoader.Current,
		Schedules: factoryd.TemporalSchedules{Client: temporal}, Workflows: workflows, Now: time.Now}
	registrar.PushRoutines = routines.MainPushed
	// a routine's run files its card and records the run in the store, and
	// pokes through its routine_ran event and the Poker; policy.yaml's
	// template pass runs the registrar's pass
	runner.Routines, runner.TemplatePass = store, registrar.TemplateUpdates
	// SigNoz's webhook channel factoryd (the signoz-objects job creates it),
	// and the alerts of the factory projects' Kustomizations (roadmap 259)
	alertIntake := &factoryd.AlertIntake{User: "signoz", Password: signozWebhookPassword,
		Namespaces: kube, Projects: store, Store: store, Forgejo: forgejo, Workflows: workflows, Timeline: runner.Timeline(), Budget: policyLoader.Budget,
		Kube: kube, Namespace: kube.Namespace(),
		// a signal card's filing pins the deployed build's commit as its
		// reproduction base (roadmap 194), from the rolls this factoryd
		// records of its own Deployments
		Rolls:     store,
		LinkHosts: func() []string { return policyLoader.Current().Document.LinkHosts },
		Flux:      kube, SigNoz: signoz, Metrics: metrics}
	registrar.FluxAlerts = alertIntake.FluxPass
	go registrar.ReconcileLoop(ctx, func() int64 { return policyLoader.Current().ID }, time.Minute, time.Hour)
	if err := taskWorker.Start(); err != nil {
		log.Fatalf("temporal worker: %v", err)
	}
	go routines.Loop(ctx, 10*time.Second)
	go func() {
		// nudged is the stale sweep's memory of its signals, task id → when
		nudged := map[string]time.Time{}
		for {
			factoryd.HealPass(ctx, store, workflows, webhookIntake, completion)
			factoryd.StaleSweep(ctx, store, workflows, webhookIntake, inner.Alive, nudged)
			runner.CriteriaPass(ctx)
			// SigNoz's rules evaluate every minute, so its list changes no
			// faster: a sweep a minute heals a lost resolve before a card's
			// first model call or a promotion's check reads the row
			if err := alertIntake.SweepSigNoz(ctx, time.Now()); err != nil {
				log.Printf("signoz sweep: %v", err)
			}
			if err := alertIntake.CloseCleared(ctx); err != nil {
				log.Printf("cleared alerts: %v", err)
			}
			// a rollout not settled at its window's cap raises its own alert
			if err := alertIntake.RolloutPass(ctx, time.Now()); err != nil {
				log.Printf("rollout pass: %v", err)
			}
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
	monthAlarm := &factoryd.MonthAlarm{Store: store, Forgejo: forgejo, Repo: configRepo, Dir: configDir, Config: configSource, Current: policyLoader.Current,
		Timeline: runner.Timeline()}
	go monthAlarm.Loop(ctx, time.Minute)
	ciRuns := &factoryd.CIRuns{Forgejo: forgejo, Owner: own.Repo().Owner, Repo: own.Repo().Name, Token: forgejo.Access(own).Actions, Metrics: metrics, Runs: store}
	go ciRuns.Run(ctx)
	rolls := &factoryd.RollWatcher{Forgejo: forgejo, Owner: own.Repo().Owner, Repo: own.Repo().Name, Token: forgejo.Access(own).Actions, Branch: "main",
		Namespace: kube.Namespace(), Kube: kube, Store: store, Metrics: metrics, Poll: 15 * time.Second, Timeout: 30 * time.Minute}
	go rolls.Run(ctx)
	// the roll as a factory task (ruling 178): main CI's images of a green
	// commit, read with a read:package token, open the roll's pull request
	registry := rollRegistry(forgejo.BaseURL, registryReadToken)
	log.Printf("roller: reads main CI's images as %s (read:package)", registry.User)
	roller := &factoryd.Roller{Forgejo: forgejo, Store: store, Workflows: workflows, Timeline: runner.Timeline(), Repo: own.Repo(), Token: forgejo.Access(own).Actions, Budget: policyLoader.Budget, Poll: 15 * time.Second,
		LinkHosts: func() []string { return policyLoader.Current().Document.LinkHosts },
		Registry:  registry, SigningKey: rollSigningKey()}
	go roller.Run(ctx)
	chainPoller := newChainPoller(forgejo, store, policyLoader.Current, workflows, runner.Timeline(), own.Repo(), runner.EndCovered)
	if err := chainPoller.RegisterPollAge(metrics.Meter); err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	webhookIntake.Nudge = chainPoller.Nudge
	go chainPoller.Run(ctx)
	// the factory's own dependency health (roadmap 299): the provider seam
	// is the control-plane probe alone, so a cloud switch stays one value
	var controlPlane factoryd.ControlPlane
	switch env("FACTORY_PROVIDER", "hetzner") {
	case "hetzner":
		controlPlane = hetznerCI
	default:
		log.Fatalf("FACTORY_PROVIDER %q: no control-plane probe", env("FACTORY_PROVIDER", "hetzner"))
	}
	depProber := &factoryd.DepProber{Store: store, Objects: objects, Control: controlPlane,
		Forgejo: forgejo, Owner: own.Repo().Owner, RepoName: own.Repo().Name, Branch: "main",
		PG: store.Ping,
		Temporal: func(ctx context.Context) error {
			_, err := temporal.CheckHealth(ctx, &client.CheckHealthRequest{})
			return err
		},
		Interval: envDuration("DEPENDENCY_HEALTH_INTERVAL", time.Minute)}
	go depProber.Run(ctx)
	revertPass := &factoryd.RevertPass{Book: store, Forgejo: forgejo, Metrics: metrics, Now: time.Now}
	go revertPass.Loop(ctx)
	go ciCollector.Run(ctx)
	go runner.LeaseLoop(ctx, 10*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/", health.Handler("factoryd", version))
	mux.Handle("GET /versionz", health.Version(version))
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
	mux.Handle("POST /webhooks/signoz", alertIntake)
	// the gateway's hand-over of an agent's escalate_to_human
	mux.Handle("POST /internal/escalations", escalationRoute(gatewayToken, store, runner))
	// the gateway's hand-over of an agent's delegate (roadmap 222, item 6)
	mux.Handle("POST /internal/delegations", requireBearer(gatewayToken, factoryd.DelegationHandler(factoryd.DelegationDeps{
		Store: store, Forgejo: forgejo, Workflows: workflows, Timeline: runner.Timeline(), Budget: policyLoader.Budget,
		Policy: func() policy.Tree { return policyLoader.Current().Tree }})))
	// the gateway's hand-over of a planner's file_plan
	mux.Handle("POST /internal/plans", requireBearer(gatewayToken, factoryd.PlanFileHandler(runner)))
	// the gateway's hand-over of an audit session's hold-out verdict
	mux.Handle("POST /internal/holdout-verdict", requireBearer(gatewayToken, runner.AuditVerdictHandler))
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
	mux.Handle("GET /projects/{name}/alerts", factoryd.ProjectAlertsHandler(store, store, kube, store))
	mux.Handle("GET /tasks/{id}/sessions/{session}", requireBearer(intakeToken, recordHandler(store, objects)))

	server := &http.Server{
		Addr:              *addr,
		Handler:           telemetry.Handler(otelProviders.Tracer, "factoryd", mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// the console is the only listener the edge gateway reaches; the
	// webhook and intake API above stay in-cluster
	operator := &factoryd.OperatorModel{GatewayURL: gatewayURL, Token: gatewayToken}
	// the messengers (roadmap 145): Telegram when its bot key is seated,
	// Discord when its application is; their routes ride the console's
	// public host
	publicURL := env("CONSOLE_PUBLIC_URL", "https://console.aisoftwarefactory.world")
	lists := consoleWriters(store, forgejo.Writers, kube.Namespace(), time.Now)
	var messengers *console.Messengers
	var telegram *factoryd.Telegram
	var discord *factoryd.Discord
	if key := optionalSecret("TELEGRAM_BOT_KEY"); key != "" {
		telegram = &factoryd.Telegram{Token: key, PublicURL: publicURL, Store: store}
		if err := telegram.Boot(ctx, publicURL); err != nil {
			log.Printf("messengers: telegram boot: %v", err)
		}
		messengers = &console.Messengers{Secret: factoryd.TelegramSecret(key), Update: telegram.Update,
			Store: store, TelegramName: telegram.Name}
	}
	if id, secret, token := env("DISCORD_CLIENT_ID", ""), optionalSecret("DISCORD_CLIENT_SECRET"),
		optionalSecret("DISCORD_BOT_TOKEN"); id != "" || secret != "" || token != "" {
		if id == "" || secret == "" || token == "" {
			log.Print("messengers: discord needs DISCORD_CLIENT_ID, DISCORD_CLIENT_SECRET and DISCORD_BOT_TOKEN together")
		} else {
			if messengers == nil {
				messengers = &console.Messengers{Store: store}
			}
			discord = &factoryd.Discord{ClientID: id, ClientSecret: secret, BotToken: token,
				PublicURL: publicURL, Store: store}
			messengers.Discord = &console.DiscordConnect{AuthorizeURL: discord.AuthorizeURL, Connect: discord.Connect}
		}
	}
	if telegram != nil || discord != nil {
		sender := &factoryd.Sender{Store: store, Telegram: telegram, Discord: discord, PublicURL: publicURL, Lists: lists}
		go sender.Loop(ctx, 5*time.Second)
	} else {
		// no venue configured: the panel still shows, naming what is
		// missing, instead of the console dropping it outright
		messengers = &console.Messengers{Store: store}
	}
	consoleHandler, err := console.New(console.Config{
		PublicURL:        publicURL,
		Messengers:       messengers,
		ForgejoURL:       forgejo.BaseURL,
		ForgejoPublicURL: forgejo.PublicURL,
		ClientID:         mustEnv("CONSOLE_CLIENT_ID"),
		ClientSecret:     consoleClientSecret,
		SessionKey:       []byte(consoleSessionKey),
		Sessions:         store,
		Tasks:            store,
		Writers:          lists,
		Timelines:        objects,
		Policies:         store,
		Costs:            store,
		Dependencies:     store,
		Approvals:        store,
		ApproveMerge: func(ctx context.Context, taskID string, pr int64, approver string) error {
			return factoryd.ApproveMerge(ctx, store, workflows, taskID, pr, approver)
		},
		RerunChecks: func(ctx context.Context, taskID string, pr int64, by string) error {
			return factoryd.RerunFactoryChecks(ctx, store, workflows, taskID, pr, by)
		},
		CloseFactoryPR: func(ctx context.Context, taskID string, pr int64, by, words string) error {
			return factoryd.CloseFactoryPR(ctx, store, workflows, taskID, pr, by, words)
		},
		MayMove:   webhookIntake.MayMove,
		CloseCard: webhookIntake.CloseCard,
		CloseCardDone: func(ctx context.Context, taskID, by, evidence string) error {
			return runner.CloseCardDone(ctx, taskID, by, evidence)
		},
		SplitCard: func(ctx context.Context, taskID, by string) error {
			return workflows.Signal(ctx, taskID, factoryd.SignalSplit, factoryd.TaskSignal{Author: by})
		},
		AnswerWhole: func(ctx context.Context, taskID string, comment int64, by string) error {
			return workflows.Signal(ctx, taskID, factoryd.SignalAnswerWhole, factoryd.TaskSignal{Author: by, Comment: comment})
		},
		AcceptPlan: func(ctx context.Context, taskID string, revision int, sha, by string) error {
			return factoryd.AcceptPlan(ctx, store, workflows, taskID, revision, sha, by)
		},
		AnswerPlan: func(ctx context.Context, taskID string, revision, idx int, answer, by string) error {
			return factoryd.AnswerPlan(ctx, store, workflows, taskID, revision, idx, answer, by)
		},
		AnswerPlanQuestions: func(ctx context.Context, taskID string, revision int, answers []factoryd.PlanAnswer, by string) error {
			return factoryd.AnswerPlanQuestions(ctx, store, workflows, taskID, revision, answers, by)
		},
		DecideHoldout: func(ctx context.Context, taskID string, round, idx int, verb, by string) error {
			return factoryd.DecideHoldout(ctx, store, taskID, round, idx, verb, by)
		},
		GrantCriteria: func(ctx context.Context, id, by string, approval int64) (int, error) {
			return factoryd.GrantCriteria(ctx, store, id, by, approval)
		},
		DecideCriterion: func(ctx context.Context, id, cid, verdict, by, reason string) error {
			return factoryd.DecideCriterion(ctx, store, id, cid, verdict, by, reason)
		},
		RecordHumanCriteria: func(ctx context.Context, taskID, approver string) (int, error) {
			return factoryd.RecordHumanCriteria(ctx, store, taskID, approver)
		},
		RejectPlan: func(ctx context.Context, taskID, by, why string) error {
			return factoryd.RejectPlan(ctx, store, workflows, taskID, by, why)
		},
		RetryPlanCard: func(ctx context.Context, taskID, by string) (int64, error) {
			return runner.RetryPlanCard(ctx, taskID, by)
		},
		GoCard: func(ctx context.Context, taskID, by string) (int64, error) {
			return runner.Go(ctx, taskID, by)
		},
		Resources:  cloudres.AgentStore(store.CloudResources()),
		Jobs:       factoryd.ClusterJobs{Kube: kube},
		Alerts:     store,
		Projects:   store,
		Events:     store,
		Figures:    store,
		Bus:        eventBus,
		ConfigRepo: forgejo.PublicURL + "/" + configRepo.String(),
		ConfigDir:  configDir,
		InfraRepo:  configRepo,
		InfraToken: forgejo.Seeded[policy.SecretConfig],
		DocsToken:  optionalSecret("FORGEJO_DOCS_TOKEN"),
		CheckProject: func(ctx context.Context, name string, file []byte) ([]string, error) {
			return checkProject(ctx, configSource, name, file, ladder.Aliases())
		},
		CheckChange: func(ctx context.Context, changes map[string][]byte) ([]string, error) {
			return checkChange(ctx, configSource, changes, ladder.Aliases())
		},
		PolicyReload: policyLoader.Load,
		MainExists: func(ctx context.Context, repo factoryd.Repo) (bool, error) {
			return forgejo.ProjectBranchExists(ctx, repo, "main")
		},
		ReconcileError: registrar.ReconcileError,
		Kustomization:  kube.KustomizationState,
		Metrics:        metrics,
		Steer:          runner.MessageSession,
		SessionFunded:  runner.SessionFunded,
		Chat:           store,
		Notes:          store,
		Escalate: func(ctx context.Context, repository string, q factoryd.HumanQuestion, by string) (int64, error) {
			repo, err := factoryd.SplitRepo(repository)
			if err != nil {
				return 0, err
			}
			t, err := factoryd.EscalateToHuman(ctx, store, forgejo, runner.Timeline(), repo, q, by, factoryd.Task{Source: factoryd.SourceChat})
			return t.IssueNumber, err
		},
		AttachAnalysis: store.AttachAnalysis,
		Operator:       operator,
		Mirrors: struct {
			*factoryd.PGStore
			*factoryd.Forgejo
		}{store, forgejo},
		GitHubSync: registrar.GitHubSync,
		Routines:   routines,
		// the projects token registration keeps the repositories with, which
		// may add a collaborator where the approver's own token may not
		AddCollaborator:    forgejo.AddProjectCollaborator,
		SetIssueDependency: forgejo.SetIssueDependency,
		RerunStall:         chainPoller.RerunStall,
		DismissStall:       store.DismissChainStall,
	})
	if err != nil {
		log.Fatalf("console: %v", err)
	}
	go (&factoryd.Poker{Store: store, Current: policyLoader.Current, Line: consoleHandler}).Loop(ctx, 5*time.Second)
	// the wake: an ask the project agent did not file wakes one turn of
	// its own on the venue member's inbox
	go (&factoryd.Waker{Store: store, Current: policyLoader.Current, Start: consoleHandler,
		Forgejo: forgejo, Timeline: runner.Timeline()}).Loop(ctx, 5*time.Second)
	// the needs-you cards' briefs: one small model call each, a pass after
	// they file, so filing never waits on the writer
	go (&factoryd.BriefWriter{Store: store, Current: policyLoader.Current, Model: supervisorModel(gatewayURL, gatewayToken)}).Loop(ctx, 5*time.Second)
	consoleServer := &http.Server{
		Addr:              *consoleAddr,
		Handler:           telemetry.Handler(otelProviders.Tracer, "factoryd", consoleHandler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	consoleServer.RegisterOnShutdown(consoleHandler.EndStreams)
	go func() {
		log.Printf("console listening on %s", *consoleAddr)
		if err := consoleServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("console: %v", err)
		}
	}()
	ciRunCtx, ciRunCancel := context.WithCancelCause(ctx)
	spawnerDone := make(chan struct{})
	go func() {
		defer close(spawnerDone)
		ciSpawner.Run(ciRunCtx)
	}()
	go func() {
		runShutdown(ciRunCtx, ciRunCancel, spawnerDone, taskWorker, hetznerCI, consoleServer, server)
	}()
	log.Printf("factoryd %s listening on %s", version, *addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// workerStopper is the temporal worker's shutdown slice: what runShutdown
// needs of it; worker.Worker satisfies it.
type workerStopper interface {
	Stop()
}

// ciSweeper is the CI sweep's provider slice; factoryd.CISweepProvider,
// which hcloud.Client satisfies.
type ciSweeper = factoryd.CISweepProvider

// errShutdown is the CI spawner context's cancel cause: the process shuts
// down.
var errShutdown = errors.New("factoryd is shutting down")

// runShutdown waits for the process context to end (SIGTERM) and shuts
// the process down in this order (roadmap 189, Stage 18 item 3.15): first
// the CI spawner's own cancellable context is cancelled, and the shutdown
// waits on spawnerDone until the spawner's loop has returned, so no new
// build machine can be created and none is deleted while a job still
// owns it; then the temporal worker stops gracefully (a running session
// pushes its work in progress and Temporal retries it on the next
// factoryd from its heartbeat); then the CI sweep deletes every machine
// labelled factory=ci left in the CI project, with a fresh 30-second
// context, before the servers close and the process exits, so the backups
// and the zero check find no CI machine.
func runShutdown(ciRunCtx context.Context, ciRunCancel context.CancelCauseFunc, spawnerDone <-chan struct{},
	taskWorker workerStopper, ciProvider ciSweeper, consoleServer *http.Server, server *http.Server) {
	<-ciRunCtx.Done()
	log.Printf("factoryd: shutting down, stopping the ci spawner")
	ciRunCancel(errShutdown)
	<-spawnerDone
	taskWorker.Stop()
	sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sweepCancel()
	if err := factoryd.SweepCIMachines(sweepCtx, ciProvider); err != nil {
		log.Printf("factoryd: the ci sweep: %v", err)
	} else {
		log.Printf("factoryd: the ci sweep found no machine left")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consoleServer.Shutdown(shutdown) // #nosec G104 -- as below
	server.Shutdown(shutdown)        // #nosec G104 -- process exits right after; shutdown error is inconsequential
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

// rollSigningKey is the public half of main CI's cosign key
// (k8s/cosign.pub, ConfigMap image-signing): the roller rolls only an
// image signed with it.
func rollSigningKey() *ecdsa.PublicKey {
	key, err := factoryd.ParseSigningKey([]byte(mustEnv("COSIGN_PUB")))
	if err != nil {
		log.Fatalf("COSIGN_PUB: %v", err)
	}
	return key
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

// optionalSecret reads a duty secret that may be absent: empty seats no
// capability, and a present value is registered for scrubbing.
func optionalSecret(name string) string {
	v := os.Getenv(name)
	if v != "" {
		bootSecrets = append(bootSecrets, v)
	}
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

// supervisorModel is factoryd's model client for the runner and the
// needs-you cards' brief writer: one constructor, one configuration.
func supervisorModel(gatewayURL, gatewayToken string) *factoryd.SupervisorModel {
	return &factoryd.SupervisorModel{
		GatewayURL: gatewayURL,
		Token:      gatewayToken,
		Budget: factoryd.TokenBudget{
			AnswerTokens:   envInt("SUPERVISOR_ANSWER_TOKENS", 2048),
			HeadroomTokens: envInt("SUPERVISOR_HEADROOM_TOKENS", 16384),
		},
	}
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

// newChainPoller records every run-mode repository's chain (roadmap 297)
// and ends each covered merged card through end, the runner's EndCovered:
// the roll stays the factory's own repository's engine and writes none of
// it.
func newChainPoller(forgejo *factoryd.Forgejo, store *factoryd.PGStore, current func() policy.Version, workflows factoryd.BoundedWorkflows,
	timeline *factoryd.Timeline, own factoryd.Repo, end func(ctx context.Context, taskID string, run int64) error) *factoryd.ChainPoller {
	return &factoryd.ChainPoller{Forgejo: forgejo, Store: store, Projects: store, Policy: current,
		Workflows: workflows, Timeline: timeline, Own: own, End: end, Now: time.Now, Poll: 15 * time.Second}
}
