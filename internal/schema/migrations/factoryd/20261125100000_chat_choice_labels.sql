-- A request's and a failure's inbox buttons take the vocabulary's labels;
-- rows written before offer the old words, which no press resolves.
UPDATE chat_messages SET choices = (
    SELECT jsonb_agg(CASE c WHEN 'Approve' THEN 'Merge it' WHEN 'Re-run the checks' THEN 'Run the failed checks again'
        WHEN 'Retry' THEN 'Try the failed card again' WHEN 'Leave it' THEN 'Not now' ELSE c END ORDER BY i)
    FROM jsonb_array_elements_text(choices) WITH ORDINALITY AS x(c, i))
WHERE birth_kind IN ('approval_requested', 'task_ended') AND jsonb_typeof(choices) = 'array'
  AND choices ?| ARRAY['Approve', 'Re-run the checks', 'Retry', 'Leave it'];
