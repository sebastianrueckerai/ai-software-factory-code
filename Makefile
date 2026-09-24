test:
	go test ./...

vet:
	go vet ./...

build:
	go build ./cmd/factoryd ./cmd/factory-gateway
