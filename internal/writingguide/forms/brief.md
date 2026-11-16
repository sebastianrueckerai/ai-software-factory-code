# Brief

The brief is the card's full text. The agent that builds the card and a human read the same text. Write for a reader who dropped in cold. This reader did not follow the work and forgot how the process runs. Give the context the reader needs and no more.

Say what to build, why, and how it is checked. Explain each reference at its first use and link it: "#2011 (the plan's last card, closed without a merge)". Link a part of a document as [words](path#heading). A bare "Phase B", "requirement 1b" or "task #2004" means nothing to this reader, so link it or say what it is. Do not name the branch. The factory adds the start line itself. Write each promise of a plan as one plain sentence.

Give steps as a numbered list, one action per item, in the imperative. Put the condition before the command: "If the build fails, run make clean." Use one name for one thing. Keep code, identifiers and error strings exact, in backticks. Write the number, never "significantly" or "much faster".

## Examples

Good:

> The console refuses to close a plan while any of its criteria are open. A criterion is a promise of the plan, and a hidden test checks it when the plan's last card ends. The refusal also stops plans that hold no criteria, so such a plan closes only with a waiver.
>
> Build:
>
> 1. In `doClose`, refuse the close only when the plan holds at least one criterion.
>
> Check:
>
> 1. Close a plan with no criteria. The close goes through.
> 2. Close a plan with two open criteria. The console refuses.
>
> This is card 1 of 3 of plan #1947 (criteria gate). It waits for no card.

Bad:

> Answer the review finding on branch card/2084, as per the plan's requirement 1b. See Phase A and task #2004.

(The branch exists in no repository. "Requirement 1b", "Phase A" and "task #2004" have no link and no explanation. The text does not say what to build, why, or how it is checked.)
