-- Each face of an ask case records its issue comment on the case's step, so a comment after the first face still answers the ask.
ALTER TABLE case_steps ADD COLUMN comment_id bigint NOT NULL DEFAULT 0;

-- The escalation intake rows are the cases now: nothing writes or reads their columns.
ALTER TABLE intake_events
  DROP COLUMN choices,
  DROP COLUMN withdraws,
  DROP COLUMN question;

-- No answer grants a share of the check-in budget any more.
ALTER TABLE tasks DROP COLUMN extra_checkin_share;
