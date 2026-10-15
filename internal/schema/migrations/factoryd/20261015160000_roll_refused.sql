-- a commit whose images carry no signature by the factory's key (roadmap
-- 46): refused, its task the human card that says why, and no roll
ALTER TABLE roll_requests DROP CONSTRAINT IF EXISTS roll_requests_state_check;
ALTER TABLE roll_requests ADD CONSTRAINT roll_requests_state_check CHECK (state IN ('opened', 'merged', 'skipped', 'failed', 'refused'));
