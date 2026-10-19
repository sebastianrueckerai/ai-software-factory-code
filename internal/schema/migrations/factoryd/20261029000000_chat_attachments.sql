-- The chat's images (roadmap 300's attachment spine, images first): a
-- member's message row carries its images as their own rows, named img1..
-- by arrival, the bytes themselves in the object store under
-- attachments/{conversation}/{id} — the table holds the meta only.

CREATE TABLE IF NOT EXISTS chat_attachments (
	id bigserial PRIMARY KEY,
	repository text NOT NULL,
	login text NOT NULL,
	conversation bigint NOT NULL,
	message_id bigint NOT NULL,
	name text NOT NULL,
	file_name text NOT NULL,
	content_type text NOT NULL,
	bytes bigint NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS chat_attachments_thread ON chat_attachments (repository, login, conversation, id);
