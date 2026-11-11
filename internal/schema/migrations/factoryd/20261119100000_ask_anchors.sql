-- Each face of an ask case records its issue comment on the case's step, so a comment after the first face still answers the ask.
ALTER TABLE case_steps ADD COLUMN comment_id bigint NOT NULL DEFAULT 0;

-- The escalation intake rows are the cases now: nothing writes or reads their columns.
ALTER TABLE intake_events
  DROP COLUMN choices,
  DROP COLUMN withdraws,
  DROP COLUMN question;

-- No answer grants a share of the check-in budget any more.
ALTER TABLE tasks DROP COLUMN extra_checkin_share;

-- The face steps are the one record of a case's faces: the newest face's comment is read from them.
CREATE INDEX case_steps_face ON case_steps (case_id, id DESC) WHERE comment_id <> 0;
CREATE FUNCTION case_face_comment(cid bigint) RETURNS bigint LANGUAGE sql STABLE AS
$$ SELECT COALESCE((SELECT s.comment_id FROM case_steps s WHERE s.case_id = cid AND s.comment_id <> 0 ORDER BY s.id DESC LIMIT 1), 0) $$;
ALTER TABLE cases DROP COLUMN comment_id;
