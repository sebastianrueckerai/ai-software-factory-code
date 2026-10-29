# Binaries

- `cmd/factoryd`: the control plane. Task intake (`POST /tasks`,
  `GET /tasks/{id}`, `GET /tasks/{id}/timeline`, bearer token from secret
  `factoryd-intake`, budget from the policy, see [Policy and costs](policy.md#configuration-policy-and-costs)), the
  policy loader and `GET /policy`, `GET /costs`, the task store
  in Postgres, and the
  Temporal worker for task queue `factory-tasks`: one `TaskWorkflow` per
  task (workflow id `task-<id>`) runs the attempts, opens the pull
  request, waits for its checks and for the reviewer. Each attempt
  (`RunSession`) gets a fresh namespace, its own pair of Forgejo tokens
  (read on the repository, write on the bot's fork, nothing else) and
  service account (so its own LiteLLM key), one gVisor pod, supervision
  (wall-clock cap, stall, reading without writing, same failure, same
  call, flip-flop, churn and no-write detectors, the check-in; see [Supervision](gates.md#supervision)) and
  a session record in S3; `sessions/<task>/timeline.jsonl` records the
  task's whole path. Spend settlement against LiteLLM and the orphan
  sweep run beside it. Runs with `strategy: Recreate`; a restart loses
  nothing: a stopping pod stops its running sessions gracefully (the
  worker pushes its work in progress, within the Temporal worker's 120 s
  stop, inside the pod's 150 s grace), and Temporal retries the
  interrupted attempt on the new pod, which continues the chain from the
  heartbeat and the pushed branch. It also
  runs the CI spawner (see [the untrusted pull-request runner](operations.md#the-untrusted-pull-request-runner-ruling-78)) in the same process, on the same
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
  model (403 `refused_capability`). The call is then metered, see [Policy and costs](policy.md#configuration-policy-and-costs): its output limit is clamped so its worst case fits the free
  headroom, the worst case is held, and the real cost is booked after the
  call (402 `refused_budget`, `refused_day_cap`, `refused_month_cap` or
  `refused_project_cap` when it cannot fit). The per-task LiteLLM virtual key is limited to the
  classes' aliases and carries no budget: the holds are the budget, and a
  second one counted by LiteLLM's own estimate refused calls the holds
  admitted (#704). A new run or policy version
  mints a new key and blocks the old one once the calls still using it
  are done. LiteLLM's one budget, the proxy-wide `max_budget`, is the
  backstop; its refusal is `refused_month_cap`. Every decision has an audit row
  naming the policy version and the task's issue author (`audit.author`,
  roadmap 41); an admitted call's row also carries its
  output limit and hold. factoryd's own model calls (the session
  supervisor, the gate supervisor, the model review, the audit) take the same route:
  the `gateway-factoryd` token (the gateway's `FACTORYD_TOKEN`) with the
  task in `X-Factory-Task` and the purpose (`supervisor`,
  `gate-supervisor`, `review`, `audit`, `brief`) in `X-Factory-Purpose`. Such a call may use
  only the alias the task's class names for the purpose
  (`supervisor_model`, `review_model`, `audit_model`; 403 `refused_capability` else),
  runs while the task is running or in review (a `brief` call, a needs-you
  card's brief, also while it waits on a person), is held and booked like an
  agent's under the ref `<purpose>/<alias>` (e.g. `review/big`),
  and its audit row has the purpose as `kind` (`agent` for an agent's
  call, `factoryd` for a factoryd call refused before its purpose is
  known). The `X-Factory-*` headers are read only behind the token: with
  a sandbox's JWT or a wrong bearer they are ignored and the call is an
  agent's. Its LiteLLM key is the task's factoryd slot, apart from the
  session's key, limited to those aliases. A refusal names its decision in
  the `X-Gateway-Decision` header. It also serves the agent's tools over MCP at
  `/mcp`, see [Tools](policy.md#tools).
- `cmd/agent-worker`: pid 1 in the sandbox pod. The pod's first init
  container, `harness`, copies agent-worker, opencode and the opencode
  bake out of the factory's image (`WORKER_IMAGE`) into the emptyDir
  `harness` at `/factory`; every later container runs
  `/factory/bin/agent-worker` with that volume read-only, whatever image
  it runs, and the worker puts `/factory/bin` first on `PATH` and copies
  the bake into its `HOME`. The session pod sets opencode's
  `OPENCODE_DISABLE_*` switches in the worker's env, not the image. The
  next init container (`MODE=clone`), the read token's only holder,
  runs the factory's image whatever image the worker runs, and
  clones the base commit
  from the repository before the agent starts (a resumed task branch that
  only the fork holds is fetched from there). The worker container starts
  with no Forgejo token, runs `opencode serve` on 127.0.0.1 in its own
  process group and drives one session over its HTTP API the way
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
  the agent's commits into a clean repository, commits there (one commit,
  its trailers written by the worker: every commit the factory or the
  console writes carries `Factory-Role` and, as they apply,
  `Factory-Task`, `Factory-Session`, `Requested-by`, the human behind it,
  and a merge's `Approved-by`; `factoryd.Attribution`, roadmap 41), and asks
  for the push token on stdout (`{"type":"push"}`). factoryd mints the
  write token on the fork then and posts it to the worker's
  `POST /push {token}` on port 8091 (same bearer; 409 until the worker
  asks, one token only); the worker waits 30 s at most and pushes
  `factory/<id>` to the fork `factory-agent/<repo>`.
  The sandbox image (`Dockerfile.sandbox`) carries opencode, git, ripgrep,
  the Go toolchain (ruling 99, the same pin as the runner
  image) and PostgreSQL 17's server for `scripts/pg-test.sh`; its stage
  `base`, pushed as `sf/sandbox-base`, is the same without the harness,
  Go and Node, apt's dated sources kept for the images built on it; its
  egress (`workerEgressSpec`) is default-deny plus the
  gateway, the in-cluster Forgejo, the Go module proxy and checksum DB,
  and `storage.googleapis.com` (proxy.golang.org redirects zip downloads
  there) by name on 443, SigNoz's collector pods on 4318 when factoryd has
  an `OTEL_EXPORTER_OTLP_ENDPOINT` (ruling 158: the worker's own spans and
  metrics; the pod gets the same variable), and kube-dns for exactly
  those names; the cloud metadata address is denied outright. The module proxy is a covert
  channel: a `go get` of a made-up module path carries that path to
  proxy.golang.org, so an agent can leak a few hundred bytes per request
  through it. Ruling 99 accepts this as the price of a usable Go
  toolchain in the sandbox. Before the agent starts, or a reproduction's
  command runs, the worker probes that egress (roadmap 42), every probe
  at once and each bounded at 2 s: the gateway's `/healthz` answers,
  while the canary (factoryd's `EGRESS_CANARY`, default
  `example.com:443`), the metadata address and a direct IP
  (`EGRESS_DIRECT_IP`, default `1.1.1.1:443`) take no connection and a
  lookup of the canary's name fails at once; the worker runs as uid
  1000 (`factoryd.SandboxUID`). factoryd refuses at boot a canary on the
  sandbox's DNS allowlist. A gateway or a DNS that does not answer is a
  transient start fault, which the activity's retry starts again; a
  sandbox more open than its policy, or on another uid, ends the start
  at step `egress probe` with an escalation that names the probe and
  what answered.

`internal/` holds the packages behind them, `k8s/` the manifests Flux
applies from this repository (namespace `factory`).

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

