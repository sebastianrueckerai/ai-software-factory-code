// Package reproducible guards the determinism of the repository's images
// (roadmaps 232 and 242): a commit determines its image, so every
// Dockerfile stage that runs a Go build pins SOURCE_DATE_EPOCH, and no step
// leaves a clock of the build in a file.
package reproducible
