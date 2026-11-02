-- plan_questions gains decision: the confirmation's closed answer,
-- confirm files the revision, revise sends the planner back with the
-- words as findings. No code parses prose for a decision: the words stay
-- findings, the decision is the field. The answered confirmations
-- history carries under the old rule: a trimmed answer of okay is a
-- confirm, every other answer a revise.
ALTER TABLE plan_questions ADD COLUMN IF NOT EXISTS decision text;
ALTER TABLE plan_questions ADD CONSTRAINT plan_question_decision_confirmation
	CHECK (COALESCE(kind, 'question') <> 'confirmation' OR decision IN ('confirm', 'revise'));
ALTER TABLE plan_questions ADD CONSTRAINT plan_question_decision_real_question
	CHECK (COALESCE(kind, 'question') = 'confirmation' OR decision IS NULL);
UPDATE plan_questions SET decision = CASE WHEN btrim(answer) = 'okay' THEN 'confirm' ELSE 'revise' END
	WHERE kind = 'confirmation' AND answer IS NOT NULL;
