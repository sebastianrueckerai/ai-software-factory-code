-- An agent card that waits on a human with no case at the human gets its newest live ask put there.
-- A case a rung holds with a wake is left alone: it climbs to its face.
UPDATE cases c SET status = 'at_human'
FROM tasks t
WHERE t.id = c.task_id AND t.kind = 'agent' AND t.status = 'waiting-human' AND c.status = 'open'
  AND NOT (c.owner_rung < 3 AND c.next_wake_at IS NOT NULL)
  AND c.kind NOT IN ('merge_approval', 'plan_criteria', 'plan_question', 'plan_confirmation', 'monitor_flag')
  AND c.id = (SELECT max(o.id) FROM cases o WHERE o.task_id = c.task_id AND o.status = 'open'
      AND o.kind NOT IN ('merge_approval', 'plan_criteria', 'plan_question', 'plan_confirmation', 'monitor_flag'))
  AND NOT EXISTS (SELECT 1 FROM cases h WHERE h.task_id = c.task_id AND h.status = 'at_human');
