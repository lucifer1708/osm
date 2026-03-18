APP     := osm
BIN     := ./bin/$(APP)
MAIN    := .
PORT    ?= 8081

.PHONY: all build run dev clean setup gen-password help

## Default target
all: build

## Build binary into ./bin/
build:
	@mkdir -p bin
	go build -o $(BIN) $(MAIN)
	@echo "Built → $(BIN)"

## Run from source (no binary)
run:
	PORT=$(PORT) go run $(MAIN)

## Dev mode: rebuild & restart on file changes (requires 'air')
dev:
	@command -v air >/dev/null 2>&1 || { \
		echo "Installing air..."; \
		go install github.com/air-verse/air@latest; \
	}
	air

## Build and run the binary
start: build
	$(BIN)

## First-time setup: copy .env.example → .env and download deps
setup:
	@if [ ! -f .env ]; then \
		cp .env.example .env; \
		echo "Created .env — fill in your credentials and run: make gen-password"; \
	else \
		echo ".env already exists, skipping copy"; \
	fi
	go mod download
	go mod tidy
	@echo ""
	@echo "Next steps:"
	@echo "  1. Edit .env  — add your storage credentials"
	@echo "  2. make gen-password  — generate AUTH_PASSWORD_HASH"
	@echo "  3. make run"

## Interactively generate a bcrypt password hash and print the .env line
gen-password:
	@go run ./cmd/gen-password

## Remove build artifacts
clean:
	rm -rf bin/
	go clean -cache

## Print help
help:
	@echo ""
	@echo "  make setup         First-time setup (copy .env.example, tidy deps)"
	@echo "  make gen-password  Generate bcrypt hash → paste into .env"
	@echo "  make run           Run from source  (PORT=$(PORT))"
	@echo "  make dev           Live-reload dev server (installs air if missing)"
	@echo "  make build         Compile binary to ./bin/osm"
	@echo "  make start         Build + run the binary"
	@echo "  make clean         Remove build artifacts"
	@echo ""
	@echo "  PORT=9090 make run   Override port at runtime"
	@echo ""
