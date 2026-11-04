-- a judgement criterion's verdict is one model call over its artifact
ALTER TABLE criteria_verdicts DROP CONSTRAINT IF EXISTS criteria_verdicts_source_check;
ALTER TABLE criteria_verdicts ADD CONSTRAINT criteria_verdicts_source_check CHECK (source IN ('review', 'approver', 'gate', 'judgement'));
