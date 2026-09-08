package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/wacrawl/internal/store/storedb"
)

type SnapshotData struct {
	Contacts            []Contact
	Chats               []Chat
	Groups              []Group
	Participants        []GroupParticipant
	Messages            []Message
	Revisions           []MessageRevision
	SourceStoreIdentity string `json:"source_store_identity,omitempty"`
	AccountIdentity     string `json:"account_identity,omitempty"`
}

func (d SnapshotData) ImportStats(sourcePath, dbPath string, finishedAt time.Time) ImportStats {
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}
	mediaMessages := 0
	for _, message := range d.Messages {
		if message.MediaType != "" || message.MediaPath != "" || message.MediaURL != "" {
			mediaMessages++
		}
	}
	return ImportStats{
		SourcePath:          sourcePath,
		SourceStoreIdentity: d.SourceStoreIdentity,
		AccountIdentity:     d.AccountIdentity,
		DBPath:              dbPath,
		Chats:               len(d.Chats),
		Contacts:            len(d.Contacts),
		Groups:              len(d.Groups),
		Participants:        len(d.Participants),
		Messages:            len(d.Messages),
		MediaMessages:       mediaMessages,
		StartedAt:           finishedAt,
		FinishedAt:          finishedAt,
	}
}

func (s *Store) ExportAll(ctx context.Context) (SnapshotData, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SnapshotData{}, err
	}
	defer func() { _ = tx.Rollback() }()
	data, err := exportSnapshot(ctx, tx)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := tx.Commit(); err != nil {
		return SnapshotData{}, err
	}
	return data, nil
}

func exportSnapshot(ctx context.Context, db storedb.DBTX) (SnapshotData, error) {
	q := storedb.New(db)
	contacts, err := exportContacts(ctx, q)
	if err != nil {
		return SnapshotData{}, err
	}
	chats, err := exportChats(ctx, q)
	if err != nil {
		return SnapshotData{}, err
	}
	groups, err := exportGroups(ctx, q)
	if err != nil {
		return SnapshotData{}, err
	}
	participants, err := exportParticipants(ctx, q)
	if err != nil {
		return SnapshotData{}, err
	}
	query, args := messageListQuery(MessageFilter{Limit: int(^uint(0) >> 1), Asc: true, IncludeDeleted: true})
	messages, err := scanMessages(ctx, db, query, args...)
	if err != nil {
		return SnapshotData{}, err
	}
	revisions, err := exportMessageRevisions(ctx, db)
	if err != nil {
		return SnapshotData{}, err
	}
	sourceStoreIdentity, err := syncStateValue(ctx, db, "merge_source_store_identity")
	if err != nil {
		return SnapshotData{}, err
	}
	accountIdentity, err := syncStateValue(ctx, db, "merge_account_identity")
	if err != nil {
		return SnapshotData{}, err
	}
	if strings.HasPrefix(accountIdentity, "wa-store:") {
		if sourceStoreIdentity == "" {
			sourceStoreIdentity = accountIdentity
		}
		accountIdentity = ""
	}
	return SnapshotData{Contacts: contacts, Chats: chats, Groups: groups, Participants: participants, Messages: messages, Revisions: revisions, SourceStoreIdentity: sourceStoreIdentity, AccountIdentity: accountIdentity}, nil
}

func syncStateValue(ctx context.Context, db storedb.DBTX, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx, `select value from sync_state where key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) Contacts(ctx context.Context) ([]Contact, error) {
	contacts, err := exportContacts(ctx, s.q)
	if err != nil {
		return nil, err
	}
	live := contacts[:0]
	for _, contact := range contacts {
		if contact.DeletedAt.IsZero() {
			live = append(live, contact)
		}
	}
	return live, nil
}

func (s *Store) ImportSnapshot(ctx context.Context, data SnapshotData, sourcePath string, finishedAt time.Time) error {
	stats := data.ImportStats(sourcePath, s.Path(), finishedAt)
	stats.Mode = "restore"
	return s.importAll(ctx, true, stats, data.Contacts, data.Chats, data.Groups, data.Participants, data.Messages, data.Revisions)
}

func exportMessageRevisions(ctx context.Context, db storedb.DBTX) ([]MessageRevision, error) {
	rows, err := db.QueryContext(ctx, `select event_id,payload_json,recorded_at,event_source,reason from message_revisions order by id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MessageRevision
	for rows.Next() {
		var revision MessageRevision
		var recordedAt int64
		if err := rows.Scan(&revision.EventID, &revision.PayloadJSON, &recordedAt, &revision.EventSource, &revision.Reason); err != nil {
			return nil, err
		}
		revision.RecordedAt = fromUnix(recordedAt)
		out = append(out, revision)
	}
	return out, rows.Err()
}

func exportContacts(ctx context.Context, q *storedb.Queries) ([]Contact, error) {
	rows, err := q.ExportContacts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(rows))
	for _, row := range rows {
		out = append(out, contactFromRow(row))
	}
	return out, nil
}

func exportChats(ctx context.Context, q *storedb.Queries) ([]Chat, error) {
	rows, err := q.ExportChats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Chat, 0, len(rows))
	for _, row := range rows {
		out = append(out, exportChatFromRow(row))
	}
	return out, nil
}

func exportGroups(ctx context.Context, q *storedb.Queries) ([]Group, error) {
	rows, err := q.ExportGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, groupFromRow(row))
	}
	return out, nil
}

func exportParticipants(ctx context.Context, q *storedb.Queries) ([]GroupParticipant, error) {
	rows, err := q.ExportParticipants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GroupParticipant, 0, len(rows))
	for _, row := range rows {
		out = append(out, participantFromRow(row))
	}
	return out, nil
}

func (d SnapshotData) Validate() error {
	if d.AccountIdentity != "" && !strings.HasPrefix(d.AccountIdentity, "wa-account:") {
		return errors.New("invalid WhatsApp account identity")
	}
	if d.SourceStoreIdentity != "" && !strings.HasPrefix(d.SourceStoreIdentity, "wa-store:") {
		return errors.New("invalid WhatsApp source-store identity")
	}
	seen := map[int64]struct{}{}
	events := map[string]struct{}{}
	for _, message := range d.Messages {
		if message.SourcePK == 0 {
			return fmt.Errorf("message with empty source_pk")
		}
		if _, ok := seen[message.SourcePK]; ok {
			return fmt.Errorf("duplicate message source_pk %d", message.SourcePK)
		}
		seen[message.SourcePK] = struct{}{}
		eventID := message.EventID
		if eventID == "" {
			eventID = messageEventID(message.SourcePK)
		}
		if _, ok := events[eventID]; ok {
			return fmt.Errorf("duplicate message event_id %q", eventID)
		}
		events[eventID] = struct{}{}
	}
	for _, revision := range d.Revisions {
		if _, ok := events[revision.EventID]; !ok {
			return fmt.Errorf("message revision references unknown event_id %q", revision.EventID)
		}
		if revision.PayloadJSON == "" || revision.EventSource == "" || revision.Reason == "" {
			return fmt.Errorf("message revision %q is incomplete", revision.EventID)
		}
	}
	return nil
}

func contactFromRow(row storedb.ExportContactsRow) Contact {
	return Contact{
		Tombstone:    Tombstone{DeletedAt: fromUnix(row.DeletedAt), DeletionSource: row.DeletionSource, DeletionReason: row.DeletionReason, LastSeenAt: fromUnix(row.LastSeenAt)},
		JID:          row.Jid,
		Phone:        row.Phone,
		FullName:     row.FullName,
		FirstName:    row.FirstName,
		LastName:     row.LastName,
		BusinessName: row.BusinessName,
		Username:     row.Username,
		LID:          row.Lid,
		AboutText:    row.AboutText,
		UpdatedAt:    fromUnix(row.UpdatedAt),
	}
}

func exportChatFromRow(row storedb.ExportChatsRow) Chat {
	return Chat{
		Tombstone:      Tombstone{DeletedAt: fromUnix(row.DeletedAt), DeletionSource: row.DeletionSource, DeletionReason: row.DeletionReason, LastSeenAt: fromUnix(row.LastSeenAt)},
		JID:            row.Jid,
		Kind:           row.Kind,
		Name:           row.Name,
		LastMessageAt:  fromUnix(row.LastMessageAt),
		UnreadCount:    int(row.UnreadCount),
		Archived:       row.Archived != 0,
		Removed:        row.Removed != 0,
		Hidden:         row.Hidden != 0,
		RawSessionType: int(row.RawSessionType),
	}
}

func groupFromRow(row storedb.ExportGroupsRow) Group {
	return Group{
		Tombstone: Tombstone{DeletedAt: fromUnix(row.DeletedAt), DeletionSource: row.DeletionSource, DeletionReason: row.DeletionReason, LastSeenAt: fromUnix(row.LastSeenAt)},
		JID:       row.Jid,
		Name:      row.Name,
		OwnerJID:  row.OwnerJid,
		CreatedAt: fromUnix(row.CreatedAt),
	}
}

func participantFromRow(row storedb.ExportParticipantsRow) GroupParticipant {
	return GroupParticipant{
		Tombstone:   Tombstone{DeletedAt: fromUnix(row.DeletedAt), DeletionSource: row.DeletionSource, DeletionReason: row.DeletionReason, LastSeenAt: fromUnix(row.LastSeenAt)},
		GroupJID:    row.GroupJid,
		UserJID:     row.UserJid,
		ContactName: row.ContactName,
		FirstName:   row.FirstName,
		IsAdmin:     row.IsAdmin != 0,
		IsActive:    row.IsActive != 0,
	}
}
