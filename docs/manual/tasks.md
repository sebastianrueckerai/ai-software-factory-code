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
left, judges the event's sender, and so does a label event that brings in
an issue with no card yet (its lane label judges poster and sender); the
bot's own label events move nothing. The heal pass knows no sender: for a
backlog card whose issue carries a starting lane's label it reads the
issue's timeline and judges the latest entry that added that label
(`type` `label`, `body` `1`) by a user other than the bot; it never infers
a removal, so a backlog card with no lane label stays. A refused entry
lands in the backlog: factoryd takes
the starting lane labels off, puts `backlog` on and comments once why
(`factoryd: lane ready takes a card from writers of <repo>; <user> is
none. The card stays in backlog.`). A card that enters a starting lane
keeps that lane's label as its one lane label, is queued at the branch
head with the lane's class as its capabilities
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

A task's state (`backlog`, `queued`, `waiting-deps`, `running`, `waiting-human`,
`waiting-image`, `waiting-poster`, `in-review`, `done`, `failed`, `closed`) is mirrored
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
`RunSession`, the first on `MODEL` (default `factory/small`), or
on `STRONGER_MODEL` when the task is a coding card (`session: code`) of
a project whose file sets `model: stronger` (the planner and the
screening keep their own rungs); every session starts from the task branch's head on the fork when the
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
retried close never repeats it). A done or closed task's branch on the
fork is deleted; a failed task's stays for a retry until a writer, or
factoryd's recorded close, closes its issue, which deletes it then.
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
A crashed factoryd answers no push request, so a running pod's
unpushed work in progress is lost (ruling 231); the retry removes the
dead session's namespace, pod and credentials and starts a new session
with the dead one as parent and its findings.

