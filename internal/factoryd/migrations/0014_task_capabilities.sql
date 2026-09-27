UPDATE tasks SET required_capabilities = '["coder"]'
WHERE kind = 'agent' AND required_capabilities IN ('null'::jsonb, '[]'::jsonb);
