-- A case stored with the memory kill of the session before its stop holds
-- it as the death record that replaced the kill: the pod reached its own
-- memory limit, with the kill's exit code, reason, container and command.
UPDATE cases SET facts = (facts - 'killed') || jsonb_build_object('death', jsonb_strip_nulls(jsonb_build_object(
	'class', 'own_limit',
	'rule', 'memory_limit',
	'exit_code', facts->'killed'->'exit_code',
	'reason', facts->'killed'->'reason',
	'container', facts->'killed'->'container',
	'last_command', facts->'killed'->'command')))
WHERE jsonb_typeof(facts->'killed') = 'object'
