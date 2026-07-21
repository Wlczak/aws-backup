# AWS-backup

Self-contained Go binary with an embedded Svelte SPA that backs up files from a
local directory or SMB share to AWS S3 (Glacier Deep Archive), with a local
SQLite index, directory-level zipping, and a web UI for monitoring and control.

This project just a specific solution for my specific problem and while it is fairly functional now I do not claim it is reliable or safe for your data in any way. It has also been mostly vibe coded as I've determined this to be a good project to test LLM vibe coding on.

## Prerequisites

- Go 1.24+ (toolchain auto-downloads newer versions if needed)
- Node.js 18+ and npm (for building the Svelte SPA)
- Docker + Docker Compose (for the local MinIO dev backend)

## Quick start

- **1.** Download the release for your operating system. On Windows,
  double-click `aws-backup-windows-amd64.exe`; it starts the local server and
  opens the setup guide in your default browser.

- **2.** Create and then sign in with a login password, choose a local folder
  or SMB share, and connect an existing S3 bucket in the web guide. No
  config-file editing is required.

- **3.** Open the dashboard and start the first backup when ready. Backups are
  manual until a schedule is configured.

For development, run `make dev-up` to start MinIO, then `make build`.

Running `aws-backup` with no command starts the server and opens the browser.
Explicit `aws-backup serve` starts it without opening a browser. Both use
`SIGINT`/`SIGTERM` for graceful shutdown.

## CLI

```text
aws-backup [--config PATH] [command]

  (no command)      run the server and open the local web UI
  config init       write a default config.json (won't overwrite existing)
  config path       print the resolved config file path
  config validate   check the config is well-formed
  run               execute one backup run and exit
  serve             run the HTTP API + scheduler; auto-writes a starter config on first launch
  --version         print version
```

Config file location defaults to the OS user config dir:

- Linux: `~/.config/aws-backup/config.json`
- macOS: `~/Library/Application Support/aws-backup/config.json`
- Windows: `%AppData%\aws-backup\config.json`

Each profile's SQLite index lives under the same OS config directory. The
first-run web guide writes the configuration files on the user's behalf.

## Development

### Backend

```sh
make test          # go test ./...
make build-go      # go build only (assumes web/dist is populated)
make fmt vet tidy
```

### Frontend

The SPA lives in [web/](web/). During development it's easier to run the Vite
dev server separately so you get HMR:

```sh
# terminal 1: backend with live API
./aws-backup serve

# terminal 2: Vite dev server on :5173, /api proxied to :8080
make web-dev
# -> http://localhost:5173
```

`make build` runs `npm ci && npm run build` in `web/` before `go build` —
the resulting binary embeds the built SPA via `go:embed`, so you can ship
a single file with no external assets.

### Cross-compile

```sh
make build-linux    # -> aws-backup-linux-amd64
make build-windows  # -> aws-backup-windows-amd64.exe
```

Pure Go stack (modernc.org/sqlite) — no CGO required.

## Make targets

| Target                                    | What it does                                 |
| ----------------------------------------- | -------------------------------------------- |
| `make build`                              | Build SPA then Go binary                     |
| `make build-go`                           | Go-only build (assumes `web/dist` is ready)  |
| `make build-linux` / `make build-windows` | Cross-compile                                |
| `make test`                               | Run Go test suite                            |
| `make web`                                | Build the SPA into `web/dist/`               |
| `make web-dev`                            | Vite dev server (proxies `/api` to `:8080`)  |
| `make web-install`                        | Install npm deps without building            |
| `make dev`                                | Build + run `aws-backup serve`               |
| `make dev-up` / `make dev-down`           | Start/stop MinIO via docker compose          |
| `make dev-logs`                           | Tail MinIO logs                              |
| `make clean`                              | Remove built binaries + `web/dist` artifacts |
| `make fmt` / `make vet` / `make tidy`     | Standard Go housekeeping                     |

## Testing against real infrastructure

Some tests hit external services and auto-skip when unreachable:

**MinIO integration** (`internal/storage`):

```sh
make dev-up
go test ./internal/storage -run MinIO -v
```

Overrides: `AWS_BACKUP_TEST_S3_ENDPOINT`, `AWS_BACKUP_TEST_S3_BUCKET`,
`AWS_BACKUP_TEST_S3_KEY`, `AWS_BACKUP_TEST_S3_SECRET`.

**SMB integration** (`internal/source`):

```sh
AWS_BACKUP_TEST_SMB_HOST=192.168.1.10 \
AWS_BACKUP_TEST_SMB_SHARE=backup \
AWS_BACKUP_TEST_SMB_USER=user AWS_BACKUP_TEST_SMB_PASS=pass \
go test ./internal/source -run SMB -v
```

## Layout

```text
cmd/aws-backup/       CLI entry point
internal/
  config/             config.json load/save + validation
  db/                 sqlite schema, migrations, typed queries
  source/             Source interface + localdir + smb adapters
  storage/            Storage interface + S3 (MinIO/AWS) + in-memory fake
  engine/             backup orchestrator: scan -> zip -> upload
  events/             in-process pub/sub bus
  api/                chi router, JSON handlers, SSE, embedded SPA
  scheduler/          robfig/cron wrapper
web/                  Svelte 5 + Vite SPA (embedded via go:embed)
deploy/               docker-compose.yml for local MinIO
```
