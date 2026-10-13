-- chat_messages' tool_calls gain read_at (roadmap 180, stage 18 item 3.5):
-- each stored tool call carries the time its result was read, so a turn's
-- replay can show an aged read beside the fresh card state. The column is
-- the jsonb array's per-element read_at timestamptz; rows written before
-- it read back stamped with the message's own time.
UPDATE chat_messages
	SET tool_calls = (
		SELECT jsonb_agg(CASE WHEN e ? 'read_at' THEN e
			ELSE e || jsonb_build_object('read_at', at) END
			ORDER BY ord)
		FROM chat_messages m2, jsonb_array_elements(m2.tool_calls) WITH ORDINALITY AS t(e, ord)
		WHERE m2.id = chat_messages.id)
	WHERE tool_calls IS NOT NULL AND jsonb_typeof(tool_calls) = 'array';
