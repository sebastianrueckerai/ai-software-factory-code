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
  call (402 `refused_budget`, `refused_day_cap` or `refused_month_cap`
  when it cannot fit). The per-task LiteLLM virtual key is limited to the
  classes' aliases and capped at the smallest headroom; a new run, policy version or UTC day
  mints a new key and blocks the old one once the calls still using it
  are done. A LiteLLM budget refusal is labelled by the headroom the key
  was capped at (`refused_budget`, `refused_day_cap` or `refused_month_cap`);
  LiteLLM's proxy-wide `max_budget` (its message names no entity) is
  `refused_month_cap`. Every decision has an audit row
  naming the policy version; an admitted call's row also carries its
  output limit and hold. factoryd's own model calls (the session
  supervisor, the gate supervisor, the model review, the audit) take the same route:
  the `gateway-factoryd` token (the gateway's `FACTORYD_TOKEN`) with the
  task in `X-Factory-Task` and the purpose (`supervisor`,
  `gate-supervisor`, `review`, `audit`) in `X-Factory-Purpose`. Such a call may use
  only the alias the task's class names for the purpose
  (`supervisor_model`, `review_model`, `audit_model`; 403 `refused_capability` else),
  runs while the task is running or in review, is held and booked like an
  agent's under the ref `<purpose>/<alias>` (e.g. `review/review-frontier`),
  and its audit row has the purpose as `kind` (`agent` for an agent's
  call, `factoryd` for a factoryd call refused before its purpose is
  known). The `X-Factory-*` headers are read only behind the token: with
  a sandbox's JWT or a wrong bearer they are ignored and the call is an
  agent's. Its LiteLLM key is the task's factoryd slot, apart from the
  session's key, limited to those aliases. A refusal names its decision in
  the `X-Gateway-Decision` header. It also serves the agent's tools over MCP at
  `/mcp`, see "Tools".
- `cmd/agent-worker`: pid 1 in the sandbox pod. Clones the base commit
  from the repository with the read token before the agent starts (a
  resumed task branch that only the fork holds is fetched from there),
  runs `opencode serve` on 127.0.0.1 in its own process group without
  either token and drives one session over its HTTP API the way
  `opencode run --auto` drives its own (every permission ask answered
  "once"; the fixture `TestRecordOpencodeServeFixture` records), serves
  factoryd's `POST /message {text}` on port 8091 (bearer per session, the
  pod holds only its hash in `MESSAGE_TOKEN_SHA256`; the text goes to the
  session's next turn through `prompt_async`) (headless:
  every looping agent capped at `limits.steps`, the `question` tool
  hidden, `doom_loop` allowed, a rejected tool call an error the model
  sees and the loop goes on; `buildOpencodeConfig`, its golden file and
  the fixture `TestRecordOpencodeFixture` records from the pinned binary
  with `OPENCODE_RECORD_BIN` set, on every version bump), kills
  and reaps every other process when the agent is done, then fetches
  the agent's commits into a clean repository and pushes `factory/<id>`
  from there to the fork `factory-agent/<repo>` with the write token.
  The sandbox image (`Dockerfile.sandbox`) carries opencode, git, ripgrep
  and the Go 1.25 toolchain (ruling 99, the same pin as the runner
  image); its egress (`workerEgressSpec`) is default-deny plus the
  gateway, the in-cluster Forgejo, the Go module proxy and checksum DB,
  and `storage.googleapis.com` (proxy.golang.org redirects zip downloads
  there) by name on 443, SigNoz's collector pods on 4318 when factoryd has
  an `OTEL_EXPORTER_OTLP_ENDPOINT` (ruling 158: the worker's own spans and
  metrics; the pod gets the same variable), and kube-dns for exactly
  those names; the cloud metadata address is denied outright. The module proxy is a covert
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
event type is logged, never the body. Every issue becomes a card of the
lane its labels ask for (`internal/factoryd/lanes.go`), idempotent per
(repository, issue number): redeliveries and later relabel events on the
same issue find the task already linked. The lane rule is fixed in code,
the same for every process:

| labels | lane |
| --- | --- |
| `human` (wins over the rest) | human lane, `waiting-human`, no agent run, no lane or process on the row |
| `agent` or `bug` | ready |
| `plan` | plan where the project's process has a plan lane, else backlog |
| `backlog` or `improvement` | backlog: status `backlog`, no workflow |
| none of these | ready (the default entry) |

A card entering a starting lane (plan, ready) is judged against the
lane's `moved_by`: writers (write access on the repository) or
approvers (the policy's). A new issue with a lane label judges its
poster; the default entry (no lane label, or `bug`) enters ready from
anyone under `writers` and from an approver only under `approvers`. On a
backlog card, a lane label added, or a lane label removed so that none is
left, judges the event's sender; the heal pass and the bot know no sender
and move nothing. A refused entry lands in the backlog: factoryd takes
the starting lane labels off, puts `backlog` on and comments once why
(`factoryd: lane ready takes a card from writers of <repo>; <user> is
none. The card stays in backlog.`). A card that enters a starting lane
is queued at the branch head with the lane's class as its capabilities
and its workflow starts; the row keeps its lane and its process
(`tasks.lane`, `tasks.process`). A backlog lane with `write_up` opens that
class's write-up of the card (source `presort`). Until it moves a
backlog card runs nothing: comments on it are neither recorded nor
signalled, and a writer's close (webhook or heal pass) ends it `closed`.
Forgejo 15 applies whatever labels its
new-issue form posts, from any account (checked on 15.0.9: an account
with no access filed issues with a template's label, with smuggled
`agent` and `improvement` label ids, and with a label and no template;
all three kept their labels, while the API drops a non-writer's labels
and refuses its later label edits with 403), so from an outside account
intake reads only `bug`, `improvement` and non-lane labels: `agent`,
`plan`, `human`, `priority/*` and `severity/*` count from writers only,
and intake takes them off an outside account's new issue. The task
keeps its author and whether they could write the repository then
(`tasks.author`, `tasks.author_writer`; ruling 152); an API task is the
bot's, as a writer. The day cap bounds the spend; there is no per-author
limit. The heal pass reads every open issue of every active project (a project
file, and for `objects: factory` a registration that has run), the
factory's own two repositories included. The issue templates in
`.forgejo/issue_template/` (`bug.yaml`, label `bug`, with a reproduction
field rendered as the `repro` block; `improvement.yaml`, label
`improvement`) are what every factory-managed repository carries; their
labels must exist in the repository, which factoryd ensures (the boot
labels below) for every project. An issue the bot user
(`FORGEJO_BOT_USER`, default `factory-agent`) filed with no task linked
is ignored, as are its comments and the pull requests it closed itself,
so the factory's own status updates never create a second task or a
stray signal; a bot issue with a task row (a plan's card) takes its
label events like any other. `POST /tasks` and SigNoz alerts file a
Forgejo issue through `FileAgentTask`: the default entry into ready of
the project's process (the backlog where the approvers move ready), so
every task — API-created or webhook-created — has exactly one linked
issue. A roll takes ready's class and no lane.

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

A task's state (`backlog`, `queued`, `running`, `waiting-human`,
`waiting-poster`, `in-review`, `done`, `failed`, `closed`) is mirrored
onto its issue as exactly one `state/*` label, swapped by `Forgejo.SetStateLabel` through the shared
`mirrorTaskState` helper (`internal/factoryd/forgejo.go`), the one call
site behind the task workflow's activities, completion, and the webhook
intake; the full label set (`agent`, `human`, `bug`, `improvement`, the
priorities and severities, one `state/*` per status, and the lane labels
of the project's process: `backlog`, and `plan` where it has a plan lane)
is created at boot by `EnsureProjectLabels` if missing, and on every
project by its registration; that step first renames `state/suggested`
to `state/backlog` in place. Mirroring
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
`limits.sessions` (3) attempts, each `SetState running` then
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
(below) can redirect, climb or escalate. Red checks on a head that a
session left as it was (the agent could not act on them) run once more
before their findings go back to the agent: `RerunChecks` puts an empty
commit on the branch (Forgejo 15 has no API to re-run a job); a head red
after its re-run is not re-run again (change id `checks-rerun`). Signals
from the webhook intake are delivered with signal-with-start, so a signal
to a task whose run died starts a new one (which reconciles first).
`pr_merged` (done),
`pr_closed` (closed without merge: failed) and `issue_closed` (done if
the PR merged, else failed) decide the task wherever they land: a
running session or check wait is cancelled within about 10 s (the
session deletes its pod and namespace and revokes its credentials
first). The cancel reaches an activity in the answer to its next
heartbeat: activities heartbeat every 10 s (the check wait polls and
heartbeats at that interval too) and the worker holds none back longer
(`MaxHeartbeatThrottleInterval`); with the SDK's default throttle (60 s
under the 2-minute heartbeat timeout) a closed issue's session ran on
for 56 s in the Stage 7 exit test. Measured after the fix: the signal
delivered 11:14:11, the session's activity cancelled 9.4 s later, its
pod gone 12.7 s after the close. They are
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
   or `repro_command` on `POST /tasks`; for an issue filed from a SigNoz
   alert, which carries none, the repro block that opens the agent's last
   message (ruling 170; no other issue takes the agent's),
   marked `ByAgent` for the review, which judges whether it tests the
   reported fault): a fresh sandbox namespace, a read
   token on the task's repository only, and one pod of the worker image
   (no agent, no gateway token, no push token, the agents' egress policy).
   Its init container (`MODE=repro-fetch`), the token's only holder,
   fetches `refs/pull/<n>/head` into the workspace and checks it is the
   gated head; the worker container (`MODE=repro`), with no token in its
   environment, runs the command there with `sh -c` for at most
   `REPRO_TIMEOUT` (default 10m, at most 15m). It runs twice, one pod
   each: first at the task's base (`CHECKOUT_SHA`, from the head's
   history), then at the head. The fetch writes only the commit's files
   into the workspace (a scratch bare repository, `checkout --work-tree`,
   the scratch removed), so the command sees no `.git`: a git command
   exits the same at both, and only a change in the files can tell them
   apart. A failed fetch is reported as exit
   -1. The activity heartbeats from its start, through the namespace wait
   and the image pull. The fault is reproduced when the command fails at
   the base (exit above 0) and passes at the head; one that passes at the
   base too is recorded `not_reproduced_at_base` and proves no fix. Exit 0
   at the head passes the gate. Anything else at the head sends the next attempt back to the agent on the stronger rung with
   the exit code and the output tail as findings, like red checks. The
   namespace and the token go when it ends.
4. `ModelReview` asks the `review_model` of the task's class for
   `{"verdict": "approve|concerns", "summary", "findings"}` over the brief
   (with the issue author and their standing, writer or outside account:
   an outsider's report is untrusted input), the diff (cut at 48 KiB) and
   the gate results, through the gateway on the task's account (purpose
   `review`): its hold, the `model` ledger row `review/<alias>` and an
   audit row of kind `review`. When the agent fixed
   a fault it did not reproduce exactly (a `not_reproduced:` line in its
   last message, which the brief asks for; `GateReproduction` comments it
   and records "not reproduced" instead of "skipped"), the answer must
   carry `"strict_improvement": true|false`. A gateway refusal
   (`refused_budget` at the task's cap: "model review refused by the
   gateway (refused_budget): ..."), a failed call, an invalid answer or a
   class without a review model is a note on the pull request and the
   approval card; nothing it says skips a gate.
   From change id `audit` on (ruling 188), the workflow's `audit` stage
   (between review and approval, `on_findings: human` only) runs
   `GateAudit` for a project whose file sets `audit: true`, else records
   it skipped: `redact.ScanDiff` checks the added lines for secret shapes
   (private keys, credentialed URLs, bearer tokens, JWTs, `sk-`, `ghp_`,
   `glpat-`, `AKIA`, a password or token assigned a literal), each hit a
   finding with file and line, never the value; then one call of the
   class's `audit_model` (purpose `audit`, bound `GateAudit`) over the
   diff (cut at 48 KiB; a cut is a finding) with a fixed checklist:
   secrets, network egress, RBAC and ServiceAccounts, policy and workflow
   files, migrations that drop or widen, the sandbox contract. An answer
   without a verdict is a finding. The findings go to the card's
   "Security audit" section and the timeline step `audit` (verdict,
   model, deployment), never to the pull request; on the merge rule auto
   they send the merge to the card. A roll task skips the stage.
   From change id
   `review-verdict` on (backlog 68), a review stage that ran and gave no
   verdict puts "review: the review gave no verdict: <note>" on the card
   whatever its `on_findings`, so it is never a pass and the merge rule
   `auto` never merges past it; a roll's review (no model call) is no
   finding, and the review stage takes no `when`: `enabled: false` is its
   only skip.
5. From change id `auto-merge` on (ruling 149), `AutoMerge` decides:
   the project's merge rule is `auto` (`projects.merge_rule`, read by
   `GatePolicy`; the factory's own repositories are `human`), the risk
   class's outcome is `auto` (allowed on class `normal` only; the policy
   decoder refuses it on any other), the review's verdict is `approve`
   with `strict_improvement: true` (ruling 169: the review answers it on
   every pull request; false or missing sends it to the card), and the
   fix is proven one of two ways (ruling 167): the reproduction failed at
   the base and passes at the head, or the agent recorded the fault it
   fixed (`not_reproduced`), with or without a reproduction command.
   Then `AutoMergeNote` comments the three conditions and the review's
   summary on the pull request and `Merge` merges as factory-merge with no
   card (main of an `auto` project requires no approval). Anything else,
   or an auto merge Forgejo refuses or that fails, goes on to:
6. `RequestApproval` opens an `approval_requests` row (kind `merge`,
   status `pending`, the task, project, pull request, and in `spec` the
   risk class, the migration flag and paths, the review's verdict and
   summary, the reproduction, the head, the agent's recommendation (its
   last message, `tasks.agent_summary`) and why no auto merge). A task
   has one pending merge request; a new attempt denies it as `factoryd`,
   and the task's end denies its pending requests of both kinds (a
   resource request survives a new attempt).

The workflow then waits, without a timer, for `approved`, a change
request, `pr_merged`, `pr_closed` or `issue_closed`. The console approves
by submitting the human's APPROVED review through the human's own OAuth
token, then calling `factoryd.ApproveMerge` (a pending merge request of
that task and pull request is required), which signals `approved` with
signal-with-start like the webhook path. `Merge` then merges as the
dedicated user `factory-merge` (`FORGEJO_MERGE_TOKEN`, secret
`factory/forgejo-merge`, on the factory's own repositories; a registered
project's own `projects.merge_token`, which also reads the project's
writers for the trust gate) with `head_commit_id` the gated head, and the
task finishes done. main's protection lets factory-merge merge but not
push, and only after an approval by a user on the approvals whitelist, so
Forgejo itself refuses the merge without the human (405; a moved head or
a conflict is 409). A refusal is commented and the wait goes on. Merge
refuses by itself a pull request whose base is not `main`, and a gated
head whose combined commit status is not green (`merge_red_head`, the
card reads `the checks are red on <head>: the merge waits for a green
head`): the approval stays, and a change request sends the agent back. A merge that
fails otherwise (Forgejo unreachable) is retried five times, then
commented and recorded (`merge_failed`); the approval stands and the
merge is tried again after 10 minutes. From change id
`merge-failure-reported` on, a Merge that ends in an error (a 404, its
last attempt timing out) is reported the same way (`ReportMergeFailure`)
and retried too; a newer approval replaces a pending retry. From change
id `merge-retry-cap` on (ruling 129), one approval's merge is retried at
most six times (an hour); then a single comment and a
`merge_retries_exhausted` step say so, no timer runs, and the task waits
for a signal: a new approval restarts the merge with a fresh count. A human merging in Forgejo stays
valid (`pr_merged`, or found by Merge and credited to that human, with
the head they merged). An approval signalled before the request opened,
or for another pull request, does nothing. From change id
`gates-pinned` on, `GatePolicy` and `ModelReview` check that the pull
request's head is still the gated commit (a moved head fails the policy
gate and skips the review with a note), and the model review races the
terminal signals like the checks and the reproduction (it heartbeats, so
the cancel cuts its model call, whose hold is booked); a task without a
reproduction command gets a `gate_reproduction` step saying so. The second pull request merged this way proved the pinned gates.

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
`limits.same_call` times in a session, 4, pinned; the repetition gets its
nudges first), flip-flop (a file returns
to an earlier content hash), churn (`SUPERVISE_CHURN` file changes since
the diff against the base last grew, with no command newly passing, 15),
half way (half the agent time `limits.agent_time` or half the budget;
once per task, recorded on the task row), and no write (rulings 141,
161): an agent event, a tool call, command or model call, arriving
`no_write` seconds after the last write, edit or patch tool call or the
session's start (the policy's `supervisor` section, read at each session
start, 300 by default); a tick or the worker's checkpoint never trips it,
silence is the stall's. The wall-clock cap ends the
session. Every other trip asks the supervisor, the `supervisor_model` of
the task's class, through the gateway on the task's account (purpose
`supervisor`): its answer limit is clamped to the free headroom, the
worst case is held, its cost is booked as a `model` ledger row with ref
`supervisor/<alias>`, and it has an audit row of kind `supervisor`; a
call the gateway refuses escalates.
It reads the brief, the trip, a progress digest and the session record's
tail, and answers one of `continue` (the detector resets, the session
runs on), `redirect` (new session, same rung, its guidance as findings),
`retry_stronger` (the next rung; at the top rung a redirect) or
`escalate`; an invalid answer or a failed call escalates. At most
`limits.supervisor_calls` (3) calls per session, each bounded by
`limits.supervisor_timeout` (6 min); the next trip escalates
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
more than `limits.agent_time`) of agent time is left under the cap, the
answer also grants the difference, so every answer gets at least that
slice of working time. Both grants are recorded on the task row. Every
decision is a `supervisor` event in the session record, a timeline step
and an issue comment.

A long gate past its bound goes to the gate supervisor (rulings 141,
162, change id `gate-supervisor`). `GateWatch` writes the `gate_bound`
alert and, for RunSession, WaitChecks, GateReproduction, ModelReview and
Merge, signals `gate_bound` to the task's workflow with the activity, its
run's id, the attempt, the alert and the bound. The short gates (policy,
migration, approval request) keep the alert as their record: their
2-minute attempt ends before a supervisor answer could arrive. The
workflow takes the signal while that activity runs and calls
`SuperviseGate` once per trip. factoryd runs one replica, so the attempt
that tripped runs in this process or has ended; one that ended (or a
later attempt of the same run) costs no call, and its alert is `noise`.
factoryd assembles a read-only bundle, the model gets no tools: the
task's objective and timeline tail, the activity's input (8 KiB), bound
and elapsed time, the run's last failure (Temporal's pending activity
info), and per activity the check statuses of the commit with the CI
runner queue (`WaitChecks`), the sandbox pod's phase, conditions,
container states and last 100 log lines (`RunSession`,
`GateReproduction`), the pull request's state and mergeability (`Merge`)
or the review model and its last `model_review` step (`ModelReview`). Every string of the bundle goes
through `internal/redact` with the task's git tokens, the factory's one
scrubber (the gateway's audit records and the worker's git errors use it
too); its 32-character rule also takes long URL paths and commit ids.
The reads have a minute. The `supervisor_model` answers through the gateway
like the session supervisor, on the task's account (purpose
`gate-supervisor`): `wait` (the attempt's bound is extended once by its
own length, and at least half a bound after the answer; not offered for
a session, whose start-to-close ends first), `retry` (the attempt is
stopped with the retryable error `gate_supervisor_retry`, so the
activity's retry policy runs it again; not offered on its last allowed
attempt), `cancel` (the workflow cancels the activity and fails the task
with the diagnosis) or `escalate` (the workflow cancels it and escalates
like the session supervisor, the diagnosis and the recommendation in the
reason; the human's wait is no agent time). A human's answer to a
session's escalation buys the next session; to a gate's, it runs the
gate again with the same arguments (a merge keeps its approval), and a
change request sent meanwhile goes back to the agent. One `wait` and one
`retry` per run; a disallowed or malformed answer, a refused or failed
call escalates. The action, the diagnosis and the recommendation go on
the alert (`detail`, verdict `noise` for `wait`, `real` otherwise,
`resolved_by` `supervisor`) and on the timeline (`gate_supervisor`). The
call runs beside the gate (ruling 171, change id `gate-supervisor-race`):
a gate that ends first wins, the workflow goes on with its result, and
the late answer is recorded on the alert with the note "gate ended
before the answer" and acts on nothing. A workflow started before the
change leaves the signal unread.

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

## Configuration, policy and costs

factoryd's configuration is the directory `factory/` of the infra
repository's `main` (ruling 183): `policy.yaml` (classes with their
`session: code|plan|report`), `processes/<name>.yaml` (ruling 206: the
blocks `lanes`, `dependencies`, `stages`, each with its `on_findings`
rule, ruling 177, and `merge`) and `projects/<name>.yaml` (one per
project: repository, merge rule, `objects: factory|seed`,
`agents: coder|none`, `process`, notify, namespaces, environments,
quota, secrets). The Go types in `internal/policy` are the
schema: `factoryd schema --out <dir>` writes one JSON Schema per file
kind (draft-07; every enumerated field lists its values, every key its
doc comment, a key is required unless its yaml tag says `omitempty`),
committed as `factory/schema/`, and each file's first line references
its schema (`# yaml-language-server: $schema=schema/policy.schema.json`,
`../schema/project.schema.json` under `projects/`,
`../schema/process.schema.json` under `processes/`) for completion and
linting in an editor. `factoryd config check <dir>` decodes a tree as
the loader does and compares its `schema/` with the generated one: exit
0 ok, 1 refused with every reason, 2 usage; the infra repository's CI
runs it on every push to `main`. `factoryd config explain [--tree <dir>]
<process> <card kind>` prints the path of a card kind (`bug`,
`improvement`, `plan`, `child`) through a process, generated from the
same graph whose check refuses a dead end or an uncapped cycle.

factoryd polls the head of `main` of `CONFIG_REPO` (the infra
repository, set in `k8s/factoryd.yaml`) every 30 s through the Forgejo API
with `FORGEJO_CONFIG_TOKEN` (user `factory-config`, a read collaborator
on the infra repository only, `read:repository` limited to it; secret
`forgejo-config`), reads `factory/` at that commit (`schema/` left out)
and decodes each file strictly: one YAML document, the schema header on
its first line, no unknown key, the Go rules, then the schema (required
keys, enumerations). A valid tree becomes an immutable `policy_versions`
row keyed by the commit (`commit_sha`, the whole tree as one JSON
document, `loaded_at`); the version in force is the latest row of the
append-only `policy_activations`; a commit seen before activates its
earlier row again. A refused commit, or one where a class a starting
lane names leaves out a model of the ladder (`MODEL`, `STRONGER_MODEL`), logs `CONFIG
REJECTED`, keeps the previous version current and files, once per
commit, a human-lane issue on the infra repository (a `human` task,
source `config`) and an open alerts row of kind `config_rejected`
(written first, so a failed filing is tried again without a second
issue). The next valid commit that loads settles them: each open config
issue gets a comment naming the loaded commit and is closed as the bot,
its task ends `done`, its alert is judged real. A failed read (a revoked
token, Forgejo unreachable) is tried again at the next poll; ten in a
row (about five minutes) raise one `config_rejected` alert of source
`read`, judged real, without an issue. factoryd refuses to boot only
when no valid version exists at all, and the gateway refuses every call
while the version in force does not validate. `config check` judges a
tree with the loader's own function (`factoryd.CheckTree`, the ladder
from `MODEL` and `STRONGER_MODEL` included). `GET /policy` and the
console's configuration page show the version in force, read-only, each
file linked at its commit.

A task's workflow pins, at its start, the version in force
(`WorkflowConfig.PolicyVersion`). Read from the pinned version: the
session cap, the agent time and the stronger rung (captured by the
workflow); the task's process (its first activity, `TaskConfig`, from
change id `processes`: the row's process, or where the loader has not
filled it yet the process of the task's project, with the row's lane,
its stages, merge queue and dependency block, and the session mode of
the lane's class (ready's for a roll, which has no lane), or of a report
task's own class, which chooses
the program: `code` the lifecycle, `report` the write-up, `plan` not
built yet); and in every session, report session,
gate-supervisor call and model review the agent time, `steps`, `supervisor_calls`,
`supervisor_timeout` and the `no_write` detector, the risk rules
(`GatePolicy`), the factoryd model aliases (`review_model`,
`supervisor_model`), and the activity bounds of a session, a
gate-supervisor call and a model review (`SessionInput`,
`GateSupervision`, `GateResults`, `GatePolicy` and `RunReport` carry the
pin; 0, a workflow from before the pin, reads the version in force).
Read live, from the version in force when used: everything the gateway
enforces (classes, tool caps, prices, the day cap), the budget at
intake, the approvers, the bounds of the other activities,
`envelope_default` and `project_default`.

`limits` in `policy.yaml`: `sessions` (the round cap, 3), `agent_time`
(20m, the wall-clock cap across a task's sessions), `supervisor_calls`
(3 per session), `supervisor_timeout` (6m per supervisor or review call,
at most 15m), `stronger_after` (1: the second return from the same
stage takes the stronger rung) and `steps` (200, also when absent: the
model turns of one agent session, the worker's `STEPS`), `nudge_after` (3)
and `nudges` (1), also when absent, `nudge_after` below `same_call` (4):
that many identical tool calls in a row, or `same_call` of them scattered
over the session (same tool, same input, from the session's event stream), get the
running session the message "You repeated the same call three times
(<tool>). Change approach, then continue." and a `nudge` timeline step,
once per repetition, counted only when the session took it; the next such
run trips `same_call` to the supervisor. A harness without messages gets the redirect with that text,
decided without a supervisor call. The activity bound of `RunSession` is the agent time plus a minute, of
`SuperviseGate` the supervisor timeout plus two, unless `bounds` names
them.

The policy's other sections, schema v1: `budget` (`task_default_eur`, `task_max_eur`,
`day_eur` for the UTC day and `month_eur` for the UTC calendar month over
all tasks) and `classes` (per capability
class the model aliases it may call, its tools with per-task caps, and
the aliases factoryd calls on its tasks' account, which the agent may
not: `review_model` for the model review, `supervisor_model` for the
session and gate supervisor, `audit_model` for the audit; a class without one gets no such call).
The gateway never reads the files, only the version in force in
Postgres. `day_eur: 0` or `month_eur: 0` stops all spend. Because the decoder is strict,
a new key lands in infra first and the factoryd that reads it rolls
right after: the running factoryd refuses the commit, keeps its version
and files the issue, and the new one loads the head at boot.

The gateway checks a model call in this order: the caller (a sandbox's
JWT, or factoryd's `gateway-factoryd` token with its `X-Factory-*`
headers, which are read only behind that token), the task status (an
agent's task running; factoryd's running or in review, checked with the
hold), the alias (an agent's must be in its classes' `models`, factoryd's
must be the class's alias for the purpose; else 403 `refused_capability`),
the task headroom (else 402 `refused_budget`), the day headroom (else
402 `refused_day_cap`) and the month headroom (else 402
`refused_month_cap`). `review-frontier` and `supervisor` are not in
`coder`'s `models`: the agent cannot call them.

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
total; month: `month_eur` minus the ledger total since the 1st, UTC;
standing lines count for neither), the output limit (`max_tokens` or `max_completion_tokens`) is
clamped to what that pays for, and a `holds` row takes the worst case.
Fewer than 1024 output tokens (a floor any model needs room to fill,
reasoning or answer) is refused unless the caller asked for fewer. After the
call one transaction
deletes the hold and books the real cost onto `tasks.spent_eur` and the
ledger; an unknown cost (no cost header, a broken stream, a value that
is not a finite non-negative number) books the hold, and a failed call
books the cost LiteLLM reports for it, else nothing. A known cost above
the hold (a stale price table, a wrong input bound) is booked in full and
flagged by the booking itself (`policy.OverHold`), the one flag of every
path: model, supervisor and review, tool and resource accrual log
`COST OVER HOLD` with both figures, and every gateway audit row written
after the booking carries it in `audit.over_hold` whatever its decision
(`allowed`, `error_upstream`, a 402 refusal, `error_tool`, `accrued`;
empty within the hold). A hold whose caller
died expires after an hour and factoryd's sweep books it in full and
writes its audit row (decision `swept`, kind and alias from the hold's
ref); a late release books only what the real cost exceeds it by.
Settlement leaves a task alone while it has an open hold. Output limits bound
reasoning: verified on the earlier reasoning models (reasoning counts inside
`max_tokens`); Qwen's thinking
does not, so the dashscope deployment runs with thinking off, and the
gateway refuses a client's `thinking` field. `holds` has kinds `model`,
`tool` (see "Tools") and `resource`. During a superseded key's grace (the
calls still in flight on it when a new run, policy version or day mints
its replacement), a task briefly has two live keys, each capped at the
headroom of its own mint, so LiteLLM does not count one key's spend
against the other; the holds remain the primary cap. LiteLLM's
`fail_closed_budget_enforcement` is the backstop.

The `ledger` table holds every booked cost, append-only: task, UTC day,
kind (`model` per call with the alias as ref, `settlement` for the
difference the settlement pass books between LiteLLM's model spend and
the task's `model` and `settlement` rows (tool rows never offset it; a
difference below 1e-9 EUR is float noise and books nothing): inside the
settle window only a raise, dated the day the pass runs; once past the
window the final row sets the task to LiteLLM's figure, down as well as
up, dated the task's finished day (ruling 196), `tool` per tool call with
the tool as ref, `resource` for the later leases), ref, EUR (never
negative, except a final `settlement` row). Costs are LiteLLM's USD prices
booked 1:1 as EUR, which errs high. The schema of `policy_versions`,
`policy_activations`, `ledger` and `holds` lives in
`internal/policy/schema.sql`. Both binaries share the `gateway` database
and migrate all of it at boot (`internal/schema`): factoryd's and the
gateway's migrations (`internal/schema/migrations/<set>/`, by file name)
and then the policy schema, under one advisory lock, so whichever boots
first migrates and the other waits. Each file applies once with its row
in `schema_migrations`, logged as `migration <set>/<file> applied`; a
boot with nothing to apply logs `migrations up to date`.

Both endpoints take the intake bearer token:

- `GET /policy`: the version in force, `{id, sha256, document, loaded_at, activated_at}`.
- `GET /costs?from=YYYY-MM-DD&to=YYYY-MM-DD` (UTC days, both included,
  each defaulting to today, at most 366 days apart): `days` (per day
  `by_kind` and `total`) and `tasks` (per task total, largest first, with
  its `lines` per ledger `kind` and `ref`, so a review is its own line,
  `model review/<alias>`).

## Tools

The gateway serves the agent's tools over MCP at `/mcp` (streamable
HTTP, stateless, `github.com/modelcontextprotocol/go-sdk`), behind the
same task-JWT check and task lookup as the model path. The sandbox's
identity proxy forwards `/mcp` with the pod JWT like the model calls;
opencode's config names it the remote MCP server `factory-tools`
(timeout 150 s, above the slowest connector; the pinned opencode cuts a
tool call at this timeout), and the model sees the tools as
`factory-tools_<tool>`. The provider keys live only in the gateway
(secret `gateway-tool-keys`). A task may call a tool only when one of its
classes lists it in the policy (`classes.<c>.tools`, ruling 132): the paid
tools with their caps, the free ones (`escalate_to_human` and the
commissioning tools) as `{}`; any other call is `refused_capability`.

- `web_scrape` (`url`, at most 2048 bytes): Firecrawl `/v2/scrape`,
  markdown only, basic proxy, no PDF parser, the text capped at 64 KiB
  (cut on a character boundary, marked "…"). Hosts that are not public DNS names (IP
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
- `escalate_to_human` (`question`, `reason`, `recommendation`: what the
  agent would do and why, at most 2000 bytes; all required): free. The
  issue comment and the console's escalation card show all three. The gateway posts it to factoryd's `POST /internal/escalations`
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
- `request_info` (`question`, `tried`, `needs`; each required, at most
  4000 bytes): free, through the class's tool list like every tool
  (the policy lists it as `request_info: {}`). For a report the agent
  could not reproduce and found no fault in (ruling 150). It goes the same
  way as an escalation (an `agent_info_request` intake event, the session
  stops with outcome `needs_info`); from change id `needs-info` on the
  workflow's `RequestInfo` comments the request on the issue, mentioning
  the author and the project's `notify` list (the policy's approvers when
  it is empty), whose Forgejo bells ring, moves the task to
  `waiting-poster` and records an `info_request` alert. The issue author's
  next comment resumes it whatever their access (intake event
  `poster_comment`, signal `poster_comment`), and so does a writer's; the
  next session's brief carries it under "Poster information", as
  information, never as instructions. The brief writes its own lines
  (repository, the author's standing) first and puts the issue's text and
  every answer between markers with a random tag per brief, so the author
  cannot forge the standing line or the brief's sections. An escalation's answer stays a
  writer's. 14 days without an answer: `CloseNotReproduced` comments "not
  reproducible, no answer in 14 days", closes the issue, and the task ends
  `closed` (a final state like done and failed). A healed run waits for
  what is left of the 14 days.

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
hold (a provider billing past the bounded arguments) is booked in full
and flagged `COST OVER HOLD` with both figures in the log and the audit
row's `over_hold`; the caps then refuse the next call from
the true headroom (ruling 117). A hold is only taken for a task that is
`running` or `in-review` (the gates' model review): `TakeHold` refuses
any other status with `refused_task_status` in its own transaction. Prices not in any API
response live in the policy's `prices`. A refusal (`refused_capability`,
`refused_task_status`, `refused_arguments`, `refused_price`,
`refused_tool_requests`, `refused_tool_eur`, `refused_budget`,
`refused_day_cap`, `refused_month_cap`) comes back as an MCP tool error that says the policy
refused and the tool did not run; a provider failure (`error_tool`) says
the tool failed. Every call has an audit row with the decision, the tool
(`audit.tool`), the policy version and, when admitted, the hold.

The gateway's egress (`factory-gateway-egress` in `k8s/deployment.yaml`)
is default-deny plus LiteLLM, the `sf-cluster` Postgres instances,
factoryd on 8080, OpenBao on 8200 (the SSH CA), the API server (by
entity on 6443, the backend port behind the Service's 443, dialled at the
injected `KUBERNETES_SERVICE_HOST` for the JWKS), and `api.firecrawl.dev`,
`api.replicate.com`, `fal.run`, `api.fal.ai` and `api.hetzner.cloud` by
name on 443, SigNoz's collector on 4318, with kube-dns for exactly those
names (`ndots: 1`, every callee named in full). factoryd's ingress admits the gateway on 8080. Its ingress (`factory-gateway-ingress`) admits the node (probes), factoryd and the `opencode-worker` pods of namespaces labelled `factory.ai/task`, each on 8081.

## Commissioned resources

Agents get servers for a task through gateway tools (architecture 5.5),
never through a provider key. They live in their own Hetzner project,
`agent`, whose token only the gateway holds (`HETZNER_AGENT_KEY`, secret
`hetzner-agent`); factoryd never sees it. Every tool is for running
tasks whose classes list it (the infra policy lists all of them for
`coder`); the project is the task's repository (`owner/name`).

- `cloud_prices`: the project's server types with EUR/h and EUR/month
  (net, `fsn1`, the server's primary IPv4 included), the volume price,
  and the envelope with what the month has used. Prices come from
  `GET /v1/pricing`, cached an hour (a failed refresh keeps the last
  catalogue).
- `commission_server` (`server_type`, `purpose`, `lease_minutes` at
  least 5, optional `volume_gb` 10 to 1024 and `ssh_public_key`): a Debian 12
  server with a public IPv4 only, a firewall admitting SSH only from the
  cluster nodes' IPv4s (`AGENT_SSH_SOURCES`, ConfigMap `cluster-facts`)
  and outbound only DNS, HTTP and HTTPS, and an optional ext4 volume
  mounted at `/mnt/HC_Volume_<id>`. cloud-init installs the OpenBao SSH
  CA's public key as `TrustedUserCAKeys`, lists the one principal
  `resource-<row id>` in `AuthorizedPrincipalsFile` for root, and sets
  `AuthorizedKeysFile none` and no passwords: only a certificate for this
  server logs in, as `root` (ruling 125). The answer
  carries the resource id, the IPv4 and the lease end, and a certificate
  when a key was given.
- `ssh_certificate` (`resource_id`, `public_key`): for one of the task's
  own servers only, OpenBao signs the key for principal
  `resource-<row id>`, for 30 minutes or until the lease's cutoff (3.5
  minutes before it ends, when the egress drops the server), whichever is
  sooner, with key id `task=<id> resource=<id>`.
- `list_resources`, `release_resource` (`resource_id`: the server, its
  firewall and volume are deleted now), `resource_request_status`
  (`request_id`).

**Pricing.** A lease is billed per started hour: hours = ceil(minutes /
60). The reservation is hours x (server hourly + IPv4 hourly) + hours x
volume GB x GB-month price / 730. For a cx23 in fsn1 for 90 minutes with
20 GB: 2 x (0.0088 + 0.0008) + 2 x 20 x 0.0572 / 730 = 0.0192 +
0.0031342 = 0.0223342 EUR. The monthly figure on approval cards is the
catalogue's monthly prices (5.49 + 0.50 + 20 x 0.0572 = 7.134 EUR).

**Envelope.** A project's envelope is its file's (ruling 183):
`envelope` (`monthly_eur`, `server_types`, `max_count`,
`max_lease_hours`, `volume_max_gb`) and `standing` in
`projects/<name>.yaml`, each absent block the policy's
`envelope_default` or `project_default.standing`; a repository no file
names takes the defaults. The gateway reads it from the policy version in
force; a change is a commit to the infra repository. Under the headroom lock, one transaction checks the envelope: the
type, the lease, the volume, the count of open servers, and this UTC
month's resource ledger rows plus open reservations plus this
reservation against `monthly_eur`. Within it, the reservation is a
`resource` hold against the task's and the day's headroom (a refusal
otherwise, as for paid tools) and the registry rows are written in the
same transaction. Beyond it, the tool files an `approval_requests` row
(kind `resource`, the spec, the reason, `monthly_eur`) and answers
`pending human approval <id>`. The spec records the ways the request is
beyond (`beyond_kinds`: type, lease, volume, count, month). A request
covers a commission of the same type, at most its lease and volume, whose
ways beyond are all among its own: asking again finds the same pending
request, an approval admits one such commission for the task (the server
row records `approval_id`, the request is marked `consumed`) and nothing
that is beyond in another way, and
a denial is answered with the human's note. A task holds at most 3
pending resource requests; a fourth is refused (`refused_approvals`). The console approves a request for the task (that one
commission); a wider envelope for the project is a commit to its file.

**Registry first.** The rows (table `cloud_resources`, project `agent`,
owner the task, the session, the repository, the type or size, the
purpose, the reservation `hold_id` that groups the commission) exist
before the firewall, the server and the volume are created, in that
order. Every resource carries the labels `factory=agent`, `task=<id>`,
`project=<owner--name>` and `resource=<row id>`. A failed create
releases what was made; a create whose answer was lost is found by its
label or deleted as unknown.

**Leases and the collector.** The gateway runs the collector on the
agent project every 2 minutes: it deletes resources whose lease ends
before its next pass (within the interval plus 90 s, ruling 124, so a
lease of N started hours never lives into hour N+1), unless standing and not released, or
whose task is `done` or `failed`, each with its whole commission through
the retrying release (server, volume, firewall: the volume waits, 3 s at a
time up to 10 tries, for the server's delete to detach it; a detach the
deleting server refuses, `locked` or a 5xx, is retried), so both
billed deletes are issued within the pass; a pass that takes longer than
the 90 s is logged. It deletes every
server, firewall and volume the registry does not know as open and every
primary IP no server holds, and closes rows whose resource is gone; the
interlock of ruling 123 (see the untrusted runner below) guards every
pass, with the cluster nodes from `AGENT_SSH_SOURCES`. Then it accrues: each resource's started hours not yet booked go to a
`resource` ledger row (`ref` `resource/<id>`) and the task's spend, and
come off the reservation, under the headroom lock. A booking beyond the
resource's own share of the reservation (its reserved hours at its price,
as far as the reservation still holds it: a resource that outlived its
lease) is booked in full and flagged: `COST OVER HOLD` in the log and an
`accrued` audit row carrying it in `over_hold` (rulings 117, 124). Once a commission's
rows are closed and fully booked, the rest of the reservation returns.
factoryd's hold sweep skips resource reservations: accrual books and
drops them. A create whose answer was lost is booked too: a release that
finds it by its label records its id first, and a collector that deletes
it as unknown gives its closed row the id. Two kinds (ruling 147,
`cloud_resources.lifetime`): a task's commission is `lease`; promotion in the
console makes it `standing` (`promoted_by`): project-owned, reservation and
lease dropped, out of the task's egress, and no `ssh_certificate` for it
(agents get no SSH to standing servers). Promotion must fit the envelope's
standing block (standing server types, count, monthly EUR over the open
unreleased standing rows' catalogue prices), else 409 and a human raises the
block first. A task can never ask for `lifetime: standing` (refused as
arguments) nor release a standing resource. A console release (`released_by`) ends the lease now and the
collector deletes it, standing or not; a standing resource counts against the
project's standing monthly line, not the task block's month.

**Reachability.** factoryd reads the live leases from the registry
(same database) every 10 s, reads each task namespace's `worker-egress`
and server-side applies it when it differs from the desired policy: one
rule item per leased server, `toCIDR <ip>/32` and `toPorts 22/TCP`
together. A lease leaves at the collector's cutoff (3.5 minutes before
it ends) or when the server is released, and an edit out of band is
repaired on the next pass. The sandbox image carries
`openssh-client`.

**The SSH CA.** OpenBao's `ssh-client-signer` engine with role
`factory-agent` (user certificates, TTL at most 30 minutes, any
principal: OpenBao 2.6.3's `allowed_users` has no glob). Ruling 125 as
amended: the binding is the server's `AuthorizedPrincipalsFile` (its one
principal `resource-<row id>`) plus the gateway signing only for the
task's own server. The gateway logs in
through Kubernetes auth role `factory-gateway` with a projected token of
audience `openbao` (`/var/run/secrets/openbao/token`), its ServiceAccount
only, and a policy that may only sign with that role; it re-reads the CA
public key hourly. OpenBao is reached over plain HTTP inside the cluster:
Cilium's WireGuard encrypts pod traffic between nodes. The infra README
has the setup commands.

The envelope covers servers with their primary IPv4 and volumes. Buckets
are not offered (ruling 121): Hetzner's Cloud API has no Object Storage
endpoint (the agent token gets 404 on every candidate path), and its S3
keys are made in the console and reach every bucket of a project.

A server's firewall admits SSH only from the cluster nodes and lets it
out only on 53, 80 and 443, as build VMs (ruling 122). Its egress is not
name-restricted: an agent can reach the internet on those ports through
its server, beyond the sandbox's allowlist, for at most the lease.

## Console (task 54)

The console's approvals were first proven live on this task.

`internal/console`, served by factoryd on `:8090` (`-console-addr`) behind
its own Service `factoryd-console` and the HTTPRoute `console` on the edge
gateway's `https-console` listener (https://console.aisoftwarefactory.world);
the webhook and intake API on `:8080` stay in-cluster. Server-rendered
`html/template` with htmx 2.0.11 vendored in `internal/console/static`
(sha256 pinned by a test), no build step. Every EUR amount shows four
decimals (ruling 135): a task's model calls cost fractions of a cent.

- Login: Forgejo OAuth2 authorization code with PKCE (S256), confidential
  client `factory-console` (secret `console-oauth`: `client_id`,
  `client_secret`, `session_key`; redirect `/auth/callback`), always the explicit scopes
  `read:user write:repository write:issue` (an unscoped grant would carry
  the user's full power). Browsers go to `FORGEJO_PUBLIC_URL`, the code
  exchange and API calls to `FORGEJO_URL`. An anonymous `/login` keeps
  nothing on the server: the state and the PKCE verifier travel in the
  `__Host-console-login` cookie, signed with HMAC-SHA256 under
  `session_key` (`CONSOLE_SESSION_KEY`, at least 32 bytes) and valid for
  10 minutes. Only the trust gate's writers
  of the code repository get in, checked at login and on every request
  (list cached a minute, fetched by one request while the others wait, the
  last list served up to 5 minutes while Forgejo fails); others get 403.
  A refused login's fresh tokens are dropped, not revoked: Forgejo has no
  token revocation API, so they lapse within their hour. Sessions are in memory (a
  restart logs everyone out), the tokens never leave the server and are
  refreshed before their hour runs out; the cookie is `__Host-`,
  HttpOnly, Secure, SameSite=Lax. Every POST needs the session's CSRF
  token (form field `csrf` or header `X-CSRF-Token`). Responses carry
  HSTS; the listener has read, write and idle timeouts. Error pages say
  what failed in a fixed sentence, the details go to the log.
- Spend is the policy's `approvers` (ruling 128; the policy file's
  `approvers: [...]`): only they approve a resource request or a merge,
  or promote. Other writers see the cards without those buttons (a POST
  gets 403) and may still deny,
  request changes, reject and release. Forgejo's approvals whitelist on
  main stays the merge authority.
- `/`: the agent lane by task status (done and failed for 7 days, at most
  300 tasks, open ones first), the human lane (approval requests,
  escalations with their reason, read from the escalation's intake event,
  and an answer box, human tasks); htmx polls `/board` every 10 s. A card
  shows the task's time in all (from the issue's `created_at`) and the gate
  it is in with the gate's age, from the row's `gate` and `gate_since`.
- `/tasks/{id}/timeline`: a gates table above the raw steps, one row per
  step that ended a gate (`factoryd.StepGates`, each carrying `started`)
  with its attempt, start, duration, outcome and a `<meter>` against the
  longest gate, the CI jobs of a `checks` step under it, and the row's
  current gate "open for N min" while no step ended it. The approval wait
  ends at the signal that ended it: an approval, a change request, a merge
  in Forgejo or a close.
- `/alerts`: the newest 200 alerts (ruling 140): `gate_bound` (an activity
  still running at its policy bound, written once per run by the
  `GateWatch` worker interceptor; the policy's `bounds` section, seconds
  per activity and `default`; without it RunSession 1260, WaitChecks 600,
  GateReproduction 900, ModelReview 180, Merge 90, SuperviseGate 360,
  every other activity 120), `supervisor_trip` (every detector trip the
  supervisor decided; `continue` is `noise`, any other action `real`) and
  `escalation` (every escalation, not a healed run's repeat of it; open until an approver clicks `real
  problem` or `noise`, here or on the escalation's board card). A
  supervisor column shows the action and diagnosis of a machine alert;
  the timeline shows them on its `supervisor` and `gate_supervisor`
  steps. An escalation's detail stays off the page.
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
  human, which fails the task. A merge Forgejo refuses (or that fails and
  is retried) notes why on the pending request, and the card shows it.
- `/resources`: the agent project's commissions by project and task (open,
  or closed in the last 7 days) with type, IPv4, lease end, EUR/h, cost
  booked so far and state; promote and release per commission (a released
  commission, or one whose lease has ended, cannot be promoted: 409; the
  promote note gives the commission's catalogue monthly price, as the
  approval cards do, against the standing block of the project's file);
  each project's envelope, read-only, with both blocks and what the month
  and the standing servers use, linked to the project's file where it is
  changed (a repository no file names shows the policy's defaults).
- `/projects`: see Projects below.
- Every project's reconcile (boot included) creates the labels intake
  reads (`factoryd.BootLabels`): `agent`, `human`, `bug`, `improvement`,
  `state/*`, `priority/p0..p3` and `severity/critical|high|medium|low`.
  The new-task form picks a project among those agents work on (`agents:
  coder`, the first by name the default) and offers the priorities its
  repository has; the issue is filed there as the human.
- Login: the writers of the factory's own project, the one whose file lists
  factoryd's namespace (`Forgejo.Writers`).
- `/costs`: the ledger per day and kind and per task for 14 days, each
  task with its lines per kind and ref (a model review is its own line,
  `model review/<alias>`), today against the day cap. `/policy`: the version in force, linked to its file
  in the infra repository for edits.

## Projects (task 65, ruling 179)

A project is a repository the factory works on, the namespaces it deploys
into, and its settings (ruling 146). The settings are its file
`projects/<name>.yaml` in the configuration tree (ruling 183): `repository`,
`merge_rule` auto|human, `objects` factory|seed, `agents` coder|none,
`notify` (empty: the policy's approvers), `namespaces` (each one project's),
`environments` (namespace, path, hostname), `quota`, `secrets`, `envelope`,
`standing`, and for `objects: seed` the `tokens` it is read with. Absent
blocks take the policy's `project_default` (quota, the standing block;
built in: 2 CPU / 4Gi / 10Gi, a closed standing block) and
`envelope_default`. Table `projects` keeps only what registration minted
(`name`, the tokens and hashes below, timestamps): every reconcile gives each
file its row, and a row whose file is gone keeps its tokens while nothing
acts on it. The console shows the projects read-only with links to their
files; a change is a commit.

The factory's own repositories are projects like any other (ruling 179): no
Go source names them (a test greps). Two settings replace the old special
cases:

- `objects`: `factory` makes the project its own Forgejo organisation whose
  objects factoryd creates and reconciles (below); `seed` leaves them to the
  infra repository and the seed. For a seed project factoryd creates nothing
  that needs `factory-projects` and nothing in the cluster: it keeps the boot
  labels and (agents coder) the bot's fork as the bot, and reports the
  webhook to factoryd (read with the project's read token: `present`, or
  `missing: the seed makes it`, or `not checkable, no admin credential on
  <repo>` when that token's user does not administer it, as on the infra
  repository; no error) and each namespace's `factory/project`
  label (`present`, `wrong`, `missing`). The fork is made for `agents:
  coder` only: a project no agent works on needs no branch to push to. A
  seed file names no environments, quota or secrets.
- `agents`: `coder` lets agent tasks run; `none` sends every issue, and
  every alert of its namespaces, to the human lane whatever its labels, and
  `POST /tasks` on it answers 422 with the reason, as it does for a
  repository no active project names.

Which of factoryd's tokens reaches a project for what is one mapping,
`Forgejo.Access`: a factory project's `merge_token` reads its writers and
merges, the projects token lists its Actions jobs and registers its runners;
a seed project's file names seeded secrets (`tokens.read`, `.actions`,
`.merge`; the values `forgejo-ci-registrar`, `forgejo-merge`,
`forgejo-config`), which factoryd holds as `FORGEJO_CI_REGISTRAR_TOKEN`,
`FORGEJO_MERGE_TOKEN` and `FORGEJO_CONFIG_TOKEN`. `forgejo-projects` is no
allowed value: a seed project never gets the projects token (ruling 164).
The code repository names `forgejo-ci-registrar` (read, Actions) and
`forgejo-merge`; the infra repository `forgejo-config` (read: the trust gate
and the heal pass's close of a human-lane task; the org-read token adds the
teams). The factory's own project is the one whose file lists factoryd's
namespace: its CI runs and rolls are watched (with its Actions token), its
writers log into the console. factoryd does not boot without it: a configuration
whose project files list no project in factoryd's namespace stops the boot
with `no project file lists factoryd's namespace factory`.

**A project is its own Forgejo organisation** (ruling 164): the organisation
`<name>`, owned by `factory-projects`, with the one repository
`<name>/<name>` (the row keeps that lower-case form; Forgejo folds case), its
packages, and the team `ci` (package write, no repository) whose one member
is the user `ci-<name>`. Nothing of org `sf` is reachable from it:
`factory-projects` is no member of `sf` and holds one token,
`write:repository` without a repository list (`FORGEJO_PROJECTS_TOKEN`,
secret `factory/forgejo-projects`); Forgejo lets no repository-limited token
pass its admin checks, and as the owner of exactly the project organisations
this token reaches them and nothing else. It cannot create or delete an
organisation or a repository (that needs `write:organization`, which it
lacks), and cannot touch teams. It can transfer a project repository
(Forgejo files transfer under the repository scope): accepted under ruling
164, since the damage stays inside the projects' bound and the next reconcile
reports the repository as waiting. Registration adopts only the CI user and
team it made itself (the user's address `ci-<name>@factory.local`, the team
package write and nothing else on no repository); any other `ci-<name>` or
`ci` makes it stop with the reason.

`factoryd register-project --name <name>` (a human, with the Forgejo site
admin's login on stdin; the project's file says `objects: factory`) gives
the project its row and does what only the admin can: the organisation, the repository
(private, `main`), the user `ci-<name>` (random password, discarded) in the
team, and the tokens, each minted with `?sudo=`: `ci-<name>`'s
`write:package` (`registry`) straight into the Actions secrets
`REGISTRY_USER`/`REGISTRY_PASSWORD` (the project pushes its own packages and
no one else's), `ci-<name>`'s `read:package` (`pull`, row `pull_token`, the
namespaces' image pull secret), `factory-projects`' `read:repository` limited
to the repository (`git-<name>`, row `git_token`, Flux's source) and
`factory-merge`'s `write:repository` limited to it (`project-<name>`, row
`merge_token`, the Merge activity's on this project). An existing token is
kept unless `--rotate`, which revokes and mints them all again. These tokens
are plaintext in Postgres and so in its backups: the namespaces and Flux need
them back after a rebuild; each reaches one project only.

factoryd reconciles every project at boot and at every new configuration
version (a file changed, the approvers), one log line per object (`project
<name>: <object>: created|updated|no change|waiting: ...`); for a factory
project:

- Forgejo, with the projects token: the webhook to factoryd (every issue and
  pull request event and pushes, JSON, the intake secret; the row keeps the secret's
  sha256, so a re-seeded secret reaches the hook), `factory-agent`,
  `factory-merge` and the policy's approvers as write collaborators (Forgejo
  keeps only writers in a whitelist), `main`'s protection: the code
  repository's (push whitelist the approvers and `factory-merge`, merge
  whitelist the same, approvals whitelist the approvers, apply to admins,
  stale approvals dismissed, rejected reviews block), `required_approvals` 0
  on `auto` (1 on `human`), no deploy key and no protected file pattern, so
  humans change workflows on main like any file (ruling 166); the branch
  `deploy`, the project's rolled state and what Flux reads, made from `main`
  once main has a commit (the repository starts empty; until then the line
  says `waiting: main has no commit yet`; a push to main while `deploy` is
  missing runs the project's reconcile from intake, so the first push makes it) and then protected: only the deploy key may push, no
  approvals, apply to admins, and the protected file patterns
  `.forgejo/**;.gitea/**;.github/**`, since every non-fork job holds
  `DEPLOY_KEY`: `deploy` keeps the workflows it was made with, and a roll
  commit that merges main into it must leave them alone (Forgejo refuses to
  create a branch its protection covers, so the protection follows the
  branch); a write deploy key `factory-roll` whose private half lives only in
  the Actions secret `DEPLOY_KEY` (the project's main workflow pushes its roll
  commits with it; Forgejo admits no Actions job token to a whitelist; a key
  or secret gone missing makes both anew); `FACTORY_READ_TOKEN` (random, its
  sha256 in `read_token_hash`; of two reconciles racing only the one that
  stores its hash sets the secret); `FACTORY_SSH_HOST_KEY`, Forgejo's
  SSH host key, ed25519 when offered (the built-in server offers RSA only
  unless configured otherwise), type and key as factoryd's own handshake with
  `forgejo-ssh.forgejo:22` finds it at every reconcile, its sha256 in
  `host_key_hash`, so a rebuild that makes new host keys updates every project
  and no workflow pins one (ruling 165; factoryd's egress admits Forgejo's
  2222). As the bot: its private fork
  `factory-agent/<name>` and the boot labels. A project whose file exists
  waits for `register-project` here.
- The cluster, per namespace: the namespace with `factory/project=<name>` and
  pod security `restricted`; RoleBinding `factoryd` (ClusterRole
  `factoryd-project`); the CiliumNetworkPolicy `project`, a default deny that
  admits in the namespace itself, the edge gateway's Envoy and the CNPG
  operator (8000, 5432), and out the namespace itself, DNS, the API server
  (CNPG's instance manager), the SigNoz collector on 4318 and anything on
  443; ResourceQuota `project` (CPU and memory bound requests and limits
  alike, storage the claims' requests); LimitRange `project` (defaults
  50m/64Mi requested, 500m/512Mi limit); ServiceAccount `flux` bound to
  ClusterRole `factory-project-deployer` (workload kinds, HTTPRoutes, CNPG
  Clusters; no RBAC, no Flux kind, no network policy, nothing
  cluster-scoped); the image pull secret `forgejo-registry` (`ci-<name>`,
  `pull_token`); each `secrets` entry's secret with a random value when
  missing, never replaced. In `flux-system`: the Secret `<name>-git`
  (`factory-projects` and `git_token`) and the GitRepository `<name>` (the
  repository's `deploy`), and per environment a Kustomization `<name>` in the
  environment's namespace (`serviceAccountName: flux`, `targetNamespace` that
  namespace, `prune`): Flux takes the account from the Kustomization's own
  namespace, and the deployer role cannot change it (ruling 157).
- The admission policy confines all of it: only labelled namespaces (a
  namespace without the label never gets it, one never moves to another
  project), these kinds, these bindings, the one network policy `project`, no
  service account token Secret anywhere, Kustomizations of the namespace's own
  project, GitRepositories named after their project at exactly
  `https://git.aisoftwarefactory.world/<name>/<name>` with the secret
  `<name>-git`, no `include` and no proxy, and that secret alone in
  `flux-system`.

```
kubectl -n factory exec -i deploy/factoryd -- factoryd register-project --name demo-app
```

with the admin's password on stdin, once `projects/demo-app.yaml` is on the
infra repository's main and loaded, makes the above (a second run changes
nothing), reconciles, and prints the edge gateway listeners and Certificates
for the infra commit (ruling 155). The console's `/projects` lists every
project read-only with its file's settings, the envelope with its use and
the standing monthly line, and a link to the file.

`GET /projects/{name}/alerts?namespace=<ns>` on factoryd's port 8080
(`http://factoryd.factory.svc.cluster.local:8080`): bearer the project's
`FACTORY_READ_TOKEN` (compared by hash in constant time; 401 otherwise, an
unknown project too), the namespace one of the project's (403 otherwise).
It answers a JSON array: SigNoz's firing alerts whose `k8s.namespace.name`,
`k8s_namespace_name` or `namespace` label is the namespace (`GET
/api/v1/alerts` on SigNoz v0.143.0, logged in as a viewer-role user, secret
`factory/signoz-factoryd`), plus one `rollout_not_ready` per Deployment whose
rollout has not finished (kubectl's test: generation observed, every replica
updated, no old one left, every updated one available); `[]` when none.
Cached 30 s per project and namespace; a source it cannot read answers 502,
never `[]`.

`POST /webhooks/signoz` on 8080 is SigNoz's webhook channel `factoryd`
(written by the infra's `signoz-objects` job, basic auth user `signoz` with
the password of secret `signoz-webhook`, env `SIGNOZ_WEBHOOK_PASSWORD`;
401 otherwise). Each alert's `namespace` label, which the job sets on
every project rule, names the project through the namespace's
`factory/project` label (read once per namespace); an active project that
lists the namespace, or the alert is logged and dropped. On a project on
`agents: none` the issue is the human lane's (labels `bug`, `human`), and no
task runs. The factory's own namespaces route the same way (ruling 180):
`factory` names the code repository's project, `platform` and `forgejo` the
infra repository's (agents none); the infra repository labels them, with
`factory/objects=seed` besides, which keeps factoryd out of them in the
admission policy. By
the alert's fingerprint: its first firing files an issue on the project's
repository as the bot, title `alert: <name> in <namespace>` (`alert: <name>
on <service> in <namespace>` when the alert carries a `service`,
`service.name`, `k8s.deployment.name` or `deployment` label), the body the
firing time, the SigNoz link, the value and annotations and the labels
(fenced, one line each), labels `bug` and `state/queued`, and starts its
agent task at the default branch's head (source `alert`, standing
`telemetry`: the brief marks the text as untrusted; no reproduction: the
agent brings its own); a firing still resent 30 minutes after its task
ended is filed once more; a later firing comments `fired again at <time>`
while that task runs and files a new issue once it ended; a resend of the
same firing does nothing; a resolve comments `resolved at <time>` once.
Each issue is an alerts row of kind `alert` with the fingerprint, judged
when its task ends: `real` on a merge, `noise` when a human closed the
issue without one. A human-lane alert issue is judged by its close: `noise`
when it carries the label `noise`, `real` otherwise (the closer is
`resolved_by`). A notification that fails in part answers 500, and
SigNoz sends it again.

The factory's own rules are `k8s/alerts/*.json`, ConfigMap `signoz-rules`
in namespace `factory`: 5xx rate above 5 % per service (factoryd, its
console and the gateway trace every request but the probes), factoryd and
the gateway each started 3 times in 10 minutes (the kubelet's `Started`
events: a crash restart and a replaced pod both count), the CI queue's p95
above 5 minutes (`factory.ci.job.queue.bucket`: SigNoz stores a histogram
under its bucket series), and factoryd's or the gateway's roll not live for
10 minutes (a ReplicaSet with fewer available replicas than desired, from
the cluster collector: factoryd's Recreate leaves no old pod to report its
own stuck roll, and the gateway's rolling update keeps the Deployment's
counts whole). Both Deployments set `OTEL_RESOURCE_ATTRIBUTES=
k8s.namespace.name=factory`, the namespace the rules are bound to; server
spans are named by route pattern (`GET /tasks/{id}`), never by path.

The CI spawner serves every active project on `agents: coder`, each with
its Actions token (Forgejo keeps the runner API to the repository's owner):
pull requests, and a factory project's own main workflow too, which never
touches `sf`'s privileged org runner. A factory project's runs get two more
callees: Forgejo's SSH
(`forgejo-ssh.forgejo`, the pod's 2222) for the deploy-key push and factoryd
on 8080 for the alerts endpoint; their namespace carries
`factory.ai/ci-project=<name>`, which factoryd's ingress admits. The
registration secret in a CI namespace names the repository its runner is
registered in (none: the code repository's).

A standing resource's hours go to the ledger as kind `standing` under
`standing:<repository>`: its project's monthly line, no task's spend and no
day's headroom.

## Build and test

    go build ./...
    go test -race ./...

The store tests run only with `FACTORYD_TEST_DATABASE_URL` and
`GATEWAY_TEST_DATABASE_URL` set to a throwaway Postgres (they wipe it).
Each package behind `FACTORYD_TEST_DATABASE_URL` migrates its own database
`factory_test_<package>`, dropped and created empty at its first test
(`internal/schema/schematest`), so the packages run in parallel; that
URL's user must be allowed to create databases. The
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
  its `if`) gets a throwaway server of type `CI_BUILD_VM_TYPE` (default
  `cx23`; debian-12,
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
  `BUILDKIT_TLS_DIR` and `BUILDKIT_TLS_SERVERNAME`. Before the runner
  starts, the pod's init container `buildkitd-ready` (same image, same
  mount and egress) waits up to three minutes for `buildctl debug
  workers` and, once it answers, writes `ready` as its termination
   message. A job's first VM that never answers fails the pod there,
   before the runner takes the job, and the spawner replaces the VM at
   once; on that second VM (`BUILDKITD_LAST_TRY`) the init container exits
   0 either way, so a VM that never answers fails the job loudly in
   `image-build`'s own wait (one more minute), not in a loop of fresh VMs.
   `image-build` then builds the changed
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
  that lists a server or a primary IP on one of the cluster nodes'
  IPv4s (the cluster's own addresses sit unassigned during a rebuild), or
  a server, firewall, volume or primary IP labelled `factory=` another
  project, deletes nothing and logs
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

## Telemetry

Every binary sends OpenTelemetry metrics, traces and logs over OTLP HTTP
to `OTEL_EXPORTER_OTLP_ENDPOINT` (SigNoz's collector,
`signoz-otel-collector.platform:4318`), stamped with its own
`service.name`: `factoryd`, `factory-gateway`, `agent-worker`. Unset, all
three providers are no-ops. Every export is fire and forget: no retry, a
failed export drops its data and logs at most once a minute. Metrics go
out every 30 s, spans and log records in batches. Every span and log
record that belongs to a task carries `task.id`, so one search for a task
id in SigNoz shows its gateway calls, audit rows, sessions and factoryd
activities. The traces are separate: a session is its own trace, and no
binary accepts trace context from another. Durations are in `s`, money
in `EUR`.

### factoryd

Spans: one per Temporal activity (`ActivitySpans`), named after it
(`RunSession`, `WaitChecks`, ...), with `temporal.workflow_id`,
`temporal.attempt` and, for a task's workflow `task-<id>`, `task.id`; a
failed activity ends in error, described by its name only (`RunSession
failed`).

Metrics, the duration buckets 5, 10, 30, 60, 120, 300, 600, 900, 1200,
1800, 2700 and 3600:

- `factory.ci.job.duration{job,event,status}`: a 60 s poll of the code
  repository's `actions/tasks` (the registrar token), once per finished
  job, its `updated_at` minus `run_started_at`.
- `factory.ci.run.duration{event,status}`: the same poll of
  `actions/runs`, once per finished run, `stopped` minus `created`: from
  the push (or pull request event) to the last job's finish, queue time
  included; Forgejo stops a failed run at its first failing job, so a
  failed run's duration is the time to the first red job, not the wall
  time of all its jobs. What finishes while factoryd is down is lost.
- `factory.ci.job.queue{label,job}`: the CI spawner's 10 s poll of
  `actions/runners/jobs` for `pr-untrusted`, `pr-untrusted-build` and main
  CI's `ubuntu-latest`, from the first poll that sees a job waiting to the
  first that sees it taken. The list holds waiting and running jobs only,
  with no creation time, so a job a runner takes before a poll sees it
  waiting (the near-zero queues, mostly on the persistent trusted runner)
  is not measured: it counts in `factory.ci.job.queue.unmeasured{label,job}`,
  and the histogram's low quantiles read high by that share. A job that
  is taken and finishes between two polls is never listed, so it is not
  counted either. The `checks`
  timeline step lists the head's jobs under `jobs`: `name`, `status`,
  `run_s` and, where measured, `queue_s`.
- `factory.ci.buildvm.lifetime`, `.ready` (create to buildkitd's first
  answer: a build runner pod's init container `buildkitd-ready` polls
  `buildctl debug workers` against its own VM, at most 3 minutes, before
  the job starts; the spawner reads its `finishedAt` before the teardown,
  only when its termination message is `ready`)
  and
  `.cost` (started hours times the row's hourly price), each
  `{server_type,deleted_by}` with `deleted_by` `teardown` or `collector`,
  at the moment a build VM's row closes.
- `factory.roll.duration{deployment}`, `.apply` and `.ready`: from boot on,
  every 15 s, factoryd reads the newest `main` commit that touched `k8s/`
  (Forgejo's history simplification answers a merged pull request's head,
  so the head of a merged pull request stands for its merge commit,
  compared with the merge's first parent; the newest roll task's pull
  request is read by its number, any other among the 50 closed pull
  requests updated last); for every roll target (factoryd, gateway, worker, ci-runner) whose pinned
  reference it changed against its parent, it waits until the target's
  Deployment carries the new reference (an image, or an env value for the
  worker and the runner, which roll factoryd) and is available on it
  (`NewReplicaSetAvailable` after the `kustomize-controller` apply),
  records one `rolls` row per target (`sha`, `image`, `deployment`, push,
  apply and ready times), emits the three histograms once per Deployment
  and logs `roll <sha> <deployment> live (<targets>): push to apply <n> s,
  apply to ready <n> s`. The push is the `created` time of the commit's
  push run in `actions/runs`; without one nothing is recorded. A commit
  that changed no reference records nothing.
- `factory.task.gate.duration{gate,outcome,model_alias}` from every
  timeline step that ends a gate (`factoryd.StepGates`) and carries
  `started`, labelled with the gate's name (session, checks, policy,
  migration, reproduction, review, approval, merge, commissioning,
  intake); commissioning ends at the step's `live`.
- `factory.activity.open_age{activity}`: the age of the oldest running
  activity per name.
- `factory.alert.count{kind,verdict}`: the `alerts` table at each export,
  `open` for no verdict yet.

Task analytics (ruling 185), counted where they happen and exported as
deltas (`Providers.Analytics`): SigNoz sums a delta series, so a series'
first count and the counts before a restart are in a day's bar, which an
increase over a cumulative series drops. A panel sums them over the
factoryd pods:

- `factory.task.outcome{project,state,model_alias,attempts,merge_rule}`:
  once per task on every path that ends it (`Complete`, which `Finish`
  and the heal pass's failure of a malformed task go through, a closed
  backlog card, a closed human-lane issue, a resolved config task), so it
  equals the tasks turned done, failed or closed. `state` is that final
  status. `project`
  and `merge_rule` come from the repository's project file (the repository
  and `human` without one), `model_alias` is the alias of the task's last
  session (`tasks.model_alias`, set with each `sessions` row; `none` for
  a task that ran none: a roll, a human-lane task), `attempts` its sessions.
- `factory.task.cost{model_alias,outcome}`: the task's `spent_eur` at
  `Finish`, in EUR, buckets 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5 and
  10; `outcome` is `merged` for a done task with a pull request, else the
  final state. Spend settled after `Finish` (LiteLLM's late batches) is
  not in it.
- `factory.review.verdict{verdict,strict_improvement,round}`: every
  model review answer, `verdict` approve, concerns or `none` (refused,
  failed or invalid), `strict_improvement` true, false or `unset`, `round`
  the review's round on the task. A roll's review asks no model and is
  not counted.
- `factory.human.intervention{kind}`: from the Forgejo intake when its
  event row is new (a redelivery or the heal pass's re-read counts
  nothing), so the console's actions (it acts as the human in Forgejo) and
  the same actions taken in Forgejo count alike: `approve` (a writer's
  approving review of a factory pull request), `request_changes` (a
  writer's review asking for changes), `reject` (a human closing a factory
  pull request without a merge), `escalation_answer` (a writer's new
  comment on a task waiting for a human, or waiting for its poster),
  `poster_answer` (the poster's new comment on a task waiting for the
  poster); `alert_verdict_click` from the console, and `extra_time` when
  a human's answer raised the task's agent time. Uncounted: a human merging
  a factory pull request in Forgejo directly, resource approvals.
- `factory.task.reverted{project}`: the daily revert pass (at boot, then
  every 24 hours) sets `tasks.reverted_at` on a done agent task (a pull
  request, not a roll) finished in the last 14 days when either a pull
  request merged into `main` of the same repository after the task
  finished, and within 14 days of it, undoes every file the task's pull
  request changed (for each such file its net change, per line text the
  times added less the times removed, is the exact opposite of the task's,
  so context, positions and how git aligned a moved block do not count;
  more files besides are allowed, a file left alone or changed
  otherwise is no revert, a quoted path is keyed by its whole header line,
  a diff over 1 MiB is never judged), or the
  task's alert is filed again (the SigNoz intake's refile) after the task
  finished and within 14 days. Each mark counts once.

### factory-gateway

Spans: a server span per request (`otelhttp`, `/healthz` not traced),
named `POST /v1/chat/completions` or `POST /mcp`, with `task.id` once the
token is verified. Under it one client span per upstream call, `model
<alias>` for an admitted model call and `tool <tool>` for a paid tool
call (web_scrape, web_search, replicate_predict, fal_run), with
`task.id`, `project` (the task's repository), `kind` (`model` or
`tool`), `name` (the alias or the tool), `upstream.status`, `hold_eur`,
`cost_eur` (what the booking added to the task's spend: the reported
cost, else the hold) and `over_hold` (bool). `upstream.status` is the
HTTP status for a model call and `error` when LiteLLM did not answer; for
a tool `ok`, the provider's status when it refused or failed, or
`error`. Anything but 2xx or `ok` ends the span in error. The free tools
and commissioning have no upstream span; their audit rows are logged.

Metrics:

- `gateway.calls{kind,name,status}`: one per upstream call, `status` as
  `upstream.status`.
- `gateway.call.duration{kind,name}`: span start to the booking, buckets
  0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120 and 300.
- `gateway.spend.eur{project}`: what each model and tool booking added
  to the task's `spent_eur` (the ledger's amount, `policy.Booking.Booked`).
  The hold sweep and commissioned-resource accruals book outside a call
  and are not in it.
- `gateway.refusals{reason}`: every audit row whose decision starts with
  `refused_`, the decision as `reason`.

Audit log records: every audit row the store accepted is also one log
record (`Telemetry.Audited` wraps the store; Postgres stays the record),
body `audit <decision>`, severity INFO, attributes `task.id`,
`audit.decision`, `audit.kind` (`agent`, factoryd's purpose, or `factoryd`), `audit.reason`, `audit.model_alias`, `audit.tool`,
`audit.spend_eur`, `audit.policy_version`, `audit.output_limit`,
`audit.hold_eur`, `audit.over_hold`; a nil or empty column is left out.
`spend_eur` on an audit row is the task's running total after the call (the
per-call cost is the difference to the previous row of the same task); swept
and accrued rows carry the event's own amount.
`task.id` and `audit.decision` are the gateway's own (the verified
token's task, a constant) and go out as they are. Every other string goes
through the scrubber first (the upstream-body rule is the gateway's, the rest `internal/redact`): a PEM block becomes `[pem redacted]`; the
gateway's own keys (`LITELLM_MASTER_KEY`, the tool keys, `FACTORYD_TOKEN`,
`HETZNER_AGENT_KEY`), URL userinfo, bearer and basic credentials, JWTs,
`sk-` keys and any run of 32 or more key characters become `[redacted]`
(which also catches long paths and digests; Postgres keeps them); and the
body an upstream error carries (`provider answered <status>: ...`, `key
generate|block: status <status>: ...`, `unreadable prediction: ...`)
becomes `[body dropped]`, keeping the gateway's own `; cost not reported,
booked the hold ... EUR` after it. No request or response body is logged. An
escalation's row carries the agent's question (its first 500 bytes) as
the reason.

Postgres is the authoritative audit record. The SigNoz copy is not proof:
ruling 158 lets sandbox code send OTLP to the same collector, so an agent
can forge a record with `service.name=factory-gateway`, an `audit`
body and any `task.id`.

On SIGTERM the gateway stops accepting, lets in-flight calls finish and
book (at most 10 s), then flushes its telemetry (at most 5 s), inside the
pod's 30 s grace period.

### agent-worker

One trace per session: the root span `session` (`task.id`, `session.id`,
`model_alias`, `outcome`), its children `brief` (restoring the opencode
config, the clone, the brief and the checkpoint store), `run` (the
harness) and `push` (the commit and push, when the session finished or
was stopped), and under `run` one span per tool call from the harness
event stream: named after the tool (`bash` for a command), with
`task.id`, `tool.name`, `tool.status` (`ok` or `error`; a command is ok
on exit 0), `tool.exit_code` for a command and `tool.bytes` (a command's
output length, a file change's size after it; not bytes written), timed
from the tool's own `state.time`. Arguments and outputs never reach a
span, and a failed phase's status says only `<phase> failed`. The worker
ends the session span and flushes all three signals (at most 5 s) before
it prints its result line, because factoryd deletes the pod on reading it.

Metrics:

- `worker.session.duration{outcome,model_alias}`: worker start to the
  result, `outcome` `pushed`, `no_changes`, `stopped` or `failed`,
  factoryd's buckets.
- `worker.tool.calls{tool,status}`.
- `worker.session.tokens{model_alias,direction}`: the tokens each model
  step reports, `direction` `input`, `output` or `reasoning`.

`model_alias` is the part of `MODEL` after the provider
(`coding-cheap`), the gateway's alias.

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
`api.hetzner.cloud` on 443 (build VMs), SigNoz's collector
(`signoz-otel-collector.platform`) on 4318, and SigNoz itself
(`signoz.platform`) on 8080 (the projects' alerts endpoint). Every
callee is named in full and the pod runs with `ndots: 1`, so the first
DNS query is the admitted one. A new callee needs a rule here first.
Ingress (`factoryd-ingress`): the node on 8080 (probes), Forgejo's
webhooks and the gateway's escalation hand-over on 8080, a project's CI
runner pods (namespace label `factory.ai/ci-project`) on 8080 for the alerts
endpoint, SigNoz (`signoz-0` in `platform`) on 8080 for its webhook, and the edge gateway's Envoy (entity `ingress`) on the console
port 8090 only.

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
`WORKER_IMAGE` and `CI_RUNNER_IMAGE`), and factoryd opens it as a task of
source `roll` (ruling 178). Every minute it reads the code repository's
push runs; for the green runs of `main` it has not judged, newest first
until one files or folds the roll (at its first judgement the newest
only, and never one older than a run whose commit a roll carries, which
would roll `main` back), it skips a commit that changes nothing outside
`k8s/` against its first parent (a roll's own merge, a hand roll: rebuilt images, whatever
the build cache made of them), and otherwise reads the images main CI tagged with the commit's short sha
from the registry (in-cluster Forgejo, `/v2`, as user `factory-registry-read`
with its `read:package` token from secret `forgejo-registry-read`, which
cannot overwrite a tag, read once at boot, so a rotation restarts
factoryd; the `forgejo-registry` pull secret serves image pulls only) and compares each with the one `k8s/` pins by the
linux/amd64 image manifest inside the index: main CI's indexes carry a
provenance attestation with build timestamps, so a rebuild of unchanged
inputs pushes a new index digest around the same image. No changed image:
a `roll_requests` row `skipped`. Otherwise it files the task: the issue
`roll <sha7>: <targets>` naming the new references, the commits since the
last roll (merges and roll commits left out) and the pull requests merged
among them (the roll tasks' own left out), and a row `opened`. The workflow runs no session and makes no model call:
`RollCommit` syncs the bot's fork, writes one commit on `factory/<task>`
that bumps the changed digests (the index digest main CI published),
records the commit it wrote and sets the task's base to the main head the
branch was cut from, and the head goes through the pull request, the checks (no image build: no
Dockerfile input changed), the policy gate (`k8s/**` is high) and the
card. The approver's click, `factory-merge` merges, Flux applies, and the
roll watcher records the rows. One open roll per repository: newer images
while it is open (and its pull request exists) are folded in as a second
commit on the same pull request, with a comment and a `roll_folded`
signal that wakes the approval wait (never a finding) and has the
workflow commit and run the stages again; images that arrive after its
merge open the next roll. Every agent task's token writes the bot's fork,
the roll's branch included, so `RollCommit` refuses a branch whose head
is not the commit it wrote, for a fold and for a resumed run; a run that
died between its write and its record adopts the head only when it is
exactly that write (its parent the recorded head or the cut base, only
the roll's files changed, each as the requests make it). Findings on
a head `RollCommit` leaves as it is (red checks, a human's change
request) fail the roll: no agent answers them. A gate trip on a roll goes
to a human without the gate supervisor's model call. A failed roll is
not retried: any new green commit on `main`, a no-op commit included,
opens the next roll, which compares with `main`'s pins and so carries
everything the failed one had. The infra
repository's rolls stay human commits. A hand roll still works and is
pushed to both remotes:

    git push origin main && git push github main

`kubectl rollout restart deployment/factoryd` restarts it twice: Flux
reverts the `restartedAt` annotation about five minutes later and rolls
it again. Both restarts are safe (Temporal retries the interrupted
activity), but plan for the second one, or suspend the `factory`
Kustomization first.

Order when a change spans them: the infra policy first (new keys before
the factoryd that reads them), then the gateway, then factoryd once the
gateway is Ready. factoryd's model calls go to the gateway, so a new
factoryd against an old gateway gets `refused_auth` on them (spurious
escalations and review notes, nothing merged); a new gateway serves an
old factoryd unchanged. The gateway reads the policy version at most
30 s old, so a call in the first 30 s after a new version can still be
judged under the previous one.
