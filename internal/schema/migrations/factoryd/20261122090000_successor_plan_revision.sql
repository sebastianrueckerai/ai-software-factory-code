-- A plan card's successor filed before it carried its revision takes the revision its chain's plan card ran against.
WITH RECURSIVE chain (id, revision) AS (
	SELECT id, plan_revision FROM tasks WHERE source = 'plan'
	UNION
	SELECT s.id, chain.revision FROM chain
	JOIN tasks s ON s.id = 't-' || left(encode(sha256(convert_to('successor ' || chain.id, 'UTF8')), 'hex'), 12)
	WHERE s.source = 'successor'
)
UPDATE tasks t SET plan_revision = chain.revision
FROM chain
WHERE t.id = chain.id AND t.source = 'successor' AND t.plan_key IS NOT NULL AND t.plan_revision = 0 AND chain.revision > 0;
