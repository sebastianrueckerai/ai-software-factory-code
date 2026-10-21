-- the intake reads the console's card_filed row of an issue factory-chat
-- posted, for the member the project agent filed it for
CREATE INDEX IF NOT EXISTS events_card_filed_idx ON events (repository, issue_number) WHERE kind = 'card_filed';
