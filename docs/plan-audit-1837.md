# The plan card audited against the view-rebuild of #1837

Card plan-audit-1837 of plan #1850 (8 of 9). The rebuild's question was
whether every face that asks renders one decide block through the shared
case-decide partial. This note audits the plan kind's faces against it rule
by rule, names every gap, and ends each gap with its fix — landed by this
card — or the reason it stays. The fixes keep C42 (a press answers on the
board alone, no confirmation line), C44 (the amber band and the carousel)
and C45 (criterion rows) exactly as the design decisions state them.

## The spec

The plan doc `2026-11-13-one-face-every-card.md` is not in the docs clone:
reading it needs a fetch the session has no credentials for. The card's text
plus the landed code are the spec. The rules the card names for the plan
kind, judged below against `internal/console/templates/board.html`,
`approvals.go`, `cases.go` and `decide.go` and the diffs the rebuild landed:

1. one decision at a time;
2. the five parts of the ask — the ask with its money line, the why, what
   the call needs, the recommendation with its source;
3. exactly one fund control;
4. one note box;
5. one send applying all three: the decision, the fund, the note.

## The gate

The rebuild's cards are on main. t-b37303f8df4c ("The plan confirmation
view: the decision, the hold-outs beside the confirmation") landed as
381cb446 through PR #1865; its siblings landed with it: #1845 ("The face of
a confirming plan holds its hold-outs", 9c0c93ff) and the plan-face card
t-44d48462de03's commits 97cf1828, bbed4c6a and fe846473, which folded the
case-slot plan asks into the shared partial. The gate holds; the audit
below judges what those commits left.

## The faces the plan kind asks on

- **The case face's slots.** A plan's round opens one case per unanswered
  question plus its confirmation; the case face walks them in the C44
  carousel, questions first, the confirmation last. Since 97cf1828 each
  slot renders through the shared partial (`cases.go` `planSlotOf`, the
  `case` template's `{{template "case-decide" .PlanDecide}}`): the ask's
  five parts, the planner's recommended choice checked, one fund control,
  one note box, and one send that posts `revision`, `index`, the choice or
  the decision, `fund_eur` and `note` to `/tasks/{id}/plan/answer`. **This
  conforms on every rule.** The audit found nothing to change here.

- **The plan approval card** (the `plan` template), the body the approval
  request shows: in the drawer (suffix `-d`), as the case row's body when a
  plan case has no slot of its own (`cases.go`, the `slotless` branch, and
  standalone on Needs you when the plan's cases are all the human's,
  `board.go`). The zones below are judged per rule.

- **The Extras copy** — an approval whose case carries its own slot renders
  beside it and asks nothing. **Stays, with reason:** the slot above it is
  the ask; a second decide block on the same row would ask the same
  question twice.

## The gaps, each with its fix or reason

### 1. The confirmation zone: two forms, no fund, no note

The landed confirmation zone asked with two separate forms — one posting
`decision=confirm`, one posting `decision=revise` with its own answer box —
and neither carried a fund control or a note box: two sends applying one
thing each (rules 1, 3, 4, 5 broken; the parts of the ask were the lead
sentence and the take-off lines).

**Fix (landed):** the zone now renders one case-decide block, the same
`plan-confirm` block the case slot renders — the ask's five parts, Confirm
and Request changes as the one form's two decisions, one fund control, one
note box, and one send to `/tasks/{id}/plan/answer` that applies all three.
The take-off lines stay beside the Confirm button, as the partial renders
them. The lead sentence stays only on the copies that ask nothing, where
the block's own why line is absent.

### 2. The questions zone: every question asked at once, a form per choice

The landed zone rendered every unanswered question its own fieldset — one
form per choice, one more for the human's own words — so a round of three
questions put eight sends on one copy and no fund control or note box
anywhere (rules 1, 3, 4, 5 broken).

**Fix (landed):** the zone now renders one decide block for the first
unanswered question — the ask's five parts with the planner's
recommendation checked, its choices as radios, own words, one fund control,
one note box, and one send — and lists the questions behind it as rows
without forms, their choices inline, each becoming the ask in turn as the
one before it is answered. One decision at a time, as the case face's
carousel already walks them.

### 3. The hold-out zone: two forms per proposed hold-out

Each proposed hold-out rendered two forms — Okay and Drop — beside the
confirmation (rule 1 held; each row is one decision, and rule 5's shape did
not: two sends where the zone's rows read as one).

**Fix (landed):** one form per proposed hold-out, Okay and Drop its two
submit buttons (`name="verb"`), the same `revision` and `index` as before.

**Reason no fund control and no note box:** a hold-out click draws nothing
and posts nothing on the issue — `TestPlanHoldoutClicksDecideTheRound`
pins that the press leaves no comment — so there is nothing for a fund or a
note to apply. The confirmation's decide block beside the zone is the
zone's one fund control and one note box.

### 4. The conformance test never covered the plan kind

`internal/factoryd/testdata/fixtures` held only `board-agent-escalate.json`
and `board-session-cap.json`, so `TestBoardFixturesRenderTheDecidePartial`
asserted the shared partial for the escalate and session-cap kinds alone;
the acceptance's plan-kind conformance output could not pass.

**Fix (landed):** `board-plan-question.json` — a plan card waiting on one
question with the planner's recommendation — seated by a plan branch of
`seatBoardFixture`, with its own case in the test's table: the five parts,
one fund control, one note box, one decide form. The per-kind fixture work
beyond the plan kind stays with the conformance integration card.

## What stayed, with its reason

- **Request changes on the plan-wide form** (`/tasks/{id}/plan/changes`,
  shown while the round holds no confirmation). It answers a different
  question than the waiting ask — restart the round with the human's words,
  standing beside whatever question waits — and it is the only out while
  no confirmation waits. One send, applying one thing: its own words on the
  issue.
- **The answered lines** stay as rows: each names its question, its answer
  and who answered, and asks nothing (C42: the answer shows on the card).
- **The order of asks on the approval card** — the confirmation hoisted
  above the questions — stays as the rebuild landed it; the case face's
  carousel walks the questions first and the confirmation last. The two
  orders are the two faces' own: the card leads with the decision that ends
  the round, the carousel walks the round's cases in plan order.
- **C42:** every send here renders the board again with no confirmation
  line; nothing added one. **C44:** the amber band, the 12 px gaps and the
  carousel are untouched — the folds change only what sits between the band
  and the carousel. **C45:** criterion rows are not part of the plan
  approval card's zones and are untouched.

## How the fixes are pinned

`TestBoardFixturesRenderTheDecidePartial` gains the plan kind's case
(fixture `board-plan-question.json`, seated by a plan branch of
`seatBoardFixture`, its counts scoped to the one decide form);
`TestPlanCardDecidesTheConfirmation`, `TestPlanCardAsksTheQuestions` and
`TestPlanFacesDecideThroughTheSharedPartial` pin the case face's slots
unchanged. The tests that pinned the landed zone markup moved with the
folds: `TestPlanCardHoldsTheHoldoutZone` and
`TestPlanCardFaceHoldsConfirmingHoldouts` now pin one form per proposed
hold-out with Okay and Drop as its two sends, `TestPlanHoldoutClicksDecideTheRound`
counts one hold-out form after an okay (its conflict replies, the signal,
the event feed and the empty comment list untouched — they pin behaviour),
`TestPlanCardOffersNoAcceptOrReject` counts the row's three forms, and
`TestPlanSlotMasksModelText` reads the listed choices masked in the row
behind the ask.
