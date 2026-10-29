# Configuration, policy and costs

factoryd's configuration is the directory `factory/` of the infra
repository's `main` (ruling 183): `policy.yaml` (classes with their
`session: code|plan|report`), `processes/<name>.yaml` (ruling 206: the
blocks `tracks` (roadmap 225: `direct` and an optional `plan`, each with
its class, the plan track with the plan's bounds), `dependencies`,
`stages`, each with its `on_findings` rule, ruling 177, and `merge`; a
file that still carries the removed `lanes` block is refused by name) and `projects/<name>.yaml` (one per
project: repository, merge rule, `objects: factory|seed`,
`agents: coder|none`, `process`, notify, namespaces, environments,
quota, secrets, `budget: {month_eur}`, `model: stronger`). The Go types in `internal/policy` are the
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
<process> <card kind>` prints the path of a card kind (`direct`, `plan`,
`child`) through a process, generated from the
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
earlier row again. A refused commit, or one where a class a track names
leaves out a model of the ladder (`MODEL`, `STRONGER_MODEL`), logs `CONFIG
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
from `MODEL` and `STRONGER_MODEL` included); the loader alone then
refuses a tree that takes a track from under open cards (a process file
that drops the plan track, or a project moved to a process without it),
since only it reads the open cards (`CheckOpenTracks`, `OpenCardsByTrack`),
and judges such a commit again at every poll from the tree it read once,
logging and raising it only when its lines change, so it loads once the
cards left the track. `config check` says so in its last line. `GET /policy` and the
console's configuration page show the version in force, read-only, each
file linked at its commit.

A task's workflow pins, at its start, the version in force
(`WorkflowConfig.PolicyVersion`). Read from the pinned version: the
session cap, the agent time and the stronger rung (captured by the
workflow); the task's process (its first activity, `TaskConfig`, from
change id `processes`: the row's process, or for a card with none of its
own (a roll, a factory pull request's card) the process of the task's
project, with the row's track, its stages, merge queue and dependency
block, and the session mode of the track's class, or of a report task's
own class, which chooses the program: `code` the lifecycle, `report` the
screening, `plan` the plan loop); and in every session, report session,
gate-supervisor call and model review the agent time, `steps`, the check-in limits,
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
(2h, the wall-clock cap across a task's sessions, at most 72h),
`supervisor_calls` (6, decides nothing since plan E, kept so a pinned
policy loads), `checkin_calls` (20, also when absent: the model calls of
one check-in conversation, the last without tools), `checkin_share` (10,
also when absent: the percent of the card's budget all its check-ins may
spend), `supervisor_timeout` (6m per supervisor or review call, at most
15m), `review_diff_kib` (48, also when absent, at most 1024: the size of
one part of the diff a model review reads, one call each; the audit reads
parts of 48 KiB),
`stronger_after` (3, the session cap, so a stage's returns never
climb a card, and 1 for a medium or large card, whose second return from
the same stage takes the stronger rung) and `steps` (1500, 200 when
absent: the model turns of one agent session, the worker's `STEPS`), `nudge_after` (3)
and `nudges` (1), also when absent, `nudge_after` below `same_call` (4):
that many identical tool calls in a row, or `same_call` of them scattered
over the session (same tool, same input, from the session's event stream), get the
running session the message "You repeated the same call three times
(<tool>). Change approach, then continue." and a `nudge` timeline step,
once per repetition, counted only when the session took it; the next such
run trips `same_call` to the check-in. A harness without messages gets the redirect with that text,
decided in code. The activity bound of `RunSession` is the agent time plus a minute, of
`SuperviseGate` the supervisor timeout plus two, unless `bounds` names
them.

The policy's other sections, schema v1: `budget` (`task_default_eur`, `task_max_eur`,
`day_eur` for the UTC day and `month_eur` for the UTC calendar month over
all tasks, `session_floor_cheap_eur` for the least free budget a session on the cheap rung starts on,
`session_floor_frontier_eur` for the same on the frontier rung, both 0 when absent, and
`premium_month_eur` for the calls on the `premium` class per UTC calendar month, no cap of its own when absent) and `classes` (per capability
class the model aliases it may call, its tools with per-task caps, and
the aliases factoryd calls on its tasks' account, which the agent may
not: `review_model` for the model review, `supervisor_model` for the
gate supervisor, `checkin_model` for the check-in (else `review_model`), `audit_model` for the audit; a class without one gets no such call;
a plan or report class may name `session_model`, one of its models, which its sessions run on instead of the stronger rung).
The text classes are `small`, `big` and `premium` (rulings 328 to 330): `premium` is a pay-per-token model
whose spend is real money, so `premium_month_eur` caps it apart. A premium call that cap cannot fit with its
whole output runs on `big`, its audit row saying so, and a premium call the provider fails LiteLLM's router sends to `big`; every key
minted for a list holding `premium` also holds `big`. The console's status strip shows the premium spend this
month against the cap while the cap is set.
The gateway never reads the files, only the version in force in
Postgres. `day_eur: 0` stops all spend; `month_eur: 0` does too, but
only in a commit that also lowers every project's `budget.month_eur` to 0
(a project cap looser than the policy's is refused). A project's
file may cap its tasks together per UTC calendar month with
`budget: {month_eur: N}`, 0 to the policy's `month_eur` (0 stops the
project's spend); without it the project has no cap of its own. A call
the project's month cannot fit is refused `refused_project_cap`, naming
`projects/<name>.yaml`, and a session that pushed nothing after such a
refusal (or a `refused_month_cap`) waits on a human, who raises the cap
by a commit and answers. Because the decoder is strict,
a new key lands in infra first and the factoryd that reads it rolls
right after: the running factoryd refuses the commit, keeps its version
and files the issue, and the new one loads the head at boot.

The gateway checks a model call in this order: the caller (a sandbox's
JWT, or factoryd's `gateway-factoryd` token with its `X-Factory-*`
headers, which are read only behind that token), the task status (an
agent's task running; factoryd's running or in review, or waiting on a
person for a card's brief, checked with the hold), the alias (an agent's must be in its classes' `models`, factoryd's
must be the class's alias for the purpose; else 403 `refused_capability`),
the task headroom (else 402 `refused_budget`), the day headroom (else
402 `refused_day_cap`), the month headroom (else 402
`refused_month_cap`) and the project's month headroom (else 402
`refused_project_cap`). The agent's key names the class's `models`
only; a purpose's call rides factoryd's token.

Intake gives every agent task the capability class `coder`
(`tasks.required_capabilities`; a one-shot backfill gave it to older
agent tasks) and the budget `task_default_eur`, or the requested budget
capped at `task_max_eur`.

The caps hold on the call itself. Prices per alias come from LiteLLM's
`GET /model/info` (the figures it bills with), read every 5 minutes: per
alias the maximum over its deployments of every per-token input rate
(base, long-context `*_above_*_tokens`, service tiers, cache writes,
`tiered_pricing`), every output and reasoning rate, and `max_output_tokens`.
An OpenAI deployment's rates no gateway call reaches are left out: the service tiers (flex, priority,
ultrafast) and the batch API's, the 12-hour cache's, web search's and, without `data_residency`, the
regional uplift; the gateway forwards none of the options that reach them (`service_tier`,
`prompt_cache_retention`, `web_search_options`). The OpenAI project must therefore stay on the default
service tier: a project whose default tier is priority or flex bills at a rate no hold counts.
An alias's output limit is the policy's `prices.max_output_tokens`, lowered
to that published maximum when it is less; the tree's check refuses a
policy without one for an alias a class or the chat calls.
A deployment with any other cost field set (per query, second, image,
character) or without an input and output rate makes its alias unpriced,
and an alias the policy allows but LiteLLM cannot price, or without an
output limit, is refused (503 `error_price`, logged as `PRICE MISSING`). While refreshes fail the last
table is used for up to three TTLs, then every call is refused. A call's input is bounded by its body's byte length (a
token covers at least one byte). Under one Postgres advisory lock, in one
transaction, the headroom left after spend and every outstanding hold is
computed (task: budget minus spent; day: `day_eur` minus today's ledger
total; month: `month_eur` minus the ledger total since the 1st, UTC;
project: its file's `budget.month_eur` minus the month's ledger rows of
the tasks of its repository and their outstanding holds; premium, for a
premium call under `premium_month_eur`: the cap minus the month's ledger
rows and outstanding holds on `premium`; standing lines
count for none), the output limit (`max_tokens` or `max_completion_tokens`,
at most the alias's, the alias's when the caller set none) is
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
A sandbox's model and tool calls are held and booked under their
session (`holds.session`, `ledger.session`): the gateway reads it from
the pod the call's projected token is bound to, `worker-<session id>`,
which the API server signs into the token; factoryd's calls and every
other row have none. A model call's row also keeps when its hold was
taken (`held_at`, its start) and the output it asked for (`want_out`).
Two floors read this math before an attempt, at the free task, day,
month and project headroom (ruling 322, roadmap 243). The attempt's
(`AttemptFunded`: every attempt of a run from before the session ladder)
needs a call of no prompt and 1024 output tokens. The session's
(`SessionFunded`: every session's start, the console's answer and its Fund action) needs a session's opening:
every call that started before its first one ended, each priced as the
hold prices it for the task on its own alias with its asked output,
summed, the largest of the newest 20 sessions' openings on the alias (on
any alias when none is recorded on it, a call on its session's first
call's alias read as on this one; the attempt's call on a ledger
without one), and at least the floor of its rung (`session_floor_cheap_eur`
or `session_floor_frontier_eur`). A
refusal names the budget or the cap that bounds it; only the budget's
offers Fund. The day cap's refusal waits for no human, since the cap
resets on its own: the console takes the answer, and the workflow's
grant waits for the day as a session the day cap refused does.
Settlement leaves a task alone while it has an open hold. Output limits bound
reasoning: verified on the earlier reasoning models (reasoning counts inside
`max_tokens`); Qwen's thinking
does not, so the dashscope deployment runs with thinking off, and the
gateway refuses a client's `thinking` field. `holds` has kinds `model`,
`tool` (see "Tools") and `resource`. During a superseded key's grace (the
calls still in flight on it when a new run or policy version mints its
replacement), a task briefly has two live keys; the holds cap both. LiteLLM's
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
  The session trips its supervision with an escalation, no check-in
  (one arriving during a check-in cuts the conversation and wins over
  its answer): the worker pushes its work in progress, the task
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
  `poster_comment`, signal `poster_comment`), and so does a writer's.
  From change id `answer-burst` on (roadmap 237), that comment opens a
  burst, as an escalation's answer does: the wait resumes once the
  answering party has posted nothing for 30 s, at the latest 3 minutes
  after the first comment, with every comment of the burst, so an answer
  in parts lands whole. The console's answer box posts one comment and
  names it whole (signal `answer_whole`), which resumes the card at once.
  A plan waiting on its approvers takes a writer's comments the same way
  (change id `plan-burst`): the planner goes back once with every part.
  The next session's brief carries it under "Poster information", as
  information, never as instructions. The brief writes its own lines
  (repository, the author's standing) first and puts the issue's text and
  every answer between markers with a random tag per brief, so the author
  cannot forge the standing line or the brief's sections. An escalation's answer stays a
  writer's. 14 days without an answer: `CloseNotReproduced` comments "not
  reproducible, no answer in 14 days", closes the issue, and the task ends
  `closed` (a final state like done and failed). A healed run waits for
  what is left of the 14 days.
- `deployment_health` (`deployment`, optional `namespace`): free, listed
  as `deployment_health: {}`, and offered only to a card whose project
  lists namespaces (`policy.OffersTool`): a project that runs nothing in
  the cluster, such as a library or an app on its own server, never sees
  it, in the agent's tools or in the chat, and a call is refused. A Deployment of the task's project as the
  cluster reports it now: its replicas, the ReplicaSets that hold pods
  (the older ones by number), each pod with every container's restarts,
  state and last termination (reason, exit code, time), and the
  Kubernetes events of the last hour for the Deployment, its ReplicaSets
  and their pods, gone ones too (at most 40, the text cut at 12 KiB). No
  log and no Secret. The gateway checks the names and posts the read to
  factoryd's `POST /internal/deployment-health` (bearer `gateway-factoryd`,
  `{task_id, deployment, namespace}`), which answers `{"text"}`. The
  namespace must be one of the project's; left out, factoryd answers from
  the first of them that holds the Deployment. A refusal is one sentence
  that names what to change (400 a bad name, 403 a namespace not the
  project's or a read the cluster forbids, 404 no such Deployment, 409 a
  final task or an inactive project, 502 the cluster did not answer), and
  the agent reads it as the tool's error. Left out, a namespace that
  refuses or fails does not stop the search; it is the answer only when no
  other holds the Deployment. The console's project agent has the
  same read as its tool `deployment_health`, for the turn's project.
  factoryd reads through ClusterRole `factoryd-project` in a registered
  project's namespaces and through `factoryd-health`, which holds those
  reads alone, in a seed project's (bound in `factory` by
  `k8s/factoryd.yaml`, in `platform` and `forgejo` by the infra
  repository). `factoryd-project` also carries registration's Secrets and
  is never bound in a seed namespace.
- `analyze_image` (exactly one of `path` and `card_image`, an optional
  `ask` of at most 2000 bytes): free, listed as `analyze_image: {}` by a
  class that names `vision_model` (the loader refuses one without). The
  look is one call of the class's vision alias on the card's account
  (purpose `vision`, the chat's door): it books as a model call and the
  card's budget caps it. The gateway checks the arguments and posts the
  look to factoryd's `POST /internal/image-look` (bearer
  `gateway-factoryd`, `{task_id, path, card_image, ask}`), which answers
  `{"text"}`, the image's name and what the model saw. A `card_image` is
  an image attached to the card's issue, its comments, or its parent
  card's issue: a `/attachments/` uuid in it picks that one, else its name
  with or without the extension; two of one name refuse with their links.
  factoryd reads it with its Forgejo login from `/attachments/{uuid}`. A
  `path` is a file of the running session's workspace, absolute under
  `/workspace` or relative to the repository, which factoryd asks the
  pod's worker for (`POST /image` under the session's message bearer).
  The worker opens it through the workspace's `os.Root`, so a symlink that
  is absolute or leaves the workspace refuses at the open itself, and a
  FIFO or other non-regular file refuses unread. The image must sniff as
  PNG, JPEG, WebP or GIF of at most 8 MiB. A refusal is one sentence (400,
  403 outside the workspace, 404 no such image, 409 a final task, no live
  session, a session's worker from before the read, comments past 1 MiB,
  a class without `vision_model`, or a refused hold, 413, 415, 502 a read
  or the model did not answer), the agent's tool error.
- `request_tool` (`tool`, 1 to 80 characters, and `need`, 1 to 600; both
  required, each one line without control characters): free, through the
  class's tool list (`request_tool: {}`), never a tool card's session; a
  report session's brief does not name it, and factoryd refuses it there
  ("this session cannot wait for a new image: finish with what the
  sandbox has").
  The agent asks for a tool its sandbox lacks instead of installing it.
  It goes the way of an escalation (an `agent_tool_request` intake event,
  the session stops with outcome `missing_tool`, the work in progress on
  the branch); from change id `tool-park` on the workflow's `ParkForTool`
  parks the card on an image: the card's own, else one named after the
  tool; a card naming image `default` names none. A tool card on the
  project's repository (source `tool_request`, small, the cheap rung, a
  child the asking card depends on) adds the tool to
  `factory/images.yaml`, starting from the asking card's image as an
  entry (its own on main, or `FactoryStack`, the factory image's Go and
  Node, where it runs on the factory's image); a second ask for the same
  image joins the open card with a comment while no session has started
  it, else files its own. The project agent's `request_image` files and
  joins the same cards (`Runner.RequestImage`), with no card that waits.
  The tool and the need reach Forgejo through the
  outbound sanitiser, as an escalation's text does. The park's row names
  the tool card before it is filed, so a retry files it once. The card
  waits in `waiting-image` (no work slot) until the imager's
  `image_ready`: once the tool card merged and the pin in force carries
  the key a pass after the merge read, the next session runs on the
  image from the branch, its findings naming the tool's arrival. A tool
  card closed unmerged, merged without declaring the image, or merged
  with an image whose three builds left no signed image, reaches the next
  session as why the tool did not come; so do, at once, a new image past
  the project's four, an ask on the configuration repository, and, as a
  backstop, a tool card's own ask (it declares tools it never gets). A
  park that ends without its image drops the card's dependency on the
  tool card, in `task_deps` and on Forgejo, so the card's issue can
  close; a drop that still fails after its retries blocks nothing, and
  the card's timeline notes it. Any ask after a refused park goes to a human, and so does an ask
  for the tool whose image arrived from a card on that image; a card not
  on it takes it at once. After 24 hours without a wake the card asks a human; the
  image's arrival ends that wait too. The asking session takes no number
  of the session cap. A session start whose image has no pin in force
  parks the same way, with no tool card; a reproduction, criteria or
  hold-out sandbox whose image has none is the start escalation at step
  `image pin`. A pull request that changes
  `factory/images.yaml` has the file checked at the gated commit; each
  finding goes back to the agent like an anti-cheat finding.

  The imager reads every project's `factory/images.yaml` but the
  configuration repository's. A pin at its desired key whose build the
  store lacks signed, as after a re-seed, is built again; a verified
  build is read again after an hour, so a store lost while factoryd runs
  is noticed too. After three builds of a key leave no signed image, one
  alert names the builder, and the key is asked for once a day; a build
  that lands forgets the key's failed dispatches. A dispatch Forgejo
  refuses for 45 minutes raises one alert too. The builder unpacks every
  upstream file as root in a stage of its own, under `/stage`, and the
  image takes `/opt` and `/usr/local` from it by `COPY` after its apt
  step, with nothing run after: a symlink one upstream line plants cannot
  carry another line's write outside those directories. A merge Forgejo refuses with 405 while
  the head's checks are pending waits; a refusal that repeats, or a
  finding, raises one alert per project and cause.

  The imager keeps the store to the pins. Once a pin changes in force, the
  store keeps of that image only the pinned build, the one pinned before
  it and the build of the key `factory/images.yaml` asks for now, which a
  pin pull request may carry: each build's tag, cosign's `sha256-<hex>`
  signature tag and the manifests their indexes name. A version pushed in
  the last 45 minutes, a build in flight, stays too. Every other version
  of `sf/sandbox-<project>.<image>` goes, through Forgejo's package API
  with the site admin's login. A name that leaves the pins keeps its last
  build for 30 days, so pinning it again undoes an unpin by mistake; then
  every version goes. Nothing is listed or deleted while infra's main is
  past the commit in force, as a pin merged there may not be loaded yet,
  nor deleted once the version in force moves during a sweep. Once a day
  per image, its row keeping when so a restart checks no sooner, an image
  other than `default` whose name has been pinned for 30 days, and which
  no card of the project named or ran on in those 30 days, gets a removal
  card on the project's repository (source `image_removal`, small, the
  cheap rung) that deletes its entry from `factory/images.yaml`, or the
  file where it is the only entry; the row names the card before it is
  filed. No other is filed
  while that card is open or within 30 days of its end. `remove-project`
  deletes the project's `sf/sandbox-<project>.*` packages and its image
  rows.

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
`running` or `in-review` (the gates' model review), and for factoryd's
`brief` call on a `waiting-human` task, the one text its card's person
reads first: `TakeHold` refuses any other status with
`refused_task_status` in its own transaction. Prices not in any API
response live in the policy's `prices`. A refusal (`refused_capability`,
`refused_task_status`, `refused_arguments`, `refused_price`,
`refused_tool_requests`, `refused_tool_eur`, `refused_budget`,
`refused_day_cap`, `refused_month_cap`, `refused_project_cap`) comes back as an MCP tool error that says the policy
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
interlock of ruling 123 (see [the untrusted runner](operations.md#the-untrusted-pull-request-runner-ruling-78)) guards every
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

