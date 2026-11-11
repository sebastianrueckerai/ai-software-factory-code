-- A case's signal to its card's run that no send delivered yet: Act's
-- case_resolved, written with the resolution, and a face's case_faced,
-- written with the face's comment. A delivered send deletes the row, and
-- the heal pass sends a pending one again, tries counting its failures.
CREATE TABLE case_signals (
	case_id bigint NOT NULL REFERENCES cases (id) ON DELETE CASCADE,
	signal text NOT NULL CHECK (signal IN ('case_resolved', 'case_faced')),
	task_id text NOT NULL,
	payload jsonb NOT NULL,
	pending_since timestamptz NOT NULL DEFAULT now(),
	tries int NOT NULL DEFAULT 0,
	PRIMARY KEY (case_id, signal)
);
