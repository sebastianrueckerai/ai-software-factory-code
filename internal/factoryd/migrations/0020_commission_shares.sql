-- each commission row's share of its reservation (its hours at its hourly
-- price: an overrun is measured against it) and its catalogue monthly price
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS reserved_eur numeric NOT NULL DEFAULT 0;
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS monthly_eur numeric NOT NULL DEFAULT 0;
