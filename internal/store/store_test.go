package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMergeAllPreservesAbsentRowsAndTracksTombstones(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	chat := Chat{JID: "group@g.us", Kind: "group", Name: "Group", LastMessageAt: now}
	group := Group{JID: chat.JID, Name: chat.Name, OwnerJID: "owner@s.whatsapp.net", CreatedAt: now.Add(-time.Hour)}
	participant := GroupParticipant{GroupJID: chat.JID, UserJID: "alice@s.whatsapp.net", ContactName: "Alice", IsActive: true}
	messages := []Message{
		{SourcePK: 1, ChatJID: chat.JID, ChatName: chat.Name, MessageID: "a", SenderJID: participant.UserJID, Timestamp: now, Text: "first body", RawType: 0, MessageType: "text"},
		{SourcePK: 2, ChatJID: chat.JID, ChatName: chat.Name, MessageID: "b", SenderJID: participant.UserJID, Timestamp: now.Add(time.Second), Text: "destination only", RawType: 0, MessageType: "text"},
	}
	stats := ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", Contacts: 1, Chats: 1, Groups: 1, Participants: 1, Messages: 2, FinishedAt: now}
	if err := st.MergeAll(ctx, stats,
		[]Contact{{JID: participant.UserJID, FullName: "Alice"}}, []Chat{chat}, []Group{group}, []GroupParticipant{participant}, messages); err != nil {
		t.Fatal(err)
	}
	var originalRowID int64
	var eventID string
	if err := st.DB().QueryRowContext(ctx, `select rowid,event_id from messages where source_pk=1`).Scan(&originalRowID, &eventID); err != nil {
		t.Fatal(err)
	}
	if eventID != "wa:1" {
		t.Fatalf("event id = %q", eventID)
	}
	bySource, err := st.MessageBySourcePK(ctx, 1)
	if err != nil || bySource.EventID != eventID {
		t.Fatalf("message by source = %+v, %v", bySource, err)
	}
	if _, err := st.MessageBySourcePK(ctx, 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing message by source error = %v", err)
	}

	// An empty observation is not an authoritative deletion set.
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now.Add(time.Minute)}, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 2 || status.Chats != 1 || status.Contacts != 1 || status.DeletedMessages != 0 {
		t.Fatalf("empty merge changed canonical rows: %+v", status)
	}

	edited := messages[0]
	edited.Text = "edited body"
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", Messages: 1, FinishedAt: now.Add(2 * time.Minute)}, nil, nil, nil, nil, []Message{edited}); err != nil {
		t.Fatal(err)
	}
	var editedRowID int64
	if err := st.DB().QueryRowContext(ctx, `select rowid from messages where source_pk=1`).Scan(&editedRowID); err != nil {
		t.Fatal(err)
	}
	if editedRowID != originalRowID {
		t.Fatalf("message row identity changed: before=%d after=%d", originalRowID, editedRowID)
	}
	status, err = st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.MessageRevisions != 1 {
		t.Fatalf("edit revisions = %d", status.MessageRevisions)
	}
	var revision string
	if err := st.DB().QueryRowContext(ctx, `select payload_json from message_revisions where event_id='wa:1'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(revision, "first body") {
		t.Fatalf("revision did not retain prior payload: %s", revision)
	}
	exported, err := st.ExportAll(ctx)
	if err != nil || len(exported.Revisions) != 1 || exported.Revisions[0].EventID != eventID {
		t.Fatalf("revision export = %+v, %v", exported.Revisions, err)
	}

	chat.Removed = true
	deletedAt := now.Add(3 * time.Minute)
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", Chats: 1, FinishedAt: deletedAt}, nil, []Chat{chat}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	status, err = st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Chats != 0 || status.Groups != 0 || status.Participants != 0 || status.Messages != 0 || status.DeletedChats != 1 || status.DeletedGroups != 1 || status.DeletedParticipants != 1 || status.DeletedMessages != 2 {
		t.Fatalf("subordinate tombstones = %+v", status)
	}
	for table, reason := range map[string]string{
		"chats": "whatsapp_removed", "groups": "parent_chat_deleted", "group_participants": "parent_group_deleted", "messages": "parent_chat_deleted",
	} {
		var missing int
		query := `select count(*) from ` + table + ` where deleted_at is not null and deletion_source='whatsapp-desktop' and deletion_reason=?`
		if err := st.DB().QueryRowContext(ctx, query, reason).Scan(&missing); err != nil {
			t.Fatal(err)
		}
		if missing == 0 {
			t.Fatalf("%s missing tombstone reason %q", table, reason)
		}
	}
	results, err := st.Search(ctx, MessageFilter{Query: "destination", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("tombstoned child remained searchable: %+v", results)
	}

	var firstDeletedAt int64
	if err := st.DB().QueryRowContext(ctx, `select deleted_at from chats where jid=?`, chat.JID).Scan(&firstDeletedAt); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", Chats: 1, FinishedAt: deletedAt.Add(time.Minute)}, nil, []Chat{chat}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var repeatedDeletedAt int64
	if err := st.DB().QueryRowContext(ctx, `select deleted_at from chats where jid=?`, chat.JID).Scan(&repeatedDeletedAt); err != nil {
		t.Fatal(err)
	}
	if repeatedDeletedAt != firstDeletedAt {
		t.Fatalf("repeated tombstone moved timestamp: before=%d after=%d", firstDeletedAt, repeatedDeletedAt)
	}
	var revisionsBefore int
	if err := st.DB().QueryRowContext(ctx, `select count(*) from message_revisions`).Scan(&revisionsBefore); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", Messages: 1, FinishedAt: deletedAt.Add(time.Minute)}, nil, nil, nil, nil, []Message{edited}); err != nil {
		t.Fatal(err)
	}
	var revisionsAfter int
	if err := st.DB().QueryRowContext(ctx, `select count(*) from message_revisions`).Scan(&revisionsAfter); err != nil {
		t.Fatal(err)
	}
	if revisionsAfter != revisionsBefore {
		t.Fatalf("stored parent tombstone created revision: before=%d after=%d", revisionsBefore, revisionsAfter)
	}

	chat.Removed = false
	chat.LastMessageAt = deletedAt.Add(time.Minute)
	participant.IsActive = true
	newMessage := Message{SourcePK: 3, ChatJID: chat.JID, ChatName: chat.Name, MessageID: "c", SenderJID: participant.UserJID, Timestamp: chat.LastMessageAt, Text: "new lifecycle", RawType: 0, MessageType: "text"}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", Chats: 1, Groups: 1, Participants: 1, Messages: 1, FinishedAt: deletedAt.Add(2 * time.Minute)},
		nil, []Chat{chat}, []Group{group}, []GroupParticipant{participant}, []Message{newMessage}); err != nil {
		t.Fatal(err)
	}
	status, err = st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Chats != 1 || status.Groups != 1 || status.Participants != 1 || status.Messages != 1 || status.DeletedMessages != 2 {
		t.Fatalf("authoritative resurrection = %+v", status)
	}
	results, err = st.Search(ctx, MessageFilter{Query: "lifecycle", Limit: 10})
	if err != nil || len(results) != 1 {
		t.Fatalf("resurrected message search = %+v, %v", results, err)
	}
}

func TestIncomingParentTombstoneMarksWholeFamily(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "family.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 16, 0, 0, 0, time.UTC)
	chat := Chat{JID: "group@g.us", Kind: "group", Removed: true}
	group := Group{JID: chat.JID, Name: "Group"}
	participant := GroupParticipant{GroupJID: chat.JID, UserJID: "member", IsActive: true}
	message := Message{Tombstone: Tombstone{DeletedAt: now.Add(-time.Minute), DeletionSource: "operator", DeletionReason: "explicit_child"}, SourcePK: 42, ChatJID: chat.JID, MessageID: "message", Timestamp: now, Text: "body", RawType: 0}
	contact := Contact{Tombstone: Tombstone{DeletedAt: now}, JID: "deleted-contact"}
	if err := st.MergeAll(ctx, ImportStats{FinishedAt: now, Contacts: 1, Chats: 1, Groups: 1, Participants: 1, Messages: 1},
		[]Contact{contact}, []Chat{chat}, []Group{group}, []GroupParticipant{participant}, []Message{message}); err != nil {
		t.Fatal(err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.DeletedContacts != 1 || status.DeletedChats != 1 || status.DeletedGroups != 1 || status.DeletedParticipants != 1 || status.DeletedMessages != 1 {
		t.Fatalf("family tombstones = %+v", status)
	}
	exported, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Contacts) != 1 || len(exported.Chats) != 1 || len(exported.Groups) != 1 || len(exported.Participants) != 1 || len(exported.Messages) != 1 {
		t.Fatalf("tombstone export = %+v", exported)
	}
	if exported.Contacts[0].DeletionSource != "snapshot" || exported.Contacts[0].DeletionReason != "explicit_tombstone" {
		t.Fatalf("default tombstone provenance = %+v", exported.Contacts[0])
	}
	if exported.Groups[0].DeletionReason != "parent_chat_deleted" || exported.Participants[0].DeletionReason != "parent_group_deleted" || exported.Messages[0].DeletionSource != "operator" || exported.Messages[0].DeletionReason != "explicit_child" {
		t.Fatalf("child reasons group=%+v participant=%+v message=%+v", exported.Groups[0], exported.Participants[0], exported.Messages[0])
	}
	deleted, err := st.Messages(ctx, MessageFilter{IncludeDeleted: true, Limit: 10})
	if err != nil || len(deleted) != 1 || deleted[0].EventID != "wa:42" {
		t.Fatalf("include deleted = %+v, %v", deleted, err)
	}
	contacts, err := st.Contacts(ctx)
	if err != nil || len(contacts) != 0 {
		t.Fatalf("live contacts = %+v, %v", contacts, err)
	}
}

func TestMergeAllTombstonesObservableWhatsAppDeletion(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	message := Message{SourcePK: 9, ChatJID: "chat", ChatName: "Chat", MessageID: "stanza", Timestamp: now, Text: "secret body", RawType: 0, MessageType: "text"}
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now, Messages: 1}, nil, []Chat{{JID: "chat", Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	message.Text = ""
	message.SourceTextNull = true
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now.Add(time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 0 || status.DeletedMessages != 1 || status.MessageRevisions != 1 {
		t.Fatalf("observable delete status = %+v", status)
	}
	results, err := st.Search(ctx, MessageFilter{Query: "secret", Limit: 10})
	if err != nil || len(results) != 0 {
		t.Fatalf("normal search exposed tombstone: %+v, %v", results, err)
	}
	results, err = st.Search(ctx, MessageFilter{Query: "secret", IncludeDeleted: true, Limit: 10})
	if err != nil || len(results) != 1 || results[0].EventID != "wa:9" || !strings.Contains(results[0].Snippet, "[secret]") {
		t.Fatalf("include-deleted search = %+v, %v", results, err)
	}
	var reason, payload string
	if err := st.DB().QueryRowContext(ctx, `select deletion_reason from messages where source_pk=9`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `select payload_json from message_revisions where event_id='wa:9'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if reason != "whatsapp_payload_cleared" || !strings.Contains(payload, "secret body") {
		t.Fatalf("delete reason=%q revision=%s", reason, payload)
	}
	firstSeenNull := Message{SourcePK: 10, ChatJID: "chat", MessageID: "empty", Timestamp: now, RawType: 0, SourceTextNull: true}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now.Add(2 * time.Minute), Messages: 2}, nil, nil, nil, nil, []Message{message, firstSeenNull}); err != nil {
		t.Fatal(err)
	}
	status, err = st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 1 || status.DeletedMessages != 1 {
		t.Fatalf("first-seen null payload was inferred deleted: %+v", status)
	}
}

func TestReplaceAllIsExplicitExactRestore(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	chat := Chat{JID: "chat", Kind: "dm"}
	first := Message{SourcePK: 1, ChatJID: chat.JID, MessageID: "one", Timestamp: now, Text: "one", RawType: 0}
	second := Message{SourcePK: 2, ChatJID: chat.JID, MessageID: "two", Timestamp: now, Text: "two", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now, Messages: 2}, nil, []Chat{chat}, nil, nil, []Message{first, second}); err != nil {
		t.Fatal(err)
	}
	first.Text = "edited"
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now.Add(time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{first}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAll(ctx, ImportStats{FinishedAt: now.Add(2 * time.Minute), Messages: 1}, nil, []Chat{chat}, nil, nil, []Message{first}); err != nil {
		t.Fatal(err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 1 || status.MessageRevisions != 0 {
		t.Fatalf("restore status = %+v", status)
	}
	var removed int
	if err := st.DB().QueryRowContext(ctx, `select count(*) from messages where source_pk=2`).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("restore retained destination-only rows: %d", removed)
	}
}

func TestMergeAllPreservesOriginalAndReusedRowReaction(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Message)
	}{
		{
			name: "matching fields from issue fixture",
			mutate: func(*Message) {
			},
		},
		{
			name: "realistic later opposite direction",
			mutate: func(message *Message) {
				message.Timestamp = message.Timestamp.Add(5 * time.Minute)
				message.FromMe = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "reaction-reuse.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()

			now := time.Date(2026, 8, 3, 15, 52, 24, 0, time.UTC)
			original := Message{
				SourcePK: 42, ChatJID: "group@g.us", MessageID: "original-stanza", Timestamp: now,
				Text: "original body", RawType: 0, MessageType: "text",
			}
			stats := ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now, Messages: 1}
			if err := st.MergeAll(ctx, stats, nil, []Chat{{JID: original.ChatJID, Kind: "group"}}, nil, nil, []Message{original}); err != nil {
				t.Fatal(err)
			}

			reaction := original
			reaction.MessageID = "reaction-stanza"
			reaction.Text = ""
			reaction.RawType = whatsappReactionRawType
			reaction.MessageType = "reaction"
			reaction.MediaTitle = original.MessageID
			reaction.SourceTextNull = true
			tt.mutate(&reaction)
			stats.FinishedAt = now.Add(time.Minute)

			var firstReactionPK int64
			var firstEventID string
			for attempt := 1; attempt <= 2; attempt++ {
				stats.FinishedAt = stats.FinishedAt.Add(time.Minute)
				if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{reaction}); err != nil {
					t.Fatalf("reaction source-row reuse attempt %d: %v", attempt, err)
				}

				messages, err := st.Messages(ctx, MessageFilter{Limit: 10, Asc: true})
				if err != nil {
					t.Fatal(err)
				}
				if len(messages) != 2 {
					t.Fatalf("attempt %d stored %d messages, want original and reaction: %+v", attempt, len(messages), messages)
				}
				var storedOriginal, storedReaction *Message
				for i := range messages {
					switch messages[i].RawType {
					case 0:
						storedOriginal = &messages[i]
					case whatsappReactionRawType:
						storedReaction = &messages[i]
					}
				}
				if storedOriginal == nil || storedReaction == nil {
					t.Fatalf("attempt %d lost an event: %+v", attempt, messages)
				}
				if storedOriginal.SourcePK != original.SourcePK || storedOriginal.SourceRowPK != original.SourcePK || storedOriginal.EventID != "wa:42" || storedOriginal.MessageID != original.MessageID || storedOriginal.Text != original.Text {
					t.Fatalf("attempt %d changed original: %+v", attempt, *storedOriginal)
				}
				if storedReaction.SourcePK < syntheticMessagePKBoundary || storedReaction.SourcePK >= 2*syntheticMessagePKBoundary || storedReaction.SourceRowPK != original.SourcePK || storedReaction.EventID == storedOriginal.EventID || storedReaction.MessageID != reaction.MessageID || storedReaction.MediaTitle != original.MessageID || !storedReaction.Timestamp.Equal(reaction.Timestamp) || storedReaction.FromMe != reaction.FromMe {
					t.Fatalf("attempt %d reaction identity/provenance = %+v", attempt, *storedReaction)
				}
				if attempt == 1 {
					firstReactionPK = storedReaction.SourcePK
					firstEventID = storedReaction.EventID
				} else if storedReaction.SourcePK != firstReactionPK || storedReaction.EventID != firstEventID {
					t.Fatalf("re-import changed reaction identity: first=(%d,%q) second=(%d,%q)", firstReactionPK, firstEventID, storedReaction.SourcePK, storedReaction.EventID)
				}
			}

			var revisions int
			if err := st.DB().QueryRowContext(ctx, `select count(*) from message_revisions`).Scan(&revisions); err != nil {
				t.Fatal(err)
			}
			if revisions != 0 {
				t.Fatalf("idempotent reaction re-import created %d revisions", revisions)
			}
		})
	}
}

func TestMergeAllPreservesOriginalAndReusedRowRevoke(t *testing.T) {
	tests := []struct {
		name  string
		actor string
	}{
		{name: "lid actor", actor: "186281455824905@lid"},
		{name: "phone actor", actor: "186281455824905@s.whatsapp.net"},
		{name: "trimmed actor", actor: " 186281455824905@lid "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "revoke-reuse.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()

			now := time.Date(2026, 8, 23, 15, 52, 24, 0, time.UTC)
			original := Message{
				SourcePK: 118607, ChatJID: "group@g.us", MessageID: "original-stanza", Timestamp: now,
				Text: "original body", RawType: 0, MessageType: "text",
			}
			stats := ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now, Messages: 1}
			if err := st.MergeAll(ctx, stats, nil, []Chat{{JID: original.ChatJID, Kind: "group"}}, nil, nil, []Message{original}); err != nil {
				t.Fatal(err)
			}

			revoke := original
			revoke.MessageID = "revoke-stanza"
			revoke.Text = tt.actor
			revoke.RawType = whatsappReactionRawType
			revoke.MessageType = "reaction"
			revoke.MediaTitle = original.MessageID
			revoke.SourceTextNull = false

			var firstRevokePK int64
			var firstEventID string
			for attempt := 1; attempt <= 2; attempt++ {
				stats.FinishedAt = stats.FinishedAt.Add(time.Minute)
				if err := st.ValidateImport(ctx, stats, []Message{revoke}, false); err != nil {
					t.Fatalf("revoke source-row reuse validation attempt %d: %v", attempt, err)
				}
				if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{revoke}); err != nil {
					t.Fatalf("revoke source-row reuse attempt %d: %v", attempt, err)
				}

				messages, err := st.Messages(ctx, MessageFilter{Limit: 10, Asc: true})
				if err != nil {
					t.Fatal(err)
				}
				if len(messages) != 2 {
					t.Fatalf("attempt %d stored %d messages, want original and revoke: %+v", attempt, len(messages), messages)
				}
				var storedOriginal, storedRevoke *Message
				for i := range messages {
					switch messages[i].MessageID {
					case original.MessageID:
						storedOriginal = &messages[i]
					case revoke.MessageID:
						storedRevoke = &messages[i]
					}
				}
				if storedOriginal == nil || storedRevoke == nil {
					t.Fatalf("attempt %d lost an event: %+v", attempt, messages)
				}
				if storedOriginal.SourcePK != original.SourcePK || storedOriginal.SourceRowPK != original.SourcePK || storedOriginal.EventID != "wa:118607" || storedOriginal.MessageID != original.MessageID || storedOriginal.Text != original.Text || storedOriginal.RawType != original.RawType {
					t.Fatalf("attempt %d changed original: %+v", attempt, *storedOriginal)
				}
				if storedRevoke.SourcePK < syntheticMessagePKBoundary || storedRevoke.SourcePK >= 2*syntheticMessagePKBoundary || storedRevoke.SourceRowPK != original.SourcePK || storedRevoke.EventID == storedOriginal.EventID || storedRevoke.MessageID != revoke.MessageID || storedRevoke.Text != revoke.Text || storedRevoke.MediaTitle != original.MessageID || !storedRevoke.Timestamp.Equal(original.Timestamp) || storedRevoke.FromMe != original.FromMe {
					t.Fatalf("attempt %d revoke identity/provenance = %+v", attempt, *storedRevoke)
				}
				if attempt == 1 {
					firstRevokePK = storedRevoke.SourcePK
					firstEventID = storedRevoke.EventID
				} else if storedRevoke.SourcePK != firstRevokePK || storedRevoke.EventID != firstEventID {
					t.Fatalf("re-import changed revoke identity: first=(%d,%q) second=(%d,%q)", firstRevokePK, firstEventID, storedRevoke.SourcePK, storedRevoke.EventID)
				}
			}

			var revisions int
			if err := st.DB().QueryRowContext(ctx, `select count(*) from message_revisions`).Scan(&revisions); err != nil {
				t.Fatal(err)
			}
			if revisions != 0 {
				t.Fatalf("idempotent revoke re-import created %d revisions", revisions)
			}
		})
	}
}

func TestReactionReusesSourceRowRejectsNonActorText(t *testing.T) {
	existing := Message{SourcePK: 118607, ChatJID: "group@g.us", MessageID: "original-stanza"}
	incoming := Message{
		SourcePK: existing.SourcePK, ChatJID: existing.ChatJID, MessageID: "revoke-stanza",
		RawType: whatsappReactionRawType, MediaTitle: existing.MessageID,
	}

	for _, text := range []string{
		"",
		"@lid",
		"actor@lid",
		"123actor@lid",
		"186281455824905@example.com",
		"186281455824905@lid extra",
	} {
		t.Run(text, func(t *testing.T) {
			incoming.Text = text
			if reactionReusesSourceRow(existing, incoming) {
				t.Fatalf("non-actor text %q matched source-row reuse", text)
			}
		})
	}
}

func TestMergeAllRejectsUnrelatedReactionSourceRowCollision(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "unrelated-reaction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 8, 3, 15, 52, 24, 0, time.UTC)
	original := Message{SourcePK: 42, ChatJID: "group@g.us", MessageID: "original-stanza", Timestamp: now, Text: "original body", RawType: 0}
	stats := ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: now, Messages: 1}
	if err := st.MergeAll(ctx, stats, nil, []Chat{{JID: original.ChatJID, Kind: "group"}}, nil, nil, []Message{original}); err != nil {
		t.Fatal(err)
	}

	unrelated := original
	unrelated.MessageID = "reaction-stanza"
	unrelated.Text = ""
	unrelated.RawType = whatsappReactionRawType
	unrelated.MessageType = "reaction"
	unrelated.MediaTitle = "another-stanza"
	unrelated.SourceTextNull = true
	stats.FinishedAt = now.Add(time.Minute)
	if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{unrelated}); err == nil || !strings.Contains(err.Error(), "different event") {
		t.Fatalf("unrelated reaction collision error = %v", err)
	}
	var messages int
	if err := st.DB().QueryRowContext(ctx, `select count(*) from messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 1 {
		t.Fatalf("failed collision changed archive row count to %d", messages)
	}
}

func TestReusedReactionDoesNotEstablishLegacyAccountContinuity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "reaction-account-continuity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 8, 3, 15, 52, 24, 0, time.UTC)
	original := Message{SourcePK: 42, ChatJID: "group@g.us", MessageID: "original-stanza", Timestamp: now, Text: "body", RawType: 0}
	base := ImportStats{SourceIdentity: "/source", SourceStoreIdentity: "wa-store:fixture", AccountIdentity: "wa-account:legacy", FinishedAt: now, Messages: 1}
	if err := st.MergeAll(ctx, base, nil, []Chat{{JID: original.ChatJID, Kind: "group"}}, nil, nil, []Message{original}); err != nil {
		t.Fatal(err)
	}

	reaction := original
	reaction.MessageID = "reaction-stanza"
	reaction.Text = ""
	reaction.RawType = whatsappReactionRawType
	reaction.MessageType = "reaction"
	reaction.MediaTitle = original.MessageID
	reaction.SourceTextNull = true
	upgrade := base
	upgrade.AccountIdentity = "wa-account:recipient"
	upgrade.LegacyAccountIDs = []string{base.AccountIdentity}
	upgrade.FinishedAt = now.Add(time.Minute)
	if err := st.ValidateImport(ctx, upgrade, []Message{reaction}, false); err == nil || !strings.Contains(err.Error(), "different WhatsApp account") {
		t.Fatalf("reused reaction row established account continuity: %v", err)
	}
}

func TestMergeAllRejectsDifferentSourceAndIdentityCollision(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 17, 0, 0, 0, time.UTC)
	message := Message{SourcePK: 1, ChatJID: "chat-a", MessageID: "one", Timestamp: now, Text: "original", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/account-a", FinishedAt: now, Messages: 1}, nil,
		[]Chat{{JID: message.ChatJID, Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/account-b", FinishedAt: now.Add(time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{message}); err == nil || !strings.Contains(err.Error(), "separate --db") {
		t.Fatalf("different source error = %v", err)
	}
	collision := message
	collision.ChatJID = "chat-b"
	collision.MessageID = "different"
	collision.Text = "replacement"
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/account-a", FinishedAt: now.Add(time.Minute), Messages: 1}, nil,
		[]Chat{{JID: collision.ChatJID, Kind: "dm"}}, nil, nil, []Message{collision}); err == nil || !strings.Contains(err.Error(), "different event") {
		t.Fatalf("identity collision error = %v", err)
	}
	stored, err := st.MessageBySourcePK(ctx, 1)
	if err != nil || stored.Text != "original" || stored.ChatJID != "chat-a" {
		t.Fatalf("collision changed stored message: %+v, %v", stored, err)
	}
	if err := st.ReplaceAll(ctx, ImportStats{SourcePath: "/account-b", SourceIdentity: "/account-b", AccountIdentity: "account-b", FinishedAt: now.Add(2 * time.Minute), Messages: 1}, nil,
		[]Chat{{JID: collision.ChatJID, Kind: "dm"}}, nil, nil, []Message{collision}); err != nil {
		t.Fatalf("explicit restore should switch sources: %v", err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/account-b", SourceIdentity: "/account-b", AccountIdentity: "account-b", FinishedAt: now.Add(3 * time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{collision}); err != nil {
		t.Fatalf("merge after explicit source switch: %v", err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/account-a", SourceIdentity: "/account-a", AccountIdentity: "account-a", FinishedAt: now.Add(4 * time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{collision}); err == nil || !strings.Contains(err.Error(), "separate --db") {
		t.Fatalf("restore did not bind replacement source: %v", err)
	}
}

func TestPortableRestoreRequiresExplicitSourceAdoption(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "portable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 17, 30, 0, 0, time.UTC)
	message := Message{SourcePK: 1, ChatJID: "chat", MessageID: "one", Timestamp: now, Text: "body", RawType: 0}
	if err := st.ReplaceAll(ctx, ImportStats{SourcePath: "backup:/portable", FinishedAt: now, Messages: 1}, nil,
		[]Chat{{JID: message.ChatJID, Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	stats := ImportStats{SourcePath: "/account-a", SourceIdentity: "/account-a", AccountIdentity: "account-a", FinishedAt: now.Add(time.Minute), Messages: 1}
	if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{message}); err == nil || !strings.Contains(err.Error(), "--adopt-source") {
		t.Fatalf("portable restore accepted an unverified source: %v", err)
	}
	stats.AdoptSource = true
	if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{message}); err != nil {
		t.Fatalf("explicit source adoption failed: %v", err)
	}
}

func TestMergeInheritsLegacySourceBinding(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "legacy-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 17, 45, 0, 0, time.UTC)
	message := Message{SourcePK: 1, ChatJID: "chat", MessageID: "one", Timestamp: now, Text: "body", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{FinishedAt: now, Messages: 1}, nil, []Chat{{JID: "chat", Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `insert into sync_state(key,value,updated_at) values('source_path','/account-a',?) on conflict(key) do update set value=excluded.value`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "/account-b", AccountIdentity: "account-b", FinishedAt: now}, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "separate --db") {
		t.Fatalf("different source bypassed legacy binding: %v", err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/account-a", SourceIdentity: "/account-a", AccountIdentity: "account-a", AdoptSource: true, FinishedAt: now, Messages: 1}, nil, nil, nil, nil, []Message{message}); err != nil {
		t.Fatalf("matching source failed legacy binding: %v", err)
	}
	var binding string
	if err := st.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_source_path'`).Scan(&binding); err != nil || binding != "/account-a" {
		t.Fatalf("seeded binding = %q, %v", binding, err)
	}
}

func TestMergeUpgradesAndChecksSourceStoreBinding(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 17, 50, 0, 0, time.UTC)
	message := Message{SourcePK: 1, ChatJID: "chat", MessageID: "one", Timestamp: now, Text: "body", RawType: 0}
	base := ImportStats{SourcePath: "/source", SourceIdentity: "/source", AccountIdentity: "wa-account:fixture", FinishedAt: now, Messages: 1}
	if err := st.MergeAll(ctx, base, nil, []Chat{{JID: "chat", Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	base.SourceStoreIdentity = "wa-store:first"
	if err := st.MergeAll(ctx, base, nil, nil, nil, nil, []Message{message}); err != nil {
		t.Fatalf("store binding upgrade: %v", err)
	}
	base.SourceStoreIdentity = ""
	if err := st.MergeAll(ctx, base, nil, nil, nil, nil, []Message{message}); err == nil || !strings.Contains(err.Error(), "different WhatsApp Desktop store") {
		t.Fatalf("missing established store error = %v", err)
	}
	base.SourceStoreIdentity = "wa-store:second"
	if err := st.MergeAll(ctx, base, nil, nil, nil, nil, []Message{message}); err == nil || !strings.Contains(err.Error(), "different WhatsApp Desktop store") {
		t.Fatalf("different store error = %v", err)
	}
}

func TestMergeAdoptsEarlyStoreUUIDAccountBinding(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "early-v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 17, 55, 0, 0, time.UTC)
	message := Message{SourcePK: 1, ChatJID: "chat", MessageID: "one", Timestamp: now, Text: "body", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{FinishedAt: now, Messages: 1}, nil, []Chat{{JID: "chat", Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"source_path": "/source", "merge_source_path": "wa-store:legacy", "merge_account_identity": "wa-store:legacy",
	} {
		if _, err := st.DB().ExecContext(ctx, `insert into sync_state(key,value,updated_at) values(?,?,?) on conflict(key) do update set value=excluded.value`, key, value, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	exported, err := st.ExportAll(ctx)
	if err != nil || exported.AccountIdentity != "" || exported.SourceStoreIdentity != "wa-store:legacy" {
		t.Fatalf("early v2 export identity = %+v, %v", exported, err)
	}
	if err := exported.Validate(); err != nil {
		t.Fatalf("early v2 export validation: %v", err)
	}
	stats := ImportStats{SourcePath: "/source", SourceIdentity: "/source", SourceStoreIdentity: "wa-store:legacy", AccountIdentity: "wa-account:verified", AdoptSource: true, FinishedAt: now, Messages: 1}
	if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{message}); err != nil {
		t.Fatalf("early v2 binding adoption: %v", err)
	}
}

func TestMergeMigratesLegacyAccountBindingWithEventContinuity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "legacy-account.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 8, 3, 4, 30, 0, 0, time.UTC)
	existing := Message{SourcePK: 1, ChatJID: "chat", MessageID: "one", Timestamp: now, Text: "body", RawType: 0}
	base := ImportStats{SourceIdentity: "/source", SourceStoreIdentity: "wa-store:fixture", AccountIdentity: "wa-account:legacy", FinishedAt: now, Messages: 1}
	if err := st.MergeAll(ctx, base, nil, []Chat{{JID: "chat", Kind: "dm"}}, nil, nil, []Message{existing}); err != nil {
		t.Fatal(err)
	}
	upgrade := ImportStats{
		SourceIdentity:      base.SourceIdentity,
		SourceStoreIdentity: base.SourceStoreIdentity,
		AccountIdentity:     "wa-account:recipient",
		LegacyAccountIDs:    []string{base.AccountIdentity},
		FinishedAt:          now.Add(time.Minute),
		Messages:            1,
	}
	disjoint := Message{SourcePK: 2, ChatJID: "chat", MessageID: "two", Timestamp: now, Text: "other", RawType: 0}
	if err := st.ValidateImport(ctx, upgrade, []Message{disjoint}, false); err == nil || !strings.Contains(err.Error(), "different WhatsApp account") {
		t.Fatalf("legacy candidate without event continuity error = %v", err)
	}
	if err := st.MergeAll(ctx, upgrade, nil, nil, nil, nil, []Message{existing}); err != nil {
		t.Fatalf("legacy account migration: %v", err)
	}
	var binding string
	if err := st.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_account_identity'`).Scan(&binding); err != nil || binding != upgrade.AccountIdentity {
		t.Fatalf("migrated account binding = %q, %v", binding, err)
	}
}

func TestStrongSourceIdentityRequiresAccountContinuity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "continuity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Date(2026, 7, 18, 18, 0, 0, 0, time.UTC)
	if err := st.ValidateImport(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-a"}, nil, false); err != nil {
		t.Fatalf("empty archive preflight: %v", err)
	}
	existing := Message{SourcePK: 1, ChatJID: "chat-a", MessageID: "one", Timestamp: now, Text: "body", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{FinishedAt: now, Messages: 1}, nil, []Chat{{JID: existing.ChatJID, Kind: "dm"}}, nil, nil, []Message{existing}); err != nil {
		t.Fatal(err)
	}
	disjoint := Message{SourcePK: 2, ChatJID: "chat-b", MessageID: "two", Timestamp: now, Text: "other", RawType: 0}
	if err := st.ValidateImport(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-a"}, []Message{disjoint}, false); err == nil || !strings.Contains(err.Error(), "--adopt-source") {
		t.Fatalf("unverified account binding error = %v", err)
	}
	if err := st.ValidateImport(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-a"}, []Message{existing}, false); err == nil || !strings.Contains(err.Error(), "--adopt-source") {
		t.Fatalf("event overlap bypassed source adoption: %v", err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-a", AdoptSource: true, FinishedAt: now, Messages: 1}, nil, nil, nil, nil, []Message{existing}); err != nil {
		t.Fatalf("explicit account adoption: %v", err)
	}
	if err := st.ValidateImport(ctx, ImportStats{SourceIdentity: "/source"}, []Message{existing}, false); err == nil || !strings.Contains(err.Error(), "different WhatsApp account") {
		t.Fatalf("missing account identity error = %v", err)
	}
	if err := st.ValidateImport(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-a"}, []Message{disjoint}, false); err != nil {
		t.Fatalf("bound account identity should admit new events: %v", err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-b", FinishedAt: now, Messages: 1}, nil, nil, nil, nil, []Message{existing}); err == nil || !strings.Contains(err.Error(), "different WhatsApp account") {
		t.Fatalf("different account error = %v", err)
	}
	collision := existing
	collision.MessageID = "different"
	if err := st.ValidateImport(ctx, ImportStats{SourceIdentity: "/source", AccountIdentity: "account-a"}, []Message{collision}, false); err == nil || !strings.Contains(err.Error(), "different event") {
		t.Fatalf("collision preflight error = %v", err)
	}
	if err := st.ValidateImport(ctx, ImportStats{}, []Message{{SourcePK: 0}}, true); err == nil || !strings.Contains(err.Error(), "empty source_pk") {
		t.Fatalf("invalid restore preflight error = %v", err)
	}
}

func TestSourceIdentityHelpers(t *testing.T) {
	if got := legacySourceIdentity("  "); got != "" {
		t.Fatalf("empty legacy identity = %q", got)
	}
	if got := legacySourceIdentity("backup:/tmp/repo"); got != "" {
		t.Fatalf("backup legacy identity = %q", got)
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "source-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := legacySourceIdentity(link); got != want {
		t.Fatalf("symlink legacy identity = %q, want %q", got, want)
	}
	if got := formatSyncTime(time.Time{}); got != "" {
		t.Fatalf("zero sync time = %q", got)
	}
}

func TestMessageRevisionComparisonUsesPersistedUnixTime(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "timestamps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	instant := time.Unix(1_752_841_800, 987_654_321).UTC()
	message := Message{SourcePK: 1, ChatJID: "chat", MessageID: "one", Timestamp: instant, Text: "body", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: instant, Messages: 1}, nil, []Chat{{JID: "chat", Kind: "dm"}}, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	message.Timestamp = instant.In(time.FixedZone("fixture", 5*60*60))
	if err := st.MergeAll(ctx, ImportStats{SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: instant.Add(time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{message}); err != nil {
		t.Fatal(err)
	}
	poisoned := Message{SourcePK: 2, ChatJID: "chat", MessageID: "poisoned", Timestamp: time.Date(12000, 1, 1, 0, 0, 0, 0, time.UTC), Text: "future", RawType: 0}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: instant.Add(2 * time.Minute), Messages: 2}, nil, nil, nil, nil, []Message{message, poisoned}); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeAll(ctx, ImportStats{SourcePath: "/fixture", SourceIdentity: "fixture-store", AccountIdentity: "fixture-account", FinishedAt: instant.Add(3 * time.Minute), Messages: 1}, nil, nil, nil, nil, []Message{poisoned}); err != nil {
		t.Fatalf("repeat poisoned timestamp merge: %v", err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.MessageRevisions != 0 || status.Messages != 2 {
		t.Fatalf("timestamp-only revisions = %+v", status)
	}
}

func TestMigrateV1ArchiveLosslessly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `
create table chats (jid text primary key, kind text not null, name text, last_message_at integer, unread_count integer not null default 0, archived integer not null default 0, removed integer not null default 0, hidden integer not null default 0, raw_session_type integer not null default 0);
create table contacts (jid text primary key, phone text, full_name text, first_name text, last_name text, business_name text, username text, lid text, about_text text, updated_at integer);
create table groups (jid text primary key, name text, owner_jid text, created_at integer);
create table group_participants (group_jid text not null, user_jid text not null, contact_name text, first_name text, is_admin integer not null default 0, is_active integer not null default 0, primary key(group_jid,user_jid));
create table messages (rowid integer primary key autoincrement, source_pk integer not null unique, chat_jid text not null, chat_name text, msg_id text not null, sender_jid text, sender_name text, ts integer not null, from_me integer not null, text text, raw_type integer not null, message_type text, media_type text, media_title text, media_path text, media_url text, media_size integer, starred integer not null default 0);
create virtual table messages_fts using fts5(text, chat, sender, media);
create table sync_state (key text primary key, value text not null, updated_at integer not null);
insert into contacts values('alice','1','Alice','','','','','','',100);
insert into chats values('chat','dm','Chat',200,0,0,0,0,0);
insert into groups values('chat','Chat','alice',50);
insert into group_participants values('chat','alice','Alice','',1,1);
insert into messages(source_pk,chat_jid,chat_name,msg_id,sender_jid,sender_name,ts,from_me,text,raw_type,message_type,media_type,media_title,media_path,media_url,media_size,starred) values(7,'chat','Chat','stanza','alice','Alice',200,0,'legacy body',0,'text','','','','',0,0);
insert into messages_fts(rowid,text,chat,sender,media) values((select rowid from messages where source_pk=7),'legacy body','Chat','Alice','');
insert into sync_state values('last_import_at','2026-07-17T12:00:00Z',1000);
pragma user_version=1;`
	if _, err := db.ExecContext(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	var rowID int64
	if err := db.QueryRowContext(ctx, `select rowid from messages where source_pk=7`).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var version int
	var migratedRowID int64
	var eventID string
	var tombstones int
	if err := st.DB().QueryRowContext(ctx, `pragma user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var sourceRowPK int64
	if err := st.DB().QueryRowContext(ctx, `select rowid,source_row_pk,event_id,(deleted_at is not null)+(deletion_source is not null)+(deletion_reason is not null) from messages where source_pk=7`).Scan(&migratedRowID, &sourceRowPK, &eventID, &tombstones); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || migratedRowID != rowID || sourceRowPK != 7 || eventID != "wa:7" || tombstones != 0 {
		t.Fatalf("migration version=%d rowid=%d/%d source_row_pk=%d event=%q tombstones=%d", version, migratedRowID, rowID, sourceRowPK, eventID, tombstones)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Contacts != 1 || status.Chats != 1 || status.Groups != 1 || status.Participants != 1 || status.Messages != 1 {
		t.Fatalf("migrated counts = %+v", status)
	}
	results, err := st.Search(ctx, MessageFilter{Query: "legacy", Limit: 10})
	if err != nil || len(results) != 1 || results[0].Text != "legacy body" {
		t.Fatalf("migrated search = %+v, %v", results, err)
	}
	var quickCheck string
	if err := st.DB().QueryRowContext(ctx, `pragma quick_check`).Scan(&quickCheck); err != nil {
		t.Fatal(err)
	}
	if quickCheck != "ok" {
		t.Fatalf("quick_check = %q", quickCheck)
	}
	if _, err := st.DB().ExecContext(ctx, `insert into messages(source_pk,event_id,chat_jid,msg_id,ts,from_me,raw_type,starred,last_seen_at) values(8,'','chat','bad',200,0,0,0,1000)`); err == nil {
		t.Fatal("empty event identity insert should fail")
	}
}

func TestStoreReplaceStatusListSearch(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	stats := ImportStats{SourcePath: "/tmp/source", DBPath: st.Path(), StartedAt: now.Add(-time.Second), FinishedAt: now}
	contacts := []Contact{{JID: "alice@s.whatsapp.net", FullName: "Alice", UpdatedAt: now}}
	chats := []Chat{{JID: "chat@g.us", Kind: "group", Name: "Chat", LastMessageAt: now, UnreadCount: 2, MessageCount: 2}}
	groups := []Group{{JID: "chat@g.us", Name: "Chat", OwnerJID: "owner@s.whatsapp.net", CreatedAt: now.Add(-time.Hour)}}
	participants := []GroupParticipant{{GroupJID: "chat@g.us", UserJID: "alice@s.whatsapp.net", ContactName: "Alice", IsAdmin: true, IsActive: true}}
	messages := []Message{
		{SourcePK: 1, ChatJID: "chat@g.us", ChatName: "Chat", MessageID: "a", SenderJID: "alice@s.whatsapp.net", SenderName: "Alice", Timestamp: now.Add(-time.Minute), Text: "hello launch", RawType: 0, MessageType: "text"},
		{SourcePK: 2, ChatJID: "chat@g.us", ChatName: "Chat", MessageID: "b", SenderJID: "me", SenderName: "me", Timestamp: now, FromMe: true, Text: "photo", RawType: 1, MessageType: "image", MediaType: "image", MediaTitle: "launch image", MediaPath: "/tmp/image.jpg", MediaSize: 123},
	}
	if err := st.ReplaceAll(ctx, stats, contacts, chats, groups, participants, messages); err != nil {
		t.Fatal(err)
	}

	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 2 || status.MediaMessages != 1 || status.UnreadChats != 1 || status.UnreadMessages != 2 || status.LastSource != "/tmp/source" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if st.DB() == nil {
		t.Fatal("DB should be available")
	}

	listed, err := st.Messages(ctx, MessageFilter{ChatJID: "chat@g.us", Limit: 10, Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].MessageID != "a" || listed[1].MessageID != "b" {
		t.Fatalf("unexpected messages: %+v", listed)
	}

	onlyMine := true
	filtered, err := st.Messages(ctx, MessageFilter{FromMe: &onlyMine, HasMedia: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].MessageID != "b" {
		t.Fatalf("unexpected filtered messages: %+v", filtered)
	}

	results, err := st.Search(ctx, MessageFilter{Query: "launch", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 search results, got %d", len(results))
	}
	if _, err := st.Search(ctx, MessageFilter{}); err == nil {
		t.Fatal("expected empty search query error")
	}

	after := now.Add(-2 * time.Minute)
	before := now.Add(time.Minute)
	results, err = st.Messages(ctx, MessageFilter{After: &after, Before: &before, Sender: "alice@s.whatsapp.net", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MessageID != "a" {
		t.Fatalf("unexpected ranged sender results: %+v", results)
	}

	chatsOut, err := st.ListChats(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatsOut) != 1 || chatsOut[0].JID != "chat@g.us" {
		t.Fatalf("unexpected chats: %+v", chatsOut)
	}
	unreadChats, err := st.ListUnreadChats(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadChats) != 1 || unreadChats[0].UnreadCount != 2 {
		t.Fatalf("unexpected unread chats: %+v", unreadChats)
	}

	exported, err := st.ExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contactsOut, err := st.Contacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contactsOut) != 1 || contactsOut[0].JID != "alice@s.whatsapp.net" {
		t.Fatalf("unexpected contacts: %+v", contactsOut)
	}
	if err := exported.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(exported.Contacts) != 1 || len(exported.Chats) != 1 || len(exported.Groups) != 1 || len(exported.Participants) != 1 || len(exported.Messages) != 2 {
		t.Fatalf("unexpected export: %+v", exported)
	}
	if stats := exported.ImportStats("backup", st.Path(), now); stats.Messages != 2 || stats.MediaMessages != 1 || stats.SourcePath != "backup" {
		t.Fatalf("unexpected export stats: %+v", stats)
	}
	if stats := exported.ImportStats("backup", st.Path(), time.Time{}); stats.FinishedAt.IsZero() || stats.StartedAt.IsZero() {
		t.Fatalf("zero finished time was not defaulted: %+v", stats)
	}
	restored, err := Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if err := restored.ImportSnapshot(ctx, exported, "backup", now); err != nil {
		t.Fatal(err)
	}
	restoredStatus, err := restored.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restoredStatus.Messages != 2 || restoredStatus.LastSource != "backup" {
		t.Fatalf("unexpected restored status: %+v", restoredStatus)
	}
	if err := (SnapshotData{Messages: []Message{{SourcePK: 1}, {SourcePK: 1}}}).Validate(); err == nil {
		t.Fatal("expected duplicate source_pk validation error")
	}
	if err := (SnapshotData{AccountIdentity: "raw-account"}).Validate(); err == nil {
		t.Fatal("expected invalid account identity error")
	}
	if err := (SnapshotData{SourceStoreIdentity: "raw-store"}).Validate(); err == nil {
		t.Fatal("expected invalid source-store identity error")
	}
	if err := (SnapshotData{Messages: []Message{{}}}).Validate(); err == nil {
		t.Fatal("expected empty source_pk validation error")
	}
	if err := (SnapshotData{Messages: []Message{{SourcePK: 1, EventID: "same"}, {SourcePK: 2, EventID: "same"}}}).Validate(); err == nil {
		t.Fatal("expected duplicate event_id validation error")
	}
	validRevision := MessageRevision{EventID: "wa:1", PayloadJSON: `{}`, RecordedAt: now, EventSource: "fixture", Reason: "edit"}
	if err := (SnapshotData{Messages: []Message{{SourcePK: 1}}, Revisions: []MessageRevision{validRevision}}).Validate(); err != nil {
		t.Fatalf("valid revision rejected: %v", err)
	}
	unknownRevision := validRevision
	unknownRevision.EventID = "wa:missing"
	if err := (SnapshotData{Messages: []Message{{SourcePK: 1}}, Revisions: []MessageRevision{unknownRevision}}).Validate(); err == nil {
		t.Fatal("expected unknown revision event validation error")
	}
	incompleteRevision := validRevision
	incompleteRevision.Reason = ""
	if err := (SnapshotData{Messages: []Message{{SourcePK: 1}}, Revisions: []MessageRevision{incompleteRevision}}).Validate(); err == nil {
		t.Fatal("expected incomplete revision validation error")
	}
}

func TestOpenRequiresPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("expected opening directory as db to fail")
	}
	if err := (*Store)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if unix(time.Time{}) != 0 {
		t.Fatal("zero time unix should be zero")
	}
	if !fromUnix(0).IsZero() {
		t.Fatal("zero unix should be zero time")
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`pragma user_version=999`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema error = %v", err)
	}
}

func TestOpenInMemoryDoesNotCreateFile(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ":memory:")); !os.IsNotExist(err) {
		t.Fatalf("in-memory archive created a disk file: %v", err)
	}
}

func TestFromUnixJSONBounds(t *testing.T) {
	got := fromUnix(maxJSONUnixSecond)
	want := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) || got.Location().String() != "UTC" {
		t.Fatalf("fromUnix(max) = %s (%s), want %s UTC", got, got.Location(), want)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("max JSON-safe timestamp should marshal: %v", err)
	}
	if got := fromUnix(maxJSONUnixSecond + 1); !got.IsZero() {
		t.Fatalf("out-of-range unix should clamp to zero, got %v", got)
	}
	if got := fromUnix(-1); !got.IsZero() {
		t.Fatalf("negative unix should clamp to zero, got %v", got)
	}
}

func TestReplaceAllDuplicateSourcePKFails(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	err = st.ReplaceAll(
		ctx, ImportStats{FinishedAt: now}, nil,
		[]Chat{{JID: "chat", Kind: "dm", Name: "Chat", LastMessageAt: now}},
		nil,
		nil,
		[]Message{
			{SourcePK: 1, ChatJID: "chat", MessageID: "a", Timestamp: now, RawType: 0},
			{SourcePK: 1, ChatJID: "chat", MessageID: "b", Timestamp: now, RawType: 0},
		},
	)
	if err == nil {
		t.Fatal("expected duplicate source_pk error")
	}
	status, statusErr := st.Status(ctx)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if status.Messages != 0 {
		t.Fatalf("failed replace should roll back, got %+v", status)
	}
}

func TestValidateImportRejectsInvalidEventIdentity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	cases := []struct {
		name     string
		messages []Message
		want     string
	}{
		{name: "empty source pk", messages: []Message{{EventID: "event"}}, want: "empty source_pk"},
		{name: "whitespace event id", messages: []Message{{SourcePK: 1, EventID: "  "}}, want: "empty event_id"},
		{name: "generated event collision", messages: []Message{{SourcePK: 1}, {SourcePK: 2, EventID: "wa:1"}}, want: "duplicate message event_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.ValidateImport(ctx, ImportStats{}, tc.messages, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestImportSnapshotRefreshesFTS(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	base := SnapshotData{
		Chats: []Chat{{JID: "chat", Kind: "dm", Name: "Chat", LastMessageAt: now}},
		Messages: []Message{{
			SourcePK:  1,
			ChatJID:   "chat",
			ChatName:  "Chat",
			MessageID: "a",
			Timestamp: now,
			Text:      "old import text",
			RawType:   0,
		}},
	}
	if err := st.ImportSnapshot(ctx, base, "first", now); err != nil {
		t.Fatal(err)
	}
	results, err := st.Search(ctx, MessageFilter{Query: "old", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected old FTS result, got %d", len(results))
	}

	updated := base
	updated.Messages[0].Text = "new import text"
	updated.Messages[0].MediaTitle = "fresh media title"
	if err := st.ImportSnapshot(ctx, updated, "second", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	results, err = st.Search(ctx, MessageFilter{Query: "old", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected old FTS text to be removed, got %+v", results)
	}
	results, err = st.Search(ctx, MessageFilter{Query: "fresh", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MessageID != "a" {
		t.Fatalf("expected updated media title FTS result, got %+v", results)
	}
}

func TestSearchMatchesNonSequentialSourcePK(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	if err := st.ReplaceAll(
		ctx,
		ImportStats{FinishedAt: now},
		nil,
		[]Chat{{JID: "chat", Kind: "dm", Name: "Chat", LastMessageAt: now}},
		nil,
		nil,
		[]Message{{
			SourcePK:  9001,
			ChatJID:   "chat",
			ChatName:  "Chat",
			MessageID: "non-sequential",
			Timestamp: now,
			Text:      "needle survives rowid mapping",
			RawType:   0,
		}},
	); err != nil {
		t.Fatal(err)
	}

	results, err := st.Search(ctx, MessageFilter{Query: "needle", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SourcePK != 9001 || results[0].MessageID != "non-sequential" {
		t.Fatalf("FTS rowid mapping returned wrong message: %+v", results)
	}
}

func TestListChatsClampsOutOfRangePersistedTimestamp(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	valid := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	if _, err := st.DB().ExecContext(ctx, `
insert into chats(jid, kind, name, last_message_at, unread_count, archived, removed, hidden, raw_session_type)
values
	('0@status', 'status', 'Status', ?, 1, 0, 0, 0, 0),
	('valid@s.whatsapp.net', 'dm', 'Valid', ?, 1, 0, 0, 0, 0)
`, maxJSONUnixSecond+1, valid.Unix()); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		list func() ([]Chat, error)
	}{
		{"ListChats", func() ([]Chat, error) { return st.ListChats(ctx, 10) }},
		{"ListUnreadChats", func() ([]Chat, error) { return st.ListUnreadChats(ctx, 10) }},
	} {
		got, err := tc.list()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 2 {
			t.Fatalf("%s: want 2 chats, got %d", tc.name, len(got))
		}
		if got[0].JID != "valid@s.whatsapp.net" || !got[0].LastMessageAt.Equal(valid) {
			t.Fatalf("%s: valid chat should sort before clamped poison, got %+v", tc.name, got)
		}
		if got[1].JID != "0@status" || !got[1].LastMessageAt.IsZero() {
			t.Fatalf("%s: poisoned chat should clamp to zero and sort oldest, got %+v", tc.name, got)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("%s: JSON marshal of already-populated archive failed: %v", tc.name, err)
		}
	}
}

func TestMessagesClampsOutOfRangePersistedTimestamp(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	valid := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	if _, err := st.DB().ExecContext(ctx, `
insert into chats(jid, kind, name, last_message_at, unread_count, archived, removed, hidden, raw_session_type)
values('c@s.whatsapp.net', 'dm', 'C', ?, 0, 0, 0, 0, 0)
`, valid.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `
insert into messages(source_pk, event_id, chat_jid, chat_name, msg_id, sender_jid, sender_name, ts, from_me, text, raw_type, message_type, media_type, media_title, media_path, media_url, media_size, starred)
values
	(1, 'wa:1', 'c@s.whatsapp.net', 'C', 'poison', '', '', ?, 0, 'poison', 0, 'text', '', '', '', '', 0, 0),
	(2, 'wa:2', 'c@s.whatsapp.net', 'C', 'valid', '', '', ?, 0, 'valid', 0, 'text', '', '', '', '', 0, 0)
`, maxJSONUnixSecond+1, valid.Unix()); err != nil {
		t.Fatal(err)
	}

	desc, err := st.Messages(ctx, MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc) != 2 || desc[0].MessageID != "valid" || desc[1].MessageID != "poison" || !desc[1].Timestamp.IsZero() {
		t.Fatalf("poisoned message should clamp to zero and sort oldest in desc order: %+v", desc)
	}
	if _, err := json.Marshal(desc); err != nil {
		t.Fatalf("messages JSON marshal failed on poisoned messages.ts: %v", err)
	}

	asc, err := st.Messages(ctx, MessageFilter{Limit: 10, Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(asc) != 2 || asc[0].MessageID != "poison" || asc[1].MessageID != "valid" {
		t.Fatalf("poisoned message should sort as oldest in asc order: %+v", asc)
	}

	after := valid.Add(-time.Hour)
	filtered, err := st.Messages(ctx, MessageFilter{After: &after, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].MessageID != "valid" {
		t.Fatalf("date filters should exclude unknown poisoned timestamps, got %+v", filtered)
	}
}

func TestStatusClampsOutOfRangeMessageTimestamp(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	valid := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	if _, err := st.DB().ExecContext(ctx, `
insert into chats(jid, kind, name, last_message_at, unread_count, archived, removed, hidden, raw_session_type)
values('c@s.whatsapp.net', 'dm', 'C', ?, 0, 0, 0, 0, 0)
`, valid.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `
insert into messages(source_pk, event_id, chat_jid, chat_name, msg_id, sender_jid, sender_name, ts, from_me, text, raw_type, message_type, media_type, media_title, media_path, media_url, media_size, starred)
values
	(1, 'wa:1', 'c@s.whatsapp.net', 'C', 'poison', '', '', ?, 0, 'poison', 0, 'text', '', '', '', '', 0, 0),
	(2, 'wa:2', 'c@s.whatsapp.net', 'C', 'valid', '', '', ?, 0, 'valid', 0, 'text', '', '', '', '', 0, 0)
`, maxJSONUnixSecond+1, valid.Unix()); err != nil {
		t.Fatal(err)
	}

	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.OldestMessage.Equal(valid) || !status.NewestMessage.Equal(valid) {
		t.Fatalf("status bounds should ignore poisoned messages.ts and keep valid bounds: %+v", status)
	}
	if _, err := json.Marshal(status); err != nil {
		t.Fatalf("status JSON marshal failed on poisoned messages.ts: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `delete from messages where source_pk = 2`); err != nil {
		t.Fatal(err)
	}
	status, err = st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.OldestMessage.IsZero() || !status.NewestMessage.IsZero() {
		t.Fatalf("all-invalid status bounds should clamp to zero: %+v", status)
	}
}
