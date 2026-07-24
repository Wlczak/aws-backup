# CLI + Dev Workflow

## CLI Subcommands

```text
aws-backup config init        write a default config.json (won't overwrite)
aws-backup config path        print the resolved config file path
aws-backup config validate    check config well-formedness
aws-backup passwd             prompt twice, hash the password, and save auth.password_hash
aws-backup run                execute one backup run, then exit
aws-backup serve              run HTTP API + scheduler (SIGINT-safe shutdown); auto-writes a starter config on first launch
aws-backup                    same server mode, then open the local UI in the default browser
aws-backup -version           print baked-in version
```

`-config <path>` overrides the central config location (resolved via `config.DefaultPath()` — see `internal/config/path.go`). `-profile <name>` overrides the active profile for the current `run` / `serve` process. Version is baked in via `-ldflags "-X main.version=..."` in the Makefile.

`serve` startup includes a transient HTTP boot UI on the configured port that shows progress with a Cancel button while the active profile's `index.db` is downloaded from S3 if needed; the transient server is shut down before the real API binds the port (#143).
On a fresh or passwordless install, startup skips source/storage construction, remote-index refresh, scheduler, and SQS so the real API can serve the onboarding guide with incomplete settings. Running with no command opens that guide automatically after the listener binds; explicit `serve` remains non-launching for scripts and services.

## Web SPA Notes

- Built by Vite into `web/dist/`, embedded via `web/embed.go`'s `go:embed`
- Hash router; any path that doesn't resolve under `dist/` falls back to `index.html`
- `web/src/lib/api.ts` is the typed fetch wrapper layer — keep `Config`, `SettingsResponse`, etc. in sync with `internal/config` and the API handlers
- `web/src/lib/api.ts` also owns the login/logout/status helpers and the `401` handler hook used to fall back to the sign-in screen
- The shared API wrapper increments `web/src/lib/api-activity.ts`; `ApiActivity.svelte` shows one delayed global busy cue for requests lasting at least 200 ms, avoiding route-specific loading gaps and fast-poll flicker
- `Onboarding.svelte` owns the password → explicit login → source → S3 → completion flow; each resource step persists through the normal settings API and tests connectivity before advancing
- `web/src/lib/toast.ts` is the canonical path for transient feedback (avoid `alert()`)
- `npm run lint` runs ESLint across the frontend `js`/`ts`/`svelte` sources; CI includes it in the lint job alongside the Go checks
- Settings is split into per-section sub-routes under `/settings/<section>`; each sub-component takes `bind:cfg` and edits a slice of the Config tree
- Profiles are managed on the dedicated `#/profiles` page. The header's "Switch profile" button links there; the page handles active switching plus create/rename/delete actions.
- Restore is split into a `Restore` tab for Glacier thaw/status and a separate `Download` tab for local S3 downloads with MD5 verification + cost estimation; the target directory is prefilled from `backup.download_dir` when Settings has one configured

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

## Release Workflow

Tag pushes that start with `v` trigger `.github/workflows/release.yml`, which builds the embedded web bundle, cross-compiles the release binaries, and publishes them as GitHub release assets alongside `SHA256SUMS`.

The Settings → About page consumes those raw assets for self-update. `SHA256SUMS` must contain asset basenames; the updater also accepts the legacy `out/<asset>` entries published before issue #389. Server boot checks are optional and non-blocking, while executable replacement always requires operator confirmation and an idle process.

## Local dev stack

`deploy/docker-compose.yml` brings up MinIO on `:9000` (S3 endpoint) + `:9001` (console) and a one-shot init container that creates the bucket. Combined with `web-dev` (Vite on `:5173` proxying to the Go server on `:8080`) this gives full hot-reload on both sides.
