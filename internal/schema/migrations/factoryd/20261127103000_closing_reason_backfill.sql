-- A card that closed before every end stored why gets why from its records,
-- by the factory's own order at an end (endClosed): its newest close answer,
-- a card_closed row's note or a close's case's note, as "closed by <who>"
-- and the note's first sentence; else the first sentence of the factory's
-- newest close record, an older record's lead taken off, a record of
-- dropped dependencies alone saying none; else "closed in Forgejo by
-- <login>" for its newest issue_closed row, unless the factory recorded a
-- close of that issue (the bot's own). A first sentence is that of the
-- first paragraph on one line. Only a closed card whose reason is empty or
-- says only that its issue closed takes one; a card with no record stays.
UPDATE tasks SET closing_reason = w.why
FROM (
	SELECT t.id, COALESCE(a.why, b.why, c.why) AS why
	FROM tasks t
	LEFT JOIN LATERAL (
		SELECT CASE WHEN l.s LIKE 'closed by %' THEN l.s
				WHEN l.s = '' THEN 'closed by ' || n.who
				ELSE 'closed by ' || n.who || ': ' || l.s END AS why
		FROM (SELECT CASE e.actor WHEN 'factoryd' THEN 'the factory' WHEN 'checkin' THEN 'the check-in'
					WHEN 'factory-chat' THEN 'the factory agent' WHEN 'planner' THEN 'the planner' ELSE e.actor END AS who,
				CASE WHEN e.kind = 'card_closed' THEN COALESCE(e.detail->>'note', '')
					WHEN k.id IS NOT NULL THEN COALESCE(k.resolution->>'reason', '')
					ELSE COALESCE(e.detail->>'answer', '') END AS note
			FROM events e LEFT JOIN cases k ON k.id = CASE WHEN e.detail->>'case_id' ~ '^[0-9]{1,18}$' THEN (e.detail->>'case_id')::bigint END
			WHERE e.task_id = t.id AND (e.kind = 'card_closed' OR e.kind = 'question_answered' AND e.detail->>'action' = 'close')
			ORDER BY e.id DESC LIMIT 1) n,
		LATERAL (SELECT btrim(regexp_replace(split_part(btrim(n.note, E' \t\r\n'), E'\n\n', 1), '\s+', ' ', 'g')) AS o) p,
		LATERAL (SELECT COALESCE(substring(p.o FROM '^(.*?[.!?])[.!?]*\s+[[:upper:]]'), p.o) AS s) l
	) a ON true
	LEFT JOIN LATERAL (
		SELECT NULLIF(CASE WHEN l.s LIKE 'It no longer waits on %' THEN '' ELSE l.s END, '') AS why
		FROM (SELECT body FROM intake_events WHERE task_id = t.id AND kind = 'factory_closed' ORDER BY id DESC LIMIT 1) r,
		LATERAL (SELECT btrim(regexp_replace(split_part(btrim(r.body, E' \t\r\n'), E'\n\n', 1), '\s+', ' ', 'g')) AS o) p,
		LATERAL (SELECT regexp_replace(COALESCE(substring(p.o FROM '^(.*?[.!?])[.!?]*\s+[[:upper:]]'), p.o),
			'^factoryd: (task \S+ closed: )?', '') AS s) l
	) b ON true
	LEFT JOIN LATERAL (
		SELECT 'closed in Forgejo by ' || i.author AS why
		FROM (SELECT author, comment_id FROM intake_events WHERE task_id = t.id AND kind = 'issue_closed' ORDER BY id DESC LIMIT 1) i
		WHERE i.author <> '' AND NOT EXISTS (SELECT 1 FROM intake_events f
			WHERE f.task_id = t.id AND f.kind = 'factory_closed' AND f.comment_id = i.comment_id)
	) c ON true
	WHERE t.status = 'closed' AND (t.closing_reason = '' OR t.closing_reason LIKE 'issue closed%')
) w
WHERE tasks.id = w.id AND w.why IS NOT NULL
