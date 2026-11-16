# Finding

A finding names one fault that a review, an audit or a check found. A human and the agent that fixes the fault read it. Give one fault per finding.

Say where the fault is, what is wrong, what it causes, and the fitting correction. Give the place exactly: a file and line, a card, or a check by its name. Say what the fault causes, with its numbers. The effect shows how much the fault matters, so leave out "critical", "severe" or "major". State the fault, and do not grade another step's work. Explain each reference at its first use. Keep code, identifiers and error strings exact, in backticks. Use the active voice and simple tenses.

## Examples

Good:

> `internal/console/static/chat.js:283` folds only notes whose kind starts with `wake`. Since 2026-11-11 the notes that wake the project agent have the kind `case_climbed`, so each one shows in full under the human's name. On 2026-11-16 that was 46 notes with 544,017 characters. Fold every note that wakes the agent, `case_climbed` among them.

Bad:

> The rung-1's read is right.

(It grades another step's work, uses a word only the factory knows, and names no place, no fault and no correction.)
