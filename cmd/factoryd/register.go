package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/openbao"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// parseRegistration reads register-project's flags: the project's name
// (its settings are its file's) and --rotate.
func parseRegistration(args []string) (name string, rotate bool, err error) {
	fs := flag.NewFlagSet("register-project", flag.ContinueOnError)
	fs.StringVar(&name, "name", "", "the project: its file projects/<name>.yaml says objects factory")
	fs.BoolVar(&rotate, "rotate", false, "mint the project's tokens again")
	if err := fs.Parse(args); err != nil {
		return "", false, err
	}
	if name == "" || fs.NArg() > 0 {
		return "", false, errors.New("register-project --name <project> [--rotate]; the settings are projects/<name>.yaml's")
	}
	return name, rotate, nil
}

// registerProject is `factoryd register-project`: Registrar.Register, with
// the Forgejo admin's login from OpenBao, then the gateway YAML the infra
// commit adds for the project's hostnames.
func registerProject(ctx context.Context, args []string, stdout io.Writer, reg *factoryd.Registrar) error {
	name, rotate, err := parseRegistration(args)
	if err != nil {
		return err
	}
	p, err := reg.Register(ctx, name, rotate)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, factoryd.ListenerYAML(p))
	return nil
}

// checkProject is the new-project button's judgement: the tree at infra's
// head with projects/<name>.yaml added, judged as the loader will
// (factoryd.CheckProjectFile); a refusal is its lines.
func checkProject(ctx context.Context, src factoryd.ConfigSource, name string, file []byte, ladder []string) ([]string, error) {
	head, err := src.Head(ctx)
	if err != nil {
		return nil, err
	}
	files, err := src.Files(ctx, head)
	if err != nil {
		return nil, err
	}
	if err := factoryd.CheckProjectFile(files, name, file, ladder); err != nil {
		return strings.Split(err.Error(), "\n"), nil
	}
	return nil, nil
}

// registerMain runs the subcommand in the factoryd pod, with its
// database, tokens and service account.
func registerMain(args []string) {
	ctx := context.Background()
	store, err := factoryd.OpenStore(ctx, mustSecret("FACTORYD_DATABASE_URL"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	kube, err := factoryd.NewInClusterKube()
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	reg := newRegistrar(store, newForgejo(), kube, func() policy.Tree {
		v, _, err := store.CurrentPolicy(ctx)
		if err != nil {
			log.Printf("current policy: %v", err)
		}
		return v.Tree
	}, mustSecret("FORGEJO_WEBHOOK_SECRET"))
	if err := registerProject(ctx, args, os.Stdout, reg); err != nil {
		log.Fatalf("register-project: %v", err)
	}
}

func newForgejo() *factoryd.Forgejo {
	return &factoryd.Forgejo{
		BaseURL:       env("FORGEJO_URL", "https://git.aisoftwarefactory.world"),
		PublicURL:     env("FORGEJO_PUBLIC_URL", "https://git.aisoftwarefactory.world"),
		BotUser:       env("FORGEJO_BOT_USER", "factory-agent"),
		BotPassword:   mustSecret("FORGEJO_BOT_PASSWORD"),
		CIOrgToken:    mustSecret("FORGEJO_CI_ORG_TOKEN"),
		ProjectsToken: mustSecret("FORGEJO_PROJECTS_TOKEN"),
		// the one place a seed project's secrets are read: its file names
		// them (policy.SeedTokens)
		Seeded: map[string]string{
			policy.SecretCIRegistrar: mustSecret("FORGEJO_CI_REGISTRAR_TOKEN"),
			policy.SecretMerge:       mustSecret("FORGEJO_MERGE_TOKEN"),
			policy.SecretConfig:      mustSecret("FORGEJO_CONFIG_TOKEN"),
		},
	}
}

func newRegistrar(store *factoryd.PGStore, forgejo *factoryd.Forgejo, kube factoryd.ProjectKube, tree func() policy.Tree, webhookSecret string) *factoryd.Registrar {
	return &factoryd.Registrar{Forgejo: forgejo, Kube: kube, Store: store, Mirrors: store, GitHubAPI: "https://api.github.com", Policy: tree,
		WebhookURL:       env("FACTORYD_WEBHOOK_URL", "http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo"),
		WebhookSecret:    webhookSecret,
		FactoryNamespace: env("FACTORY_NAMESPACE", "factory"), PullSecret: env("PULL_SECRET", "forgejo-registry"),
		GitURL: env("FORGEJO_PUBLIC_URL", "https://git.aisoftwarefactory.world"), RegistryHost: env("REGISTRY_HOST", "git.aisoftwarefactory.world"),
		MergeUser: env("FORGEJO_MERGE_USER", "factory-merge"),
		HostKey: func(ctx context.Context) (string, error) {
			return factoryd.ScanHostKey(ctx, env("FORGEJO_SSH_ADDR", "forgejo-ssh.forgejo.svc.cluster.local:22"), 10*time.Second)
		},
		ProjectsUser: env("FORGEJO_PROJECTS_USER", "factory-projects"),
		OpenBao: &openbao.Client{BaseURL: env("OPENBAO_URL", "http://openbao.platform.svc.cluster.local:8200"), AuthRole: "factoryd",
			TokenFile: openbao.TokenPath, HTTP: &http.Client{Timeout: 15 * time.Second}}}
}
