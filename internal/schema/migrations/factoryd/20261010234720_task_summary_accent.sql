-- proposal-v1's console card (stage15 phase 3a): a human's summary of a
-- task, shown instead of the generated title clause, and a human's accent
-- colour on its card. Both nullable: NULL is "nothing set", the generated
-- title and no edge. Additive only; nothing reads them but the console.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS human_summary text;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS accent text;
