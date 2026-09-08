package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAuditMergeMediaRetention(t *testing.T) {
	cases := []string{
		"equal-bytes", "cache-absent", "new-missing", "different-bytes",
		"raw-type", "media-type", "url", "size", "title", "no-evidence", "old-empty",
		"old-outside", "old-missing", "old-symlink", "new-symlink", "new-directory",
		"account-conflict", "event-conflict", "tombstone",
	}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			root := t.TempDir()
			oldPath, newPath := filepath.Join(root, "legacy.jpg"), filepath.Join(root, "new")
			for _, name := range []string{oldPath, newPath} {
				if err := os.WriteFile(name, []byte("image"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Unix(1700000000, 0).UTC()
			old := Message{
				SourcePK: 1, ChatJID: "chat", MessageID: "message", Timestamp: now,
				RawType: 1, MediaType: "image", MediaURL: "https://example.invalid/media",
				MediaTitle: "attachment", MediaSize: 5, MediaPath: oldPath,
			}
			switch kind {
			case "old-empty":
				old.MediaPath = ""
			case "old-outside":
				old.MediaPath = filepath.Join(t.TempDir(), "not-owned")
			case "no-evidence":
				old.MediaURL, old.MediaSize = "", 0
			}
			stats := ImportStats{
				SourceIdentity: "fixture-source", AccountIdentity: "wa-account:fixture",
				SourceStoreIdentity: "wa-store:fixture", FinishedAt: now, MediaRoot: root, Messages: 1,
			}
			if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{old}); err != nil {
				t.Fatal(err)
			}
			before, err := st.ExportAll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			incoming := old
			incoming.MediaPath = newPath
			wantPath, wantError := newPath, false
			stats.FinishedAt = now.Add(time.Minute)
			switch kind {
			case "equal-bytes":
				wantPath = oldPath
			case "cache-absent":
				incoming.MediaPath, wantPath = "", oldPath
			case "new-missing":
				if err := os.Remove(newPath); err != nil {
					t.Fatal(err)
				}
				wantPath = oldPath
			case "different-bytes":
				if err := os.WriteFile(newPath, []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "raw-type":
				incoming.RawType = 2
			case "media-type":
				incoming.MediaType = ""
			case "url":
				incoming.MediaURL = "https://example.invalid/other"
			case "size":
				incoming.MediaSize++
			case "title":
				incoming.MediaTitle = "other"
			case "old-missing":
				if err := os.Remove(oldPath); err != nil {
					t.Fatal(err)
				}
			case "old-symlink", "new-symlink", "new-directory":
				name := newPath
				if kind == "old-symlink" {
					name = oldPath
				}
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if kind == "new-directory" {
					err = os.Mkdir(name, 0o700)
				} else {
					err = os.Symlink(filepath.Join(t.TempDir(), "missing"), name)
				}
				if err != nil {
					t.Fatal(err)
				}
				wantError = true
			case "account-conflict":
				stats.AccountIdentity = "wa-account:other"
				wantError = true
			case "event-conflict":
				incoming.MessageID = "other"
				wantError = true
			case "tombstone":
				incoming.RawType, incoming.MediaSize = 0, 0
				incoming.MediaType, incoming.MediaURL, incoming.MediaTitle, incoming.MediaPath = "", "", "", ""
				incoming.SourceTextNull = true
				wantPath = ""
			}
			err = st.MergeAll(ctx, stats, nil, []Chat{{JID: "new-parent", Name: "must roll back on failure"}}, nil, nil, []Message{incoming})
			if (err != nil) != wantError {
				t.Fatalf("merge error=%v; want error=%v", err, wantError)
			}
			after, err := st.ExportAll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if wantError {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed merge changed canonical rows/revisions/bindings")
				}
				return
			}
			if len(after.Messages) != 1 || after.Messages[0].MediaPath != wantPath ||
				after.AccountIdentity != before.AccountIdentity || after.SourceStoreIdentity != before.SourceStoreIdentity {
				t.Fatalf("retention/binding: %+v", after)
			}
			want := incoming
			want.MediaPath = wantPath
			want.EventID, want.SourceRowPK = before.Messages[0].EventID, before.Messages[0].SourceRowPK
			want.SourceTextNull = false
			want.storedUnix = messageUnix(want)
			want.LastSeenAt = stats.FinishedAt
			if kind == "tombstone" {
				want.Tombstone = sourceTombstone(stats.FinishedAt, "whatsapp_payload_cleared")
			}
			if !reflect.DeepEqual(want, after.Messages[0]) {
				t.Fatalf("unexpected canonical message:\nwant %+v\ngot %+v", want, after.Messages[0])
			}
			previous, err := canonicalMessageJSON(before.Messages[0])
			if err != nil {
				t.Fatal(err)
			}
			current, err := canonicalMessageJSON(after.Messages[0])
			if err != nil {
				t.Fatal(err)
			}
			if previous == current {
				if len(after.Revisions) != 0 {
					t.Fatalf("spurious revision: %+v", after.Revisions)
				}
			} else if len(after.Revisions) != 1 || after.Revisions[0].PayloadJSON != previous ||
				after.Revisions[0].EventID != before.Messages[0].EventID || !after.Revisions[0].RecordedAt.Equal(stats.FinishedAt) {
				t.Fatalf("revision lost prior payload: %+v", after.Revisions)
			}
			if kind == "tombstone" {
				stats.FinishedAt = stats.FinishedAt.Add(time.Minute)
				if err := st.MergeAll(ctx, stats, nil, nil, nil, nil, []Message{old}); err != nil {
					t.Fatal(err)
				}
				again, err := st.ExportAll(ctx)
				if err != nil || !reflect.DeepEqual(after.Messages, again.Messages) || !reflect.DeepEqual(after.Revisions, again.Revisions) {
					t.Fatalf("retention revived tombstone or changed history: %v", err)
				}
			}
		})
	}
}
