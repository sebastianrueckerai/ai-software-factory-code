-- a needs-you card's LLM-written brief and detail (ruling 116): one cheap
-- model call when the card files; brief_error marks a writer that failed,
-- so the card keeps today's text and the pass does not retry it
ALTER TABLE approval_requests ADD COLUMN IF NOT EXISTS brief text NOT NULL DEFAULT '';
ALTER TABLE approval_requests ADD COLUMN IF NOT EXISTS detail text NOT NULL DEFAULT '';
ALTER TABLE approval_requests ADD COLUMN IF NOT EXISTS brief_error text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS approval_requests_briefless ON approval_requests (id)
    WHERE status = 'pending' AND brief = '' AND brief_error = '';
