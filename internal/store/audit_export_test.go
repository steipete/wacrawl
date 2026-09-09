package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/openclaw/wacrawl/internal/store/storedb"
)

type exportBarrier struct {
	storedb.DBTX
	calls  int
	commit func()
}

func TestAuditExportErrorsReturnNoPartialSnapshot(t *testing.T) {
	for _, stage := range []string{"contacts", "chats", "groups", "participants", "messages", "revisions", "source-binding", "revision-scan", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			seed := SnapshotData{
				SourceStoreIdentity: "wa-store:fixture", AccountIdentity: "wa-account:fixture",
				Contacts: []Contact{{JID: "person"}}, Chats: []Chat{{JID: "chat"}},
				Groups: []Group{{JID: "group"}}, Participants: []GroupParticipant{{GroupJID: "group", UserJID: "person"}},
				Messages: []Message{{SourcePK: 1, ChatJID: "chat", MessageID: "message", Text: "preserved"}},
			}
			if err := st.ImportSnapshot(ctx, seed, "synthetic", time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			var query string
			switch stage {
			case "contacts":
				query = "drop table contacts"
			case "chats":
				query = "drop table chats"
			case "groups":
				query = "drop table groups"
			case "participants":
				query = "drop table group_participants"
			case "messages":
				query = "drop table messages"
			case "revisions":
				query = "drop table message_revisions"
			case "source-binding":
				query = "alter table sync_state rename to retained_sync_state"
			case "revision-scan":
				query = "insert into message_revisions(event_id,payload_json,recorded_at,event_source,reason) values('wa:1','{}','not-an-integer','synthetic','edit')"
			}
			if query != "" {
				if _, err := st.db.ExecContext(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			// Read-back before/after proves the failed read transaction changes no surviving state.
			bindingsQuery := `select group_concat(key || '=' || value, '|') from (select key,value from sync_state order by key)`
			if stage == "source-binding" {
				bindingsQuery = `select group_concat(key || '=' || value, '|') from (select key,value from retained_sync_state order by key)`
			}
			var before string
			if err := st.db.QueryRowContext(ctx, bindingsQuery).Scan(&before); err != nil {
				t.Fatal(err)
			}
			exportCtx := ctx
			if stage == "canceled" {
				var cancel context.CancelFunc
				exportCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := st.ExportAll(exportCtx)
			if err == nil || !reflect.DeepEqual(got, SnapshotData{}) {
				t.Fatalf("partial snapshot on %s failure: %+v, %v", stage, got, err)
			}
			var after string
			if err := st.db.QueryRowContext(ctx, bindingsQuery).Scan(&after); err != nil || before != after {
				t.Fatalf("surviving bindings changed: %q -> %q, %v", before, after, err)
			}
			if stage != "messages" {
				var text string
				if err := st.db.QueryRowContext(ctx, `select text from messages where source_pk=1`).Scan(&text); err != nil || text != "preserved" {
					t.Fatalf("surviving message changed: %q, %v", text, err)
				}
			}
			if _, err := st.db.ExecContext(ctx, `create table export_probe(value text)`); err != nil {
				t.Fatalf("connection not usable after rollback: %v", err)
			}
		})
	}
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
