# [[.Name]]

A TypeScript single-page frontend and a Go API, built into one image. The
factory seeded this repository from its template `ts-go`; its CI
(`.forgejo/`) is fixed and relies on the layout below. Agents cannot change
`.forgejo/`, so keep to these conventions: a change that breaks them turns
the checks red, and no merge happens without green checks.

## Layout

- The Go module is at the root: `go.mod`, module `[[.Image]]`.
- `go.mod` carries `ignore ./web/node_modules` (the directive Go 1.25 has),
  because `go vet ./...`, `gosec ./...` and `go test ./...` would otherwise
  walk `web/node_modules`, where some npm packages ship `.go` files.
- `cmd/server` serves the API under `/api/`, `/healthz`, and the
  single-page app from `web/dist` under `/app/`.
- `web/dist` is embedded with `//go:embed all:dist` in package `web`
  (`web/embed.go`), with `web/dist/.gitkeep` committed so the embed
  compiles before a build.
- `web/` is a Vite, React and TypeScript app whose `package.json` has the
  scripts `lint`, `test` (vitest) and `build`, with `package-lock.json`
  committed. The checks install with `npm ci --ignore-scripts`: no install
  script runs in CI.
- Migrations are named `<UTC yyyymmddhhmmss>_<slug>.sql`, the time the file
  was written, slug `[a-z0-9_]+`, never by the next number. The checks
  refuse a new `.sql` file under any `migrations/` directory named
  otherwise.
- Acceptance tests live in `acceptance/`: plain Go tests over `httptest`,
  run by `go test ./...`.
- One `Dockerfile` at the root builds `web` in a pinned Node stage and the
  Go binary in a pinned Go stage, and serves 8080. Pin each base image by
  digest (`node:24-bookworm-slim@sha256:...`).
- `deploy/base/` holds the workload, `deploy/staging/` sets the namespace
  and the image digest, which only the CI's roll commits change. A
  Deployment that mounts a ReadWriteOnce volume uses `strategy: Recreate`,
  since a rolling update would stall on the second pod's attach.

## What the checks run

Pull requests (`.forgejo/workflows/pr.yml`), in this order:

1. In `web/`: `npm ci --ignore-scripts`, `npm run lint`, `npm test`,
   `npm run build`. Skipped while there is no `web/package.json`.
2. `go vet ./...`, `gofmt` on every tracked `.go` file, `gosec ./...`,
   `go test -race ./...`. Skipped while there is no `go.mod`.
3. The names of added migrations.
4. With a `Dockerfile`: the image builds.

Deleting `web/package.json`, `go.mod` or the `Dockerfile` once they exist
turns the checks red. On `main` (`.forgejo/workflows/ci.yml`) the same
checks run; with a `Dockerfile` the image is pushed by digest and staging
rolls; with `deploy/prod/kustomization.yaml` prod follows after 15 minutes
on staging without an alert.
