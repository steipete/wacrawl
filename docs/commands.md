# Command reference

`wacrawl` reads macOS WhatsApp Desktop data into its own local SQLite archive. This page covers paths, import behavior, commands, filters, and automatic sync.

## Paths

The default WhatsApp Desktop source is:

```text
~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared
```

The importer reads:

```text
ChatStorage.sqlite
ContactsV2.sqlite
Axolotl.sqlite
Message/Media/
```

The local archive defaults to:

```text
~/.wacrawl/wacrawl.db
```

Override either location with global flags:

```bash
wacrawl --source "/path/to/WhatsApp.shared" doctor
wacrawl --db /tmp/wacrawl.db sync
```

## Import and sync

`sync` and `import` are aliases. They copy the WhatsApp SQLite database, WAL, and SHM files into a temporary snapshot, then merge the useful records into the archive in one transaction.

```bash
wacrawl sync
wacrawl sync --copy-media
wacrawl import --adopt-source
wacrawl import --restore
```

Routine imports merge by stable row identity. When WhatsApp reuses a message row for a reaction or delete-for-everyone event, the original message remains intact and the new event receives its own stable archive identity. Rows absent from the current Desktop snapshot stay in the archive because absence alone is not a deletion signal. This preserves older history after WhatsApp evicts it locally.

The archive binds the canonical Desktop source, a hashed portable CoreData store marker, and a separately hashed account-owned JID from `Axolotl.sqlite`. A nonempty legacy archive without that verified account binding refuses routine merges until an explicit `--adopt-source`. Established account and store fingerprints stay strict across encrypted backup restore. Use a separate `--db` for a different account, or `--restore` when the current snapshot must exactly replace the archive. `--adopt-source` and `--restore` are mutually exclusive.

Explicit WhatsApp signals create source-attributed tombstones instead of deleting rows. Removed chats tombstone their archived groups, participants, and messages; inactive group members are tombstoned; and a message payload observed changing to SQL `NULL` is retained as a deleted message with its previous payload in `message_revisions`. Message tombstones remain sticky during later merges. An exact `--restore` is authoritative and can revive a source row; it also removes destination-only rows and local revision history. A removed chat can start a new live lifecycle when WhatsApp reports post-tombstone activity without reviving its historical messages. Normal list, search, status, and web reads exclude tombstones, while encrypted backups retain them.

By default, media paths continue to point into WhatsApp Desktop's app container. `--copy-media` copies referenced files into `media/` beside the archive and rewrites the imported paths. Missing media is counted but does not fail the import.

## Read commands

### `doctor`

Inspect source availability, discovered database files, row counts, date range, archive path, and schema notes:

```bash
wacrawl doctor
wacrawl --json doctor
```

### `status`

Show live and deleted entity counts, revisions, unread counts, media-message count, date range, last import, and source metadata:

```bash
wacrawl status
wacrawl --sync never status
```

### `chats` and `unread`

List chats by newest message, optionally filtering to chats with unread messages:

```bash
wacrawl chats --limit 100
wacrawl chats --unread
wacrawl unread --limit 20
```

Unread state comes from WhatsApp Desktop's per-chat counter; the message rows do not expose a reliable incoming per-message “read by me” flag.

### `messages`

```bash
wacrawl messages --limit 20
wacrawl messages --chat 1234567890@s.whatsapp.net --asc
wacrawl messages --after 2026-01-01 --from-them
wacrawl --json messages --has-media --limit 100
```

| Flag | Meaning |
| --- | --- |
| `--chat JID` | Restrict to one chat |
| `--sender JID` | Restrict to one sender |
| `--limit N` | Maximum rows; default 50 |
| `--after TIME` | After an RFC 3339 timestamp or `YYYY-MM-DD` |
| `--before TIME` | Before an RFC 3339 timestamp or `YYYY-MM-DD` |
| `--from-me` | Outgoing messages only |
| `--from-them` | Incoming messages only |
| `--has-media` | Messages with media metadata only |
| `--asc` | Oldest first |

### `search`

Search the portable SQLite FTS5 index across message text, chat name, sender name, and media title. Search accepts the same filters as `messages`, before or after the query.

```bash
wacrawl search "launch"
wacrawl search "invoice" --from-them --after 2026-01-01
wacrawl --json search --chat 1234567890@s.whatsapp.net "release notes"
```

### `contacts export`

Export contacts that have a display name and phone number using the CrawlKit contact schema:

```bash
wacrawl --json --sync never contacts export
```

### `sql`

Run a single read-only `SELECT` statement against the archive:

```bash
wacrawl sql "SELECT count(*) FROM messages"
wacrawl --json sql "SELECT chat_jid, count(*) FROM messages GROUP BY chat_jid"
```

Use `--db PATH` to query another archive.

### `web`

```bash
wacrawl web
wacrawl --sync never web --port 8787
```

The viewer prints a private URL and runs until Ctrl-C. It binds to IPv4 loopback, chooses a random free port by default, and uses a random access key stored in the URL fragment. The page consumes the key into memory and removes it from the address bar.

The read-only interface uses a two-pane chat layout with day separators, deterministic contact avatars, dark and light themes, search highlighting, older-message pagination, and WhatsApp-style formatting for emphasis, code, quotes, lists, and links. Photos, stickers, and GIFs can render from local files behind the same key; media reads are content-sniffed, size-capped, and restricted to the archive media directory and bound WhatsApp source. Other attachments stay metadata-only. The viewer cannot alter archives, backup configuration, or sync schedules, and it cannot listen on a non-loopback address.

### `metadata`

Print `crawlkit.control.v1` metadata for schedulers and local automation:

```bash
wacrawl metadata
```

## Automatic sync

Before `status`, `chats`, `contacts export`, `unread`, `messages`, `search`, `sql`, `web`, or `backup push`, `wacrawl` checks the archive's last import time. If the archive is stale, it imports only when the WhatsApp Desktop source is newer.

```text
--sync auto     Sync when the archive is stale and the source is ahead. Default.
--sync always   Force a sync before every read command.
--sync never    Read only the existing archive.
```

The default staleness window is 15 minutes:

```bash
wacrawl --sync always status
wacrawl --sync never --json messages --limit 10
wacrawl --sync-max-age 1h chats
```

When the source is unavailable but an archive exists, `--sync auto` warns and continues with the archive. `--sync always` returns an error.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--db PATH` | Archive database; default `~/.wacrawl/wacrawl.db` |
| `--source PATH` | WhatsApp Desktop source |
| `--sync MODE` | `auto`, `always`, or `never`; default `auto` |
| `--sync-max-age DURATION` | Staleness window; default `15m` |
| `--json` | Emit JSON instead of human-readable output |
| `--version` | Print the CLI version |

Run `wacrawl --help`, `wacrawl help <command>`, or `wacrawl backup help <command>` for the flags compiled into the installed binary.
