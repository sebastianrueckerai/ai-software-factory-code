FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/factory-gateway ./cmd/factory-gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/factoryd ./cmd/factoryd

FROM alpine:3.22
LABEL org.opencontainers.image.source=https://git.aisoftwarefactory.world/sf/ai-software-factory-code
# One image, both control-plane binaries; each deployment sets its command.
COPY --from=build /out/factory-gateway /usr/local/bin/factory-gateway
COPY --from=build /out/factoryd /usr/local/bin/factoryd
USER 65534:65534
EXPOSE 8081
ENTRYPOINT ["factory-gateway"]
