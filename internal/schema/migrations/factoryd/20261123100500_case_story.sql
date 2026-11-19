-- Clear the briefs of live cases, so the case writer tells their story.
UPDATE cases SET brief = '', brief_detail = '', brief_error = '' WHERE status IN ('open', 'at_human');
