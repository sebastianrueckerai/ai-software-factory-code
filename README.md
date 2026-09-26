# ai-software-factory-code

The factory's own code. Design lives in the `ai-software-factory` repo,
the cluster in `ai-software-factory-infra`. This is the first repository
the agents work on.

## Binaries

- `cmd/factoryd`: the control plane. Task intake (`POST /tasks`,
  `GET /tasks/{id}`, bearer token from secret `factoryd-intake`, budget
  clamped to `MAX_BUDGET_EUR`), the task store in Postgres, one namespace
  and one gVisor pod per task, per-task Forgejo token minting and
  revocation, session records to S3, supervision (stall watchdog,
  wall-clock cap, tool calls without file changes), spend settlement
  against LiteLLM, and the orphan sweep. Runs with `strategy: Recreate`;
  a restart requeues the running task.
- `cmd/factory-gateway`: the enforcement point. Validates the pod's
  projected ServiceAccount JWT, mints a per-task LiteLLM virtual key,
  forwards the call, books each call's cost into the task row, refuses
  with 402 past the budget, and writes an audit row per decision.
- `cmd/agent-worker`: pid 1 in the sandbox pod. Clones with the task
  token before the agent starts, runs opencode in its own process group
  without the token, kills and reaps every other process when the agent
  is done, then fetches the agent's commits into a clean repository and
  pushes from there.

`internal/` holds the packages behind them, `k8s/` the manifests Flux
applies from this repository (namespace `factory`).

## Build and test

    go build ./...
    go test -race -p 1 ./...

The store tests run only with `FACTORYD_TEST_DATABASE_URL` and
`GATEWAY_TEST_DATABASE_URL` set to a throwaway Postgres (they wipe it);
`-p 1` keeps the two packages from racing on the same database. The
revocation test runs only with the `FACTORYD_LIVE_*` variables set
against the live cluster. CI (`.forgejo/workflows/ci.yml`) runs on push
to protected `main` only, tests, then builds the factory image and the
sandbox image into the Forgejo registry.

## Ship

CI pushes images by digest. A deploy is a commit that rolls the digests
in `k8s/factoryd.yaml` and `k8s/deployment.yaml` (factoryd, the gateway
and `WORKER_IMAGE`), pushed to both remotes:

    git push origin main && git push github main
