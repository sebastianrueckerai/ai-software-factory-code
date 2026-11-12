UPDATE cases c SET facts = c.facts || jsonb_build_object('policy_version', (a.spec->>'policy_version')::bigint)
FROM approval_requests a
WHERE c.kind = 'merge_approval' AND NOT c.facts ? 'policy_version'
  AND jsonb_typeof(c.facts->'approval') = 'number' AND a.id = (c.facts->>'approval')::bigint
  AND jsonb_typeof(a.spec->'policy_version') = 'number';
