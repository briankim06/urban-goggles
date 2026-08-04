.PHONY: infra-up infra-down proto build run-ingestor run-processor run-server test download-static

COMPOSE_FILE := deployments/docker-compose.yml
PROTO_DIR := proto
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

# All time-of-day math (schedule matching, headways, transfer margins) uses
# the process-local timezone, which must match the agency's. Override with
# e.g. `make run-server TZ=America/Chicago`.
TZ ?= America/New_York

infra-up:
	docker compose -f $(COMPOSE_FILE) up -d

infra-down:
	docker compose -f $(COMPOSE_FILE) down

proto:
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=. \
		--go_opt=module=github.com/briankim06/urban-goggles \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/briankim06/urban-goggles \
		$(PROTO_DIR)/*.proto

build:
	go build ./...

run-ingestor:
	TZ=$(TZ) go run ./cmd/ingestor

run-processor:
	TZ=$(TZ) go run ./cmd/processor

run-server:
	TZ=$(TZ) go run ./cmd/server

test:
	go test ./... -v

download-static:
	./scripts/download_static.sh
