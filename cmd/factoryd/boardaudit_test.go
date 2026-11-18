package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// boards is an audit of the boards of order, each with its findings in
// found; the read of repository fail fails.
func boards(order []string, found map[string][]factoryd.BoardFinding, fail string) func(context.Context) (boardAudit, func(), error) {
	return func(context.Context) (boardAudit, func(), error) {
		return boardAudit{repositories: order, findings: func(_ context.Context, repo string) ([]factoryd.BoardFinding, error) {
			if repo == fail {
				return nil, errors.New("read the board: connection refused")
			}
			return found[repo], nil
		}}, func() {}, nil
	}
}

const planWhy = "A card of a plan has no why line. Write in one sentence why the plan needs it."

// The audit prints a line per finding, the card's repository and issue,
// the field, the mark of a human's own issue and the refusal, then the
// count of cards and findings to fix and of a human's own issue. It exits
// 1 while a card is left to fix, 0 when only a human's own issue keeps its
// text, and 2 when it could not run.
func TestAuditBoardPrintsEveryFindingAndGatesOnTheCardsToFix(t *testing.T) {
	human := map[string][]factoryd.BoardFinding{
		"sf/app":  {{Issue: 2090, Field: "why", Refusal: planWhy, HumanIssue: true}},
		"sf/docs": nil,
	}
	fix := map[string][]factoryd.BoardFinding{
		"sf/app": {
			{Issue: 2100, Field: "title", Refusal: "The title has 105 characters. Keep it under 70."},
			{Issue: 2100, Field: "why", Refusal: "The why line gives the reason in one sentence: who needs this, or which problem it removes."},
			{Issue: 2101, Field: "answers", Refusal: `An ask offers 2 to 4 answers, and "Is the work of this card done?" offers 1: Done.`},
		},
		"sf/docs": {{Issue: 12, Field: "title", Refusal: "The title has 2 lines. Write it on one line.", HumanIssue: true}},
	}
	for _, tc := range []struct {
		name  string
		found map[string][]factoryd.BoardFinding
		code  int
		out   string
	}{
		{"an empty board", map[string][]factoryd.BoardFinding{}, 0,
			"Cards to fix: 0 (findings: 0). Cards of a human's own issue, kept as written: 0 (findings: 0).\n"},
		{"a human's own issue", human, 0,
			"sf/app#2090 why [a human's own issue, kept as written]: " + planWhy + "\n" +
				"Cards to fix: 0 (findings: 0). Cards of a human's own issue, kept as written: 1 (findings: 1).\n"},
		{"cards to fix", fix, 1,
			"sf/app#2100 title: The title has 105 characters. Keep it under 70.\n" +
				"sf/app#2100 why: The why line gives the reason in one sentence: who needs this, or which problem it removes.\n" +
				`sf/app#2101 answers: An ask offers 2 to 4 answers, and "Is the work of this card done?" offers 1: Done.` + "\n" +
				"sf/docs#12 title [a human's own issue, kept as written]: The title has 2 lines. Write it on one line.\n" +
				"Cards to fix: 2 (findings: 3). Cards of a human's own issue, kept as written: 1 (findings: 1).\n"},
	} {
		var out, errOut bytes.Buffer
		code := runAuditBoard(context.Background(), nil, &out, &errOut, redact.New(), boards([]string{"sf/app", "sf/docs"}, tc.found, ""))
		if code != tc.code || out.String() != tc.out || errOut.Len() != 0 {
			t.Errorf("%s: exit %d, stdout\n%s\nstderr %q; want exit %d, stdout\n%s", tc.name, code, out.String(), errOut.String(), tc.code, tc.out)
		}
	}

	var out, errOut bytes.Buffer
	if code := runAuditBoard(context.Background(), nil, &out, &errOut, redact.New(), boards([]string{"sf/app", "sf/docs"}, fix, "sf/docs")); code != 2 ||
		!strings.Contains(errOut.String(), "sf/docs: read the board: connection refused") || strings.Contains(out.String(), "Cards to fix") {
		t.Errorf("a failed read: exit %d, stdout %q, stderr %q; want exit 2 naming the repository and no count", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	opened := false
	never := func(context.Context) (boardAudit, func(), error) { opened = true; return boardAudit{}, func() {}, nil }
	if code := runAuditBoard(context.Background(), []string{"sf/app"}, &out, &errOut, redact.New(), never); code != 2 || opened ||
		!strings.Contains(errOut.String(), auditBoardUsage) || out.Len() != 0 {
		t.Errorf("an argument: exit %d, opened %v, stderr %q; want exit 2 with the usage before any read", code, opened, errOut.String())
	}
}

// The audit prints card text, so it takes the daemon's scrubber or none:
// it names every required secret that is unset.
func TestAuditBoardNeedsEveryRequiredSecret(t *testing.T) {
	secrets := map[string]string{}
	for _, s := range daemonSecrets {
		if s.required {
			secrets[s.name] = "value-" + s.name
		}
	}
	delete(secrets, "FORGEJO_BOT_PASSWORD")
	delete(secrets, "LITELLM_MASTER_KEY")
	_, _, err := boardAuditFrom(secrets)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "FORGEJO_BOT_PASSWORD") || !strings.Contains(err.Error(), "LITELLM_MASTER_KEY") ||
		strings.Contains(err.Error(), "value-") || strings.Contains(err.Error(), "FORGEJO_CHAT_TOKEN") {
		t.Fatalf("open with two required secrets unset = %v; want both named and nothing set", err)
	}
}
