# internal/factoryd flake record: CI run 2518, 2026-11-09

The `pr / test` check of the pull request at 08ec15f4 (branch
factory/t-64461e3a26b9, changes only acceptance/console_rig_test.go and
acceptance/documents_test.go) failed in CI run 2518 with
`FAIL github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd
273.882s`; every other package passed. The run's available log tail stops at
that package FAIL line, so the failing test's name is not in it, and the run's
full log is not reachable from a sandbox (the Forgejo HTML route answers 404 to
an unauthenticated fetch and its API 401s the gateway's token). This file
records what run 2518's failure could and could not be reproduced as, per the
convention of docs/flake-hunt-2026-10-17.md.

## CI conditions reproduced

pr.yml runs the PR's packages without `-race`, all in one
`scripts/pg-test.sh go test` call beside a throwaway PostgreSQL 17
(FACTORYD_TEST_DATABASE_URL/GATEWAY_TEST_DATABASE_URL). Two runs, both on this
sandbox (two CPUs) at 2026-11-09:

| run | command | verdict | factoryd time |
| --- | --- | --- | --- |
| A | `scripts/pg-test.sh go test ./internal/factoryd/ -count=1` | ok | 265.314s |
| B | `scripts/pg-test.sh go test -count=1 ./...` (the full suite, CI's shape under parallel load) | ok, every package | 371.339s |
| C | `bash check changed` (this file in the tree selects the full suite, `-p 2`) | ok, every package | 326.989s |

For scale: a local `go test ./internal/factoryd/ -count=1` without the Postgres
environment passes in about 119s, so run 2518's 273.882s is the suite's normal
Postgres-backed duration (runs A and B bracket it: 265s alone, 371s under the
whole suite's load), not a timeout.

## Verdict

- The failure did not reproduce: three runs under CI's exact conditions, two
  of them the full suite under parallel load and slower than run 2518 itself,
  all green. The one-off FAIL of run 2518 is a flake; the failing test's
  name stays unknown until a full CI log of that run can be read.
- The suite measured 0/22 flaky on 2026-10-17 (docs/flake-hunt-2026-10-17.md,
  under `-race -p 1`); this is the first recorded factoryd failure since.
- Action: rerun the check — this file's own commit carries no code change, so
  the pull request's next `pr / test` run is the rerun. If factoryd fails
  again, its log names the test this record could not, and that test's timing
  assumptions are the place to look.
