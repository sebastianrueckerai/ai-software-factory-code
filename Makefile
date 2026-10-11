test:
	scripts/pg-test.sh go test -race -p 1 ./...

vet:
	go vet ./...

build:
	go build ./cmd/factoryd ./cmd/factory-gateway
