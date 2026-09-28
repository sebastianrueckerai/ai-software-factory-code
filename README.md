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
  projected ServiceAccount JWT and checks the call against the policy
  version in force (read from Postgres, at most 30 s old; 503 while none
  is loaded). It forwards only the OpenAI chat fields (400 `refused_body`
  for anything else, above all LiteLLM's `fallbacks`,
  `router_settings_override` and `mock_testing_*`, and for `n` above 1),
  and of their values only function tools and text content: a provider's
  built-in tools (web search, retrieval) bill per use and reach the
  internet past the sandbox, and image, audio or file parts cost far more
  than their bytes.
  The task must be running and its capability classes must allow the
  model (403 `refused_capability`). The call is then metered, see "Policy
  and costs": its output limit is clamped so its worst case fits the free
  headroom, the worst case is held, and the real cost is booked after the
  call (402 `refused_budget` or `refused_day_cap` when it cannot fit). The
  per-task LiteLLM virtual key is limited to the classes' aliases and
  capped at the smaller headroom; a new run, policy version or UTC day
  mints a new key and blocks the old one once the calls still using it
  are done. A LiteLLM budget refusal is labelled by the headroom the key
  was capped at (`refused_budget` or `refused_day_cap`). Every decision has an audit row
  naming the policy version; an admitted call's row also carries its
  output limit and hold. It also serves the agent's tools over MCP at
  `/mcp`, see "Tools".
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
  gateway, the in-cluster Forgejo, the Go module proxy and checksum DB,
  and `storage.googleapis.com` (proxy.golang.org redirects zip downloads
  there) by name on 443, and kube-dns for exactly those names; the cloud
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

## Gates, review, approval and merge (task 55, ruling 109)

Green checks are not a merge. A workflow started after this change
(change id `gates`) takes a head with green checks through these
activities, each with a timeline step and a comment on the pull request:

1. `GatePolicy` lists the pull request's changed paths (`pulls/{n}/files`,
   every page; a rename counts its old and new name, which the compare
   API's file list cannot tell) and classifies them under the policy
   version in force (`risk` in the policy file: ordered path globs to a
   class, a default, the classes most severe first, each `forbidden` or
   `requires_approval`, `migration: true` for schema migrations). The
   class goes on the task row (`risk_class`). A forbidden class fails the
   task with the paths, which closes the pull request.
2. `GateMigration`, when a migration-class path changed: the comment and
   the approval card list them under "schema migration: human review
   required".
3. `GateReproduction`, when the task carries a reproduction
   (`repro_command`: the issue body's first fenced block labelled `repro`,
   or `repro_command` on `POST /tasks`): a fresh sandbox namespace, a read
   token on the task's repository only, and the worker image in its repro
   mode (`MODE=repro`, no agent, no gateway token, no push token, the
   agents' egress policy) fetches `refs/pull/<n>/head`, checks it is the
   gated head and runs the command with `sh -c` for at most
   `REPRO_TIMEOUT` (default 10m, at most 15m). Exit 0 passes. Anything
   else sends the next attempt back to the agent on the stronger rung with
   the exit code and the output tail as findings, like red checks. The
   namespace and the token go when it ends.
4. `ModelReview` asks `REVIEW_MODEL` (default `review-frontier`) for
   `{"verdict": "approve|concerns", "summary", "findings"}` over the brief,
   the diff (cut at 48 KiB) and the gate results, through the supervisor's
   metered path: a per-call LiteLLM key of the task, the output clamped to
   the task's and the day's headroom, the cost booked as a `model` ledger
   row. It is evidence only: no headroom, a failed call or an invalid answer
   is a note, and nothing it says skips a gate or merges.
5. `RequestApproval` opens an `approval_requests` row (kind `merge`,
   status `pending`, the task, project, pull request, and in `spec` the
   risk class, the migration flag and paths, the review's verdict and
   summary, the reproduction, the head). A task has one pending merge
   request; a new attempt denies it as `factoryd`, and the task's end
   denies its pending requests of both kinds (a resource request survives
   a new attempt).

The workflow then waits, without a timer, for `approved`, a change
request, `pr_merged`, `pr_closed` or `issue_closed`. The console approves
by submitting the human's APPROVED review through the human's own OAuth
token, then calling `factoryd.ApproveMerge` (a pending merge request of
that task and pull request is required), which signals `approved` with
signal-with-start like the webhook path. `Merge` then merges as the
dedicated user `factory-merge` (`FORGEJO_MERGE_TOKEN`, secret
`factory/forgejo-merge`) with `head_commit_id` the gated head, and the
task finishes done. main's protection lets factory-merge merge but not
push, and only after an approval by a user on the approvals whitelist, so
Forgejo itself refuses the merge without the human (405; a moved head or
a conflict is 409). A refusal is commented and the wait goes on. Merge
refuses by itself a pull request whose base is not `main`. A merge that
fails otherwise (Forgejo unreachable) is retried five times, then
commented and recorded (`merge_failed`); the approval stands and the
merge is tried again after 10 minutes. A human merging in Forgejo stays
valid (`pr_merged`, or found by Merge and credited to that human, with
the head they merged). An approval signalled before the request opened,
or for another pull request, does nothing. From change id
`gates-pinned` on, `GatePolicy` and `ModelReview` check that the pull
request's head is still the gated commit (a moved head fails the policy
gate and skips the review with a note), and the model review races the
terminal signals like the checks and the reproduction; a task without a
reproduction command gets a `gate_reproduction` step saying so.

The first pull request merged this way added this sentence.

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
alias `supervisor`, called directly and metered like a gateway call: its
answer limit is clamped to the free headroom, the worst case is held, and
its cost is booked as a `model` ledger row with ref `supervisor` right
after the call; a call that cannot fit escalates. Its virtual key is
minted per call (metadata `task_id`, `max_budget` the hold, model
`supervisor` only, so the session end blocks it).
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
issue, and waits: a newer non-bot issue comment starts one more attempt,
closing the issue fails the task. The workflow keeps every answer of the
task (change id `answers`, ruling 118), and every later session gets all
of them, newest first, as `SessionInput.Answers` (worker env `ANSWERS`),
each as `<author>, to "<the escalation's first line>": <answer>` (the
answer cut to 4000 bytes; the issue keeps it whole). Escalate records its
comment as an `escalation` intake event, and `Reconcile` rebuilds the
answers from the intake events (every trusted issue comment newer than an
escalation's comment, tagged with its question; ruling 120), so a run the
heal pass starts keeps them too. The opencode prompt puts the answers
ahead of the objective as settled
answers the agent must not escalate again; the `session_start` timeline
step records them. The
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
`loaded_at`). The version in force is the latest row of the append-only
`policy_activations`; known content (a revert) activates its earlier row
again. An invalid file, or one whose `coder` class leaves out a model of
the ladder (`MODEL`, `STRONGER_MODEL`), logs `POLICY REJECTED` and the
previous version stays in force; factoryd refuses to boot only when no
valid version exists at all. The gateway never reads the file, only the
version in force in Postgres. `day_eur: 0` stops all spend.

Intake gives every agent task the capability class `coder`
(`tasks.required_capabilities`; a one-shot backfill gave it to older
agent tasks) and the budget `task_default_eur`, or the requested budget
capped at `task_max_eur`.

The caps hold on the call itself. Prices per alias come from LiteLLM's
`GET /model/info` (the figures it bills with), read every 5 minutes: per
alias the maximum over its deployments of every per-token input rate
(base, long-context `*_above_*_tokens`, service tiers, cache writes,
`tiered_pricing`), every output and reasoning rate, and the output limit.
A deployment with any other cost field set (per query, second, image,
character) or without an input and output rate makes its alias unpriced,
and an alias the policy allows but LiteLLM cannot price is refused (503
`error_price`, logged as `PRICE MISSING`). While refreshes fail the last
table is used for up to three TTLs, then every call is refused. A call's input is bounded by its body's byte length (a
token covers at least one byte). Under one Postgres advisory lock, in one
transaction, the headroom left after spend and every outstanding hold is
computed (task: budget minus spent; day: `day_eur` minus today's ledger
total), the output limit (`max_tokens` or `max_completion_tokens`) is
clamped to what that pays for, and a `holds` row takes the worst case.
Fewer than 1024 output tokens (GLM reasons before it answers) is refused
unless the caller asked for fewer. After the call one transaction
deletes the hold and books the real cost onto `tasks.spent_eur` and the
ledger; an unknown cost (no cost header, a broken stream, a value that
is not a finite non-negative number) books the hold, and a failed call
books the cost LiteLLM reports for it, else nothing. A hold whose caller
died expires after an hour and factoryd's sweep books it in full; a late
release books only what the real cost exceeds it by. Settlement leaves a
task alone while it has an open hold. Output limits bound reasoning:
verified on GLM (reasoning counts inside `max_tokens`); Qwen's thinking
does not, so the dashscope deployment runs with thinking off, and the
gateway refuses a client's `thinking` field. `holds` has kinds `model`,
`tool` (see "Tools") and `resource`. During a superseded key's grace (the
calls still in flight on it when a new run, policy version or day mints
its replacement), a task briefly has two live keys, each capped at the
headroom of its own mint, so LiteLLM does not count one key's spend
against the other; the holds remain the primary cap. LiteLLM's
`fail_closed_budget_enforcement` is the backstop.

The `ledger` table holds every booked cost, append-only: task, UTC day,
kind (`model` per call with the alias as ref, `settlement` for the raise
the settlement pass makes when LiteLLM shows more model spend than the
task's `model` and `settlement` rows hold (tool rows never offset it; a
difference below 1e-9 EUR is float noise and books nothing),
dated the day the pass runs, `tool` per tool call with the tool as ref,
`resource` for the later leases), ref, EUR (never negative). Costs are LiteLLM's USD prices
booked 1:1 as EUR, which errs high. The schema of `policy_versions`,
`policy_activations`, `ledger` and `holds` lives in
`internal/policy/schema.sql` and is applied by both binaries, which share
the `gateway` database.

Both endpoints take the intake bearer token:

- `GET /policy`: the version in force, `{id, sha256, document, loaded_at, activated_at}`.
- `GET /costs?from=YYYY-MM-DD&to=YYYY-MM-DD` (UTC days, both included,
  each defaulting to today, at most 366 days apart): `days` (per day
  `by_kind` and `total`) and `tasks` (per task total, largest first).

## Tools

The gateway serves the agent's tools over MCP at `/mcp` (streamable
HTTP, stateless, `github.com/modelcontextprotocol/go-sdk`), behind the
same task-JWT check and task lookup as the model path. The sandbox's
identity proxy forwards `/mcp` with the pod JWT like the model calls;
opencode's config names it the remote MCP server `factory-tools`
(timeout 150 s, above the slowest connector; the pinned opencode cuts a
tool call at this timeout), and the model sees the tools as
`factory-tools_<tool>`. The provider keys live only in the gateway
(secret `gateway-tool-keys`).

- `web_scrape` (`url`, at most 2048 bytes): Firecrawl `/v2/scrape`,
  markdown only, basic proxy, no PDF parser, the text capped at 64 KiB
  (cut on a rune boundary). Hosts that are not public DNS names (IP
  literals in any spelling, single labels, `localhost`, `.local`,
  `.internal` and the like, so every cluster name) are refused before
  any hold.
- `web_search` (`query`, 1 to 1000 bytes, `limit` 1 to 10): Firecrawl
  `/v2/search`.
- `replicate_predict` (`model` owner/name, optional `version`, `input`):
  a Replicate prediction. The version lookup runs under the hold, so it
  counts as a request. The create call sends `Cancel-After:
  <max_seconds - 5>s`, so Replicate itself stops the prediction inside
  the hold, and `Prefer: wait` for that long (at most 60 s); one still
  running then is cancelled (4 attempts, backoff from 1 s). A cancel that
  never lands books the hold and is logged and audited as `REPLICATE
  CANCEL FAILED` with the prediction id. `max_seconds` must be whole
  seconds, at least 10. Only models the policy maps to a hardware SKU
  run.
- `fal_run` (`endpoint`, `input`): `https://fal.run/<endpoint>`, priced
  from fal's pricing API (cached an hour per endpoint); only endpoints
  priced per request or per megapixel run. A megapixel endpoint takes
  only the input keys `prompt`, `negative_prompt`, `seed`, `image_size`,
  `num_images`, `num_inference_steps`, `guidance_scale`,
  `enable_safety_checker` and `output_format` (ruling 117); any other key
  (a source image, a count alias, another size name) is refused before
  the hold. `num_images` (default 1) times the megapixels of `image_size`
  (a fal preset or `{width, height}`, default `landscape_4_3`, each side
  rounded up to a multiple of 64), rounded up per image, must fit
  `max_megapixels`, and both are sent explicitly.
- `escalate_to_human` (`question`, `reason`): free, for the `coder`
  class. The gateway posts it to factoryd's `POST /internal/escalations`
  (bearer from secret `gateway-factoryd`, key `token`); factoryd hands
  it to the running session, answers 202 only once the session took it
  (a session that stops or ends first answers 409, and the agent is told
  it did not arrive), then records an `agent_escalation` intake event.
  The session trips its supervision with an escalation, no supervisor
  call (one arriving during a supervisor call cuts the call and wins
  over its answer): the worker pushes its work in progress, the task
  goes `waiting-human`, the question is commented on the issue, and a
  trusted answer resumes the task. The agent reads "escalated; this
  session will end".

The fal and Replicate outputs are usually file URLs (`v3.fal.media`,
`replicate.delivery`), which the sandbox's egress cannot fetch: the
agent gets the URLs and metadata, not the files.

Every paid call, in order: the task is running; one of its classes lists
the tool (`classes.<c>.tools`, the most generous cap when several do);
the arguments are valid and the call's worst case is priced (Firecrawl:
`scrape_credits` or `search_credits` at `eur_per_credit`; Replicate:
`max_seconds` at the model's hardware rate; fal: the endpoint's unit
price, times `max_megapixels` for a megapixel endpoint); under the
headroom lock, the task's requests and euros on the tool (booked rows
and open holds) and the free task and day headroom must fit it, and a
`tool` hold takes it; then the provider is called and one transaction
books the real cost onto the task and a `tool` ledger row (every
admitted call writes one, at 0 EUR too: the rows count the requests).
Real costs: Firecrawl's `creditsUsed` (a call that returns no result is
not charged), Replicate's `metrics.predict_time` at the hardware rate,
fal's unit price times the units (1 per request, megapixels rounded up
per returned image). An unknown cost (a 5xx, a transport error, a
missing field) books the hold; a 4xx books 0. A reported cost above the
hold (a provider billing past the bounded arguments) is booked in full,
as decision `allowed_over_hold` with `COST OVER HOLD` and both figures
in the audit reason and the log; the caps then refuse the next call from
the true headroom (ruling 117). A hold is only taken for a task that is
`running` or `in-review` (the gates' model review): `TakeHold` refuses
any other status with `refused_task_status` in its own transaction. Prices not in any API
response live in the policy's `prices`. A refusal (`refused_capability`,
`refused_task_status`, `refused_arguments`, `refused_price`,
`refused_tool_requests`, `refused_tool_eur`, `refused_budget`,
`refused_day_cap`) comes back as an MCP tool error that says the policy
refused and the tool did not run; a provider failure (`error_tool`) says
the tool failed. Every call has an audit row with the decision, the tool
(`audit.tool`), the policy version and, when admitted, the hold.

The gateway's egress (`factory-gateway-egress` in `k8s/deployment.yaml`)
is default-deny plus LiteLLM, the `sf-cluster` Postgres instances,
factoryd on 8080, OpenBao on 8200 (the SSH CA), the API server (by
entity on 6443, the backend port behind the Service's 443, dialled at the
injected `KUBERNETES_SERVICE_HOST` for the JWKS), and `api.firecrawl.dev`,
`api.replicate.com`, `fal.run`, `api.fal.ai` and `api.hetzner.cloud` by
name on 443, with kube-dns for exactly those names (`ndots: 1`, every
callee named in full). factoryd's ingress admits the gateway on 8080.

## Commissioned resources

Agents get servers for a task through gateway tools (architecture 5.5),
never through a provider key. They live in their own Hetzner project,
`agent`, whose token only the gateway holds (`HETZNER_AGENT_KEY`, secret
`hetzner-agent`); factoryd never sees it. Every tool is for running
`coder` tasks; the project is the task's repository (`owner/name`).

- `cloud_prices`: the project's server types with EUR/h and EUR/month
  (net, `fsn1`, the server's primary IPv4 included), the volume price,
  and the envelope with what the month has used. Prices come from
  `GET /v1/pricing`, cached an hour (a failed refresh keeps the last
  catalogue).
- `commission_server` (`server_type`, `purpose`, `lease_minutes`,
  optional `volume_gb` 10 to 1024 and `ssh_public_key`): a Debian 12
  server with a public IPv4 only, a firewall admitting SSH only from the
  cluster nodes' IPv4s (`AGENT_SSH_SOURCES`, ConfigMap `cluster-facts`)
  and outbound only DNS, HTTP and HTTPS, and an optional ext4 volume
  mounted at `/mnt/HC_Volume_<id>`. cloud-init installs the OpenBao SSH
  CA's public key as `TrustedUserCAKeys` with `AuthorizedKeysFile none`
  and no passwords: only certificates log in, as `root`. The answer
  carries the resource id, the IPv4 and the lease end, and a certificate
  when a key was given.
- `ssh_certificate` (`resource_id`, `public_key`): OpenBao signs the key
  for principal `root`, for 30 minutes or until the lease ends, whichever
  is sooner, with key id `task=<id> resource=<id>`.
- `list_resources`, `release_resource` (`resource_id`: the server, its
  firewall and volume are deleted now), `resource_request_status`
  (`request_id`).

**Pricing.** A lease is billed per started hour: hours = ceil(minutes /
60). The reservation is hours x (server hourly + IPv4 hourly) + hours x
volume GB x GB-month price / 730. For a cx23 in fsn1 for 90 minutes with
20 GB: 2 x (0.0088 + 0.0008) + 2 x 20 x 0.0572 / 730 = 0.0192 +
0.0031342 = 0.0223342 EUR. The monthly figure on approval cards is the
catalogue's monthly prices (5.49 + 0.50 + 20 x 0.0572 = 7.134 EUR).

**Envelope.** Table `envelopes` (one row per project: `monthly_eur`,
`server_types`, `max_count`, `max_lease_hours`, `volume_max_gb`,
`set_by`, `set_at`). A project's first request copies the policy's
`envelope_default` into its row (`set_by` `default`); the console edits
it. Under the headroom lock, one transaction checks the envelope: the
type, the lease, the volume, the count of open servers, and this UTC
month's resource ledger rows plus open reservations plus this
reservation against `monthly_eur`. Within it, the reservation is a
`resource` hold against the task's and the day's headroom (a refusal
otherwise, as for paid tools) and the registry rows are written in the
same transaction. Beyond it, the tool files an `approval_requests` row
(kind `resource`, the spec, the reason, `monthly_eur`) and answers
`pending human approval <id>`; asking again finds the same request. An
approval covers one commission of that spec or a smaller one for the
task (the server row records `approval_id`); a denial is answered with
the human's note. The console approves a request for the task (that one
commission) or for the project: the envelope widens just enough for it
(its type, lease, volume, one more server, and the month to the cent
above use plus its reservation) and the request is approved, both as the
human, in one transaction under the headroom lock.

**Registry first.** The rows (table `cloud_resources`, project `agent`,
owner the task, the session, the repository, the type or size, the
purpose, the reservation `hold_id` that groups the commission) exist
before the firewall, the server and the volume are created, in that
order. Every resource carries the labels `factory=agent`, `task=<id>`,
`project=<owner--name>` and `resource=<row id>`. A failed create
releases what was made; a create whose answer was lost is found by its
label or deleted as unknown.

**Leases and the collector.** The gateway runs the collector on the
agent project every 2 minutes: it deletes resources whose lease ended
(unless promoted) or whose task is `done` or `failed`, deletes every
server, firewall and volume the registry does not know as open and every
primary IP no server holds, and closes rows whose resource is gone; the
interlock of ruling 123 (see the untrusted runner below) guards every
pass, with the cluster nodes from `AGENT_SSH_SOURCES`. Then it accrues: each resource's started hours not yet booked go to a
`resource` ledger row (`ref` `resource/<id>`) and the task's spend, and
come off the reservation; once a commission's rows are closed and fully
booked, the rest of the reservation returns. A reservation expires 24 h
after its lease, so factoryd's hold sweep books it whole only if the
collector stopped for a day. Promotion (`promoted`, set in the console,
`promoted_by`) drops the reservation and the lease; a console release
(`released_by`) ends the lease now and the collector deletes it; the resource then counts against
the project's month as it accrues.

**Reachability.** factoryd reads the live leases from the registry
(same database) every 10 s and server-side applies the task namespace's
`worker-egress` with one rule item per leased server: `toCIDR <ip>/32`
and `toPorts 22/TCP` together. When the lease ends or the server is
released, the address leaves the policy. The sandbox image carries
`openssh-client`.

**The SSH CA.** OpenBao's `ssh-client-signer` engine with role
`factory-agent` (user certificates, principal `root` only, TTL at most
30 minutes); the gateway logs in through Kubernetes auth role
`factory-gateway` (its ServiceAccount only) with a policy that may only
sign with that role. The infra README has the setup commands.

The envelope covers servers with their primary IPv4 and volumes. Buckets
are not offered (ruling 121): Hetzner's Cloud API has no Object Storage
endpoint (the agent token gets 404 on every candidate path), and its S3
keys are made in the console and reach every bucket of a project.

A server's firewall admits SSH only from the cluster nodes and lets it
out only on 53, 80 and 443, as build VMs (ruling 122). Its egress is not
name-restricted: an agent can reach the internet on those ports through
its server, beyond the sandbox's allowlist, for at most the lease.

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
- Approval cards in the human lane, from `approval_requests`. A resource
  request shows its type, purpose, lease, monthly cost, project and task;
  approve for the task or the project, or deny with a reason (`decided_by`,
  `decided_at`, `decision_note`). A merge request shows the pull request,
  the risk class, the migration flag and the model review. Approve submits
  an APPROVED review at the gated head as the human, and only once Forgejo
  took it calls `factoryd.ApproveMerge`; deny with a reason submits a
  REQUEST_CHANGES review (the changes-requested path sends the task back)
  or rejects: the reason is commented and the pull request closed as the
  human, which fails the task.
- `/resources`: the agent project's commissions by project and task (open,
  or closed in the last 7 days) with type, IPv4, lease end, EUR/h, cost
  booked so far and state; promote and release per commission; each
  project's envelope (the code repository's shows the policy default until
  saved) with an edit form, saved with `set_by`.
- Boot creates the labels intake reads on the code repository
  (`factoryd.BootLabels`): `agent`, `human`, `state/*`, `priority/p0..p3`
  and `severity/critical|high|medium|low`; the new-task form offers the
  priorities.
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
into the Forgejo registry. Each build imports and exports (`mode=max`)
its image's layer cache in the in-cluster build cache,
`build-cache.forgejo.svc.cluster.local:5000/cache/<image>:main` (infra
`k8s/platform/build-cache`, ruling 111), which the pull-request builds
read; a cache outage only warns.

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
module proxy and checksum DB, `storage.googleapis.com` (proxy.golang.org
redirects zip downloads there), `data.forgejo.org`, and for a build job
`auth.docker.io` (BuildKit fetches the build VM's registry tokens through
the `buildctl` session) and the build cache on tcp 5000; nothing else, `169.254.169.254/32` explicitly denied; the module proxy remains
the covert channel named above, accepted by ruling 99). The namespace and the
ephemeral runner registration are torn down when the pod finishes or its
45-minute deadline passes. Idempotency is the Forgejo job id on the
namespace label, so a restart or a second poll tick never spawns twice.

The runner image (`Dockerfile.runner`) ships forgejo-runner, git, bash,
node, the Go 1.25 toolchain, gosec, `buildctl` and `crane`, and nothing that builds
images: no rootless BuildKit comes up under gVisor, and a root build pod
in the cluster was the one exception the admission policy had to make.
Image builds run outside the cluster instead (ruling 110,
`internal/factoryd/buildvm.go`):

- **The VM.** A job whose `runs_on` includes `pr-untrusted-build`
  (`pr.yml`'s `image-build`, labelled so only when the `changes` job
  selected an image: Forgejo hands a job to a runner before evaluating
  its `if`) gets a throwaway `cx23` server (debian-12,
  fsn1) in the Hetzner CI project, which holds nothing else. Its
  cloud-init installs the checksum-pinned buildkitd v0.33.0 release (with
  the release's own runc) and serves it on tcp 1234 behind mTLS. No SSH
  key, no credential: the VM holds only its own server key. The user_data
  (the server key and the CA certificate) stays readable from inside the
  VM through Hetzner's metadata service, so a Dockerfile's `RUN` step can
  read it; that gives nothing: the key authenticates only this VM, and the
  CA key that could sign another client is gone.
- **The certificates.** factoryd mints a CA per VM, signs a server
  certificate (fixed DNS name `buildkitd`, since the IP is unknown until
  the server exists) and a client certificate, and drops the CA key. The
  CA and the client pair go into secret `buildkit-client` in the job's
  namespace, mounted into the runner pod; the pod gets `BUILDKIT_HOST`,
  `BUILDKIT_TLS_DIR` and `BUILDKIT_TLS_SERVERNAME`. `image-build` waits up
  to three minutes for `buildctl debug workers`, then builds the changed
  Dockerfiles in parallel, output to `/dev/null`; nothing is pushed.
- **The cache.** Before each build the runner pod copies the image's
  cache, which main CI wrote, from the in-cluster build cache into a local
  OCI layout (`crane pull --format=oci`, by digest) and passes it with
  `--import-cache type=local,...,digest=<digest>`; buildctl streams what
  the build needs to the VM over the session it already holds. The VM
  never reaches the cache and holds no credential for it; nothing does:
  the cache's network policy is the access control, and a CI namespace
  (label `factory.ai/ci=true`) may only GET and HEAD. A missing cache
  (a new image, the first build after a rebuild) logs `no cache for
  <image>` and builds cold.
- **The network.** A Hetzner firewall per VM admits tcp 1234 only from the
  cluster nodes' public IPv4s (`CI_BUILD_VM_SOURCES`, from ConfigMap
  `cluster-facts`, ruling 112) and lets the VM out on tcp 80/443 and
  tcp/udp 53 only. The runner pod's egress policy gains exactly
  `<vm ip>/32` on tcp 1234. The VM's egress is not name-restricted (Hetzner
  firewalls match IPs): the build context is code the agent already had,
  and the lease bounds the window.
- **The lifecycle.** Every server and firewall is a row in
  `cloud_resources`, written before the create call and labelled
  `factory=ci`, `job=<id>`, `resource=<row>`. When the job's pod finishes
  (success, failure, deadline) the spawner deletes the server, then the
  firewall (once: a firewall its deleting server still holds is left to
  the collector, so teardown never stalls the poll loop), then the
  namespace. For three minutes after a runner finished, its job is not
  claimed again even if Forgejo still lists it waiting. The collector
  (every minute) is the backstop: it deletes resources whose lease (job
  deadline plus 15 minutes) expired or whose job namespace is gone,
  deletes any server or firewall in the project the registry does not
  know and any primary IP no server holds, and marks rows whose resource
  vanished. Deletes are idempotent; primary-IP counts lag a few seconds
  after a delete, so nothing trusts one snapshot.
- **The interlock (ruling 123).** The collector deletes whatever its
  project holds, so a wrong token would wipe another project. A pass
  that lists a server on one of the cluster nodes' IPv4s, or a server
  labelled `factory=` another project, deletes nothing and logs
  `refusing the pass: ...`; so does a pass without node IPs to compare.
  A hand-made unlabelled server off the node addresses is still deleted.
  The gateway's agent-project collector runs the same check.
- **Failure and back-off.** The price catalogue is read once an hour (a
  failed refresh keeps the last one). A failed provision (the API down, a
  bad token, the quota) pauses build claims for a minute, so an outage
  costs no runner registration and namespace per tick. The Hetzner
  client retries a 429 after its `Retry-After` (at most three tries, only
  short waits). A missing or malformed `cluster-facts` stops factoryd at
  boot, not just the builds: intake, heal and settle wait for it too, as
  with an invalid policy.
- **The cap and the cost.** At most `CI_BUILD_VMS_MAX` (2, the project's
  primary-IP quota) VMs live at once; further build jobs stay queued in
  Forgejo while other jobs run. A build costs one billed hour of `cx23`
  plus its IPv4: 0.0096 EUR net.

## factoryd's own network

factoryd holds the LiteLLM master key, the bot password, the S3 keys and
the Hetzner CI token, so its egress is default-deny (`factoryd-egress` in
`k8s/factoryd.yaml`): kube-dns for exactly the names below, the API
server (by entity; factoryd dials the injected
`KUBERNETES_SERVICE_HOST`, no DNS), the `sf-cluster` Postgres instances
on 5432, the in-cluster Forgejo on 3000 (`FORGEJO_URL`; the public host
resolves to the edge gateway; links for people use
`FORGEJO_PUBLIC_URL`), LiteLLM on 4000, the Temporal frontend on 7233,
the S3 endpoint on 443 (its FQDN must match `S3_ENDPOINT`), and
`api.hetzner.cloud` on 443 (build VMs). Every
callee is named in full and the pod runs with `ndots: 1`, so the first
DNS query is the admitted one. A new callee needs a rule here first.
Ingress (`factoryd-ingress`): the node on 8080 (probes), Forgejo's
webhooks and the gateway's escalation hand-over on 8080, and the edge
gateway's Envoy (entity `ingress`) on the console port 8090 only.

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
