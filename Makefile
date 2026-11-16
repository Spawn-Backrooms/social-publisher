GO ?= go
BINARY ?= bin/publisher
IMAGE ?= social-publisher:local
BUILD_REVISION ?= development

.PHONY: help build test test-race test-postgres vet keygen migrate serve api worker image compose-db compose-migrate compose-up compose-down

help:
	@printf '%s\n' \
	  'make build          Build the native static publisher binary' \
	  'make test           Run tests; database tests skip without TEST_DATABASE_URL' \
	  'make test-race      Run the Go race detector (requires a C compiler)' \
	  'make test-postgres  Run the native PostgreSQL release gate; rejects skipped tests' \
	  'make vet            Run go vet' \
	  'make keygen         Print fresh signing, encryption, operations and control keys' \
	  'make migrate        Apply embedded migrations using exported DATABASE_URL' \
	  'make serve          Run API, publishing workers, and token maintenance' \
	  'make api            Run HTTP API only' \
	  'make worker         Run workers and token maintenance only' \
	  'make image          Build the Docker image' \
	  'make compose-db     Start local PostgreSQL' \
	  'make compose-migrate Apply migrations using the local Compose database' \
	  'make compose-up     Start the combined service (migrate first)' \
	  'make compose-down   Stop local containers; keep the database volume'

build:
	mkdir -p $(dir $(BINARY))
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-X socialpublisher/internal/buildinfo.Revision=$(BUILD_REVISION)" -o $(BINARY) ./cmd/publisher

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-postgres:
	GO=$(GO) ./scripts/verify/native-postgres.sh

vet:
	$(GO) vet ./...

keygen: build
	$(BINARY) keygen

migrate: build
	$(BINARY) migrate

serve: build
	$(BINARY) serve

api: build
	$(BINARY) api

worker: build
	$(BINARY) worker

image:
	docker build --build-arg BUILD_REVISION=$(BUILD_REVISION) -t $(IMAGE) .

compose-db:
	docker compose up -d postgres

compose-migrate:
	docker compose run --rm migrate

compose-up:
	docker compose up -d publisher

compose-down:
	docker compose down
