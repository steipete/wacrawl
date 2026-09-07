# wacrawl 🧾 — WhatsApp archaeology with encrypted receipts

![wacrawl banner](docs/assets/readme-banner.jpg)

[![CI](https://img.shields.io/github/actions/workflow/status/openclaw/wacrawl/ci.yml?branch=main&style=flat-square&label=ci)](https://github.com/openclaw/wacrawl/actions/workflows/ci.yml)
[![GitHub release](https://img.shields.io/github/v/release/openclaw/wacrawl?style=flat-square)](https://github.com/openclaw/wacrawl/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/openclaw/wacrawl?style=flat-square&logo=go)](go.mod)
[![License](https://img.shields.io/github/license/openclaw/wacrawl?style=flat-square)](LICENSE)
[![Homebrew](https://img.shields.io/badge/Homebrew-openclaw%2Ftap%2Fwacrawl-FBB040?style=flat-square&logo=homebrew&logoColor=black)](https://github.com/openclaw/homebrew-tap/blob/main/Formula/wacrawl.rb)

`wacrawl` makes a read-only snapshot of the macOS WhatsApp Desktop databases and imports chats, contacts, messages, and media metadata into a local SQLite archive. It is for people who want fast local search, structured exports, a private web viewer, or encrypted Git backups without connecting to WhatsApp's network protocol.

![wacrawl web viewer](docs/assets/webui-chat-dark.png)

## Install

On macOS, v0.3.10 and newer require macOS 13 or newer. v0.3.9 remains the last release supporting macOS 12.

Homebrew is the smallest path:

```bash
brew install openclaw/tap/wacrawl
```

Upgrade later with `brew upgrade openclaw/tap/wacrawl`.

Or install from source with Go 1.27.0 or newer:

```bash
go install github.com/openclaw/wacrawl/cmd/wacrawl@latest
```

Source builds with Go 1.27 also require macOS 13 or newer.

Direct source discovery requires macOS and the desktop WhatsApp app. Release builds for macOS, Linux, and Windows can work with an existing archive or encrypted backup.

## Quick start

Check the source, import it, then inspect the archive:

```bash
wacrawl doctor
wacrawl sync
wacrawl status
wacrawl search "invoice"
```

Read commands refresh a stale archive when the WhatsApp source is newer. To browse instead:

```bash
wacrawl web
```

The viewer prints a private local URL, binds only to `127.0.0.1`, and stops with Ctrl-C.

## What it reads

WhatsApp Desktop keeps its macOS data under:

```text
~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared
```

`wacrawl` snapshots `ChatStorage.sqlite`, `ContactsV2.sqlite`, and their SQLite sidecars before reading. With `--copy-media`, it also copies referenced files from `Message/Media/`. Its own archive defaults to `~/.wacrawl/wacrawl.db`.

Imports merge by stable source identity, retain older history that has disappeared from the current desktop snapshot, and preserve explicit edits and deletions as revisions or tombstones. Use a separate `--db` for another WhatsApp account; source adoption and exact restore are deliberate operations described in the [command reference](docs/commands.md#import-and-sync).

## Search and automation

Search covers message text, chat and sender names, and media titles:

```bash
wacrawl search "release notes" --from-them --after 2026-01-01
wacrawl messages --chat 1234567890@s.whatsapp.net --has-media
wacrawl sql "SELECT count(*) FROM messages"
```

Add `--json` for scripts and agents:

```bash
wacrawl --json --sync never search "invoice"
wacrawl --json --sync never contacts export
```

See the [command reference](docs/commands.md) for filters, sync modes, SQL constraints, and every subcommand.

Running imports on a launchd or cron schedule? macOS prompts "would like to access data from other apps" on every background run, and Allow does not persist. The [scheduled imports guide](docs/scheduled-imports.md) covers the Full Disk Access setup that makes unattended imports work.

## Encrypted backups

`wacrawl backup` exports deterministic JSONL shards, compresses them, and encrypts them to one or more X25519 age recipients before Git sees the data. Copied media can travel in content-deduplicated encrypted blobs, and restores verify hashes and cross-table references before replacing the archive.

```bash
wacrawl backup init --repo ~/Projects/backup-wacrawl --remote <git-url>
wacrawl backup push
wacrawl backup snapshots
wacrawl backup pull
```

The manifest remains cleartext and reveals backup timing, public recipients, counts, shard paths, encrypted sizes, and hashes. Read the [backup guide and threat model](docs/backups.md) before relying on it for recovery.

## Safety boundary

- The WhatsApp databases are opened read-only through a temporary SQLite snapshot.
- Normal archive and search commands do not upload data or write into WhatsApp's app container.
- The web viewer is loopback-only, read-only, protected by a random per-run access key, and restricts media reads to known roots.
- `backup push` is the explicit networked path; it sends age-encrypted shards to the configured Git remote.

The archive still contains private message data in plaintext. Keep `~/.wacrawl/wacrawl.db` and copied media out of commits, shared logs, and untrusted backups unless sharing them is intentional.

## Commands

| Command | Purpose |
| --- | --- |
| `doctor` | Inspect source and archive paths |
| `sync`, `import` | Snapshot and merge WhatsApp Desktop data |
| `status`, `chats`, `unread`, `messages` | Inspect archived conversations |
| `contacts export` | Export named contacts with phone numbers |
| `search` | Search the portable SQLite FTS5 index |
| `sql` | Run one read-only `SELECT` statement |
| `web` | Open the private local viewer |
| `backup` | Initialize, push, inspect, or restore encrypted Git backups |
| `metadata` | Print CrawlKit control metadata for automation |

Run `wacrawl help <command>` or open the [full command reference](docs/commands.md).

## Documentation

- [Commands, filters, paths, and sync behavior](docs/commands.md)
- [Scheduled imports on macOS and the TCC prompt loop](docs/scheduled-imports.md)
- [Encrypted backups, recovery, and threat model](docs/backups.md)
- [Archive identities and data model](docs/data-model.md)
- [Release process](docs/releasing.md)

## Development

Requires Go 1.27.0 or newer.

The preferred build toolchain is Go 1.27.1, selected automatically by `go.mod` and CI. The source minimum remains Go 1.27.0.

Keep `modernc.org/libc` at the exact version required by `modernc.org/sqlite`; SQLite's generated code depends on that runtime pairing. Update them together, and check test-only compiler updates for indirect libc upgrades. `make deps` and CI verify the resolved versions.

```bash
make build
make test
make check
```

`make check` mirrors the local CI gates: formatting, analysis, tests, race and coverage checks, dependency and vulnerability checks, a credential-free GoReleaser snapshot, release-script tests, and secret scans.

## License

MIT. See [LICENSE](LICENSE).
