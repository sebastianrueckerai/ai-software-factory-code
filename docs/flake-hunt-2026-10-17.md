# Flake hunt: internal/factoryd race suite, 2026-10-17

Question (issue a33247da): how flaky is `go test -race ./internal/factoryd/`
really? CI run 1402 at 078d004 was cancelled by design, but the standing
theory before that cause was named was a flake, and nothing had measured the
suite. This file is the log the issue asks for: every run, its seed context,
its failing tests, what repeated, what was fixed, and the verdict.

## Method

- Base commit under test: 0325bbc (`git rev-parse HEAD`), the tree the issue
  was filed against.
- Each run: `go test -race -p 1 -count=1 -shuffle=<seed> ./internal/factoryd/`
  against a throwaway PostgreSQL 17 server on a unix socket
  (`FACTORYD_TEST_DATABASE_URL`/`GATEWAY_TEST_DATABASE_URL`), the same shape
  `scripts/pg-test.sh` gives CI's `make test`.
  - `-count=1` defeats Go's test-result cache, which would otherwise turn
    repeats into one execution.
  - `-shuffle=<seed>` shuffles test execution order and is recorded below as
    the run's seed context; `-p 1` matches CI.
- Two lanes ran concurrently (runs 2-9 and 10-19), each against **its own**
  throwaway Postgres, so no run shared a database, port, or Temporal
  environment with another; CPU contention between lanes is the only
  difference from a solo run, and it makes timing tighter, not looser.
- Run 0 was an unseeded warm-up run (`go test -race -p 1 ./internal/factoryd/`)
  that also primed the build cache; it is counted in the twenty.
- A fresh `-shuffle` seed was drawn per run; the seed is printed by `go test`
  as `-test.shuffle <n>` and recorded here.

## The log of every run

| run | started (UTC) | ended (UTC) | seed (-shuffle) | verdict | failing tests |
| 0 | 2026-10-17T05:41Z | 2026-10-17T05:43Z | off | pass | — |
| 1 | 2026-10-17T05:46:13Z | 2026-10-17T05:48:18Z | 162392085 | pass | — |
| 2 | 2026-10-17T05:51:24Z | 2026-10-17T05:54:03Z | 166798611 | pass | — |
| 10 | 2026-10-17T05:51:35Z | 2026-10-17T05:54:10Z | 267237479 | pass | — |
| 3 | 2026-10-17T05:54:03Z | 2026-10-17T05:56:39Z | 498224851 | pass | — |
| 11 | 2026-10-17T05:54:10Z | 2026-10-17T05:56:45Z | 821093817 | pass | — |
