# Console (task 54)

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
  gets 403) and may still deny (a writer of the request's project),
  request changes, reject and release. Each button's action is one
  function in `internal/console/actions.go` (`doApprove`, `doDeny`,
  …) with its checks, its Forgejo calls as the human and its one event
  row; the project agent's tools call the same functions. Forgejo's approvals whitelist on
  main stays the merge authority.
- `/`: the board (done and failed for 7 days, at most 300 tasks, open
  ones first) in four columns: Needs you, Queued (`queued`,
  `waiting-deps`, `waiting-image`), In progress and Done. `?view=wide`
  shows one column per task state instead: Queued, Running, Waiting on a
  human, Waiting for image, Waiting on the author, In review, Merged,
  Done, Failed, Closed. `?project=<name>` (a select at the top) shows one
  project. Needs you holds approval requests, escalations with their
  reason, read from the escalation's intake event, and an answer box,
  human tasks, and cards waiting on their author. htmx
  polls `/board` every 10 s. Above the columns the board names the
  configuration version factoryd runs with its commit, linked to the
  commit's tree in the infra repository, as `/policy` shows it. A card
  shows the task's time in all (from the
  issue's `created_at`) and the gate it is in with the gate's age, from the
  row's `gate` and `gate_since`; a plan's card `part of plan #n`.
- Filing a card: no card waits undecided and none moves between columns
  by hand (roadmap 225). The new-task form's track select (Direct by
  default, Plan, Human) sets `agent`, `plan` or `human` on the issue;
  Plan is offered only for a project whose process has a plan track, and
  a POST for one without is refused with one sentence. The intake files
  a direct or plan card `queued` on its track, and the scheduler starts
  it once a work slot is free.
- The plan card: a pending approval of kind `plan` is the record of a
  revision's open questions. It shows the spec's first lines, the
  summary, the cards (key, role, title, size, depends on, factoryd's
  edges included) and each open question with its choices and a free
  answer, posted on the issue as the member. The answers go to the
  planner, and its next revision with no open question files the cards,
  `queued` on the direct track, and the plan card ends done. A stale
  revision shows `the plan changed to revision <m>; reload` on the card.
  "Request changes" posts a comment as the member, which the planner
  revises the plan with. A done plan lists its cards by issue and state. A
  plan's escalation shows on its card only when newer than the pending
  request. The card's forms keep the project filter; a browser's form is
  answered with a redirect to the board, htmx with the board itself.
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
- Approval cards in the human lane, from `approval_requests`. Each opens
  with its brief, one factoryd call (purpose `brief`) a moment after the
  request files. A brief the card's own budget refuses (`refused_budget`)
  says so in its place, and an approver's Fund there (`POST
  /tasks/{id}/fund`) raises the card's budget and queues the brief again,
  without answering the card. A resource
  request shows its type, purpose, lease, monthly cost, project and task;
  approve for the task or the project, or deny with a reason (`decided_by`,
  `decided_at`, `decision_note`). A merge request shows the pull request,
  the risk class, the migration flag and the model review. Approve submits
  an APPROVED review at the gated head as the human, and only once Forgejo
  took it calls `factoryd.ApproveMerge`; deny with a reason submits a
  REQUEST_CHANGES review (the changes-requested path sends the task back)
  or rejects: the reason is commented and the pull request closed as the
  human, which fails the task. A merge Forgejo refuses (or that fails and
  is retried) notes why on the pending request, and the card shows it. A
  pull request factoryd opened has no agent (roadmap 264): its card offers
  Approve on green checks and Re-run the checks on red ones
  (`POST /approvals/{id}/rerun`, an empty commit on its head, and the card
  asks again once they settle), with Close beside either, the deny box
  without a choice: the reason goes to its workflow as a change request,
  which closes the card and its pull request and issue with it.
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
- The project agent (stage 15 phase 4a, architecture 5.7): a writer of a
  project chats at `POST /projects/{name}/chat` (form field `message`;
  403 for anyone else) and gets the turn as an event stream: `text`
  pieces, `tool` blocks (name, arguments, duration, the result's first
  line), `proposal` (id, tool, arguments, description), `applied` (with
  an `undo` token where one exists), then `done`, or `error`, which ends
  it. The loop calls the gateway with purpose `operator` on the project's
  chat task `t-chat-<name>-<yyyy-mm>` (kind `chat`, capability `chat`, the
  block's `monthly_eur` as budget, filed by the month's first message,
  which closes the month before's, no card and no workflow; a raise of
  `monthly_eur` reaches the running month, and none lowers a fund). The
  gateway alone judges whether a call fits: a call it refuses for money
  ends the turn on a row of kind `budget` and a `budget` event carrying
  it. The chat task's own `refused_budget` records the stop once
  (`chat_budget_spent` on the chat task, with the fund it suggests: the
  last 7 days' rate carried to the month's end, at least 1 EUR), which
  pokes the inbox, holds every wake (`ErrChatUnfunded`) and offers an
  approver Fund and go on under the row (`POST
  /projects/{name}/chat/budget`, `eur`): the budget rises and every
  stopped turn goes on. A factory cap's refusal names the cap and what
  raises it. The resumed context leaves the `budget` rows out. Its tools (`policy.ChatTools`) are the reads `system` (the
  factory's manual, rendered from the code at call time; its states,
  gates and section on sandbox images come from `internal/manual`, which
  the workers' `manual` tool reads too), `board`,
  `card` (the card in full, with a pointer to the read holding its
  reviews, audit, checks, plan and sessions), `history`, `card_history`,
  `costs`, `diff`, `plan` (a plan's newest revision: its rows with their
  issues and the filed cards' states, and for a revision without questions
  the earlier revision's answers it was built from), `pr` (the newest checks' jobs, the
  model reviews, the audit, the files' diff stat, the diff on request),
  `file` (a file at a ref, read raw through the member's token, cut at 64
  KiB, a binary one refused), `session`, `session_steps`, `session_step`,
  `session_search` (a session's summary with why it started, its steps one
  line each, steps in full, a search of the transcripts and, with no step
  matching, of the sessions' ends; transcripts over 8 MiB are only
  searched), `ci` (a commit's combined CI status with each job's, main's
  head unless a ref or sha names another, through the member's token) and
  `project_images` (the project's pins in force with their tools, egress,
  short digest and key, its open tool cards, and its cards in
  `waiting-image` with the image each waits for), and
  one per console action,
  run as the human through `actions.go`, each refusing a card of another
  project; a tool the `chat.confirm` list names answers a proposal, which
  `POST /projects/{name}/chat/proposals/{id}` runs, and `comment`,
  `set_summary` and `set_accent` answer an undo token for
  `POST /projects/{name}/chat/undo/{token}`. `new_card` takes an `image`,
  a name the project pins in force (`default` names none), which reaches
  the issue as an `Image:` line among the field lines (`Size:`, `Rung:`)
  after a blank line; the intake reads it into the card's row from a
  writer's issue only. The proposal and the filing refuse any other name
  with the pins and `request_image`. `new_card` and `comment` take
  `attachments`, at most 4 of the thread's images by name (`img1`): an
  unknown name refuses before anything is posted; the text gains a line
  `Attachments: img1.png, ...` and each image goes up as an asset of the
  issue or the comment, named for its type, as the member. An upload
  Forgejo refuses leaves the card or comment standing, and the answer
  names the images to attach again. A session looks at them with
  `analyze_image`. `request_image` (`tool`, `need`, an
  optional `image`, else one named after the tool) puts the member's ask
  on a tool card as a session's `request_tool` does (above), refused at
  the same cap; the card opens with who asked for which tool and why. A
  filing meets the chat's admission as `new_card`'s does (a wake files
  nothing on a closed intake, and no ask files past the queue cap; a
  join files nothing and meets neither) and writes a `card_filed` row
  with the track, the reason for it, the title, image and tool. A project without agents offers
  neither `project_images` nor `request_image`. Proposals and
  undo tokens are the member's on
  that project, used once, for 30 minutes (404 otherwise, 403 for
  another member's). A wake, the turn the Waker starts on a member's
  thread with no member behind it, makes every Forgejo call as
  `factory-chat` with the project's chat token (`Forgejo.Access`'s
  `Chat`): it needs no login of the member and reads none. A wake's turn
  that fails, as a fund's resume or a card answer in words that fails,
  leaves `This turn failed: <why>` in its thread, since no member reads
  its stream. Its chat rows
  and events stay the woken member's, decided by the agent, and so do the
  cards it files (the intake reads the author of an issue `factory-chat`
  posted from the console's `card_filed` row; an issue with none is no
  card) and the cards of a plan it files. A filing whose `card_filed`
  row cannot be written is closed with a comment and refused, naming the
  issue. Its comments, and so its answers to escalations, are
  `factory-chat`'s: the agent reads such an answer as the project agent's,
  never a human's. The intake judges a label by its Forgejo actor, so a
  wake labels as `factory-chat`, a writer. A review the agent decides, in a
  wake or in a member's turn, goes out as `factory-chat` too: an approval
  posts none, a change request is `factory-chat`'s, which the intake
  trusts and signals and which blocks no later merge. A project without a chat
  token gets no wake; the refusal names `tokens.chat`. `steer` reaches a
  running session on the supervisor's message path
  (`Runner.MessageSession`). The conversation
  is `chat_messages`, per project and member, each assistant row with its
  tool calls and their results; a turn's context is the project's status
  line (for a project without a board, agents none or not worked on,
  why it has none and its spend this month against its month cap), a
  line of its sandbox images (`Sandbox images:` the pins in force
  with their tools, and what a card without an image runs; none for a
  project without agents), the thread's
  summary and its last 200 messages, the status and every tool text
  through `sanitize.Inbound`.
  `GET /projects/{name}/chat` answers the last 100 as JSON. The `chat`
  block of `policy.yaml` or a project file (`model`, `monthly_eur`,
  `confirm`; defaults in `policy.DefaultChat`) sets the rest; a project
  file's keys replace the policy's, its `monthly_eur` at most the
  policy's. By default the OK is asked for `new_card`, `request_image`,
  `answer` and `steer` (text injected into the board could otherwise make
  the agent answer a waiting card or message a running session as the
  member), `retry`, `request_changes`, `approve_merge`, `deny_merge`,
  `invite`, `github_export`, `github_sync`, `close`, `edit`,
  `grant_session` and `set_dependency`; `confirm: []` asks for none.
  `new_card` takes a `track` (`direct`, `plan` or `human`) and a one-line
  `track_reason`, both required. `close` closes the card's issue as the member (the face's Close
  button too; a failed card's close ends it `closed`, with an issue or without, and
  Close shows on every failed card), `edit` its title or body and the task's objective, which the
  next session's brief reads, `grant_session` answers a card at its session
  cap, and `set_dependency` adds or removes a dependency of a card that has
  not started, in the store and on the issues as the bot. An added
  dependency joins two cards of one plan; a card of no plan is a group of
  one, and a refusal names the fitting action.

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
merges, its `gate_token` approves the merge gate's merges, its
`chat_token` is the project agent's own identity in a wake, the projects
token lists its Actions jobs and registers its runners; a seed project's
file names seeded secrets (`tokens.read`, `.actions`, `.merge`, each one
of `forgejo-ci-registrar`, `forgejo-merge`, `forgejo-config`, `.gate`,
only `forgejo-gate`, and `.chat`, only `forgejo-chat`), which factoryd
holds as `FORGEJO_CI_REGISTRAR_TOKEN`, `FORGEJO_MERGE_TOKEN`,
`FORGEJO_CONFIG_TOKEN`, `FORGEJO_GATE_TOKEN` and `FORGEJO_CHAT_TOKEN`.
`forgejo-projects` is no allowed value: a seed project never gets the
projects token (ruling 164). A seed project without `tokens.gate` merges
nothing on the merge rule `auto`: the merge is refused, naming the
missing gate token. The code repository names `forgejo-ci-registrar`
(read, Actions), `forgejo-merge`, `forgejo-gate` and `forgejo-chat`; the
infra repository `forgejo-config` (read: the trust gate and the heal
pass's close of a human-lane task; the org-read token adds the teams) and
`forgejo-chat`. `factory-chat` is a write collaborator on no whitelist:
the trust gate hears its comments as a writer's, as a member's, it can
neither push to main nor merge, and no approval of its counts. The factory's own project is the one whose file lists factoryd's
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

The reconcile of a factory project (its file says `objects: factory`) whose
row lacks a token registers it first (ruling 212): with the Forgejo site
admin's login, read from OpenBao's kv-v2 `factory/forgejo-admin` (role
`factoryd`, the projected token of audience `openbao`) into memory for that
run and never logged or stored, it does what only the admin can: the organisation, the repository
(private, `main`), the user `ci-<name>` (random password, discarded) in the
team, and the tokens, each minted with `?sudo=`: `ci-<name>`'s
`write:package` (`registry`) straight into the Actions secrets
`REGISTRY_USER`/`REGISTRY_PASSWORD` (the project pushes its own packages and
no one else's), `ci-<name>`'s `read:package` (`pull`, row `pull_token`, the
namespaces' image pull secret), `factory-projects`' `read:repository` limited
to the repository (`git-<name>`, row `git_token`, Flux's source),
`factory-merge`'s `write:repository` limited to it (`project-<name>`, row
`merge_token`, the Merge activity's on this project), `factory-gate`'s
(`project-<name>`, row `gate_token`, the merge gate's approval) and
`factory-chat`'s `write:repository` and `write:issue` limited to it
(`project-<name>`, row `chat_token`, the project agent's wakes). A row
without one of these tokens, a project registered before its bot,
registers again on its next reconcile, which mints the missing token
alone. An existing token is
kept unless `factoryd register-project --name <name> --rotate`, which
revokes and mints them all again. The reconcile decides from the row read
under its lock, so a reconcile queued behind a registering one mints
nothing again. A failed registration logs its line, the console's panel
shows the reconcile's error, and the next reconcile tries again. These tokens
are plaintext in Postgres and so in its backups: the namespaces and Flux need
them back after a rebuild; each reaches one project only.

factoryd reconciles every project at boot and at every new configuration
version (a file changed, the approvers), one log line per object (`project
<name>: <object>: created|updated|no change|waiting: ...`); the first
reconcile of a factory project whose every line is created, updated, no
change, ensured or ready sets the row's `ready_at` (the console's "Board
ready"), which nothing clears; for a factory project:

- Forgejo, with the projects token: the webhook to factoryd (every issue and
  pull request event and pushes, JSON, the intake secret; the row keeps the secret's
  sha256, so a re-seeded secret reaches the hook), `factory-agent`,
  `factory-merge`, `factory-gate`, `factory-chat` and the policy's approvers as write collaborators (Forgejo
  keeps only writers in a whitelist); the seed: when the repository has no
  `main` and the project file names a `template`, the files of
  `templates/<template>/` of the configuration version in force, rendered
  with Go's `text/template` (delimiters `[[` `]]`, `missingkey=error`, data
  name, repository, image and each environment's namespace by path), as
  `main`'s first commit by `factory-projects` through the contents API,
  message `Seed from template <template> (registration)`; a repository with
  `main` is never seeded, and without a template the line waits for the
  first push; `main`'s protection: the code
  repository's (push whitelist the approvers and `factory-merge`, merge
  whitelist the same, approvals whitelist the approvers and `factory-gate`, apply to admins,
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
  2222); with a `deploy/staging` environment, after the cluster objects,
  `STAGING_URL` (`http://<name>.<namespace>.svc.cluster.local:8080`, the
  template's Service) and `STAGING_SECRETS`, one `KEY='value'` line per
  `secrets` entry with the staging namespace's value single-quoted, since the
  template's smoke test sources it as shell, then one per key of each Opaque
  Secret in the staging namespace that the project's staging Kustomization
  applied (Flux's labels `kustomize.toolkit.fluxcd.io/name` and
  `/namespace`), by Secret name and key, a key already written left out, so
  the file's entry wins (a key that is no shell variable name gets a comment
  line and no value; with no line at all, one comment line, since Forgejo
  refuses an empty secret), written again when the sha256 of both in
  `staging_hash` changes, at the reconcile and, for a ready project, at
  every one-minute poll between; no value is logged. As the bot: its private fork
  `factory-agent/<name>` and the boot labels. A project whose registration
  failed waits here.
- The cluster, per namespace: the namespace with `factory/project=<name>` and
  pod security `restricted`; RoleBinding `factoryd` (ClusterRole
  `factoryd-project`, which also holds `deployment_health`'s reads); the CiliumNetworkPolicy `project`, a default deny that
  admits in the namespace itself, the edge gateway's Envoy, the CNPG
  operator (8000, 5432) and, in the `deploy/staging` environment's
  namespace, the project's own CI runners (pods `ci-runner` in namespaces
  labelled `factory.ai/ci-project=<name>`) on 8080, and out the namespace itself, DNS, the API server
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
kubectl -n factory exec deploy/factoryd -- factoryd register-project --name demo-app [--rotate]
```

once `projects/demo-app.yaml` is on the infra repository's main and loaded,
reconciles the project (registering it when its row lacks a token, every
token again with `--rotate`; a second run changes nothing) and prints the
edge gateway listeners and Certificates for the infra commit (ruling 155).

Its inverse is `factoryd remove-project <name>` (roadmap 272), run as a one-off
Job with its own ServiceAccount (`k8s/remove-project.yaml`, not in
kustomization.yaml): `--dry-run` lists every step's target and acts on nothing.
It refuses while `projects/<name>.yaml` is still in the configuration in force
(the hourly reconcile would register the project again) and while a card of the
project is open. Each step is idempotent, so a failed step stops the run and a
second run finishes the rest. Runbook, `--dry-run` first:

```
kubectl apply -f k8s/remove-project.yaml
kubectl -n factory delete job/factoryd-remove-project 2>/dev/null || true
kubectl -n factory create job factoryd-remove-project \
  --image="$(kgp -n factory -l app=factoryd -o jsonpath='{.items[0].spec.containers[0].image}')" \
  -- /factoryd remove-project <name> --dry-run   # then without --dry-run
kubectl -n factory logs job/factoryd-remove-project -f
```

The console's `/projects` lists every project read-only with its file's
settings, the envelope with its use and the standing monthly line, and a
link to the file. The console's New project (`/projects/new`, filed by
approvers only) is the
same path without a shell (stage11-design.md section 5). Its form (name,
process, template, merge rule) writes the file with one staging namespace
deployed from `deploy/staging` and no hostname. factoryd judges the file with
`CheckProjectFile`, which runs the loader's `CheckTree` over infra's head
with the file added. The filing starts only on a token of the human that
lasts at least 5 more minutes, refreshed first when it does not (a
refused refresh sends them to `/login`, nothing written), and only while
infra has no branch `project-<name>` (an earlier filing's: the form says
to delete it). The human's token then commits the file to a new
branch `project-<name>` and opens its pull request, and the console
records the `project_filed` event with the pull request's number. When
the opening answers an error, the console reads infra's open pull
requests (newest first, `forgejo-config` token) and keeps the one from
the branch into main as the filing's. Any write that fails (the commit,
no pull request, the import row or the event not written) undoes the
filing as the human: its pull request is closed and its branch deleted
while the branch's head is the commit the filing made, so the name can
be filed again. A branch with another head, a racing filing's or one a
commit that errs left, stays, and the answer says so. The status panel
`/projects/<name>/status` polls every 3 s until its four steps are done:
1. the pull request, read with the `forgejo-config` token by the number
   the name's last `project_filed` event records, so a second filing of
   a name shows and merges its own pull request;
2. the merge, with a Merge button that merges as the human and shows
   Forgejo's refusal, and the version in force holding the file;
3. the repository: no click; the reconcile that loads the new version
   registers the project (ruling 219), and the step shows the row's tokens
   and `main`, or the last reconcile error (`Registrar.ReconcileError`, in
   memory, cleared by a clean reconcile);
4. `ready_at`, with a link to the project's board, or that error once the
   repository has `main`.

The form may name a GitHub repository (owner/name) to start from, with a
token for a private one. The console stores it as the name's
`project_mirrors` row (direction `import`, state `pending`) before the pull
request, and the pull request's number on the row (`pr`) once it opens;
the file, the pull request and the `project_filed` event never hold the
token. An undone filing deletes the row with its token once no pull
request of the filing can merge: none opened, it was closed, or its
branch is gone. If neither the close nor the branch's deletion succeeds,
the row stays so a merge still imports, and the console names what is
left open. Registration then makes the repository with Forgejo's
migrate (`POST /api/v1/repos/migrate` as the site admin, private, no
mirror) in place of the empty create. The migrate runs in a goroutine of
its own, one per project at most, so no other project's reconcile waits
for it: the row reads `copying since <time>` (its `last_update` the
start) and the project's registration waits without failing. The
migrate's end clears the row's token, sets `imported <time>` and
reconciles the project again, or sets `failed: <error>` with the token
taken out and the start kept. factoryd's stop leaves the row copying.
Forgejo 15 finishes a migrate its caller left, and keeps the repository,
empty, when it refuses one (a second migrate into it answers 409). So a
row not imported, with no copy running in factoryd, over a repository is
read against Forgejo: a non-empty repository is the copy's, imported. An
empty one made under 20 minutes ago (twice the migrate's timeout, since
Forgejo syncs branches and tags after the clone while the repository reads
empty) may still be filling: a copying row waits, any other fails the
reconcile. An older empty one under a failed row is the refused
migrate's, which Forgejo keeps marked as being migrated for good: it is
deleted and copied again without asking GitHub, so a refused import is
copied again by a reconcile, at least hourly, 20 minutes after its last
leftover. Under a pending or copying row, GitHub is asked once (`GET
/repos/{owner}/{repo}/commits?per_page=1` on `api.github.com`, with the
row's token if any; factoryd's egress admits it on 443): 409, an empty
source, is imported; a commit means the copy was cut, so it is deleted
and copied again; any other answer leaves the row and fails the reconcile. The site
admin deletes a leftover only when a second read just before the delete
still finds it empty with the same creation time. With no
repository the copy starts. `register-project` waits for a copy it
started and fails while the row still reads copying. The seed gives a source
whose default branch is not `main` a `main` from it, seeds an empty one
whole, and otherwise commits only the template's `.forgejo/` files and `AGENTS.md` that the copy lacks,
with the template's `deploy/` when the copy has none ("Factory workflows",
"Factory workflows and manifests"). The row lives until registration
consumes it or its pull request closes unmerged, however long the review
takes: each poll, every minute, reads with the `forgejo-config` token the
pull request of every import row not imported whose project no file
names. Closed unmerged, the row is deleted with its token; open, it
stays. Merged, it stays for the registration the merge's load starts
while infra's main holds `projects/<name>.yaml`, and is deleted with its
token once main no longer does: the filing was undone. A row that
records no pull request, or whose pull request or file Forgejo does not
answer for, stays and is logged at every poll. Rows put before the `pr`
column take the pull request of their project's last `project_filed`
event that names their remote. The panel
shows a fifth step, "Imported from GitHub", between the repository and
`ready_at`.

A factory project's export to GitHub (the projects page's form or the
chat's `github_export`, an approver's action) has a mode: `mirror`, the
default, a Forgejo push mirror of every branch and tag, or `pull_requests`.
In `pull_requests` the registrar's poll pushes each card done since the
export was put, its head as merged (`refs/pull/<n>/head`), to GitHub as
`factory/<task id>` and opens a pull request of it into the default branch
with the export's token (git reads it from its environment, never an
argument). The task keeps the pull request's number, URL and state, shown
on the card's face and in the chat's `card` read; one GET per open pull
request at most every 5 minutes refreshes it. A definite refusal (a GitHub
4xx, from the first read of the repository on, a push GitHub refuses, a
project repository over 200 MiB) stays on the card as `failed: ...`; a network error, a timeout or a GitHub 5xx is retried by
the next poll. A put of the same remote and mode keeps `since` and gives
each failed card one more try. git runs one operation at a time, each
killed with its process group after 2 minutes, a pass after 4, with one
pack thread and small pack windows (factoryd's limit is 512Mi, its Go
heap held at 460MiB by `GOMEMLIMIT`; its pod
shares its process namespace, so the pause container reaps a killed git's
helpers; git ships in the image the gateway shares). Sync latest (the export row's button, the
chat's `github_sync`) answers at once and brings GitHub's default branch
back when it moved since the last sync and main lacks it: branch
`github/sync-<short sha>` on the bot's fork and a pull request to main,
`Sync from GitHub <short sha>`, as the bot, so its CI runs without the
repository's secrets; a head that changes `.forgejo/`, `.gitea/` or
`.github/`, that shares no history with main, or whose GitHub repository is
over 200 MiB, is refused. The result is a `github_synced` event and the
export row's last sync. At most every 5 minutes the poll deletes the fork's
`github/sync-*` branches whose pull request is no longer open.

`GET /projects/{name}/alerts?namespace=<ns>` on factoryd's port 8080
(`http://factoryd.factory.svc.cluster.local:8080`): bearer the project's
`FACTORY_READ_TOKEN` (compared by hash in constant time; 401 otherwise, an
unknown project too), the namespace one of the project's (403 otherwise).
It answers a JSON array: the alerts firing in the namespace, read from
their alerts rows, the one record of SigNoz's alerts (`source` `signoz`,
taken in by the [webhook intake](tasks.md#webhook-intake-task-42)) and of factoryd's own alert of a
Kustomization that applied once and stands not Ready (`flux`): each
fingerprint's newest row while no resolve came after its start and its
verdict is not `noise` (a roll window's own restart, or a human's close),
plus one
`rollout_not_ready` per Deployment whose rollout has not finished
(kubectl's test: generation observed, every replica updated, no old one
left, every updated one available); `[]` when none. Cached 30 s per
project and namespace; a source it cannot read answers 502, never `[]`.

`POST /webhooks/signoz` on 8080 is SigNoz's webhook channel `factoryd`
(written by the infra's `signoz-objects` job, basic auth user `signoz` with
the password of secret `signoz-webhook`, env `SIGNOZ_WEBHOOK_PASSWORD`;
401 otherwise). Each alert's namespace, its `namespace` label (which the
job sets on every project rule), else its `k8s.namespace.name` or
`k8s_namespace_name` label (a rule's query grouped by it), names the
project through the namespace's `factory/project` label (read once per
namespace). An app cannot forge these: SigNoz's collector drops the
namespace an app's own OTLP names and sets the one of the pod it comes
from (the infra repository's `k8s/platform/signoz/helmrelease.yaml`). An
active project that
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
(fenced, one line each), then, when the service label names a
Deployment in the alert's namespace, factoryd's reading of it as
`deployment_health` gives it (fenced, read within 5 s) or one line saying
why it could not read it, labels `bug` and `state/queued`, and starts its
agent task at the default branch's head (source `alert`, standing
`telemetry`: the brief marks the text as untrusted; no reproduction: the
agent brings its own); a firing still resent 30 minutes after its task
ended is filed once more; a later firing comments `fired again at <time>`
while that task runs and files a new issue once it ended; a resend of the
same firing does nothing; a resolve comments `resolved at <time>` once,
and is recorded on the alerts row even when its filing failed, so a later
firing files anew. Every minute, beside the heal pass, factoryd reads
SigNoz's list of firing alerts (`GET /api/v1/alerts` on SigNoz v0.143.0,
logged in as a viewer-role user, secret `factory/signoz-factoryd`) and
resolves the same way each row that still fires while SigNoz no longer
lists its fingerprint as active, so a lost resolve delivery heals within
a minute (SigNoz's rules evaluate every minute); only the issue of a task
still running hears of such a resolve, so stale rows of finished cards
resolve quietly; a list whose active alerts carry no fingerprint
resolves nothing. An alert card's pre-check
before its first model call reads the card's alerts row: a resolve after
its start closes the card. A rule labelled `event: "true"` (`Job failed`)
marks a failure that happened: its resolve is recorded without a comment
and closes no card, neither through the pre-check nor as a cleared
alert. Each issue is an alerts row of kind `alert` with the
fingerprint, judged
when its task ends: `real` on a merge, `noise` when a human closed the
issue without one. A human-lane alert issue is judged by its close: `noise`
when it carries the label `noise`, `real` otherwise (the closer is
`resolved_by`). A notification that fails in part answers 500, and
SigNoz sends it again.

factoryd raises one alert of its own the same way (roadmap 259): every
minute the registrar's poll reads the Flux Kustomization of each
environment of each ready factory project, and one that applied once
(`status.lastAppliedRevision`) and has read Ready `False` for 10 minutes
fires `Kustomization not Ready` in its namespace, fingerprint
`flux/<namespace>/<project>`, with Flux's message as its value and the
action as an annotation; Ready again resolves it. A path no green build
wrote never applied and raises nothing; `Unknown`, Flux mid-reconcile,
changes nothing. The firing's start is kept on its alerts row, so a
restart of factoryd neither files it again nor misses the resolve; the
SigNoz sweep leaves these rows alone.

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
counts whole). SigNoz's collector names their telemetry's namespace,
`factory`, the one the rules are bound to, from the pods it comes from;
server spans are named by route pattern (`GET /tasks/{id}`), never by
path.

The CI spawner serves every active project on `agents: coder`, each with
its Actions token (Forgejo keeps the runner API to the repository's owner):
pull requests, and a factory project's own main workflow too, which never
touches `sf`'s privileged org runner. A factory project's runs get two more
callees: Forgejo's SSH
(`forgejo-ssh.forgejo`, the pod's 2222) for the deploy-key push and factoryd
on 8080 for the alerts endpoint, and with a `deploy/staging` environment
that namespace's pods on 80, 443 and 8080 and its Services' names
(`*.<namespace>.svc.cluster.local`) for the smoke test, never prod; their
namespace carries
`factory.ai/ci-project=<name>`, which factoryd's ingress admits. The
registration secret in a CI namespace names the repository its runner is
registered in (none: the code repository's).

A standing resource's hours go to the ledger as kind `standing` under
`standing:<repository>`: its project's monthly line, no task's spend and no
day's headroom.

