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
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// parseRegistration reads register-project's flags: the project's name
// (its settings are its file's), the site admin and --rotate.
func parseRegistration(args []string) (name, admin string, rotate bool, err error) {
	fs := flag.NewFlagSet("register-project", flag.ContinueOnError)
	fs.StringVar(&name, "name", "", "the project: its file projects/<name>.yaml says objects factory")
	fs.StringVar(&admin, "admin-user", "sebastian", "the Forgejo site admin; the password is read from stdin")
	fs.BoolVar(&rotate, "rotate", false, "mint the project's tokens again")
	if err := fs.Parse(args); err != nil {
		return "", "", false, err
	}
	if name == "" || fs.NArg() > 0 {
		return "", "", false, errors.New("register-project --name <project> [--admin-user <user>] [--rotate]; the settings are projects/<name>.yaml's")
	}
	return name, admin, rotate, nil
}

// registerProject is `factoryd register-project`: Registrar.Register with
// the Forgejo admin's password from stdin, then the gateway YAML the infra
// commit adds for the project's hostnames.
func registerProject(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, reg *factoryd.Registrar) error {
	name, adminUser, rotate, err := parseRegistration(args)
	if err != nil {
		return err
	}
	password, err := bufio.NewReader(stdin).ReadString('\n')
	if password = strings.TrimSpace(password); password == "" {
		return errors.Join(errors.New("the Forgejo admin's password is read from stdin"), err)
	}
	p, err := reg.Register(ctx, name, factoryd.AdminAuth{User: adminUser, Password: password}, rotate)
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
	if err := registerProject(ctx, args, os.Stdin, os.Stdout, reg); err != nil {
		log.Fatalf("register-project: %v", err)
	}
}

func newForgejo() *factoryd.Forgejo {
	return &factoryd.Forgejo{
		BaseURL:       env("FORGEJO_URL", "https://git.aisoftwarefactory.world"),
		PublicURL:     env("FORGEJO_PUBLIC_URL", "https://git.aisoftwarefactory.world"),
		BotUser:       env("FORGEJO_BOT_USER", "factory-agent"),
		BotPassword:   mustEnv("FORGEJO_BOT_PASSWORD"),
		CIOrgToken:    mustEnv("FORGEJO_CI_ORG_TOKEN"),
		ProjectsToken: mustEnv("FORGEJO_PROJECTS_TOKEN"),
		// the one place a seed project's secrets are read: its file names
		// them (policy.SeedTokens)
		Seeded: map[string]string{
			policy.SecretCIRegistrar: mustEnv("FORGEJO_CI_REGISTRAR_TOKEN"),
			policy.SecretMerge:       mustEnv("FORGEJO_MERGE_TOKEN"),
			policy.SecretConfig:      mustEnv("FORGEJO_CONFIG_TOKEN"),
		},
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
