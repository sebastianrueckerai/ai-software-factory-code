UPDATE cases c SET facts = c.facts || jsonb_build_object(
    'review_approved', COALESCE(a.spec->'review'->>'verdict', '') = 'approve' AND COALESCE(a.spec->'review'->>'note', '') = '',
    'review_findings', COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(a.spec->'review'->'findings') = 'array' THEN a.spec->'review'->'findings' END), 0))
  || CASE WHEN COALESCE(a.spec->'review'->>'note', '') = '' THEN jsonb_build_object('review_verdict', COALESCE(a.spec->'review'->>'verdict', '')) ELSE '{}'::jsonb END
  || CASE WHEN jsonb_typeof(a.spec->'audit'->'findings') = 'array' AND jsonb_array_length(a.spec->'audit'->'findings') > 0
          THEN jsonb_build_object('audit_findings', jsonb_array_length(a.spec->'audit'->'findings')) ELSE '{}'::jsonb END
FROM approval_requests a
WHERE c.kind = 'merge_approval' AND c.status IN ('open', 'at_human') AND NOT c.facts ? 'review_findings'
  AND jsonb_typeof(c.facts->'approval') = 'number' AND a.id = (c.facts->>'approval')::bigint
  AND jsonb_typeof(a.spec->'review') = 'object';
UPDATE cases SET facts = facts - 'review_summary' WHERE kind = 'merge_approval' AND facts ? 'review_summary';
