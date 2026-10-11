-- One action, one row on answer: a question_answered event is unique per
-- task and comment id. The console's Answer button names the comment it
-- posted; intake stays silent for that comment when the webhook delivers
-- it, so the row is written once whatever the timing. The runner strips
-- comments now, but no semicolon in a comment keeps the convention.
CREATE UNIQUE INDEX IF NOT EXISTS events_answer_comment_idx ON events (task_id, (detail->>'comment')) WHERE kind = 'question_answered';
