-- tasks gain their sandbox image: image is the project image a plan card
-- names, '' for the project's default; image_digest is the image,
-- ref@sha256:..., the task's latest sandbox ran on and image_egress the
-- registries that sandbox reached, which the lease loop keeps in its
-- egress policy; '' and none before its first.
ALTER TABLE tasks ADD COLUMN image text NOT NULL DEFAULT '', ADD COLUMN image_digest text NOT NULL DEFAULT '',
  ADD COLUMN image_egress jsonb NOT NULL DEFAULT '[]';
