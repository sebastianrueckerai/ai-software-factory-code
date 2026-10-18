-- one LIVE card per issue (roadmap 298): a final card frees its issue
-- number, so the intake of an issue a finished card once held files again;
-- a second live card stays refused, identity only, likeness never judged
DROP INDEX IF EXISTS tasks_repository_issue_number_key;
CREATE UNIQUE INDEX tasks_repository_issue_number_key
    ON tasks (repository, issue_number)
    WHERE issue_number <> 0 AND status NOT IN ('done', 'failed', 'closed');
