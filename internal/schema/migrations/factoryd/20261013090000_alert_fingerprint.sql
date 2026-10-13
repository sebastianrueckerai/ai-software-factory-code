-- the SigNoz alert fingerprint that filed an alert card's task, so its
-- pre-check finds its alert again before the first model call
-- (roadmap 181, Stage 18 item 3.12)
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS alert_fingerprint text;
