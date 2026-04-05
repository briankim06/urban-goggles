.PHONY: infra-up infra-down proto build run-ingestor run-processor run-server test

COMPOSE_FILE := deployments/docker-compose.yml
PROTO_DIR := proto
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

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
	go run ./cmd/ingestor

run-processor:
	go run ./cmd/processor

run-server:
	go run ./cmd/server

test:
	go test ./... -v
