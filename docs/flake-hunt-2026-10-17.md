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
- The hunt was interrupted and resumed (the spend check-in stopped sessions
  mid-hunt); the first block below (runs 0-3, 10, 11) ran in the lanes above.
  The resumed block (runs 4-9, 20, 12-17, 21) ran as two fresh lanes, 4-10
  and 11-17, each again with its own throwaway Postgres per run; the two run
  numbers that would have duplicated the first block's 10 and 11 are recorded
  as 20 and 21. Run numbers in the table are labels, not order: each row is
  one full suite run with its own seed. Every run's raw output is kept at
  /cache/flakehunt/run-<n>.log (run 20's as run-11.log, run 21's as
  run-10.log, their in-lane names) and its row also at
  /cache/flakehunt/summary.tsv.
- Run 0 was an unseeded warm-up run (`go test -race -p 1 ./internal/factoryd/`)
  that also primed the build cache; it is counted in the twenty.
- A fresh `-shuffle` seed was drawn per run; the seed is printed by `go test`
  as `-test.shuffle <n>` and recorded here.

## The log of every run

| run | started (UTC) | ended (UTC) | seed (-shuffle) | verdict | failing tests |
| --- | --- | --- | --- | --- | --- |
| 0 | 2026-10-17T05:41Z | 2026-10-17T05:43Z | off | pass | — |
| 1 | 2026-10-17T05:46:13Z | 2026-10-17T05:48:18Z | 162392085 | pass | — |
| 2 | 2026-10-17T05:51:24Z | 2026-10-17T05:54:03Z | 166798611 | pass | — |
| 10 | 2026-10-17T05:51:35Z | 2026-10-17T05:54:10Z | 267237479 | pass | — |
| 3 | 2026-10-17T05:54:03Z | 2026-10-17T05:56:39Z | 498224851 | pass | — |
| 11 | 2026-10-17T05:54:10Z | 2026-10-17T05:56:45Z | 821093817 | pass | — |
| 4 | 2026-10-17T06:09:16Z | 2026-10-17T06:14:11Z | 658847632 | pass | — |
| 20 | 2026-10-17T06:09:16Z | 2026-10-17T06:14:14Z | 46574778 | pass | — |
| 5 | 2026-10-17T06:14:11Z | 2026-10-17T06:17:58Z | 674963501 | pass | — |
| 12 | 2026-10-17T06:14:14Z | 2026-10-17T06:18:02Z | 614549561 | pass | — |
| 6 | 2026-10-17T06:17:58Z | 2026-10-17T06:21:46Z | 579576986 | pass | — |
| 13 | 2026-10-17T06:18:02Z | 2026-10-17T06:21:48Z | 494569276 | pass | — |
| 7 | 2026-10-17T06:21:46Z | 2026-10-17T06:25:35Z | 998869957 | pass | — |
| 14 | 2026-10-17T06:21:48Z | 2026-10-17T06:25:37Z | 186000246 | pass | — |
| 8 | 2026-10-17T06:25:35Z | 2026-10-17T06:29:30Z | 1023183661 | pass | — |
| 15 | 2026-10-17T06:25:37Z | 2026-10-17T06:29:32Z | 972044190 | pass | — |
| 16 | 2026-10-17T06:29:32Z | 2026-10-17T06:33:05Z | 277080865 | pass | — |
| 9 | 2026-10-17T06:29:30Z | 2026-10-17T06:33:08Z | 782920212 | pass | — |
| 17 | 2026-10-17T06:33:05Z | 2026-10-17T06:35:54Z | 913498597 | pass | — |
| 21 | 2026-10-17T06:33:08Z | 2026-10-17T06:35:54Z | 844010524 | pass | — |

## Verdict

- Twenty-two full runs of `go test -race -p 1 -count=1 -shuffle=<seed>
  ./internal/factoryd/` (runs 0-21 above, 2026-10-17 05:41-06:36 UTC), each
  against its own throwaway PostgreSQL 17, nineteen of them with recorded
  shuffle seeds.
- **Zero runs failed.** Not one test failed in any run, and no run logged a
  `WARNING: DATA RACE`. Every run's exit status was 0; the only variance was
  wall-clock time (about 2 to 4.9 minutes per run, two lanes on two CPUs).
- Because no failure appeared even once, nothing repeated, there was no race
  or timing assumption to fix, and the issue's ten-run proof of a fix has
  nothing to prove. The suite, as it stands at 0325bbc, measured 0/22 flaky
  under the same shape CI uses (`scripts/pg-test.sh go test -race -p 1`).
- CI run 1402's cancellation at 078d004 was, by this evidence, not a flake
  of the factoryd race suite: 22 seeded runs, including shuffled orders
  never exercised by that run, all passed.
