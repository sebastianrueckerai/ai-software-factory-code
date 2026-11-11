-- The brief writer reads a case from its record and files the prose of the face's parts 3 and 4 on it.
ALTER TABLE cases
  ADD COLUMN brief text NOT NULL DEFAULT '',
  ADD COLUMN brief_detail text NOT NULL DEFAULT '',
  ADD COLUMN brief_error text NOT NULL DEFAULT '';
