-- A plan whose cards ran out with criteria no verdict row holds asks
-- criteria_rerun: its approver runs the review that never judged them,
-- apart from the criteria gap of rows that say unmet.
ALTER TABLE approval_requests DROP CONSTRAINT IF EXISTS approval_requests_kind_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_kind_check CHECK (kind IN ('merge', 'resource', 'plan', 'criteria', 'criteria_rerun'));
