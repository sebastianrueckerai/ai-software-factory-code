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
)

// parseRemoval reads remove-project's flags: the project's name and
// --dry-run.
func parseRemoval(args []string) (name string, dry bool, err error) {
	fs := flag.NewFlagSet("remove-project", flag.ContinueOnError)
	fs.BoolVar(&dry, "dry-run", false, "list the steps and their targets without acting")
	if err := fs.Parse(args); err != nil {
		return "", false, err
	}
	name = strings.Join(fs.Args(), "")
	if name == "" || len(fs.Args()) != 1 {
		return "", false, errors.New("remove-project <name> [--dry-run]")
	}
	return name, dry, nil
}

// removeProject is `factoryd remove-project <name>` (roadmap 272):
// register's inverse, Registrar.Remove, each step's line on stdout.
func removeProject(ctx context.Context, args []string, stdout io.Writer, rem *factoryd.Remover) error {
	name, dry, err := parseRemoval(args)
	if err != nil {
		return err
	}
	lines, err := rem.Remove(ctx, name, dry)
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	return err
}

// removeProjectMain runs the subcommand in the removal's Job, with its
// database, the Forgejo admin's login from OpenBao, its own service
// account and the LiteLLM master key (k8s/remove-project.yaml). The
// removal's Forgejo calls need two logins only: the projects token (the
// repository's push mirrors, the GitHub export) and the site admin's
// (OpenBao's), so this is not register's client with its every
// credential.
func removeProjectMain(args []string) {
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
	rem := &factoryd.Remover{
		Forgejo: &factoryd.Forgejo{BaseURL: env("FORGEJO_URL", "https://git.aisoftwarefactory.world"),
			BotUser: env("FORGEJO_BOT_USER", "factory-agent"), ProjectsToken: mustSecret("FORGEJO_PROJECTS_TOKEN")},
		Kube:  kube,
		Store: store,
		OpenBao: &openbao.Client{BaseURL: env("OPENBAO_URL", "http://openbao.platform.svc.cluster.local:8200"), AuthRole: "factoryd",
			TokenFile: openbao.TokenPath, HTTP: &http.Client{Timeout: 15 * time.Second}},
		LiteLLM:      &factoryd.LiteLLMAdmin{BaseURL: env("LITELLM_URL", "http://litellm.platform.svc:4000"), MasterKey: mustSecret("LITELLM_MASTER_KEY")},
		MergeUser:    env("FORGEJO_MERGE_USER", "factory-merge"),
		GateUser:     env("FORGEJO_GATE_USER", "factory-gate"),
		ProjectsUser: projectsUser()}
	if err := removeProject(ctx, args, os.Stdout, rem); err != nil {
		log.Fatalf("remove-project: %v", err)
	}
}
