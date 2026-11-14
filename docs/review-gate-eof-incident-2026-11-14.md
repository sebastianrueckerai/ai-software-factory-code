# Review-gate EOF incident record: six invalid empty answers, 2026-11-14

On 2026-11-14 six model review calls across three cards returned invalid
empty (EOF) answers inside a four-hour window, each holding a proven-green
merge for 20–90 minutes until a settle ruling called it a transient EOF,
not a refused review. This file is the provider incident report for that
window and the recorded decision on the review gate's handling of it, per
the filing issue's acceptance. The per-event settle ruling (transient EOF,
not a refused review) stays.

## The window

All times UTC, 2026-11-14, from the filing issue's record of the card
timelines:

| # | card | step | time | head | how it ended |
| --- | --- | --- | --- | --- | --- |
| 1 | #1868 | step 36 | ~11:18 | the card's head | settled by sebastian at step 37, third-occurrence threshold attached |
| 2 | #1868 | step 42 | ~11:45 | the same head | settled by sebastian at step 41's class ruling (case 2947) |
| 3 | #1879 | step 27 | ~12:53 | 25ce5cb | settled at step 28 (case 2967) |
| 4 | #1879 | step 32 | ~13:17 | 25ce5cb | settled at step 33 (case 3017) |
| 5 | #1889 | step 32 | ~14:56 | 9b15de3 | settled (case 3581) |
| 6 | #1889 | reverify after case 3669 | ~15:15 | 9b15de3 | the standing third-occurrence threshold fired (case 3673) |

The control, which rules the reviewer model out as the cause: in the same
window reviews kept approving on other cards — #1899's review approve at
its merge, #1910's review approve, #1857's review approve four times. The
reviewer works when an answer arrives; the failures cluster on the largest
review inputs (big console and plan diffs, the multi-part calls of
`ModelReview`). Roughly two hours of human and agent attention went to the
five settlements.

Each of the six had the answer-retry in force: a review answer that does
not parse has been asked once more, same part, same model, since 2026-11-10
(commit 56956445). Which of the two shapes an event took — the retry also
returning an invalid answer, or the call failing outright so no retry ran —
is in the card timelines; both were possible on the day, because a call
that failed without the gateway refusing it (a transport EOF, an upstream
5xx) had no retry at all until this card (below).

## Provider response

None is available to record. The review call goes through the factory's
gateway (a LiteLLM router in front of the provider, docs/manual/policy.md);
an empty EOF answer arrives as an HTTP success with empty content, so the
router sees a completed call and its provider-failure routing never fires.
The gateway's logs and any provider-side ticket exchange are outside this
sandbox's reach, and no provider response had arrived by the filing. This
file is the incident report to attach or send: it names the window, the six
calls with their cards, heads and times, and the control that separates a
provider reliability pattern from a reviewer-model fault. If the provider
answers a ticket raised from it, the answer lands beside this file as an
update.

## Decision

Recorded decision, of the issue's three options:

1. **Retry — keep and complete it.** The one bounded retry on an invalid
   answer stays (an answer that does not parse is called once more with the
   parse error and the answer bounds in the input; the second miss is the
   note as today). This card extends the same single retry to a call that
   failed without the gateway refusing it — a transport EOF or an upstream
   5xx got no retry before; now it is called once more the same way. A
   money refusal is never called again: its note says what lifts it. Both
   retries book to the card like the first call and stop at one, so a hard
   provider outage costs one extra call per review, not a loop. This is in
   `ModelReview` beside `reviewRetry` in internal/factoryd/gates.go.
2. **No fallback model at the review call.** The review model is a policy
   fact — the class's `review_model`, not factoryd's choice — and the
   review is evidence, never authority; a silent fallback would judge the
   change on a model the policy never named, and the one fallback the
   system already has (the router sending a provider-failed premium call to
   `big`, docs/manual/policy.md) does not apply: an empty answer is a
   success to the router, and the review classes here are not premium.
3. **Provider ticket — this report is it.** Raise it from this file if the
   pattern recurs; one more day like 2026-11-14, with the retry in force,
   is the evidence a ticket needs that the provider drops the largest
   completions.

Rationale: the failures clustered on the largest inputs and recovered on a
human's settle every time, so a bounded second call is the cheapest
handling that can settle most events without attention; a fallback changes
what the review is, and a ticket changes nothing until the provider
answers. The threshold attached to the third occurrence on a head stays as
the human route around whatever the retry cannot clear.

## What this card changed

- internal/factoryd/gates.go: `ModelReview` retries a failed,
  non-refused review call once through the same `reviewRetry` as an
  unparseable answer; money refusals are never retried.
- internal/factoryd/supervisor_test.go: the fake gateway takes per-call
  statuses.
- Tests: a failed call is retried and its retry's input names the failure;
  the second miss stays the note (TestModelReviewAsksAFailedCallOnceMore,
  the upstream-error case of TestModelReviewNotes).

Checked with `bash check changed` on this card's tree.
