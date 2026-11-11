-- The factory's one money case: the judgment budget is spent, so every case goes to the
-- human unjudged until it is funded.
ALTER TABLE cases DROP CONSTRAINT cases_kind_check;
ALTER TABLE cases ADD CONSTRAINT cases_kind_check CHECK (kind IN ('session_cap','agent_time_cap','budget_floor','day_cap','month_cap','project_cap','checkin_human','checkin_share','checkin_failed','gate_stop','findings_repeated','dependency_closed','wait_expired','pod_never_started','provider_refused','no_answer','content_filter','image_wait','screening_suspect','agent_escalate','agent_request_info','plan_question','plan_confirmation','merge_approval','human_card','other','dependency_failed','monitor_flag','judgment_budget'));
