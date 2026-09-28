CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id text PRIMARY KEY,
    received_at timestamptz NOT NULL DEFAULT now()
);
