-- a human resets a project's escalation pool too: its draws count from
-- its newest reset in the UTC calendar month, and it resets with its
-- repository as a project's month cap does
ALTER TABLE cap_resets DROP CONSTRAINT IF EXISTS cap_resets_cap_check;
ALTER TABLE cap_resets ADD CONSTRAINT cap_resets_cap_check CHECK (cap IN ('day', 'month', 'project', 'premium', 'pool'));
ALTER TABLE cap_resets DROP CONSTRAINT IF EXISTS cap_resets_check;
ALTER TABLE cap_resets ADD CONSTRAINT cap_resets_check CHECK ((cap IN ('project', 'pool')) = (repository <> ''));
