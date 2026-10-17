-- the settling's birth kind (the notification-settling concept, roadmap
-- 292): a poke row carries the kind of the event that wrote it, so the
-- settle rule reads the row's family from the row itself at read, with
-- nothing stored when a row settles.
ALTER TABLE chat_messages ADD COLUMN birth_kind text NOT NULL DEFAULT '';

-- the rows already stored join their card's events on card and time: an
-- escalation row the newest question_asked or approval_requested at or
-- before it, a poke row the newest event of the kinds that birth one
-- (only those can have written it). A row that finds none keeps '' and
-- reads as an inform, but every one that names a card is logged, so a
-- wrong family is visible in the store's log.
UPDATE chat_messages m SET birth_kind = COALESCE((
	SELECT e.kind FROM events e
	WHERE e.task_id = m.task_id AND e.at <= m.at
		AND (m.kind = 'escalation' AND e.kind IN ('question_asked', 'approval_requested')
			OR m.kind = 'poke' AND e.kind IN ('question_withdrawn', 'task_ended', 'all_children_done',
				'card_filed', 'card_moved', 'plan_started', 'plan_accepted', 'card_retried', 'session_granted', 'routine_ran'))
	ORDER BY e.at DESC, e.id DESC LIMIT 1), '')
WHERE m.kind IN ('escalation', 'poke') AND m.task_id <> '' AND m.birth_kind = '';

DO $$
DECLARE r record;
BEGIN
	FOR r IN SELECT id, kind, task_id, at FROM chat_messages
		WHERE kind IN ('escalation', 'poke') AND task_id <> '' AND birth_kind = '' ORDER BY id LOOP
		RAISE WARNING 'chat_messages row % (kind %, task %, at %) matched no event: it reads as an inform', r.id, r.kind, r.task_id, r.at;
	END LOOP;
END $$;
