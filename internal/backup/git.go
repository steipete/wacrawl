package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ckbackup "github.com/openclaw/crawlkit/backup"
	"github.com/openclaw/crawlkit/mirror"
)

func mirrorOptions(cfg Config) mirror.Options {
	return mirror.Options{RepoPath: cfg.Repo, Remote: cfg.Remote, Branch: "main"}
}

func syncOptions(cfg Config) mirror.Options {
	opts := mirrorOptions(cfg)
	if _, err := os.Stat(filepath.Join(cfg.Repo, ".git")); err == nil {
		opts.Remote = ""
	}
	return opts
}

func ensureRepo(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.Repo) == "" {
		return fmt.Errorf("backup repo path is required")
	}
	opts := syncOptions(cfg)
	err := mirror.SyncCurrentForWrite(ctx, opts)
	if err == nil || opts.Remote == "" {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(cfg.Repo, ".git")); statErr == nil {
		return err
	}
	local := opts
	local.Remote = ""
	if initErr := mirror.EnsureRepo(ctx, local); initErr != nil {
		return fmt.Errorf("initialize backup repo after clone failed: %w", initErr)
	}
	if remoteErr := mirror.EnsureRemote(ctx, opts); remoteErr != nil {
		return fmt.Errorf("configure backup remote after clone failed: %w", remoteErr)
	}
	return nil
}

func ensureRepoForWrite(ctx context.Context, cfg Config) error {
	if _, err := os.Stat(filepath.Join(cfg.Repo, ".git")); err == nil {
		return mirror.EnsureRepo(ctx, syncOptions(cfg))
	}
	return ensureRepo(ctx, cfg)
}

func ensureRepoForRead(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.Repo) == "" {
		return fmt.Errorf("backup repo path is required")
	}
	return mirror.Fetch(ctx, syncOptions(cfg))
}

func commitAndPush(ctx context.Context, cfg Config, message string, push bool, manifests ...ckbackup.Manifest) (bool, error) {
	paths, err := ownedPathspecs(ctx, cfg, manifests...)
	if err != nil {
		return false, err
	}
	if err := prepareOwnedDeletions(ctx, cfg.Repo, paths); err != nil {
		return false, err
	}
	changed, err := mirror.CommitPaths(ctx, mirrorOptions(cfg), message, paths)
	if err != nil || !push {
		return changed, err
	}
	if err := verifyPendingHistory(ctx, cfg); err != nil {
		return changed, err
	}
	return changed, mirror.PushAtomic(ctx, mirrorOptions(cfg), "HEAD")
}

func prepareOwnedDeletions(ctx context.Context, repo string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"diff", "--cached", "--name-only", "--diff-filter=D", "-z", "--"}, paths...)
	deleted, err := scopeGit(ctx, repo, args...)
	if err != nil {
		return err
	}
	var specs []string
	for _, name := range strings.Split(string(deleted), "\x00") {
		if name != "" {
			specs = append(specs, ":(literal)"+name)
		}
	}
	if len(specs) == 0 {
		return nil
	}
	// The pinned CommitPaths stages by pathname, which requires deleted paths
	// to remain in the index. Only restore owned index entries, not file bytes.
	args = append([]string{"restore", "--source=HEAD", "--staged", "--"}, specs...)
	_, err = scopeGit(ctx, repo, args...)
	return err
}
