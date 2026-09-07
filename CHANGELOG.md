# Changelog

## Unreleased

**Highlights:** Keep SQLite paired with its supported runtime and catch incompatible dependency updates before release.

### Fixed

- Align SQLite v1.58.0 with its required libc v1.75.6 runtime and compatible test-only ccgo v4.35.0; check this pairing in local dependency checks and CI.

### Changed

- Refresh CrawlKit to v0.14.9 and the test-only pprof dependency.

## 0.3.10 - 2026-09-05

**Highlights:** New binaries require macOS 13 or newer; v0.3.9 remains the last release supporting macOS 12.

### Changed

- Require Go 1.27.0 or newer and prefer Go 1.27.1 for builds; Go 1.27 builds require macOS 13 or newer, while the published v0.3.9 binaries retain macOS 12 support.
- Update age to v1.3.2, CrawlKit to v0.14.8, and cryptography dependencies for encrypted-backup fixes and input hardening; encrypted headers are limited to 2 MiB and 1024 recipient stanzas.
- Refresh modernc SQLite to v1.58.0 and its libc runtime to v1.75.7.
- Refresh test-only compiler and pprof dependencies and CI analysis tools, including CodeQL v4.37.9.

## v0.3.9 - 2026-08-23

### Fixed

- Preserve the original message and delete-for-everyone event when WhatsApp reuses a message row instead of aborting imports.

## v0.3.8 - 2026-08-14

### Fixed

- Preserve both the original message and reaction when WhatsApp reuses a Core Data message row, with stable identities across repeat imports (#68).

### Changed

- Refresh modernc SQLite's transitive libc, memory, and C compiler runtime dependencies.
- Update the minimum Go toolchain to 1.26.6 to resolve GO-2026-5026, GO-2026-5972, GO-2026-6090, and GO-2026-6218.
## [0.3.7] - 2026-08-08

### Changed

- Document the least-privilege Full Disk Access setup for unattended macOS imports (#61, thanks @ss251).
- Update CrawlKit to v0.14.6 and modernc SQLite to v1.56.0, including its journal-rollback corruption fix.
- Update CodeQL analysis to v4.37.6.

## [0.3.6] - 2026-08-03

### Fixed

- Verify unified release archives with normalized `./` tar members.
- Import current single-account WhatsApp Desktop stores when the dedicated account table is empty, while preserving account-mixing safeguards (#54, thanks @vincentkoc).

### Changed

- Rewrite the README to the house standard and move detailed commands, backups, data model, and release guidance into `docs/`.
- Update CrawlKit to v0.14.5 and refresh the test-only pprof dependency.
- Add pinned CodeQL analysis and weekly Dependabot updates for Go modules and GitHub Actions.
- Keep the shared release workflow on its moving `v1` line while suppressing routine minor and patch update churn and preserving major-version review.

## [0.3.5] - 2026-08-02

### Highlights

- Preserve cumulative WhatsApp history with tombstone-safe merge imports, explicit exact restores, stable message event identity, and retained edit/delete revisions (#38).

### Changed

- Make `import` and `sync` merge by default so rows missing from an incomplete Desktop snapshot remain searchable, bind merges to hashed account and source-store fingerprints, provide explicit `--adopt-source` for legacy archives, and use `--restore` for intentional exact replacement.
- Add source-attributed tombstones to contacts, chats, groups, participants, and messages, including subordinate tombstones for deleted parents and explicit WhatsApp removal, inactivity, and cleared-message signals.
- Migrate existing archives in place without rewriting canonical rows, retain prior message payloads in `message_revisions`, and exclude tombstoned rows from normal reads while keeping them in encrypted backups.
- Standardize local build, CI-parity check, snapshot, and fail-closed release commands under the shared crawler Makefile contract.
- Update go-isatty to v0.0.24 and refresh Go cryptography, system, and SQLite runtime dependencies.
- Refresh GitHub Actions and local analysis tooling to their latest releases.
- Move official releases to the shared OpenClaw workflow for organization-managed signing, notarization, verification, publication, and Homebrew handoff.

### Security

- Constrain web viewer media reads to the archive media directory and bound WhatsApp Desktop source, including symlink-safe containment checks.

## [0.3.4] - 2026-07-17

### Highlights

- Ship properly notarized official macOS binaries for both Apple Silicon and Intel, with Apple's assessment required before packaging.
- Keep missing credentials, rejected submissions, and failed hardened-runtime or notarization checks out of release archives.

### Changed

- Notarize each signed macOS binary through the managed runtime keychain profile and recheck its ticket during release validation.
- Update CrawlKit to v0.14.3 and terminal detection to go-isatty v0.0.23.

## [0.3.3] - 2026-07-17

### Highlights

- Use relative `--source` and `--db` paths reliably across source inspection, imports, archive status, and read-only SQL.
- Report the installed module version for source-built binaries so `wacrawl --version` matches the actual build.

### Changed

- Update CrawlKit to v0.14.2 and modernc SQLite to v1.54.0.

### Fixed

- Resolve relative archive and WhatsApp source paths at the shared SQLite file-URI boundary (#41).
- Report the installed module version for source-built binaries instead of a stale hard-coded release fallback.

## [0.3.2] - 2026-07-09

### Changed

- Build official macOS release binaries locally through the managed OpenClaw Foundation signing identity and verify both Darwin assets before updating Homebrew, while keeping CI and cross-platform snapshot builds credential-free.
- Update CrawlKit to v0.13.4.

### Security

- Update Go to 1.26.5 to fix the reachable GO-2026-5856 `crypto/tls` vulnerability.

## [0.3.1] - 2026-07-02

### Added

- Add a private loopback-only web viewer for archive status, chats, messages, and search, with per-run access keys and no media/configuration/write surface (#10, thanks @greenido).
- Redesign the web viewer as a WhatsApp-style two-pane reader with dark/light/auto themes, deterministic contact avatars, message bubbles with day separators, WhatsApp text formatting and markdown rendering, media and starred indicators, chat filters, highlighted search results, and older-message pagination.
- Render photo, sticker, and GIF attachments inline in the web viewer with lazy loading and a lightbox, served read-only from local media files through the authenticated loopback API (images only, content-sniffed, size-capped, paths never exposed); media WhatsApp has not downloaded locally falls back to the metadata card instead of blocking, and WhatsApp's internal media-hash strings no longer masquerade as attachment filenames or captions.
- Keep the web viewer chat list stable when opening a chat: the selection moves in place instead of re-rendering the sidebar, preserving scroll position and skipping entrance-animation replays.

### Fixed

- Harden archive reads and restores for URI-sensitive paths, no-media backups, and alternate WhatsApp media relationships (thanks @vincentkoc).
- Save backup configuration atomically with owner-only permissions to protect existing settings from interrupted writes (#25, thanks @TurboTheTurtle).
- Prevent stale web viewer responses from replacing active content, and preserve the current view when refresh metadata is temporarily unavailable.

## [0.3.0] - 2026-06-19

### Added

- Back up copied WhatsApp media as content-deduplicated encrypted Git blobs and restore current or historical media with portable paths and integrity checks.
- Add read-only SQL archive queries with JSON output, automatic sync support, and lossless duplicate-column handling (#18, thanks @TurboTheTurtle).
- Add named Git backup snapshots, snapshot history listing, and non-mutating historical restores through `backup pull --ref`.

### Changed

- Keep media filenames and archive paths inside an encrypted backup index; cleartext manifests expose only counts, encrypted blob paths, sizes, and hashes.
- Retry concurrent encrypted backup branch-and-tag pushes after rebasing and retargeting the unpublished tag.
- Move encrypted snapshot, Git history/tag/ref, SQLite bundle, contact export, and safe FTS query mechanics to CrawlKit while preserving the archive schema, backup manifest format, and CLI JSON contracts.

## [0.2.7] - 2026-06-10

### Fixed

- Clamp invalid WhatsApp timestamp sentinels so JSON reads survive existing archives and fresh imports (#16, thanks @rmorgans).

## [0.2.6] - 2026-06-07

### Added

- Add `metadata --json` crawlkit control metadata for schedulers and local automation.

### Changed

- Move source, install, and release automation references to `openclaw/wacrawl` and `openclaw/tap`.
- Update Go to 1.26.4 and refresh Go dependencies, including `crawlkit` to v0.11.0 and `modernc.org/sqlite` to v1.52.0.

### Fixed

- Resolve WhatsApp Desktop `Media/...` paths through `Message/Media` before copying archive media (#9, thanks @pasogott).

## [0.2.5] - 2026-05-15

### Changed

- Move stable archive-store SQLite reads and writes to sqlc-generated wrappers while keeping runtime schema setup, dynamic message/search filters, and WhatsApp Desktop source readers handwritten.

### Fixed

- Improve WhatsApp Desktop group sender-name resolution with profile push names while preserving readable message-level push-name fallbacks (#7, thanks @michalparkola).

## [0.2.4] - 2026-05-08

### Fixed

- Update the baked-in CLI version fallback so binaries installed with `go install` report the released version.

## [0.2.3] - 2026-05-08

### Fixed

- Format the backup coverage regression test so the release branch passes CI
  lint.

## [0.2.2] - 2026-05-08

### Fixed

- Keep the backup regression suite above the CI coverage floor after moving
  shared age encryption helpers into `crawlkit`.

## [0.2.1] - 2026-05-08

### Changed

- Reuse `crawlkit`'s shared encrypted backup helpers for age identities,
  JSONL/Gzip shard encryption, hashes, and restore verification.

### Added

- Add command-specific help menus with examples for `doctor`, `import`, `sync`, `status`, `chats`, `unread`, `messages`, `search`, and backup subcommands.
- Add `import --copy-media` / `sync --copy-media` to copy referenced WhatsApp media into the archive media directory while treating missing files as non-fatal import stats.
- Surface WhatsApp Desktop per-chat unread counts in `status` and `chats`, with `chats --unread` and an `unread` shortcut command.

### Fixed

- Merge duplicate WhatsApp Desktop chat, group, and group participant rows during import so older app data does not fail archive sync on unique constraints.

### Security

- Document the encrypted Git backup threat model, visible metadata, key recovery, and rotation limits.
- Reject backup manifest shard paths that do not point to encrypted files under the backup `data/` tree.

## [0.2.0] - 2026-04-27

### Added

- Add encrypted Git backups with `backup init`, `backup push`, `backup pull`, and `backup status`, storing WhatsApp archive data as age-encrypted JSONL gzip shards in a Git repository.
- Add multi-machine backup support with explicit age recipients, recipient-aware manifests, and automatic re-encryption of unchanged shards when recipients change.
- Add restore verification for encrypted backups, including plaintext shard hashes, cross-table validation, and import into a configured archive database.
- Add read-time sync for `status`, `chats`, `messages`, and `search`, with `--sync auto|always|never`, `--sync-max-age`, and `sync` as an alias for `import`.
- Add a `wacrawl` Codex skill for local WhatsApp archive workflows.

### Changed

- Expand the README with Homebrew install instructions, automatic sync behavior, encrypted Git backup setup, command cheat sheet, multi-machine setup, and recovery checklist.
- Document the `backup-wacrawl` repository layout and restore flow in the generated backup README.

### Fixed

- Allow `search` filters before or after the query, so documented examples like `wacrawl search "invoice" --from-them` work as expected.
- Keep Go module metadata tidy and CI-clean after adding age encryption dependencies.

## [0.1.0] - 2026-04-25

### Added

- Initial read-only WhatsApp Desktop archive CLI.
- macOS source discovery for
  `~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared`.
- Safe SQLite snapshot import for `ChatStorage.sqlite` and `ContactsV2.sqlite`.
- Archive schema for chats, contacts, groups, group participants, messages, and
  FTS5 search.
- Commands: `doctor`, `import`, `status`, `chats`, `messages`, and `search`.
- JSON output mode for scripting.
- Message filters for chat, sender, date range, direction, media presence, sort
  order, and limit.
- WhatsApp CoreData extraction for `ZWACHATSESSION`, `ZWAMESSAGE`,
  `ZWAMEDIAITEM`, `ZWAGROUPINFO`, and `ZWAGROUPMEMBER`.
- Apple-epoch timestamp conversion.
- Group sender resolution through `ZWAMESSAGE.ZGROUPMEMBER`.
- Media metadata extraction through both message-to-media join paths.
- Build, lint, coverage, and test automation through `make check`.
- GitHub Actions CI mirroring Discrawl: lint, tests, race tests, dependency
  checks, vulnerability scan, secret scan, and GoReleaser snapshot check.
- GoReleaser config for macOS, Linux, and Windows release archives.
- Release workflow for `v*` tags and manual tag reruns.
- `--version` flag with release-time ldflags injection.

### Changed

- Project now targets Go 1.26.
- Dependencies updated, including `modernc.org/sqlite` v1.50.0.
- Linting tightened with `golangci-lint` v2 configuration.

### Security

- Import is read-only against WhatsApp's app container.
- WhatsApp SQLite files are copied to a temporary snapshot before extraction.
- Archive writes are isolated to the configured `wacrawl` database.

### Quality

- Coverage gate added at 85% total statement coverage.
- Current test coverage: 86.3%.
- Focused tests cover CLI behavior, archive storage, import fixtures, filtering,
  search, JSON output, schema edge cases, and failure paths.
