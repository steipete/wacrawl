package whatsappdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ckbackup "github.com/openclaw/crawlkit/backup"
	"github.com/openclaw/wacrawl/internal/backup"
	"github.com/openclaw/wacrawl/internal/store"
)

func TestAuditInvalidMediaPreservesMetadata(t *testing.T) {
	for _, kind := range []string{"absolute", "traversal", "message-symlink", "root-symlink", "parent-symlink", "leaf-symlink", "directory", "null-text"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st, source, cache := auditMediaFixture(t)
			db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if kind == "null-text" {
				mustExec(t, db, `update ZWAMESSAGE set ZTEXT=null,ZMESSAGETYPE=0 where Z_PK=3;
update ZWAMEDIAITEM set ZTITLE='',ZMEDIAURL='',ZFILESIZE=0 where Z_PK=1;`)
			}
			if _, err := Import(ctx, st, source); err != nil {
				t.Fatal(err)
			}
			before, err := st.ExportAll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(outside, []byte("preserved outside bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := "Media/123@g.us/a/test.jpg"
			switch kind {
			case "absolute", "null-text":
				path = outside
			case "traversal":
				path = "Media/../../sentinel"
			case "message-symlink", "root-symlink", "parent-symlink":
				target := filepath.Join(source, "Message")
				switch kind {
				case "root-symlink":
					target = filepath.Join(target, "Media")
				case "parent-symlink":
					target = filepath.Dir(cache)
				}
				moved := filepath.Join(t.TempDir(), "moved")
				if err := os.Rename(target, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, target); err != nil {
					t.Fatal(err)
				}
			case "leaf-symlink", "directory":
				if err := os.Remove(cache); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					err = os.Mkdir(cache, 0o700)
				} else {
					err = os.Symlink(outside, cache)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`update ZWAMEDIAITEM set ZMEDIALOCALPATH=? where Z_PK=1`, path); err != nil {
				t.Fatal(err)
			}
			snap, err := SnapshotPath(source)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(snap.Root) }()
			extracted, err := Extract(ctx, snap)
			if err != nil || len(extracted.Messages) != len(before.Messages) {
				t.Fatalf("metadata extraction dropped rows: %+v, %v", extracted, err)
			}
			for _, message := range extracted.Messages {
				if message.MessageID == "group-image" && (message.MediaPath != "" || message.SourceTextNull != (kind == "null-text")) {
					t.Fatalf("unsafe path or altered source null state: %+v", message)
				}
			}
			if _, err := Import(ctx, st, source); err != nil {
				t.Fatalf("optional attachment aborted metadata import: %v", err)
			}
			after, err := st.ExportAll(ctx)
			if err != nil || len(after.Messages) != len(before.Messages) || after.AccountIdentity != before.AccountIdentity || after.SourceStoreIdentity != before.SourceStoreIdentity {
				t.Fatalf("metadata or binding changed: %+v, %v", after, err)
			}
			for i, old := range before.Messages {
				got := after.Messages[i]
				old.LastSeenAt = got.LastSeenAt
				if old.MessageID == "group-image" {
					old.MediaPath = ""
				}
				if !reflect.DeepEqual(got, old) {
					t.Fatalf("message %d changed beyond rejected path/observation time:\n%+v\n%+v", i, old, got)
				}
			}
			if len(after.Revisions) != len(before.Revisions)+1 {
				t.Fatalf("revision count: %+v", after.Revisions)
			}
			revision := after.Revisions[len(after.Revisions)-1]
			if revision.Reason != "whatsapp_edit" || revision.EventSource != "whatsapp-desktop" {
				t.Fatalf("fabricated deletion: %+v", revision)
			}
			encoded, err := json.Marshal(after)
			if err != nil || bytes.Contains(encoded, []byte(outside)) || bytes.Contains(encoded, []byte("SourceMediaPathRejected")) {
				t.Fatalf("unsafe path or transient evidence persisted: %s, %v", encoded, err)
			}
			data, err := os.ReadFile(outside) // #nosec G304 -- unchanged bytes of the sentinel this test created in a separate temp directory.
			if err != nil || string(data) != "preserved outside bytes" {
				t.Fatal("outside sentinel changed")
			}
		})
	}
}

func TestAuditInvalidMediaCopyPreflightsWholeBatch(t *testing.T) {
	ctx := context.Background()
	st, source, _ := auditMediaFixture(t)
	if _, err := Import(ctx, st, source); err != nil {
		t.Fatal(err)
	}
	before, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mustExec(t, db, `insert into ZWAMESSAGE values (5,1,null,2,'invalid-last',0,700000004,'keep metadata',1,0,'111@s.whatsapp.net','','');
insert into ZWAMEDIAITEM values (2,5,'Media/../../outside','','', '',0);`)
	root := filepath.Join(filepath.Dir(st.Path()), "media")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("prior media"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalOpen := openMediaFileForCopy
	t.Cleanup(func() { openMediaFileForCopy = originalOpen })
	opened := 0
	openMediaFileForCopy = func(root, name string) (*os.File, error) {
		opened++
		return originalOpen(root, name)
	}
	stats, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true})
	if err == nil || !strings.Contains(err.Error(), "media") || stats.MediaCopied != 0 || opened != 0 {
		t.Fatalf("batch preflight: copied=%d opened=%d error=%v", stats.MediaCopied, opened, err)
	}
	after, err := st.ExportAll(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("archive/revisions/bindings changed on rejected batch: %v", err)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 || files[0].Name() != "sentinel" {
		t.Fatalf("media changed: %v, %v", files, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sentinel")) // #nosec G304 -- fixed sentinel under this test's own archive media root.
	if err != nil || string(data) != "prior media" {
		t.Fatal("prior media bytes changed")
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".wacrawl-media-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("batch created stages: %v, %v", stages, err)
	}
}

func auditMediaObjectPath(root string, content []byte) string {
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	return filepath.Join(root, digest[:2], digest)
}

func TestAuditMediaOwnershipIncludesAncestorAliases(t *testing.T) {
	source := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	if !containsMediaPath(source, filepath.Join(alias, "not-created")) {
		t.Fatal("missed the filesystem identity of an existing ancestor")
	}
	if containsMediaPath(source, filepath.Join(t.TempDir(), "not-created")) {
		t.Fatal("classified an unrelated output as a source alias")
	}
}

func TestAuditMediaRejectsCaseEquivalentSourceRoot(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "Source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "source")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("requires a case-insensitive filesystem")
	} else if err != nil {
		t.Fatal(err)
	}
	createFixtureDBs(t, source)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	before, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{alias, filepath.Join(alias, "new-media")} {
		if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true, MediaRoot: output}); err == nil {
			t.Fatal("accepted a media root inside the case-equivalent source")
		}
	}
	after, err := os.ReadDir(source)
	if err != nil || len(after) != len(before) {
		t.Fatalf("source entries changed: %v", err)
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatal("source entries changed")
		}
	}
}

func auditMediaFixture(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	source := t.TempDir()
	createFixtureDBs(t, source)
	path := filepath.Join(source, "Message", "Media", "123@g.us", "a", "test.jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, source, path
}

func TestAuditMediaRefreshAndBackupRestoreRetainLegacyReference(t *testing.T) {
	ctx := context.Background()
	st, source, cache := auditMediaFixture(t)
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(st.Path()), "media")
	object := auditMediaObjectPath(root, []byte("image"))
	legacy := filepath.Join(root, "Message", "Media", "old.jpg")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(object, legacy); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET media_path=? WHERE msg_id='group-image'`, legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index, copyMedia := range []bool{false, true, true} {
		if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: copyMedia}); err != nil {
			t.Fatal(err)
		}
		after, err := st.ExportAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Revisions) != len(before.Revisions) {
			t.Fatal("media refresh manufactured a revision")
		}
		for _, message := range after.Messages {
			if message.MessageID == "group-image" && message.MediaPath != legacy {
				t.Fatalf("legacy reference replaced: %q", message.MediaPath)
			}
		}
		if index == 1 {
			_ = os.Remove(cache)
		}
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	if output, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil { // #nosec G204 -- fixed Git command creates this test's absolute temp bare-repository path.
		t.Fatalf("init local remote: %s %v", output, err)
	}
	opts := backup.Options{
		Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
		Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	if _, _, err := backup.Init(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Push(ctx, st, opts); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if _, err := backup.Pull(ctx, restored, opts); err != nil {
		t.Fatal(err)
	}
	got, err := restored.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restoredObject := auditMediaObjectPath(filepath.Join(filepath.Dir(restored.Path()), "media"), []byte("image"))
	if data, err := os.ReadFile(restoredObject); err != nil || string(data) != "image" { // #nosec G304 -- expected content-addressed path beneath this test's temp restored archive.
		t.Fatal("new content-addressed logical path did not restore")
	}
	for _, message := range got.Messages {
		if message.MessageID == "group-image" {
			data, err := os.ReadFile(message.MediaPath)
			if err != nil || string(data) != "image" {
				t.Fatalf("restored media: %q %v", data, err)
			}
		}
	}
}

func TestAuditMediaAttachmentAndTombstoneOrdering(t *testing.T) {
	ctx := context.Background()
	st, source, cache := auditMediaFixture(t)
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err != nil {
		t.Fatal(err)
	}
	oldPath := auditMediaObjectPath(filepath.Join(filepath.Dir(st.Path()), "media"), []byte("image"))
	db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mustExec(t, db, `UPDATE ZWAMEDIAITEM SET ZMEDIAURL='https://example.invalid/changed.enc' WHERE Z_PK=1`)
	if _, err := Import(ctx, st, source); err != nil {
		t.Fatal(err)
	}
	current, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range current.Messages {
		if message.MessageID == "group-image" && message.MediaPath != cache {
			t.Fatal("changed attachment inherited prior bytes")
		}
	}
	mustExec(t, db, `UPDATE ZWAMEDIAITEM SET ZMEDIAURL='https://example.invalid/media.enc' WHERE Z_PK=1`)
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE ZWAMESSAGE SET ZTEXT=NULL, ZMEDIAITEM=NULL, ZMESSAGETYPE=0 WHERE Z_PK=3`)
	mustExec(t, db, `UPDATE ZWAMEDIAITEM SET ZMESSAGE=NULL WHERE Z_PK=1`)
	if _, err := Import(ctx, st, source); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range deleted.Messages {
		if message.MessageID == "group-image" && (message.DeletedAt.IsZero() || message.MediaPath != "") {
			t.Fatalf("retention suppressed the original cleared-payload tombstone: %+v", message)
		}
	}
	mustExec(t, db, `UPDATE ZWAMESSAGE SET ZTEXT='launch now', ZMEDIAITEM=1, ZMESSAGETYPE=1 WHERE Z_PK=3`)
	mustExec(t, db, `UPDATE ZWAMEDIAITEM SET ZMESSAGE=3 WHERE Z_PK=1`)
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err != nil {
		t.Fatal(err)
	}
	again, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted.Messages) != len(again.Messages) {
		t.Fatal("copy changed the message set")
	}
	targets := 0
	for index, before := range deleted.Messages {
		after := again.Messages[index]
		if before.MessageID == "group-image" {
			targets++
			if before.DeletedAt.IsZero() || before.MediaPath != "" {
				t.Fatal("expected the target message to remain tombstoned without media")
			}
		}
		// Live rows record each observation; deleted rows must stay unchanged.
		if before.DeletedAt.IsZero() {
			after.LastSeenAt = before.LastSeenAt
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("copy changed canonical message or tombstone: event=%s before=%+v after=%+v", before.EventID, before, after)
		}
	}
	if targets != 1 {
		t.Fatalf("expected one target tombstone, got %d", targets)
	}
	if !reflect.DeepEqual(deleted.Revisions, again.Revisions) {
		t.Fatal("copy changed revision history")
	}
	if data, err := os.ReadFile(oldPath); err != nil || string(data) != "image" { // #nosec G304 -- expected object in this test's temp archive checks historical media bytes.
		t.Fatal("tombstone removed historical media")
	}
}

func TestAuditMediaPublicationFailuresNeverReplacePriorObject(t *testing.T) {
	for _, failure := range []string{"open", "read", "write", "sync", "close", "conflicting-object", "source-hardlink"} {
		t.Run(failure, func(t *testing.T) {
			source := t.TempDir()
			src := filepath.Join(source, "media.jpg")
			root := filepath.Join(t.TempDir(), "media")
			if err := os.WriteFile(src, []byte("image"), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := auditMediaObjectPath(root, []byte("image"))
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				t.Fatal(err)
			}
			prior := []byte("image")
			if failure == "conflicting-object" {
				prior = []byte("preserve corrupt prior object")
			}
			if failure == "source-hardlink" {
				if err := os.Link(src, dest); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(dest, prior, 0o600); err != nil {
				t.Fatal(err)
			}
			open, copyFn, syncFn, closeFn := openMediaFileForCopy, copyMediaBytes, syncMediaCopy, closeMediaCopy
			defer func() {
				openMediaFileForCopy, copyMediaBytes, syncMediaCopy, closeMediaCopy = open, copyFn, syncFn, closeFn
			}()
			boom := errors.New("synthetic copy failure")
			switch failure {
			case "open":
				openMediaFileForCopy = func(string, string) (*os.File, error) { return nil, boom }
			case "read":
				copyMediaBytes = func(io.Writer, io.Reader) (int64, error) { return 0, boom }
			case "write":
				copyMediaBytes = func(w io.Writer, _ io.Reader) (int64, error) {
					_, _ = io.WriteString(w, "partial")
					return 7, boom
				}
			case "sync":
				syncMediaCopy = func(*os.File) error { return boom }
			case "close":
				closeMediaCopy = func(f *os.File) error { _ = f.Close(); return boom }
			}
			if _, err := copyMediaFile(source, src, root); err == nil {
				t.Fatal("expected copy failure")
			}
			got, err := os.ReadFile(dest) // #nosec G304 -- destination object or hardlink created in this test's temp root before injected copy failure.
			if err != nil || !bytes.Equal(got, prior) {
				t.Fatalf("prior destination changed: %q %v", got, err)
			}
			if got, err := os.ReadFile(src); err != nil || string(got) != "image" { // #nosec G304 -- fixed temp media.jpg written by this test; checks source preservation.
				t.Fatal("source changed")
			}
		})
	}
}

func TestAuditMediaStageExcludedAndFailuresPreserveArchive(t *testing.T) {
	ctx := context.Background()
	st, source, cache := auditMediaFixture(t)
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err != nil {
		t.Fatal(err)
	}
	before, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(st.Path()), "media")
	oldObject := auditMediaObjectPath(root, []byte("image"))
	if err := os.WriteFile(cache, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCopy := copyMediaBytes
	t.Cleanup(func() { copyMediaBytes = oldCopy })
	copyMediaBytes = func(out io.Writer, in io.Reader) (int64, error) {
		if _, err := io.WriteString(out, "partial"); err != nil {
			t.Fatal(err)
		}
		files, err := ckbackup.CollectFiles(ctx, root, "media")
		if err != nil || len(files) != 1 {
			t.Fatalf("backup collector saw temporary media: %+v %v", files, err)
		}
		return 7, errors.New("synthetic read failure")
	}
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err == nil {
		t.Fatal("copy failure was treated as a cache miss")
	}
	copyMediaBytes = oldCopy
	after, err := st.ExportAll(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("copy failure changed archive")
	}
	db, err := sql.Open("sqlite", st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER audit_fail BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic import failure'); END`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := ImportWithOptions(ctx, st, ImportOptions{SourcePath: source, CopyMedia: true}); err == nil {
		t.Fatal("expected database failure after media publication")
	}
	after, err = st.ExportAll(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("database failure changed archive references")
	}
	data, err := os.ReadFile(oldObject) // #nosec G304 -- expected prior object in the fixture's temp archive after copy and database failures.
	if err != nil || !bytes.Equal(data, []byte("image")) {
		t.Fatal("previous media object changed")
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".wacrawl-media-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned stages remain: %v %v", entries, err)
	}
}

func TestAuditMediaRejectsSourceOverlapAndEscapes(t *testing.T) {
	st, source, cache := auditMediaFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(source, "Message"), alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{source, filepath.Join(source, "Media"), alias + "/../media"} {
		if _, err := ImportWithOptions(context.Background(), st, ImportOptions{SourcePath: source, CopyMedia: true, MediaRoot: root}); err == nil {
			t.Fatalf("accepted source/output overlap: %q", root)
		}
	}
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cache); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportWithOptions(context.Background(), st, ImportOptions{SourcePath: source, CopyMedia: true}); err == nil {
		t.Fatal("accepted a source media symlink")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside sentinel" { // #nosec G304 -- separate t.TempDir sentinel created by this test before rejecting a symlink to it.
		t.Fatal("outside file changed")
	}
}
