package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ckbackup "github.com/openclaw/crawlkit/backup"
	"github.com/openclaw/wacrawl/internal/store"
)

func TestZeroCountPublicationTransitions(t *testing.T) {
	ctx := context.Background()
	opts := zeroCountOptions(t)
	st := openFixtureStore(t, "source.db")
	runGit(t, opts.Repo, "config", "maintenance.auto", "false")
	if err := os.WriteFile(filepath.Join(opts.Repo, "notes.txt"), []byte("unrelated staged fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, opts.Repo, "add", "--", "notes.txt")
	staged := zeroCountGit(t, opts.Repo, "diff", "--cached", "--binary")
	var prior ckbackup.Manifest
	for _, messages := range []int{0, 1, 0} {
		t.Run(fmt.Sprintf("messages-%d-after-%d-shards", messages, len(prior.Shards)), func(t *testing.T) {
			data := zeroCountData(messages)
			if err := st.ImportSnapshot(ctx, data, "/fixture", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			result, err := Push(ctx, st, opts)
			if err != nil || !result.Changed {
				t.Fatalf("transition push: %+v, %v", result, err)
			}
			current := zeroCountManifest(t, opts, messages, 0)
			currentPaths := map[string]bool{}
			for _, shard := range current.Shards {
				currentPaths[shard.Path] = true
			}
			for _, old := range prior.Shards {
				if !currentPaths[old.Path] {
					if _, err := scopeGit(ctx, opts.Repo, "cat-file", "-e", "HEAD:"+old.Path); err == nil {
						t.Fatalf("obsolete managed path remains committed: %s", old.Path)
					}
				}
			}
			zeroCountStable(t, st, opts, messages, 0)
			if got := zeroCountGit(t, opts.Repo, "diff", "--cached", "--binary"); got != staged {
				t.Fatal("backup changed unrelated staged content")
			}
			target := openFixtureStore(t, "restored.db")
			if err := target.ImportSnapshot(ctx, zeroCountData(1), "/fixture", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			zeroCountPull(t, target, opts)
			got, err := target.ExportAll(ctx)
			if err != nil || len(got.Messages) != messages || len(got.Revisions) != 0 {
				t.Fatalf("restore: messages=%d revisions=%d err=%v", len(got.Messages), len(got.Revisions), err)
			}
			prior = current
		})
	}
}

func zeroCountData(messages int) store.SnapshotData {
	data := store.SnapshotData{}
	if messages != 0 {
		data.Chats = []store.Chat{{JID: "chat", Kind: "dm"}}
		data.Messages = []store.Message{{
			SourcePK: 1, ChatJID: "chat", MessageID: "one",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Text: "fixture",
		}}
	}
	return data
}

func zeroCountOptions(t *testing.T) Options {
	t.Helper()
	cfg := Config{Repo: filepath.Join(t.TempDir(), "repo"), Identity: filepath.Join(t.TempDir(), "age.key")}
	if err := os.Mkdir(cfg.Repo, 0o700); err != nil {
		t.Fatal(err)
	}
	recipient, err := EnsureIdentity(cfg.Identity)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Recipients = []string{recipient}
	configPath := filepath.Join(t.TempDir(), "backup.json")
	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	runGit(t, cfg.Repo, "init", "-b", "main")
	return Options{ConfigPath: configPath, Repo: cfg.Repo}
}

func zeroCountGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	data, err := scopeGit(context.Background(), repo, args...)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func zeroCountManifest(t *testing.T, opts Options, messages, revisions int) ckbackup.Manifest {
	t.Helper()
	cfg, err := ResolveOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ckbackup.ReadManifest(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"messages": messages, "message_revisions": revisions} {
		if got, ok := manifest.Counts[key]; !ok || got != want {
			t.Fatalf("raw count %s = %d, present=%v; want %d", key, got, ok, want)
		}
		seen := 0
		for _, shard := range manifest.Shards {
			if shard.Table != key {
				continue
			}
			seen++
			if want == 0 {
				plain, err := ckbackup.DecryptShardFile(crawlkitConfig(cfg), shard)
				if err != nil || len(plain) != 0 || shard.Rows != 0 || shard.Bytes <= 0 || shard.SHA256 != ckbackup.SHA256Hex(nil) {
					t.Fatalf("invalid typed empty %s shard: %+v, %v", key, shard, err)
				}
			}
		}
		if seen != 1 {
			t.Fatalf("%s shards = %d, want one", key, seen)
		}
	}
	return manifest
}

func zeroCountPublished(t *testing.T, opts Options) map[string]string {
	t.Helper()
	cfg, err := ResolveOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ckbackup.ReadManifest(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{"git-head": zeroCountGit(t, cfg.Repo, "rev-parse", "HEAD")}
	paths := []string{"manifest.json"}
	for _, shard := range manifest.Shards {
		paths = append(paths, shard.Path)
	}
	for _, file := range manifest.Files {
		paths = append(paths, file.Shard)
	}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(cfg.Repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		out[path] = string(data)
	}
	return out
}

func zeroCountStable(t *testing.T, st *store.Store, opts Options, messages, revisions int) {
	t.Helper()
	before := zeroCountPublished(t, opts)
	for push := 2; push <= 3; push++ {
		result, err := Push(context.Background(), st, opts)
		if err != nil || result.Changed {
			t.Fatalf("unchanged push %d: %+v, %v", push, result, err)
		}
		zeroCountManifest(t, opts, messages, revisions)
		if !reflect.DeepEqual(before, zeroCountPublished(t, opts)) {
			t.Fatalf("push %d changed manifest timestamp/bytes, ciphertext references/bytes or Git HEAD", push)
		}
	}
}

func zeroCountPull(t *testing.T, target *store.Store, opts Options) {
	t.Helper()
	if _, err := Pull(context.Background(), target, opts); err != nil {
		t.Fatal(err)
	}
}
