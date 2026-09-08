package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacrawl/internal/store/storedb"
)

type exportBarrier struct {
	storedb.DBTX
	calls  int
	commit func()
}

func (b *exportBarrier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	b.calls++
	if b.calls == 2 {
		b.commit()
	}
	return b.DBTX.QueryContext(ctx, query, args...)
}

func TestAuditExportSnapshotIncludesBindingAndAllRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	reader, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	before := SnapshotData{
		SourceStoreIdentity: "wa-store:before",
		AccountIdentity:     "wa-account:before",
		Contacts:            []Contact{{JID: "person", FullName: "before"}},
		Chats:               []Chat{{JID: "chat", Name: "before"}},
		Groups:              []Group{{JID: "chat", Name: "before"}},
		Participants:        []GroupParticipant{{GroupJID: "chat", UserJID: "person", ContactName: "before"}},
		Messages:            []Message{{SourcePK: 1, ChatJID: "chat", Text: "before", Timestamp: time.Unix(1, 0)}},
	}
	if err := reader.ImportSnapshot(ctx, before, "synthetic", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	tx, err := reader.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	barrier := &exportBarrier{DBTX: tx, commit: func() {
		after := before
		after.SourceStoreIdentity = "wa-store:after"
		after.AccountIdentity = "wa-account:after"
		after.Contacts = []Contact{{JID: "person", FullName: "after"}}
		after.Chats = []Chat{{JID: "chat", Name: "after"}}
		after.Groups = []Group{{JID: "chat", Name: "after"}}
		after.Participants = []GroupParticipant{{GroupJID: "chat", UserJID: "person", ContactName: "after"}}
		after.Messages = []Message{{SourcePK: 1, ChatJID: "chat", Text: "after", Timestamp: time.Unix(1, 0)}}
		if err := writer.ImportSnapshot(ctx, after, "synthetic", time.Unix(3, 0)); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := exportSnapshot(ctx, barrier)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got.SourceStoreIdentity != before.SourceStoreIdentity || got.AccountIdentity != before.AccountIdentity ||
		len(got.Contacts) != 1 || got.Contacts[0].FullName != "before" ||
		len(got.Chats) != 1 || got.Chats[0].Name != "before" ||
		len(got.Groups) != 1 || got.Groups[0].Name != "before" ||
		len(got.Participants) != 1 || got.Participants[0].ContactName != "before" ||
		len(got.Messages) != 1 || got.Messages[0].Text != "before" {
		t.Fatalf("mixed snapshot: %+v", got)
	}
	current, err := reader.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.AccountIdentity != "wa-account:after" || len(current.Messages) != 1 || current.Messages[0].Text != "after" {
		t.Fatalf("writer did not commit during export: %+v", current)
	}
}
