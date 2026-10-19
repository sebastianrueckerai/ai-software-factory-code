-- A chat-filed escalation's question reached only the chat thread (card
-- #102): its question_asked row now builds the intake escalation row the
-- board and drawer read, so every waiting card names its ask. One row per
-- chat-source card still waiting, from its newest question_asked row,
-- none where an intake row already exists.
INSERT INTO intake_events (task_id, kind, author, body, question)
SELECT DISTINCT ON (t.id) t.id, 'escalation', 'factoryd', COALESCE(e.detail->>'ask', ''),
    jsonb_strip_nulls(jsonb_build_object(
        'ask', e.detail->>'ask',
        'why', e.detail->>'why',
        'info', e.detail->>'info',
        'recommendation', NULLIF(e.detail->>'recommendation', ''),
        'choices', (SELECT jsonb_agg(jsonb_build_object('value', choice #>> '{}', 'label', choice #>> '{}'))
            FROM jsonb_array_elements(COALESCE(e.detail->'choices', '[]'::jsonb)) AS choice)))
FROM tasks t
JOIN events e ON e.task_id = t.id AND e.kind = 'question_asked'
WHERE t.source = 'chat' AND t.status = 'waiting-human'
  AND NOT EXISTS (SELECT 1 FROM intake_events ie WHERE ie.task_id = t.id AND ie.kind = 'escalation')
ORDER BY t.id, e.at DESC;
