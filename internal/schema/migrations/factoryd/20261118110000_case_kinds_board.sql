-- The case kinds of the board: a policy class, a red main, a held roll and a refused configuration.
ALTER TABLE cases DROP CONSTRAINT cases_kind_check;
ALTER TABLE cases ADD CONSTRAINT cases_kind_check CHECK (kind IN ('session_cap','agent_time_cap','budget_floor','day_cap','month_cap','project_cap','checkin_human','checkin_share','checkin_failed','gate_stop','findings_repeated','dependency_closed','wait_expired','pod_never_started','provider_refused','no_answer','content_filter','image_wait','screening_suspect','agent_escalate','agent_request_info','plan_question','plan_confirmation','merge_approval','human_card','other','dependency_failed','monitor_flag','judgment_budget','policy_class','red_main','roll_held','config_refused'));
UPDATE cases SET kind = 'policy_class' WHERE kind = 'checkin_human' AND facts->>'reason_kind' = 'policy_class';
UPDATE cases SET kind = 'roll_held' WHERE kind = 'human_card' AND facts->>'source' = 'production-held';
UPDATE cases SET kind = 'config_refused' WHERE kind = 'human_card' AND facts->>'source' = 'config';
UPDATE cases SET kind = 'red_main' WHERE kind = 'human_card' AND facts->>'source' = 'still-red';
