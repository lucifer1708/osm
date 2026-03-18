APP  := osm
BIN  := ./bin/$(APP)
PORT ?= 8081

.PHONY: all build run dev start clean setup create-user help

all: build

## Compile binary → ./bin/osm
build:
	@mkdir -p bin
	go build -o $(BIN) .
	@echo "Built → $(BIN)"

## Run from source (loads .env automatically)
run:
	PORT=$(PORT) go run .

## Live-reload dev server (installs air if missing)
dev:
	@command -v air >/dev/null 2>&1 || { echo "Installing air..."; go install github.com/air-verse/air@latest; }
	air

## Build then run the binary
start: build
	$(BIN)

## First-time setup: copy .env.example and tidy deps
setup:
	@if [ ! -f .env ]; then \
		cp .env.example .env; \
		echo "✓ Created .env — fill in your storage credentials"; \
	else \
		echo ".env already exists, skipping copy"; \
	fi
	go mod download
	go mod tidy
	@mkdir -p data
	@echo ""
	@echo "Next steps:"
	@echo "  1. Edit .env       — add your storage endpoint + keys"
	@echo "  2. make create-user — create your first admin account"
	@echo "  3. make run"

## Interactively create a user in the SQLite database
create-user:
	@go run ./cmd/create-user

## Remove build artifacts (keeps database)
clean:
	rm -rf bin/
	go clean -cache

## Print usage
help:
	@echo ""
	@echo "  make setup        First-time setup (copy .env, tidy deps)"
	@echo "  make create-user  Add a user to the database"
	@echo "  make run          Run from source   (PORT=$(PORT))"
	@echo "  make dev          Live-reload with air"
	@echo "  make build        Compile → ./bin/osm"
	@echo "  make start        Build + run binary"
	@echo "  make clean        Remove build artifacts"
	@echo ""
	@echo "  PORT=9090 make run    override port"
	@echo "  DB_PATH=/tmp/x.db make run    custom DB path"
	@echo ""
