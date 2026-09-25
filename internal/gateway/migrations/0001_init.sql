CREATE TABLE IF NOT EXISTS tasks (
    id text PRIMARY KEY,
    budget_eur numeric NOT NULL,
    spent_eur numeric NOT NULL DEFAULT 0,
    status text NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
    id bigserial PRIMARY KEY,
    ts timestamptz NOT NULL DEFAULT now(),
    task_id text NOT NULL,
    decision text NOT NULL,
    reason text NOT NULL,
    model_alias text NOT NULL DEFAULT '',
    spend_eur numeric
);
