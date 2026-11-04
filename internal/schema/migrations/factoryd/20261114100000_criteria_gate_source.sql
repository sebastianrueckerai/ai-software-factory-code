-- the criteria gate writes its own verdict rows
ALTER TABLE criteria_verdicts DROP CONSTRAINT IF EXISTS criteria_verdicts_source_check;
ALTER TABLE criteria_verdicts ADD CONSTRAINT criteria_verdicts_source_check CHECK (source IN ('review', 'approver', 'gate'));
