# ai-software-factory-code

The factory's own code. Design lives in the `ai-software-factory` repo,
the cluster in `ai-software-factory-infra`. This is the first repository
the agents work on.

## Binaries

- `cmd/factoryd`: the control plane. Task intake (`POST /tasks`,
  `GET /tasks/{id}`, bearer token from secret `factoryd-intake`, budget
  clamped to `MAX_BUDGET_EUR`), the task store in Postgres, and the
  Temporal worker for task queue `factory-tasks`: one `TaskWorkflow` per
  task (workflow id `task-<id>`) runs the attempts, opens the pull
  request, waits for its checks and for the reviewer. Each attempt
  (`RunSession`) gets a fresh namespace, its own Forgejo token and
  service account (so its own LiteLLM key), one gVisor pod, supervision
  (stall watchdog, wall-clock cap, tool calls without file changes) and
  a session record in S3; `sessions/<task>/timeline.jsonl` records the
  task's whole path. Spend settlement against LiteLLM and the orphan
  sweep run beside it. Runs with `strategy: Recreate`; a restart loses
  nothing: Temporal retries the interrupted attempt on the new pod,
  which continues the dead session's chain from its heartbeat. It also
  runs the CI spawner (see below) in the same process, on the same
  in-cluster Kubernetes client.
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

## Webhook intake (task 42)

`POST /webhooks/forgejo` (`internal/factoryd/intake.go`) turns Forgejo
issue, comment and review events into task rows and intake events.
`X-Forgejo-Signature` (hex HMAC-SHA256 of the raw body, secret
`factoryd-webhook`) is checked with a constant-time compare before the
body is parsed at all; a bad or missing signature is a 401 and only the
event type is logged, never the body. An issue labelled `agent` or
`human` becomes a task, idempotent per (repository, issue number) —
redeliveries and later relabel events on the same issue find the task
already linked and no-op. `human` takes priority over `agent` when both
are present, and skips the branch/SHA lookup: that lane never runs the
agent. Issues and comments authored by the bot user itself
(`FORGEJO_BOT_USER`, default `factory-agent`) are ignored, so the
factory's own status updates never create a second task. `POST /tasks`
files a Forgejo issue and labels it the same way, so every task —
API-created or webhook-created — has exactly one linked issue.

Priority is the higher of two label scales, default 0 if neither is present:

| label | priority |
| --- | --- |
| `priority/p0` or `severity/critical` | 3 |
| `priority/p1` or `severity/high` | 2 |
| `priority/p2` or `severity/medium` | 1 |
| `priority/p3` or `severity/low` | 0 |

PR and issue comments, and PR reviews, are recorded as intake events
(`intake_events`, task 43's input) rather than acted on directly: an
issue comment resolves its task by issue number, a PR comment resolves
it by reading the pull request's own head branch (`factory/<task id>`),
and a review reads that same head branch straight off its own payload.
A comment or review on anything the factory did not file — no task
behind that issue number or branch — is silently dropped rather than
erroring.

A task's state (`queued`, `running`, `waiting-human`, `in-review`,
`done`, `failed`) is mirrored onto its issue as exactly one `state/*`
label, swapped by `Forgejo.SetStateLabel` through the shared
`mirrorTaskState` helper (`internal/factoryd/forgejo.go`), the one call
site behind the runner, completion, and the webhook intake; the full
label set (`agent`, `human`, and one `state/*` per status) is created
at boot by `EnsureLabels` if missing. Mirroring failures are logged and
retried on the next transition, never fail the task itself. A
webhook-created task is mirrored immediately after its row exists, not
only once a runner claims it: `state/queued` for the agent lane, and
`state/waiting-human` for the human lane, since no runner ever claims a
human-lane task (architecture 5.6's "waiting on a human" board column
is the human lane's whole lifecycle, not a transient state on the way
to `running`).

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
