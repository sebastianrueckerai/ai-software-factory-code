-- the lines of the plan's opened proofs a card's clone holds: new files of
-- its pull request, so every line-bound check reads the size's plan plus
-- extra_lines plus this. RunSession sets it, never adds to it.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS proof_lines integer NOT NULL DEFAULT 0;
