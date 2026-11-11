-- Each live case's newest ask row that names no case, written at or before the case opened, names it, as asks have named their case since: the face finds its ask by that name.
UPDATE events e SET detail = e.detail || jsonb_build_object('case_id', pick.case_id)
FROM (SELECT DISTINCT ON (c.id) c.id AS case_id, v.id AS event_id
      FROM cases c JOIN events v ON v.task_id = c.task_id AND v.kind = 'question_asked' AND NOT v.detail ? 'case_id' AND v.at <= c.opened_at
      WHERE c.status IN ('open', 'at_human') ORDER BY c.id, v.id DESC) pick
WHERE e.id = pick.event_id;
