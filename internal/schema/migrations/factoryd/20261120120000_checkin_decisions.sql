-- The check-in's moments answer with the rungs' one action list (Offered),
-- so the decision values case_steps holds move onto its names and one
-- reader stays: the moment that cannot act answered human, which reads
-- hand_up now, the rungs' own hand-up; the gate's drop of every finding
-- answered dropped_all, which reads reverify now, the action's name. Rows
-- of other decisions and empty decisions stay: the rungs wrote those names
-- already.
UPDATE case_steps SET decision = 'hand_up' WHERE decision = 'human';
UPDATE case_steps SET decision = 'reverify' WHERE decision = 'dropped_all';
