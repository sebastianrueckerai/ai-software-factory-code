# Criteria gate merges verdicts, 2026-11-17

Plan #1837's six criteria record their verdicts only at a PR merge press:
the GateCriteria run on the merged head is what the criteria card reads.
A plan whose pull request never merges leaves its criteria unrecorded, so
every card that owes a merge walks its PR to approve_merge before it ends.
