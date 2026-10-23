# Security policy

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository
(Security → Report a vulnerability). Do not open a public issue, and do
not report a vulnerability only in a pull request.

## Scope

The code in this repository: the control plane, the gateway, the agent
worker and their tests. A vulnerability in a dependency belongs to that
dependency's upstream; this repository pins its inputs and runs
govulncheck in CI against a pinned database, so a fix here is an
ordinary dependency bump.

## Supported versions

The `main` branch only; there are no releases to backport to.
