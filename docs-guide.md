# Docs Guide

> **Read this file at the start of every session.** It is the index to the project's documentation. The detail pages live under `docs/`; read only the ones relevant to your task.

This project's docs are split by topic so a session can pull only what it needs. `docs-guide.md` (this file) is small enough to read every time. The detail files under `docs/` are the authoritative project reference; the git log is the authoritative record of what changed and when.

## What's in each doc

| File | Read it when you need… |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | High-level overview, repository tree, per-package responsibilities, key design decisions, Go dependencies |
| [docs/data-model.md](docs/data-model.md) | SQLite schema (`files` / `runs` / `run_logs` / `settings`), file-status transitions, zip naming + sidecar convention |
| [docs/config.md](docs/config.md) | Full `config.json` schema, hot-reload + deferred-apply semantics, default config path resolution |
| [docs/api.md](docs/api.md) | HTTP endpoint table and SSE event catalogue |
| [docs/engine.md](docs/engine.md) | Backup-run lifecycle (scan → reconcile → pipeline → finalize), `api.Server` run-state concurrency, restore subsystem |
| [docs/cli-and-dev.md](docs/cli-and-dev.md) | CLI subcommands, web SPA notes, Makefile targets, MinIO dev stack |
| [docs/workflow.md](docs/workflow.md) | Issue-handling workflow, project conventions |
| [docs/changelog.md](docs/changelog.md) | Closed-issue table — searchable record of past architectural calls |

## How to use this guide

1. Read `CLAUDE.md` and this file at session start (already cheap — they're short).
2. From the user's request, pick the docs above that match the area of code being touched. Read those before searching code.
3. Don't read all of `docs/` by reflex; the split exists so you can be selective.
4. When you change something the docs describe, update the relevant doc in the same turn. Add a row to `docs/changelog.md` for noteworthy fixes.
