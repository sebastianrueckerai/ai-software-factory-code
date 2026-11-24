-- a human resets the premium cap's count too: it counts from its newest
-- reset in the UTC calendar month
ALTER TABLE cap_resets DROP CONSTRAINT IF EXISTS cap_resets_cap_check;
ALTER TABLE cap_resets ADD CONSTRAINT cap_resets_cap_check CHECK (cap IN ('day', 'month', 'project', 'premium'));
