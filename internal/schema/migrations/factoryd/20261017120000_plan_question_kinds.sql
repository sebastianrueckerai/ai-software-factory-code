-- plan_questions gains kind (the spec-intake concept, piece 5): question,
-- the planner's, or confirmation, the one factoryd appends to show the
-- spec to the members. Asked until answered once: an answered
-- confirmation never asks again, so the kind, not the text, tells which
-- row it is.
ALTER TABLE plan_questions ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'question';
