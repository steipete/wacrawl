# Encrypted Git backups

`wacrawl` can export its archive into age-encrypted JSONL shards stored in a Git repository. The message data and copied media are encrypted before Git sees them.

## Repository layout

```text
README.md
manifest.json
data/chats.jsonl.gz.age
data/contacts.jsonl.gz.age
data/groups.jsonl.gz.age
data/group_participants.jsonl.gz.age
data/messages/YYYY/MM.jsonl.gz.age
data/message_revisions.jsonl.gz.age
data/files/index*.jsonl.gz.age
data/files/objects/OPAQUE_ID.gz.age
```

`manifest.json` is intentionally cleartext so a machine can inspect freshness, public age recipients, counts, shard paths, encrypted byte sizes, and plaintext hashes without decrypting the payloads. It does not contain message text, chat names, contacts, participant IDs, media metadata, filenames, or archive paths.

Copied media is included by default. Identical files share one encrypted blob with a random opaque object ID; content hashes and paths remain inside the encrypted index. Use `--no-media` when only archive rows are wanted.

## Setup

Initialize the backup checkout and a local age identity:

```bash
wacrawl backup init \
  --repo ~/Projects/backup-wacrawl \
  --remote <git-url>
```

When omitted, the current default remote is `https://github.com/steipete/backup-wacrawl.git`; pass `--remote` for another private repository.

This writes `~/.wacrawl/backup.json`, creates `~/.wacrawl/age.key` if needed, clones or initializes the local checkout, and prints the public age recipient.

```json
{
  "repo": "~/Projects/backup-wacrawl",
  "remote": "<git-url>",
  "identity": "~/.wacrawl/age.key",
  "recipients": ["age1..."]
}
```

Keep the `AGE-SECRET-KEY-...` identity private. The matching `age1...` recipient is public and can be stored in the config and manifest.

## Push and inspect

```bash
wacrawl backup push
wacrawl backup status
wacrawl backup snapshots
```

`backup push` does not fetch or rebase an existing checkout. It applies the normal read-time sync policy, exports stable JSONL, compresses and encrypts each shard for every recipient, updates the manifest, removes stale encrypted shards, and commits only owned backup artifacts before checking unpublished history and attempting publication. It includes copied files already under the archive's `media/` directory; run `wacrawl sync --copy-media` first to capture media still available from WhatsApp Desktop. The backup command never reads media directly from the WhatsApp container.

Useful variants:

```bash
# Commit locally without pushing.
wacrawl backup push --no-push

# Omit copied media from the current snapshot.
wacrawl backup push --no-media

# Name the resulting snapshot commit.
wacrawl backup push --tag snapshot/before-phone-migration
```

Re-running `backup push` without archive changes leaves Git clean. When `--tag` is used without a data change, the tag points to the existing current snapshot; existing tags are never moved. The command reports the checkout path, whether data changed, encryption state, shard count, message count, and copied-media count. Tag names and commit metadata remain visible to anyone who can inspect the repository, so keep tag names non-sensitive.

A refused push can leave a completed local snapshot commit. Re-running an unchanged push retries publication after the same history checks; it does not repair divergence or unverified history. See the recovery guidance below before changing the checkout. `backup pull --ref` fetches refs and reads the selected Git objects without checking them out or rewriting the backup checkout, but still replaces the configured archive.

`backup status` reads only the cleartext manifest. `backup snapshots --limit N` lists the newest manifest-changing commits and their tags.

## Restore

```bash
wacrawl backup pull
```

The pull command updates the configured checkout, decrypts every shard with the local identity, verifies plaintext hashes, restores copied media, validates cross-table references, and replaces the configured archive in one transaction.

Test a restore without touching the primary archive:

```bash
wacrawl --db /tmp/wacrawl-restore-test.db backup pull
wacrawl --db /tmp/wacrawl-restore-test.db --sync never status
```

Restore a historical tag, commit, or branch without changing the backup checkout:

```bash
wacrawl --db /tmp/wacrawl-history.db backup pull \
  --ref snapshot/before-phone-migration
```

Pass `--no-media` to restore archive rows without copied media. On push, `--no-media` removes media from the current snapshot, but earlier Git commits still retain their encrypted media blobs.

## Multiple machines

Each machine that restores the backup should have its own age identity. On the new machine, run `backup init`, then copy its printed public recipient into the `recipients` list on a machine that can already decrypt the backup. The next `backup push` re-encrypts shards for all configured recipients, including unchanged plaintext shards when the recipient set changes.

Keep a recovery copy of each `~/.wacrawl/age.key` in a password manager. Never commit the identity or paste it into issues, logs, documentation, or chat.

## Recovery checklist

On a new machine:

```bash
brew install openclaw/tap/wacrawl
git clone <git-url> ~/Projects/backup-wacrawl
mkdir -p ~/.wacrawl
```

Restore `~/.wacrawl/age.key` from the password manager and create `~/.wacrawl/backup.json` pointing at the clone. Then run:

```bash
wacrawl backup pull
wacrawl --sync never status
```

If decryption fails, the configured identity does not match any recipient used for the shards.

For a refused push on an existing machine, distinguish an unknown remote baseline, divergent history, unverified unpublished paths, and repository permission failures. The command does not fetch or rebase to resolve these conditions, and a completed local snapshot commit may remain unpublished.

Preserve the original checkout, including its index, local commits and tags, together with the archive database, copied media, backup configuration and private identity. Inspect the reported history or path and confirm the intended remote before retrying. Resetting, rebasing, force-pushing, or pulling a backup over the primary archive is not a general recovery procedure.

After deliberately choosing the intended remote and archive, a separate fresh checkout and configuration may be appropriate. Publishing a fresh snapshot does not merge divergent archive contents or transfer every old unpublished commit and tag. Keep the original recovery material intact; the new-machine restore steps above are not instructions to overwrite an existing primary archive.

## Encryption flow

Backups use the Go `filippo.io/age` library with X25519 recipients. There is no backup password.

For each push, `wacrawl`:

1. Exports deterministic JSONL rows.
2. Streams copied media through hashing, gzip, and age encryption.
3. Deduplicates identical media by content and encrypts the private path index.
4. Encrypts every compressed payload for each configured recipient.
5. Writes only encrypted `*.jsonl.gz.age` payloads to Git.
6. Writes cleartext manifest metadata for status, diffing, and restore verification.

Pull performs the reverse, verifies hashes, localizes portable media paths, validates references, and imports the snapshot transactionally.

The age decoder rejects encrypted headers larger than 2 MiB or containing more than 1024 recipient stanzas. Restores of older backups exceeding either limit fail before importing their contents.

## Threat model

The backup protects message text, contacts, chat names, participant IDs, media metadata, filenames, archive paths, and copied media from a read-only Git compromise or accidental clone. Each listed recipient can decrypt every shard. Age detects encrypted-file corruption, and `wacrawl` also verifies manifest hashes after decryption.

The Git repository still reveals:

- Export time, public recipients, table names, row counts, shard paths, encrypted byte sizes, and row or index hashes in `manifest.json`.
- Message activity by year and month through paths such as `data/messages/2026/04.jsonl.gz.age`.
- Backup cadence and changed shards through Git history.

Important limits:

- This is confidentiality and integrity checking, not end-to-end provenance. A writer can replace the backup with different data encrypted to your public recipient.
- Losing every matching private identity makes the backup unrecoverable.
- Removing a compromised recipient and pushing re-encrypts current shards, but old Git history can remain decryptable until it is rewritten or deleted.
- X25519 recipients are not post-quantum.
- The local WhatsApp source, archive database, and copied media remain plaintext on the machine.

## Flags

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--config PATH` | all | Config path; default `~/.wacrawl/backup.json` |
| `--repo PATH` | all | Local backup checkout |
| `--remote URL` | all | Git remote |
| `--identity PATH` | init, push, pull, status | Local age identity |
| `--recipient AGE` | init, push | Public recipient; repeatable |
| `--no-push` | init, push | Commit locally without pushing |
| `--no-media` | push, pull | Omit copied media |
| `--tag NAME` | push | Tag the resulting snapshot |
| `--ref REF` | pull | Restore a tag, commit, or branch |
| `--limit N` | snapshots | Limit history results; default 20 |
