.PHONY: build test lint vet bench fuzz up down staticcheck

BINARY_GATEWAY   := bin/gateway
BINARY_FAKE      := bin/fakeupstream
GO_FLAGS         := -trimpath
GOTEST           := go test -timeout 120s

## build: compile both binaries
build:
	go build $(GO_FLAGS) -o $(BINARY_GATEWAY)   ./cmd/gateway
	go build $(GO_FLAGS) -o $(BINARY_FAKE)       ./cmd/fakeupstream

## test: run all unit tests (no Docker required)
test:
	$(GOTEST) ./internal/...

## integration: run integration tests (requires Docker)
integration:
	$(GOTEST) -v ./integration/...

## fuzz: run all fuzz targets for 10 seconds each
fuzz:
	go test ./internal/cache/... -fuzz=FuzzCacheKey -fuzztime=10s

## lint: vet + staticcheck
lint: vet staticcheck

## vet: run go vet
vet:
	go vet ./...

## staticcheck: run staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
staticcheck:
	staticcheck ./...

## bench: run Go micro-benchmarks
bench:
	$(GOTEST) -bench=. -benchmem -run='^$$' ./internal/...

## up: start gateway + redis + fakeupstream via docker compose
up:
	docker compose up --build -d

## down: stop all services
down:
	docker compose down

## race-docker: run race-detector tests inside golang:1.26 container
## (Windows host lacks gcc; race detector needs 64-bit gcc)
race-docker:
	docker run --rm -v $(shell pwd):/src -w /src golang:1.26 \
	    go test -race -timeout 120s ./internal/...

## tidy: clean up go.mod and go.sum
tidy:
	go mod tidy

## clean: remove build outputs
clean:
	rm -rf bin/
