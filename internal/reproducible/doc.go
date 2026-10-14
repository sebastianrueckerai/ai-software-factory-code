// Package reproducible guards the determinism of the repository's images
// (roadmap 232): a commit determines its image, so every Dockerfile stage
// that runs a Go build pins SOURCE_DATE_EPOCH.
package reproducible
