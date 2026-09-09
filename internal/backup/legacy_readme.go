package backup

func ownedBackupReadme(content []byte) bool {
	return string(content) == backupReadme || string(content) == legacyBackupReadme
}

// Exact template from producer 0663d388e8270e3cdf7d091138d0d42fa0b33ae8,
// internal/backup/backup.go blob 8f65807701d2956cb68e6ed3c8e1d3176d5f7a4b.
// Historical bytes are recognized, not installed or rewritten.
const legacyBackupReadme = `# backup-wacrawl

Encrypted Git backup for a local wacrawl archive.

This repository is written by ` + "`wacrawl backup push`" + `. It is safe to keep on
GitHub because the archive payload is encrypted before Git sees it.

## Layout

` + "```text" + `
README.md
manifest.json
data/chats.jsonl.gz.age
data/contacts.jsonl.gz.age
data/groups.jsonl.gz.age
data/group_participants.jsonl.gz.age
data/messages/YYYY/MM.jsonl.gz.age
data/files/index*.jsonl.gz.age
data/files/objects/OPAQUE_ID.gz.age
` + "```" + `

` + "`manifest.json`" + ` is cleartext and contains format version, export time,
public age recipients, table counts, shard paths, encrypted byte sizes, and
plaintext hashes used for restore verification. Message text, contacts, chat
names, participant IDs, media metadata, filenames, and archive paths stay inside
encrypted ` + "`*.jsonl.gz.age`" + ` shards.

## Security Model

Shard contents are JSONL, gzip-compressed with a fixed gzip timestamp, and
encrypted with age for every configured public recipient. The local
` + "`~/.wacrawl/age.key`" + ` identity is required to decrypt.

Git can still see manifest metadata: export time, public recipients, table
names, row counts, shard paths, encrypted byte sizes, plaintext shard hashes,
backup cadence, and which encrypted shards changed. Git cannot read message
text, contacts, chat names, participant IDs, media metadata, filenames, or
archive paths without an age identity.

Anyone who can push to this repository can replace encrypted backup data with
different data encrypted to your public recipient. Keep repository write access
restricted and review unexpected backup commits. If an age identity is
compromised, remove its public recipient and push a new backup; old Git history
may still contain shards decryptable by the compromised key.

## Push

` + "```bash" + `
wacrawl backup push
wacrawl backup push --tag snapshot/before-phone-migration
` + "```" + `

The command pulls/rebases this checkout, refreshes the local wacrawl archive
according to the normal sync policy, writes encrypted row shards and copied
media blobs, updates the manifest, commits, and pushes this repository.

Every changed backup is a Git commit. Optional tags name important checkpoints;
tag names are visible Git metadata and should not contain sensitive text.

## Restore

` + "```bash" + `
wacrawl backup pull
wacrawl backup snapshots
wacrawl --db /tmp/wacrawl-history.db backup pull --ref snapshot/before-phone-migration
` + "```" + `

` + "`backup pull`" + ` decrypts every payload with the local age identity, verifies
the manifest hashes, restores copied media, validates the snapshot, and imports
it into the configured wacrawl archive database. Historical refs are read
directly from Git objects without changing this checkout's current branch.

## Recovery

Install wacrawl, clone this repo to the path in ` + "`~/.wacrawl/backup.json`" + `,
restore the local age identity file, then run:

` + "```bash" + `
wacrawl backup pull
wacrawl --sync never status
` + "```" + `

Do not commit the age identity. Only public ` + "`age1...`" + ` recipients belong in
config; ` + "`AGE-SECRET-KEY-...`" + ` values must stay local or in a password manager.
`
