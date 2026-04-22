BINARY      := aws-backup
CMD         := ./cmd/aws-backup
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS     := -X main.version=$(VERSION)
COMPOSE     := docker compose -f deploy/docker-compose.yml
WEB_SRC     := $(shell find web/src web/index.html web/vite.config.ts web/package.json web/package-lock.json 2>/dev/null -type f)

.PHONY: all build build-linux build-windows build-go test tidy run clean dev dev-up dev-down dev-logs fmt vet web web-dev web-install

all: build

# Full build: frontend first, then the Go binary (embeds web/dist).
build: web build-go

# Go-only build; assumes web/dist is already populated.
build-go:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

build-linux: web
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 $(CMD)

build-windows: web
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-windows-amd64.exe $(CMD)

# Build the SPA into web/dist/.
web: web/dist/index.html

web/dist/index.html: $(WEB_SRC)
	cd web && npm ci --prefer-offline --no-audit && npm run build

# Install the npm deps without building.
web-install:
	cd web && npm install

# Run the Vite dev server (proxies /api to :8080).
web-dev:
	cd web && npm run dev

# Run `aws-backup serve` against the default config.
dev: build
	./$(BINARY) serve

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
	rm -rf web/dist/assets web/dist/index.html
	rm -f *.db *.db-shm *.db-wal

dev-up:
	$(COMPOSE) up -d
	@echo "MinIO console: http://localhost:9001 (minioadmin / minioadmin)"

dev-down:
	$(COMPOSE) down -v

dev-logs:
	$(COMPOSE) logs -f
