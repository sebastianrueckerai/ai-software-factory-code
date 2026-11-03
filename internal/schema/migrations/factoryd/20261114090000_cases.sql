CREATE TABLE cases (
    id          bigserial PRIMARY KEY,
    task_id     text NOT NULL,
    repository  text NOT NULL DEFAULT '',
    kind        text NOT NULL CHECK (kind IN ('session_cap','agent_time_cap','budget_floor','day_cap','month_cap','project_cap','checkin_human','checkin_share','gate_stop','findings_repeated','dependency_closed','wait_expired','pod_never_started','provider_refused','no_answer','content_filter','image_wait','screening_suspect','agent_escalate','agent_request_info','plan_question','plan_confirmation','merge_approval','human_card','other')),
    state_key   text NOT NULL,
    status      text NOT NULL CHECK (status IN ('open','at_human','resolved','withdrawn')),
    problem     text NOT NULL,
    facts       jsonb NOT NULL DEFAULT '{}'::jsonb,
    money       jsonb,
    comment_id  bigint NOT NULL DEFAULT 0,
    follows     bigint REFERENCES cases(id),
    opened_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolution  jsonb
);
CREATE UNIQUE INDEX cases_open_key ON cases (task_id, kind, state_key) WHERE status IN ('open','at_human');
CREATE INDEX cases_task ON cases (task_id, id);
CREATE INDEX cases_repo_open ON cases (repository) WHERE status IN ('open','at_human');

CREATE TABLE case_steps (
    id         bigserial PRIMARY KEY,
    case_id    bigint NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    rung       smallint NOT NULL CHECK (rung BETWEEN 0 AND 3),
    actor      text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz,
    read       jsonb NOT NULL DEFAULT '[]'::jsonb,
    found      text NOT NULL DEFAULT '',
    did        jsonb NOT NULL DEFAULT '[]'::jsonb,
    decision   text NOT NULL DEFAULT '',
    grounds    text NOT NULL DEFAULT '',
    cost_eur   numeric(12,6) NOT NULL DEFAULT 0,
    outcome    text NOT NULL CHECK (outcome IN ('in_progress','resolved','handed_up','withdrawn'))
);
CREATE INDEX case_steps_case ON case_steps (case_id, id);
