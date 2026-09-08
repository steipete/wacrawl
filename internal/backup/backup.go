package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ckbackup "github.com/openclaw/crawlkit/backup"
	"github.com/openclaw/crawlkit/mirror"
	"github.com/openclaw/wacrawl/internal/store"
)

const formatVersion = ckbackup.FormatVersion

type Manifest struct {
	Format     int          `json:"format"`
	Encrypted  bool         `json:"encrypted"`
	Exported   time.Time    `json:"exported"`
	Recipients []string     `json:"recipients,omitempty"`
	Counts     Counts       `json:"counts"`
	Shards     []ShardEntry `json:"shards"`
	Files      []FileEntry  `json:"files,omitempty"`
}

type Counts struct {
	Contacts     int `json:"contacts"`
	Chats        int `json:"chats"`
	Groups       int `json:"groups"`
	Participants int `json:"participants"`
	Messages     int `json:"messages"`
	Revisions    int `json:"message_revisions,omitempty"`
	Identity     int `json:"archive_identity,omitempty"`
	MediaFiles   int `json:"media_files,omitempty"`
}

type (
	ShardEntry = ckbackup.ShardEntry
	FileEntry  = ckbackup.FileEntry
)

type Result struct {
	Repo       string `json:"repo"`
	Changed    bool   `json:"changed"`
	Encrypted  bool   `json:"encrypted"`
	Shards     int    `json:"shards"`
	Messages   int    `json:"messages"`
	MediaFiles int    `json:"media_files"`
	Ref        string `json:"ref,omitempty"`
	Tag        string `json:"tag,omitempty"`
}

type archiveIdentity struct {
	SourceStoreIdentity string `json:"source_store_identity"`
	AccountIdentity     string `json:"account_identity"`
}

func Init(ctx context.Context, opts Options) (Config, string, error) {
	cfg, err := ResolveOptions(opts)
	if err != nil {
		return Config{}, "", err
	}
	if err := validateWriteLayout(cfg, opts); err != nil {
		return Config{}, "", err
	}
	if err := validateOwnedFiles(cfg, []string{"README.md", "manifest.json"}); err != nil {
		return Config{}, "", err
	}
	recipient, err := EnsureIdentity(cfg.Identity)
	if err != nil {
		return Config{}, "", err
	}
	// Creation exposes filesystem aliases (including case-equivalent names).
	// Recheck before saving config or initializing a publication repository.
	if err := validateWriteLayout(cfg, opts); err != nil {
		return Config{}, "", err
	}
	if len(cfg.Recipients) == 0 {
		cfg.Recipients = []string{recipient}
	}
	if err := SaveConfig(opts.ConfigPath, cfg); err != nil {
		return Config{}, "", err
	}
	if err := validateWriteLayout(cfg, opts); err != nil {
		return Config{}, "", err
	}
	if err := ensureRepoForWrite(ctx, cfg); err != nil {
		return Config{}, "", err
	}
	if err := validateOwnedFiles(cfg, []string{"README.md", "manifest.json"}); err != nil {
		return Config{}, "", err
	}
	if err := writeBackupReadme(cfg.Repo); err != nil {
		return Config{}, "", err
	}
	_, err = commitAndPush(ctx, cfg, "docs: describe encrypted wacrawl backup", opts.Push)
	return cfg, recipient, err
}

func Push(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	cfg, err := ResolveOptions(opts)
	if err != nil {
		return Result{}, err
	}
	opts.ArchivePath = st.Path()
	if err := validateWriteLayout(cfg, opts); err != nil {
		return Result{}, err
	}
	if len(cfg.Recipients) == 0 {
		recipient, err := RecipientFromIdentity(cfg.Identity)
		if err != nil {
			return Result{}, err
		}
		cfg.Recipients = []string{recipient}
	}
	if err := ensureRepoForWrite(ctx, cfg); err != nil {
		return Result{}, err
	}
	if err := validateSnapshotTag(ctx, cfg.Repo, opts.Tag); err != nil {
		return Result{}, err
	}
	if err := validateOwnedFiles(cfg, []string{"manifest.json"}); err != nil {
		return Result{}, err
	}
	oldManifest, err := readManifest(cfg.Repo)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if err == nil && (oldManifest.Format != formatVersion || !oldManifest.Encrypted) {
		return Result{}, errors.New("previous backup is not a supported encrypted snapshot")
	}
	if err := validateSnapshotInputs(ctx, cfg, toCrawlkitManifest(oldManifest)); err != nil {
		return Result{}, err
	}
	if err := writeBackupReadme(cfg.Repo); err != nil {
		return Result{}, err
	}
	data, err := st.ExportAll(ctx)
	if err != nil {
		return Result{}, err
	}
	var files []ckbackup.File
	if !opts.NoMedia {
		files, err = collectBackupMedia(ctx, st.Path(), data.Messages)
		if err != nil {
			return Result{}, err
		}
	}
	manifest, err := writeSnapshot(ctx, cfg, data, files, oldManifest)
	if err != nil {
		return Result{}, err
	}
	pushWithTag := opts.Push && strings.TrimSpace(opts.Tag) != ""
	changed, err := commitAndPush(ctx, cfg, "sync: update encrypted wacrawl backup", opts.Push && !pushWithTag, toCrawlkitManifest(oldManifest), toCrawlkitManifest(manifest))
	if err != nil {
		return Result{}, err
	}
	if pushWithTag {
		if err := verifyPendingHistory(ctx, cfg); err != nil {
			return Result{}, err
		}
	}
	tag, err := tagSnapshot(ctx, cfg, opts.Tag)
	if err != nil {
		return Result{}, err
	}
	if pushWithTag {
		if err := mirror.PushAtomic(ctx, mirrorOptions(cfg), "HEAD", "refs/tags/"+tag); err != nil {
			return Result{}, err
		}
	}
	return Result{Repo: cfg.Repo, Changed: changed, Encrypted: true, Shards: len(manifest.Shards), Messages: manifest.Counts.Messages, MediaFiles: len(manifest.Files), Tag: tag}, nil
}

func Pull(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	cfg, err := ResolveOptions(opts)
	if err != nil {
		return Result{}, err
	}
	ensure := ensureRepo
	if strings.TrimSpace(opts.Ref) != "" {
		ensure = ensureRepoForRead
	}
	if err := ensure(ctx, cfg); err != nil {
		return Result{}, err
	}
	manifest, ref, err := readManifestAtRef(ctx, cfg.Repo, opts.Ref)
	if err != nil {
		return Result{}, err
	}
	var data store.SnapshotData
	if ref == "" {
		data, err = readSnapshot(cfg, manifest)
	} else {
		data, err = readSnapshotAtRef(ctx, cfg, manifest, ref)
	}
	if err != nil {
		return Result{}, err
	}
	if err := data.Validate(); err != nil {
		return Result{}, err
	}
	root, err := archiveRoot(st.Path())
	if err != nil {
		return Result{}, err
	}
	stageRoot := ""
	if !opts.NoMedia && len(manifest.Files) > 0 {
		stageRoot, err = os.MkdirTemp(root, ".wacrawl-media-restore-")
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = os.RemoveAll(stageRoot) }()
		if ref == "" {
			_, err = ckbackup.RestoreFilesUnder(ctx, crawlkitConfig(cfg), toCrawlkitManifest(manifest), stageRoot, "media")
		} else {
			_, _, err = ckbackup.RestoreFilesAtUnder(ctx, crawlkitConfig(cfg), mirrorOptions(cfg), toCrawlkitManifest(manifest), ref, stageRoot, "media")
		}
		if err != nil {
			return Result{}, err
		}
	}
	if stageRoot != "" {
		if err := localizeMediaPaths(data.Messages, root); err != nil {
			return Result{}, err
		}
	}
	sourcePath := "backup:" + cfg.Repo
	if ref != "" {
		sourcePath += "@" + ref
	}
	importSnapshot := func() error { return st.ImportSnapshot(ctx, data, sourcePath, manifest.Exported) }
	if stageRoot != "" {
		err = replaceMediaDuring(filepath.Join(stageRoot, "media"), filepath.Join(root, "media"), importSnapshot)
	} else {
		err = importSnapshot()
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Repo: cfg.Repo, Changed: true, Encrypted: manifest.Encrypted, Shards: len(manifest.Shards), Messages: len(data.Messages), MediaFiles: len(manifest.Files), Ref: ref}, nil
}

func Status(ctx context.Context, opts Options) (Manifest, string, error) {
	cfg, err := ResolveOptions(opts)
	if err != nil {
		return Manifest{}, "", err
	}
	if err := ensureRepo(ctx, cfg); err != nil {
		return Manifest{}, "", err
	}
	manifest, err := readManifest(cfg.Repo)
	if err != nil {
		return Manifest{}, "", err
	}
	return manifest, cfg.Repo, nil
}

func writeSnapshot(ctx context.Context, cfg Config, data store.SnapshotData, files []ckbackup.File, old Manifest) (Manifest, error) {
	identities := []archiveIdentity(nil)
	if data.SourceStoreIdentity != "" || data.AccountIdentity != "" {
		identities = append(identities, archiveIdentity{SourceStoreIdentity: data.SourceStoreIdentity, AccountIdentity: data.AccountIdentity})
	}
	shards := []ckbackup.Shard{
		{Table: "contacts", Path: "data/contacts.jsonl.gz.age", Rows: data.Contacts},
		{Table: "chats", Path: "data/chats.jsonl.gz.age", Rows: data.Chats},
		{Table: "groups", Path: "data/groups.jsonl.gz.age", Rows: data.Groups},
		{Table: "group_participants", CountKey: "participants", Path: "data/group_participants.jsonl.gz.age", Rows: data.Participants},
		{Table: "message_revisions", CountKey: "message_revisions", Path: "data/message_revisions.jsonl.gz.age", Rows: data.Revisions},
		{Table: "archive_identity", CountKey: "archive_identity", Path: "data/archive_identity.jsonl.gz.age", Rows: identities},
	}
	for _, shard := range messageShards(data.Messages) {
		shards = append(shards, ckbackup.Shard{Table: "messages", Path: shard.path, Rows: shard.messages})
	}
	sharedOld := toCrawlkitManifest(old)
	if len(data.Messages) == 0 {
		delete(sharedOld.Counts, "messages")
	}
	manifest, err := ckbackup.WriteSnapshotWithFiles(ctx, crawlkitConfig(cfg), shards, files, sharedOld)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Counts["contacts"] = len(data.Contacts)
	manifest.Counts["chats"] = len(data.Chats)
	manifest.Counts["groups"] = len(data.Groups)
	manifest.Counts["participants"] = len(data.Participants)
	manifest.Counts["messages"] = len(data.Messages)
	manifest.Counts["message_revisions"] = len(data.Revisions)
	manifest.Counts["archive_identity"] = len(identities)
	if ckbackup.EquivalentManifest(toCrawlkitManifest(old), manifest) {
		return old, nil
	}
	if err := ckbackup.WriteManifest(cfg.Repo, manifest); err != nil {
		return Manifest{}, err
	}
	return fromCrawlkitManifest(manifest), nil
}

func readSnapshot(cfg Config, manifest Manifest) (store.SnapshotData, error) {
	shards, err := ckbackup.ReadSnapshot(crawlkitConfig(cfg), toCrawlkitManifest(manifest))
	if err != nil {
		return store.SnapshotData{}, err
	}
	return decodeSnapshot(shards)
}

func decodeSnapshot(shards []ckbackup.DecodedShard) (store.SnapshotData, error) {
	var data store.SnapshotData
	for _, shard := range shards {
		switch shard.Entry.Table {
		case "contacts":
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &data.Contacts); err != nil {
				return store.SnapshotData{}, err
			}
		case "chats":
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &data.Chats); err != nil {
				return store.SnapshotData{}, err
			}
		case "groups":
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &data.Groups); err != nil {
				return store.SnapshotData{}, err
			}
		case "group_participants":
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &data.Participants); err != nil {
				return store.SnapshotData{}, err
			}
		case "messages":
			var messages []store.Message
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &messages); err != nil {
				return store.SnapshotData{}, err
			}
			data.Messages = append(data.Messages, messages...)
		case "message_revisions":
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &data.Revisions); err != nil {
				return store.SnapshotData{}, err
			}
		case "archive_identity":
			var identities []archiveIdentity
			if err := ckbackup.DecodeJSONL(shard.Plaintext, &identities); err != nil {
				return store.SnapshotData{}, err
			}
			if len(identities) > 1 {
				return store.SnapshotData{}, errors.New("backup contains multiple archive identities")
			}
			if len(identities) == 1 {
				data.SourceStoreIdentity = identities[0].SourceStoreIdentity
				data.AccountIdentity = identities[0].AccountIdentity
			}
		default:
			return store.SnapshotData{}, fmt.Errorf("unknown backup table %q", shard.Entry.Table)
		}
	}
	sort.Slice(data.Messages, func(i, j int) bool {
		if data.Messages[i].Timestamp.Equal(data.Messages[j].Timestamp) {
			return data.Messages[i].SourcePK < data.Messages[j].SourcePK
		}
		return data.Messages[i].Timestamp.Before(data.Messages[j].Timestamp)
	})
	return data, nil
}

type messageShard struct {
	path     string
	messages []store.Message
}

func messageShards(messages []store.Message) []messageShard {
	buckets := map[string][]store.Message{}
	for _, message := range messages {
		t := message.Timestamp.UTC()
		year, month := "unknown", "00"
		if !t.IsZero() {
			year = fmt.Sprintf("%04d", t.Year())
			month = fmt.Sprintf("%02d", int(t.Month()))
		}
		rel := fmt.Sprintf("data/messages/%s/%s.jsonl.gz.age", year, month)
		buckets[rel] = append(buckets[rel], message)
	}
	paths := make([]string, 0, len(buckets))
	for path := range buckets {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]messageShard, 0, len(paths))
	for _, path := range paths {
		values := buckets[path]
		sort.Slice(values, func(i, j int) bool {
			if values[i].Timestamp.Equal(values[j].Timestamp) {
				return values[i].SourcePK < values[j].SourcePK
			}
			return values[i].Timestamp.Before(values[j].Timestamp)
		})
		out = append(out, messageShard{path: path, messages: values})
	}
	return out
}

func readManifest(repo string) (Manifest, error) {
	manifest, err := ckbackup.ReadManifest(repo)
	if err != nil {
		return Manifest{}, err
	}
	return fromCrawlkitManifest(manifest), nil
}

func crawlkitConfig(cfg Config) ckbackup.Config {
	return ckbackup.Config{Repo: cfg.Repo, Identity: cfg.Identity, Recipients: cfg.Recipients}
}

func toCrawlkitManifest(manifest Manifest) ckbackup.Manifest {
	return ckbackup.Manifest{
		Format:     manifest.Format,
		Encrypted:  manifest.Encrypted,
		Exported:   manifest.Exported,
		Recipients: manifest.Recipients,
		Counts: map[string]int{
			"contacts":          manifest.Counts.Contacts,
			"chats":             manifest.Counts.Chats,
			"groups":            manifest.Counts.Groups,
			"participants":      manifest.Counts.Participants,
			"messages":          manifest.Counts.Messages,
			"message_revisions": manifest.Counts.Revisions,
			"archive_identity":  manifest.Counts.Identity,
		},
		Shards: manifest.Shards,
		Files:  manifest.Files,
	}
}

func fromCrawlkitManifest(manifest ckbackup.Manifest) Manifest {
	participants := manifest.Counts["participants"]
	if participants == 0 {
		participants = manifest.Counts["group_participants"]
	}
	return Manifest{
		Format:     manifest.Format,
		Encrypted:  manifest.Encrypted,
		Exported:   manifest.Exported,
		Recipients: manifest.Recipients,
		Counts: Counts{
			Contacts:     manifest.Counts["contacts"],
			Chats:        manifest.Counts["chats"],
			Groups:       manifest.Counts["groups"],
			Participants: participants,
			Messages:     manifest.Counts["messages"],
			Revisions:    manifest.Counts["message_revisions"],
			Identity:     manifest.Counts["archive_identity"],
			MediaFiles:   len(manifest.Files),
		},
		Shards: manifest.Shards,
		Files:  manifest.Files,
	}
}

func collectBackupMedia(ctx context.Context, dbPath string, messages []store.Message) ([]ckbackup.File, error) {
	root, err := archiveRoot(dbPath)
	if err != nil {
		return nil, err
	}
	files, err := ckbackup.CollectFiles(ctx, filepath.Join(root, "media"), "media")
	if err != nil {
		return nil, err
	}
	logicalBySource := make(map[string]string, len(files))
	for _, file := range files {
		absolute, err := filepath.Abs(file.Source)
		if err != nil {
			return nil, err
		}
		logicalBySource[filepath.Clean(absolute)] = file.Path
	}
	for index := range messages {
		mediaPath := messages[index].MediaPath
		if mediaPath == "" {
			continue
		}
		absolute, err := filepath.Abs(mediaPath)
		if err != nil {
			return nil, err
		}
		if logical, ok := logicalBySource[filepath.Clean(absolute)]; ok {
			messages[index].MediaPath = logical
		}
	}
	return files, nil
}

func localizeMediaPaths(messages []store.Message, root string) error {
	for index := range messages {
		value := filepath.ToSlash(messages[index].MediaPath)
		if value == "media" || !strings.HasPrefix(value, "media/") {
			continue
		}
		clean := path.Clean(value)
		if clean == "media" || !strings.HasPrefix(clean, "media/") {
			return fmt.Errorf("backup media path escapes archive root: %s", value)
		}
		target := filepath.Clean(filepath.Join(root, filepath.FromSlash(clean)))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("backup media path escapes archive root: %s", value)
		}
		messages[index].MediaPath = target
	}
	return nil
}

func archiveRoot(dbPath string) (string, error) {
	absolute, err := filepath.Abs(filepath.Dir(dbPath))
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func replaceMediaDuring(staged, target string, commit func() error) error {
	stagedInfo, err := os.Lstat(staged)
	if err != nil {
		return err
	}
	if stagedInfo.Mode()&os.ModeSymlink != 0 || !stagedInfo.IsDir() {
		return fmt.Errorf("staged media is not a directory: %s", staged)
	}
	parent := filepath.Dir(target)
	previous, err := os.MkdirTemp(parent, ".wacrawl-media-previous-")
	if err != nil {
		return err
	}
	if err := os.Remove(previous); err != nil {
		return err
	}
	hadPrevious := false
	if targetInfo, statErr := os.Lstat(target); statErr == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.IsDir() {
			return fmt.Errorf("archive media is not a directory: %s", target)
		}
		if err := os.Rename(target, previous); err != nil {
			return err
		}
		hadPrevious = true
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.Rename(staged, target); err != nil {
		var restoreErr error
		if hadPrevious {
			restoreErr = os.Rename(previous, target)
		}
		return errors.Join(err, restoreErr)
	}
	if err := commit(); err != nil {
		removeErr := os.RemoveAll(target)
		var restoreErr error
		if hadPrevious {
			restoreErr = os.Rename(previous, target)
		}
		return errors.Join(err, removeErr, restoreErr)
	}
	if hadPrevious {
		_ = os.RemoveAll(previous)
	}
	return nil
}

const backupReadme = `# backup-wacrawl

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

The command refreshes the local wacrawl archive according to the normal sync
policy, writes encrypted shards and media blobs, updates the manifest, and
commits only owned backup artifacts. Explicit pushes verify unpublished history
first. Divergence requires explicit handling; pushing does not rebase commits.

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

func writeBackupReadme(repo string) error {
	path := filepath.Join(repo, "README.md")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(backupReadme), 0o600)
}
