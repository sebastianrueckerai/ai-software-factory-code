-- The pull request whose head a carry card's clone supplies as branch
-- pull/<n>; 0 = the card carries none.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS carry_pr bigint NOT NULL DEFAULT 0
