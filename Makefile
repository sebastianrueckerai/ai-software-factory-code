test:
	scripts/pg-test.sh go test -race -p 1 -timeout 30m ./...

vet:
	go vet ./...

build:
	go build ./cmd/factoryd ./cmd/factory-gateway
