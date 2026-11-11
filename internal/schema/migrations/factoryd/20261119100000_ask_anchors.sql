-- Each face of an ask case records its issue comment on the case's step, so a comment after the first face still answers the ask.
ALTER TABLE case_steps ADD COLUMN IF NOT EXISTS comment_id bigint NOT NULL DEFAULT 0;

-- The escalation intake rows are the cases now: nothing writes or reads their columns.
ALTER TABLE intake_events
  DROP COLUMN IF EXISTS choices,
  DROP COLUMN IF EXISTS withdraws,
  DROP COLUMN IF EXISTS question;

-- No answer grants a share of the check-in budget any more.
ALTER TABLE tasks DROP COLUMN IF EXISTS extra_checkin_share;

-- The face steps are the one record of a case's faces: the newest face's comment is read from them.
CREATE INDEX IF NOT EXISTS case_steps_face ON case_steps (case_id, id DESC) WHERE comment_id <> 0;
CREATE OR REPLACE FUNCTION case_face_comment(cid bigint) RETURNS bigint LANGUAGE sql STABLE AS
$$ SELECT COALESCE((SELECT s.comment_id FROM case_steps s WHERE s.case_id = cid AND s.comment_id <> 0 ORDER BY s.id DESC LIMIT 1), 0) $$;

-- A case from before keeps the comment of its face on its newest face step. A case
-- with no face step gets one, dated at the ask row that carried the comment.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'cases' AND column_name = 'comment_id') THEN
    UPDATE case_steps s SET comment_id = c.comment_id FROM cases c
      WHERE s.case_id = c.id AND c.comment_id <> 0 AND s.comment_id = 0
        AND s.id = (SELECT max(f.id) FROM case_steps f WHERE f.case_id = c.id AND f.rung = 3 AND f.actor = 'factoryd');
    INSERT INTO case_steps (case_id, rung, actor, started_at, ended_at, outcome, comment_id)
      SELECT c.id, 3, 'factoryd',
        COALESCE((SELECT min(e.at) FROM events e WHERE e.task_id = c.task_id AND e.kind = 'question_asked'
                  AND e.detail->>'comment' = c.comment_id::text), c.opened_at),
        c.resolved_at,
        CASE c.status WHEN 'resolved' THEN 'resolved' WHEN 'withdrawn' THEN 'withdrawn' ELSE 'in_progress' END,
        c.comment_id
      FROM cases c
      WHERE c.comment_id <> 0
        AND NOT EXISTS (SELECT 1 FROM case_steps s WHERE s.case_id = c.id AND s.rung = 3 AND s.actor = 'factoryd');
    ALTER TABLE cases DROP COLUMN comment_id;
  END IF;
END $$;
