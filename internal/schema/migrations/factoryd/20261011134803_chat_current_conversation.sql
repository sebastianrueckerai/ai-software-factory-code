-- chat_current_conversation tracks each member's current conversation on
-- each project so NewConversation can start a new one without waiting for
-- a message row. The row is upserted on every NewConversation call.
CREATE TABLE IF NOT EXISTS chat_current_conversation (
	repository text NOT NULL,
	login text NOT NULL,
	conversation bigint NOT NULL,
	PRIMARY KEY (repository, login)
);
