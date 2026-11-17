-- The case writer's lines are a story now, what happened and what is left:
-- a live case's older brief is none, so the writer tells it again.
UPDATE cases SET brief = '', brief_detail = '', brief_error = '' WHERE status IN ('open', 'at_human');
