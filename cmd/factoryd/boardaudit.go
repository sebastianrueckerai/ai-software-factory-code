package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

const auditBoardUsage = "usage: factoryd audit-board"

// boardAudit is what audit-board reads: the repositories of the projects
// in force and the findings on each one's board.
type boardAudit struct {
	repositories []string
	findings     func(ctx context.Context, repository string) ([]factoryd.BoardFinding, error)
}

// runAuditBoard is `factoryd audit-board`: a line per finding on the board
// of every project, then the count of the cards to fix and of the cards of
// a human's own issue, whose text stays as written. It returns the exit
// code: 0 when no card is left to fix, 1 when one is, 2 when the audit
// could not run. Every line goes through scrub.
func runAuditBoard(ctx context.Context, args []string, stdout, stderr io.Writer, scrub *redact.Scrubber, open func(context.Context) (boardAudit, func(), error)) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, scrub.String(err.Error()))
		return 2
	}
	if len(args) > 0 {
		return fail(fmt.Errorf("%s: it takes no arguments", auditBoardUsage))
	}
	a, done, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer done()
	var lines []string
	fix, kept := map[string]bool{}, map[string]bool{}
	fixes, keeps := 0, 0
	for _, repo := range a.repositories {
		found, err := a.findings(ctx, repo)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", repo, err))
		}
		for _, f := range found {
			card, mark := fmt.Sprintf("%s#%d", repo, f.Issue), ""
			if f.HumanIssue {
				kept[card], keeps, mark = true, keeps+1, " [a human's own issue, kept as written]"
			} else {
				fix[card], fixes = true, fixes+1
			}
			lines = append(lines, fmt.Sprintf("%s %s%s: %s", card, f.Field, mark, f.Refusal))
		}
	}
	lines = append(lines, fmt.Sprintf("Cards to fix: %d (findings: %d). Cards of a human's own issue, kept as written: %d (findings: %d).",
		len(fix), fixes, len(kept), keeps))
	for _, l := range lines {
		fmt.Fprintln(stdout, scrub.String(l))
	}
	if fixes > 0 {
		return 1
	}
	return 0
}

// boardAuditFrom opens the daemon's database for reading only and reads
// the cards' references in Forgejo as the bot, with GET calls alone. It
// prints card text, so it takes every secret the daemon's scrubber knows.
func boardAuditFrom(secrets map[string]string) func(context.Context) (boardAudit, func(), error) {
	return func(ctx context.Context) (boardAudit, func(), error) {
		if unset := unsetSecrets(secrets); len(unset) > 0 {
			return boardAudit{}, nil, fmt.Errorf("audit-board needs the daemon's secrets, so that its scrubber knows every one the daemon's does: %s unset", strings.Join(unset, ", "))
		}
		store, err := factoryd.OpenStoreReadOnly(ctx, secrets["FACTORYD_DATABASE_URL"])
		if err != nil {
			return boardAudit{}, nil, fmt.Errorf("open store: %w", err)
		}
		v, ok, err := store.CurrentPolicy(ctx)
		if err != nil || !ok {
			_ = store.Close()
			if err == nil {
				err = errors.New("no policy version is stored in this database")
			}
			return boardAudit{}, nil, fmt.Errorf("read the current policy: %w", err)
		}
		forgejo := &factoryd.Forgejo{BaseURL: daemonURL("FORGEJO_URL"), BotUser: env("FORGEJO_BOT_USER", "factory-agent"), BotPassword: secrets["FORGEJO_BOT_PASSWORD"]}
		a := boardAudit{findings: func(ctx context.Context, repository string) ([]factoryd.BoardFinding, error) {
			refs, err := factoryd.BotRefs(ctx, forgejo, v.Tree, repository)
			if err != nil {
				return nil, err
			}
			return factoryd.AuditBoard(ctx, store, repository, refs)
		}}
		for _, name := range slices.Sorted(maps.Keys(v.Tree.Projects)) {
			if repo := v.Tree.Projects[name].Repository; repo != "" {
				a.repositories = append(a.repositories, repo)
			}
		}
		return a, func() { _ = store.Close() }, nil
	}
}

func auditBoardMain(args []string) {
	secrets := loadSecrets(false)
	scrub := redact.New(bootSecrets...)
	os.Exit(runAuditBoard(context.Background(), args, os.Stdout, os.Stderr, scrub, boardAuditFrom(secrets)))
}
