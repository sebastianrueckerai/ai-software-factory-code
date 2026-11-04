The authored one-liners, one per agent, picked by Sebastian from the
phase 4 drafts (2026-11-01). They change only when the factory's shape
changes.

- coding: Writes the card's code in the sandbox and commits it. No other agent in the factory writes code.
- screening: Reads a fresh issue before any code exists and answers clear or suspect as JSON. Only work it understands becomes a card.
- sizing: Reads a clear issue and sizes the card small, medium or large before the first session starts.
- planner: Reads the repository and files a plan through the file_plan tool before the session writes code.
- prover: Writes one test per criterion on main before the cards build the feature, and files them through the file_proofs tool. The builder sees a test only after it has run once at the gate.
- message-responder: Answers a writer's message pushed into a running session. The message becomes the loop's next turn.
- opencode-subagents: opencode's own general and explore subagents, set by the agent block in our session config. Their prompts live inside the pinned binary; the config sets their model and step cap.
- checkin: Reads a session that stopped at a checkpoint short of done and answers one of continue, message, handover, human or replan.
- model-review: Judges the change against one review criterion or behaviour, the texts the criteria and behaviours agents draft.
- security-audit: Checks the change against the security checklist. No other agent makes this reading.
- gate-supervisor: Reads the verdicts and answers approve or escalate. Nothing merges past this agent unjudged.
- review-criteria: Drafts the criteria text the model reviews judge by.
- review-behaviours: Drafts the behaviours text the model reviews watch for.
- delta-planner: Plans the delta work between one review and the next.
- holdout-proposer: Proposes the hold-out criteria the finished work is judged by. The session that writes the work does not see them.
- answer-screener: Screens a late answer from the issue's author before it changes the card.
- brief-writer: Writes the approval brief a human reads to decide.
- title: Names a chat thread from its first message. One cheap call.
- project-agent: The always-on chat about the repository, with the project's tools. Its escalations file cards the factory then works.
- compact: Summarises a chat thread's older turns when the thread grows long. One cheap call.
- vision: Describes an image a human attached, in words the text-only agents read.
