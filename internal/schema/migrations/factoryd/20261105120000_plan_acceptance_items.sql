-- A card's acceptance became a list of items {criterion, text}; the
-- revisions stored before that hold plain strings. Each string s becomes
-- {"text": s}; objects, order and every other field stay.
UPDATE plan_revisions
SET plan = jsonb_set(plan, '{cards}', (
	SELECT jsonb_agg(
		CASE WHEN jsonb_typeof(c->'acceptance') = 'array' THEN jsonb_set(c, '{acceptance}', (
			SELECT COALESCE(jsonb_agg(
				CASE WHEN jsonb_typeof(a) = 'string' THEN jsonb_build_object('text', a #>> '{}') ELSE a END
				ORDER BY n), '[]'::jsonb)
			FROM jsonb_array_elements(c->'acceptance') WITH ORDINALITY AS i(a, n)))
		ELSE c END
		ORDER BY k)
	FROM jsonb_array_elements(plan->'cards') WITH ORDINALITY AS j(c, k)))
WHERE jsonb_path_exists(plan, '$.cards[*].acceptance[*] ? (@.type() == "string")')
