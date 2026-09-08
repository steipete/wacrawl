package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditBackupUnchangedRetryPublishesPendingCommit(t *testing.T) {
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
		Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	if _, _, err := Init(ctx, opts); err != nil {
		t.Fatal(err)
	}
	st := openFixtureStore(t, "archive.db")
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	opts.Push = true
	if _, err := Push(ctx, st, opts); err == nil {
		t.Fatal("expected synthetic remote refusal")
	}
	local, err := scopeGit(ctx, opts.Repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	result, err := Push(ctx, st, opts)
	if err != nil || result.Changed {
		t.Fatalf("unchanged retry: %+v, %v", result, err)
	}
	published, err := scopeGit(ctx, remote, "rev-parse", "refs/heads/main")
	if err != nil || string(published) != string(local) {
		t.Fatalf("retry did not publish existing commit: %s, %v", published, err)
	}
}

func TestAuditBackupRefusesOlderUnpublishedPlaintext(t *testing.T) {
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
		Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	if _, _, err := Init(ctx, opts); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(opts.Repo, "private-key.txt")
	if err := os.WriteFile(key, []byte("synthetic unpublished private sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, opts.Repo, "add", "--", "private-key.txt")
	runGit(t, opts.Repo, "commit", "-m", "test: unsafe ancestor")
	runGit(t, opts.Repo, "rm", "--", "private-key.txt")
	runGit(t, opts.Repo, "commit", "-m", "test: remove current copy")
	opts.Push = true
	_, err := Push(ctx, openFixtureStore(t, "archive.db"), opts)
	if err == nil || !strings.Contains(err.Error(), "unverified unpublished") {
		t.Fatalf("unsafe history not rejected: %v", err)
	}
	remoteRefs, err := scopeGit(ctx, opts.Repo, "ls-remote", "--heads", "origin")
	if err != nil || len(remoteRefs) != 0 {
		t.Fatalf("unsafe history was published: %s, %v", remoteRefs, err)
	}
}

func TestAuditBackupChecksActualPushDestination(t *testing.T) {
	ctx := context.Background()
	fetch := filepath.Join(t.TempDir(), "fetch.git")
	push := filepath.Join(t.TempDir(), "push.git")
	runGit(t, t.TempDir(), "init", "--bare", fetch)
	runGit(t, t.TempDir(), "init", "--bare", push)
	opts := Options{
		Repo: filepath.Join(t.TempDir(), "repo"), Remote: fetch,
		Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	cfg, _, err := Init(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Repo, "private.txt"), []byte("synthetic sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, cfg.Repo, "add", "private.txt")
	runGit(t, cfg.Repo, "commit", "-m", "test: unsafe local history")
	runGit(t, cfg.Repo, "push", "origin", "HEAD")
	runGit(t, cfg.Repo, "config", "remote.origin.pushurl", push)
	if err := verifyPendingHistory(ctx, cfg); err == nil {
		t.Fatal("accepted a baseline from a different push destination")
	}
}

func TestAuditBackupRejectsUnpublishedManifestCleartext(t *testing.T) {
	for _, field := range []string{"unknown", "recipient", "table", "sha256", "count-key", "file-path", "duplicate"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			remote := filepath.Join(t.TempDir(), "remote.git")
			runGit(t, t.TempDir(), "init", "--bare", remote)
			opts := Options{
				Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
				Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
			}
			cfg, _, err := Init(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Push(ctx, openFixtureStore(t, "archive.db"), opts); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(cfg.Repo, "manifest.json")
			original, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(original, &value); err != nil {
				t.Fatal(err)
			}
			const sentinel = "synthetic private sentinel"
			switch field {
			case "unknown":
				value["private_note"] = sentinel
			case "recipient":
				value["recipients"] = []string{sentinel}
			case "table", "sha256":
				value["shards"].([]any)[0].(map[string]any)[field] = sentinel
			case "count-key":
				value["counts"].(map[string]any)[sentinel] = 1
			case "file-path":
				shard := value["shards"].([]any)[0].(map[string]any)
				value["files"] = []any{map[string]any{"shard": shard["path"], "bytes": shard["bytes"], "path": sentinel}}
			}
			modified, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if field == "duplicate" {
				modified = append([]byte(`{"recipients":["synthetic private sentinel"],`), modified[1:]...)
			}
			if err := os.WriteFile(name, modified, 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Repo, "add", "manifest.json")
			runGit(t, cfg.Repo, "commit", "-m", "test: unsafe old manifest")
			if err := os.WriteFile(name, original, 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Repo, "add", "manifest.json")
			runGit(t, cfg.Repo, "commit", "-m", "test: restore current manifest")
			if err := verifyPendingHistory(ctx, cfg); err == nil {
				t.Fatal("accepted cleartext in an older unpublished manifest")
			}
		})
	}
}

func TestAuditBackupChecksManifestOnlyArtifactReferences(t *testing.T) {
	for _, field := range []string{"missing-path", "bytes"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			remote := filepath.Join(t.TempDir(), "remote.git")
			runGit(t, t.TempDir(), "init", "--bare", remote)
			opts := Options{
				Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
				Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
			}
			cfg, _, err := Init(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Push(ctx, openFixtureStore(t, "archive.db"), opts); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(cfg.Repo, "manifest.json")
			original, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var manifest Manifest
			if err := json.Unmarshal(original, &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.Shards) == 0 {
				t.Fatal("fixture has no shards")
			}
			if field == "missing-path" {
				manifest.Shards[0].Path = "data/missing.age"
			} else {
				manifest.Shards[0].Bytes++
			}
			modified, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, modified, 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Repo, "add", "manifest.json")
			runGit(t, cfg.Repo, "commit", "-m", "test: broken historical reference")
			if err := os.WriteFile(name, original, 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Repo, "add", "manifest.json")
			runGit(t, cfg.Repo, "commit", "-m", "test: restore current manifest")
			if err := verifyPendingHistory(ctx, cfg); err == nil {
				t.Fatal("accepted a broken reference in an older manifest-only commit")
			}
		})
	}
}

func TestAuditBackupAcceptsLegacyParticipantCount(t *testing.T) {
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
		Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	cfg, _, err := Init(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, openFixtureStore(t, "archive.db"), opts); err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	legacy := toCrawlkitManifest(manifest)
	legacy.Counts["group_participants"] = legacy.Counts["participants"]
	delete(legacy.Counts, "participants")
	if got := fromCrawlkitManifest(legacy).Counts.Participants; got != manifest.Counts.Participants {
		t.Fatalf("legacy reader count = %d, want %d", got, manifest.Counts.Participants)
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Repo, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, cfg.Repo, "add", "manifest.json")
	runGit(t, cfg.Repo, "commit", "-m", "test: supported legacy count key")
	if err := verifyPendingHistory(ctx, cfg); err != nil {
		t.Fatalf("supported legacy count rejected: %v", err)
	}
}

func TestAuditBackupRejectsDeletedSnapshotReferences(t *testing.T) {
	for _, kind := range []string{"artifact", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			remote := filepath.Join(t.TempDir(), "remote.git")
			runGit(t, t.TempDir(), "init", "--bare", remote)
			opts := Options{
				Repo: filepath.Join(t.TempDir(), "repo"), Remote: remote,
				Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
			}
			cfg, _, err := Init(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Push(ctx, openFixtureStore(t, "archive.db"), opts); err != nil {
				t.Fatal(err)
			}
			manifest, err := readManifest(cfg.Repo)
			if err != nil {
				t.Fatal(err)
			}
			name := "manifest.json"
			if kind == "artifact" {
				if len(manifest.Shards) == 0 {
					t.Fatal("fixture has no shards")
				}
				name = manifest.Shards[0].Path
			}
			target := filepath.Join(cfg.Repo, filepath.FromSlash(name))
			original, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Repo, "rm", "--", ":(literal)"+name)
			runGit(t, cfg.Repo, "commit", "-m", "test: remove required snapshot object")
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, original, 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Repo, "add", "--", ":(literal)"+name)
			runGit(t, cfg.Repo, "commit", "-m", "test: restore current snapshot object")
			if err := verifyPendingHistory(ctx, cfg); err == nil {
				t.Fatal("accepted a missing required object in an older unpublished commit")
			}
		})
	}
}
