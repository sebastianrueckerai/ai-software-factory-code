package factoryd

import (
	"context"
	"log"
	"time"
)

// SourceUpstream is the source of the weekly card that asks a human to
// bump the factory's pinned outside inputs (roadmaps 203 and 242).
const SourceUpstream = "upstream"

// upstreamEvery is the upstream card's rhythm: one a week after the last
// was filed, and none while one is open.
const upstreamEvery = 7 * 24 * time.Hour

// The upstream card is a human's: an agent's sandbox reaches no registry,
// snapshot or release page to read a newer version or its digest from.
const (
	upstreamTitle = "Weekly bump of the pinned outside inputs"
	upstreamBody  = "The factory pins every outside input of its builds and its cluster (roadmaps 203 and 242): nothing moves until this card moves it. " +
		"Bump each to the newest release of its line in one pull request, and check what each item says:\n\n" +
		"- **Outside images**: `upstream-images.txt`, one line per image, its tag and its linux/amd64 digest " +
		"(`crane digest --platform linux/amd64 <image>:<tag>`). Every reference names the store's copy of a line: the Dockerfiles' " +
		"`FROM ${UPSTREAM}` lines and ci.yml's `BUILDKIT` here (`go test ./internal/reproducible` names a reference off the list), " +
		"the infra repository's manifests and HelmReleases there (its `scripts/upstream-images-test/test.sh`). A line marked bootstrap " +
		"(Forgejo, CNPG, cert-manager, local-path, Flux) is an upgrade of what Forgejo runs on: read its release notes first.\n" +
		"- **Debian snapshot**: `DEBIAN_SNAPSHOT` in Dockerfile.runner and Dockerfile.sandbox, one snapshot.debian.org time for both, " +
		"moved with the `debian:bookworm-slim` line.\n" +
		"- **PostgreSQL**: `POSTGRESQL_VERSION`, `LIBPQ_VERSION` and `POSTGRESQL_COMMON_VERSION` in both Dockerfiles, the versions " +
		"bookworm-pgdg serves now, which apt-archive.postgresql.org keeps.\n" +
		"- **Go toolchain**: the patch release of go.mod's `toolchain` and of the golang lines, one Go for all " +
		"(`TestDockerfilesBuildWithOneGo`).\n" +
		"- **Downloaded tools**: the version ARGs and the `ADD --checksum` sums of Dockerfile.runner and Dockerfile.sandbox, and " +
		"`buildkitVersion` and `buildkitSHA256` in internal/factoryd/buildvm.go with `BUILDKIT_VERSION`.\n\n" +
		"The pull request's image-build builds the changed images from the upstreams at the new digests. Its merge has main CI's " +
		"upstream job copy the new lines into the store; only then can the infra repository's references move to them, since a pod " +
		"whose copy is missing does not start.\n\nfiled by factoryd"
)

// upstream files the upstream card on the code repository when none is
// open and the last is a week old, or none was ever filed.
func (r *Roller) upstream(ctx context.Context) error {
	last, ok, err := r.Store.NewestTaskOf(ctx, r.Repo.String(), SourceUpstream)
	if err != nil {
		return err
	}
	if ok && (!Final(last.Status) || time.Since(last.CreatedAt) < upstreamEvery) {
		return nil
	}
	t := Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard", Objective: upstreamTitle + "\n\n" + upstreamBody,
		Source: SourceUpstream, Author: r.Forgejo.BotUser}
	t, number, err := fileHumanTask(ctx, r.Store, r.Forgejo, r.Repo, upstreamTitle, upstreamBody, t)
	if err != nil {
		return err
	}
	log.Printf("roller: upstream card %s, issue #%d", t.ID, number)
	return nil
}
