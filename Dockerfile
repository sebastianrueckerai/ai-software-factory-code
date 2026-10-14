FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
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

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
LABEL org.opencontainers.image.source=https://git.aisoftwarefactory.world/sf/ai-software-factory-code
# One image, both control-plane binaries; each deployment sets its command.
COPY --from=build /out/factory-gateway /usr/local/bin/factory-gateway
COPY --from=build /out/factoryd /usr/local/bin/factoryd
# git: factoryd's GitHub exports of mode pull_requests fetch and push with it
RUN apk add --no-cache git
USER 65534:65534
EXPOSE 8081
ENTRYPOINT ["factory-gateway"]
