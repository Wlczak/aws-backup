# aws-backup

Self-contained Go binary with an embedded Svelte SPA that backs up files from a
local directory or SMB share to AWS S3 (Glacier Deep Archive), with a local
SQLite index, directory-level zipping, and a web UI for monitoring and control.

See [plan.md](plan.md) for the full design.

## Prerequisites

- Go 1.24+ (toolchain auto-downloads newer versions if needed)
- Node.js 18+ and npm (for building the Svelte SPA)
- Docker + Docker Compose (for the local MinIO dev backend)

## Quick start

```sh
# 1. Bring up MinIO (dev S3 backend on :9000, console on :9001)
make dev-up

# 2. Build the single binary (npm build + go build)
make build

# 3. Write a default config.json
./aws-backup config init
./aws-backup config path         # prints where it ended up

# 4. Edit the config — at minimum, set source.localdir.root to a
#    directory you want to back up. The default config is already
#    pointed at the local MinIO (endpoint: http://localhost:9000,
#    bucket: aws-backup-dev, creds: minioadmin / minioadmin).

# 5. Run the server (HTTP + scheduler) and open the UI
./aws-backup serve
# -> http://127.0.0.1:8080
```

`aws-backup serve` uses `SIGINT`/`SIGTERM` for graceful shutdown.

## CLI

```text
aws-backup [--config PATH] <command>

  config init       write a default config.json (won't overwrite existing)
  config path       print the resolved config file path
  config validate   check the config is well-formed
  run               execute one backup run and exit
  serve             run the HTTP API + scheduler (Ctrl-C to stop)
  --version         print version
```

Config file location defaults to the OS user config dir:

- Linux: `~/.config/aws-backup/config.json`
- macOS: `~/Library/Application Support/aws-backup/config.json`
- Windows: `%AppData%\aws-backup\config.json`

The SQLite index (`index.db`) lives next to `config.json`.

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

| Target                                    | What it does                                  |
| ----------------------------------------- | --------------------------------------------- |
| `make build`                              | Build SPA then Go binary                      |
| `make build-go`                           | Go-only build (assumes `web/dist` is ready)   |
| `make build-linux` / `make build-windows` | Cross-compile                                 |
| `make test`                               | Run Go test suite                             |
| `make web`                                | Build the SPA into `web/dist/`                |
| `make web-dev`                            | Vite dev server (proxies `/api` to `:8080`)   |
| `make web-install`                        | Install npm deps without building             |
| `make dev`                                | Build + run `aws-backup serve`                |
| `make dev-up` / `make dev-down`           | Start/stop MinIO via docker compose           |
| `make dev-logs`                           | Tail MinIO logs                               |
| `make clean`                              | Remove built binaries + `web/dist` artifacts  |
| `make fmt` / `make vet` / `make tidy`     | Standard Go housekeeping                      |

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

## Recommended bucket lifecycle rule

The resumable multipart path persists `UploadId` across runs but does
not actively abort orphans (a tmp that's deleted before the next run
or a config change to a new bucket leaves the upload stranded).
Configure a lifecycle rule on the backup bucket so abandoned multipart
uploads stop accruing storage cost:

```json
{
  "Rules": [
    {
      "ID": "aws-backup-abort-incomplete-mpu",
      "Status": "Enabled",
      "Filter": {},
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 7 }
    }
  ]
}
```

Apply via the AWS console or:

```sh
aws s3api put-bucket-lifecycle-configuration \
  --bucket your-backup-bucket \
  --lifecycle-configuration file://lifecycle.json
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

## Status

The whole pipeline is wired against **MinIO** by default — no real AWS calls
happen until you change `s3.endpoint` in `config.json`. The Glacier restore
trigger (`POST /api/restore/trigger`) returns `503` until that gate is lifted.
