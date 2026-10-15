package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
)

// Live check that completion's revocation refuses both task credentials.
// It mints a Forgejo token and a LiteLLM key exactly as a task run does,
// proves both work, runs the completion reconcile, and proves both are then
// refused. Credential values never leave the process. Skipped unless every
// FACTORYD_LIVE_* variable is set.
func TestLiveRevocationRefusesTaskCredentials(t *testing.T) {
	env := map[string]string{}
	for _, k := range []string{"FACTORYD_LIVE_FORGEJO_URL", "FACTORYD_LIVE_FORGEJO_BOT_PASSWORD", "FACTORYD_LIVE_LITELLM_URL", "FACTORYD_LIVE_LITELLM_MASTER_KEY"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			t.Skip(k + " not set")
		}
	}
	ctx := context.Background()
	taskID := fmt.Sprintf("t-revoke-%d", time.Now().Unix())
	repo := Repo{Owner: "sf", Name: "ai-software-factory-code"}
	c := &Completion{
		Forgejo: &Forgejo{BaseURL: env["FACTORYD_LIVE_FORGEJO_URL"], BotUser: "factory-agent", BotPassword: env["FACTORYD_LIVE_FORGEJO_BOT_PASSWORD"]},
		LiteLLM: &LiteLLMAdmin{BaseURL: env["FACTORYD_LIVE_LITELLM_URL"], MasterKey: env["FACTORYD_LIVE_LITELLM_MASTER_KEY"]},
	}
	// what production mints: read on the repository, write on the bot's fork
	base, err := c.Forgejo.MintToken(ctx, BaseTokenName(taskID), "read:repository", []Repo{repo})
	if err != nil {
		t.Fatal(err)
	}
	fork := c.Forgejo.Fork(repo)
	tok, err := c.Forgejo.MintToken(ctx, TokenName(taskID), "write:repository", []Repo{fork})
	if err != nil {
		t.Fatal(err)
	}
	minter := &gateway.LiteLLM{BaseURL: env["FACTORYD_LIVE_LITELLM_URL"], MasterKey: env["FACTORYD_LIVE_LITELLM_MASTER_KEY"]}
	key, err := minter.Mint(ctx, taskID, taskID, []string{"coding-cheap"})
	if err != nil {
		t.Fatal(err)
	}

	gitCall := func(r Repo, secret string) int {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/repos/%s/%s", env["FACTORYD_LIVE_FORGEJO_URL"], r.Owner, r.Name), nil)
		req.Header.Set("Authorization", "token "+secret)
		return status(t, req)
	}
	modelCall := func() int {
		req, _ := http.NewRequest(http.MethodPost, env["FACTORYD_LIVE_LITELLM_URL"]+"/v1/chat/completions",
			strings.NewReader(`{"model":"coding-cheap","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		return status(t, req)
	}

	before := [3]int{gitCall(repo, base.Secret), gitCall(fork, tok.Secret), modelCall()}
	if err := c.reconcile(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	after := [3]int{gitCall(repo, base.Secret), gitCall(fork, tok.Secret), modelCall()}
	t.Logf("task %s: base token %d -> %d, fork token %d -> %d, litellm key %d -> %d", taskID, before[0], after[0], before[1], after[1], before[2], after[2])
	if before != [3]int{http.StatusOK, http.StatusOK, http.StatusOK} {
		t.Fatalf("before revocation = %v, want all 200", before)
	}
	if after != [3]int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusUnauthorized} {
		t.Fatalf("after revocation = %v, want all 401", after)
	}
}

func status(t *testing.T, req *http.Request) int {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}
