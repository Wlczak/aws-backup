# Project Instructions

## Index semantics

The SQLite index in this project represents the **state of the S3 bucket**, not the state of the local source directory. When a file is deleted from the source, it transitions to `status = 'missing'` and **must remain in the index** until it is also deleted from S3. Do not add code, endpoints, or UI controls that purge `missing` rows based solely on source-side absence — that desyncs the index from the bucket.

## plan.md is the project briefing

`plan.md` at the repo root is the authoritative record of architecture decisions, the issue-handling workflow, and the current state of open work. Read it at the start of every session in this repo before answering questions or making changes — it replaces re-deriving project state from `git log` or directory structure each time. When a change you make alters something plan.md describes (architecture, conventions, open issues), update plan.md in the same turn.

## Web feedback UI

Transient success/error feedback in the web app goes through the toast system (`web/src/lib/toast.ts` + `web/src/components/Toaster.svelte`, mounted once in `App.svelte`). Use `toast.success(...)` / `toast.error(...)` / `toast.info(...)` instead of adding new inline alert banners or per-component `err`/`msg` state. Persistent status that belongs next to a specific value (e.g., a file's backup state) still uses inline UI like `StatusBadge`.

## Updating these instructions

Whenever the user says "remember …", or otherwise shares important project information (architecture invariants, conventions, gotchas, decisions), update `CLAUDE.md` in the same turn so the rule is durable across sessions. Do not rely on conversation memory alone for project-level rules.
