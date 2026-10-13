-- a deleting row counts against a cap only while its release is still
-- being retried; a delete the collector owns holds no slot
ALTER TABLE cloud_resources ADD COLUMN IF NOT EXISTS held boolean NOT NULL DEFAULT false;
