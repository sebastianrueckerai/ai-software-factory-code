-- the escalation an escalation_withdrawn intake event withdraws, by its
-- comment's id (roadmap 246): the event's own comment_id is the
-- withdrawal's comment, so a comment between the two still answers the
-- escalation. 0 for every other event.
ALTER TABLE intake_events ADD COLUMN IF NOT EXISTS withdraws bigint NOT NULL DEFAULT 0;
