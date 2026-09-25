FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/factory-gateway ./cmd/factory-gateway

FROM alpine:3.22
LABEL org.opencontainers.image.source=https://git.aisoftwarefactory.world/sf/ai-software-factory-code
COPY --from=build /out/factory-gateway /usr/local/bin/factory-gateway
USER 65534:65534
EXPOSE 8081
ENTRYPOINT ["factory-gateway"]
