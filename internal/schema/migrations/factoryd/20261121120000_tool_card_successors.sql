-- A card that continues a tool card is one and carries its image. An open
-- retry or successor filed before cards carried it gets the image of the
-- nearest card it continues, along the ids the factory derives from that
-- card: t- and the first 12 hex digits of sha256("retry <id>") or
-- sha256("successor <id>").
WITH RECURSIVE chain(id, image) AS (
    SELECT id, tool_image FROM tasks WHERE tool_image IS NOT NULL
  UNION
    SELECT t.id, COALESCE(t.tool_image, chain.image) FROM chain
      JOIN tasks t ON t.id IN (
        't-' || left(encode(sha256(convert_to('retry ' || chain.id, 'UTF8')), 'hex'), 12),
        't-' || left(encode(sha256(convert_to('successor ' || chain.id, 'UTF8')), 'hex'), 12))
)
UPDATE tasks SET tool_image = chain.image FROM chain
 WHERE tasks.id = chain.id AND tasks.tool_image IS NULL
   AND tasks.status NOT IN ('done', 'failed', 'closed', 'merged');
