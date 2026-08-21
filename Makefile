SERVICE := masterdata-service

.PHONY: tools proto build run test test-integration lint tidy swagger seed seed-dry docker

# Install the development tooling this repo needs.
tools:
	go install github.com/bufbuild/buf/cmd/buf@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	go install github.com/swaggo/swag/cmd/swag@v1.16.4
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

# Regenerate the gRPC bindings from proto/.
#
# The contracts are duplicated across the service repos by design. When one
# changes, copy the updated .proto into every repo that speaks it and rerun
# this target there — otherwise the services drift apart silently, which is the
# cost this layout trades for full independence.
proto:
	cd proto && buf lint && buf generate
	go mod tidy

build:
	go build -o bin/server ./cmd/server
	go build -o bin/seed ./cmd/seed

run:
	go run ./cmd/server

test:
	go test ./... -race

# Regenerate the OpenAPI document from the handler annotations.
# Requires: go install github.com/swaggo/swag/cmd/swag@v1.16.4
swagger:
	swag init -g cmd/server/main.go -o docs --parseDependency --parseInternal
	gofmt -w docs

# Integration tests run against a real database. They carry a build tag, so
# `make test` never picks them up, and they skip themselves when MONGO_TEST_URI is
# unset. `make -C .. test-integration-up` starts throwaway containers.
MONGO_TEST_URI ?= mongodb://localhost:27017

test-integration:
	MONGO_TEST_URI="$(MONGO_TEST_URI)" go test -tags=integration ./tests/integration/... -race -v

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

# seed-dry reports what would be written and touches nothing. Run this first.
seed-dry:
	go run ./cmd/seed -file seed/catalog.json

# seed writes the global catalogues. It refuses any target that looks like the
# legacy production database, and only writes to md_-prefixed collections.
seed:
	go run ./cmd/seed -file seed/catalog.json -confirm

docker:
	docker build -t karlo/$(SERVICE):latest .
