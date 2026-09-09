package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacrawl/internal/store"
	"github.com/openclaw/wacrawl/internal/whatsappdb"
	_ "modernc.org/sqlite"
)

func TestRunEndToEnd(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createDesktopFixture(t, source)
	dbPath := filepath.Join(t.TempDir(), "archive.db")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"help", []string{"--db", dbPath, "help"}, "wacrawl reads local WhatsApp"},
		{"version", []string{"--version"}, currentVersion()},
		{"metadata", []string{"--json", "metadata"}, `"id": "wacrawl"`},
		{"doctor", []string{"--db", dbPath, "--source", source, "doctor"}, "message_rows"},
		{"import", []string{"--db", dbPath, "--source", source, "import"}, "messages=3"},
		{"import copy media", []string{"--db", dbPath, "--source", source, "import", "--copy-media"}, "media_copied=1"},
		{"status", []string{"--db", dbPath, "status"}, "unread_messages=2"},
		{"chats", []string{"--db", dbPath, "chats", "--limit", "5"}, "UNREAD"},
		{"contacts export", []string{"--db", dbPath, "--json", "--sync", "never", "contacts", "export"}, `"display_name": "Alice Contact"`},
		{"chats unread", []string{"--db", dbPath, "chats", "--unread", "--limit", "5"}, "Launch Group"},
		{"unread", []string{"--db", dbPath, "unread", "--limit", "5"}, "Launch Group"},
		{"messages", []string{"--db", dbPath, "messages", "--chat", "123@g.us", "--asc"}, "launch now"},
		{"search", []string{"--db", dbPath, "search", "--limit", "5", "launch"}, "[launch] now"},
		{"search flags after query", []string{"--db", dbPath, "search", "launch", "--limit", "5"}, "[launch] now"},
		{"sql", []string{"--db", dbPath, "sql", "SELECT count(*) AS messages FROM messages"}, "messages"},
		{"json", []string{"--db", dbPath, "--json", "search", "launch"}, `"message_id"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := Run(ctx, tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("Run() error = %v stderr=%s", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("stdout missing %q:\n%s", tc.want, stdout.String())
			}
		})
	}
}

func TestRunImportMergesByDefaultAndRestoreReplaces(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createDesktopFixture(t, source)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "import", "--restore", "--adopt-source"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("restore/adopt conflict = %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "import"}, &stdout, &stderr); err != nil {
		t.Fatalf("initial import: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "mode=merge") {
		t.Fatalf("initial import mode: %s", stdout.String())
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	var sourceIdentity, sourceStoreIdentity, accountIdentity string
	if err := st.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_source_path'`).Scan(&sourceIdentity); err != nil {
		t.Fatal(err)
	}
	existing, err := st.MessageBySourcePK(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_account_identity'`).Scan(&accountIdentity); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_source_store_identity'`).Scan(&sourceStoreIdentity); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeAll(ctx, store.ImportStats{SourcePath: source, SourceIdentity: sourceIdentity, SourceStoreIdentity: sourceStoreIdentity, AccountIdentity: accountIdentity, FinishedAt: now, Messages: 2}, nil,
		[]store.Chat{{JID: "historical", Kind: "dm"}}, nil, nil,
		[]store.Message{existing, {SourcePK: 99, ChatJID: "historical", MessageID: "historical", Timestamp: now, Text: "retained history", RawType: 0}}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "sync"}, &stdout, &stderr); err != nil {
		t.Fatalf("merge sync: %v stderr=%s", err, stderr.String())
	}
	st, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if status.Messages != 4 {
		_ = st.Close()
		t.Fatalf("default merge messages = %d", status.Messages)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "import", "--restore"}, &stdout, &stderr); err != nil {
		t.Fatalf("restore import: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "mode=restore") {
		t.Fatalf("restore import mode: %s", stdout.String())
	}
	st, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	status, err = st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 3 {
		t.Fatalf("exact restore messages = %d", status.Messages)
	}
}

func TestRunImportAdoptsLegacyArchiveExplicitly(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createDesktopFixture(t, source)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	legacy := store.Message{SourcePK: 99, ChatJID: "legacy", MessageID: "legacy", Timestamp: now, Text: "retained legacy history", RawType: 0}
	if err := st.ReplaceAll(ctx, store.ImportStats{SourcePath: "backup:legacy", FinishedAt: now, Messages: 1}, nil,
		[]store.Chat{{JID: legacy.ChatJID, Kind: "dm"}}, nil, nil, []store.Message{legacy}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "import"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "--adopt-source") {
		t.Fatalf("unverified import error = %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "import", "--adopt-source"}, &stdout, &stderr); err != nil {
		t.Fatalf("adopt source: %v stderr=%s", err, stderr.String())
	}
	st, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.MessageBySourcePK(ctx, legacy.SourcePK); err != nil {
		t.Fatalf("adoption dropped legacy history: %v", err)
	}
	var accountBinding string
	if err := st.DB().QueryRowContext(ctx, `select value from sync_state where key='merge_account_identity'`).Scan(&accountBinding); err != nil || !strings.HasPrefix(accountBinding, "wa-account:") {
		t.Fatalf("account binding = %q, %v", accountBinding, err)
	}
}

func TestRunRelativeAndAbsolutePathContracts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	createDesktopFixture(t, source)
	t.Chdir(root)

	for _, tc := range []struct {
		name   string
		source string
		dbPath string
		dbFile string
	}{
		{name: "relative", source: "./source", dbPath: "./relative.db", dbFile: filepath.Join(root, "relative.db")},
		{name: "absolute", source: source, dbPath: filepath.Join(root, "absolute.db"), dbFile: filepath.Join(root, "absolute.db")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runJSON := func(args []string, target any) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				if err := Run(ctx, args, &stdout, &stderr); err != nil {
					t.Fatalf("Run(%q) error = %v stderr=%s", args, err, stderr.String())
				}
				if err := json.Unmarshal(stdout.Bytes(), target); err != nil {
					t.Fatalf("Run(%q) JSON = %s error = %v", args, stdout.String(), err)
				}
			}

			var doctor struct {
				Desktop whatsappdb.Source `json:"desktop"`
			}
			runJSON([]string{"--json", "--source", tc.source, "doctor"}, &doctor)
			if !doctor.Desktop.Available || doctor.Desktop.MessageRows != 3 || doctor.Desktop.ChatRows != 2 || doctor.Desktop.ContactRows != 2 {
				t.Fatalf("doctor = %+v", doctor.Desktop)
			}

			var imported store.ImportStats
			runJSON([]string{"--json", "--source", tc.source, "--db", tc.dbPath, "import"}, &imported)
			if imported.SourcePath != tc.source || imported.DBPath != tc.dbPath || imported.Messages != 3 {
				t.Fatalf("import = %+v", imported)
			}
			if _, err := os.Stat(tc.dbFile); err != nil {
				t.Fatalf("archive path = %q: %v", tc.dbFile, err)
			}

			var status store.Status
			runJSON([]string{"--json", "--db", tc.dbPath, "--sync", "never", "status"}, &status)
			if status.DBPath != tc.dbPath || status.Messages != 3 || status.Contacts != 2 {
				t.Fatalf("status = %+v", status)
			}

			var rows []struct {
				Messages int `json:"messages"`
			}
			runJSON([]string{"--json", "--db", tc.dbPath, "--sync", "never", "sql", "SELECT count(*) AS messages FROM messages"}, &rows)
			if len(rows) != 1 || rows[0].Messages != 3 {
				t.Fatalf("sql rows = %+v", rows)
			}
		})
	}
}

func TestRunSQLJSONAndReadOnlyValidation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAll(ctx, store.ImportStats{}, nil, []store.Chat{
		{JID: "123@g.us", Kind: "group", Name: "Launch Group", MessageCount: 2},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m1", Text: "launch now"},
		{SourcePK: 2, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m2", Text: "ship later"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"--db", dbPath, "--json", "sql", "SELECT chat_jid, count(*) AS messages FROM messages GROUP BY chat_jid"}, &stdout, &stderr); err != nil {
		t.Fatalf("sql json: %v stderr=%s", err, stderr.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("json = %s err=%v", stdout.String(), err)
	}
	if len(rows) != 1 || rows[0]["chat_jid"] != "123@g.us" || rows[0]["messages"] != float64(2) {
		t.Fatalf("rows = %#v", rows)
	}

	stdout.Reset()
	stderr.Reset()
	err = Run(ctx, []string{"--db", dbPath, "sql", "DELETE FROM messages"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), readOnlySelectError) {
		t.Fatalf("expected read-only select error, got %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	err = Run(ctx, []string{"--db", dbPath, "sql", "SELECT count(*) FROM messages; SELECT count(*) FROM chats"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "only a single read-only select statement is allowed") {
		t.Fatalf("expected single statement error, got %v", err)
	}

	invalidDBPath := filepath.Join(t.TempDir(), "archive.db")
	source := t.TempDir()
	createDesktopFixture(t, source)
	err = Run(ctx, []string{"--db", invalidDBPath, "--source", source, "--sync", "always", "sql", "DELETE FROM messages"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), readOnlySelectError) {
		t.Fatalf("expected read-only select error, got %v", err)
	}
	if _, statErr := os.Stat(invalidDBPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid SQL created archive: %v", statErr)
	}
}

func TestContactsExportUsesContractShapeAndSkipsUnsafeNames(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	contacts := []store.Contact{
		{JID: "safe@s.whatsapp.net", Phone: "+15550100", FullName: "Safe Person"},
		{JID: "safe-duplicate@s.whatsapp.net", Phone: "+15550100", FullName: "Safe Person"},
		{JID: "business@s.whatsapp.net", Phone: "+15550101", BusinessName: "Business Name"},
		{JID: "first-last@s.whatsapp.net", Phone: "+15550102", FirstName: "First", LastName: "Last"},
		{JID: "username@s.whatsapp.net", Phone: "+15550103", Username: "handle", FullName: "@handle"},
		{JID: "phone@s.whatsapp.net", Phone: "+15550104", FullName: "+15550104"},
		{JID: "jid@s.whatsapp.net", Phone: "+15550105", FullName: "jid@s.whatsapp.net"},
		{JID: "case-jid@s.whatsapp.net", Phone: "+15550107", FullName: "CASE-JID@S.WHATSAPP.NET"},
		{JID: "blank@s.whatsapp.net", Phone: "+15550106"},
		{JID: "missing-phone@s.whatsapp.net", FullName: "Missing Phone"},
	}
	if err := st.ReplaceAll(ctx, store.ImportStats{}, contacts, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"--db", dbPath, "--json", "--sync", "never", "contacts", "export"}, &stdout, &stderr); err != nil {
		t.Fatalf("contacts export: %v stderr=%s", err, stderr.String())
	}
	var payload struct {
		Contacts []struct {
			DisplayName  string   `json:"display_name"`
			PhoneNumbers []string `json:"phone_numbers"`
			JID          string   `json:"jid"`
			Username     string   `json:"username"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json = %s err=%v", stdout.String(), err)
	}
	assertContactExportKeys(t, stdout.Bytes())
	gotNames := make([]string, 0, len(payload.Contacts))
	for _, contact := range payload.Contacts {
		gotNames = append(gotNames, contact.DisplayName)
		if contact.JID != "" || contact.Username != "" {
			t.Fatalf("leaked source fields = %#v", contact)
		}
		if len(contact.PhoneNumbers) != 1 {
			t.Fatalf("bad phone numbers = %#v", contact)
		}
	}
	wantNames := []string{"Business Name", "First Last", "Safe Person"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("names = %#v, want %#v", gotNames, wantNames)
	}

	stdout.Reset()
	stderr.Reset()
	err = Run(ctx, []string{"--db", dbPath, "--source", filepath.Join(t.TempDir(), "missing"), "--sync", "always", "contacts", "export"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "source unavailable") {
		t.Fatalf("expected --sync always to fail without source, got %v", err)
	}
}

func assertContactExportKeys(t *testing.T, data []byte) {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	contactsJSON, ok := root["contacts"]
	if !ok || len(root) != 1 {
		t.Fatalf("root keys = %#v, want only contacts", root)
	}
	var contacts []map[string]json.RawMessage
	if err := json.Unmarshal(contactsJSON, &contacts); err != nil {
		t.Fatal(err)
	}
	for _, contact := range contacts {
		if _, ok := contact["display_name"]; !ok {
			t.Fatalf("contact keys = %#v, missing display_name", contact)
		}
		if _, ok := contact["phone_numbers"]; !ok {
			t.Fatalf("contact keys = %#v, missing phone_numbers", contact)
		}
		if len(contact) != 2 {
			t.Fatalf("contact keys = %#v, want only display_name and phone_numbers", contact)
		}
	}
}

func TestMetadataAdvertisesContactExport(t *testing.T) {
	manifest := controlManifest()
	webCommand, ok := manifest.Commands["web"]
	if !ok {
		t.Fatalf("commands = %#v", manifest.Commands)
	}
	if webCommand.Mutates || webCommand.JSON {
		t.Fatalf("web command = %#v", webCommand)
	}
	webWant := []string{"wacrawl", "--sync", "never", "web"}
	if !reflect.DeepEqual(webCommand.Argv, webWant) {
		t.Fatalf("web argv = %#v, want %#v", webCommand.Argv, webWant)
	}

	sqlCommand, ok := manifest.Commands["sql"]
	if !ok {
		t.Fatalf("commands = %#v", manifest.Commands)
	}
	if sqlCommand.Mutates || !sqlCommand.JSON {
		t.Fatalf("sql command = %#v", sqlCommand)
	}
	sqlWant := []string{"wacrawl", "--json", "--sync", "never", "sql"}
	if !reflect.DeepEqual(sqlCommand.Argv, sqlWant) {
		t.Fatalf("sql argv = %#v, want %#v", sqlCommand.Argv, sqlWant)
	}

	command, ok := manifest.Commands["contact-export"]
	if !ok {
		t.Fatalf("commands = %#v", manifest.Commands)
	}
	if command.Mutates || !command.JSON {
		t.Fatalf("contact-export command = %#v", command)
	}
	want := []string{"wacrawl", "--json", "--sync", "never", "contacts", "export"}
	if !reflect.DeepEqual(command.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", command.Argv, want)
	}
}

func TestRunUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("empty args should print help: %v", err)
	}
	err := Run(context.Background(), []string{"bogus"}, &stdout, &stderr)
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("expected usage exit, got err=%v code=%d", err, ExitCode(err))
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil exit code should be zero")
	}
	if ExitCode(errors.New("plain")) != 1 {
		t.Fatal("plain error exit code should be one")
	}

	err = Run(context.Background(), []string{"messages", "--from-me", "--from-them"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
	err = Run(context.Background(), []string{"messages", "--after", "nope"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "invalid time") {
		t.Fatalf("expected invalid time error, got %v", err)
	}
	err = Run(context.Background(), []string{"search"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "exactly one query") {
		t.Fatalf("expected query error, got %v", err)
	}
	err = Run(context.Background(), []string{"doctor", "extra"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "flags only") {
		t.Fatalf("expected doctor args error, got %v", err)
	}
	for _, args := range [][]string{
		{"import", "extra"},
		{"sync", "extra"},
		{"chats", "extra"},
		{"messages", "extra"},
		{"unread", "extra"},
		{"web", "extra"},
	} {
		err = Run(context.Background(), args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "flags only") {
			t.Fatalf("expected flags-only error for %v, got %v", args, err)
		}
	}
	err = Run(context.Background(), []string{"--db", "", "status"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "db path is required") {
		t.Fatalf("expected db path error, got %v", err)
	}
	err = Run(context.Background(), []string{"--sync", "sometimes", "status"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--sync must be one of") {
		t.Fatalf("expected sync mode error, got %v", err)
	}
	err = Run(context.Background(), []string{"status", "extra"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "flags only") {
		t.Fatalf("expected status args error, got %v", err)
	}
	err = Run(context.Background(), []string{"contacts"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "contacts supports export only") {
		t.Fatalf("expected contacts command error, got %v", err)
	}
	err = Run(context.Background(), []string{"contacts", "import"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "contacts supports export only") {
		t.Fatalf("expected contacts subcommand error, got %v", err)
	}
	err = Run(context.Background(), []string{"contacts", "export", "extra"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "contacts export takes no arguments") {
		t.Fatalf("expected contacts export args error, got %v", err)
	}
	err = Run(context.Background(), []string{"web", "--port", "-1"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--port must be between") {
		t.Fatalf("expected web port error, got %v", err)
	}
	err = Run(context.Background(), []string{"--json", "web"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not support --json") {
		t.Fatalf("expected web json error, got %v", err)
	}
}

func TestRunHelpMenus(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"global short", []string{"--help"}, "Examples:"},
		{"backup help", []string{"backup", "help"}, "wacrawl backup <init|push|pull|status|snapshots>"},
		{"backup topic", []string{"help", "backup"}, "wacrawl backup <init|push|pull|status|snapshots>"},
		{"doctor topic", []string{"help", "doctor"}, "wacrawl doctor [--source PATH]"},
		{"command help topic", []string{"help", "messages"}, "wacrawl messages [flags]"},
		{"doctor flag", []string{"doctor", "--help"}, "wacrawl doctor [--source PATH]"},
		{"status flag", []string{"status", "--help"}, "unread counts"},
		{"chats flag", []string{"chats", "--help"}, "wacrawl chats [--limit N] [--unread]"},
		{"contacts topic", []string{"help", "contacts"}, "wacrawl [--json] [--sync auto|always|never] contacts export"},
		{"contacts export flag", []string{"contacts", "export", "--help"}, "wacrawl [--json] [--sync auto|always|never] contacts export"},
		{"unread flag", []string{"unread", "--help"}, "wacrawl unread [--limit N]"},
		{"command flag", []string{"messages", "--help"}, "--has-media"},
		{"search flag", []string{"search", "--help"}, "wacrawl search [flags] <query>"},
		{"sql topic", []string{"help", "sql"}, "wacrawl sql <select query>"},
		{"sql flag", []string{"sql", "--help"}, "read-only SQL query"},
		{"web topic", []string{"help", "web"}, "wacrawl web [--port N]"},
		{"web flag", []string{"web", "--help"}, "private web viewer"},
		{"import flag", []string{"import", "--help"}, "--copy-media"},
		{"sync topic", []string{"help", "sync"}, "wacrawl sync [--source PATH]"},
		{"backup flag", []string{"backup", "--help"}, "wacrawl backup <init|push|pull|status|snapshots>"},
		{"backup nested flag", []string{"backup", "init", "--help"}, "wacrawl backup init [flags]"},
		{"backup nested topic", []string{"backup", "help", "push"}, "wacrawl backup push [flags]"},
		{"backup pull topic", []string{"help", "backup", "pull"}, "wacrawl backup pull [flags]"},
		{"backup status topic", []string{"help", "backup", "status"}, "wacrawl backup status [flags]"},
		{"backup snapshots topic", []string{"help", "backup", "snapshots"}, "wacrawl backup snapshots [flags]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := Run(context.Background(), tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("Run() error = %v stderr=%s", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("stdout missing %q:\n%s", tc.want, stdout.String())
			}
		})
	}
	var discard bytes.Buffer
	if printCommandUsage(&discard, "missing") {
		t.Fatal("unknown help topic should return false")
	}
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"help", "missing"}, &stdout, &stderr)
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "unknown help topic") {
		t.Fatalf("expected unknown help topic error, got %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	err = Run(context.Background(), []string{"backup", "help", "missing"}, &stdout, &stderr)
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "unknown backup help topic") {
		t.Fatalf("expected unknown backup help topic error, got %v", err)
	}
}

func TestReadCommandsSyncArchive(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createDesktopFixture(t, source)
	dbPath := filepath.Join(t.TempDir(), "archive.db")

	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "status"}, &stdout, &stderr); err != nil {
		t.Fatalf("status error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "messages=3") || !strings.Contains(stdout.String(), "last_import=20") {
		t.Fatalf("status should auto-sync archive:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sync: syncing WhatsApp Desktop snapshot") {
		t.Fatalf("stderr should note sync, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", filepath.Join(t.TempDir(), "archive.db"), "--source", source, "--sync", "never", "status"}, &stdout, &stderr); err != nil {
		t.Fatalf("status --sync never error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "messages=0") || !strings.Contains(stdout.String(), "last_import=-") {
		t.Fatalf("status should stay archive-only with --sync never:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", filepath.Join(t.TempDir(), "archive.db"), "--source", source, "--sync", "always", "search", "launch"}, &stdout, &stderr); err != nil {
		t.Fatalf("search --sync always error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[launch] now") {
		t.Fatalf("search should sync before reading:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", filepath.Join(t.TempDir(), "archive.db"), "--source", source, "--sync", "always", "sql", "SELECT count(*) AS messages FROM messages"}, &stdout, &stderr); err != nil {
		t.Fatalf("sql --sync always error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "messages") || !strings.Contains(stdout.String(), "3") {
		t.Fatalf("sql should sync before reading:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sync: syncing WhatsApp Desktop snapshot") {
		t.Fatalf("sql should report sync before reading, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	err := Run(ctx, []string{"--db", filepath.Join(t.TempDir(), "archive.db"), "--source", filepath.Join(source, "missing"), "--sync", "always", "status"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "source unavailable") {
		t.Fatalf("expected --sync always to fail without source, got %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", filepath.Join(t.TempDir(), "contacts.db"), "--source", source, "--sync", "always", "--json", "contacts", "export"}, &stdout, &stderr); err != nil {
		t.Fatalf("contacts export --sync always error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"display_name": "Alice Contact"`) {
		t.Fatalf("contacts export should sync before reading:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sync: syncing WhatsApp Desktop snapshot") {
		t.Fatalf("contacts export should report sync before reading, got %q", stderr.String())
	}

	addDesktopContact(t, source, "333@s.whatsapp.net", "+333", "Charlie Contact")
	autoDB := filepath.Join(t.TempDir(), "auto-contacts.db")
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", autoDB, "--source", source, "--sync", "always", "--json", "status"}, &stdout, &stderr); err != nil {
		t.Fatalf("seed contact auto-sync archive: %v stderr=%s", err, stderr.String())
	}
	addDesktopContact(t, source, "444@s.whatsapp.net", "+444", "Delta Contact")
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", autoDB, "--source", source, "--sync", "auto", "--sync-max-age", "0s", "--json", "contacts", "export"}, &stdout, &stderr); err != nil {
		t.Fatalf("contacts export --sync auto error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"display_name": "Delta Contact"`) {
		t.Fatalf("contacts export should auto-sync contact count drift:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sync: syncing WhatsApp Desktop snapshot") {
		t.Fatalf("contacts export should report contact drift sync, got %q", stderr.String())
	}

	updateDesktopContact(t, source, "444@s.whatsapp.net", "+444", "Delta Renamed")
	markDesktopContactsModified(t, source, time.Now().Add(time.Second))
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", autoDB, "--source", source, "--sync", "auto", "--sync-max-age", "0s", "--json", "contacts", "export"}, &stdout, &stderr); err != nil {
		t.Fatalf("contacts export --sync auto same-count edit error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"display_name": "Delta Renamed"`) {
		t.Fatalf("contacts export should auto-sync contact DB mtime drift:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sync: syncing WhatsApp Desktop snapshot") {
		t.Fatalf("contacts export should report contact mtime drift sync, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", filepath.Join(t.TempDir(), "contacts.db"), "--source", source, "--sync", "never", "--json", "contacts", "export"}, &stdout, &stderr); err != nil {
		t.Fatalf("contacts export --sync never error = %v stderr=%s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), `"display_name"`) {
		t.Fatalf("contacts export should stay archive-only with --sync never:\n%s", stdout.String())
	}
}

func addDesktopContact(t *testing.T, dir, jid, phone, name string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "ContactsV2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`insert into ZWAADDRESSBOOKCONTACT values (?, ?, ?, '', '', '', '', '', '', 700000000)`, jid, phone, name)
	if err != nil {
		t.Fatal(err)
	}
}

func updateDesktopContact(t *testing.T, dir, jid, phone, name string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "ContactsV2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`update ZWAADDRESSBOOKCONTACT set ZPHONENUMBER = ?, ZFULLNAME = ?, ZLASTUPDATED = 700000100 where ZWHATSAPPID = ?`, phone, name, jid)
	if err != nil {
		t.Fatal(err)
	}
}

func markDesktopContactsModified(t *testing.T, dir string, ts time.Time) {
	t.Helper()
	path := filepath.Join(dir, "ContactsV2.sqlite")
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCommands(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createDesktopFixture(t, source)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := exec.Command("git", "init", "--bare", remote).Run(); err != nil { // #nosec G204 -- test creates a temp bare Git remote.
		t.Fatal(err)
	}
	// Detached receive-side maintenance can outlive the push and race TempDir cleanup.
	if err := exec.Command("git", "-C", remote, "config", "maintenance.auto", "false").Run(); err != nil { // #nosec G204 -- test configures its temp bare Git remote.
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "backup.json")
	repo := filepath.Join(t.TempDir(), "backup")
	identity := filepath.Join(t.TempDir(), "age.key")

	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"--db", dbPath, "--source", source, "sync", "--copy-media"}, &stdout, &stderr); err != nil {
		t.Fatalf("sync error = %v stderr=%s", err, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"backup", "init", "--config", config, "--repo", repo, "--remote", remote, "--identity", identity, "--no-push"}, &stdout, &stderr); err != nil {
		t.Fatalf("backup init error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "recipient=age1") {
		t.Fatalf("backup init did not print recipient:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", dbPath, "--sync", "never", "backup", "push", "--config", config, "--no-push", "--tag", "snapshot/initial"}, &stdout, &stderr); err != nil {
		t.Fatalf("backup push error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "encrypted=true") || !strings.Contains(stdout.String(), "messages=3") || !strings.Contains(stdout.String(), "media_files=1") || !strings.Contains(stdout.String(), "tag=snapshot/initial") {
		t.Fatalf("backup push output mismatch:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"backup", "snapshots", "--config", config}, &stdout, &stderr); err != nil {
		t.Fatalf("backup snapshots error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "snapshot/initial") || !strings.Contains(stdout.String(), "MESSAGES") {
		t.Fatalf("backup snapshots output mismatch:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--json", "backup", "snapshots", "--config", config, "--limit", "1"}, &stdout, &stderr); err != nil {
		t.Fatalf("JSON backup snapshots error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"repo"`) || !strings.Contains(stdout.String(), `"snapshots"`) || !strings.Contains(stdout.String(), `"ref"`) {
		t.Fatalf("JSON backup snapshots output mismatch:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"backup", "snapshots", "--config", config, "--limit", "0"}, &stdout, &stderr); err == nil || ExitCode(err) != 2 {
		t.Fatalf("invalid backup snapshots limit error = %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"backup", "snapshots", "--config", config, "extra"}, &stdout, &stderr); err == nil || ExitCode(err) != 2 {
		t.Fatalf("backup snapshots positional argument error = %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"backup", "status", "--config", config}, &stdout, &stderr); err != nil {
		t.Fatalf("backup status error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "encrypted=true") || !strings.Contains(stdout.String(), "repo="+repo) {
		t.Fatalf("backup status output mismatch:\n%s", stdout.String())
	}
	restoredDB := filepath.Join(t.TempDir(), "restored.db")
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", restoredDB, "backup", "pull", "--config", config, "--ref", "snapshot/initial"}, &stdout, &stderr); err != nil {
		t.Fatalf("backup pull error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ref=") {
		t.Fatalf("backup pull should report resolved ref:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := Run(ctx, []string{"--db", restoredDB, "--sync", "never", "search", "launch"}, &stdout, &stderr); err != nil {
		t.Fatalf("restored search error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[launch] now") {
		t.Fatalf("restored search mismatch:\n%s", stdout.String())
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("image")))
	restoredMedia := filepath.Join(filepath.Dir(restoredDB), "media", digest[:2], digest)
	if body, err := os.ReadFile(restoredMedia); err != nil || string(body) != "image" { // #nosec G304 -- test reads its expected temp restore path.
		t.Fatalf("restored media = %q err=%v", body, err)
	}
}

func TestSyncArchiveKeepsExistingArchiveWhenSourceUnavailable(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UTC().Add(-time.Hour)
	if err := st.ReplaceAll(
		ctx,
		store.ImportStats{SourcePath: "/missing", DBPath: st.Path(), FinishedAt: now},
		nil,
		[]store.Chat{{JID: "chat", Kind: "dm", Name: "Chat", LastMessageAt: now}},
		nil,
		nil,
		[]store.Message{{SourcePK: 1, ChatJID: "chat", MessageID: "a", Timestamp: now, RawType: 0, Text: "cached"}},
	); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	a := app{stderr: &stderr, source: filepath.Join(t.TempDir(), "missing"), syncMode: archiveSyncAuto, syncMaxAge: 0}
	if err := a.syncArchive(ctx, st); err != nil {
		t.Fatalf("syncArchive should keep existing archive, got %v", err)
	}
	if !strings.Contains(stderr.String(), "using existing archive") {
		t.Fatalf("expected fallback warning, got %q", stderr.String())
	}
}

func TestSyncDecisionHelpers(t *testing.T) {
	now := time.Now().UTC()
	status := store.Status{Messages: 3, NewestMessage: now, NewestObserved: now, LastImportAt: now.Add(-time.Hour)}
	if !archiveNeedsSyncCheck(status, 15*time.Minute) {
		t.Fatal("old import should need sync check")
	}
	if archiveNeedsSyncCheck(status, -1) {
		t.Fatal("negative max age should disable auto checks")
	}
	if !sourceAheadOfArchive(whatsappdb.Source{MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Add(time.Second).Format(time.RFC3339)}, status) {
		t.Fatal("newer source timestamp should be ahead")
	}
	if !sourceAheadOfArchive(whatsappdb.Source{MessageRows: 4, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, status) {
		t.Fatal("different source row count should be ahead")
	}
	if sourceAheadOfArchive(whatsappdb.Source{MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, status) {
		t.Fatal("equal source should not be ahead")
	}
	chatDB := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	if err := os.WriteFile(chatDB, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(chatDB, now, now); err != nil {
		t.Fatal(err)
	}
	if !sourceAheadOfArchive(whatsappdb.Source{ChatDB: chatDB, MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, status) {
		t.Fatal("in-place ChatStorage change should be ahead")
	}
	snapshotStatus := status
	snapshotStatus.LastImportAt = now.Add(time.Hour)
	snapshotStatus.LastSourceSnapshot = now.Add(-time.Hour)
	if !sourceAheadOfArchive(whatsappdb.Source{ChatDB: chatDB, MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, snapshotStatus) {
		t.Fatal("source snapshot watermark should cover changes before import completion")
	}
	if sourceAheadOfArchive(whatsappdb.Source{MessageRows: 3, MessageRowsKnown: true, NewestMessage: "not-time"}, status) {
		t.Fatal("invalid source timestamp should not be ahead")
	}
	tombstoneOnly := status
	tombstoneOnly.Messages = 0
	tombstoneOnly.DeletedMessages = 3
	if sourceAheadOfArchive(whatsappdb.Source{MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, tombstoneOnly) {
		t.Fatal("tombstone-only archive should not be treated as uninitialized")
	}
	tombstoneOnly.NewestMessage = time.Time{}
	tombstoneOnly.NewestObserved = now
	if sourceAheadOfArchive(whatsappdb.Source{MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, tombstoneOnly) {
		t.Fatal("tombstoned newest message should use the observed watermark")
	}
	recordedZero := status
	recordedZero.LastSourceMessages = 0
	recordedZero.SourceMessagesKnown = true
	if !sourceAheadOfArchive(whatsappdb.Source{MessageRows: 3, MessageRowsKnown: true, NewestMessage: now.Format(time.RFC3339)}, recordedZero) {
		t.Fatal("recorded zero source count should not fall back to archive totals")
	}
	if !sourceAheadOfArchive(whatsappdb.Source{}, store.Status{}) {
		t.Fatal("empty archive should sync")
	}
}

func TestCLIHelpers(t *testing.T) {
	if _, err := parseTime("2026-04-25"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTime("2026-04-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if formatTime(timeZero()) != "-" {
		t.Fatal("zero time should format as dash")
	}
	if firstNonEmpty("", "x") != "x" || firstNonEmpty("", "") != "" {
		t.Fatal("firstNonEmpty mismatch")
	}
	args, query, err := splitSearchArgs([]string{"launch", "--limit", "5", "--from-them"})
	if err != nil {
		t.Fatal(err)
	}
	if query != "launch" || strings.Join(args, " ") != "--limit 5 --from-them" {
		t.Fatalf("unexpected split args=%v query=%q", args, query)
	}
	if _, _, err := splitSearchArgs([]string{"one", "two"}); err == nil {
		t.Fatal("expected multi-query split error")
	}
	if _, _, err := splitSearchArgs([]string{"--limit"}); err == nil {
		t.Fatal("expected missing flag value error")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := bindMessageFlags(fs)
	if err := fs.Parse([]string{"--from-them", "--after", "2026-04-25", "--before", "2026-04-26"}); err != nil {
		t.Fatal(err)
	}
	filter, err := flags.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if filter.FromMe == nil || *filter.FromMe || filter.After == nil || filter.Before == nil {
		t.Fatalf("unexpected resolved filter: %+v", filter)
	}
	if mode, err := parseArchiveSyncMode("auto"); err != nil || mode != archiveSyncAuto {
		t.Fatalf("unexpected sync mode parse: %q %v", mode, err)
	}
	if _, err := parseArchiveSyncMode("nope"); err == nil {
		t.Fatal("expected invalid sync mode error")
	}
}

func timeZero() (out time.Time) { return out }

func createDesktopFixture(t *testing.T, dir string) {
	t.Helper()
	chat, err := sql.Open("sqlite", filepath.Join(dir, "ChatStorage.sqlite"))
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
insert into Z_METADATA values (1, 'cli-fixture-account', null);
insert into ZWACHATSESSION values (1, '111@s.whatsapp.net', 'Bob', 700000020, 0, 0, 0, 0, 0);
insert into ZWACHATSESSION values (2, '123@g.us', 'Launch Group', 700000010, 2, 0, 0, 0, 1);
insert into ZWAGROUPINFO values (1, 2, 'owner@s.whatsapp.net', 699999000);
insert into ZWAGROUPMEMBER values (1, 2, '222@lid', 'Alice', 'Alice', 1, 1);
insert into ZWAMEDIAITEM values (1, 3, 'Media/123@g.us/a/test.jpg', 'https://example.invalid/media.enc', 'launch image', '', 42);
insert into ZWAMESSAGE values (1, 1, null, null, 'dm-in', 0, 700000000, 'hello', 0, 0, '111@s.whatsapp.net', '', 'Bob');
insert into ZWAMESSAGE values (2, 1, null, null, 'dm-out', 1, 700000001, 'roger', 0, 0, '', '111@s.whatsapp.net', '');
insert into ZWAMESSAGE values (3, 2, 1, 1, 'group-image', 0, 700000002, 'launch now', 1, 1, '123@g.us', '', 'Alice');
`)
	contacts, err := sql.Open("sqlite", filepath.Join(dir, "ContactsV2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contacts.Close() }()
	mustExec(t, contacts, `
create table ZWAADDRESSBOOKCONTACT (ZWHATSAPPID varchar, ZPHONENUMBER varchar, ZFULLNAME varchar, ZGIVENNAME varchar, ZLASTNAME varchar, ZBUSINESSNAME varchar, ZUSERNAME varchar, ZLID varchar, ZABOUTTEXT varchar, ZLASTUPDATED timestamp);
insert into ZWAADDRESSBOOKCONTACT values ('111@s.whatsapp.net', '+111', 'Bob', 'Bob', '', '', '', '', '', 700000000);
insert into ZWAADDRESSBOOKCONTACT values ('222@s.whatsapp.net', '+222', 'Alice Contact', 'Alice', '', '', '', '222', '', 700000000);
`)
	axolotl, err := sql.Open("sqlite", filepath.Join(dir, "Axolotl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = axolotl.Close() }()
	mustExec(t, axolotl, `
create table ZWAZMDACCOUNT (Z_PK integer primary key, ZACCOUNTJIDSTRING varchar, ZUSERJIDSTRING varchar);
insert into ZWAZMDACCOUNT values (1, 'cli-owner@s.whatsapp.net', 'cli-owner@s.whatsapp.net');
`)
	mediaPath := filepath.Join(dir, "Media", "123@g.us", "a", "test.jpg")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
