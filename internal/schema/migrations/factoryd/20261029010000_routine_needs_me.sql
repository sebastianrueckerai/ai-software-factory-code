-- the routine's live toggle (roadmap 1131): one side, needs me or runs
-- on its own, read when the run fires. NULL follows the config's files;
-- the setter's login rides beside it, as the skip's does.

ALTER TABLE routine_state ADD COLUMN needs_me boolean, ADD COLUMN needs_me_by text NOT NULL DEFAULT '';
