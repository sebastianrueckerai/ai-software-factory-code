# Every outside image comes from the factory's store by the digest
# upstream-images.txt pins (roadmap 203). A pull request's build VM holds no
# login to the store, so pr.yml passes UPSTREAM= and the VM pulls the same
# digests from their upstreams.
ARG UPSTREAM=git.aisoftwarefactory.world/upstream/
FROM ${UPSTREAM}docker.io/library/golang:1.26.9-alpine@sha256:397ecc648150dd585f1734dfcd8d1bb4746664e54464191d77ad0ecffaf38bdb AS build
# roadmap 232: this stage's only clock is the commit's; ci.yml passes
# --build-arg SOURCE_DATE_EPOCH=<commit time> so a content-identical tree
# builds the same digest at any wall-clock time.
ARG SOURCE_DATE_EPOCH
ENV SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH
WORKDIR /src
COPY go.mod go.sum ./
# Go telemetry off before the first go call: its counter files carry the
# build's date, and `go telemetry off` would date its mode file too; an
# undated "off" is the same mode.
RUN mkdir -p "$HOME/.config/go/telemetry" && printf off > "$HOME/.config/go/telemetry/mode" && go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/factory-gateway ./cmd/factory-gateway \
 && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/factoryd ./cmd/factoryd

FROM ${UPSTREAM}docker.io/library/debian:bookworm-slim@sha256:f3034a6ec3c1205360777c4aae76234998866ad18806ae62b63a3f84ccad782b
LABEL org.opencontainers.image.source=https://git.aisoftwarefactory.world/sf/ai-software-factory-code
ARG DEBIAN_SNAPSHOT=20261015T000000Z
# git: factoryd's GitHub exports of mode pull_requests fetch and push with
# it; ca-certificates: every HTTPS call of both binaries. Debian's packages
# come from snapshot.debian.org at DEBIAN_SNAPSHOT, as in Dockerfile.runner,
# which says why; the install's clocks go with it.
RUN printf '%s\n' 'Acquire::Retries "8";' 'Acquire::http::Timeout "60";' 'Acquire::https::Timeout "60";' 'Acquire::Check-Valid-Until "false";' \
           > /etc/apt/apt.conf.d/80snapshot \
      && rm /etc/apt/sources.list.d/debian.sources \
      && printf '%s\n' "deb http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm main" \
           "deb http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm-updates main" \
           "deb http://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT} bookworm-security main" > /etc/apt/sources.list \
      && DEBIAN_FRONTEND=noninteractive apt-get update \
      && apt-get install -y --no-install-recommends git ca-certificates \
      && rm -rf /var/lib/apt/lists/* \
      && rm /var/cache/ldconfig/aux-cache /var/log/apt/history.log /var/log/apt/term.log /var/log/dpkg.log
# One image, both control-plane binaries; each deployment sets its command.
COPY --from=build /out/factory-gateway /usr/local/bin/factory-gateway
COPY --from=build /out/factoryd /usr/local/bin/factoryd
USER 65534:65534
EXPOSE 8081
ENTRYPOINT ["factory-gateway"]
