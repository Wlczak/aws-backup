BINARY      := aws-backup
CMD         := ./cmd/aws-backup
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS     := -X main.version=$(VERSION)
COMPOSE     := docker compose -f deploy/docker-compose.yml

.PHONY: all build test tidy run clean dev-up dev-down dev-logs fmt vet

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 $(CMD)

build-windows:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-windows-amd64.exe $(CMD)

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY) $(BINARY)-*-amd64 $(BINARY)-*-amd64.exe
	rm -rf web/dist
	rm -f *.db *.db-shm *.db-wal

dev-up:
	$(COMPOSE) up -d
	@echo "MinIO console: http://localhost:9001 (minioadmin / minioadmin)"

dev-down:
	$(COMPOSE) down -v

dev-logs:
	$(COMPOSE) logs -f
