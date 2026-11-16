-- A tool card names the sandbox image it adds a tool to, which the next
-- ask for that image joins: the lookup reads this fact, not the title.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS tool_image text;

-- a tool card filed before reads its image off its title
UPDATE tasks SET tool_image = substring(split_part(objective, E'\n', 1) from ' to image (.+)$')
 WHERE source = 'tool_request' AND tool_image IS NULL;
