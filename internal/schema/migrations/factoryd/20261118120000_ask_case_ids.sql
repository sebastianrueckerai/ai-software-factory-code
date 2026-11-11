-- An unnamed ask row goes to at most one live case of its task, the one that opened first at or after it, and a case takes at most its newest such row, as asks have named their case since: the face finds its ask by that name.
UPDATE events e SET detail = e.detail || jsonb_build_object('case_id', pick.case_id)
FROM (SELECT DISTINCT ON (case_id) case_id, event_id
      FROM (SELECT DISTINCT ON (v.id) v.id AS event_id, c.id AS case_id
            FROM events v JOIN cases c ON c.task_id = v.task_id AND c.status IN ('open', 'at_human') AND c.opened_at >= v.at
            WHERE v.kind = 'question_asked' AND NOT v.detail ? 'case_id'
            ORDER BY v.id, c.opened_at, c.id) one
      ORDER BY case_id, event_id DESC) pick
WHERE e.id = pick.event_id;
