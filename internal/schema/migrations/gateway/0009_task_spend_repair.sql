-- the final settlement of 2026-09-29 20:38:20Z added LiteLLM's figure onto
-- the spend of 41 tasks settled before the ledger existed, which held it
-- already: a settled task's spend is its ledger sum
UPDATE tasks SET spent_eur = l.eur
FROM (SELECT task_id, sum(eur) AS eur FROM ledger GROUP BY task_id) l
WHERE tasks.id = l.task_id AND tasks.settled_at IS NOT NULL AND abs(tasks.spent_eur - l.eur) >= 1e-9;

-- 14 Stage 4 tasks finished before finished_at was stamped: their last
-- audit row or session, else created_at, so the final settlement sees them
UPDATE tasks SET finished_at = COALESCE(GREATEST(
        (SELECT max(ts) FROM audit WHERE audit.task_id = tasks.id),
        (SELECT max(created_at) FROM sessions WHERE sessions.task_id = tasks.id)), created_at)
WHERE status IN ('done', 'failed', 'closed') AND finished_at IS NULL;
