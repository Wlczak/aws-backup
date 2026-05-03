# Project Instructions

## Index semantics

The SQLite index in this project represents the **state of the S3 bucket**, not the state of the local source directory. When a file is deleted from the source, it transitions to `status = 'missing'` and **must remain in the index** until it is also deleted from S3. Do not add code, endpoints, or UI controls that purge `missing` rows based solely on source-side absence — that desyncs the index from the bucket.

## Updating these instructions

Whenever the user says "remember …", or otherwise shares important project information (architecture invariants, conventions, gotchas, decisions), update `CLAUDE.md` in the same turn so the rule is durable across sessions. Do not rely on conversation memory alone for project-level rules.
