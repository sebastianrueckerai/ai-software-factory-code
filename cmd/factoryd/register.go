package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// parseRegistration reads register-project's flags into a managed
// project, its unset settings from the policy's project_default.
func parseRegistration(args []string, def policy.ProjectDefault) (factoryd.Project, string, bool, error) {
	fs := flag.NewFlagSet("register-project", flag.ContinueOnError)
	var namespaces, envs, hosts, secrets listFlag
	name := fs.String("name", "", "project name, [a-z0-9-]: its Forgejo organisation and repository name/name")
	rule := fs.String("merge-rule", def.MergeRule, "auto or human")
	notify := fs.String("notify", "", "comma separated Forgejo users to ping; empty: the policy's approvers")
	cpu := fs.String("quota-cpu", def.Quota.CPU, "each namespace's CPU quota")
	memory := fs.String("quota-memory", def.Quota.Memory, "each namespace's memory quota")
	storage := fs.String("quota-storage", def.Quota.Storage, "each namespace's storage quota")
	admin := fs.String("admin-user", "sebastian", "the Forgejo site admin; the password is read from stdin")
	rotate := fs.Bool("rotate", false, "mint the project's tokens again")
	fs.Var(&namespaces, "namespace", "a namespace the project deploys into (repeatable)")
	fs.Var(&envs, "environment", "namespace=path: one Flux Kustomization (repeatable)")
	fs.Var(&hosts, "hostname", "namespace=hostname for the printed gateway YAML (repeatable)")
	fs.Var(&secrets, "secret", "secret/key: a random value in each namespace when missing (repeatable)")
	if err := fs.Parse(args); err != nil {
		return factoryd.Project{}, "", false, err
	}
	environments, err := factoryd.ParseEnvironments(envs, hosts)
	if err != nil {
		return factoryd.Project{}, "", false, err
	}
	for _, e := range environments {
		if !slices.Contains(namespaces, e.Namespace) {
			namespaces = append(namespaces, e.Namespace)
		}
	}
	var notifyList []string
	for _, u := range strings.Split(*notify, ",") {
		if u = strings.TrimSpace(u); u != "" {
			notifyList = append(notifyList, u)
		}
	}
	p := factoryd.Project{Name: *name, Repository: *name + "/" + *name, MergeRule: *rule, Notify: notifyList, Namespaces: namespaces,
		Environments: environments, Quota: policy.Quota{CPU: *cpu, Memory: *memory, Storage: *storage}, Secrets: secrets, Managed: true}
	return p, *admin, *rotate, p.Validate()
}

// registerProject is `factoryd register-project`: Registrar.Register with
// the Forgejo admin's password from stdin, then the gateway YAML the infra
// commit adds for the project's hostnames.
func registerProject(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, store interface {
	factoryd.ProjectStore
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
}, reg *factoryd.Registrar) error {
	v, ok, err := store.CurrentPolicy(ctx)
	if err != nil || !ok {
		return errors.Join(errors.New("no current policy"), err)
	}
	p, adminUser, rotate, err := parseRegistration(args, v.Document.Project())
	if err != nil {
		return err
	}
	password, err := bufio.NewReader(stdin).ReadString('\n')
	if password = strings.TrimSpace(password); password == "" {
		return errors.Join(errors.New("the Forgejo admin's password is read from stdin"), err)
	}
	p, err = reg.Register(ctx, p, factoryd.AdminAuth{User: adminUser, Password: password}, rotate)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, factoryd.ListenerYAML(p))
	return nil
}

// registerMain runs the subcommand in the factoryd pod, with its
// database, tokens and service account.
func registerMain(args []string) {
	ctx := context.Background()
	store, err := factoryd.OpenStore(ctx, mustEnv("FACTORYD_DATABASE_URL"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	kube, err := factoryd.NewInClusterKube()
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	reg := newRegistrar(store, newForgejo(), kube, func() policy.Document {
		v, _, err := store.CurrentPolicy(ctx)
		if err != nil {
			log.Printf("current policy: %v", err)
		}
		return v.Document
	})
	if err := registerProject(ctx, args, os.Stdin, os.Stdout, store, reg); err != nil {
		log.Fatalf("register-project: %v", err)
	}
}

func newForgejo() *factoryd.Forgejo {
	return &factoryd.Forgejo{
		BaseURL:          env("FORGEJO_URL", "https://git.aisoftwarefactory.world"),
		PublicURL:        env("FORGEJO_PUBLIC_URL", "https://git.aisoftwarefactory.world"),
		BotUser:          env("FORGEJO_BOT_USER", "factory-agent"),
		BotPassword:      mustEnv("FORGEJO_BOT_PASSWORD"),
		CIRegistrarToken: mustEnv("FORGEJO_CI_REGISTRAR_TOKEN"),
		CIOrgToken:       mustEnv("FORGEJO_CI_ORG_TOKEN"),
		MergeToken:       mustEnv("FORGEJO_MERGE_TOKEN"),
		ProjectsToken:    mustEnv("FORGEJO_PROJECTS_TOKEN"),
	}
}

func newRegistrar(store factoryd.ProjectStore, forgejo *factoryd.Forgejo, kube factoryd.ProjectKube, doc func() policy.Document) *factoryd.Registrar {
	return &factoryd.Registrar{Forgejo: forgejo, Kube: kube, Store: store, Policy: doc,
		WebhookURL:       env("FACTORYD_WEBHOOK_URL", "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo"),
		WebhookSecret:    mustEnv("FORGEJO_WEBHOOK_SECRET"),
		FactoryNamespace: env("FACTORY_NAMESPACE", "factory"), PullSecret: env("PULL_SECRET", "forgejo-registry"),
		GitURL: env("FORGEJO_PUBLIC_URL", "https://git.aisoftwarefactory.world"), RegistryHost: env("REGISTRY_HOST", "git.aisoftwarefactory.world"),
		MergeUser: env("FORGEJO_MERGE_USER", "factory-merge"),
		HostKey: func(ctx context.Context) (string, error) {
			return factoryd.ScanHostKey(ctx, env("FORGEJO_SSH_ADDR", "forgejo-ssh.forgejo.svc.cluster.local:22"), 10*time.Second)
		},
		ProjectsUser: env("FORGEJO_PROJECTS_USER", "factory-projects")}
}
