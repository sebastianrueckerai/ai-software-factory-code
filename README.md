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
  a restart requeues the running task. It also runs the CI spawner (see
  below) in the same process, on the same in-cluster Kubernetes client.
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
to protected `main` only, on the privileged dind runner, tests, then
builds the factory image, the sandbox image and the CI runner image
into the Forgejo registry.

## The untrusted pull-request runner (ruling 78)

`.forgejo/workflows/pr.yml` runs on pull requests from agent branches,
`runs-on: pr-untrusted`. factoryd's CI spawner (`internal/factoryd/ci.go`)
polls Forgejo for Actions jobs waiting on that label, registers a
repo-scoped ephemeral runner (`forgejo-ci-registrar` token, never the
repository or registry credential, never mounted into the runner pod),
and creates a namespace `factory-agents-ci-<job id>` holding one
`runtimeClassName: gvisor` pod running `forgejo-runner one-job --wait` —
non-root, no automounted ServiceAccount token, dropped capabilities,
default-deny egress (`ciEgressSpec`: the in-cluster Forgejo, the Go
module proxy and checksum DB, `data.forgejo.org`, Docker Hub; nothing
else, `169.254.169.254/32` explicitly denied). The namespace and the
ephemeral runner registration are torn down when the pod finishes or its
45-minute deadline passes. Idempotency is the Forgejo job id on the
namespace label, so a restart or a second poll tick never spawns twice.

The runner image (`Dockerfile.runner`) ships forgejo-runner, git, bash,
node, the Go 1.25 toolchain, gosec and BuildKit (`buildctl`/`buildkitd`)
plus a pinned `runc` pre-installed. A live spike (task-41-report.md,
research-stage6-buildkit-gvisor.md) found no *rootless* BuildKit path
comes up under gVisor's systrap platform — RootlessKit's own re-exec is
blocked by the default seccomp profile, and its `newuidmap` fallback
needs privilege escalation, which Kubernetes' `restricted` Pod Security
Standard forbids either way. Ruling 91 resolves it instead: jobs whose
`runs_on` includes `pr-untrusted-build` (`.forgejo/workflows/pr.yml`'s
`image-build` job) get, alongside the usual restricted runner pod, a
second pod in the same namespace running `buildkitd` as sandbox root
(`runAsUser: 0`, no added capabilities, still under gVisor) inside a
nested `unshare --user --mount` with a wide uid/gid map — that
namespace's Pod Security label is `baseline` rather than `restricted`,
which a ValidatingAdmissionPolicy admits only for
`factory-agents-ci-*` namespaces and only for a pod carrying the
`factory.ai/build-pod` label. The runner pod holds no extra
credentials for this; it drives the build pod over `BUILDKIT_HOST`
(`buildctl`), which factoryd's CI spawner injects, and the build never
pushes.

## Ship

CI pushes images by digest. A deploy is a commit that rolls the digests
in `k8s/factoryd.yaml` and `k8s/deployment.yaml` (factoryd, the gateway,
`WORKER_IMAGE` and `CI_RUNNER_IMAGE`), pushed to both remotes:

    git push origin main && git push github main
