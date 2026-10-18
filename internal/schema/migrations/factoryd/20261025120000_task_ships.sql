-- a build card citing a design ruling states which clauses of the
-- ruling's screen after it ships (the design decisions' C-entries of
-- the project's document repository): the issue's "Ships: C20 clauses
-- 1, 3" line, read at intake into ships_ruling and ships_clauses,
-- 1-based in the entry's order. The review gate unions the clause
-- lists of every card citing the ruling and fails the card when a
-- clause of the screen after is left to no card.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ships_ruling text NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS ships_clauses jsonb NOT NULL DEFAULT '[]';
