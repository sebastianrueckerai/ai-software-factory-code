-- a resource approval admits one commission: spent by the commission it
-- admitted, or by the envelope widening of a project-wide approve
ALTER TABLE approval_requests ADD COLUMN IF NOT EXISTS consumed boolean NOT NULL DEFAULT false;
UPDATE approval_requests SET consumed = true
WHERE id IN (SELECT approval_id FROM cloud_resources WHERE approval_id IS NOT NULL)
   OR decision_note = 'approved for the project: its envelope was widened to fit';
