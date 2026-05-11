# CLI + Dev Workflow

## CLI Subcommands

```text
aws-backup config init        write a default config.json (won't overwrite)
aws-backup config path        print the resolved config file path
aws-backup config validate    check config well-formedness
aws-backup run                execute one backup run, then exit
aws-backup serve              run HTTP API + scheduler (SIGINT-safe shutdown)
aws-backup -version           print baked-in version
```

`-config <path>` overrides the default config location (resolved via `config.DefaultPath()` — see `internal/config/path.go`). Version is baked in via `-ldflags "-X main.version=..."` in the Makefile.

`serve` startup includes a transient HTTP boot UI on the configured port that shows progress with a Cancel button while `index.db` is downloaded from S3 if needed; the transient server is shut down before the real API binds the port (#143).

## Web SPA Notes

- Built by Vite into `web/dist/`, embedded via `web/embed.go`'s `go:embed`
- Hash router; any path that doesn't resolve under `dist/` falls back to `index.html`
- `web/src/lib/api.ts` is the typed fetch wrapper layer — keep `Config`, `SettingsResponse`, etc. in sync with `internal/config` and the API handlers
- `web/src/lib/toast.ts` is the canonical path for transient feedback (avoid `alert()`)
- Settings is split into per-section sub-routes under `/settings/<section>`; each sub-component takes `bind:cfg` and edits a slice of the Config tree
- Restore is split into a `Restore` tab for Glacier thaw/status and a separate `Download` tab for local S3 downloads with MD5 verification

## Makefile targets

```text
build         build SPA + Go binary (web → web/dist → embed → go build)
build-go      Go-only (assumes web/dist is populated)
build-linux   GOOS=linux GOARCH=amd64
build-windows GOOS=windows GOARCH=amd64
web           Vite production build
web-install   npm install only
web-dev       Vite dev server (proxies /api → :8080)
dev           build + ./aws-backup serve
test          go test ./...
tidy          go mod tidy
fmt vet vuln  formatting + go vet + govulncheck
run           build + ./aws-backup (no args)
clean         remove binaries + dist/
dev-up        docker-compose up MinIO + bucket-init
dev-down      docker-compose down
dev-logs      docker-compose logs -f
```

Builds use `-trimpath` for reproducibility and to keep developer paths out of panics/DWARF (#80).

## Local dev stack

`deploy/docker-compose.yml` brings up MinIO on `:9000` (S3 endpoint) + `:9001` (console) and a one-shot init container that creates the bucket. Combined with `web-dev` (Vite on `:5173` proxying to the Go server on `:8080`) this gives full hot-reload on both sides.
