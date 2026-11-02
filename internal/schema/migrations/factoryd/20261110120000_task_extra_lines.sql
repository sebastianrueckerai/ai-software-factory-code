-- the line-bound raises: what the card's extend answers added on top of
-- the bound its size plans. Every line-bound check — the worker's push
-- rule, the gate's, the supervisor's trip — reads the size's plan plus
-- this, so an extend lifts them at once.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS extra_lines integer NOT NULL DEFAULT 0;
