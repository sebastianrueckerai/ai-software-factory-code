-- A plan question's choices each say what they change in the plan: the planner writes one short
-- line per choice, which the choice's face shows as its effect. A question stored before holds none.
ALTER TABLE plan_questions ADD COLUMN IF NOT EXISTS changes jsonb NOT NULL DEFAULT '[]'::jsonb;
