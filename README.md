# ai-software-factory-code

The factory's own code. Design lives in the `ai-software-factory` repo,
the cluster in `ai-software-factory-infra`. This is the first repository
the agents work on.

## Binaries

- `cmd/factoryd`: the control plane. Task intake (`POST /tasks`,
  `GET /tasks/{id}`, `GET /tasks/{id}/timeline`, bearer token from secret
  `factoryd-intake`, budget from the policy, see "Policy and costs"), the
  policy loader and `GET /policy`, `GET /costs`, the task store
  in Postgres, and the
  Temporal worker for task queue `factory-tasks`: one `TaskWorkflow` per
  task (workflow id `task-<id>`) runs the attempts, opens the pull
  request, waits for its checks and for the reviewer. Each attempt
  (`RunSession`) gets a fresh namespace, its own pair of Forgejo tokens
  (read on the repository, write on the bot's fork, nothing else) and
  service account (so its own LiteLLM key), one gVisor pod, supervision
  (wall-clock cap, stall, reading without writing, same failure, same
  call, flip-flop, churn and half-way detectors; see `supervise.go`
  below) and
  a session record in S3; `sessions/<task>/timeline.jsonl` records the
  task's whole path. Spend settlement against LiteLLM and the orphan
  sweep run beside it. Runs with `strategy: Recreate`; a restart loses
  nothing: a stopping pod stops its running sessions gracefully (the
  worker pushes its work in progress, within the Temporal worker's 120 s
  stop, inside the pod's 150 s grace), and Temporal retries the
  interrupted attempt on the new pod, which continues the chain from the
  heartbeat and the pushed branch. It also
  runs the CI spawner (see below) in the same process, on the same
  in-cluster Kubernetes client.
- `cmd/factory-gateway`: the enforcement point. Validates the pod's
  projected ServiceAccount JWT, checks the call against the policy
  version in force (read from Postgres, at most 30 s old; 503 while none
  is loaded): the task must be running, its capability classes must allow
  the requested model (403 `refused_capability` otherwise), and both the
  task's headroom (budget minus spent) and the day's headroom (the day cap
  minus today's ledger total, all tasks) must be positive (402
  `refused_budget` or `refused_day_cap`). It mints a per-task LiteLLM
  virtual key capped at the smaller headroom, forwards the call, books its
  cost onto the task row and a `model` ledger row in one statement, and
  writes an audit row per decision naming the policy version.
- `cmd/agent-worker`: pid 1 in the sandbox pod. Clones the base commit
  from the repository with the read token before the agent starts (a
  resumed task branch that only the fork holds is fetched from there),
  runs opencode in its own process group without either token, kills
  and reaps every other process when the agent is done, then fetches
  the agent's commits into a clean repository and pushes `factory/<id>`
  from there to the fork `factory-agent/<repo>` with the write token.
  The sandbox image (`Dockerfile.sandbox`) carries opencode, git, ripgrep
  and the Go 1.25 toolchain (ruling 99, the same pin as the runner
  image); its egress (`workerEgressSpec`) is default-deny plus the
  gateway, the in-cluster Forgejo, the Go module proxy and checksum DB
  by name on 443, and kube-dns for exactly those names; the cloud
  metadata address is denied outright. The module proxy is a covert
  channel: a `go get` of a made-up module path carries that path to
  proxy.golang.org, so an agent can leak a few hundred bytes per request
  through it. Ruling 99 accepts this as the price of a usable Go
  toolchain in the sandbox.

`internal/` holds the packages behind them, `k8s/` the manifests Flux
applies from this repository (namespace `factory`).

## Webhook intake (task 42)

`POST /webhooks/forgejo` (`internal/factoryd/intake.go`) turns Forgejo
issue, comment and review events into task rows and intake events. The
sender is a repository webhook on the code repository (JSON, all
issue and pull-request event groups, target
`http://factoryd.factory.svc.cluster.local:8080/webhooks/forgejo`,
signing secret = key `secret` of secret `factoryd-webhook`). It lives in
Forgejo's database, so a restore carries it; a new secret must be set on
both sides.
`X-Forgejo-Signature` (hex HMAC-SHA256 of the raw body, secret
`factoryd-webhook`) is checked with a constant-time compare before the
body is parsed at all; a bad or missing signature is a 401 and only the
event type is logged, never the body. An issue labelled `agent` or
`human` becomes a task, idempotent per (repository, issue number) —
redeliveries and later relabel events on the same issue find the task
already linked and no-op. `human` takes priority over `agent` when both
are present, and skips the branch/SHA lookup: that lane never runs the
agent. Issues and comments authored by the bot user itself
(`FORGEJO_BOT_USER`, default `factory-agent`) are ignored, and so are
pull requests it closed itself, so the factory's own status updates
never create a second task or a stray signal. `POST /tasks`
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
it by reading the pull request's own head (`factory/<task id>` on the
`factory-agent` fork; the same branch name on any other repository is
nobody's task), and a review reads that same head straight off its own
payload.
A comment or review on anything the factory did not file — no task
behind that issue number or branch — is silently dropped rather than
erroring.

A task's state (`queued`, `running`, `waiting-human`, `in-review`,
`done`, `failed`) is mirrored onto its issue as exactly one `state/*`
label, swapped by `Forgejo.SetStateLabel` through the shared
`mirrorTaskState` helper (`internal/factoryd/forgejo.go`), the one call
site behind the task workflow's activities, completion, and the webhook
intake; the full label set (`agent`, `human`, and one `state/*` per
status) is created at boot by `EnsureLabels` if missing. Mirroring
failures are logged and retried on the next transition, never fail the
task itself. A webhook-created task is mirrored immediately after its
row exists: `state/queued` for the agent lane, and `state/waiting-human`
for the human lane, which starts no workflow (architecture 5.6's
"waiting on a human" board column is the human lane's whole lifecycle,
not a transient state on the way to `running`).

## Task workflow

Both intake paths store the task row, then start its workflow; a heal
pass at boot and every minute starts the workflow of any agent task not
yet done or failed again (and forgets webhook deliveries older than 30
days). Temporal refuses the start while the workflow runs or after it
completed, and allows it after a failure or termination, so no task is
stranded (terminating a workflow is not a cancel; closing the issue is).
A task whose repository is not `owner/name` is failed by the heal pass
instead. Every run first calls `Reconcile`: a task already done or
failed does nothing; a merged PR finishes it done; an interrupted
completion (`closing`), a closed issue or a PR closed without merge
finish it failed; an open PR is adopted, and the run goes straight to
its checks and the review wait, with no new session. Then: up to
`MAX_SESSIONS` (default 3) attempts, each `SetState running` then
`RunSession`, the first on `MODEL` (default `factory/coding-cheap`);
every session starts from the task branch's head on the fork when the
branch exists, else from the base SHA. After a pushed commit
`OpenPullRequest` (head `factory-agent:factory/<id>`, base `main`,
`Closes #<issue>`, reusing an open PR for that fork branch and recording
its number on the task; a commit whose `.forgejo`, `.gitea` or `.github`
tree entry differs from the base's gets no PR, and an already open one is
closed with the reason), `SetState in-review`, `WaitChecks` on the head commit's
combined status, which Forgejo records on the base repository
(`CHECKS_TIMEOUT`, default 45m). A task that fails (its issue closed,
its PR closed unmerged, the ladder exhausted) closes its open PR, found
by number or, when none was recorded, by its branch on the fork, and
then comments the reason on it once (the comment follows the close, so a
retried close never repeats it). Every finished task's branch on the
fork is deleted. Close and delete happen before the task turns final and
retry through a Forgejo outage, so no failed task keeps an open PR; one
Forgejo refuses (a 4xx, which no retry fixes) is logged and the task
completes anyway.
Red checks make the next attempt climb to `STRONGER_MODEL` (default
`factory/coding-frontier`) with the failures as findings; supervision
(below) can redirect, climb or escalate. Signals from the webhook intake
are delivered with signal-with-start, so a signal to a task whose run
died starts a new one (which reconciles first). `pr_merged` (done),
`pr_closed` (closed without merge: failed) and `issue_closed` (done if
the PR merged, else failed) decide the task wherever they land: a
running session or check wait is cancelled at once (the session deletes
its pod and namespace and revokes its credentials first), and they are
drained before every attempt, before a PR is opened and after the
checks; green checks wait for them at no cost. `human_comment` (a new
comment on the task's issue) answers an escalation. Comments and reviews
signal only when their author may write the repository's code: a member
of one of its teams with code write, or a direct collaborator with write
(the org's teams, their repositories and members read with factory-ci's
`read:organization`-only token `forgejo-ci-org-read`, since Forgejo refuses
that scope on a repository-limited token; collaborators and assignees
with the registrar token; a collaborator's own permission is unreadable with a scoped
token, so a collaborator counts by being on the assignee list and in no
team without code write, which could be what lists them there; a grant is
reused for 60 s, a denial checked every time); others are recorded only. The same holds for
closing the issue, which only a writer or factoryd's bot can use to fail
the task. `changes_requested` (a review requesting changes, a review
comment, or a PR comment by a non-bot user) starts one more attempt, all
pending requests coalesced into its findings. Pull request signals count
only for the task's own PR. A webhook delivery is marked processed (by
`X-Forgejo-Delivery`) after it was processed; a redelivery of a
processed one is a no-op, and intake events are keyed by delivery id.
Forgejo never retries a failed delivery, so the heal pass takes in every
open `agent` or `human` issue of the code repository that no task is
linked to, through the webhook's own intake (the same label, bot and
idempotency rules), and for every task in review or waiting on a human
it re-reads its pull request, issue
(closer from the timeline) and comments, and records and signals what
the webhooks missed, each event once; a decision (merge, close) that has
not moved the task out of review or the human wait within 15 minutes is
signalled again every pass, since its claim may have outlived a factoryd
that died before signalling it. A failure comments its reason on the
issue; `Finish` retries for a day, and a workflow that still fails is
restarted by the heal pass. The wall-clock cap (20 min) bounds the agent's working time
summed over the attempts; waiting on checks or reviewers does not count.
The used time and the attempts are recorded on the task row
(`RecordUsage` after every session), so a new workflow run for the same
task (a resubmitted issue, a run restarted by hand) resumes the count
instead of starting a fresh cap. A stopping factoryd stops a running
`RunSession` gracefully: the worker pushes its work in progress, the
session ends `stopped` and Temporal retries the attempt on the new pod,
which continues from the pushed branch with the stop report as findings.
A crashed factoryd loses nothing either: the retry removes the dead
session's namespace, pod and credentials and starts a new session with
the dead one as parent and its findings.

## Supervision

`internal/factoryd/supervise.go` watches every session's event stream.
Detectors, thresholds with their env overrides: stall (no events for
`SUPERVISE_NO_PROGRESS_AFTER`, 5m), reading without writing
(`SUPERVISE_READING_WITHOUT_WRITING` tool calls and commands without a
file change, 24), same failure (the same command failing with the same
output signature `SUPERVISE_SAME_FAILURE` times, 3, each repeat after a
file change: an edit, or a changed diff checkpoint for edits made in the
shell; reruns with nothing changed and silent commands do not count; the
signature is the command, its exit code and its output tail with
timestamps, hex addresses, durations, temp paths, line and column
numbers and pids stripped; a pass ends the run unless the command masks
its exit status with `;`, `|`, `||` or `$?`, so `cmd; echo $?` does not
hide a failure), same call (the same tool with the same arguments
`SUPERVISE_SAME_CALL` times in a session, 4), flip-flop (a file returns
to an earlier content hash), churn (`SUPERVISE_CHURN` file changes since
the diff against the base last grew, with no command newly passing, 15),
half way (half the wall-clock cap `WALL_CLOCK_CAP` or half the budget;
once per task, recorded on the task row). The wall-clock cap ends the
session. Every other trip asks the supervisor, a model under the LiteLLM
alias `supervisor`, called directly with a virtual key minted per call
(metadata `task_id`, `max_budget` the budget minus the larger of
spent_eur and the spend on the task's LiteLLM keys, never more than the
day cap's headroom, model `supervisor` only, so the settlement books its
spend and the session end blocks it).
It reads the brief, the trip, a progress digest and the session record's
tail, and answers one of `continue` (the detector resets, the session
runs on), `redirect` (new session, same rung, its guidance as findings),
`retry_stronger` (the next rung; at the top rung a redirect) or
`escalate`; an invalid answer or a failed call escalates. At most
`SUPERVISOR_MAX_CALLS` (3) calls per session; the next trip escalates
without one. Nothing it answers changes a cap, a budget or the ladder. A
stopping session's pod is deleted with a 60 s grace period (a log stream
that breaks meanwhile, or at any time while the pod runs, is reopened
and the lines already read are skipped): the worker ends the agent,
commits and pushes its work in progress through the same clean push
path, and the next session starts from it. Escalation moves the task to
`waiting-human`, comments the reason and the digest summary on the
issue, and waits: a newer non-bot issue comment starts one more attempt
with the comment as findings, closing the issue fails the task. The
human is the one party allowed to raise a limit: the answer buys the
extra attempt, and if less than `ESCALATION_ANSWER_SLICE` (10 min, never
more than `WALL_CLOCK_CAP`) of agent time is left under the cap, the
answer also grants the difference, so every answer gets at least that
slice of working time. Both grants are recorded on the task row. Every
decision is a `supervisor` event in the session record, a timeline step
and an issue comment.

The worker reports what the detectors read: commands with exit code and
output tail, tool calls with arguments and their canonical hash, file
changes with the content hash, and a checkpoint with the diff against
the base (`--shortstat` numbers and the capped patch) every 15 s when it
changed. It measures the diff in fresh repos of its own that borrow the
base objects, never in the agent's repo: hooks and fsmonitor off, no
external diff or textconv, no system or global config, no token in the
environment.

Changing `TaskWorkflow`: a running workflow can wait days for its
reviewer and replays on the new code, so every change to the commands it
issues is gated with `workflow.GetVersion` (see the version constants in
`internal/factoryd/workflow.go`), and `TestWorkflowReplaysLiveHistories`
replays histories recorded on the cluster (`internal/factoryd/testdata`).
`GET /tasks/{id}/timeline` serves `sessions/<id>/timeline.jsonl`.

## Policy and costs

The capability policy is a YAML document in the infra repository
(`k8s/platform/policy/policy.yaml`, ConfigMap `factory-policy`, applied
by Flux; its README says how to change it), schema v1 in
`internal/policy`: `budget` (`task_default_eur`, `task_max_eur`,
`day_eur` for the UTC day over all tasks) and `classes` (per capability
class the model aliases it may call and its tools with per-task caps).
Unknown keys are rejected. factoryd mounts it read-only at
`/etc/factory-policy/policy.yaml` (`POLICY_FILE`), reads it on boot and
every 30 s, and records each new valid content as an immutable
`policy_versions` row (sha256 of the file, the document as JSON,
`loaded_at`) made current in `policy_current`; known content (a revert)
makes its earlier row current again. An invalid file logs
`POLICY REJECTED` and the previous version stays current; factoryd
refuses to boot only when no valid version exists at all. The gateway
never reads the file, only the current version in Postgres.

Intake gives every agent task the capability class `coder`
(`tasks.required_capabilities`; a one-shot backfill gave it to older
agent tasks) and the budget `task_default_eur`, or the requested budget
capped at `task_max_eur`.

The `ledger` table holds every booked cost: task, UTC day, kind (`model`
per gateway call with the alias as ref, `settlement` for the raise the
settlement pass makes when LiteLLM shows more spend than was booked,
`tool` and `resource` for later connectors), ref, EUR. Its day total is
what the day cap is measured against. The schema of `policy_versions`,
`policy_current` and `ledger` lives in `internal/policy/schema.sql` and is
applied by both binaries, which share the `gateway` database.

Both endpoints take the intake bearer token:

- `GET /policy`: the version in force, `{id, sha256, loaded_at, document}`.
- `GET /costs?from=YYYY-MM-DD&to=YYYY-MM-DD` (UTC days, both included,
  each defaulting to today, at most 366 days apart): `days` (per day
  `by_kind` and `total`) and `tasks` (per task total, largest first).

## Console (task 54)

`internal/console`, served by factoryd on `:8090` (`-console-addr`) behind
its own Service `factoryd-console` and the HTTPRoute `console` on the edge
gateway's `https-console` listener (https://console.aisoftwarefactory.world);
the webhook and intake API on `:8080` stay in-cluster. Server-rendered
`html/template` with htmx 2.0.11 vendored in `internal/console/static`
(sha256 pinned by a test), no build step.

- Login: Forgejo OAuth2 authorization code with PKCE (S256), confidential
  client `factory-console` (secret `console-oauth`: `client_id`,
  `client_secret`; redirect `/auth/callback`), always the explicit scopes
  `read:user write:repository write:issue` (an unscoped grant would carry
  the user's full power). Browsers go to `FORGEJO_PUBLIC_URL`, the code
  exchange and API calls to `FORGEJO_URL`. Only the trust gate's writers
  of the code repository get in, checked at login and on every request
  (list cached a minute); others get 403. Sessions are in memory (a
  restart logs everyone out), the tokens never leave the server and are
  refreshed before their hour runs out; the cookie is `__Host-`,
  HttpOnly, Secure, SameSite=Lax. Every POST needs the session's CSRF
  token (form field `csrf` or header `X-CSRF-Token`).
- `/`: the agent lane by task status (done and failed for 7 days), the
  human lane (approval requests, escalations with their reason and an
  answer box, human tasks); htmx polls `/board` every 10 s.
- `/tasks/new`: files the issue as the human, labelled `agent` or `human`
  and optionally `priority/pN` (offered when the label exists); intake
  makes it a task. An escalation's answer is an issue comment as the
  human, which resumes the task through the human-comment path.
- `/costs`: the ledger per day and kind and per task for 14 days, today
  against the day cap. `/policy`: the version in force, linked to its file
  in the infra repository for edits.

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

Agent pull requests come from the bot's fork, never from a branch of
the repository itself (ruling 97): Forgejo hands a same-repository PR
job a per-run token that can push to the repository, and hands a fork
PR job a read-only one and no secrets. factoryd creates the private
fork `factory-agent/<repo>` at boot and before every session if it is
missing, as the bot, and switches the fork's Actions unit off, so a
session token cannot run a workflow there by pushing the fork's `main`;
the per-task write token is scoped to the fork alone, and the base
clone uses a second token scoped read-only to the repository. `pr.yml`'s `job-env-audit` job proves both on every PR: no
credential-shaped variable beyond the job-token names, no credential
file, and a push with the job token refused.

`.forgejo/workflows/pr.yml` runs on those pull requests,
`runs-on: pr-untrusted`. factoryd's CI spawner (`internal/factoryd/ci.go`)
polls Forgejo for Actions jobs waiting on that label, registers a
repo-scoped ephemeral runner (`forgejo-ci-registrar` token, never the
repository or registry credential, never mounted into the runner pod),
and creates a namespace `factory-agents-ci-<job id>` (at most
`CI_MAX_CONCURRENT`, 8, live at a time; further jobs wait in Forgejo's
queue for a later tick) holding one
`runtimeClassName: gvisor` pod running `forgejo-runner one-job --wait` —
non-root, no automounted ServiceAccount token, dropped capabilities,
default-deny egress (`ciEgressSpec`: the in-cluster Forgejo, the Go
module proxy and checksum DB, `data.forgejo.org`, Docker Hub; nothing
else, `169.254.169.254/32` explicitly denied; the module proxy remains
the covert channel named above, accepted by ruling 99). The namespace and the
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

## factoryd's own network

factoryd holds the LiteLLM master key, the bot password and the S3 keys,
so its egress is default-deny (`factoryd-egress` in
`k8s/factoryd.yaml`): kube-dns for exactly the names below, the API
server (by entity; factoryd dials the injected
`KUBERNETES_SERVICE_HOST`, no DNS), the `sf-cluster` Postgres instances
on 5432, the in-cluster Forgejo on 3000 (`FORGEJO_URL`; the public host
resolves to the edge gateway; links for people use
`FORGEJO_PUBLIC_URL`), LiteLLM on 4000, the Temporal frontend on 7233,
and the S3 endpoint on 443 (its FQDN must match `S3_ENDPOINT`). Every
callee is named in full and the pod runs with `ndots: 1`, so the first
DNS query is the admitted one. A new callee needs a rule here first.
Ingress (`factoryd-ingress`): the node on 8080 (probes), Forgejo's
webhooks on 8080, and the edge gateway's Envoy (entity `ingress`) on the
console port 8090 only.

## Operating it

- Submit a task through the authenticated intake: port-forward to
  factoryd with an explicit PID
  (`kubectl -n factory port-forward deploy/factoryd <port>:8080 &`, then
  `kill <pid>`), read the bearer token from secret `factoryd-intake`
  (key `token`) into a shell variable, never print it, and
  `POST /tasks` with `objective`, `repository`, optional `sha` and
  `budget_eur`. Filing an issue labelled `agent` does the same through
  the webhook, with the policy's default budget.
- Follow a task: its issue's `state/*` label, factoryd's log
  (`webhook intake: issue #N …: agent task <id>`,
  `task <id>: workflow task-<id> started`,
  `task <id>: signal <name> from <user> delivered`),
  `GET /tasks/{id}/timeline`, and the session records
  `sessions/<task>/<session>.jsonl` in the sessions bucket.
- Webhook deliveries: this Forgejo has no delivery-list API; the web UI
  (repository settings, webhooks) or Forgejo's `hook_task` table shows
  them (select id, event_type, is_succeed, delivered and the response
  status only: the request column carries the signature headers).
- PR job logs: this Forgejo has no job-log API; they are in the Forgejo
  storage bucket at
  `actions_log/<owner>/<repo>/<task id % 256, two hex digits>/<task id>.log.zst`.
- Agents cannot change workflows (rulings 90, 100): the worker compares
  the tree entries of `.forgejo`, `.gitea` and `.github` between the base
  and the commit it is about to push and refuses when any differs (a
  `push_failed` event in the session record; the commit never reaches
  the fork, so a follow-up push on an open PR cannot trigger the head's
  workflows either), and factoryd refuses to open a PR for a commit whose
  entries differ from the base's, closing an already open one. Object ids
  are compared, not diff path listings, so renames out of the directory
  and quoted non-ASCII names cannot slip past.

## Ship

CI pushes images by digest. A deploy is a commit that rolls the digests
in `k8s/factoryd.yaml` and `k8s/deployment.yaml` (factoryd, the gateway,
`WORKER_IMAGE` and `CI_RUNNER_IMAGE`), pushed to both remotes:

    git push origin main && git push github main

`kubectl rollout restart deployment/factoryd` restarts it twice: Flux
reverts the `restartedAt` annotation about five minutes later and rolls
it again. Both restarts are safe (Temporal retries the interrupted
activity), but plan for the second one, or suspend the `factory`
Kustomization first.
