# ai-software-factory-code

The factory's own code. Design lives in the
[`ai-software-factory`](https://github.com/sebastianrueckerai/ai-software-factory)
repository, the cluster in
[`ai-software-factory-infra`](https://github.com/sebastianrueckerai/ai-software-factory-infra).
This is the first repository the agents work on. The factory was rebuilt
from the Stage 17 backups on 2026-10-12; the rebuild recipe lives in the
infra repository README.

## The binaries

- `cmd/factoryd`: the control plane. One Temporal workflow per task runs
  the sessions, the gates and the merge; it also serves task intake, the
  policy and the console.
- `cmd/factory-gateway`: the enforcement point. Every model call is
  checked against the policy in force and metered; it also serves the
  agent's tools over MCP.
- `cmd/agent-worker`: pid 1 in the sandbox pod, the only code that runs
  beside the agent in its workspace.

[Each binary in full](docs/manual/binaries.md).

## Build and test

    go build ./...
    make test

`make test` runs `scripts/pg-test.sh go test -race -p 1 ./...`, the line
CI runs: it starts a throwaway Postgres for the store tests
(`FACTORYD_TEST_DATABASE_URL`, `GATEWAY_TEST_DATABASE_URL`) and each
package migrates its own `factory_test_<package>` database. The
API-server and live-cluster tests need further variables, and the
pinned-inputs machinery (the image store, `upstream-images.txt`, the
snapshot dates) sits behind all of it:
[Build and test in full](docs/manual/operations.md#build-and-test).

`.githooks/pre-push` refuses a push to `main` that carries a vet, gofmt
or gosec finding (`git config core.hooksPath .githooks` once per clone).

## The manual

| Document | What it holds |
|---|---|
| [Binaries](docs/manual/binaries.md) | The three binaries in full: every endpoint, workflow and guarantee, and factoryd's own network |
| [Tasks](docs/manual/tasks.md) | How a Forgejo event becomes a task, and how a task's sessions run |
| [Gates](docs/manual/gates.md) | Checks, policy, review, approval and the merge queue; supervision of a running session |
| [Policy](docs/manual/policy.md) | Configuration, the policy document, costs and budgets; the agent's tools; commissioned resources |
| [Console](docs/manual/console.md) | The console's pages and the chat with the project agent; projects and their files |
| [Operations](docs/manual/operations.md) | Build and test in full, the untrusted pull-request runner, telemetry, operating and shipping |

## Security

Report vulnerabilities privately: [SECURITY.md](SECURITY.md).

## Licence

Source available under the [PolyForm Noncommercial License 1.0.0](LICENSE). Non-commercial use is free as the licence defines it: personal use, research, education, charities and public bodies. Any commercial use, a company's evaluation included, needs a commercial licence: write to mail@sebastianruecker.com. Outside contributions are not accepted yet.

Files copied from other projects keep their own licences: `internal/console/static/htmx.min.js` is htmx, under the Zero-Clause BSD licence.
