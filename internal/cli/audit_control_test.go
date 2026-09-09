package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/openclaw/wacrawl/internal/backup"
	"github.com/openclaw/wacrawl/internal/store"
)

func TestAuditMetadataUsesBackupConfigPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got, want := controlManifest().Paths.DefaultConfig, backup.DefaultConfigPath(); got != want {
		t.Fatalf("config path = %q, want %q", got, want)
	}
}

func TestAuditMetadataReadCommandsDoNotSync(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	createDesktopFixture(t, source)
	for _, name := range []string{"search", "sql"} {
		t.Run(name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			command := controlManifest().Commands[name]
			if command.Mutates {
				t.Fatal("read command marked mutating")
			}
			args := append([]string{"--db", dbPath, "--source", source}, command.Argv[1:]...)
			if name == "search" {
				args = append(args, "launch")
			} else {
				args = append(args, "SELECT count(*) FROM messages")
			}
			var stdout, stderr bytes.Buffer
			if err := Run(ctx, args, &stdout, &stderr); err != nil {
				t.Fatalf("advertised argv: %v; stderr=%s", err, stderr.String())
			}
			st, err = store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := st.Close(); err != nil {
					t.Error(err)
				}
			}()
			status, err := st.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if status.Messages != 0 || !status.LastImportAt.IsZero() {
				t.Fatalf("read argv imported source: messages=%d imported=%s", status.Messages, status.LastImportAt)
			}
			if stderr.Len() != 0 {
				t.Fatalf("read argv attempted source sync: %s", stderr.String())
			}
		})
	}
}
