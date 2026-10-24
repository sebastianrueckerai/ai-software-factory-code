# internal/console test rules

One owner per contract, at the strongest boundary that can reach it. Extend a
table-driven test or an existing rig before adding a file or a test.

Learned the hard way in this suite:

- No test re-asserts a production table (routes, policy lists) that a
  behavioral test already drives. The behavioral test is the proof.
- Expected wording is written as the literal in the test, never produced by
  the formatter under test (comparing to `capSpent(...)` guards nothing).
- Every refusal row asserts its exact status code, not a union of codes.
- Fakes mirror the store's guards exactly: a fake missing a store error
  (the wake cap once) keeps the suite green over a production bug.
- A Config field production never wires is a test-only seam; delete it and
  pay the honest timing cost, or wire it — do not keep it for tests alone.
- Tests of `internal/factoryd` symbols belong in `internal/factoryd`; a
  console test of a factoryd function misfiles its failures.
