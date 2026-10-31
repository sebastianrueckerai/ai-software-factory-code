-- a chat thread's open question, the line its last human message asked
-- (card #1345): carried once a reply has passed it without answering,
-- which the next turn's prompt carries with the instruction to answer it.
-- A later human question replaces the row; the reply that follows a
-- carried prompt removes it.
CREATE TABLE IF NOT EXISTS chat_open_questions (
    repository text NOT NULL,
    login text NOT NULL,
    conversation bigint NOT NULL,
    question text NOT NULL,
    carried boolean NOT NULL DEFAULT false,
    PRIMARY KEY (repository, login, conversation)
);
