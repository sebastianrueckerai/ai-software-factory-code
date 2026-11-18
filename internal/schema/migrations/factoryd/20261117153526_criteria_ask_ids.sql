-- A criteria ask names its criteria in its spec, the key a denied ask is matched on.
-- An older ask takes them from its approval_requested row; a gap ask with no such
-- row takes them from its reason, which named them.
UPDATE approval_requests a SET spec = a.spec || jsonb_build_object('criteria', e.detail->'criteria')
FROM events e
WHERE a.kind IN ('criteria', 'criteria_rerun') AND NOT a.spec ? 'criteria'
  AND e.task_id = a.task_id AND e.kind = 'approval_requested' AND e.detail->>'approval' = a.id::text
  AND jsonb_typeof(e.detail->'criteria') = 'array';

UPDATE approval_requests
SET spec = spec || jsonb_build_object('criteria', to_jsonb(string_to_array(substring(reason FROM '^criteria not met with the plan''s cards done: (.+)$'), ', ')))
WHERE kind = 'criteria' AND NOT spec ? 'criteria' AND reason ~ '^criteria not met with the plan''s cards done: .+$';
