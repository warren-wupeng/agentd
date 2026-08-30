GO ?= go
BINARY := agentd-server

.PHONY: build test vet lint arch dev-up dev-down migrate run clean

build:
	$(GO) build -o bin/$(BINARY) ./cmd/agentd-server

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint: vet
	golangci-lint run ./...

arch:
	go-arch-lint check

dev-up:
	docker compose up -d postgres
	$(GO) run ./cmd/agentd-server migrate

dev-down:
	docker compose down -v

migrate:
	$(GO) run ./cmd/agentd-server migrate

run:
	$(GO) run ./cmd/agentd-server serve

clean:
	rm -rf bin/
