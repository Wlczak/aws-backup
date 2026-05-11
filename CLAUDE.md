# Project Instructions

## Index semantics

The SQLite index in this project represents the **state of the S3 bucket**, not the state of the local source directory. When a file is deleted from the source, it transitions to `status = 'missing'` and **must remain in the index** until it is also deleted from S3. Do not add code, endpoints, or UI controls that purge `missing` rows based solely on source-side absence — that desyncs the index from the bucket.

## docs-guide.md is the project briefing

`docs-guide.md` at the repo root is the index to the per-topic project docs under `docs/`. Read it at the start of every session in this repo before answering questions or making changes — it tells you which detail files (`docs/architecture.md`, `docs/api.md`, `docs/engine.md`, etc.) are worth opening for the current task, so you can be selective rather than re-deriving state from `git log` or directory structure. This replaces the old single-file `plan.md`. When a change you make alters something a doc describes, update the relevant doc in the same turn (and add a `docs/changelog.md` row if it's a noteworthy fix).

## Commit + push when work lands

After finishing a task that produces a working-tree change, commit and `git push` to `origin/main` in the same turn — don't leave dirty state for the user. Full rule + exceptions live in `docs/workflow.md`.

## Web feedback UI

Transient success/error feedback in the web app goes through the toast system (`web/src/lib/toast.ts` + `web/src/components/Toaster.svelte`, mounted once in `App.svelte`). Use `toast.success(...)` / `toast.error(...)` / `toast.info(...)` instead of adding new inline alert banners or per-component `err`/`msg` state. Persistent status that belongs next to a specific value (e.g., a file's backup state) still uses inline UI like `StatusBadge`.

## Updating these instructions

Whenever the user says "remember …", or otherwise shares important project information (architecture invariants, conventions, gotchas, decisions), update `CLAUDE.md` in the same turn so the rule is durable across sessions. Do not rely on conversation memory alone for project-level rules.

If the new rule also belongs in the topical project docs (anything that affects architecture, the API, the data model, the engine lifecycle, the config schema, the dev workflow, etc.), update the matching file under `docs/` in the same turn — `CLAUDE.md` is for cross-cutting harness rules; `docs/*.md` is where future readers go for details. When both apply, write the rule once in the right doc and keep the `CLAUDE.md` entry as a short pointer.

## Commiting

When you commit new changes always mention yourself (your current model) as a co-author.
