-- A case names the rung that owns its next move, the events that end its
-- wait and its fallback wake, which the sweep reads through cases_due. A
-- step carries the time its rung must answer by. The monitor's flags open
-- cases of kind monitor_flag.
ALTER TABLE cases ADD COLUMN owner_rung smallint NOT NULL DEFAULT 3 CHECK (owner_rung BETWEEN 0 AND 3),
                  ADD COLUMN next_wake_at timestamptz,
                  ADD COLUMN wake_on text[] NOT NULL DEFAULT '{}';
CREATE INDEX cases_due ON cases (next_wake_at) WHERE status IN ('open','at_human') AND next_wake_at IS NOT NULL;
ALTER TABLE case_steps ADD COLUMN due_at timestamptz;
CREATE INDEX case_steps_due ON case_steps (due_at) WHERE outcome = 'in_progress' AND due_at IS NOT NULL;
ALTER TABLE cases DROP CONSTRAINT cases_kind_check;
ALTER TABLE cases ADD CONSTRAINT cases_kind_check CHECK (kind IN ('session_cap','agent_time_cap','budget_floor','day_cap','month_cap','project_cap','checkin_human','checkin_share','gate_stop','findings_repeated','dependency_closed','wait_expired','pod_never_started','provider_refused','no_answer','content_filter','image_wait','screening_suspect','agent_escalate','agent_request_info','plan_question','plan_confirmation','merge_approval','human_card','other','dependency_failed','monitor_flag'));
