package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditBackupReadmeCompatibility(t *testing.T) {
	const producerHash = "c1055ca973a7b06da3ebdeece5975dd8ab5eed3e575439f191da5c48e9b222a3"
	if fmt.Sprintf("%x", sha256.Sum256([]byte(legacyBackupReadme))) != producerHash || len(legacyBackupReadme) != 3041 {
		t.Fatal("legacy template differs from producer 0663d388")
	}
	for _, tc := range []struct {
		name       string
		body       string
		failedPush bool
		executable bool
		reject     bool
	}{
		{name: "unpublished-legacy-init", body: legacyBackupReadme},
		{name: "failed-legacy-push", body: legacyBackupReadme, failedPush: true},
		{name: "current", body: backupReadme},
		{name: "edited-legacy", body: strings.Replace(legacyBackupReadme, "# backup-wacrawl", "# edited-backup", 1), reject: true},
		{name: "private-addition", body: legacyBackupReadme + "\nsynthetic private note\n", reject: true},
		{name: "executable-legacy", body: legacyBackupReadme, executable: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			parent := t.TempDir()
			remote := filepath.Join(parent, "remote.git")
			initBareRemote(t, remote)
			opts := Options{
				Repo: filepath.Join(parent, "repo"), Remote: remote,
				Identity: filepath.Join(parent, "age.key"), ConfigPath: filepath.Join(parent, "backup.json"),
			}
			runGit(t, parent, "init", "-b", "main", opts.Repo)
			runGit(t, opts.Repo, "remote", "add", "origin", remote)
			readme := filepath.Join(opts.Repo, "README.md")
			if err := os.WriteFile(readme, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, opts.Repo, "add", "README.md")
			if tc.executable {
				runGit(t, opts.Repo, "update-index", "--chmod=+x", "README.md")
			}
			runGit(t, opts.Repo, "commit", "-m", "test: unpublished producer init")
			oldHead := strings.TrimSpace(string(auditGitBytes(t, opts.Repo, "rev-parse", "HEAD")))
			oldCommit := auditGitBytes(t, opts.Repo, "cat-file", "commit", oldHead)
			runGit(t, opts.Repo, "tag", "snapshot/producer-init")
			tags := auditGitBytes(t, opts.Repo, "show-ref", "--tags")
			if tc.reject {
				if err := os.WriteFile(readme, []byte(legacyBackupReadme), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, opts.Repo, "add", "README.md")
				runGit(t, opts.Repo, "commit", "-m", "test: repair only current README")
			}
			if _, _, err := Init(ctx, opts); err != nil {
				t.Fatal(err)
			}
			read := func(dir, name string) []byte {
				t.Helper()
				data, err := fs.ReadFile(os.DirFS(dir), name)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			originalReadme := read(opts.Repo, "README.md")
			info, err := os.Stat(readme)
			if err != nil {
				t.Fatal(err)
			}
			identity, config := read(parent, "age.key"), read(parent, "backup.json")
			sentinel := filepath.Join(opts.Repo, "unrelated.txt")
			if err := os.WriteFile(sentinel, []byte("staged sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, opts.Repo, "add", "unrelated.txt")
			if err := os.WriteFile(sentinel, []byte("working sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(opts.Repo, "untracked.txt"), []byte("untracked sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			index := auditGitBytes(t, opts.Repo, "diff", "--cached", "--binary")
			st := openFixtureStore(t, "archive.db")
			opts.Push = true
			var pending []byte
			if tc.failedPush {
				hook := filepath.Join(remote, "hooks", "pre-receive")
				if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { // #nosec G306 -- owner-only executable refusal hook in this test's temp bare remote.
					t.Fatal(err)
				}
				if _, err := Push(ctx, st, opts); err == nil || strings.Contains(err.Error(), "unverified unpublished") {
					t.Fatalf("expected remote refusal after valid history: %v", err)
				}
				pending = auditGitBytes(t, opts.Repo, "rev-parse", "HEAD")
				if err := os.Remove(hook); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Push(ctx, st, opts)
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), "unverified unpublished") {
					t.Fatalf("accepted unsafe historical README: %v", err)
				}
			} else if err != nil || result.Changed == tc.failedPush {
				t.Fatalf("publish known README: %+v, %v", result, err)
			}
			head := auditGitBytes(t, opts.Repo, "rev-parse", "HEAD")
			if tc.failedPush && !bytes.Equal(head, pending) {
				t.Fatal("retry replaced the refused local snapshot commit")
			}
			retry, err := Push(ctx, st, opts)
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), "unverified unpublished") {
					t.Fatalf("unchanged retry accepted unsafe history: %v", err)
				}
				if got := auditGitBytes(t, remote, "for-each-ref"); len(got) != 0 {
					t.Fatalf("unsafe history published: %s", got)
				}
			} else {
				if err != nil || retry.Changed {
					t.Fatalf("unchanged retry: %+v, %v", retry, err)
				}
				if !bytes.Equal(head, auditGitBytes(t, remote, "rev-parse", "refs/heads/main")) {
					t.Fatal("remote did not receive the existing snapshot")
				}
			}
			if !bytes.Equal(head, auditGitBytes(t, opts.Repo, "rev-parse", "HEAD")) {
				t.Fatal("unchanged retry created or replaced a commit")
			}
			runGit(t, opts.Repo, "merge-base", "--is-ancestor", oldHead, "HEAD")
			if !bytes.Equal(oldCommit, auditGitBytes(t, opts.Repo, "cat-file", "commit", oldHead)) ||
				!bytes.Equal(tags, auditGitBytes(t, opts.Repo, "show-ref", "--tags")) ||
				string(auditGitBytes(t, opts.Repo, "show", oldHead+":README.md")) != tc.body {
				t.Fatal("producer commit, README or tags changed")
			}
			after, err := os.Stat(readme)
			if err != nil || after.Mode() != info.Mode() || !bytes.Equal(originalReadme, read(opts.Repo, "README.md")) {
				t.Fatalf("working README bytes or mode changed: %v", err)
			}
			if !bytes.Equal(index, auditGitBytes(t, opts.Repo, "diff", "--cached", "--binary")) ||
				string(read(opts.Repo, "unrelated.txt")) != "working sentinel" ||
				string(read(opts.Repo, "untracked.txt")) != "untracked sentinel" {
				t.Fatal("unrelated index or worktree content changed")
			}
			if !bytes.Equal(identity, read(parent, "age.key")) || !bytes.Equal(config, read(parent, "backup.json")) {
				t.Fatal("private identity or configuration changed")
			}
		})
	}
}
