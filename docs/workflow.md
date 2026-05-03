# Workflow + Conventions

## Issue Handling Workflow

When fixing a GitHub issue:

1. `gh issue view <N> --json title,body,labels,comments`
2. Implement the fix with a test where applicable
3. `go build ./... && go test ./...` (add `-race` for concurrency-touching changes)
4. Commit with the issue number in the title, e.g. `Fix foo (#N)`
5. Close the issue with a SHA citation:

   ```bash
   gh issue close <N>
   gh issue comment <N> --body "Fixed in <SHA> — <one-line summary>"
   ```

6. Update the relevant doc under `docs/` if the fix changes architecture, the public API, or a workflow. Add a row to `docs/changelog.md`.

## Conventions

- New Go file → start with a one-sentence package doc on whichever file is its canonical owner (see `internal/config/config.go` for the pattern). Don't add a doc comment to every file
- Tests for any concurrency-touching change run `go test -race`
- Don't create planning, decision, or analysis docs unless asked; the `docs/` directory plus `docs-guide.md` is the single doc of record
- Keep the index in sync with the bucket: never purge `missing` rows on source-side absence alone (`CLAUDE.md`)

## Updating these instructions

Whenever the user says "remember …", or otherwise shares important project information (architecture invariants, conventions, gotchas, decisions), update `CLAUDE.md` in the same turn so the rule is durable across sessions. Do not rely on conversation memory alone for project-level rules.

If the new rule also belongs in the topical project docs (anything that affects architecture, the API, the data model, the engine lifecycle, the config schema, the dev workflow, etc.), update the matching file under `docs/` in the same turn — `CLAUDE.md` is for cross-cutting harness rules; `docs/*.md` is where future readers go for details. When both apply, write the rule once in the right doc and keep the `CLAUDE.md` entry as a short pointer.
