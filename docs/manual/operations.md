# Operations

Build and test machinery, the pull-request runner, telemetry, factoryd's own network, day-to-day operation and shipping.

## Build and test

    go build ./...
    make test

`make test` runs `scripts/pg-test.sh go test -race -p 1 ./...`, the line
`pr.yml` and `ci.yml` run, and `bash check` runs it after vet, gofmt and
gosec (the runner image's `GOSEC_VERSION`). `bash check static` stops
before the tests. `.githooks/pre-push` runs `bash check static` on the
commit a push to `main` carries and refuses the push on a finding, so a
push from a clone with the hook on cannot turn `main` red on vet, gofmt or
gosec (`git push --no-verify` skips it). Turn it on once per clone:
`git config core.hooksPath .githooks`. The store tests run only with `FACTORYD_TEST_DATABASE_URL` and
`GATEWAY_TEST_DATABASE_URL` set to a throwaway Postgres (they wipe it):
`scripts/pg-test.sh` starts one (PostgreSQL 17's `initdb`, `pg_ctl` and
`postgres`, on `PATH` or in `/usr/lib/postgresql/17/bin`; the runner and
sandbox images carry them, main CI installs them) in a temporary
directory, listening only on a unix socket there, sets both variables to
its `postgres` database, runs the command and removes it all again.
Each package behind `FACTORYD_TEST_DATABASE_URL` migrates its own database
`factory_test_<package>`, dropped and created empty at its first test
(`internal/schema/schematest`), so the packages run in parallel; that
URL's user must be allowed to create databases. The API-server tests
run only with `FACTORYD_APISERVER` (host:port of a throwaway k3s),
`FACTORYD_APISERVER_SA` (factoryd's token and the CA, k8s/factoryd.yaml's
RBAC and admission policies applied) and `FACTORYD_APISERVER_ADMIN` (an
admin's token; for the cache tests also the CA, and the cluster runs
infra `k8s/platform/local-path` with its `factory-cache` class). The
revocation test runs only with the `FACTORYD_LIVE_*` variables set
against the live cluster. CI (`.forgejo/workflows/ci.yml`) runs on push
to protected `main` only, on the privileged dind runner: it copies the
outside images into the store, tests, then
builds the factory image, the sandbox image and the CI runner image
into the Forgejo registry. Every image the factory uses and does not
build is a line of `upstream-images.txt`, pinned by its linux/amd64
digest (roadmaps 203 and 242); the `upstream` job runs
`scripts/upstream-copy`, which copies each line the store lacks to
`git.aisoftwarefactory.world/upstream/<its name>` with that digest and
tags it with its upstream tag as well, and every Dockerfile, workflow and
infra manifest names the copy. The nodes' registry mirror (infra
`talos/patches.yaml`) serves the lines marked bootstrap, Talos's own
images and Cilium's from the store too, by digest or by that tag, and
 falls back to their upstreams while the store does not answer. The
 Dockerfiles take their bases from the store through `ARG UPSTREAM` (a
 local build without a login sets it empty for the same digests at their
 upstreams). The file inputs come from the store too (roadmap 203,
 step 2): every `ADD` download and the build VM's buildkit tarball read
 the store's copy of an `upstream-files.txt` line at the line's sha256
 (`scripts/upstream-files` fills them), each Go build — the CI test job,
 the images' builder stages, the build VM's — restores the store's
 `go-modules` tarball of its `go.sum` and runs with `GOPROXY=off
 GOFLAGS=-mod=mod`, and the sandbox image's `npm ci` runs `--offline`
 against the store's `npm-cache` tarball of the plugin lockfile, so no
 build reaches an upstream; a missing or wrong store copy fails it. A
 pull request's image build pulls from the store too:
`pr.yml` writes the job token as buildctl's login, and the build runner's
egress policy admits the edge gateway (entity `ingress`, 443), where
buildctl fetches the VM's registry tokens. Debian
packages come from snapshot.debian.org at one `DEBIAN_SNAPSHOT` and
PostgreSQL's from apt-archive.postgresql.org at exact versions. Nothing
moves on its own: the code project's routine `weekly-bump` files a
human card on this repository every Monday 06:00, "Weekly bump of the
pinned outside inputs", and holds while one is open (its project file and
brief `routines/weekly-bump.md` in the configuration,
`internal/factoryd/routine_run.go`), naming the list,
the snapshot, the PostgreSQL versions, the Go patch and the downloaded
tools; a human's, since no agent sandbox reaches a registry or a
release page. Each build imports and exports (`mode=max`)
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
node, the Go toolchain, gosec, govulncheck (go.mod's tool) with the
store's copy of its database at `GOVULNDB_VERSION` (CI passes `-db`, so
no job reaches vuln.go.dev), PostgreSQL 17's server for
`scripts/pg-test.sh`, `buildctl` and `crane`, and nothing that builds
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

The four task metrics below also carry `process` and `track` from the
task row (`none` where it holds none: a human-lane task, the console's
alert verdict click, a roll's process); a report task carries its
parent's.

- `factory.task.outcome{project,state,model_alias,attempts,merge_rule,process,track}`:
  once per task on every path that ends it (`Complete`, which `Finish`
  and the heal pass's failure of a malformed task go through, a closed
  backlog card, a closed human-lane issue, a resolved config task), so it
  equals the tasks turned done, failed or closed. `state` is that final
  status. `project`
  and `merge_rule` come from the repository's project file (the repository
  and `human` without one), `model_alias` is the alias of the task's last
  session (`tasks.model_alias`, set with each `sessions` row; `none` for
  a task that ran none: a roll, a human-lane task), `attempts` its sessions.
- `factory.task.cost{model_alias,outcome,process,track}`: the task's `spent_eur` at
  `Finish`, in EUR, buckets 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5 and
  10; `outcome` is `merged` for a done task with a pull request, else the
  final state. Spend settled after `Finish` (LiteLLM's late batches) is
  not in it.
- `factory.review.verdict{verdict,strict_improvement,round,process,track}`: every
  model review answer, `verdict` approve, concerns or `none` (refused,
  failed or invalid), `strict_improvement` true, false or `unset`, `round`
  the review's round on the task. A roll's review asks no model and is
  not counted.
- `factory.human.intervention{kind,process,track}`: from the Forgejo intake when its
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
`sk-` keys and any run of 32 or more key characters that holds a digit, a
`+` or an `=` and is no commit id, sha256 digest or lowercase path become
`[redacted]` (Postgres keeps the whole text); and the
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
(`small`), the gateway's alias.


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
- While a plan of the code repository is open on `main`, factoryd keeps
  the Flux Kustomization `flux-system/factory` suspended, so no change
  under `k8s/` goes live; it resumes it when the hold ends and while a
  roll or a Deploy now goes out. Every roller poll sets the level, so
  while factoryd runs it undoes a hand suspend or resume within 15
  seconds: before suspending a Kustomization by hand (a shutdown's
  `flux suspend kustomization --all`, an incident), scale factoryd to 0
  with `kubectl -n factory scale deployment factoryd --replicas=0`. A
  failed suspend is logged and noted on the hold's ask, and the hold goes
  on; a failed resume stops the roll. If factoryd is down while a hold
  stands, a manifest fix needs
  `flux resume kustomization factory -n flux-system` by hand.


## Ship

CI pushes images by digest. A deploy is a commit that rolls the digests
in `k8s/factoryd.yaml` and `k8s/deployment.yaml` (factoryd, the gateway,
`WORKER_IMAGE`, `SANDBOX_BASE_IMAGE` and `CI_RUNNER_IMAGE`), and factoryd opens it as a task of
source `roll` (ruling 178). Every minute it reads the code repository's
push runs; for the green runs of `main` it has not judged, newest first
until one files or folds the roll (at its first judgement the newest
only, and never one older than a run whose commit a roll carries, which
would roll `main` back), and only while `main`'s head, the commit of its
newest push run, has a green `ci / test (push)`: the roll's branch is cut
from that head, so its pull request's checks judge the head's code, and a
roll cut while the head's tests run or fail fails for code not its own
(roll #1219). While the head's tests run or fail, the roll waits and says
so once in the log. It skips a commit that changes nothing outside
`k8s/` against its first parent (a roll's own merge, a hand roll: rebuilt images, whatever
the build cache made of them), and otherwise reads the images main CI tagged with the commit's short sha
from the registry (in-cluster Forgejo, `/v2`, as user `factory-registry-read`
with its `read:package` token from secret `forgejo-registry-read`, which
cannot overwrite a tag, read once at boot, so a rotation restarts
factoryd; the `forgejo-registry` pull secret serves image pulls only) and compares each with the one `k8s/` pins by the
linux/amd64 image manifest inside the index: main CI's indexes carry a
provenance attestation with build timestamps, so a rebuild of unchanged
inputs pushes a new index digest around the same image. A pin the
registry does not hold (a seed, a digest the store collected) counts as
changed. No changed image:
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
roll watcher records the rows. The merge waits for a session gap (ruling
269; the card says so): factoryd runs one replica, so its restart stops
every running session. `AwaitSessionGap` holds new sessions (a
`roll_holds` row) and polls every 15 s until no session runs in factoryd,
the roll's own aside, or until the longest agent time in force (limits,
classes, processes, projects) plus 30 minutes, when it merges anyway and
logs so; steps `roll_waiting` (the live tasks) and `roll_gap`
(`waited_s`). Every code and plan session first waits in `AwaitRollHold`
while a hold is in force, before its agent time starts (one `held` step;
the card's chip "waiting for a roll"); report sessions are not held, only
waited for. The hold ends when the merge ends unmerged and when the roll
ends unmerged; a merge keeps it five more minutes, past the restart Flux's
next minutely look at `main` brings, and a factoryd that just started
waits three minutes before it believes a gap, until the sessions its
restart stopped are retried. One open roll per repository: newer images
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

While a plan of the code repository is open on `main` (from its first
merged card until its `plan_closed`; a reverted card does not count), the
roller opens no roll and keeps the Flux Kustomization `flux-system/factory`
suspended, so no change under `k8s/` goes live either (Operating it). It logs
`roller: plan #N is open on main: no roll until no plan is open on main`
once per commit and posts `factory/roll` `success` with a description
starting `held:` on `main`'s newest commit it has not judged, so a merged
card still ends `done`. During a hold that status does not block the
commit: its CI runs decide, and green then means green on `main`, not
live. A commit whose roll is open or failed keeps its `pending` or
`failure`. The commit an approver released with Deploy now rolls. Once no
plan is open on `main`, the next poll rolls every commit since the last
roll. A Retry the roll answered during a hold resets the failed attempts;
the fresh roll starts once no plan is open on `main`. A red `main` pauses
the queue, re-runs its failed jobs and files its card during a hold as at
any other time.

`kubectl rollout restart deployment/factoryd` restarts it twice: Flux
reverts the `restartedAt` annotation about five minutes later and rolls
it again. Both restarts are safe (Temporal retries the interrupted
activity), but plan for the second one: factoryd undoes a hand suspend
of the `factory` Kustomization within 15 seconds (Operating it).

Order when a change spans them: the infra policy first (new keys before
the factoryd that reads them), then the gateway, then factoryd once the
gateway is Ready. factoryd's model calls go to the gateway, so a new
factoryd against an old gateway gets `refused_auth` on them (spurious
escalations and review notes, nothing merged); a new gateway serves an
old factoryd unchanged. The gateway reads the policy version at most
30 s old, so a call in the first 30 s after a new version can still be
judged under the previous one.

