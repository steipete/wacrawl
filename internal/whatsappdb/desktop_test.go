package whatsappdb

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacrawl/internal/store"
	_ "modernc.org/sqlite"
)

func TestImportDesktopCoreDataShape(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)

	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	stats, err := Import(ctx, archive, source)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Chats != 2 || stats.Contacts != 2 || stats.Groups != 1 || stats.Participants != 1 || stats.Messages != 4 || stats.MediaMessages != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	status, err := archive.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 4 || status.MediaMessages != 1 || status.UnreadChats != 1 || status.UnreadMessages != 2 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if stats.SourceIdentity == "" || stats.SourceStoreIdentity == "" || stats.AccountIdentity == "" || stats.SourceSnapshotAt.IsZero() || stats.SourceSnapshotAt.After(stats.FinishedAt) || status.LastSourceSnapshot.IsZero() || status.LastSourceNewest.IsZero() || !status.LastSourceNewest.Equal(time.Unix(appleEpoch+700000003, 0).UTC()) {
		t.Fatalf("missing source identity or watermark: stats=%+v status=%+v", stats, status)
	}

	results, err := archive.Search(ctx, store.MessageFilter{Query: "launch", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}
	if results[0].SenderJID != "222@lid" || results[0].SenderName != "Alice" {
		t.Fatalf("group sender not resolved from member row: %+v", results[0])
	}
	if results[0].ChatJID != "123@g.us" || results[0].MediaType != "image" {
		t.Fatalf("group/media fields wrong: %+v", results[0])
	}

	dms, err := archive.Messages(ctx, store.MessageFilter{ChatJID: "111@s.whatsapp.net", Limit: 10, Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dms) != 3 {
		t.Fatalf("expected 3 dm messages, got %d", len(dms))
	}
	if dms[0].SenderJID != "111@s.whatsapp.net" || dms[0].SenderName != "Bob" {
		t.Fatalf("incoming dm sender wrong: %+v", dms[0])
	}
	if !dms[1].FromMe || dms[1].SenderName != "me" {
		t.Fatalf("outgoing dm sender wrong: %+v", dms[1])
	}
}

func TestImportDesktopPreservesReusedRowReactionAcrossReimport(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatal(err)
	}

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
insert into ZWAMEDIAITEM values (2, 1, '', '', 'dm-in', '', 0);
update ZWAMESSAGE set ZMEDIAITEM=2, ZSTANZAID='reaction-stanza', ZISFROMME=1, ZMESSAGEDATE=700000300, ZTEXT=null, ZMESSAGETYPE=14, ZFROMJID='', ZTOJID='111@s.whatsapp.net', ZPUSHNAME='' where Z_PK=1;`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	var reactionPK int64
	var reactionEventID string
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := Import(ctx, archive, source); err != nil {
			t.Fatalf("reaction import attempt %d: %v", attempt, err)
		}
		messages, err := archive.Messages(ctx, store.MessageFilter{ChatJID: "111@s.whatsapp.net", Limit: 10, Asc: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 4 {
			t.Fatalf("attempt %d direct messages = %d, want three originals plus reaction: %+v", attempt, len(messages), messages)
		}
		var original, reaction *store.Message
		for i := range messages {
			switch messages[i].MessageID {
			case "dm-in":
				if messages[i].SourcePK == 1 {
					original = &messages[i]
				}
			case "reaction-stanza":
				reaction = &messages[i]
			}
		}
		if original == nil || reaction == nil {
			t.Fatalf("attempt %d missing original or reaction: %+v", attempt, messages)
		}
		if original.Text != "hello" || original.EventID != "wa:1" || original.SourceRowPK != 1 {
			t.Fatalf("attempt %d original changed: %+v", attempt, *original)
		}
		if reaction.RawType != 14 || reaction.SourceRowPK != 1 || reaction.MediaTitle != "dm-in" || !reaction.FromMe || !reaction.Timestamp.Equal(time.Unix(appleEpoch+700000300, 0).UTC()) {
			t.Fatalf("attempt %d reaction fields/provenance = %+v", attempt, *reaction)
		}
		if attempt == 1 {
			reactionPK = reaction.SourcePK
			reactionEventID = reaction.EventID
		} else if reaction.SourcePK != reactionPK || reaction.EventID != reactionEventID {
			t.Fatalf("reaction identity changed across import: first=(%d,%q) second=(%d,%q)", reactionPK, reactionEventID, reaction.SourcePK, reaction.EventID)
		}
	}

	status, err := archive.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 5 || status.MessageRevisions != 0 {
		t.Fatalf("reused reaction status = %+v", status)
	}
}

func TestImportDesktopPreservesReusedRowRevokeAcrossReimport(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatal(err)
	}

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
update ZWAMEDIAITEM set ZTITLE='group-image', ZMEDIALOCALPATH='', ZMEDIAURL='', ZFILESIZE=0 where Z_PK=1;
update ZWAMESSAGE set ZSTANZAID='revoke-stanza', ZTEXT='186281455824905@lid', ZMESSAGETYPE=14 where Z_PK=3;`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	var revokePK int64
	var revokeEventID string
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := Import(ctx, archive, source); err != nil {
			t.Fatalf("revoke import attempt %d: %v", attempt, err)
		}
		messages, err := archive.Messages(ctx, store.MessageFilter{ChatJID: "123@g.us", Limit: 10, Asc: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 2 {
			t.Fatalf("attempt %d group messages = %d, want original and revoke: %+v", attempt, len(messages), messages)
		}
		var original, revoke *store.Message
		for i := range messages {
			switch messages[i].MessageID {
			case "group-image":
				original = &messages[i]
			case "revoke-stanza":
				revoke = &messages[i]
			}
		}
		if original == nil || revoke == nil {
			t.Fatalf("attempt %d missing original or revoke: %+v", attempt, messages)
		}
		if original.SourcePK != 3 || original.SourceRowPK != 3 || original.EventID != "wa:3" || original.Text != "launch now" || original.MediaTitle != "launch image" {
			t.Fatalf("attempt %d original changed: %+v", attempt, *original)
		}
		if revoke.SourcePK == 3 || revoke.SourceRowPK != 3 || revoke.EventID == original.EventID || revoke.RawType != 14 || revoke.Text != "186281455824905@lid" || revoke.MediaTitle != original.MessageID || !revoke.Timestamp.Equal(original.Timestamp) || revoke.FromMe != original.FromMe {
			t.Fatalf("attempt %d revoke fields/provenance = %+v", attempt, *revoke)
		}
		if attempt == 1 {
			revokePK = revoke.SourcePK
			revokeEventID = revoke.EventID
		} else if revoke.SourcePK != revokePK || revoke.EventID != revokeEventID {
			t.Fatalf("revoke identity changed across import: first=(%d,%q) second=(%d,%q)", revokePK, revokeEventID, revoke.SourcePK, revoke.EventID)
		}
	}

	status, err := archive.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 5 || status.MessageRevisions != 0 {
		t.Fatalf("reused revoke status = %+v", status)
	}
}

func TestImportDesktopTurnsNullTextTransitionIntoTombstone(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatal(err)
	}

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chatDB.ExecContext(ctx, `update ZWAMESSAGE set ZTEXT=null where Z_PK=1`); err != nil {
		_ = chatDB.Close()
		t.Fatal(err)
	}
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatal(err)
	}
	status, err := archive.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 3 || status.DeletedMessages != 1 || status.MessageRevisions != 1 {
		t.Fatalf("null-text import status = %+v", status)
	}
	var reason, revision string
	if err := archive.DB().QueryRowContext(ctx, `select deletion_reason from messages where source_pk=1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if err := archive.DB().QueryRowContext(ctx, `select payload_json from message_revisions where event_id='wa:1'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if reason != "whatsapp_payload_cleared" || !strings.Contains(revision, `"text":"hello"`) {
		t.Fatalf("reason=%q revision=%s", reason, revision)
	}
}

func TestImportDesktopRejectsAccountSwitchAtSameStore(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatal(err)
	}
	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
delete from ZWAMESSAGE;
insert into ZWAMEDIAITEM values (2, 10, 'foreign.bin', '', 'foreign', '', 7);
insert into ZWAMESSAGE values (10, 1, null, 2, 'account-b', 0, 700000010, 'other account', 1, 0, '111@s.whatsapp.net', '', 'Other');`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}
	axolotlDB, err := sql.Open("sqlite", filepath.Join(source, axolotlDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, axolotlDB, `update ZWAZMDACCOUNT set ZACCOUNTJIDSTRING='other-owner@s.whatsapp.net', ZUSERJIDSTRING='other-owner@s.whatsapp.net'`)
	if err := axolotlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "foreign.bin"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, archive, source); err == nil || !strings.Contains(err.Error(), "different WhatsApp account") {
		t.Fatalf("same-path account switch error = %v", err)
	}
	mediaRoot := filepath.Join(t.TempDir(), "media")
	if _, err := ImportWithOptions(ctx, archive, ImportOptions{SourcePath: source, CopyMedia: true, MediaRoot: mediaRoot}); err == nil || !strings.Contains(err.Error(), "different WhatsApp account") {
		t.Fatalf("copy-media account switch error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(mediaRoot, "foreign.bin")); !os.IsNotExist(err) {
		t.Fatalf("rejected import wrote foreign media: %v", err)
	}
	if _, err := ImportWithOptions(ctx, archive, ImportOptions{SourcePath: source, Restore: true}); err != nil {
		t.Fatalf("explicit restore should switch accounts: %v", err)
	}
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatalf("merge after restore should have event continuity: %v", err)
	}
}

func TestReadSourceIdentity(t *testing.T) {
	ctx := context.Background()
	t.Run("message churn does not rotate", func(t *testing.T) {
		source := t.TempDir()
		createFixtureDBs(t, source)
		before, err := readSourceIdentity(ctx, filepath.Join(source, chatDBName))
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `update ZWAMESSAGE set ZFROMJID='owner@s.whatsapp.net' where ZISFROMME=1`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, err := readSourceIdentity(ctx, filepath.Join(source, chatDBName))
		if err != nil || identity != before || !strings.HasPrefix(identity, "wa-store:") {
			t.Fatalf("identity rotated: before=%q after=%q err=%v", before, identity, err)
		}
	})
	t.Run("missing marker", func(t *testing.T) {
		source := t.TempDir()
		createFixtureDBs(t, source)
		db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `drop table Z_METADATA`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, err := readSourceIdentity(ctx, filepath.Join(source, chatDBName))
		if err != nil || identity != "" {
			t.Fatalf("missing identity = %q, %v", identity, err)
		}
	})
	t.Run("empty store uuid", func(t *testing.T) {
		source := t.TempDir()
		createFixtureDBs(t, source)
		db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `update Z_METADATA set Z_UUID=''`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, err := readSourceIdentity(ctx, filepath.Join(source, chatDBName))
		if err != nil || identity != "" {
			t.Fatalf("empty identity = %q, %v", identity, err)
		}
	})
	t.Run("missing metadata row", func(t *testing.T) {
		source := t.TempDir()
		createFixtureDBs(t, source)
		db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `delete from Z_METADATA`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, err := readSourceIdentity(ctx, filepath.Join(source, chatDBName))
		if err != nil || identity != "" {
			t.Fatalf("missing metadata identity = %q, %v", identity, err)
		}
	})
}

func TestReadAccountIdentity(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	path := filepath.Join(source, axolotlDBName)
	chatPath := filepath.Join(source, chatDBName)
	before, _, err := readAccountIdentity(ctx, path, chatPath)
	if err != nil || !strings.HasPrefix(before, "wa-account:") {
		t.Fatalf("account identity = %q, %v", before, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `update ZWAZMDACCOUNT set ZUSERJIDSTRING='other-owner@s.whatsapp.net'`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after, _, err := readAccountIdentity(ctx, path, chatPath)
	if err != nil || after == before || !strings.HasPrefix(after, "wa-account:") {
		t.Fatalf("account switch identity: before=%q after=%q err=%v", before, after, err)
	}
	if identity, _, err := readAccountIdentity(ctx, filepath.Join(t.TempDir(), axolotlDBName), filepath.Join(t.TempDir(), chatDBName)); err != nil || identity != "" {
		t.Fatalf("missing account database identity = %q, %v", identity, err)
	}

	ambiguous := t.TempDir()
	createFixtureDBs(t, ambiguous)
	db, err = sql.Open("sqlite", filepath.Join(ambiguous, axolotlDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `insert into ZWAZMDACCOUNT values (2, 'second@s.whatsapp.net', 'second@s.whatsapp.net')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAccountIdentity(ctx, filepath.Join(ambiguous, axolotlDBName), filepath.Join(ambiguous, chatDBName)); err == nil || !strings.Contains(err.Error(), "multiple WhatsApp account identities") || strings.Contains(err.Error(), "--restore") {
		t.Fatalf("ambiguous account identity error = %v", err)
	}
}

func TestReadAccountIdentityFallbacks(t *testing.T) {
	ctx := context.Background()
	t.Run("ZMD account JID", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), axolotlDBName)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `create table ZWAZMDACCOUNT (ZACCOUNTJIDSTRING varchar, ZUSERJIDSTRING varchar); insert into ZWAZMDACCOUNT values ('owner@s.whatsapp.net', '')`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, _, err := readAccountIdentity(ctx, path, filepath.Join(t.TempDir(), chatDBName))
		if err != nil || !strings.HasPrefix(identity, "wa-account:") {
			t.Fatalf("ZMD account fallback = %q, %v", identity, err)
		}
	})
	t.Run("incoming message recipient", func(t *testing.T) {
		dir := t.TempDir()
		axolotlPath := filepath.Join(dir, axolotlDBName)
		db, err := sql.Open("sqlite", axolotlPath)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `
create table ZWAZMDACCOUNT (ZACCOUNTJIDSTRING varchar, ZUSERJIDSTRING varchar);
create table ZWAAXOLOTLIDENTITY (ZACCOUNTJIDSTRING varchar);
create table ZWAAXOLOTLSESSION (ZACCOUNTJIDSTRING varchar);
create table ZWASENDERKEY (ZACCOUNTJIDSTRING varchar);
insert into ZWAAXOLOTLIDENTITY values ('remote-a@s.whatsapp.net');
insert into ZWAAXOLOTLSESSION values ('remote-b@s.whatsapp.net');
insert into ZWASENDERKEY values ('remote-c@s.whatsapp.net');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		chatPath := filepath.Join(dir, chatDBName)
		db, err = sql.Open("sqlite", chatPath)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `
create table ZWAMESSAGE (ZISFROMME integer, ZTOJID varchar);
insert into ZWAMESSAGE values (0, 'OWNER@S.WHATSAPP.NET');
insert into ZWAMESSAGE values (0, 'owner@s.whatsapp.net');
insert into ZWAMESSAGE values (1, 'remote@s.whatsapp.net');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, legacy, err := readAccountIdentity(ctx, axolotlPath, chatPath)
		if err != nil || !strings.HasPrefix(identity, "wa-account:") {
			t.Fatalf("message recipient fallback = %q, %v", identity, err)
		}
		if len(legacy) != 3 {
			t.Fatalf("legacy account candidates = %d, want 3", len(legacy))
		}
	})
	t.Run("message recipients are ambiguous", func(t *testing.T) {
		chatPath := filepath.Join(t.TempDir(), chatDBName)
		db, err := sql.Open("sqlite", chatPath)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `
create table ZWAMESSAGE (ZISFROMME integer, ZTOJID varchar);
insert into ZWAMESSAGE values (0, 'owner-a@s.whatsapp.net');
insert into ZWAMESSAGE values (0, 'owner-b@s.whatsapp.net');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readAccountIdentity(ctx, filepath.Join(t.TempDir(), axolotlDBName), chatPath); err == nil || !strings.Contains(err.Error(), "multiple WhatsApp account identities") {
			t.Fatalf("ambiguous message recipients error = %v", err)
		}
	})
	t.Run("metadata and messages disagree", func(t *testing.T) {
		dir := t.TempDir()
		axolotlPath := filepath.Join(dir, axolotlDBName)
		db, err := sql.Open("sqlite", axolotlPath)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `create table ZWAZMDACCOUNT (ZUSERJIDSTRING varchar); insert into ZWAZMDACCOUNT values ('owner-a@s.whatsapp.net')`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		chatPath := filepath.Join(dir, chatDBName)
		db, err = sql.Open("sqlite", chatPath)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `create table ZWAMESSAGE (ZISFROMME integer, ZTOJID varchar); insert into ZWAMESSAGE values (0, 'owner-b@s.whatsapp.net')`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readAccountIdentity(ctx, axolotlPath, chatPath); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("account metadata mismatch error = %v", err)
		}
	})
	t.Run("empty schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), axolotlDBName)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `create table unrelated (value text)`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		identity, _, err := readAccountIdentity(ctx, path, filepath.Join(t.TempDir(), chatDBName))
		if err != nil || identity != "" {
			t.Fatalf("empty schema identity = %q, %v", identity, err)
		}
	})
}

func TestExtractRejectsAccountChangeDuringSnapshot(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	snap, err := SnapshotPath(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(snap.Root) }()
	db, err := sql.Open("sqlite", filepath.Join(snap.Root, axolotlDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `update ZWAZMDACCOUNT set ZUSERJIDSTRING='other@s.whatsapp.net'`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(ctx, snap); err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("snapshot account change error = %v", err)
	}
}

func TestImportDesktopUpgradesPathBindingToStoreFingerprint(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `drop table Z_METADATA`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	stats, err := Import(ctx, archive, source)
	if err != nil || stats.SourceStoreIdentity != "" {
		t.Fatalf("path-bound import = %+v, %v", stats, err)
	}
	db, err = sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `create table Z_METADATA (Z_VERSION integer primary key, Z_UUID varchar(255), Z_PLIST blob); insert into Z_METADATA values (1, 'late-store-marker', null)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err = Import(ctx, archive, source)
	if err != nil || stats.SourceStoreIdentity == "" {
		t.Fatalf("store fingerprint upgrade = %+v, %v", stats, err)
	}
	var binding string
	if err := archive.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_source_store_identity'`).Scan(&binding); err != nil || binding != stats.SourceStoreIdentity {
		t.Fatalf("store fingerprint binding = %q, %v", binding, err)
	}
	db, err = sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `drop table Z_METADATA`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, archive, source); err == nil || !strings.Contains(err.Error(), "different WhatsApp Desktop store") {
		t.Fatalf("missing established store marker error = %v", err)
	}
}

func TestImportDesktopWithoutAccountIdentityCannotMergeNonemptyArchive(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	if err := os.Remove(filepath.Join(source, axolotlDBName)); err != nil {
		t.Fatal(err)
	}
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatalf("initial unbound import: %v", err)
	}
	if _, err := Import(ctx, archive, source); err == nil || !strings.Contains(err.Error(), "--adopt-source") {
		t.Fatalf("unbound merge error = %v", err)
	}
	if _, err := ImportWithOptions(ctx, archive, ImportOptions{SourcePath: source, AdoptSource: true}); err == nil || !strings.Contains(err.Error(), "--adopt-source") {
		t.Fatalf("adoption without account identity error = %v", err)
	}
}

func TestImportDesktopMigratesLegacyAccountBinding(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `update ZWAMESSAGE set ZTOJID='fixture-owner@s.whatsapp.net' where ZISFROMME=0`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}
	axolotlDB, err := sql.Open("sqlite", filepath.Join(source, axolotlDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, axolotlDB, `
delete from ZWAZMDACCOUNT;
create table ZWAAXOLOTLSESSION (ZACCOUNTJIDSTRING varchar);
insert into ZWAAXOLOTLSESSION values ('legacy-peer@s.whatsapp.net');`)
	if err := axolotlDB.Close(); err != nil {
		t.Fatal(err)
	}

	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	stats, err := Import(ctx, archive, source)
	if err != nil || stats.AccountIdentity == "" || len(stats.LegacyAccountIDs) != 1 {
		t.Fatalf("initial import = %+v, %v", stats, err)
	}
	legacyIdentity := stats.LegacyAccountIDs[0]
	if legacyIdentity == stats.AccountIdentity {
		t.Fatal("legacy and recipient account identities unexpectedly match")
	}
	if _, err := archive.DB().ExecContext(ctx, `update sync_state set value=? where key='merge_account_identity'`, legacyIdentity); err != nil {
		t.Fatal(err)
	}
	axolotlDB, err = sql.Open("sqlite", filepath.Join(source, axolotlDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, axolotlDB, `insert into ZWAZMDACCOUNT values (1, 'fixture-owner@s.whatsapp.net', 'fixture-owner@s.whatsapp.net')`)
	if err := axolotlDB.Close(); err != nil {
		t.Fatal(err)
	}

	stats, err = Import(ctx, archive, source)
	if err != nil {
		t.Fatalf("legacy binding upgrade after metadata appears: %v", err)
	}
	if len(stats.LegacyAccountIDs) != 1 || stats.LegacyAccountIDs[0] != legacyIdentity {
		t.Fatalf("metadata transition legacy candidates = %v, want %q", stats.LegacyAccountIDs, legacyIdentity)
	}
	var binding string
	if err := archive.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_account_identity'`).Scan(&binding); err != nil || binding != stats.AccountIdentity {
		t.Fatalf("migrated account binding = %q, want %q: %v", binding, stats.AccountIdentity, err)
	}
	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatalf("post-migration merge: %v", err)
	}
}

func TestImportDesktopDuplicateSourceRows(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
insert into ZWACHATSESSION values (3, '111@s.whatsapp.net', 'Bob New', 700000030, 5, 1, 0, 0, 0);
insert into ZWAMESSAGE values (5, 3, null, null, 'dm-new', 0, 700000030, 'newest message', 0, 0, '111@s.whatsapp.net', '', 'Bob New');
insert into ZWAGROUPINFO values (2, 2, 'owner-new@s.whatsapp.net', 699998000);
insert into ZWAGROUPMEMBER values (2, 2, '222@lid', 'Alice Duplicate', 'Alicia', 0, 0);
`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	stats, err := Import(ctx, archive, source)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Chats != 2 || stats.Groups != 1 || stats.Participants != 1 || stats.Messages != 5 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	chats, err := archive.ListChats(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("expected 2 chats, got %d", len(chats))
	}
	if chats[0].JID != "111@s.whatsapp.net" || chats[0].Name != "Bob New" || chats[0].UnreadCount != 5 || !chats[0].Archived {
		t.Fatalf("duplicate chat rows were not merged correctly: %+v", chats[0])
	}

	exported, err := archive.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Groups) != 1 || exported.Groups[0].OwnerJID != "owner@s.whatsapp.net" {
		t.Fatalf("duplicate group rows were not merged correctly: %+v", exported.Groups)
	}
	if len(exported.Participants) != 1 || !exported.Participants[0].IsAdmin || !exported.Participants[0].IsActive {
		t.Fatalf("duplicate participant rows were not merged correctly: %+v", exported.Participants)
	}
}

func TestImportDesktopReadsMediaLinkedByMessage(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
insert into ZWAMEDIAITEM values (2, 4, 'Media/111@s.whatsapp.net/fallback.pdf', 'https://example.invalid/fallback.enc', 'fallback title', '', 99);
`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	stats, err := Import(ctx, archive, source)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MediaMessages != 2 {
		t.Fatalf("expected media linked by ZMESSAGE to count, got %+v", stats)
	}
	messages, err := archive.Messages(ctx, store.MessageFilter{ChatJID: "111@s.whatsapp.net", Limit: 10, Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	var found store.Message
	for _, msg := range messages {
		if msg.SourcePK == 4 {
			found = msg
			break
		}
	}
	if found.MediaPath != filepath.Join(source, "Message", "Media", "111@s.whatsapp.net", "fallback.pdf") ||
		found.MediaURL != "https://example.invalid/fallback.enc" ||
		found.MediaTitle != "fallback title" ||
		found.MediaSize != 99 {
		t.Fatalf("media linked only through ZMESSAGE was not imported: %+v", found)
	}
}

func TestImportDesktopUsesProfilePushNames(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
create table ZWAPROFILEPUSHNAME (Z_PK integer primary key, ZJID varchar, ZPUSHNAME varchar);
insert into ZWAPROFILEPUSHNAME values (1, '333@s.whatsapp.net', 'Profile Pat');
insert into ZWAGROUPMEMBER values (2, 2, '333@s.whatsapp.net', '', '+EAA=', 0, 1);
insert into ZWAMESSAGE values (5, 2, 2, null, 'profile-name', 0, 700000004, 'profile-backed sender', 0, 0, '123@g.us', '', '+EAA=');
`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "wacrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	if _, err := Import(ctx, archive, source); err != nil {
		t.Fatal(err)
	}

	msgs, err := archive.Messages(ctx, store.MessageFilter{ChatJID: "123@g.us", Limit: 10, Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	var found store.Message
	for _, msg := range msgs {
		if msg.MessageID == "profile-name" {
			found = msg
			break
		}
	}
	if found.SenderJID != "333@s.whatsapp.net" || found.SenderName != "Profile Pat" {
		t.Fatalf("profile push name was not used for sender: %+v", found)
	}

	results, err := archive.Search(ctx, store.MessageFilter{Query: "Profile Pat", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MessageID != "profile-name" {
		t.Fatalf("profile push name was not indexed for search: %+v", results)
	}
}

func TestSenderSkipsResolvedJIDFallback(t *testing.T) {
	jid, name := sender(false, "123@g.us", "444@s.whatsapp.net", "", "Readable Push", "", "", "", map[string]string{
		"444@s.whatsapp.net": "444@s.whatsapp.net",
	})
	if jid != "444@s.whatsapp.net" || name != "Readable Push" {
		t.Fatalf("sender used JID fallback before readable push name: jid=%q name=%q", jid, name)
	}
}

func TestCleanDesktopMediaRel(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"blank", "", ""},
		{"current", ".", ""},
		{"parent", "..", ""},
		{"parent prefix", filepath.Join("..", "..", "Media", "photo.jpg"), "photo.jpg"},
		{"absolute", filepath.Join(string(os.PathSeparator), "Media", "photo.jpg"), filepath.Join("Media", "photo.jpg")},
		{"normal", filepath.Join("Media", "chat", "photo.jpg"), filepath.Join("Media", "chat", "photo.jpg")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanDesktopMediaRel(tc.path); got != tc.want {
				t.Fatalf("cleanDesktopMediaRel(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestImportDesktopCopyMedia(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	mediaPath := filepath.Join(source, "Message", "Media", "123@g.us", "a", "test.jpg")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}

	chatDB, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, chatDB, `
insert into ZWAMEDIAITEM values (2, 5, 'Media/123@g.us/a/missing.jpg', 'https://example.invalid/missing.enc', 'missing image', '', 7);
insert into ZWAMESSAGE values (5, 2, 1, 2, 'missing-media', 0, 700000004, 'missing media', 1, 0, '123@g.us', '', 'Alice');
`)
	if err := chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	archive, err := store.Open(ctx, archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	stats, err := ImportWithOptions(ctx, archive, ImportOptions{SourcePath: source, CopyMedia: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MediaCopied != 1 || stats.MediaMissing != 1 || stats.MediaMessages != 2 {
		t.Fatalf("unexpected media stats: %+v", stats)
	}

	msgs, err := archive.Messages(ctx, store.MessageFilter{ChatJID: "123@g.us", Limit: 10, Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	var copiedPath, missingPath string
	for _, msg := range msgs {
		switch msg.MessageID {
		case "group-image":
			copiedPath = msg.MediaPath
		case "missing-media":
			missingPath = msg.MediaPath
		}
	}
	wantCopied := filepath.Join(filepath.Dir(archivePath), "media", "Message", "Media", "123@g.us", "a", "test.jpg")
	if copiedPath != wantCopied {
		t.Fatalf("copied media path = %q, want %q", copiedPath, wantCopied)
	}
	if data, err := os.ReadFile(copiedPath); err != nil || string(data) != "image" { // #nosec G304 -- copiedPath is asserted against the expected temp archive path above.
		t.Fatalf("copied media content = %q err=%v", data, err)
	}
	wantMissing := filepath.Join(source, "Message", "Media", "123@g.us", "a", "missing.jpg")
	if missingPath != wantMissing {
		t.Fatalf("missing media path = %q, want original %q", missingPath, wantMissing)
	}
}

func TestResolveDesktopMediaPathPrefersMessageMedia(t *testing.T) {
	source := t.TempDir()
	messageMedia := filepath.Join(source, "Message", "Media", "chat", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(messageMedia), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messageMedia, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveDesktopMediaPath(source, "Media/chat/photo.jpg"); got != messageMedia {
		t.Fatalf("resolved media path = %q, want %q", got, messageMedia)
	}

	legacyMedia := filepath.Join(source, "Media", "chat", "legacy.jpg")
	if err := os.MkdirAll(filepath.Dir(legacyMedia), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyMedia, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveDesktopMediaPath(source, "Media/chat/legacy.jpg"); got != legacyMedia {
		t.Fatalf("legacy media path = %q, want %q", got, legacyMedia)
	}

	missing := filepath.Join(source, "Message", "Media", "chat", "missing.jpg")
	if got := resolveDesktopMediaPath(source, "Media/chat/missing.jpg"); got != missing {
		t.Fatalf("missing media path = %q, want %q", got, missing)
	}

	absolute := filepath.Join(string(os.PathSeparator), "tmp", "outside.jpg")
	confined := filepath.Join(source, "tmp", "outside.jpg")
	if got := resolveDesktopMediaPath(source, absolute); got != confined {
		t.Fatalf("absolute media path = %q, want confined %q", got, confined)
	}

	traversal := filepath.Join(source, "outside.jpg")
	if got := resolveDesktopMediaPath(source, "../outside.jpg"); got != traversal {
		t.Fatalf("traversal media path = %q, want confined %q", got, traversal)
	}
}

func TestCopyArchiveMediaDeduplicatesAndConfinesPaths(t *testing.T) {
	source := t.TempDir()
	mediaRoot := filepath.Join(t.TempDir(), "media")
	mediaPath := filepath.Join(source, "Message", "Media", "chat", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(source, "Message", "Media", "chat", "missing.jpg")
	messages := []store.Message{
		{MediaPath: mediaPath},
		{MediaPath: mediaPath},
		{MediaPath: missingPath},
		{MediaPath: missingPath},
	}

	copied, missing, err := copyArchiveMedia(messages, source, mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 1 || missing != 1 {
		t.Fatalf("copy stats = %d/%d, want 1/1", copied, missing)
	}
	wantCopied := filepath.Join(mediaRoot, "Message", "Media", "chat", "photo.jpg")
	if messages[0].MediaPath != wantCopied || messages[1].MediaPath != wantCopied {
		t.Fatalf("duplicate copied media paths not rewritten: %+v", messages[:2])
	}
	if messages[2].MediaPath != missingPath || messages[3].MediaPath != missingPath {
		t.Fatalf("duplicate missing media paths should stay original: %+v", messages[2:])
	}

	outsidePath := filepath.Join(t.TempDir(), "outside.jpg")
	dest, err := archiveMediaPath(source, mediaRoot, outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if dest != filepath.Join(mediaRoot, "outside.jpg") {
		t.Fatalf("outside path fallback = %q", dest)
	}
	if _, err := archiveMediaPath(source, mediaRoot, source); err == nil {
		t.Fatal("expected source root path to be rejected")
	}
}

func TestFileMaterializedRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fileMaterialized(info) {
		t.Fatal("regular local file should be materialized")
	}
}

func TestCopyMediaFileRejectsUnmaterialized(t *testing.T) {
	old := mediaMaterialized
	t.Cleanup(func() { mediaMaterialized = old })
	mediaMaterialized = func(os.FileInfo) bool { return false }

	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	dest := filepath.Join(dir, "out", "photo.jpg")
	if err := os.WriteFile(src, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := copyMediaFile(src, dest)
	if !errors.Is(err, errMediaNotDownloaded) {
		t.Fatalf("unmaterialized copyMediaFile error = %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("unmaterialized source must not be copied: dest stat=%v", statErr)
	}
}

func TestCopyMediaFileCopiesWhenMaterialized(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	dest := filepath.Join(dir, "out", "photo.jpg")
	if err := os.WriteFile(src, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyMediaFile(src, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest) // #nosec G304 -- dest is a path we just created under t.TempDir.
	if err != nil || string(data) != "image" {
		t.Fatalf("copied media = %q err=%v", data, err)
	}
}

func TestCopyMediaFileOpenErrorDoesNotCreateDestination(t *testing.T) {
	old := openMediaFileForCopy
	t.Cleanup(func() { openMediaFileForCopy = old })
	wantErr := errors.New("open failed")
	openMediaFileForCopy = func(string) (*os.File, error) { return nil, wantErr }

	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	dest := filepath.Join(dir, "out", "photo.jpg")
	if err := os.WriteFile(src, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyMediaFile(src, dest); !errors.Is(err, wantErr) {
		t.Fatalf("copyMediaFile error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination should not exist after open failure: %v", err)
	}
}

func TestCopyMediaFileReadErrorRemovesDestination(t *testing.T) {
	old := openMediaFileForCopy
	t.Cleanup(func() { openMediaFileForCopy = old })

	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	dest := filepath.Join(dir, "out", "photo.jpg")
	if err := os.WriteFile(src, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOnly, err := os.OpenFile(filepath.Join(dir, "write-only"), os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- test path is confined under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	openMediaFileForCopy = func(string) (*os.File, error) { return writeOnly, nil }

	if err := copyMediaFile(src, dest); err == nil {
		t.Fatal("copyMediaFile should fail when the source cannot be read")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("partial destination should be removed after read failure: %v", err)
	}
}

func TestCopyArchiveMediaSkipsUnmaterialized(t *testing.T) {
	old := mediaMaterialized
	t.Cleanup(func() { mediaMaterialized = old })
	mediaMaterialized = func(os.FileInfo) bool { return false }

	source := t.TempDir()
	mediaRoot := filepath.Join(t.TempDir(), "media")
	mediaPath := filepath.Join(source, "Message", "Media", "chat", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := []store.Message{{MediaPath: mediaPath}}
	copied, missing, err := copyArchiveMedia(messages, source, mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 0 || missing != 1 {
		t.Fatalf("copy stats = %d/%d, want 0/1", copied, missing)
	}
	if messages[0].MediaPath != mediaPath {
		t.Fatalf("unmaterialized path should stay original: %q", messages[0].MediaPath)
	}
	if _, statErr := os.Stat(filepath.Join(mediaRoot, "Message", "Media", "chat", "photo.jpg")); !os.IsNotExist(statErr) {
		t.Fatal("unmaterialized source must not be copied into the archive")
	}
}

func TestDiscoverAndHelpers(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)

	discovered, err := Discover(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if !discovered.Available || discovered.MessageRows != 4 || discovered.ChatRows != 2 || discovered.ContactRows != 2 || discovered.MediaRows != 1 {
		t.Fatalf("unexpected discovery: %+v", discovered)
	}
	if discovered.OldestMessage == "" || discovered.NewestMessage == "" || len(discovered.SchemaNotes) == 0 {
		t.Fatalf("discovery missing metadata: %+v", discovered)
	}

	missing, err := Discover(ctx, filepath.Join(source, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Available {
		t.Fatalf("missing source should not be available: %+v", missing)
	}

	if runtime.GOOS == "darwin" && DefaultPath() == "" {
		t.Fatal("default path should be set on darwin")
	}
	if defaultedPath(source) != source {
		t.Fatal("explicit path should win")
	}
	if runtime.GOOS == "darwin" && defaultedPath("") == "" {
		t.Fatal("empty path should default")
	}

	if _, err := SnapshotPath(filepath.Join(source, "missing")); err == nil {
		t.Fatal("expected snapshot error for missing source")
	}
	filePath := filepath.Join(source, "file")
	mustExecFile(t, filePath)
	if _, err := Discover(ctx, filePath); err == nil {
		t.Fatal("expected file source error")
	}
	if _, _, err := openReadOnly(filepath.Join(source, "missing.sqlite")); err == nil {
		t.Fatal("expected read-only open error")
	}
	if !appleNullTime(sql.NullFloat64{}).IsZero() {
		t.Fatal("invalid apple null time should be zero")
	}
	want := time.Unix(appleEpoch+42, 0).UTC()
	if got := appleTime(42); !got.Equal(want) {
		t.Fatalf("appleTime = %s, want %s", got, want)
	}
}

func TestExtractWithoutContactsDB(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createFixtureDBs(t, source)
	if err := os.Remove(filepath.Join(source, contactsDBName)); err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotPath(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(snap.Root) }()
	data, err := Extract(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Contacts) != 0 || len(data.Messages) == 0 {
		t.Fatalf("unexpected data without contacts: %+v", data)
	}
}

func TestExtractReportsBrokenChatSchema(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(source, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("create table nope(v integer)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotPath(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(snap.Root) }()
	if _, err := Extract(ctx, snap); err == nil {
		t.Fatal("expected broken schema error")
	}
}

func TestReadProfilePushNamesReportsBrokenOptionalSchema(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mustExec(t, db, `create table ZWAPROFILEPUSHNAME (Z_PK integer primary key);`)
	if _, err := readProfilePushNameRows(ctx, db); err == nil {
		t.Fatal("expected broken profile push name schema error")
	}
}

func TestClassifiers(t *testing.T) {
	chatKinds := map[string]string{
		"123@g.us":           "group",
		"123@newsletter":     "newsletter",
		"123@status":         "status",
		"status@broadcast":   "status",
		"123@s.whatsapp.net": "dm",
	}
	for jid, want := range chatKinds {
		if got := chatKind(jid, 0); got != want {
			t.Fatalf("chatKind(%q) = %q, want %q", jid, got, want)
		}
	}
	if got := chatKind("123@s.whatsapp.net", 3); got != "status" {
		t.Fatalf("raw status chatKind = %q", got)
	}

	messageTypes := map[int]string{
		0: "text", 1: "image", 2: "video", 3: "audio", 4: "location", 5: "contact",
		6: "system", 7: "link", 8: "document", 10: "group_event", 11: "gif",
		14: "reaction", 15: "sticker", 99: "type_99",
	}
	for raw, want := range messageTypes {
		if got := messageType(raw); got != want {
			t.Fatalf("messageType(%d) = %q, want %q", raw, got, want)
		}
	}
	mediaTypes := map[int]string{1: "image", 2: "video", 3: "audio", 7: "link", 8: "document", 11: "gif", 15: "sticker", 99: ""}
	for raw, want := range mediaTypes {
		if got := mediaType(raw); got != want {
			t.Fatalf("mediaType(%d) = %q, want %q", raw, got, want)
		}
	}
}

func TestCanonicalSourcePath(t *testing.T) {
	if path, err := canonicalSourcePath(""); err != nil || path != "" {
		t.Fatalf("empty canonical path = %q, %v", path, err)
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "source-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path, err := canonicalSourcePath(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if path != want {
		t.Fatalf("canonical path = %q, want %q", path, want)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	path, err = canonicalSourcePath(missing)
	if err != nil {
		t.Fatal(err)
	}
	if path != missing {
		t.Fatalf("missing canonical path = %q, want %q", path, missing)
	}
}

func createFixtureDBs(t *testing.T, dir string) {
	t.Helper()
	chat, err := sql.Open("sqlite", filepath.Join(dir, chatDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = chat.Close() }()
	mustExec(t, chat, `
create table ZWACHATSESSION (Z_PK integer primary key, ZCONTACTJID varchar, ZPARTNERNAME varchar, ZLASTMESSAGEDATE timestamp, ZUNREADCOUNT integer, ZARCHIVED integer, ZREMOVED integer, ZHIDDEN integer, ZSESSIONTYPE integer);
create table ZWAGROUPINFO (Z_PK integer primary key, ZCHATSESSION integer, ZOWNERJID varchar, ZCREATIONDATE timestamp);
create table ZWAGROUPMEMBER (Z_PK integer primary key, ZCHATSESSION integer, ZMEMBERJID varchar, ZCONTACTNAME varchar, ZFIRSTNAME varchar, ZISADMIN integer, ZISACTIVE integer);
create table ZWAMEDIAITEM (Z_PK integer primary key, ZMESSAGE integer, ZMEDIALOCALPATH varchar, ZMEDIAURL varchar, ZTITLE varchar, ZVCARDNAME varchar, ZFILESIZE integer);
create table ZWAMESSAGE (Z_PK integer primary key, ZCHATSESSION integer, ZGROUPMEMBER integer, ZMEDIAITEM integer, ZSTANZAID varchar, ZISFROMME integer, ZMESSAGEDATE timestamp, ZTEXT varchar, ZMESSAGETYPE integer, ZSTARRED integer, ZFROMJID varchar, ZTOJID varchar, ZPUSHNAME varchar);
create table Z_METADATA (Z_VERSION integer primary key, Z_UUID varchar(255), Z_PLIST blob);
insert into Z_METADATA values (1, 'fixture-account-a', null);
insert into ZWACHATSESSION values (1, '111@s.whatsapp.net', 'Bob', 700000020, 0, 0, 0, 0, 0);
insert into ZWACHATSESSION values (2, '123@g.us', 'Launch Group', 700000010, 2, 0, 0, 0, 1);
insert into ZWAGROUPINFO values (1, 2, 'owner@s.whatsapp.net', 699999000);
insert into ZWAGROUPMEMBER values (1, 2, '222@lid', 'Alice', 'Alice', 1, 1);
insert into ZWAMEDIAITEM values (1, 3, 'Media/123@g.us/a/test.jpg', 'https://example.invalid/media.enc', 'launch image', '', 42);
insert into ZWAMESSAGE values (1, 1, null, null, 'dm-in', 0, 700000000, 'hello', 0, 0, '111@s.whatsapp.net', '', 'Bob');
insert into ZWAMESSAGE values (2, 1, null, null, 'dm-out', 1, 700000001, 'roger', 0, 0, '', '111@s.whatsapp.net', '');
insert into ZWAMESSAGE values (3, 2, 1, 1, 'group-image', 0, 700000002, 'launch now', 1, 1, '123@g.us', '', 'Alice');
insert into ZWAMESSAGE values (4, 1, null, null, 'dm-in', 0, 700000003, 'duplicate stanza id', 0, 0, '111@s.whatsapp.net', '', 'Bob');
`)

	contacts, err := sql.Open("sqlite", filepath.Join(dir, contactsDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contacts.Close() }()
	mustExec(t, contacts, `
create table ZWAADDRESSBOOKCONTACT (ZWHATSAPPID varchar, ZPHONENUMBER varchar, ZFULLNAME varchar, ZGIVENNAME varchar, ZLASTNAME varchar, ZBUSINESSNAME varchar, ZUSERNAME varchar, ZLID varchar, ZABOUTTEXT varchar, ZLASTUPDATED timestamp);
insert into ZWAADDRESSBOOKCONTACT values ('111@s.whatsapp.net', '+111', 'Bob', 'Bob', '', '', '', '', '', 700000000);
insert into ZWAADDRESSBOOKCONTACT values ('222@s.whatsapp.net', '+222', 'Alice Contact', 'Alice', '', '', '', '222', '', 700000000);
`)
	axolotl, err := sql.Open("sqlite", filepath.Join(dir, axolotlDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = axolotl.Close() }()
	mustExec(t, axolotl, `
create table ZWAZMDACCOUNT (Z_PK integer primary key, ZACCOUNTJIDSTRING varchar, ZUSERJIDSTRING varchar);
insert into ZWAZMDACCOUNT values (1, 'fixture-owner@s.whatsapp.net', 'fixture-owner@s.whatsapp.net');
`)
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func mustExecFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("create table t(v integer)"); err != nil {
		t.Fatal(err)
	}
}
