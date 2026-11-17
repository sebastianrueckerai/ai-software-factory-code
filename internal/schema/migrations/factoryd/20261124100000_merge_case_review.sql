UPDATE cases c SET facts = c.facts || jsonb_build_object(
    'review_approved', COALESCE(a.spec->'review'->>'verdict', '') = 'approve' AND COALESCE(a.spec->'review'->>'note', '') = '',
    'review_summary', COALESCE(a.spec->>'verdict_summary', ''))
FROM approval_requests a
WHERE c.kind = 'merge_approval' AND c.status IN ('open', 'at_human') AND NOT c.facts ? 'review_approved'
  AND jsonb_typeof(c.facts->'approval') = 'number' AND a.id = (c.facts->>'approval')::bigint
  AND jsonb_typeof(a.spec->'review') = 'object';
