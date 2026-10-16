// Package reproducible guards the determinism of the repository's images
// (roadmaps 232 and 242): a commit determines its image, so every
// Dockerfile stage that runs a Go build pins SOURCE_DATE_EPOCH, no step
// leaves a clock of the build in a file, and every outside input is
// pinned: apt sources by a date or a version, images by a digest of
// upstream-images.txt, downloads by the sha256 lines of
// upstream-files.txt, taken from the factory's store (roadmap 203).
package reproducible
