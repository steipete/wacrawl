package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	ckbackup "github.com/openclaw/crawlkit/backup"
	"github.com/openclaw/wacrawl/internal/store"
)

func TestEncryptedBackupPushPull(t *testing.T) {
	ctx := context.Background()
	source := openFixtureStore(t, "source.db")
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	data := store.SnapshotData{
		SourceStoreIdentity: "wa-store:fixture",
		AccountIdentity:     "wa-account:fixture",
		Contacts:            []store.Contact{{JID: "alice@s.whatsapp.net", FullName: "Alice", UpdatedAt: now}},
		Chats:               []store.Chat{{JID: "chat@g.us", Kind: "group", Name: "Launch Group", LastMessageAt: now}},
		Groups:              []store.Group{{JID: "chat@g.us", Name: "Launch Group", OwnerJID: "owner@s.whatsapp.net", CreatedAt: now}},
		Participants:        []store.GroupParticipant{{GroupJID: "chat@g.us", UserJID: "alice@s.whatsapp.net", ContactName: "Alice", IsAdmin: true, IsActive: true}},
		Messages: []store.Message{
			{SourcePK: 1, ChatJID: "chat@g.us", ChatName: "Launch Group", MessageID: "a", SenderJID: "alice@s.whatsapp.net", SenderName: "Alice", Timestamp: now, Text: "secret launch text", RawType: 0, MessageType: "text"},
		},
	}
	mediaPath := filepath.Join(filepath.Dir(source.Path()), "media", "photo.jpg ")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("private media bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	data.Messages[0].MediaType = "image"
	data.Messages[0].MediaPath = mediaPath
	if err := source.ImportSnapshot(ctx, data, "/fixture", now); err != nil {
		t.Fatal(err)
	}
	edited := data.Messages[0]
	edited.Text = "edited launch text"
	if err := source.MergeAll(ctx, store.ImportStats{SourcePath: "/fixture", SourceStoreIdentity: data.SourceStoreIdentity, AccountIdentity: data.AccountIdentity, Messages: 1, FinishedAt: now.Add(time.Minute)}, nil, nil, nil, nil, []store.Message{edited}); err != nil {
		t.Fatal(err)
	}

	remote := filepath.Join(t.TempDir(), "remote.git")
	initBareRemote(t, remote)
	repo := filepath.Join(t.TempDir(), "backup")
	identity := filepath.Join(t.TempDir(), "age.key")
	configPath := filepath.Join(t.TempDir(), "backup.json")
	cfg, recipient, err := Init(ctx, Options{ConfigPath: configPath, Repo: repo, Remote: remote, Identity: identity, Push: false})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo != repo || !strings.HasPrefix(recipient, "age1") {
		t.Fatalf("unexpected init cfg=%+v recipient=%q", cfg, recipient)
	}
	opts := Options{ConfigPath: configPath, Push: false}
	result, err := Push(ctx, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Messages != 1 || result.MediaFiles != 1 || result.Shards == 0 {
		t.Fatalf("unexpected push result: %+v", result)
	}
	second, err := Push(ctx, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("second push should be unchanged: %+v", second)
	}
	status, statusRepo, err := Status(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if statusRepo != repo || status.Counts.Messages != 1 || status.Counts.Revisions != 1 || status.Counts.MediaFiles != 1 {
		t.Fatalf("unexpected backup status repo=%s status=%+v", statusRepo, status)
	}
	manifest, err := readManifest(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Encrypted || manifest.Counts.Messages != 1 || manifest.Counts.Revisions != 1 || manifest.Counts.Identity != 1 || len(manifest.Files) != 1 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	ciphertext, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(manifest.Shards[len(manifest.Shards)-1].Path))) // #nosec G304 -- test reads a generated shard path from its temp repo manifest.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "secret launch text") {
		t.Fatal("encrypted shard contains plaintext")
	}
	manifestBody, err := os.ReadFile(filepath.Join(repo, "manifest.json")) // #nosec G304 -- test reads the manifest from its temp backup repo.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestBody), "photo.jpg ") || strings.Contains(string(manifestBody), "private media bytes") {
		t.Fatalf("backup manifest exposes media details: %s", manifestBody)
	}

	restored := openFixtureStore(t, "restored.db")
	pulled, err := Pull(ctx, restored, opts)
	if err != nil {
		t.Fatal(err)
	}
	if pulled.Messages != 1 || pulled.MediaFiles != 1 {
		t.Fatalf("unexpected pull result: %+v", pulled)
	}
	results, err := restored.Search(ctx, store.MessageFilter{Query: "edited", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Text != "edited launch text" {
		t.Fatalf("restore search mismatch: %+v", results)
	}
	restoredStatus, err := restored.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restoredStatus.MessageRevisions != 1 {
		t.Fatalf("restored revisions = %d", restoredStatus.MessageRevisions)
	}
	restoredData, err := restored.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(restoredData.Revisions) != 1 || !strings.Contains(restoredData.Revisions[0].PayloadJSON, "secret launch text") || restoredData.AccountIdentity != data.AccountIdentity || restoredData.SourceStoreIdentity != data.SourceStoreIdentity {
		t.Fatalf("restored revision = %+v", restoredData.Revisions)
	}
	wantRestoredMedia := filepath.Join(filepath.Dir(restored.Path()), "media", "photo.jpg ")
	if results[0].MediaPath != wantRestoredMedia {
		t.Fatalf("restored media path = %q, want %q", results[0].MediaPath, wantRestoredMedia)
	}
	mediaBody, err := os.ReadFile(wantRestoredMedia) // #nosec G304 -- test reads its expected temp restore path.
	if err != nil || string(mediaBody) != "private media bytes" {
		t.Fatalf("restored media = %q err=%v", mediaBody, err)
	}

	noMediaRestored := openFixtureStore(t, "no-media-restored.db")
	noMediaPulled, err := Pull(ctx, noMediaRestored, Options{ConfigPath: configPath, NoMedia: true})
	if err != nil {
		t.Fatal(err)
	}
	if noMediaPulled.Messages != 1 || noMediaPulled.MediaFiles != 1 {
		t.Fatalf("unexpected no-media pull result: %+v", noMediaPulled)
	}
	noMediaResults, err := noMediaRestored.Search(ctx, store.MessageFilter{Query: "edited", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(noMediaResults) != 1 || noMediaResults[0].MediaPath != "media/photo.jpg " {
		t.Fatalf("no-media restore should keep logical media path, got %+v", noMediaResults)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(noMediaRestored.Path()), "media", "photo.jpg ")); !os.IsNotExist(err) {
		t.Fatalf("no-media restore should not create local media file, stat err=%v", err)
	}

	secondIdentity := filepath.Join(t.TempDir(), "second-age.key")
	secondRecipient, err := EnsureIdentity(secondIdentity)
	if err != nil {
		t.Fatal(err)
	}
	updatedCfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updatedCfg.Recipients = append(updatedCfg.Recipients, secondRecipient)
	if err := SaveConfig(configPath, updatedCfg); err != nil {
		t.Fatal(err)
	}
	recipientChange, err := Push(ctx, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !recipientChange.Changed {
		t.Fatal("adding a recipient should re-encrypt unchanged shards")
	}
	secondRestored := openFixtureStore(t, "second-restored.db")
	secondPulled, err := Pull(ctx, secondRestored, Options{ConfigPath: configPath, Identity: secondIdentity})
	if err != nil {
		t.Fatal(err)
	}
	if secondPulled.Messages != 1 {
		t.Fatalf("unexpected second-recipient pull result: %+v", secondPulled)
	}
	secondResults, err := secondRestored.Search(ctx, store.MessageFilter{Query: "edited", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondResults) != 1 || secondResults[0].Text != "edited launch text" {
		t.Fatalf("second-recipient restore mismatch: %+v", secondResults)
	}
	sameRecipients, err := Push(ctx, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sameRecipients.Changed {
		t.Fatalf("unchanged recipients should not rewrite backup: %+v", sameRecipients)
	}

	derivedRepo := filepath.Join(t.TempDir(), "derived-recipient")
	if err := os.MkdirAll(derivedRepo, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, derivedRepo, "init")
	derived, err := Push(ctx, source, Options{Repo: derivedRepo, Identity: identity, Push: false, NoMedia: true})
	if err != nil {
		t.Fatal(err)
	}
	if !derived.Changed || derived.Messages != 1 || derived.MediaFiles != 0 {
		t.Fatalf("unexpected derived-recipient push: %+v", derived)
	}

	data.Messages = append(data.Messages, store.Message{SourcePK: 2, ChatJID: "chat@g.us", ChatName: "Launch Group", MessageID: "b", SenderJID: "alice@s.whatsapp.net", SenderName: "Alice", Timestamp: now.Add(time.Second), Text: "second secret", RawType: 0, MessageType: "text"})
	if err := source.ImportSnapshot(ctx, data, "/fixture", now); err != nil {
		t.Fatal(err)
	}
	pushed, err := Push(ctx, source, Options{ConfigPath: configPath, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pushed.Changed || pushed.Messages != 2 {
		t.Fatalf("unexpected pushed backup: %+v", pushed)
	}
}

func TestReplaceMediaDuringRollsBackFailedImport(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "media")
	staged := filepath.Join(dir, "staged")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "new"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("import failed")
	if err := replaceMediaDuring(staged, target, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("replace error = %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(target, "old")); err != nil || string(body) != "old" { // #nosec G304 -- test reads its temp media target.
		t.Fatalf("previous media was not restored: %q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(target, "new")); !os.IsNotExist(err) {
		t.Fatalf("failed media remained after rollback: %v", err)
	}
	staged = filepath.Join(dir, "staged-success")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "new"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceMediaDuring(staged, target, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(target, "new")); err != nil || string(body) != "new" { // #nosec G304 -- test reads its temp media target.
		t.Fatalf("promoted media = %q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(target, "old")); !os.IsNotExist(err) {
		t.Fatalf("previous media remained after success: %v", err)
	}
}

func TestReplaceMediaDuringValidatesAndHandlesNoPreviousMedia(t *testing.T) {
	dir := t.TempDir()
	stagedFile := filepath.Join(dir, "staged-file")
	if err := os.WriteFile(stagedFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceMediaDuring(stagedFile, filepath.Join(dir, "media"), func() error { return nil }); err == nil {
		t.Fatal("regular staged file should fail")
	}

	staged := filepath.Join(dir, "staged")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "new"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "media")
	if err := os.WriteFile(target, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceMediaDuring(staged, target, func() error { return nil }); err == nil {
		t.Fatal("regular target file should fail")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := replaceMediaDuring(staged, target, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(target, "new")); err != nil || string(body) != "new" { // #nosec G304 -- test reads its temp media target.
		t.Fatalf("new media without previous directory = %q err=%v", body, err)
	}
}

func TestLocalizeMediaPathsRejectsEscape(t *testing.T) {
	messages := []store.Message{{MediaPath: "media/../outside"}}
	if err := localizeMediaPaths(messages, t.TempDir()); err == nil {
		t.Fatal("escaping portable media path should fail")
	}
}

func TestDecodeSnapshotRejectsInvalidShards(t *testing.T) {
	for _, table := range []string{"contacts", "chats", "groups", "group_participants", "messages", "unknown"} {
		t.Run(table, func(t *testing.T) {
			shard := ckbackup.DecodedShard{
				Entry:     ckbackup.ShardEntry{Table: table},
				Plaintext: []byte("{"),
			}
			if _, err := decodeSnapshot([]ckbackup.DecodedShard{shard}); err == nil {
				t.Fatal("invalid shard should fail")
			}
		})
	}
}

func TestHistoricalSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	source := openFixtureStore(t, "history-source.db")
	now := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	data := store.SnapshotData{
		Chats: []store.Chat{{JID: "chat@g.us", Kind: "group", Name: "History", LastMessageAt: now}},
		Messages: []store.Message{{
			SourcePK: 1, ChatJID: "chat@g.us", ChatName: "History", MessageID: "first",
			SenderJID: "alice@s.whatsapp.net", Timestamp: now, Text: "first snapshot", MessageType: "text",
		}},
	}
	mediaPath := filepath.Join(filepath.Dir(source.Path()), "media", "history.jpg")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("first historical media"), 0o600); err != nil {
		t.Fatal(err)
	}
	data.Messages[0].MediaType = "image"
	data.Messages[0].MediaPath = mediaPath
	if err := source.ImportSnapshot(ctx, data, "/fixture", now); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(t.TempDir(), "backup")
	remote := filepath.Join(t.TempDir(), "remote.git")
	initBareRemote(t, remote)
	identity := filepath.Join(t.TempDir(), "age.key")
	configPath := filepath.Join(t.TempDir(), "backup.json")
	if _, _, err := Init(ctx, Options{ConfigPath: configPath, Repo: repo, Remote: remote, Identity: identity, Push: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, source, Options{ConfigPath: configPath, Push: false, Tag: "snapshot/initial"}); err != nil {
		t.Fatal(err)
	}
	idempotent, err := Push(ctx, source, Options{ConfigPath: configPath, Push: false, Tag: "snapshot/initial"})
	if err != nil {
		t.Fatal(err)
	}
	if idempotent.Changed || idempotent.Tag != "snapshot/initial" {
		t.Fatalf("unexpected idempotent tagged push: %+v", idempotent)
	}
	initial, err := resolveCommit(ctx, repo, "snapshot/initial")
	if err != nil {
		t.Fatal(err)
	}

	data.Messages = append(data.Messages, store.Message{
		SourcePK: 2, ChatJID: "chat@g.us", ChatName: "History", MessageID: "second",
		SenderJID: "alice@s.whatsapp.net", Timestamp: now.Add(time.Minute), Text: "second snapshot", MessageType: "text",
	})
	if err := os.WriteFile(mediaPath, []byte("second historical media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.ImportSnapshot(ctx, data, "/fixture", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, source, Options{ConfigPath: configPath, Push: false}); err != nil {
		t.Fatal(err)
	}
	current, err := resolveCommit(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if current == initial {
		t.Fatal("updated backup did not create a new snapshot commit")
	}
	tagged, err := Push(ctx, source, Options{ConfigPath: configPath, Push: true, Tag: "snapshot/current"})
	if err != nil {
		t.Fatal(err)
	}
	if tagged.Changed || tagged.Tag != "snapshot/current" {
		t.Fatalf("unexpected unchanged tagged push: %+v", tagged)
	}
	remoteTags, err := gitOutput(ctx, repo, "ls-remote", "--tags", "origin", "refs/tags/snapshot/current")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(remoteTags), "refs/tags/snapshot/current") {
		t.Fatalf("snapshot tag was not pushed: %s", remoteTags)
	}

	snapshots, snapshotsRepo, err := Snapshots(ctx, Options{ConfigPath: configPath, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if snapshotsRepo != repo || len(snapshots) != 2 {
		t.Fatalf("unexpected snapshots repo=%s snapshots=%+v", snapshotsRepo, snapshots)
	}
	if snapshots[0].Ref != current || snapshots[0].Counts.Messages != 2 || len(snapshots[0].Tags) != 1 || snapshots[0].Tags[0] != "snapshot/current" {
		t.Fatalf("unexpected current snapshot: %+v", snapshots[0])
	}
	if snapshots[1].Ref != initial || len(snapshots[1].Tags) != 1 || snapshots[1].Tags[0] != "snapshot/initial" {
		t.Fatalf("unexpected tagged snapshot: %+v", snapshots[1])
	}
	peer := filepath.Join(t.TempDir(), "peer")
	runGit(t, "", "clone", remote, peer)
	if err := os.WriteFile(filepath.Join(peer, "remote-note.txt"), []byte("remote advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, peer, "add", "remote-note.txt")
	runGit(t, peer, "commit", "-m", "test: advance remote")
	runGit(t, peer, "push", "origin", "HEAD")
	remoteHead, err := resolveCommit(ctx, peer, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if remoteHead == current {
		t.Fatal("remote test commit did not advance")
	}

	restored := openFixtureStore(t, "history-restored.db")
	pulled, err := Pull(ctx, restored, Options{ConfigPath: configPath, Ref: "snapshot/initial"})
	if err != nil {
		t.Fatal(err)
	}
	if pulled.Messages != 1 || pulled.MediaFiles != 1 || pulled.Ref != initial {
		t.Fatalf("unexpected historical pull: %+v", pulled)
	}
	after, err := resolveCommit(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if after != current {
		t.Fatalf("historical pull changed checkout: before=%s after=%s", current, after)
	}
	results, err := restored.Search(ctx, store.MessageFilter{Query: "snapshot", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MessageID != "first" {
		t.Fatalf("historical restore mismatch: %+v", results)
	}
	historicalMedia, err := os.ReadFile(filepath.Join(filepath.Dir(restored.Path()), "media", "history.jpg"))
	if err != nil || string(historicalMedia) != "first historical media" {
		t.Fatalf("historical media = %q err=%v", historicalMedia, err)
	}
	if _, err := Pull(ctx, restored, Options{ConfigPath: configPath, Ref: "missing-ref"}); err == nil {
		t.Fatal("missing historical ref should fail")
	}
	if _, err := Push(ctx, source, Options{ConfigPath: configPath, Push: false, Tag: "snapshot/initial"}); err == nil {
		t.Fatal("moving an existing snapshot tag should fail")
	}
}

func TestEmptyBackupPreservesCountsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openFixtureStore(t, "empty.db")
	repo := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	identity := filepath.Join(t.TempDir(), "age.key")
	recipient, err := EnsureIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Repo: repo, Remote: "", Identity: identity, Recipients: []string{recipient}, Push: false}
	first, err := Push(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Fatal("first empty backup should commit")
	}
	manifest, err := ckbackup.ReadManifest(repo)
	if err != nil {
		t.Fatal(err)
	}
	if messages, ok := manifest.Counts["messages"]; !ok || messages != 0 {
		t.Fatalf("empty message count missing: %+v", manifest.Counts)
	}
	second, err := Push(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatal("identical empty backup should not commit")
	}
}

func TestSnapshotHistoryValidation(t *testing.T) {
	ctx := context.Background()
	if err := ensureRepoForRead(ctx, Config{}); err == nil {
		t.Fatal("empty backup repo path should fail")
	}
	createdRepo := filepath.Join(t.TempDir(), "created-backup")
	if err := ensureRepoForRead(ctx, Config{Repo: createdRepo}); err != nil {
		t.Fatalf("read-only setup should initialize a missing repository: %v", err)
	}
	localRepo := filepath.Join(t.TempDir(), "local-backup")
	runGit(t, "", "init", localRepo)
	if err := ensureRepoForRead(ctx, Config{Repo: localRepo}); err != nil {
		t.Fatalf("read-only setup without origin: %v", err)
	}

	repo := filepath.Join(t.TempDir(), "backup")
	remote := filepath.Join(t.TempDir(), "remote.git")
	initBareRemote(t, remote)
	configPath := filepath.Join(t.TempDir(), "backup.json")
	if _, _, err := Init(ctx, Options{ConfigPath: configPath, Repo: repo, Remote: remote, Identity: filepath.Join(t.TempDir(), "age.key"), Push: false}); err != nil {
		t.Fatal(err)
	}
	snapshots, _, err := Snapshots(ctx, Options{ConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("new backup repo should have no data snapshots: %+v", snapshots)
	}
	if _, _, err := Snapshots(ctx, Options{ConfigPath: configPath, Limit: -1}); err == nil {
		t.Fatal("negative snapshot limit should fail")
	}
	if err := validateSnapshotTag(ctx, repo, "not a tag"); err == nil {
		t.Fatal("invalid snapshot tag should fail")
	}
	if tag, err := tagSnapshot(ctx, Config{Repo: repo}, ""); err != nil || tag != "" {
		t.Fatalf("empty snapshot tag = %q, %v", tag, err)
	}
	if _, err := resolveCommit(ctx, repo, ""); err == nil {
		t.Fatal("empty backup ref should fail")
	}
	if _, err := decodeManifest([]byte("{")); err == nil {
		t.Fatal("invalid manifest JSON should fail")
	}
	if got := shortRef("short"); got != "short" {
		t.Fatalf("short ref = %q", got)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	cfg := DefaultConfig()
	cfg.Repo = "~/Projects/example"
	cfg.Recipients = []string{"age1example"}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Repo != cfg.Repo || loaded.Recipients[0] != "age1example" {
		t.Fatalf("config mismatch: %+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %#o, want 0600", got)
	}
	if DefaultConfigPath() == "" {
		t.Fatal("default config path should not be empty")
	}
	if expandHome("~") == "~" || !strings.Contains(expandHome("~/Projects/example"), "Projects") {
		t.Fatal("home expansion did not expand")
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(t.TempDir()); err == nil {
		t.Fatal("expected directory config load error")
	}
	if err := SaveConfig(t.TempDir(), cfg); err == nil {
		t.Fatal("expected directory config save error")
	}

	badJSON := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badJSON, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(badJSON); err == nil {
		t.Fatal("expected invalid config JSON error")
	}

	overridePath := filepath.Join(t.TempDir(), "override.json")
	if err := SaveConfig(overridePath, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveOptions(Options{
		ConfigPath: overridePath,
		Repo:       "~/Projects/backup",
		Remote:     "https://example.invalid/repo.git",
		Identity:   "~/.wacrawl/test.key",
		Recipients: []string{" age1two ", "age1one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resolved.Repo, filepath.Join("Projects", "backup")) || resolved.Remote != "https://example.invalid/repo.git" || !strings.Contains(resolved.Identity, filepath.Join(".wacrawl", "test.key")) {
		t.Fatalf("options did not resolve overrides: %+v", resolved)
	}
	if strings.Join(resolved.Recipients, ",") != " age1two ,age1one" {
		t.Fatalf("recipient overrides changed: %#v", resolved.Recipients)
	}
}

func TestSaveConfigPreservesExistingConfigOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.json")
	original := DefaultConfig()
	original.Repo = "~/Projects/original"
	original.Recipients = []string{"age1original"}
	if err := SaveConfig(path, original); err != nil {
		t.Fatal(err)
	}

	renameErr := errors.New("rename stopped")
	previousRename := renameConfigFile
	renameConfigFile = func(_, _ string) error {
		return renameErr
	}
	defer func() {
		renameConfigFile = previousRename
	}()

	updated := original
	updated.Repo = "~/Projects/updated"
	updated.Recipients = []string{"age1updated"}
	if err := SaveConfig(path, updated); !errors.Is(err, renameErr) {
		t.Fatalf("SaveConfig error = %v, want %v", err, renameErr)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Repo != original.Repo || strings.Join(loaded.Recipients, ",") != "age1original" {
		t.Fatalf("config was replaced after failed rename: %+v", loaded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".backup.json.") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary config file was not cleaned up: %s", entry.Name())
		}
	}
}

func TestAtomicConfigWriteReportsFilesystemErrors(t *testing.T) {
	missingParent := filepath.Join(t.TempDir(), "missing", "backup.json")
	if err := writeFileAtomic(missingParent, []byte("{}\n"), 0o600); err == nil {
		t.Fatal("expected missing parent error")
	}
	if err := syncConfigDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected missing directory sync error")
	}
}

func TestCryptoHelpers(t *testing.T) {
	identity := filepath.Join(t.TempDir(), "age.key")
	recipient, err := EnsureIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	again, err := EnsureIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if again != recipient {
		t.Fatalf("recipient changed: %q != %q", again, recipient)
	}
	fromIdentity, err := RecipientFromIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if fromIdentity != recipient {
		t.Fatalf("recipient mismatch: %q != %q", fromIdentity, recipient)
	}
	encrypted, hash, err := encryptShard([]byte("private text\n"), []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	if hash != sha256Hex([]byte("private text\n")) || strings.Contains(string(encrypted), "private text") {
		t.Fatal("encrypted shard mismatch")
	}
	tmp := filepath.Join(t.TempDir(), "shard.age")
	if err := os.WriteFile(tmp, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	plain, err := decryptShard(encrypted, identity)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "private text\n" {
		t.Fatalf("decrypt mismatch: %q", plain)
	}
	if _, _, err := encryptShard([]byte("x"), []string{"bad"}); err == nil {
		t.Fatal("expected bad recipient error")
	}
	if _, _, err := encryptShard([]byte("x"), nil); err == nil {
		t.Fatal("expected missing recipient encrypt error")
	}
	emptyIdentity := filepath.Join(t.TempDir(), "empty.key")
	if err := os.WriteFile(emptyIdentity, []byte("# comment\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecipientFromIdentity(emptyIdentity); err == nil {
		t.Fatal("expected empty identity error")
	}
	badIdentity := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badIdentity, []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureIdentity(badIdentity); err == nil {
		t.Fatal("expected bad existing identity error")
	}
	if _, err := RecipientFromIdentity(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Fatal("expected missing identity error")
	}
	if _, err := RecipientFromIdentity(badIdentity); err == nil {
		t.Fatal("expected bad identity parse error")
	}
	if _, err := decryptShard([]byte("not age"), identity); err == nil {
		t.Fatal("expected bad ciphertext error")
	}
	otherIdentity := filepath.Join(t.TempDir(), "other.key")
	if _, err := EnsureIdentity(otherIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := decryptShard(encrypted, otherIdentity); err == nil {
		t.Fatal("expected wrong identity decrypt error")
	}
	recipientValue, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		t.Fatal(err)
	}
	var rawAge bytes.Buffer
	w, err := age.Encrypt(&rawAge, recipientValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("not gzip")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := decryptShard(rawAge.Bytes(), identity); err == nil {
		t.Fatal("expected non-gzip decrypt error")
	}
	if _, err := EnsureIdentity(filepath.Join(t.TempDir(), "missing", "dir")); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotErrorAndUtilityPaths(t *testing.T) {
	if _, _, err := ckbackup.EncodeJSONL(1); err == nil {
		t.Fatal("expected unsupported JSONL row type")
	}
	var contacts []store.Contact
	if err := ckbackup.DecodeJSONL([]byte("{bad json}\n"), &contacts); err == nil {
		t.Fatal("expected invalid JSONL error")
	}
	if err := ckbackup.RemoveStaleShards(t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if ckbackup.EquivalentManifest(toCrawlkitManifest(Manifest{Format: 1}), toCrawlkitManifest(Manifest{Format: 2})) {
		t.Fatal("different manifests should not be equivalent")
	}
	if _, err := readSnapshot(Config{}, Manifest{Format: 99}); err == nil {
		t.Fatal("expected unsupported format error")
	}
	if _, err := readSnapshot(Config{}, Manifest{Format: formatVersion, Shards: []ShardEntry{{Table: "nope"}}}); err == nil {
		t.Fatal("expected shard read error")
	}
	identity := filepath.Join(t.TempDir(), "age.key")
	recipient, err := EnsureIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if _, err := ckbackup.ResolveShardPath(repo, "../outside.age"); err == nil {
		t.Fatal("expected escaping shard path error")
	}
	if _, err := ckbackup.ResolveShardPath(repo, "manifest.json"); err == nil {
		t.Fatal("expected invalid shard path error")
	}
	encrypted, hash, err := encryptShard([]byte("{}\n"), []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	shardPath := filepath.Join("data", "unknown.jsonl.gz.age")
	fullShardPath := filepath.Join(repo, shardPath)
	if err := os.MkdirAll(filepath.Dir(fullShardPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullShardPath, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Repo: repo, Identity: identity}
	unknownManifest := Manifest{Format: formatVersion, Shards: []ShardEntry{{Table: "unknown", Path: filepath.ToSlash(shardPath), SHA256: hash}}}
	if _, err := readSnapshot(cfg, unknownManifest); err == nil {
		t.Fatal("expected unknown table error")
	}
	badHashManifest := Manifest{Format: formatVersion, Shards: []ShardEntry{{Table: "contacts", Path: filepath.ToSlash(shardPath), SHA256: "bad"}}}
	if _, err := readSnapshot(cfg, badHashManifest); err == nil {
		t.Fatal("expected hash mismatch")
	}
	duplicatePlain, duplicateHash, err := encryptShard([]byte(`{"source_pk":1,"chat_jid":"chat","message_id":"a","timestamp":"2026-04-27T12:00:00Z","raw_type":0}`+"\n"+`{"source_pk":1,"chat_jid":"chat","message_id":"b","timestamp":"2026-04-27T12:00:01Z","raw_type":0}`+"\n"), []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	duplicatePath := filepath.Join("data", "duplicate.jsonl.gz.age")
	fullDuplicatePath := filepath.Join(repo, duplicatePath)
	if err := os.WriteFile(fullDuplicatePath, duplicatePlain, 0o600); err != nil {
		t.Fatal(err)
	}
	duplicateManifest := Manifest{Format: formatVersion, Shards: []ShardEntry{{Table: "messages", Path: filepath.ToSlash(duplicatePath), SHA256: duplicateHash}}}
	duplicateData, err := readSnapshot(cfg, duplicateManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicateData.Validate(); err == nil {
		t.Fatal("expected duplicate restored data validation error")
	}
	if err := ckbackup.WriteManifest(repo, toCrawlkitManifest(Manifest{Format: formatVersion})); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(filepath.Join(repo, "missing")); err == nil {
		t.Fatal("expected missing manifest error")
	}
	unknown := store.Message{SourcePK: 1, ChatJID: "chat", MessageID: "a"}
	shards := messageShards([]store.Message{unknown})
	if len(shards) != 1 || !strings.Contains(shards[0].path, "unknown") {
		t.Fatalf("unexpected unknown-time shard: %+v", shards)
	}
	stalePath := filepath.Join(repo, "data", "stale.age")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ckbackup.RemoveStaleShards(repo, []ShardEntry{{Path: filepath.ToSlash(shardPath)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatal("expected stale shard removal")
	}

	left := Manifest{
		Format:     formatVersion,
		Encrypted:  true,
		Recipients: []string{" age1b", "age1a", "age1a"},
		Counts:     Counts{Messages: 1},
		Shards:     []ShardEntry{{Table: "messages", Path: "data/messages/2026/05.jsonl.gz.age", Rows: 1, SHA256: "abc", Bytes: 10}},
	}
	right := left
	right.Recipients = []string{"age1a", "age1b"}
	right.Shards = append([]ShardEntry(nil), left.Shards...)
	right.Shards[0].Bytes = 20
	if !ckbackup.EquivalentManifest(toCrawlkitManifest(left), toCrawlkitManifest(right)) {
		t.Fatal("equivalent manifests should ignore recipient order and byte drift")
	}
	right.Shards[0].SHA256 = "def"
	if ckbackup.EquivalentManifest(toCrawlkitManifest(left), toCrawlkitManifest(right)) {
		t.Fatal("manifest hash changes should not be equivalent")
	}
}

func TestGitHelpersWithoutRemote(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	cfg := Config{Repo: repo}
	if err := ensureRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	changed, err := commitAndPush(ctx, cfg, "test: no changes", false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("empty repo without changes should not commit")
	}
}

func TestEnsureRepoFallsBackToLocalInitWhenCloneFails(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "backup")
	remote := filepath.Join(t.TempDir(), "missing.git")
	if err := ensureRepo(ctx, Config{Repo: repo, Remote: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	out, err := gitOutput(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != remote {
		t.Fatalf("origin = %q, want %q", strings.TrimSpace(string(out)), remote)
	}
}

func TestTopLevelErrorPaths(t *testing.T) {
	ctx := context.Background()
	source := openFixtureStore(t, "source.db")
	badConfig := t.TempDir()
	if _, _, err := Init(ctx, Options{ConfigPath: badConfig}); err == nil {
		t.Fatal("expected init config load error")
	}
	if _, err := Push(ctx, source, Options{ConfigPath: badConfig}); err == nil {
		t.Fatal("expected push config load error")
	}
	if _, err := Pull(ctx, source, Options{ConfigPath: badConfig}); err == nil {
		t.Fatal("expected pull config load error")
	}
	if _, _, err := Status(ctx, Options{ConfigPath: badConfig}); err == nil {
		t.Fatal("expected status config load error")
	}

	repo := t.TempDir()
	runGit(t, repo, "init")
	if _, err := Pull(ctx, source, Options{Repo: repo, Identity: filepath.Join(t.TempDir(), "age.key")}); err == nil {
		t.Fatal("expected missing manifest pull error")
	}
	if _, _, err := Status(ctx, Options{Repo: repo}); err == nil {
		t.Fatal("expected missing manifest status error")
	}

	now := time.Now().UTC()
	if err := source.ImportSnapshot(ctx, store.SnapshotData{
		Chats:    []store.Chat{{JID: "chat", Kind: "dm", Name: "Chat", LastMessageAt: now}},
		Messages: []store.Message{{SourcePK: 1, ChatJID: "chat", MessageID: "a", Timestamp: now, RawType: 0, Text: "hello"}},
	}, "/fixture", now); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, source, Options{Repo: repo, Recipients: []string{"bad"}, Push: false}); err == nil {
		t.Fatal("expected bad recipient push error")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "age.key")
	recipient, err := EnsureIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, source, Options{Repo: repo, Identity: identity, Recipients: []string{recipient}, Push: false}); err == nil {
		t.Fatal("expected closed store push error")
	}
	if err := ensureRepo(ctx, Config{}); err == nil {
		t.Fatal("expected empty repo path error")
	}
	if _, err := commitAndPush(ctx, Config{Repo: filepath.Join(t.TempDir(), "missing")}, "test", false); err == nil {
		t.Fatal("expected commit in missing repo error")
	}
}

func openFixtureStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func initBareRemote(t *testing.T, dir string) {
	t.Helper()
	runGit(t, "", "init", "--bare", dir)
	// Detached receive-side maintenance can outlive the push and race TempDir cleanup.
	runGit(t, dir, "config", "maintenance.auto", "false")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitOutput(context.Background(), dir, args...); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- tests pass only fixed Git commands and temporary paths.
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_AUTHOR_NAME=wacrawl-test",
		"GIT_AUTHOR_EMAIL=wacrawl-test@example.invalid",
		"GIT_COMMITTER_NAME=wacrawl-test",
		"GIT_COMMITTER_EMAIL=wacrawl-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
