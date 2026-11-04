-- The escalation pool's records. An extend may add agent time, which the
-- console alone writes beside raised_sessions. ceiling_eur is the most a
-- card may be funded to, NULL for the policy's. A draw from the pool is a
-- card_funded or card_extended row whose detail says pool, summed per
-- month by the partial index. A case may name a failed dependency.
ALTER TABLE tasks ADD COLUMN raised_time_s bigint NOT NULL DEFAULT 0,
                  ADD COLUMN ceiling_eur numeric(12,2);
CREATE INDEX events_pool_draws ON events (repository, at)
    WHERE kind IN ('card_funded','card_extended') AND detail->>'pool' = 'true';
ALTER TABLE cases DROP CONSTRAINT cases_kind_check;
ALTER TABLE cases ADD CONSTRAINT cases_kind_check CHECK (kind IN ('session_cap','agent_time_cap','budget_floor','day_cap','month_cap','project_cap','checkin_human','checkin_share','gate_stop','findings_repeated','dependency_closed','wait_expired','pod_never_started','provider_refused','no_answer','content_filter','image_wait','screening_suspect','agent_escalate','agent_request_info','plan_question','plan_confirmation','merge_approval','human_card','other','dependency_failed'));
