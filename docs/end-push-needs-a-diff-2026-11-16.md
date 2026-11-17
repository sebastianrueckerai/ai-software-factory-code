# End push needs a diff, 2026-11-16

The criteria gate records a plan's verdicts at the PR merge press: without a
pushed branch, an open PR and the merge, no verdict is recorded. The worker's
end push stages the work tree, so a work tree identical to base never pushes
and no PR can open. A card that owes a merged PR therefore lands a non-empty
change.
