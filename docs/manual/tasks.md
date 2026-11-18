# Webhook intake (task 42)

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
event type is logged, never the body. Every issue becomes a card on the
track its labels ask for (`internal/factoryd/tracks.go`, roadmap 225),
idempotent per (repository, issue number): redeliveries and later relabel
events on the same issue find the task already linked. Whoever files a
card sets its track, plan or direct, and no card is filed undecided: the
schema refuses an agent card without one (`tasks.track`, constraint
`tasks_agent_track`). The track rule is fixed in code, the same for every
process:

| labels | card |
| --- | --- |
| `human` (wins over the rest) | a person's card, `waiting-agent` until its case's face, no agent run, no track or process on the row |
| `agent` or `bug` | direct |
| `plan` | plan where the project's process has a plan track, else no card |
| `backlog` or `improvement` | no card: the issue holds an interest until a writer's label commits it |
| none of these | direct |

A direct or plan card is queued at the branch head with its track's class
as its capabilities, its row keeps its track and its process
(`tasks.track`, `tasks.process`), and its workflow starts under the work
bound (the admission below). An issue that files no card is read again by
every label event and every heal pass: a writer's `agent` or `plan` label,
or the removal of `backlog`, files it then, its author still the poster.
A label event moves no card's track, and only retries a queued card's
workflow start that failed. The sizing step moves a direct card that is not
small to the plan track before its first session. A running direct card
turns plan-track only through `BecomePlan` (signal `to_plan`, change id
`direct-to-plan`), which the sizing step and the signal both use, between
sessions: the card keeps its row and its issue, takes the plan class, and
its issue gets the `plan` label in place of `agent`.
An issue whose reproduction command breaks the sandbox rule files a
person's card instead, which asks the poster to file it again with a
command that keeps the rule. Forgejo 15 applies whatever labels its
new-issue form posts, from any account (checked on 15.0.9: an account
with no access filed issues with a template's label, with smuggled
`agent` and `improvement` label ids, and with a label and no template;
all three kept their labels, while the API drops a non-writer's labels
and refuses its later label edits with 403), so from an outside account
intake reads only `bug`, `improvement` and non-track labels: `agent`,
`plan`, `human`, `priority/*` and `severity/*` count from writers only,
and intake takes them off an outside account's new issue. The task
keeps its author and whether they could write the repository then
(`tasks.author`, `tasks.author_writer`); an API task is the
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
label events like any other. `POST /tasks`, SigNoz alerts, the roller,
the routines and the factory's own pull requests file a Forgejo issue
through `FileAgentTask`: a direct card of the project's process, so every
task — API-created or webhook-created — has exactly one linked issue. A
roll and a factory pull request's card take the direct track's class, no
process of their own, and run outside the work bound.

The work bound (`BoundedWorkflows`) is the one admission step every start
passes: a queued card starts only while fewer than its project's
`work_in_progress` cards hold a slot, no human's hold keeps it, every
card it depends on is done (or one ended closed, which its workflow takes
to a person), and its project's month budget has headroom. Otherwise it
stays queued, holding no slot.

A human holds a card that has not started (queued and never admitted, or
in `waiting-deps`) with the drawer's Hold or the chat's `hold_card`: a
reason the board shows, and an optional release card, any card of the
same repository of any plan but not a plan itself, which is `done` once
it files its cards, before their work lands. A release card that already
waits on the held card, through its dependencies or another hold, is
refused: both would wait for ever. The held card gets no work slot, and
its dependency wait asks nothing, until the human releases it (Release,
`release_hold`) or its release card is `merged` or `done`. The hold
follows the release card's work into a retry or a successor, as a
dependency edge does. A release card that ends `failed` or `closed`, or
that a cut turns into a plan, releases nothing: the card stays held and
its chip turns to the warning that names the release card, its plan, how
it ended and the ways on. For a release card that failed, a retry of it
comes first, since its retry takes the hold over. A release of the hold,
or a hold on another release card, comes next. A closed one leaves these
two. A release card's end `failed` or `closed` is told once too: its end
records `card_hold_stranded` on each card its hold keeps, and the inbox
of each human the project tells gets one inform, at needs-you, naming
the held card, the release card, its plan, how it ended and the same
ways on. A hold released, or taken over by a retry's card, before the
poke goes out tells nothing. It opens no case and asks no rung. A card that ends, or moves to the human lane, takes its
hold with it, and its `card_hold_ended` row names the move. Release
refuses a card that no hold holds.

Every revision of a plan keeps the key of a held card, whoever asked for
the replan, and the card takes the entry the revision gives that key. A
revision that drops the key is refused at its filing, naming the hold
and what fits: keep the key, and when the plan no longer needs the card,
ask in a question of the revision that the human close it or release its
hold on its drawer. Once the human closed it, the replan rules ask for
its key to be left out. The planner's record of the plan's cards shows
each held card's hold.

A held card owns no wait, so no queue or dependency flag opens a case on
it. Rungs 0 to 2 neither start nor close it: the hold is a fact of its
cases (`hold`, and `plan_hold` for a held card of the plan, read through
the successors its cards were cut into), and the action table refuses
their resume, continue, handover, successor, close and replan of the card
itself, and a restart_plan of a plan that holds a held card, naming the
hold and what fits the rung there: the case's note or settle where the
table offers them, and the hand-up. A rung's statement lists each as not
offered with that reason, and a hold that could not be read refuses them
the same way. Their other answers, a fund, a note or the retry of a
failed dependency among them, go through. Act records a rung 2 answer as the project agent's
(`factory-chat`), never as the human whose turn woke it. The chat sets
and ends a hold only on a line the human typed in that turn, or on their
press.

A card that ends or parks on a person or its image frees its slot,
and the oldest queued card of its project starts (the pull, in filing
order); the heal pass re-derives the same admission every minute.

A card back from a wait (a human's answer, an approval, a dependency, a
fund after a cap) passes the same bound at its session start
(`AdmitResume`): it starts while the project's live count is under
`work_in_progress` plus `surge` (2 unless the project's file says
otherwise), else it waits queued, holding no slot. The queue admits the
resumed cards, oldest resumption first (by when the wait ended), before any
never-started card. The planner's sessions pass the same step.

CI pods are bound apart from the work bound. The CI spawner starts a
runner pod only on a node with room for its ephemeral storage, CPU and
memory, and pins the pod there: the node's allocatable less what every pod
placed on it requests, in every namespace, as the scheduler counts it, and
less the storage of the pods still unplaced and 10 GiB for each cache the
factory runs (each project's CI cache and agent cache) on every node, since
a cache's directory stays on a node up to 14 days after its last pod. A
node with a NoSchedule or NoExecute taint takes none. A job that does not
fit stays queued in Forgejo, and the log names the shortfall.
`CI_MAX_CONCURRENT` stays the outer cap.

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

A task's state (`queued`, `waiting-deps`, `running`, `waiting-agent`, `waiting-human`,
`waiting-image`, `waiting-poster`, `in-review`, `done`, `failed`, `closed`) is mirrored
onto its issue as exactly one `state/*` label, swapped by `Forgejo.SetStateLabel` through the shared
`mirrorTaskState` helper (`internal/factoryd/forgejo.go`), the one call
site behind the task workflow's activities, completion, and the webhook
intake; the full label set (`agent`, `human`, `bug`, `improvement`, the
priorities and severities, one `state/*` per status, and the track labels
of the project's process: `backlog`, and `plan` where it has a plan track)
is created at boot by `EnsureProjectLabels` if missing, and on every
project by its registration. Mirroring
failures are logged and retried on the next transition, never fail the
task itself. A webhook-created task is mirrored immediately after its
row exists: `state/queued` for an agent card, and `state/waiting-agent`
for the human lane, which starts no workflow.

factoryd's own human cards (a stalled or red chain, main still red, a
refused configuration, held production, a failed or refused roll, an
alert, a routine's card, a factory pull request on a project without
agents, a person's issue in the human lane and a plan's workflow card)
are written with their question in `waiting-agent`, which Needs you does
not list, and their ask row is marked `held`, so it pokes and wakes no
one. The ask's case opens at rung 0 and climbs: rung 2, the factory
agent, may take one of the card's choices, though it may end the card
(close, check again, wait, dismiss) only once factoryd reads the card's
condition gone, and never park it out of sight. A card rung 2 decides
ends with no issue. Otherwise its face files the issue (a card that has
one gets a comment), moves the card to `waiting-human` and pokes. The
chat's escalation, the month cap's card and the judgment budget's card
ask a human at once.

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
completion (`closing`) finishes with the end the run stored with its
move to closing, which `Finish` makes before its cleanup (failed when
none is stored); a closed issue or a PR closed without merge finish it
failed; an open PR is adopted, and the run goes straight to
its checks and the review wait, with no new session. Before the first
session of a direct card that a person or the chat filed (source `webhook`
or `api`) in a process with a plan track, and that carries no pull request,
the sizing step runs (gate `sizing`, `SizeCard`). A
writer's filed `medium` or `large` runs no session: the card goes to the
planner. Otherwise an `analyzer` report task of source `sizing` reads the
issue and the repository and answers a size, a reason and the files; an
outsider's filed size is no floor. The step is recorded first, then the
size is written, so a retry completes the write. The write sets the size,
and the budget of that size (`small` 4, `medium` 5, `large` 10 EUR) when
the card still holds the budget its filing's size gave it; a `Budget:` line
or a fund keeps its value. A `small`
card runs the coder. A `medium` or `large` card turns plan-track through
`BecomePlan`, and the planner starts from the files the sizing named. A
`to_plan` a person sent during the sizing joins that switch. If
`BecomePlan` refuses, the card runs as filed. If the sizing fails or
answers no size, the card runs as filed, and the Time section shows the
size and the reason, or why there is none. Then: up to
`limits.sessions` (3) attempts, each `SetState running` then
`RunSession`, the first on `MODEL` (default `factory/small`), or
on `STRONGER_MODEL` when the task is a coding card (`session: code`) of
a project whose file sets `model: stronger` (the planner, the screening
and the sizing keep their own rungs); every session starts from the task branch's head on the fork when the
branch exists, else from the base SHA. After a pushed commit
`OpenPullRequest` (head `factory-agent:factory/<id>`, base `main`,
`Closes #<issue>`, reusing an open PR for that fork branch and recording
its number on the task; a commit whose `.forgejo`, `.gitea` or `.github`
tree entry differs from the base's gets no PR, and an already open one is
closed with the reason), `SetState in-review`, `WaitChecks` on the head commit's
combined status, which Forgejo records on the base repository
(bound `waits.checks`, default 50m). A task that fails (its issue closed,
its PR closed unmerged, the ladder exhausted) closes its open PR, found
by number or, when none was recorded, by its branch on the fork, and
then comments the reason on it once (the comment follows the close, so a
retried close never repeats it). The branch on the
fork is deleted only for a task whose pull request merged. The branch of
any other task, failed or closed, stays so that a refile can build on it,
and an hourly pass (`SweepBranches`) deletes it once the task ended 30 days ago.
Close and delete happen before the task turns final and
retry through a Forgejo outage, so no failed task keeps an open PR; one
Forgejo refuses (a 4xx, which no retry fixes) is logged and the task
completes anyway.
Red checks make the next attempt climb to `STRONGER_MODEL` (default
`factory/big`) with the failures as findings; supervision
(see [Supervision](gates.md#supervision)) can redirect, climb or escalate. Red checks on a head that a
session left as it was (the agent could not act on them) run once more
before their findings go back to the agent: `RerunChecks` puts an empty
commit on the branch (Forgejo 15 has no API to re-run a job); a head red
after its re-run is not re-run again. Signals
from the webhook intake are delivered with signal-with-start, so a signal
to a task whose run died starts a new one (which reconciles first).
`pr_merged` (done),
`pr_closed` (closed without merge: failed) and `issue_closed` (done if
the PR merged, else failed) decide the task wherever they land (a
trusted user's close of a failed card's issue ends the card `closed` in the
store, so its plan can close): a
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
restarted by the heal pass. The wall-clock cap (the class's `agent_time`,
halved for a small card and raised by half for a large one) bounds the
agent's working time summed over the attempts; waiting on checks or
reviewers does not count. A session starts only on 5 minutes of it left,
or on the whole cap when the card's cap is shorter, so a small card whose
class gives under 10 minutes still runs its first session (change id
`sized-least`). Its stop says the agent time is used up, apart
from the session cap's stop, and a grant buys the next session a quarter
of the cap. The used time and the attempts are recorded on the task row
(`RecordUsage` after every session), so a new workflow run for the same
task (a resubmitted issue, a run restarted by hand) resumes the count
instead of starting a fresh cap. The console's Extend raises sessions and
lines in columns the workflow never writes; the workflow reads them before
each cap check and after every answer, so an Extend on a running or
queued card stands. A stopping factoryd detaches from a running
`RunSession`: the pod runs on, and the next factoryd's retry reattaches to
it, a pod that finished meanwhile included, and reads its log from where
the old factoryd stopped. A pod that is gone starts the attempt over from
the pushed branch.
A crashed factoryd answers no push request, so a running pod's
unpushed work in progress is lost; the retry removes the
dead session's namespace, pod and credentials and starts a new session
with the dead one as parent and its findings.

