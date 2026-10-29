# Gates, review, approval and merge (task 55, ruling 109)

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
   After the harness copy, its fetch init container (`MODE=repro-fetch`),
   the token's only holder, on the factory's image, fetches `refs/pull/<n>/head` into the
   workspace and checks it is the
   gated head; the worker container (`MODE=repro`), with no token in its
   environment, probes the sandbox's egress as a session's worker does
   (a sandbox that cannot start is the start escalation, and the answer
   runs the gate again), then runs the command there with `sh -c` for at most
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
   Then `AutoMergeNote` records the auto merge on the timeline, and
   `Merge` first approves the head as the merge gate's user `factory-gate`
   (ruling R59: an APPROVED review whose body lists every condition
   `AutoMerge` found met, in factoryd's words alone, never a command, note
   or summary an issue, an agent or a model wrote (ruling R61), with the
   project's gate token), then merges as factory-merge with no card. A
   project whose merge rule is no longer `auto` when Merge runs, a missing
   gate token or a refused approval is a refusal like Forgejo's, and the
   first two post no review. Anything else,
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
signal-with-start like the webhook path. An approval the project agent
decides (in a wake, or unconfirmed in a member's turn at full auto) is the
factory's own, whoever is online: the console posts no review and meets
no human criterion, and every approval request, plan answer and signal
it decides names `factory-chat`. `Merge` treats approver `factory-chat` as the merge gate's own
decision: on the merge rule `auto` it approves the head as `factory-gate`
with `factoryd: the project agent approved <head> on the project's merge
rule auto`; on any other rule, or without a gate token, it refuses as
above. `Merge` then merges as the
dedicated user `factory-merge` (`FORGEJO_MERGE_TOKEN`, secret
`factory/forgejo-merge`, on the factory's own repositories; a registered
project's own `projects.merge_token`, which also reads the project's
writers for the trust gate) with `head_commit_id` the gated head, and the
task finishes done. main's protection lets factory-merge merge but not
push, and only after an approval by a user on the approvals whitelist, so
Forgejo itself refuses the merge without the human or, on the merge rule
`auto`, the merge gate's `factory-gate` (`FORGEJO_GATE_TOKEN`, secret
`factory/forgejo-gate`; a registered project's `projects.gate_token`),
which may approve but neither merge nor push (405; a moved head or a
conflict is 409). The intake ignores factory-gate's reviews as it does
the bot's: they are factoryd's own, no member's decision. The trust gate
never trusts factory-gate, a writer on the repository, so its comments
steer no task (ruling R61). A refusal is commented with what fits and the
wait goes on: a refused auto merge goes to a human, whom the next merge
request asks (approve it in the console, or merge it in Forgejo); any
other approval decided its request, so what fits is a merge by hand in
Forgejo by a human on main's merge whitelist, or the card's close. Merge
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


## The merge queue (merge: {queue: project}, change id `merge-queue`)

A process with `merge: {queue: project}` merges one pull request of a
project at a time. At the approval stage the task enters the project's
queue (`merge_queue`, the project being its repository, oldest entry
first) before the card or the `auto` decision, and holds the turn
through the approval wait and the merge. Behind others, its status stays
`in-review`, its gate is `merge-queue` since it entered (the card reads
`merge queue, 2 ahead, for 6 min`), and the workflow waits for
`queue_turn` or 5 minutes, then asks again. At its turn `SyncWithMain`
merges main into the head branch as the bot, which owns the fork
(`POST /repos/{owner}/{repo}/pulls/{index}/update?style=merge`, proven on
Forgejo 15.0.9 by `TestUpdateBranchAsBotOnFork`), unless the pull
request's merge base is main's head; Forgejo recomputes the merge base in
the background, so a stale one costs an update that moves nothing. A
head that moved (`queue_synced {from, to, main}`) runs the checks stage
again, recorded as factoryd's own move like a re-run's head; policy,
review and audit do not run again. Red checks there go back to the agent
prefixed `after main was merged in:`, a 409 takes backlog 70's conflict
path, and both leave the queue; green, the approval request and the
merge are pinned to the synced head. Every way out of the turn leaves
the queue (`queue_left`) and signals `queue_turn` to the new head, and so
does `Finish`; the heal pass signals every head still in gate
`merge-queue` once a minute (a head at its turn gets no signal, so a long
approval wait adds nothing to its history). Timeline steps:
`queue_entered`, `queue_turn` (which ends the gate `merge-queue` of a
task that waited), `queue_synced`, `queue_left`.

From change id `red-head-restage` on (roadmap 202 with 67), a queue head
whose checks are red at its merge (`merge_red_head`) does not wait for a
human: its approval request is withdrawn (an approval sent meanwhile
finds none open) and it takes its turn again. A red main pauses the
queue and files a fix-forward card. The green run that ends the pause
also ends that card, with a comment naming the run, unless the card's own
merge made main green. A red run whose failed jobs all lost their runner
pauses with no card while factoryd re-runs it once (Forgejo 17's rerun
API); the human card to re-run it comes when the re-run fails, Forgejo
offers none, or the re-run lost its runner too. So that turn comes once main is green again; main is merged in,
and a head that moved runs its checks. With nothing new from main the
head's own checks are waited on and the red ones run again on an empty
commit (`RerunChecks`); red checks that pass there, on the same tree, are
flaky (`checks_flaky {commit, head, checks}`) and never a finding. A
green head goes to the approval again (dismissed stale approvals make it
a new card or the rule `auto`). A queue entry gets at most three such
turns; after them a head still red, or green there though its merge read
red again, leaves the queue, so the cards behind it merge, and waits
outside it for a human: the escalation names the red checks with their
log tails, or the merge's refusal. A reply puts it back at the queue's
end as a new entry with three turns afresh; a change request sends the
task to the agent, whose fix enters the queue anew. No model call is
made on the way. Every escalation comment lists, after its fenced
reason, the reason's links the sanitiser keeps (the public Forgejo host,
the policy's `link_hosts`, the project's environments), since a fence
renders none as a link. From change id `checked-head` on, the approval
stage's green read after a cheap turn reads the head whose checks hold,
not the synced head whose checks just started, and no checked head
outlives its pass.


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
and no write (rulings 141, 161): an agent event, a tool call, command or
model call, arriving `no_write` seconds after the last write (a write,
edit or patch tool call, or a checkpoint whose diff changed: a file
written through the shell) or the session's start (the policy's
`supervisor` section, read at each session start, 300 by default); a
tick or the worker's checkpoint never trips it, silence is the stall's.
The size trip (`size`) trips once per session, when the card's diff
(`card_diff` in the worker's checkpoint: added plus deleted lines against
main) passes the bound its size plans: 30, 60 or 100 percent of the
policy's `risk.large_lines` (300, 600 and 1,000 lines by default) for a
small, medium or large card, and the large bound for a card with no
size. Its check-in
asks one question: will the rest of the card fit its bound, its budget and
its time? Nothing it answers cuts the card. The session's brief states the
bound in its facts line: "This card is planned at most N changed lines
(size S)", or, for a card with no size, "This card has no size, so it is
planned at most N changed lines, the policy's large_lines."
The check-in (ruling 216) trips at every `check_in` of the task's agent
time (`policy.Time`), counted from the task's start, so it continues
across sessions. Its detail is "scheduled check-in, no detector
tripped", and none comes in a session's first 10 minutes. The wall
clock, the cheap cap, a loop (three nudges) and the agent's own asks
decide in code. Every other trip asks the check-in, the class's
`checkin_model` (purpose `checkin`) with read tools on the live
workspace. It reads the trip, the guidance already delivered, the gates
and the transcript's tail, and answers `continue` (the detector resets),
`message` (its guidance goes into the running session through the
nudge's path), `handover` (the session stops, and its note opens the
next brief) or `human`. An unreadable answer, a note-less handover too,
gets one repair round, then a human. A message the worker cannot take
(501, network) stops the session with the guidance as a finding
(`redirect`). A delivered message starts every progress detector afresh
(the diff's baseline and the nudges stay). The stuck rule (rulings 224,
327): after a delivered message, the next detector trip with no file
written since, or a flip_flop or churn trip, is stuck, with the finding
`stuck after the supervisor's message: <kind>: <detail>` and a line in
the statement. A handover on it is `retry_stronger` with the note: the
session climbs in place when it can, else the next session starts on the
stronger rung (the same model at the top) from the note. A check-in that
cannot answer (a refused call, no model) gives `retry_stronger` on a
stuck trip and `continue` on any other. The stall, the clock,
`spend_no_push` and the size trip are never stuck.
The statement of a check-in at a session's start or in a running session
ends with the pace line: "Pace of the card: N changed lines so far
against its bound of M; X EUR spent, Y left; A minutes of agent time
spent, B left; session I of J." Where no diff was measured, the line says
the changed lines are not measured at this moment. A conversation gets
`limits.checkin_calls` (20) calls, the last without tools, then a human.
`limits.checkin_share` (10 percent of the card's budget) caps all the
card's conversations, checked before each, and a trip past it asks a
human with no call. Nothing the check-in answers changes a cap or a
budget. A stopping session's pod is deleted with a 60 s grace period (a log stream
that breaks meanwhile, or at any time while the pod runs, is reopened
and the lines already read are skipped): the worker ends the agent,
commits and pushes its work in progress through the same clean push
path, with the push token factoryd answers its request with during the
grace period, and the next session starts from it. Escalation moves the task to
`waiting-human`, comments the reason and the digest summary on the
issue, and waits: a newer non-bot issue comment starts one more attempt,
closing the issue fails the task. The workflow keeps every answer of the
task (change id `answers`, ruling 118), and every later session gets all
of them, newest first (the comments answering one escalation in the
order they were written), as `SessionInput.Answers` (worker env
`ANSWERS`), each as `<author>, to "<the escalation's first line>":
<answer>` (the answer cut to 4000 bytes; the issue keeps it whole, and
the console refuses a longer answer, naming the bound and its length).
Every worker env value that grows with its input (`OBJECTIVE`,
`FINDINGS`, `ANSWERS`, `POSTER_INFO`, `DIRECTORIES`, `SPEC`,
`REPORT_GUIDE`) stays within the 128 KiB Linux allows one environment
string: a list leaves out its oldest whole entries first, a text is cut,
and `LEFT_OUT` carries factoryd's note on each, which the brief says in
factoryd's own words ahead of its sections, outside every quoted block.
Escalate records its
comment as an `escalation` intake event, and `Reconcile` rebuilds the
answers from the intake events (every trusted issue comment newer than an
escalation's comment, tagged with its question; ruling 120), so a run the
heal pass starts keeps them too. The opencode prompt puts the answers
ahead of the objective as settled
answers the agent must not escalate again; the `session_start` timeline
step records them. The
human is the one party allowed to raise a limit: the answer buys the
extra attempt, and if less than a quarter of the task's resolved agent
time (never under the 5 minutes a session needs to start) is left under
the cap, the answer also grants the difference, so every answer gets at
least that slice of working time (change id `answer-slice`, ruling 237).
Both grants are recorded on the task row. Every
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
on the task's account (purpose
`gate-supervisor`): `wait` (the attempt's bound is extended once by its
own length, and at least half a bound after the answer; not offered for
a session, whose start-to-close ends first), `retry` (the attempt is
stopped with the retryable error `gate_supervisor_retry`, so the
activity's retry policy runs it again; not offered on its last allowed
attempt), `cancel` (the workflow cancels the activity and fails the task
with the diagnosis) or `escalate` (the workflow cancels it and escalates
like a session's check-in, the diagnosis and the recommendation in the
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

The events (`internal/factoryd/events.go`) are timeline lines of step
`event` on the task they concern: `card_created` (intake's new row,
`FileAgentTask`'s card, a plan's cards) with `track` and `by`,
`task_ended` with `state` (`Finish`, the heal pass's failure of a
malformed task, a closed human-lane issue),
`pull_request_merged` with `pr` and `by` (`Merge` for factory-merge's
merges, intake for a human's merge in Forgejo) and `all_children_done`
with `cards` on the plan task when the last of its cards turns done,
which also comments on the plan's issue.

The dependency wait (`internal/factoryd/deps.go`, change id `dep-wait`):
a card of a process with the `dependencies` block waits in the queue,
holding no work slot, while a card it depends on (`task_deps`, which the
plan's filing fills) is open: the admission starts it once every one is
done or one ended closed. Its workflow asks again before its first
session. None open: it starts. One ended `failed` or `closed`: the card
waits for a human (`dependency #3 (links-api) ended failed: close this
card, or answer to start it anyway`); an answer starts it with that
line as a finding and is kept on the timeline (`dependencies_started`,
so a later run does not ask again), a close ends it `closed`, as does a
close Reconcile finds before the card started. Else it enters `waiting-deps`
in gate `dependencies` (the board reads `waiting on #3, #5`) and waits
for the signal `dep_ended`, which `task_ended` sends to every card
waiting on the task, or 10 minutes, then asks again. The heal pass starts
every waiting card's workflow and signals `dep_ended` to each whose wait
would end.

The planner (`internal/factoryd/plan.go`, change id `plan`): a card on
the plan track runs report-mode sessions of its class (the `planner`) on the
stronger rung with the plan guide: the issue is the spec, the answer one
fenced json block (a summary and `min_cards` to `max_cards` cards, each with key,
role, title, brief, directories, depends_on, acceptance and size), a card
one session of the pinned `agent_time` and `steps`. `ValidatePlan` judges
it (one line per finding); factoryd adds every card's edge to the
foundation card and the integration card's to every other, and refuses
a cycle. An invalid
plan goes back to the planner as its findings. A valid one is the task's
next revision (`plan_revisions`, timeline `plan_revision`), and
`RequestPlan` asks its questions (and the confirmation of its spec and
criteria, once). A revision with no question files its cards in the same
step and no click (roadmap 225): `fileRevision` judges it again (a
finding files nothing, comments `revision <n> files no cards: ...` once
and goes to the planner), then files each card in topological order,
once however often it runs: a task row queued on the direct track at
main's head (source `plan`, the plan's process, `plan_key`, the plan
card's author as author) and a bot issue labelled `agent` and
`state/queued` under `<!-- factoryd plan <task> card <key> -->` (found by
that marker on a retry), `card_created` per card; the edges as Forgejo
issue dependencies and `task_deps`; then starts each card under the work
bound, where it waits for its dependencies; the issues in the plan's
comment under `<!-- factoryd plan <task> revision <n> -->` (sanitised:
summary, a table of the cards, each brief in a details block); the plan
task ends `done` (`plan revision <n> filed <k> cards`). Plans do not
nest: a plan files direct cards only. A revision with open questions is
posted with no cards, an approval request of kind `plan` (`{"revision",
"sha256"}`) records what waits, and the task waits in `waiting-human`
(Needs you), its work slot freed. The answer that leaves no question
open sends the planner back with the answers; a writer's comment (one
made while a session ran included) sends it back with the comment and the
last plan; an answer to a revision that is no longer the newest is
refused (`the plan changed to revision <m>; reload`); a closed issue ends
the task `closed`. A planner that stops to ask a human (its
`escalate_to_human`, the gate supervisor) puts the question on the card,
and the answer goes to the next session; a session without a plan passes
its last message on with the findings. Every session counts against
`limits.sessions`; at the cap the card waits with the findings and a
writer's comment buys one more. A card's sessions
get the section "The spec and the plan" (the plan's issue and summary,
the pod's `SPEC`) and the feature guide instead of the work guide
(`CARD_ROLE`; the integration card's adds the `acceptance/` tests).

