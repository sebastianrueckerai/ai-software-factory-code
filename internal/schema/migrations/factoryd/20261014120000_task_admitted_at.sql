-- tasks gain admitted_at (roadmap 225, the work bound): the moment a
-- queued card took one of its project's work_in_progress slots, stamped
-- before its workflow starts so two admissions never take one slot. A
-- card in running or a later status holds its slot by its status alone;
-- the column stays NULL for a card that never started.
ALTER TABLE tasks ADD COLUMN admitted_at timestamptz NULL;
