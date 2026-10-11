-- One answer, one row: a question_answered event is unique per task and
-- the issue comment it names, whichever of the console's Answer button and
-- intake's webhook path records it first. The older timing could leave a
-- later duplicate behind, which would stop the index, so the later row of
-- each such pair goes first and the earliest stays.
DELETE FROM events later USING events earlier
WHERE later.kind = 'question_answered' AND earlier.kind = 'question_answered'
  AND later.task_id = earlier.task_id AND later.detail->>'comment' = earlier.detail->>'comment'
  AND later.id > earlier.id;
CREATE UNIQUE INDEX IF NOT EXISTS events_answer_comment_idx ON events (task_id, (detail->>'comment')) WHERE kind = 'question_answered';
