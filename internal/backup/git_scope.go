package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	ckbackup "github.com/openclaw/crawlkit/backup"
)

// ValidateWriteOptions runs before the CLI opens or syncs an archive.
func ValidateWriteOptions(opts Options) error {
	cfg, err := ResolveOptions(opts)
	if err != nil {
		return err
	}
	return validateWriteLayout(cfg, opts)
}

func validateWriteLayout(cfg Config, opts Options) error {
	repo, err := resolvedPath(cfg.Repo)
	if err != nil {
		return err
	}
	identity, err := resolvedPath(cfg.Identity)
	if err != nil {
		return err
	}
	if pathWithin(repo, identity) || pathWithin(identity, repo) {
		return errors.New("backup identity must be outside the backup repository")
	}
	var archivePaths []string
	if opts.ArchivePath != "" {
		archivePaths = []string{opts.ArchivePath, opts.ArchivePath + "-wal", opts.ArchivePath + "-shm", opts.ArchivePath + "-journal", filepath.Join(filepath.Dir(opts.ArchivePath), "media")}
	}
	for _, protected := range append(append([]string{}, archivePaths...), opts.SourcePath) {
		if protected == "" {
			continue
		}
		root, err := resolvedPath(protected)
		if err != nil {
			return err
		}
		if protected == opts.SourcePath {
			if info, err := os.Stat(root); err == nil && !info.IsDir() {
				root = filepath.Dir(root)
			}
		}
		if pathWithin(root, repo) || pathWithin(repo, root) {
			return errors.New("backup repository overlaps the archive or selected source")
		}
	}
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = DefaultConfigPath()
	}
	configPath, err = resolvedPath(expandHome(configPath))
	if err != nil {
		return err
	}
	if pathWithin(repo, configPath) {
		return errors.New("backup configuration must be outside the backup repository")
	}
	if pathsAlias(identity, configPath) {
		return errors.New("backup configuration aliases the private identity")
	}
	for _, protected := range archivePaths {
		archive, err := resolvedPath(protected)
		if err != nil {
			return err
		}
		if pathsAlias(archive, configPath) || pathsAlias(archive, identity) ||
			pathWithin(archive, configPath) || pathWithin(archive, identity) {
			return errors.New("backup configuration or identity aliases the archive")
		}
	}
	if opts.SourcePath != "" {
		source, err := resolvedPath(opts.SourcePath)
		if err != nil {
			return err
		}
		if pathsAlias(source, configPath) || pathsAlias(source, identity) {
			return errors.New("backup configuration or identity aliases the selected source")
		}
		if info, err := os.Stat(source); err == nil && !info.IsDir() {
			source = filepath.Dir(source)
		}
		if pathWithin(source, configPath) || pathWithin(source, identity) {
			return errors.New("backup configuration or identity overlaps the selected source")
		}
	}
	return nil
}

func pathsAlias(left, right string) bool {
	if left == right {
		return true
	}
	a, aErr := os.Stat(left)
	b, bErr := os.Stat(right)
	return aErr == nil && bErr == nil && os.SameFile(a, b)
}

func resolvedPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("backup path is required")
	}
	absolute := filepath.FromSlash(name)
	if !filepath.IsAbs(absolute) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		absolute = cwd + string(filepath.Separator) + absolute
	}
	current := absolute
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// Preserve symlink/.. OS semantics until the existing prefix is resolved.
		// filepath.Abs/Dir would clean those components before EvalSymlinks.
		current = strings.TrimRight(current, string(filepath.Separator))
		i := strings.LastIndexByte(current, byte(filepath.Separator))
		if i < 0 {
			return "", fmt.Errorf("cannot resolve backup path")
		}
		part := current[i+1:]
		if part == "." || part == ".." {
			return "", errors.New("cannot resolve traversal through a missing backup path")
		}
		suffix = append(suffix, part)
		current = current[:i+1]
	}
}

func pathWithin(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return true
	}
	for current := name; ; current = filepath.Dir(current) {
		if pathsAlias(root, current) {
			return true
		}
		if filepath.Dir(current) == current {
			return false
		}
	}
}

func ownedArtifactPaths(manifests ...ckbackup.Manifest) ([]string, error) {
	paths := map[string]bool{"README.md": true}
	for _, manifest := range manifests {
		paths["manifest.json"] = true
		for _, shard := range manifest.Shards {
			if err := validateArtifactPath(shard.Path); err != nil {
				return nil, err
			}
			paths[shard.Path] = true
		}
		for _, file := range manifest.Files {
			if err := validateArtifactPath(file.Shard); err != nil {
				return nil, err
			}
			paths[file.Shard] = true
		}
	}
	out := make([]string, 0, len(paths))
	for name := range paths {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func validateArtifactPath(name string) error {
	if name != path.Clean(name) || name != strings.TrimSpace(name) ||
		strings.ContainsAny(name, "\\\x00") || !strings.HasPrefix(name, "data/") || !strings.HasSuffix(name, ".age") {
		return errors.New("invalid owned backup artifact path")
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return errors.New("backup artifact overlaps Git metadata")
		}
	}
	return nil
}

func validateOwnedFiles(cfg Config, names []string) error {
	identity, identityErr := os.Stat(cfg.Identity)
	if identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return identityErr
	}
	for _, name := range names {
		parts := strings.Split(name, "/")
		current := cfg.Repo
		for i, part := range parts {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
				return fmt.Errorf("backup artifact is not a regular confined file: %q", name)
			}
			if identityErr == nil && os.SameFile(identity, info) {
				return errors.New("backup artifact aliases the private identity")
			}
		}
	}
	return nil
}

func ownedPathspecs(ctx context.Context, cfg Config, manifests ...ckbackup.Manifest) ([]string, error) {
	names, err := ownedArtifactPaths(manifests...)
	if err != nil {
		return nil, err
	}
	if err := validateOwnedFiles(cfg, names); err != nil {
		return nil, err
	}
	tracked, err := scopeGit(ctx, cfg.Repo, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	deleted, err := scopeGit(ctx, cfg.Repo, "diff", "--cached", "--name-only", "--diff-filter=D", "-z")
	if err != nil {
		return nil, err
	}
	index := map[string]bool{}
	for _, name := range strings.Split(string(tracked), "\x00") {
		index[name] = true
	}
	for _, name := range strings.Split(string(deleted), "\x00") {
		index[name] = true
	}
	var specs []string
	for _, name := range names {
		if name == "README.md" {
			content, err := os.ReadFile(filepath.Join(cfg.Repo, name))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if string(content) != backupReadme {
				continue
			}
		}
		_, err := os.Lstat(filepath.Join(cfg.Repo, filepath.FromSlash(name)))
		if err == nil || index[name] {
			if err == nil && strings.HasSuffix(name, ".age") {
				if err := validateCiphertextFile(cfg.Repo, name); err != nil {
					return nil, err
				}
			}
			specs = append(specs, ":(literal)"+name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return specs, nil
}

func validateCiphertextFile(repo, name string) error {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("backup ciphertext is not a regular file")
	}
	// Parse with the age implementation without loading a private identity.
	// This validates the encrypted envelope, not the plaintext checksum.
	_, err = age.ExtractHeader(io.LimitReader(f, 1<<20))
	if err != nil {
		return fmt.Errorf("backup artifact is not an age ciphertext: %q", name)
	}
	return nil
}

func validateSnapshotInputs(ctx context.Context, cfg Config, manifest ckbackup.Manifest) error {
	names, err := ownedArtifactPaths(manifest)
	if err != nil {
		return err
	}
	if err := validateOwnedFiles(cfg, names); err != nil {
		return err
	}
	owned := make(map[string]bool, len(names))
	for _, name := range names {
		owned[name] = true
	}
	// The pinned writer may touch logical paths not present in the old manifest.
	// Check the data tree before calling it, including identity hardlink aliases.
	return filepath.WalkDir(filepath.Join(cfg.Repo, "data"), func(name string, entry os.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(cfg.Repo, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, ".age") && !owned[rel] {
			return fmt.Errorf("unowned ciphertext %q would be removed by the pinned backup writer; inspect it before retrying", rel)
		}
		return validateOwnedFiles(cfg, []string{rel})
	})
}

func scopeGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	var out boundedGitOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("inspect backup Git state: %w", err)
	}
	return out.Bytes(), nil
}

type boundedGitOutput struct{ buffer bytes.Buffer }

func (b *boundedGitOutput) Write(p []byte) (int, error) {
	if len(p) > (8<<20)-b.buffer.Len() {
		return 0, errors.New("backup Git inspection exceeds 8 MiB")
	}
	return b.buffer.Write(p)
}

func (b *boundedGitOutput) Bytes() []byte { return b.buffer.Bytes() }
