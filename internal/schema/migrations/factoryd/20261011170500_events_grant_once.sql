-- One answer, one row, a grant included: session_granted names the comment
-- that grants the session as question_answered names an answer's, and
-- intake's webhook path answers that very comment with question_answered,
-- so the index holds both kinds.
DROP INDEX IF EXISTS events_answer_comment_idx;
CREATE UNIQUE INDEX IF NOT EXISTS events_answer_comment_idx ON events (task_id, (detail->>'comment')) WHERE kind IN ('question_answered', 'session_granted');
