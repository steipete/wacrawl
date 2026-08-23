package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ckstore "github.com/openclaw/crawlkit/store"
	"github.com/openclaw/wacrawl/internal/sqlitedsn"
	"github.com/openclaw/wacrawl/internal/store/storedb"
	_ "modernc.org/sqlite"
)

const (
	schemaVersion           = 3
	whatsappReactionRawType = 14
	// Keep synthetic keys in [2^52, 2^53) so they remain distinguishable from
	// practical Core Data row IDs and exactly representable in web JSON.
	syntheticMessagePKBoundary = int64(1 << 52)
	maxJSONUnixSecond          = 253402300799 // 9999-12-31T23:59:59Z, the largest time.Time JSON can marshal.

	messageSelectColumns = `source_pk, source_row_pk, event_id, chat_jid, coalesce(chat_name,'') as chat_name, msg_id, coalesce(sender_jid,'') as sender_jid, coalesce(sender_name,'') as sender_name, ts, from_me, coalesce(text,'') as text, raw_type, coalesce(message_type,'') as message_type, coalesce(media_type,'') as media_type, coalesce(media_title,'') as media_title, coalesce(media_path,'') as media_path, coalesce(media_url,'') as media_url, coalesce(media_size,0) as media_size, starred, coalesce(deleted_at,0) as deleted_at, coalesce(deletion_source,'') as deletion_source, coalesce(deletion_reason,'') as deletion_reason, last_seen_at, '' as snippet`
	messageScanColumns   = `source_pk, source_row_pk, event_id, chat_jid, chat_name, msg_id, sender_jid, sender_name, ts, from_me, text, raw_type, message_type, media_type, media_title, media_path, media_url, media_size, starred, deleted_at, deletion_source, deletion_reason, last_seen_at, snippet`
)

type Store struct {
	db   *sql.DB
	q    *storedb.Queries
	path string
}

type ImportStats struct {
	Mode                string    `json:"mode"`
	SourceIdentity      string    `json:"-"`
	SourceStoreIdentity string    `json:"-"`
	AccountIdentity     string    `json:"-"`
	LegacyAccountIDs    []string  `json:"-"`
	AdoptSource         bool      `json:"-"`
	SourceSnapshotAt    time.Time `json:"-"`
	SourceNewestMessage time.Time `json:"-"`
	SourcePath          string    `json:"source_path"`
	DBPath              string    `json:"db_path"`
	Chats               int       `json:"chats"`
	Contacts            int       `json:"contacts"`
	Groups              int       `json:"groups"`
	Participants        int       `json:"participants"`
	Messages            int       `json:"messages"`
	MediaMessages       int       `json:"media_messages"`
	MediaCopied         int       `json:"media_copied,omitempty"`
	MediaMissing        int       `json:"media_missing,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
}

type Status struct {
	DBPath              string    `json:"db_path"`
	Chats               int       `json:"chats"`
	UnreadChats         int       `json:"unread_chats"`
	UnreadMessages      int       `json:"unread_messages"`
	Contacts            int       `json:"contacts"`
	Groups              int       `json:"groups"`
	Participants        int       `json:"participants"`
	Messages            int       `json:"messages"`
	MediaMessages       int       `json:"media_messages"`
	DeletedChats        int       `json:"deleted_chats"`
	DeletedContacts     int       `json:"deleted_contacts"`
	DeletedGroups       int       `json:"deleted_groups"`
	DeletedParticipants int       `json:"deleted_participants"`
	DeletedMessages     int       `json:"deleted_messages"`
	MessageRevisions    int       `json:"message_revisions"`
	OldestMessage       time.Time `json:"oldest_message,omitzero"`
	NewestMessage       time.Time `json:"newest_message,omitzero"`
	LastImportAt        time.Time `json:"last_import_at,omitzero"`
	LastSourceSnapshot  time.Time `json:"-"`
	LastSource          string    `json:"last_source,omitempty"`
	SourceRoot          string    `json:"-"`
	LastSourceMessages  int       `json:"last_source_messages,omitempty"`
	LastSourceContacts  int       `json:"last_source_contacts,omitempty"`
	SourceMessagesKnown bool      `json:"-"`
	SourceContactsKnown bool      `json:"-"`
	LastSourceNewest    time.Time `json:"-"`
	NewestObserved      time.Time `json:"-"`
}

type Tombstone struct {
	DeletedAt      time.Time `json:"deleted_at,omitzero"`
	DeletionSource string    `json:"deletion_source,omitempty"`
	DeletionReason string    `json:"deletion_reason,omitempty"`
	LastSeenAt     time.Time `json:"last_seen_at,omitzero"`
}

type Chat struct {
	Tombstone
	JID            string
	Kind           string
	Name           string
	LastMessageAt  time.Time
	UnreadCount    int
	Archived       bool
	Removed        bool
	Hidden         bool
	RawSessionType int
	MessageCount   int
}

type ChatFilter struct {
	Limit      int
	OnlyUnread bool
}

type Contact struct {
	Tombstone
	JID          string
	Phone        string
	FullName     string
	FirstName    string
	LastName     string
	BusinessName string
	Username     string
	LID          string
	AboutText    string
	UpdatedAt    time.Time
}

type Group struct {
	Tombstone
	JID       string
	Name      string
	OwnerJID  string
	CreatedAt time.Time
}

type GroupParticipant struct {
	Tombstone
	GroupJID    string
	UserJID     string
	ContactName string
	FirstName   string
	IsAdmin     bool
	IsActive    bool
}

type Message struct {
	Tombstone
	SourcePK       int64     `json:"source_pk"`
	SourceRowPK    int64     `json:"source_row_pk"`
	EventID        string    `json:"event_id"`
	ChatJID        string    `json:"chat_jid"`
	ChatName       string    `json:"chat_name,omitempty"`
	MessageID      string    `json:"message_id"`
	SenderJID      string    `json:"sender_jid,omitempty"`
	SenderName     string    `json:"sender_name,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
	FromMe         bool      `json:"from_me"`
	Text           string    `json:"text,omitempty"`
	RawType        int       `json:"raw_type"`
	MessageType    string    `json:"message_type,omitempty"`
	MediaType      string    `json:"media_type,omitempty"`
	MediaTitle     string    `json:"media_title,omitempty"`
	MediaPath      string    `json:"media_path,omitempty"`
	MediaURL       string    `json:"media_url,omitempty"`
	MediaSize      int64     `json:"media_size,omitempty"`
	Starred        bool      `json:"starred,omitempty"`
	Snippet        string    `json:"snippet,omitempty"`
	SourceTextNull bool      `json:"-"`
	storedUnix     int64
}

type MessageRevision struct {
	EventID     string    `json:"event_id"`
	PayloadJSON string    `json:"payload_json"`
	RecordedAt  time.Time `json:"recorded_at"`
	EventSource string    `json:"event_source"`
	Reason      string    `json:"reason"`
}

type MessageFilter struct {
	Query   string
	ChatJID string
	Sender  string
	Limit   int
	After   *time.Time
	Before  *time.Time
	// BeforePK tightens Before into a composite cursor: rows must have
	// ts < Before, or ts == Before with source_pk < BeforePK. Without it,
	// paging by timestamp alone can stall when a page boundary lands inside
	// a run of messages that share the same second.
	BeforePK       int64
	FromMe         *bool
	HasMedia       bool
	Asc            bool
	IncludeDeleted bool
	// SnippetStart and SnippetEnd wrap search matches inside snippets.
	// Both default to the CLI-friendly "[" and "]" markers.
	SnippetStart string
	SnippetEnd   string
}

func Open(ctx context.Context, path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("db path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}
	dsn, err := sqlitedsn.File(
		path,
		sqlitedsn.P("_pragma", "foreign_keys(1)"),
		sqlitedsn.P("_pragma", "journal_mode(WAL)"),
		sqlitedsn.P("_pragma", "synchronous(NORMAL)"),
		sqlitedsn.P("_pragma", "busy_timeout(5000)"),
	)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, q: storedb.New(db), path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "pragma user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this wacrawl build supports (%d)", current, schemaVersion)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	for _, table := range []string{"contacts", "chats", "groups", "group_participants", "messages"} {
		for _, column := range []struct{ name, definition string }{
			{"deleted_at", "integer"},
			{"deletion_source", "text"},
			{"deletion_reason", "text"},
			{"last_seen_at", "integer not null default 0"},
		} {
			if err := ensureColumn(ctx, tx, table, column.name, column.definition); err != nil {
				return err
			}
		}
	}
	if err := ensureColumn(ctx, tx, "messages", "event_id", "text"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, tx, "messages", "source_row_pk", "integer not null default 0"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update messages set source_row_pk = source_pk where source_row_pk = 0`); err != nil {
		return fmt.Errorf("backfill message source-row provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `update messages set event_id = printf('wa:%lld', source_pk) where event_id is null or trim(event_id) = ''`); err != nil {
		return fmt.Errorf("backfill message event identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `create unique index if not exists idx_messages_event_id on messages(event_id)`); err != nil {
		return fmt.Errorf("index message event identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
create trigger if not exists messages_event_id_required_insert
before insert on messages when new.event_id is null or trim(new.event_id) = ''
begin select raise(abort, 'messages.event_id is required'); end;
create trigger if not exists messages_event_id_required_update
before update of event_id on messages when new.event_id is null or trim(new.event_id) = ''
begin select raise(abort, 'messages.event_id is required'); end;`); err != nil {
		return fmt.Errorf("enforce message event identity: %w", err)
	}
	for _, table := range []string{"contacts", "chats", "groups", "group_participants", "messages"} {
		statement := fmt.Sprintf(`update %s set last_seen_at = coalesce((select max(updated_at) from sync_state), 0) where last_seen_at = 0`, table) // #nosec G201 -- table is from the fixed list above.
		if _, err := tx.ExecContext(ctx, statement); err != nil {                                                                                    //nolint:gosec // table is from the fixed list above.
			return fmt.Errorf("backfill %s last_seen_at: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("pragma user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.QueryContext(ctx, "pragma table_info("+table+")") //nolint:gosec // fixed migration identifiers only.
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "alter table "+table+" add column "+column+" "+definition); err != nil { //nolint:gosec // fixed migration identifiers only.
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) ReplaceAll(ctx context.Context, stats ImportStats, contacts []Contact, chats []Chat, groups []Group, participants []GroupParticipant, messages []Message) error {
	stats.Mode = "restore"
	return s.importAll(ctx, true, stats, contacts, chats, groups, participants, messages, nil)
}

func (s *Store) MergeAll(ctx context.Context, stats ImportStats, contacts []Contact, chats []Chat, groups []Group, participants []GroupParticipant, messages []Message) error {
	stats.Mode = "merge"
	return s.importAll(ctx, false, stats, contacts, chats, groups, participants, messages, nil)
}

func (s *Store) ValidateImport(ctx context.Context, stats ImportStats, messages []Message, restore bool) error {
	if err := validateImportMessages(messages); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer rollback(tx)
	messages, err = resolveReusedReactionIdentities(ctx, tx, restore, messages)
	if err != nil {
		return err
	}
	_, err = validateImportSource(ctx, tx, restore, stats, messages)
	return err
}

func (s *Store) importAll(ctx context.Context, restore bool, stats ImportStats, contacts []Contact, chats []Chat, groups []Group, participants []GroupParticipant, messages []Message, revisions []MessageRevision) error {
	if err := validateImportMessages(messages); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	messages, err = resolveReusedReactionIdentities(ctx, tx, restore, messages)
	if err != nil {
		return err
	}
	mergeSource, err := validateImportSource(ctx, tx, restore, stats, messages)
	if err != nil {
		return err
	}
	if restore {
		if _, err := tx.ExecContext(ctx, `
delete from messages_fts;
delete from message_revisions;
delete from messages;
delete from group_participants;
delete from groups;
delete from chats;
delete from contacts;
delete from sync_state;`); err != nil {
			return err
		}
	}
	now := stats.FinishedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	observedAt := unix(now)
	sourceSnapshotAt := stats.SourceSnapshotAt
	if sourceSnapshotAt.IsZero() {
		sourceSnapshotAt = now
	}
	prepareImportTombstones(now, chats, groups, participants, messages)
	if err := prepareStoredParentTombstones(ctx, tx, chats, groups, participants, messages); err != nil {
		return err
	}
	for _, c := range contacts {
		t := normalizedTombstone(c.Tombstone, now)
		if _, err := tx.ExecContext(ctx, `insert into contacts(
jid, phone, full_name, first_name, last_name, business_name, username, lid, about_text, updated_at,
deleted_at, deletion_source, deletion_reason, last_seen_at)
values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
on conflict(jid) do update set
phone=excluded.phone, full_name=excluded.full_name, first_name=excluded.first_name,
last_name=excluded.last_name, business_name=excluded.business_name, username=excluded.username,
lid=excluded.lid, about_text=excluded.about_text, updated_at=excluded.updated_at,
deleted_at=case when contacts.deleted_at is not null then contacts.deleted_at else excluded.deleted_at end,
deletion_source=case when contacts.deleted_at is not null then contacts.deletion_source else excluded.deletion_source end,
deletion_reason=case when contacts.deleted_at is not null then contacts.deletion_reason else excluded.deletion_reason end,
last_seen_at=excluded.last_seen_at`,
			c.JID, c.Phone, c.FullName, c.FirstName, c.LastName, c.BusinessName, c.Username, c.LID, c.AboutText, unix(c.UpdatedAt),
			nullableUnix(t.DeletedAt), nullableString(t.DeletionSource), nullableString(t.DeletionReason), unix(t.LastSeenAt)); err != nil {
			return err
		}
	}
	for _, c := range chats {
		t := normalizedTombstone(c.Tombstone, now)
		if _, err := tx.ExecContext(ctx, `insert into chats(
jid, kind, name, last_message_at, unread_count, archived, removed, hidden, raw_session_type,
deleted_at, deletion_source, deletion_reason, last_seen_at)
values(?,?,?,?,?,?,?,?,?,?,?,?,?)
on conflict(jid) do update set
kind=excluded.kind, name=excluded.name, last_message_at=excluded.last_message_at,
unread_count=excluded.unread_count, archived=excluded.archived, removed=excluded.removed,
hidden=excluded.hidden, raw_session_type=excluded.raw_session_type,
deleted_at=case when chats.deleted_at is not null and excluded.deleted_at is null and excluded.last_message_at > chats.deleted_at then null when chats.deleted_at is not null then chats.deleted_at else excluded.deleted_at end,
deletion_source=case when chats.deleted_at is not null and excluded.deleted_at is null and excluded.last_message_at > chats.deleted_at then null when chats.deleted_at is not null then chats.deletion_source else excluded.deletion_source end,
deletion_reason=case when chats.deleted_at is not null and excluded.deleted_at is null and excluded.last_message_at > chats.deleted_at then null when chats.deleted_at is not null then chats.deletion_reason else excluded.deletion_reason end,
last_seen_at=excluded.last_seen_at`,
			c.JID, c.Kind, c.Name, unix(c.LastMessageAt), c.UnreadCount, boolInt(c.Archived), boolInt(c.Removed), boolInt(c.Hidden), c.RawSessionType,
			nullableUnix(t.DeletedAt), nullableString(t.DeletionSource), nullableString(t.DeletionReason), unix(t.LastSeenAt)); err != nil {
			return err
		}
	}
	for _, g := range groups {
		t := normalizedTombstone(g.Tombstone, now)
		if _, err := tx.ExecContext(ctx, `insert into groups(
jid, name, owner_jid, created_at, deleted_at, deletion_source, deletion_reason, last_seen_at)
values(?,?,?,?,?,?,?,?)
on conflict(jid) do update set name=excluded.name, owner_jid=excluded.owner_jid,
created_at=excluded.created_at,
deleted_at=case when groups.deleted_at is not null and excluded.deleted_at is null and exists(select 1 from chats where jid=excluded.jid and deleted_at is null) then null when groups.deleted_at is not null then groups.deleted_at else excluded.deleted_at end,
deletion_source=case when groups.deleted_at is not null and excluded.deleted_at is null and exists(select 1 from chats where jid=excluded.jid and deleted_at is null) then null when groups.deleted_at is not null then groups.deletion_source else excluded.deletion_source end,
deletion_reason=case when groups.deleted_at is not null and excluded.deleted_at is null and exists(select 1 from chats where jid=excluded.jid and deleted_at is null) then null when groups.deleted_at is not null then groups.deletion_reason else excluded.deletion_reason end,
last_seen_at=excluded.last_seen_at`,
			g.JID, g.Name, g.OwnerJID, unix(g.CreatedAt), nullableUnix(t.DeletedAt), nullableString(t.DeletionSource), nullableString(t.DeletionReason), unix(t.LastSeenAt)); err != nil {
			return err
		}
	}
	for _, p := range participants {
		t := normalizedTombstone(p.Tombstone, now)
		if _, err := tx.ExecContext(ctx, `insert into group_participants(
group_jid, user_jid, contact_name, first_name, is_admin, is_active,
deleted_at, deletion_source, deletion_reason, last_seen_at)
values(?,?,?,?,?,?,?,?,?,?)
on conflict(group_jid,user_jid) do update set contact_name=excluded.contact_name,
first_name=excluded.first_name, is_admin=excluded.is_admin, is_active=excluded.is_active,
deleted_at=case when group_participants.deleted_at is not null and excluded.deleted_at is null and excluded.is_active=1 and exists(select 1 from groups where jid=excluded.group_jid and deleted_at is null) then null when group_participants.deleted_at is not null then group_participants.deleted_at else excluded.deleted_at end,
deletion_source=case when group_participants.deleted_at is not null and excluded.deleted_at is null and excluded.is_active=1 and exists(select 1 from groups where jid=excluded.group_jid and deleted_at is null) then null when group_participants.deleted_at is not null then group_participants.deletion_source else excluded.deletion_source end,
deletion_reason=case when group_participants.deleted_at is not null and excluded.deleted_at is null and excluded.is_active=1 and exists(select 1 from groups where jid=excluded.group_jid and deleted_at is null) then null when group_participants.deleted_at is not null then group_participants.deletion_reason else excluded.deletion_reason end,
last_seen_at=excluded.last_seen_at`,
			p.GroupJID, p.UserJID, p.ContactName, p.FirstName, boolInt(p.IsAdmin), boolInt(p.IsActive),
			nullableUnix(t.DeletedAt), nullableString(t.DeletionSource), nullableString(t.DeletionReason), unix(t.LastSeenAt)); err != nil {
			return err
		}
	}
	for _, m := range messages {
		if err := upsertMessage(ctx, tx, m, now); err != nil {
			return err
		}
	}
	for _, revision := range revisions {
		if _, err := tx.ExecContext(ctx, `insert into message_revisions(event_id,payload_json,recorded_at,event_source,reason) values(?,?,?,?,?)`,
			revision.EventID, revision.PayloadJSON, unix(revision.RecordedAt), revision.EventSource, revision.Reason); err != nil {
			return err
		}
	}
	if err := tombstoneSubordinates(ctx, tx, now); err != nil {
		return err
	}
	for key, value := range map[string]string{
		"last_import_at":        now.Format(time.RFC3339Nano),
		"source_path":           stats.SourcePath,
		"import_mode":           stats.Mode,
		"source_messages":       fmt.Sprintf("%d", stats.Messages),
		"source_contacts":       fmt.Sprintf("%d", stats.Contacts),
		"source_newest_message": formatSyncTime(stats.SourceNewestMessage),
		"source_snapshot_at":    formatSyncTime(sourceSnapshotAt),
	} {
		if _, err := tx.ExecContext(ctx, `insert into sync_state(key,value,updated_at) values(?,?,?)
on conflict(key) do update set value=excluded.value, updated_at=excluded.updated_at`, key, value, observedAt); err != nil {
			return err
		}
	}
	if strings.TrimSpace(mergeSource) != "" {
		if _, err := tx.ExecContext(ctx, `insert into sync_state(key,value,updated_at) values('merge_source_path',?,?)
on conflict(key) do update set value=excluded.value, updated_at=excluded.updated_at`, mergeSource, observedAt); err != nil {
			return err
		}
	}
	if strings.TrimSpace(stats.SourceStoreIdentity) != "" {
		if _, err := tx.ExecContext(ctx, `insert into sync_state(key,value,updated_at) values('merge_source_store_identity',?,?)
on conflict(key) do update set value=excluded.value, updated_at=excluded.updated_at`, strings.TrimSpace(stats.SourceStoreIdentity), observedAt); err != nil {
			return err
		}
	}
	if strings.TrimSpace(stats.AccountIdentity) != "" {
		if _, err := tx.ExecContext(ctx, `insert into sync_state(key,value,updated_at) values('merge_account_identity',?,?)
on conflict(key) do update set value=excluded.value, updated_at=excluded.updated_at`, strings.TrimSpace(stats.AccountIdentity), observedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validateImportSource(ctx context.Context, tx *sql.Tx, restore bool, stats ImportStats, messages []Message) (string, error) {
	strongSource := strings.TrimSpace(stats.SourceIdentity)
	weakSource := legacySourceIdentity(stats.SourcePath)
	if restore {
		return strongSource, nil
	}
	var existingStrong string
	err := tx.QueryRowContext(ctx, `select value from sync_state where key='merge_source_path'`).Scan(&existingStrong)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var existingStore string
	err = tx.QueryRowContext(ctx, `select value from sync_state where key='merge_source_store_identity'`).Scan(&existingStore)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if strings.HasPrefix(existingStrong, "wa-store:") {
		existingStore = existingStrong
		existingStrong = ""
	}
	var existingWeak string
	if existingStrong == "" {
		err = tx.QueryRowContext(ctx, `select value from sync_state where key='source_path'`).Scan(&existingWeak)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		existingWeak = legacySourceIdentity(existingWeak)
	}
	if strongSource != "" && existingStrong != "" && strongSource != existingStrong {
		return "", fmt.Errorf("archive is bound to WhatsApp source %q, not %q; use a separate --db or import --restore", existingStrong, strongSource)
	}
	incomingStore := strings.TrimSpace(stats.SourceStoreIdentity)
	if existingStore != "" && incomingStore != existingStore {
		return "", errors.New("archive is bound to a different WhatsApp Desktop store; use a separate --db or import --restore")
	}
	if existingStrong == "" && existingWeak != "" && weakSource != existingWeak {
		return "", fmt.Errorf("archive is bound to WhatsApp source path %q, not %q; use a separate --db or import --restore", existingWeak, weakSource)
	}
	matchingMessages := 0
	for _, message := range messages {
		existing, found, err := messageBySourcePK(ctx, tx, message.SourcePK)
		if err != nil {
			return "", err
		}
		if found && messageIdentityConflict(existing, message) {
			return "", fmt.Errorf("message source_pk %d belongs to a different event; use a separate archive or import --restore", message.SourcePK)
		}
		if found {
			matchingMessages++
		}
	}
	accountIdentity := strings.TrimSpace(stats.AccountIdentity)
	var existingAccount string
	err = tx.QueryRowContext(ctx, `select value from sync_state where key='merge_account_identity'`).Scan(&existingAccount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	existingAccount = strings.TrimSpace(existingAccount)
	if strings.HasPrefix(existingAccount, "wa-store:") {
		// Early schema-v2 builds mistook Core Data's persistent-store UUID for
		// an account identity. It is only a source marker and cannot prove that
		// a logout/login cycle kept the same WhatsApp account.
		existingAccount = ""
	}
	if existingAccount != "" {
		legacyMatch := false
		for _, candidate := range stats.LegacyAccountIDs {
			if strings.TrimSpace(candidate) == existingAccount {
				legacyMatch = true
				break
			}
		}
		if accountIdentity == "" || (existingAccount != accountIdentity && (!legacyMatch || matchingMessages == 0)) {
			return "", errors.New("archive is bound to a different WhatsApp account; use a separate --db or import --restore")
		}
	} else {
		entityRows, err := archiveEntityRows(ctx, tx)
		if err != nil {
			return "", err
		}
		if entityRows > 0 && (!stats.AdoptSource || accountIdentity == "") {
			return "", errors.New("archive has no verified WhatsApp account binding; rerun an explicit import with --adopt-source, use a separate --db, or import --restore")
		}
	}
	if strongSource != "" {
		return strongSource, nil
	}
	return existingStrong, nil
}

func archiveEntityRows(ctx context.Context, tx *sql.Tx) (int, error) {
	var entityRows int
	if err := tx.QueryRowContext(ctx, `select
(select count(*) from contacts)+(select count(*) from chats)+(select count(*) from groups)+
(select count(*) from group_participants)+(select count(*) from messages)`).Scan(&entityRows); err != nil {
		return 0, err
	}
	return entityRows, nil
}

func formatSyncTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func legacySourceIdentity(source string) string {
	source = strings.TrimSpace(source)
	if source == "" || strings.HasPrefix(source, "backup:") {
		return ""
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return filepath.Clean(source)
	}
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = evaluated
	}
	return filepath.Clean(absolute)
}

func validateImportMessages(messages []Message) error {
	seen := make(map[int64]struct{}, len(messages))
	seenEvents := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		if message.SourcePK == 0 {
			return errors.New("message with empty source_pk")
		}
		if _, ok := seen[message.SourcePK]; ok {
			return fmt.Errorf("duplicate message source_pk %d", message.SourcePK)
		}
		seen[message.SourcePK] = struct{}{}
		eventID := message.EventID
		if eventID == "" {
			eventID = messageEventID(message.SourcePK)
		} else if strings.TrimSpace(eventID) == "" {
			return fmt.Errorf("message source_pk %d has empty event_id", message.SourcePK)
		}
		if _, ok := seenEvents[eventID]; ok {
			return fmt.Errorf("duplicate message event_id %q", eventID)
		}
		seenEvents[eventID] = struct{}{}
	}
	return nil
}

func prepareImportTombstones(now time.Time, chats []Chat, groups []Group, participants []GroupParticipant, messages []Message) {
	deletedChats := make(map[string]Tombstone)
	for i := range chats {
		if chats[i].Removed && chats[i].DeletedAt.IsZero() {
			chats[i].Tombstone = sourceTombstone(now, "whatsapp_removed")
		}
		if !chats[i].DeletedAt.IsZero() {
			deletedChats[chats[i].JID] = chats[i].Tombstone
		}
	}
	deletedGroups := make(map[string]Tombstone)
	for i := range groups {
		if parent, ok := deletedChats[groups[i].JID]; ok && groups[i].DeletedAt.IsZero() {
			groups[i].Tombstone = childTombstone(parent, "parent_chat_deleted")
		}
		if !groups[i].DeletedAt.IsZero() {
			deletedGroups[groups[i].JID] = groups[i].Tombstone
		}
	}
	for i := range participants {
		if parent, ok := deletedGroups[participants[i].GroupJID]; ok && participants[i].DeletedAt.IsZero() {
			participants[i].Tombstone = childTombstone(parent, "parent_group_deleted")
		} else if !participants[i].IsActive && participants[i].DeletedAt.IsZero() {
			participants[i].Tombstone = sourceTombstone(now, "whatsapp_inactive")
		}
	}
	for i := range messages {
		if parent, ok := deletedChats[messages[i].ChatJID]; ok && messages[i].DeletedAt.IsZero() {
			messages[i].Tombstone = childTombstone(parent, "parent_chat_deleted")
		}
	}
}

func prepareStoredParentTombstones(ctx context.Context, tx *sql.Tx, chats []Chat, groups []Group, participants []GroupParticipant, messages []Message) error {
	liveChats := make(map[string]struct{})
	for _, chat := range chats {
		if chat.DeletedAt.IsZero() {
			liveChats[chat.JID] = struct{}{}
		}
	}
	for i := range groups {
		if !groups[i].DeletedAt.IsZero() {
			continue
		}
		if _, live := liveChats[groups[i].JID]; live {
			continue
		}
		parent, found, err := storedTombstone(ctx, tx, "chats", "jid", groups[i].JID)
		if err != nil {
			return err
		}
		if found {
			groups[i].Tombstone = childTombstone(parent, "parent_chat_deleted")
		}
	}
	liveGroups := make(map[string]struct{})
	for _, group := range groups {
		if group.DeletedAt.IsZero() {
			liveGroups[group.JID] = struct{}{}
		}
	}
	for i := range participants {
		if !participants[i].DeletedAt.IsZero() {
			continue
		}
		if _, live := liveGroups[participants[i].GroupJID]; live {
			continue
		}
		parent, found, err := storedTombstone(ctx, tx, "groups", "jid", participants[i].GroupJID)
		if err != nil {
			return err
		}
		if found {
			participants[i].Tombstone = childTombstone(parent, "parent_group_deleted")
		}
	}
	for i := range messages {
		if !messages[i].DeletedAt.IsZero() {
			continue
		}
		if _, live := liveChats[messages[i].ChatJID]; live {
			continue
		}
		parent, found, err := storedTombstone(ctx, tx, "chats", "jid", messages[i].ChatJID)
		if err != nil {
			return err
		}
		if found {
			messages[i].Tombstone = childTombstone(parent, "parent_chat_deleted")
		}
	}
	return nil
}

func storedTombstone(ctx context.Context, tx *sql.Tx, table, key, value string) (Tombstone, bool, error) {
	var deletedAt, lastSeenAt int64
	var source, reason string
	query := "select deleted_at,coalesce(deletion_source,''),coalesce(deletion_reason,''),last_seen_at from " + table + " where " + key + "=? and deleted_at is not null" //nolint:gosec // fixed internal table and key names only.
	err := tx.QueryRowContext(ctx, query, value).Scan(&deletedAt, &source, &reason, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Tombstone{}, false, nil
	}
	if err != nil {
		return Tombstone{}, false, err
	}
	return Tombstone{DeletedAt: fromUnix(deletedAt), DeletionSource: source, DeletionReason: reason, LastSeenAt: fromUnix(lastSeenAt)}, true, nil
}

func sourceTombstone(at time.Time, reason string) Tombstone {
	return Tombstone{DeletedAt: at, DeletionSource: "whatsapp-desktop", DeletionReason: reason, LastSeenAt: at}
}

func childTombstone(parent Tombstone, reason string) Tombstone {
	return Tombstone{DeletedAt: parent.DeletedAt, DeletionSource: parent.DeletionSource, DeletionReason: reason, LastSeenAt: parent.LastSeenAt}
}

func normalizedTombstone(t Tombstone, observedAt time.Time) Tombstone {
	if t.LastSeenAt.IsZero() {
		t.LastSeenAt = observedAt
	}
	if !t.DeletedAt.IsZero() {
		if t.DeletionSource == "" {
			t.DeletionSource = "snapshot"
		}
		if t.DeletionReason == "" {
			t.DeletionReason = "explicit_tombstone"
		}
	}
	return t
}

func messageEventID(sourcePK int64) string {
	return fmt.Sprintf("wa:%d", sourcePK)
}

func resolveReusedReactionIdentities(ctx context.Context, tx *sql.Tx, restore bool, messages []Message) ([]Message, error) {
	resolved := append([]Message(nil), messages...)
	for i := range resolved {
		message := &resolved[i]
		if message.SourceRowPK == 0 {
			message.SourceRowPK = message.SourcePK
		}
		if restore {
			continue
		}
		existing, found, err := messageBySourcePK(ctx, tx, message.SourcePK)
		if err != nil {
			return nil, err
		}
		if !found || !reactionReusesSourceRow(existing, *message) {
			continue
		}
		eventID, sourcePK := reusedReactionIdentity(*message)
		collision, collisionFound, err := messageBySourcePK(ctx, tx, sourcePK)
		if err != nil {
			return nil, err
		}
		if collisionFound && collision.EventID != eventID {
			return nil, fmt.Errorf("synthetic reaction source_pk %d belongs to a different event", sourcePK)
		}
		message.SourcePK = sourcePK
		message.EventID = eventID
	}
	if err := validateImportMessages(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func reactionReusesSourceRow(existing, incoming Message) bool {
	return incoming.RawType == whatsappReactionRawType &&
		(incoming.SourceTextNull || looksLikeActorJID(strings.TrimSpace(incoming.Text))) &&
		incoming.MessageID != "" &&
		incoming.MessageID != existing.MessageID &&
		incoming.MediaTitle == existing.MessageID &&
		incoming.ChatJID == existing.ChatJID
}

func looksLikeActorJID(value string) bool {
	local, found := strings.CutSuffix(value, "@lid")
	if !found {
		local, found = strings.CutSuffix(value, "@s.whatsapp.net")
	}
	if !found || local == "" {
		return false
	}
	for i := range len(local) {
		if local[i] < '0' || local[i] > '9' {
			return false
		}
	}
	return true
}

func reusedReactionIdentity(message Message) (string, int64) {
	seed := fmt.Sprintf("%d\x00%s\x00%s\x00%s", message.SourceRowPK, message.ChatJID, message.MessageID, message.MediaTitle)
	digest := sha256.Sum256([]byte(seed))
	eventID := fmt.Sprintf("wa-reaction:%d:%x", message.SourceRowPK, digest[:16])
	sourcePK := syntheticMessagePKBoundary | int64(binary.BigEndian.Uint64(digest[:8])&uint64(syntheticMessagePKBoundary-1))
	return eventID, sourcePK
}

func upsertMessage(ctx context.Context, tx *sql.Tx, m Message, observedAt time.Time) error {
	sourcePayloadCleared := m.SourceTextNull && m.RawType == 0 && m.MediaTitle == "" && m.MediaType == "" && m.MediaPath == "" && m.MediaURL == "" && m.DeletedAt.IsZero()
	preserveExistingFTS := false
	if m.EventID == "" {
		m.EventID = messageEventID(m.SourcePK)
	}
	if m.SourceRowPK == 0 {
		m.SourceRowPK = m.SourcePK
	}
	m.Tombstone = normalizedTombstone(m.Tombstone, observedAt)
	existing, found, err := messageBySourcePK(ctx, tx, m.SourcePK)
	if err != nil {
		return err
	}
	if found {
		if messageIdentityConflict(existing, m) {
			return fmt.Errorf("message source_pk %d belongs to a different event; use a separate archive or import --restore", m.SourcePK)
		}
		if !existing.DeletedAt.IsZero() {
			return nil
		}
		if sourcePayloadCleared && (!existing.DeletedAt.IsZero() || messageHasPayload(existing)) {
			m.Tombstone = sourceTombstone(observedAt, "whatsapp_payload_cleared")
			preserveExistingFTS = messageHasPayload(existing)
		}
		m.EventID = existing.EventID
		previous, err := canonicalMessageJSON(existing)
		if err != nil {
			return err
		}
		incoming, err := canonicalMessageJSON(m)
		if err != nil {
			return err
		}
		if previous != incoming {
			reason := "whatsapp_edit"
			if !m.DeletedAt.IsZero() && existing.DeletedAt.IsZero() {
				reason = m.DeletionReason
			}
			if _, err := tx.ExecContext(ctx, `insert into message_revisions(event_id,payload_json,recorded_at,event_source,reason) values(?,?,?,?,?)`,
				existing.EventID, previous, unix(observedAt), "whatsapp-desktop", reason); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `insert into messages(
source_pk,source_row_pk,event_id,chat_jid,chat_name,msg_id,sender_jid,sender_name,ts,from_me,text,
raw_type,message_type,media_type,media_title,media_path,media_url,media_size,starred,
deleted_at,deletion_source,deletion_reason,last_seen_at)
values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
on conflict(source_pk) do update set source_row_pk=excluded.source_row_pk, event_id=excluded.event_id, chat_jid=excluded.chat_jid,
chat_name=excluded.chat_name, msg_id=excluded.msg_id, sender_jid=excluded.sender_jid,
sender_name=excluded.sender_name, ts=excluded.ts, from_me=excluded.from_me, text=excluded.text,
raw_type=excluded.raw_type, message_type=excluded.message_type, media_type=excluded.media_type,
media_title=excluded.media_title, media_path=excluded.media_path, media_url=excluded.media_url,
media_size=excluded.media_size, starred=excluded.starred, deleted_at=excluded.deleted_at,
deletion_source=excluded.deletion_source, deletion_reason=excluded.deletion_reason,
last_seen_at=excluded.last_seen_at`,
		m.SourcePK, m.SourceRowPK, m.EventID, m.ChatJID, m.ChatName, m.MessageID, m.SenderJID, m.SenderName, messageUnix(m), boolInt(m.FromMe), m.Text,
		m.RawType, m.MessageType, m.MediaType, m.MediaTitle, m.MediaPath, m.MediaURL, m.MediaSize, boolInt(m.Starred),
		nullableUnix(m.DeletedAt), nullableString(m.DeletionSource), nullableString(m.DeletionReason), unix(m.LastSeenAt)); err != nil {
		return err
	}
	if !preserveExistingFTS {
		if _, err := tx.ExecContext(ctx, `delete from messages_fts where rowid=(select rowid from messages where source_pk=?)`, m.SourcePK); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into messages_fts(rowid,text,chat,sender,media)
values((select rowid from messages where source_pk=?),?,?,?,?)`, m.SourcePK,
			strings.TrimSpace(m.Text+" "+m.MediaTitle), m.ChatName, m.SenderName, m.MediaType); err != nil {
			return err
		}
	}
	return nil
}

func messageHasPayload(message Message) bool {
	return strings.TrimSpace(message.Text) != "" || strings.TrimSpace(message.MediaTitle) != "" || message.MediaType != "" || message.MediaPath != "" || message.MediaURL != ""
}

func messageIdentityConflict(existing, incoming Message) bool {
	return existing.ChatJID != incoming.ChatJID || existing.MessageID != incoming.MessageID || existing.FromMe != incoming.FromMe || messageUnix(existing) != messageUnix(incoming)
}

func tombstoneSubordinates(ctx context.Context, tx *sql.Tx, observedAt time.Time) error {
	rows, err := tx.QueryContext(ctx, `select `+messageSelectColumns+` from messages m
where m.deleted_at is null and exists (select 1 from chats c where c.jid=m.chat_jid and c.deleted_at is not null)`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var messages []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return err
		}
		messages = append(messages, m)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range messages {
		var deletedAt int64
		var source string
		if err := tx.QueryRowContext(ctx, `select deleted_at,coalesce(deletion_source,'whatsapp-desktop') from chats where jid=?`, m.ChatJID).Scan(&deletedAt, &source); err != nil {
			return err
		}
		m.Tombstone = Tombstone{DeletedAt: fromUnix(deletedAt), DeletionSource: source, DeletionReason: "parent_chat_deleted", LastSeenAt: observedAt}
		if err := upsertMessage(ctx, tx, m, observedAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `update groups set
deleted_at=coalesce(deleted_at,(select deleted_at from chats where chats.jid=groups.jid)),
deletion_source=coalesce(deletion_source,(select deletion_source from chats where chats.jid=groups.jid),'whatsapp-desktop'),
deletion_reason=coalesce(deletion_reason,'parent_chat_deleted')
where deleted_at is null and exists(select 1 from chats where chats.jid=groups.jid and chats.deleted_at is not null)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update group_participants set
deleted_at=coalesce(deleted_at,(select deleted_at from groups where groups.jid=group_participants.group_jid)),
deletion_source=coalesce(deletion_source,(select deletion_source from groups where groups.jid=group_participants.group_jid),'whatsapp-desktop'),
deletion_reason=coalesce(deletion_reason,'parent_group_deleted')
where deleted_at is null and exists(select 1 from groups where groups.jid=group_participants.group_jid and groups.deleted_at is not null)`); err != nil {
		return err
	}
	return nil
}

func canonicalMessageJSON(m Message) (string, error) {
	payload := struct {
		SourcePK       int64  `json:"source_pk"`
		SourceRowPK    int64  `json:"source_row_pk"`
		EventID        string `json:"event_id"`
		ChatJID        string `json:"chat_jid"`
		ChatName       string `json:"chat_name,omitempty"`
		MessageID      string `json:"message_id"`
		SenderJID      string `json:"sender_jid,omitempty"`
		SenderName     string `json:"sender_name,omitempty"`
		Timestamp      int64  `json:"timestamp_unix"`
		FromMe         bool   `json:"from_me"`
		Text           string `json:"text,omitempty"`
		RawType        int    `json:"raw_type"`
		MessageType    string `json:"message_type,omitempty"`
		MediaType      string `json:"media_type,omitempty"`
		MediaTitle     string `json:"media_title,omitempty"`
		MediaPath      string `json:"media_path,omitempty"`
		MediaURL       string `json:"media_url,omitempty"`
		MediaSize      int64  `json:"media_size,omitempty"`
		Starred        bool   `json:"starred,omitempty"`
		DeletedAt      int64  `json:"deleted_at,omitempty"`
		DeletionSource string `json:"deletion_source,omitempty"`
		DeletionReason string `json:"deletion_reason,omitempty"`
	}{
		SourcePK: m.SourcePK, SourceRowPK: m.SourceRowPK, EventID: m.EventID, ChatJID: m.ChatJID, ChatName: m.ChatName,
		MessageID: m.MessageID, SenderJID: m.SenderJID, SenderName: m.SenderName,
		Timestamp: messageUnix(m), FromMe: m.FromMe, Text: m.Text, RawType: m.RawType,
		MessageType: m.MessageType, MediaType: m.MediaType, MediaTitle: m.MediaTitle,
		MediaPath: m.MediaPath, MediaURL: m.MediaURL, MediaSize: m.MediaSize, Starred: m.Starred,
		DeletedAt: unix(m.DeletedAt), DeletionSource: m.DeletionSource, DeletionReason: m.DeletionReason,
	}
	data, err := json.Marshal(payload)
	return string(data), err
}

func messageUnix(message Message) int64 {
	if message.storedUnix != 0 || message.Timestamp.IsZero() {
		return message.storedUnix
	}
	return unix(message.Timestamp)
}

func nullableUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return unix(t)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	out := Status{DBPath: s.path}
	var err error
	if out.Chats, err = countInt(ctx, s.q.CountChats); err != nil {
		return out, err
	}
	if out.UnreadChats, err = countInt(ctx, s.q.CountUnreadChats); err != nil {
		return out, err
	}
	if out.UnreadMessages, err = countInt(ctx, s.q.CountUnreadMessages); err != nil {
		return out, err
	}
	if out.Contacts, err = countInt(ctx, s.q.CountContacts); err != nil {
		return out, err
	}
	if out.Groups, err = countInt(ctx, s.q.CountGroups); err != nil {
		return out, err
	}
	if out.Participants, err = countInt(ctx, s.q.CountParticipants); err != nil {
		return out, err
	}
	if out.Messages, err = countInt(ctx, s.q.CountMessages); err != nil {
		return out, err
	}
	if out.MediaMessages, err = countInt(ctx, s.q.CountMediaMessages); err != nil {
		return out, err
	}
	for query, destination := range map[string]*int{
		`select count(*) from chats where deleted_at is not null`:              &out.DeletedChats,
		`select count(*) from contacts where deleted_at is not null`:           &out.DeletedContacts,
		`select count(*) from groups where deleted_at is not null`:             &out.DeletedGroups,
		`select count(*) from group_participants where deleted_at is not null`: &out.DeletedParticipants,
		`select count(*) from messages where deleted_at is not null`:           &out.DeletedMessages,
		`select count(*) from message_revisions`:                               &out.MessageRevisions,
	} {
		if err := s.db.QueryRowContext(ctx, query).Scan(destination); err != nil {
			return out, err
		}
	}
	bounds, err := s.q.GetMessageTimeBounds(ctx)
	if err != nil {
		return out, err
	}
	out.OldestMessage = fromUnix(bounds.OldestTs)
	out.NewestMessage = fromUnix(bounds.NewestTs)
	var newestObserved int64
	if err := s.db.QueryRowContext(ctx, `select cast(coalesce(max(case when ts > 0 and ts <= 253402300799 then ts end),0) as integer) from messages`).Scan(&newestObserved); err != nil {
		return out, err
	}
	out.NewestObserved = fromUnix(newestObserved)
	lastImport, _ := s.q.GetSyncState(ctx, "last_import_at")
	if t, err := time.Parse(time.RFC3339Nano, lastImport); err == nil {
		out.LastImportAt = t
	}
	if value, err := s.q.GetSyncState(ctx, "source_snapshot_at"); err == nil {
		out.LastSourceSnapshot, _ = time.Parse(time.RFC3339Nano, value)
	}
	out.LastSource, _ = s.q.GetSyncState(ctx, "source_path")
	out.SourceRoot, _ = s.q.GetSyncState(ctx, "merge_source_path")
	if out.SourceRoot == "" {
		out.SourceRoot = out.LastSource
	}
	if value, err := s.q.GetSyncState(ctx, "source_messages"); err == nil {
		if out.LastSourceMessages, err = strconv.Atoi(value); err == nil {
			out.SourceMessagesKnown = true
		}
	}
	if value, err := s.q.GetSyncState(ctx, "source_contacts"); err == nil {
		if out.LastSourceContacts, err = strconv.Atoi(value); err == nil {
			out.SourceContactsKnown = true
		}
	}
	if value, err := s.q.GetSyncState(ctx, "source_newest_message"); err == nil {
		out.LastSourceNewest, _ = time.Parse(time.RFC3339Nano, value)
	}
	return out, nil
}

func (s *Store) ListChats(ctx context.Context, limit int) ([]Chat, error) {
	return s.listChats(ctx, ChatFilter{Limit: limit})
}

func (s *Store) ListUnreadChats(ctx context.Context, limit int) ([]Chat, error) {
	return s.listChats(ctx, ChatFilter{Limit: limit, OnlyUnread: true})
}

func (s *Store) listChats(ctx context.Context, filter ChatFilter) ([]Chat, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.OnlyUnread {
		rows, err := s.q.ListUnreadChats(ctx, int64(filter.Limit))
		if err != nil {
			return nil, err
		}
		out := make([]Chat, 0, len(rows))
		for _, row := range rows {
			out = append(out, unreadChatFromRow(row))
		}
		return out, nil
	}
	rows, err := s.q.ListChats(ctx, int64(filter.Limit))
	if err != nil {
		return nil, err
	}
	out := make([]Chat, 0, len(rows))
	for _, row := range rows {
		out = append(out, chatFromRow(row))
	}
	return out, nil
}

func (s *Store) Messages(ctx context.Context, filter MessageFilter) ([]Message, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	query, args := messageListQuery(filter)
	return scanMessages(ctx, s.db, query, args...)
}

// MessageBySourcePK returns the single message stored under the given source
// primary key, or sql.ErrNoRows when it does not exist.
func (s *Store) MessageBySourcePK(ctx context.Context, sourcePK int64) (Message, error) {
	messages, err := scanMessages(ctx, s.db, "select "+messageSelectColumns+" from messages where source_pk = ? and deleted_at is null", sourcePK)
	if err != nil {
		return Message{}, err
	}
	if len(messages) == 0 {
		return Message{}, sql.ErrNoRows
	}
	return messages[0], nil
}

func messageListQuery(filter MessageFilter) (string, []any) {
	validQuery, validArgs := filteredMessagesQuery(filter, "")
	validQuery += " and " + validUnixPredicate("ts")
	if filter.After != nil || filter.Before != nil {
		if filter.Asc {
			validQuery += " order by ts asc, source_pk asc limit ?"
		} else {
			validQuery += " order by ts desc, source_pk desc limit ?"
		}
		return validQuery, append(validArgs, filter.Limit)
	}

	if filter.Asc {
		validQuery, validArgs = filteredMessagesQuery(filter, ", 1 as sort_bucket, ts as sort_ts")
		validQuery += " and " + validUnixPredicate("ts")
		invalidQuery, invalidArgs := filteredMessagesQuery(filter, ", 0 as sort_bucket, 0 as sort_ts")
		invalidQuery += " and " + invalidUnixPredicate("ts")
		query := "select " + messageScanColumns + " from (select * from (" + invalidQuery + " order by source_pk asc limit ?) union all select * from (" + validQuery + " order by ts asc, source_pk asc limit ?)) order by sort_bucket asc, sort_ts asc, source_pk asc limit ?"
		args := append([]any{}, invalidArgs...)
		args = append(args, filter.Limit)
		args = append(args, validArgs...)
		args = append(args, filter.Limit, filter.Limit)
		return query, args
	}

	validQuery, validArgs = filteredMessagesQuery(filter, ", 0 as sort_bucket, ts as sort_ts")
	validQuery += " and " + validUnixPredicate("ts")
	invalidQuery, invalidArgs := filteredMessagesQuery(filter, ", 1 as sort_bucket, 0 as sort_ts")
	invalidQuery += " and " + invalidUnixPredicate("ts")
	query := "select " + messageScanColumns + " from (select * from (" + validQuery + " order by ts desc, source_pk desc limit ?) union all select * from (" + invalidQuery + " order by source_pk desc limit ?)) order by sort_bucket asc, sort_ts desc, source_pk desc limit ?"
	args := append([]any{}, validArgs...)
	args = append(args, filter.Limit)
	args = append(args, invalidArgs...)
	args = append(args, filter.Limit, filter.Limit)
	return query, args
}

func filteredMessagesQuery(filter MessageFilter, extraColumns string) (string, []any) {
	query := "select " + messageSelectColumns + extraColumns + " from messages where 1=1"
	return applyMessageFilters(query, nil, filter, false)
}

func (s *Store) Search(ctx context.Context, filter MessageFilter) ([]Message, error) {
	if strings.TrimSpace(filter.Query) == "" {
		return nil, errors.New("search query required")
	}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	ftsQuery, err := ckstore.FTS5Terms(filter.Query, "")
	if err != nil {
		return nil, err
	}
	snippetStart := filter.SnippetStart
	if snippetStart == "" {
		snippetStart = "["
	}
	snippetEnd := filter.SnippetEnd
	if snippetEnd == "" {
		snippetEnd = "]"
	}
	query := `select m.source_pk, m.source_row_pk, m.event_id, m.chat_jid, coalesce(m.chat_name,''), m.msg_id, coalesce(m.sender_jid,''), coalesce(m.sender_name,''), m.ts, m.from_me, coalesce(m.text,''), m.raw_type, coalesce(m.message_type,''), coalesce(m.media_type,''), coalesce(m.media_title,''), coalesce(m.media_path,''), coalesce(m.media_url,''), coalesce(m.media_size,0), m.starred, coalesce(m.deleted_at,0), coalesce(m.deletion_source,''), coalesce(m.deletion_reason,''), m.last_seen_at, snippet(messages_fts, 0, ?, ?, '...', 12) from messages_fts f join messages m on m.rowid=f.rowid where messages_fts match ?`
	args := []any{snippetStart, snippetEnd, ftsQuery}
	query, args = applyMessageFilters(query, args, filter, true)
	query += " order by bm25(messages_fts) limit ?"
	args = append(args, filter.Limit)
	return scanMessages(ctx, s.db, query, args...)
}

func applyMessageFilters(query string, args []any, filter MessageFilter, joined bool) (string, []any) {
	prefix := ""
	if joined {
		prefix = "m."
	}
	if !filter.IncludeDeleted {
		query += " and " + prefix + "deleted_at is null"
	}
	if strings.TrimSpace(filter.ChatJID) != "" {
		query += " and " + prefix + "chat_jid = ?"
		args = append(args, filter.ChatJID)
	}
	if strings.TrimSpace(filter.Sender) != "" {
		query += " and " + prefix + "sender_jid = ?"
		args = append(args, filter.Sender)
	}
	if filter.After != nil {
		query += " and " + prefix + "ts >= ?"
		args = append(args, unix(*filter.After))
	}
	if filter.Before != nil {
		if filter.BeforePK > 0 {
			query += " and (" + prefix + "ts < ? or (" + prefix + "ts = ? and " + prefix + "source_pk < ?))"
			args = append(args, unix(*filter.Before), unix(*filter.Before), filter.BeforePK)
		} else {
			query += " and " + prefix + "ts <= ?"
			args = append(args, unix(*filter.Before))
		}
	}
	if filter.FromMe != nil {
		query += " and " + prefix + "from_me = ?"
		args = append(args, boolInt(*filter.FromMe))
	}
	if filter.HasMedia {
		query += " and (" + prefix + "media_type <> '' or " + prefix + "media_path <> '' or " + prefix + "media_url <> '')"
	}
	return query, args
}

func scanMessages(ctx context.Context, db *sql.DB, query string, args ...any) ([]Message, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type messageScanner interface {
	Scan(dest ...any) error
}

func scanMessage(row messageScanner) (Message, error) {
	var m Message
	var ts, deletedAt, lastSeenAt int64
	var fromMe, starred int
	if err := row.Scan(&m.SourcePK, &m.SourceRowPK, &m.EventID, &m.ChatJID, &m.ChatName, &m.MessageID, &m.SenderJID, &m.SenderName,
		&ts, &fromMe, &m.Text, &m.RawType, &m.MessageType, &m.MediaType, &m.MediaTitle, &m.MediaPath, &m.MediaURL,
		&m.MediaSize, &starred, &deletedAt, &m.DeletionSource, &m.DeletionReason, &lastSeenAt, &m.Snippet); err != nil {
		return Message{}, err
	}
	m.Timestamp = fromUnix(ts)
	m.storedUnix = ts
	m.FromMe = fromMe != 0
	m.Starred = starred != 0
	m.DeletedAt = fromUnix(deletedAt)
	m.LastSeenAt = fromUnix(lastSeenAt)
	return m, nil
}

func messageBySourcePK(ctx context.Context, tx *sql.Tx, sourcePK int64) (Message, bool, error) {
	row := tx.QueryRowContext(ctx, "select "+messageSelectColumns+" from messages where source_pk = ?", sourcePK)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	return m, err == nil, err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}

func fromUnix(v int64) time.Time {
	if !validUnixTimestamp(v) {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func validUnixTimestamp(v int64) bool {
	return v > 0 && v <= maxJSONUnixSecond
}

func validUnixPredicate(column string) string {
	return fmt.Sprintf("%s > 0 and %s <= %d", column, column, maxJSONUnixSecond)
}

func invalidUnixPredicate(column string) string {
	return fmt.Sprintf("(%s <= 0 or %s > %d)", column, column, maxJSONUnixSecond)
}

func countInt(ctx context.Context, count func(context.Context) (int64, error)) (int, error) {
	v, err := count(ctx)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

func chatFromRow(row storedb.ListChatsRow) Chat {
	return Chat{
		JID:            row.Jid,
		Kind:           row.Kind,
		Name:           row.Name,
		LastMessageAt:  fromUnix(row.LastMessageAt),
		UnreadCount:    int(row.UnreadCount),
		Archived:       row.Archived != 0,
		Removed:        row.Removed != 0,
		Hidden:         row.Hidden != 0,
		RawSessionType: int(row.RawSessionType),
		MessageCount:   int(row.MessageCount),
	}
}

func unreadChatFromRow(row storedb.ListUnreadChatsRow) Chat {
	return Chat{
		JID:            row.Jid,
		Kind:           row.Kind,
		Name:           row.Name,
		LastMessageAt:  fromUnix(row.LastMessageAt),
		UnreadCount:    int(row.UnreadCount),
		Archived:       row.Archived != 0,
		Removed:        row.Removed != 0,
		Hidden:         row.Hidden != 0,
		RawSessionType: int(row.RawSessionType),
		MessageCount:   int(row.MessageCount),
	}
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}
