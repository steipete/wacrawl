package whatsappdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/cache"
	"github.com/openclaw/wacrawl/internal/mediafile"
	"github.com/openclaw/wacrawl/internal/sqlitedsn"
	"github.com/openclaw/wacrawl/internal/store"
	_ "modernc.org/sqlite"
)

const (
	chatDBName                  = "ChatStorage.sqlite"
	contactsDBName              = "ContactsV2.sqlite"
	axolotlDBName               = "Axolotl.sqlite"
	axolotlBeforeDBName         = "Axolotl.before.sqlite"
	appleEpoch                  = 978307200
	maxJSONUnixSecond           = 253402300799
	maxJSONAppleSecondExclusive = maxJSONUnixSecond - appleEpoch + 1
)

type Source struct {
	Path             string   `json:"path"`
	Available        bool     `json:"available"`
	ChatDB           string   `json:"chat_db,omitempty"`
	ContactsDB       string   `json:"contacts_db,omitempty"`
	MediaDir         string   `json:"media_dir,omitempty"`
	SupportingDBs    []string `json:"supporting_dbs,omitempty"`
	MessageRows      int      `json:"message_rows,omitempty"`
	MessageRowsKnown bool     `json:"-"`
	ChatRows         int      `json:"chat_rows,omitempty"`
	ContactRows      int      `json:"contact_rows,omitempty"`
	ContactRowsKnown bool     `json:"-"`
	MediaRows        int      `json:"media_rows,omitempty"`
	OldestMessage    string   `json:"oldest_message,omitempty"`
	NewestMessage    string   `json:"newest_message,omitempty"`
	SchemaNotes      []string `json:"schema_notes,omitempty"`
}

type Snapshot struct {
	Root       string
	SourcePath string
}

type Data struct {
	Contacts            []store.Contact
	Chats               []store.Chat
	Groups              []store.Group
	Participants        []store.GroupParticipant
	Messages            []store.Message
	MediaCount          int
	SourceStoreIdentity string
	AccountIdentity     string
	LegacyAccountIDs    []string
}

type ImportOptions struct {
	SourcePath  string
	CopyMedia   bool
	MediaRoot   string
	Restore     bool
	AdoptSource bool
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared")
	}
	return ""
}

func Discover(ctx context.Context, path string) (Source, error) {
	path = defaultedPath(path)
	source := Source{Path: path}
	if path == "" {
		return source, errors.New("WhatsApp desktop path is only auto-detected on macOS")
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return source, nil
		}
		return source, err
	}
	if !info.IsDir() {
		return source, fmt.Errorf("desktop path is not a directory: %s", path)
	}
	source.Available = true
	source.ChatDB = filepath.Join(path, chatDBName)
	source.ContactsDB = filepath.Join(path, contactsDBName)
	source.MediaDir = filepath.Join(path, "Message", "Media")
	for _, rel := range []string{
		"Axolotl.sqlite",
		"LID.sqlite",
		"LocalKeyValue.sqlite",
		"BackedUpKeyValue.sqlite",
		filepath.Join("fts", "ChatSearchV5f.sqlite"),
	} {
		full := filepath.Join(path, rel)
		if _, err := os.Stat(full); err == nil {
			source.SupportingDBs = append(source.SupportingDBs, rel)
		}
	}
	chatDB, closeChat, err := openReadOnly(source.ChatDB)
	if err == nil {
		defer closeChat()
		if err := chatDB.QueryRowContext(ctx, "select count(*) from ZWAMESSAGE").Scan(&source.MessageRows); err == nil {
			source.MessageRowsKnown = true
		}
		_ = chatDB.QueryRowContext(ctx, "select count(*) from ZWACHATSESSION").Scan(&source.ChatRows)
		_ = chatDB.QueryRowContext(ctx, "select count(*) from ZWAMEDIAITEM").Scan(&source.MediaRows)
		var minDate, maxDate sql.NullFloat64
		_ = chatDB.QueryRowContext(ctx, "select min(ZMESSAGEDATE), max(ZMESSAGEDATE) from ZWAMESSAGE").Scan(&minDate, &maxDate)
		if minDate.Valid {
			source.OldestMessage = appleTime(minDate.Float64).Format(time.RFC3339)
		}
		if maxDate.Valid {
			source.NewestMessage = appleTime(maxDate.Float64).Format(time.RFC3339)
		}
		source.SchemaNotes = append(
			source.SchemaNotes,
			"CoreData tables: ZWACHATSESSION, ZWAMESSAGE, ZWAMEDIAITEM, ZWAGROUPINFO, ZWAGROUPMEMBER",
			"timestamps are seconds since 2001-01-01 UTC",
			"ZWAMESSAGE.ZGROUPMEMBER identifies group senders",
			"ZWAMEDIAITEM joins both via ZWAMESSAGE.ZMEDIAITEM and ZWAMEDIAITEM.ZMESSAGE",
		)
	}
	contactsDB, closeContacts, err := openReadOnly(source.ContactsDB)
	if err == nil {
		defer closeContacts()
		if err := contactsDB.QueryRowContext(ctx, "select count(*) from ZWAADDRESSBOOKCONTACT").Scan(&source.ContactRows); err == nil {
			source.ContactRowsKnown = true
		}
	}
	return source, nil
}

func SnapshotPath(path string) (Snapshot, error) {
	path = defaultedPath(path)
	if path == "" {
		return Snapshot{}, errors.New("desktop path is required")
	}
	root, err := os.MkdirTemp("", "wacrawl-desktop-*")
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := cache.SnapshotSQLite(cache.SQLiteSnapshotOptions{SourcePath: filepath.Join(path, axolotlDBName), DestinationDir: root, Name: axolotlBeforeDBName}); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(root)
		return Snapshot{}, err
	}
	if _, err := cache.SnapshotSQLite(cache.SQLiteSnapshotOptions{SourcePath: filepath.Join(path, chatDBName), DestinationDir: root, Name: chatDBName}); err != nil {
		_ = os.RemoveAll(root)
		return Snapshot{}, err
	}
	if _, err := cache.SnapshotSQLite(cache.SQLiteSnapshotOptions{SourcePath: filepath.Join(path, contactsDBName), DestinationDir: root, Name: contactsDBName}); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(root)
		return Snapshot{}, err
	}
	if _, err := cache.SnapshotSQLite(cache.SQLiteSnapshotOptions{SourcePath: filepath.Join(path, axolotlDBName), DestinationDir: root, Name: axolotlDBName}); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(root)
		return Snapshot{}, err
	}
	return Snapshot{Root: root, SourcePath: path}, nil
}

func Extract(ctx context.Context, snap Snapshot) (Data, error) {
	contacts, names, err := readContacts(ctx, filepath.Join(snap.Root, contactsDBName))
	if err != nil {
		return Data{}, err
	}
	chats, groups, participants, messages, mediaCount, err := readChats(ctx, filepath.Join(snap.Root, chatDBName), snap.SourcePath, names)
	if err != nil {
		return Data{}, err
	}
	sourceStoreIdentity, err := readSourceIdentity(ctx, filepath.Join(snap.Root, chatDBName))
	if err != nil {
		return Data{}, err
	}
	chatDBPath := filepath.Join(snap.Root, chatDBName)
	accountIdentity, legacyAccountIDs, err := readAccountIdentity(ctx, filepath.Join(snap.Root, axolotlDBName), chatDBPath)
	if err != nil {
		return Data{}, err
	}
	accountIdentityBefore, legacyAccountIDsBefore, err := readAccountIdentity(ctx, filepath.Join(snap.Root, axolotlBeforeDBName), chatDBPath)
	if err != nil {
		return Data{}, err
	}
	if accountIdentity != accountIdentityBefore {
		return Data{}, errors.New("WhatsApp account changed while the Desktop snapshot was being captured; retry the import")
	}
	legacyAccountIDs = sharedIdentities(legacyAccountIDs, legacyAccountIDsBefore)
	return Data{Contacts: contacts, Chats: chats, Groups: groups, Participants: participants, Messages: messages, MediaCount: mediaCount, SourceStoreIdentity: sourceStoreIdentity, AccountIdentity: accountIdentity, LegacyAccountIDs: legacyAccountIDs}, nil
}

func readSourceIdentity(ctx context.Context, chatDBPath string) (string, error) {
	db, closeDB, err := openReadOnly(chatDBPath)
	if err != nil {
		return "", err
	}
	defer closeDB()
	var metadataTables int
	if err := db.QueryRowContext(ctx, `select count(*) from sqlite_master where type='table' and name='Z_METADATA'`).Scan(&metadataTables); err != nil {
		return "", err
	}
	if metadataTables == 0 {
		return "", nil
	}
	var storeUUID string
	if err := db.QueryRowContext(ctx, `select coalesce(Z_UUID,'') from Z_METADATA limit 1`).Scan(&storeUUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read WhatsApp store identity: %w", err)
	}
	storeUUID = strings.TrimSpace(storeUUID)
	if storeUUID == "" {
		return "", nil
	}
	fingerprint := sha256.Sum256([]byte("chat-store\x00" + storeUUID))
	return fmt.Sprintf("wa-store:%x", fingerprint), nil
}

func readAccountIdentity(ctx context.Context, axolotlDBPath, chatDBPath string) (string, []string, error) {
	var metadataIDs map[string]struct{}
	legacyIDs := make(map[string]struct{})
	if _, err := os.Stat(axolotlDBPath); err == nil {
		db, closeDB, err := openReadOnly(axolotlDBPath)
		if err != nil {
			return "", nil, err
		}
		defer closeDB()

		metadataIDs, err = accountIDs(ctx, db, "ZWAZMDACCOUNT", "ZUSERJIDSTRING")
		if err != nil {
			return "", nil, err
		}
		if len(metadataIDs) == 0 {
			metadataIDs, err = accountIDs(ctx, db, "ZWAZMDACCOUNT", "ZACCOUNTJIDSTRING")
			if err != nil {
				return "", nil, err
			}
		}
		for _, table := range []string{"ZWAAXOLOTLIDENTITY", "ZWAAXOLOTLSESSION", "ZWASENDERKEY"} {
			ids, err := accountIDs(ctx, db, table, "ZACCOUNTJIDSTRING")
			if err != nil {
				return "", nil, err
			}
			for id := range ids {
				legacyIDs[id] = struct{}{}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}

	messageIDs, err := incomingMessageAccountIDs(ctx, chatDBPath)
	if err != nil {
		return "", nil, err
	}
	metadataIdentity, err := accountFingerprint(metadataIDs)
	if err != nil {
		return "", nil, err
	}
	messageIdentity, err := accountFingerprint(messageIDs)
	if err != nil {
		return "", nil, err
	}
	if metadataIdentity != "" && messageIdentity != "" && metadataIdentity != messageIdentity {
		return "", nil, errors.New("WhatsApp account metadata does not match the imported message store")
	}
	if metadataIdentity != "" {
		return metadataIdentity, accountFingerprints(legacyIDs), nil
	}
	return messageIdentity, accountFingerprints(legacyIDs), nil
}

func accountFingerprints(ids map[string]struct{}) []string {
	fingerprints := make([]string, 0, len(ids))
	for id := range ids {
		fingerprint := sha256.Sum256([]byte("account-jid\x00" + id))
		fingerprints = append(fingerprints, fmt.Sprintf("wa-account:%x", fingerprint))
	}
	sort.Strings(fingerprints)
	return fingerprints
}

func sharedIdentities(current, before []string) []string {
	beforeSet := make(map[string]struct{}, len(before))
	for _, identity := range before {
		beforeSet[identity] = struct{}{}
	}
	shared := make([]string, 0, len(current))
	for _, identity := range current {
		if _, ok := beforeSet[identity]; ok {
			shared = append(shared, identity)
		}
	}
	return shared
}

func incomingMessageAccountIDs(ctx context.Context, chatDBPath string) (map[string]struct{}, error) {
	ids := make(map[string]struct{})
	if _, err := os.Stat(chatDBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ids, nil
		}
		return nil, err
	}
	db, closeDB, err := openReadOnly(chatDBPath)
	if err != nil {
		return nil, err
	}
	defer closeDB()

	for _, column := range []string{"ZISFROMME", "ZTOJID"} {
		var present int
		if err := db.QueryRowContext(ctx, `select count(*) from pragma_table_info('ZWAMESSAGE') where name=?`, column).Scan(&present); err != nil {
			return nil, err
		}
		if present == 0 {
			return ids, nil
		}
	}
	if err := collectAccountIDs(ctx, db, `
select coalesce(ZTOJID,'')
from ZWAMESSAGE
where coalesce(ZISFROMME,0)=0
  and trim(coalesce(ZTOJID,''))<>''`, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func accountIDs(ctx context.Context, db *sql.DB, table string, columns ...string) (map[string]struct{}, error) {
	ids := make(map[string]struct{})
	var tables int
	if err := db.QueryRowContext(ctx, `select count(*) from sqlite_master where type='table' and name=?`, table).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		return ids, nil
	}
	for _, column := range columns {
		var present int
		if err := db.QueryRowContext(ctx, `select count(*) from pragma_table_info(?) where name=?`, table, column).Scan(&present); err != nil {
			return nil, err
		}
		if present == 0 {
			continue
		}
		query := fmt.Sprintf("select coalesce(%s,'') from %s", column, table) //nolint:gosec // names come from fixed internal lists.
		if err := collectAccountIDs(ctx, db, query, ids); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func collectAccountIDs(ctx context.Context, db *sql.DB, query string, ids map[string]struct{}) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		id = strings.ToLower(strings.TrimSpace(id))
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	return rows.Err()
}

func accountFingerprint(ids map[string]struct{}) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) != 1 {
		return "", errors.New("source contains multiple WhatsApp account identities and cannot be imported safely")
	}
	var id string
	for id = range ids {
	}
	fingerprint := sha256.Sum256([]byte("account-jid\x00" + id))
	return fmt.Sprintf("wa-account:%x", fingerprint), nil
}

func Import(ctx context.Context, st *store.Store, path string) (store.ImportStats, error) {
	return ImportWithOptions(ctx, st, ImportOptions{SourcePath: path})
}

func ImportWithOptions(ctx context.Context, st *store.Store, opts ImportOptions) (store.ImportStats, error) {
	sourcePath := defaultedPath(opts.SourcePath)
	sourceIdentity, err := canonicalSourcePath(sourcePath)
	if err != nil {
		return store.ImportStats{}, err
	}
	stats := store.ImportStats{SourcePath: sourcePath, SourceIdentity: sourceIdentity, AdoptSource: opts.AdoptSource, DBPath: st.Path(), StartedAt: time.Now().UTC()}
	sourcePath = sourceIdentity
	stats.SourceSnapshotAt = stats.StartedAt
	snap, err := SnapshotPath(sourcePath)
	if err != nil {
		return stats, err
	}
	defer func() { _ = os.RemoveAll(snap.Root) }()
	data, err := Extract(ctx, snap)
	if err != nil {
		return stats, err
	}
	stats.SourceStoreIdentity = data.SourceStoreIdentity
	stats.AccountIdentity = data.AccountIdentity
	stats.LegacyAccountIDs = data.LegacyAccountIDs
	stats.Chats = len(data.Chats)
	stats.Contacts = len(data.Contacts)
	stats.Groups = len(data.Groups)
	stats.Participants = len(data.Participants)
	stats.Messages = len(data.Messages)
	for _, message := range data.Messages {
		if message.Timestamp.After(stats.SourceNewestMessage) {
			stats.SourceNewestMessage = message.Timestamp
		}
	}
	stats.MediaMessages = data.MediaCount
	stats.FinishedAt = time.Now().UTC()
	stats.Mode = "merge"
	if opts.Restore {
		stats.Mode = "restore"
	}
	if err := st.ValidateImport(ctx, stats, data.Messages, opts.Restore); err != nil {
		return stats, err
	}
	mediaRoot := opts.MediaRoot
	if strings.TrimSpace(mediaRoot) == "" {
		mediaRoot = filepath.Join(filepath.Dir(st.Path()), "media")
	}
	mediaRoot, err = prepareMediaRoot(sourcePath, mediaRoot)
	if err != nil {
		if opts.CopyMedia {
			return stats, err
		}
		mediaRoot = ""
	}
	archivePath, err := mediafile.Resolve(st.Path())
	if err != nil {
		return stats, err
	}
	if mediaRoot != "" && containsMediaPath(mediaRoot, archivePath) {
		if opts.CopyMedia {
			return stats, errors.New("archive media root overlaps the archive database")
		}
		mediaRoot = ""
	}
	stats.MediaRoot = mediaRoot
	if opts.CopyMedia {
		copied, missing, err := copyArchiveMedia(data.Messages, sourcePath, mediaRoot)
		if err != nil {
			return stats, err
		}
		stats.MediaCopied = copied
		stats.MediaMissing = missing
	}
	importArchive := st.MergeAll
	if opts.Restore {
		importArchive = st.ReplaceAll
	}
	if err := importArchive(ctx, stats, data.Contacts, data.Chats, data.Groups, data.Participants, data.Messages); err != nil {
		return stats, err
	}
	return stats, nil
}

func canonicalSourcePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	absolute, err := mediafile.Resolve(path)
	if err != nil {
		return "", fmt.Errorf("resolve desktop path: %w", err)
	}
	return filepath.Abs(absolute)
}

func copyArchiveMedia(messages []store.Message, sourceRoot, mediaRoot string) (int, int, error) {
	var err error
	mediaRoot, err = prepareMediaRoot(sourceRoot, mediaRoot)
	if err != nil {
		return 0, 0, err
	}
	type result struct {
		path    string
		missing bool
	}
	seen := map[string]result{}
	copied := 0
	missing := 0
	for i := range messages {
		src := strings.TrimSpace(messages[i].MediaPath)
		if src == "" {
			continue
		}
		if r, ok := seen[src]; ok {
			if !r.missing {
				messages[i].MediaPath = r.path
			}
			continue
		}
		root, err := sourceMediaRoot(sourceRoot, src)
		if err != nil {
			return copied, missing, err
		}
		dest, err := copyMediaFile(root, src, mediaRoot)
		if err != nil {
			if errors.Is(err, errMediaMissing) || errors.Is(err, errMediaNotDownloaded) {
				missing++
				seen[src] = result{missing: true}
				continue
			}
			return copied, missing, err
		}
		copied++
		seen[src] = result{path: dest}
		messages[i].MediaPath = dest
	}
	return copied, missing, nil
}

// mediaMaterialized reports whether a media file's bytes are already on disk.
// Tests replace this to simulate macOS SF_DATALESS (iCloud) stubs.
var (
	mediaMaterialized    = fileMaterialized
	openMediaFileForCopy = mediafile.Open
)

var (
	errMediaNotDownloaded = errors.New("media not downloaded locally")
	errMediaMissing       = errors.New("source media is missing")
)

func normalizeMediaReadError(src string, err error) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", errMediaMissing, src)
	}
	if mediaReadWouldBlock(err) {
		return fmt.Errorf("%w: %s", errMediaNotDownloaded, src)
	}
	return err
}

func readContacts(ctx context.Context, path string) ([]store.Contact, map[string]string, error) {
	db, closeFn, err := openReadOnly(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, map[string]string{}, nil
		}
		return nil, nil, err
	}
	defer closeFn()
	rows, err := db.QueryContext(ctx, `select coalesce(ZWHATSAPPID,''), coalesce(ZPHONENUMBER,''), coalesce(ZFULLNAME,''), coalesce(ZGIVENNAME,''), coalesce(ZLASTNAME,''), coalesce(ZBUSINESSNAME,''), coalesce(ZUSERNAME,''), coalesce(ZLID,''), coalesce(ZABOUTTEXT,''), ZLASTUPDATED from ZWAADDRESSBOOKCONTACT`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var contacts []store.Contact
	names := map[string]string{}
	for rows.Next() {
		var c store.Contact
		var updated sql.NullFloat64
		if err := rows.Scan(&c.JID, &c.Phone, &c.FullName, &c.FirstName, &c.LastName, &c.BusinessName, &c.Username, &c.LID, &c.AboutText, &updated); err != nil {
			return nil, nil, err
		}
		if c.JID == "" {
			continue
		}
		c.UpdatedAt = appleNullTime(updated)
		contacts = append(contacts, c)
		name := bestName(c.FullName, c.BusinessName, c.Username, c.FirstName, c.Phone, c.JID)
		names[c.JID] = name
		if c.LID != "" {
			names[c.LID] = name
			names[c.LID+"@lid"] = name
		}
	}
	sort.Slice(contacts, func(i, j int) bool { return contacts[i].JID < contacts[j].JID })
	return contacts, names, rows.Err()
}

func readChats(ctx context.Context, path, sourceRoot string, names map[string]string) ([]store.Chat, []store.Group, []store.GroupParticipant, []store.Message, int, error) {
	db, closeFn, err := openReadOnly(path)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	defer closeFn()
	profileNames, err := readProfilePushNameRows(ctx, db)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	mergeMissingNames(names, profileNames)
	chats, err := readChatRows(ctx, db)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	groups, err := readGroupRows(ctx, db)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	participants, err := readParticipantRows(ctx, db)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	messages, mediaCount, err := readMessageRows(ctx, db, sourceRoot, names)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	return chats, groups, participants, messages, mediaCount, nil
}

func readProfilePushNameRows(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `select coalesce(ZJID,''), coalesce(ZPUSHNAME,'') from ZWAPROFILEPUSHNAME`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: ZWAPROFILEPUSHNAME") {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := map[string]string{}
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}
		if jid != "" && name != "" {
			names[jid] = name
		}
	}
	return names, rows.Err()
}

func mergeMissingNames(dst, src map[string]string) {
	for jid, name := range src {
		if strings.TrimSpace(dst[jid]) == "" {
			dst[jid] = name
		}
	}
}

func readChatRows(ctx context.Context, db *sql.DB) ([]store.Chat, error) {
	rows, err := db.QueryContext(ctx, `select coalesce(c.ZCONTACTJID,''), coalesce(c.ZPARTNERNAME,''), c.ZLASTMESSAGEDATE, coalesce(c.ZUNREADCOUNT,0), coalesce(c.ZARCHIVED,0), coalesce(c.ZREMOVED,0), coalesce(c.ZHIDDEN,0), coalesce(c.ZSESSIONTYPE,0), count(m.Z_PK) from ZWACHATSESSION c left join ZWAMESSAGE m on m.ZCHATSESSION=c.Z_PK group by c.Z_PK`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	merged := map[string]store.Chat{}
	for rows.Next() {
		var c store.Chat
		var last sql.NullFloat64
		var archived, removed, hidden int
		if err := rows.Scan(&c.JID, &c.Name, &last, &c.UnreadCount, &archived, &removed, &hidden, &c.RawSessionType, &c.MessageCount); err != nil {
			return nil, err
		}
		if c.JID == "" {
			continue
		}
		c.Kind = chatKind(c.JID, c.RawSessionType)
		c.LastMessageAt = appleNullTime(last)
		c.Archived = archived != 0
		c.Removed = removed != 0
		c.Hidden = hidden != 0
		if existing, ok := merged[c.JID]; ok {
			merged[c.JID] = mergeChatRows(existing, c)
			continue
		}
		merged[c.JID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]store.Chat, 0, len(merged))
	for _, chat := range merged {
		out = append(out, chat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMessageAt.After(out[j].LastMessageAt) })
	return out, nil
}

func mergeChatRows(existing, candidate store.Chat) store.Chat {
	merged := existing
	older := candidate
	if merged.LastMessageAt.Before(candidate.LastMessageAt) {
		merged = candidate
		older = existing
	}
	merged.MessageCount += older.MessageCount
	if merged.Name == "" {
		merged.Name = older.Name
	}
	if merged.RawSessionType == 0 && older.RawSessionType != 0 {
		merged.RawSessionType = older.RawSessionType
		merged.Kind = chatKind(merged.JID, merged.RawSessionType)
	}
	return merged
}

func readGroupRows(ctx context.Context, db *sql.DB) ([]store.Group, error) {
	rows, err := db.QueryContext(ctx, `select coalesce(c.ZCONTACTJID,''), coalesce(c.ZPARTNERNAME,''), coalesce(g.ZOWNERJID,''), g.ZCREATIONDATE from ZWAGROUPINFO g join ZWACHATSESSION c on c.Z_PK=g.ZCHATSESSION`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	merged := map[string]store.Group{}
	for rows.Next() {
		var g store.Group
		var created sql.NullFloat64
		if err := rows.Scan(&g.JID, &g.Name, &g.OwnerJID, &created); err != nil {
			return nil, err
		}
		if g.JID == "" {
			continue
		}
		g.CreatedAt = appleNullTime(created)
		if existing, ok := merged[g.JID]; ok {
			merged[g.JID] = mergeGroupRows(existing, g)
			continue
		}
		merged[g.JID] = g
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]store.Group, 0, len(merged))
	for _, group := range merged {
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JID < out[j].JID })
	return out, nil
}

func mergeGroupRows(existing, candidate store.Group) store.Group {
	merged := existing
	if merged.Name == "" {
		merged.Name = candidate.Name
	}
	if merged.OwnerJID == "" {
		merged.OwnerJID = candidate.OwnerJID
	}
	if merged.CreatedAt.IsZero() || (!candidate.CreatedAt.IsZero() && candidate.CreatedAt.Before(merged.CreatedAt)) {
		merged.CreatedAt = candidate.CreatedAt
	}
	return merged
}

func readParticipantRows(ctx context.Context, db *sql.DB) ([]store.GroupParticipant, error) {
	rows, err := db.QueryContext(ctx, `select coalesce(c.ZCONTACTJID,''), coalesce(gm.ZMEMBERJID,''), coalesce(gm.ZCONTACTNAME,''), coalesce(gm.ZFIRSTNAME,''), coalesce(gm.ZISADMIN,0), coalesce(gm.ZISACTIVE,0) from ZWAGROUPMEMBER gm join ZWACHATSESSION c on c.Z_PK=gm.ZCHATSESSION`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	merged := map[string]store.GroupParticipant{}
	for rows.Next() {
		var p store.GroupParticipant
		var admin, active int
		if err := rows.Scan(&p.GroupJID, &p.UserJID, &p.ContactName, &p.FirstName, &admin, &active); err != nil {
			return nil, err
		}
		if p.GroupJID == "" || p.UserJID == "" {
			continue
		}
		p.IsAdmin = admin != 0
		p.IsActive = active != 0
		key := p.GroupJID + "\x00" + p.UserJID
		if existing, ok := merged[key]; ok {
			merged[key] = mergeParticipantRows(existing, p)
			continue
		}
		merged[key] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]store.GroupParticipant, 0, len(merged))
	for _, participant := range merged {
		out = append(out, participant)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupJID == out[j].GroupJID {
			return out[i].UserJID < out[j].UserJID
		}
		return out[i].GroupJID < out[j].GroupJID
	})
	return out, nil
}

func mergeParticipantRows(existing, candidate store.GroupParticipant) store.GroupParticipant {
	merged := existing
	if merged.ContactName == "" {
		merged.ContactName = candidate.ContactName
	}
	if merged.FirstName == "" {
		merged.FirstName = candidate.FirstName
	}
	merged.IsAdmin = merged.IsAdmin || candidate.IsAdmin
	merged.IsActive = merged.IsActive || candidate.IsActive
	return merged
}

func readMessageRows(ctx context.Context, db *sql.DB, sourceRoot string, names map[string]string) ([]store.Message, int, error) {
	rows, err := db.QueryContext(ctx, `
select m.Z_PK, coalesce(c.ZCONTACTJID,''), coalesce(c.ZPARTNERNAME,''), coalesce(m.ZSTANZAID,''), coalesce(m.ZISFROMME,0), m.ZMESSAGEDATE,
       m.ZTEXT, coalesce(m.ZMESSAGETYPE,0), coalesce(m.ZSTARRED,0), coalesce(m.ZFROMJID,''), coalesce(m.ZTOJID,''), coalesce(m.ZPUSHNAME,''),
       coalesce(gm.ZMEMBERJID,''), coalesce(gm.ZCONTACTNAME,''), coalesce(gm.ZFIRSTNAME,''),
       coalesce(mi.ZMEDIALOCALPATH,''), coalesce(mi.ZMEDIAURL,''), coalesce(mi.ZTITLE,''), coalesce(mi.ZVCARDNAME,''), coalesce(mi.ZFILESIZE,0)
from ZWAMESSAGE m
left join ZWACHATSESSION c on c.Z_PK=m.ZCHATSESSION
left join ZWAGROUPMEMBER gm on gm.Z_PK=m.ZGROUPMEMBER
left join ZWAMEDIAITEM mi on mi.Z_PK=coalesce(nullif(m.ZMEDIAITEM, 0), (select mi2.Z_PK from ZWAMEDIAITEM mi2 where mi2.ZMESSAGE=m.Z_PK order by mi2.Z_PK limit 1))
order by m.ZMESSAGEDATE asc, m.Z_PK asc`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Message
	mediaCount := 0
	for rows.Next() {
		var m store.Message
		var msgDate sql.NullFloat64
		var text sql.NullString
		var fromMe, starred int
		var fromJID, toJID, pushName, memberJID, memberName, memberFirst, mediaPath, mediaURL, mediaTitle, vcardName string
		if err := rows.Scan(&m.SourcePK, &m.ChatJID, &m.ChatName, &m.MessageID, &fromMe, &msgDate, &text, &m.RawType, &starred, &fromJID, &toJID, &pushName, &memberJID, &memberName, &memberFirst, &mediaPath, &mediaURL, &mediaTitle, &vcardName, &m.MediaSize); err != nil {
			return nil, 0, err
		}
		m.SourceRowPK = m.SourcePK
		if m.ChatJID == "" || m.MessageID == "" {
			continue
		}
		m.Timestamp = appleNullTime(msgDate)
		m.Text = text.String
		m.SourceTextNull = !text.Valid
		m.FromMe = fromMe != 0
		m.Starred = starred != 0
		m.MessageType = messageType(m.RawType)
		m.MediaType = mediaType(m.RawType)
		m.MediaTitle = firstNonEmpty(mediaTitle, vcardName)
		if mediaPath != "" {
			m.MediaPath = resolveDesktopMediaPath(sourceRoot, mediaPath)
			if m.MediaPath == "" {
				return nil, 0, errors.New("source media path is outside Message/Media and Media")
			}
			root, err := sourceMediaRoot(sourceRoot, m.MediaPath)
			if err != nil {
				return nil, 0, err
			}
			if _, err := mediafile.Stat(root, m.MediaPath); err != nil && !os.IsNotExist(err) {
				return nil, 0, err
			}
		}
		m.MediaURL = mediaURL
		m.SenderJID, m.SenderName = sender(m.FromMe, m.ChatJID, fromJID, toJID, pushName, memberJID, memberName, memberFirst, names)
		if m.Text == "" && m.MediaTitle != "" {
			m.Text = m.MediaTitle
		}
		if m.MediaType != "" || m.MediaPath != "" || m.MediaURL != "" {
			mediaCount++
		}
		out = append(out, m)
	}
	return out, mediaCount, rows.Err()
}

func resolveDesktopMediaPath(sourceRoot, dbPath string) string {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return ""
	}
	rel := cleanDesktopMediaRel(filepath.FromSlash(dbPath))
	if rel == "" {
		return ""
	}
	primary := filepath.Join(sourceRoot, rel)
	if firstPathElement(rel) != "Media" {
		return primary
	}
	messageMedia := filepath.Join(sourceRoot, "Message", rel)
	if _, err := os.Stat(messageMedia); err == nil {
		return messageMedia
	}
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	return messageMedia
}

func cleanDesktopMediaRel(path string) string {
	rel := filepath.Clean(path)
	if filepath.IsAbs(path) {
		return ""
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return ""
		}
	}
	for _, root := range []string{"Media", filepath.Join("Message", "Media")} {
		if strings.HasPrefix(rel, root+string(filepath.Separator)) {
			return rel
		}
	}
	return ""
}

func firstPathElement(path string) string {
	if path == "." || path == "" {
		return ""
	}
	if i := strings.IndexRune(path, os.PathSeparator); i >= 0 {
		return path[:i]
	}
	return path
}

func sender(fromMe bool, chatJID, fromJID, toJID, pushName, memberJID, memberName, memberFirst string, names map[string]string) (string, string) {
	if fromMe {
		return firstNonEmpty(toJID), "me"
	}
	jid := firstNonEmpty(memberJID, fromJID, chatJID)
	name := firstNonEmpty(memberName, resolvedName(jid, names), memberFirst, pushName, jid)
	return jid, name
}

func resolvedName(jid string, names map[string]string) string {
	for _, key := range []string{jid, strings.TrimSuffix(jid, "@lid")} {
		if name := strings.TrimSpace(names[key]); name != "" && name != key && name != jid {
			return name
		}
	}
	return ""
}

func chatKind(jid string, raw int) string {
	switch {
	case strings.HasSuffix(jid, "@g.us"):
		return "group"
	case strings.Contains(jid, "@newsletter"):
		return "newsletter"
	case strings.Contains(jid, "@status") || jid == "status@broadcast":
		return "status"
	case raw == 3:
		return "status"
	default:
		return "dm"
	}
}

func messageType(raw int) string {
	switch raw {
	case 0:
		return "text"
	case 1:
		return "image"
	case 2:
		return "video"
	case 3:
		return "audio"
	case 4:
		return "location"
	case 5:
		return "contact"
	case 6:
		return "system"
	case 7:
		return "link"
	case 8:
		return "document"
	case 10:
		return "group_event"
	case 11:
		return "gif"
	case 14:
		return "reaction"
	case 15:
		return "sticker"
	default:
		return fmt.Sprintf("type_%d", raw)
	}
}

func mediaType(raw int) string {
	switch raw {
	case 1:
		return "image"
	case 2:
		return "video"
	case 3:
		return "audio"
	case 7:
		return "link"
	case 8:
		return "document"
	case 11:
		return "gif"
	case 15:
		return "sticker"
	default:
		return ""
	}
}

func defaultedPath(path string) string {
	if strings.TrimSpace(path) != "" {
		return path
	}
	return DefaultPath()
}

func openReadOnly(path string) (*sql.DB, func(), error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil, err
	}
	dsn, err := sqlitedsn.File(
		path,
		sqlitedsn.P("mode", "ro"),
		sqlitedsn.P("_pragma", "query_only(1)"),
		sqlitedsn.P("_pragma", "busy_timeout(5000)"),
		sqlitedsn.P("_pragma", "temp_store(MEMORY)"),
	)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, func() { _ = db.Close() }, nil
}

func appleNullTime(v sql.NullFloat64) time.Time {
	seconds := v.Float64
	if !v.Valid || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds >= maxJSONAppleSecondExclusive {
		return time.Time{}
	}
	return appleTime(seconds)
}

func appleTime(seconds float64) time.Time {
	whole := int64(seconds)
	nano := int64((seconds - float64(whole)) * 1e9)
	return time.Unix(whole+appleEpoch, nano).UTC()
}

func bestName(values ...string) string {
	return firstNonEmpty(values...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
