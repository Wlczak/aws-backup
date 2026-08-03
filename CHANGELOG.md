# Changelog

Notable changes to aws-backup are documented here. Releases are scoped by Git
tags; issue lists include every GitHub issue closed by changes in that release.

## [v0.3.1] - 2026-08-01

### Fixed

- Prevented duplicate uploads when the local database has lost records of files
  already stored in zip archives. Before planning uploads, the engine now recovers
  pending rows only when a valid zip sidecar contains the exact source-relative
  path and current file size.
- Changed local files are not rebound to stale archives when their indexed size
  differs. Legacy path-only sidecars also no longer suppress uploads because they
  cannot prove that the archived member is the current version.

### Closed issues

- [#411](https://github.com/Wlczak/aws-backup/issues/411) — Prevent duplicate
  uploads after local upload-index data is lost.

### Maintenance

- Updated the AWS SQS SDK, `go-chi`, Go security/system modules, Vite, the Svelte
  Vite plugin, and TypeScript ESLint tooling.
- Updated the transitive `brace-expansion` package from 5.0.7 to 5.0.8 through
  `npm audit fix`.

## [v0.3.0] - 2026-07-24

### Highlights

- Added an in-app update checker and operator-confirmed self-update flow under
  Settings → About. Updates select the correct platform binary, verify it against
  `SHA256SUMS`, and can restart the service or shut it down after installation.
- Added a guided first-run web setup for creating the initial password and
  configuring the source and S3 destination. A new installation can now be
  completed without the previous CLI-first setup flow.
- Improved double-click/no-command startup so desktop users are taken directly to
  login or onboarding, while established command-line workflows remain compatible.

### Fixed

- Continuing a gracefully stopping backup now resumes pending uploads within the
  same run instead of allowing the run to finish as stopped.
- SMB paths containing Windows backslashes are normalized to share-relative paths,
  preventing valid files from being rejected for having a leading separator.
- Self-update restart and shutdown now preserve the executable path, drain active
  event streams promptly, and remain compatible with older release binaries.
- Creating the first password clears any existing session and requires an explicit
  login before onboarding can access protected settings.

### Closed issues

- [#368](https://github.com/Wlczak/aws-backup/issues/368) — Create an onboarding
  setup guide to replace the CLI-based setup.
- [#369](https://github.com/Wlczak/aws-backup/issues/369) — Fix backslash
  compatibility for SMB connections.
- [#384](https://github.com/Wlczak/aws-backup/issues/384) — Fix upload resume during
  graceful stop.
- [#389](https://github.com/Wlczak/aws-backup/issues/389) — Implement self-update.

### Maintenance

- Updated Go and web dependencies and added `npm audit` to CI.

[v0.3.1]: https://github.com/Wlczak/aws-backup/compare/v0.3.0...v0.3.1
[v0.3.0]: https://github.com/Wlczak/aws-backup/compare/v0.2.0...v0.3.0
