package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"filippo.io/age"
	ckbackup "github.com/openclaw/crawlkit/backup"
)

// Check unpublished ancestry, not just the prospective commit. A scoped commit
// cannot remove plaintext already reachable from an older unpublished commit.
func verifyPendingHistory(ctx context.Context, cfg Config) error {
	if err := verifyPushDestination(ctx, cfg.Repo); err != nil {
		return err
	}
	branch, err := scopeGit(ctx, cfg.Repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return err
	}
	remote, err := scopeGit(ctx, cfg.Repo, "ls-remote", "--refs", "origin", "refs/heads/"+strings.TrimSpace(string(branch)))
	if err != nil {
		return err
	}
	args := []string{"rev-list", "--parents", "--max-count=129", "HEAD"}
	if fields := strings.Fields(string(remote)); len(fields) != 0 {
		if len(fields) != 2 {
			return errors.New("cannot establish backup remote baseline")
		}
		args = append(args, "^"+fields[0])
	}
	pending, err := scopeGit(ctx, cfg.Repo, args...)
	if err != nil {
		return fmt.Errorf("cannot establish unpublished backup history; inspect the remote baseline: %w", err)
	}
	lines := strings.FieldsFunc(string(pending), func(r rune) bool { return r == '\n' })
	if len(lines) > 128 {
		return errors.New("more than 128 unpublished backup commits require independent history review")
	}
	for _, line := range lines {
		refs := strings.Fields(line)
		if len(refs) == 0 || len(refs) > 2 {
			return errors.New("unpublished backup merge history requires independent review")
		}
		current, err := publicationManifest(ctx, cfg.Repo, refs[0])
		if err != nil {
			return fmt.Errorf("inspect unpublished backup %.12s: %w", refs[0], err)
		}
		var prior ckbackup.Manifest
		if len(refs) == 2 {
			prior, err = publicationManifest(ctx, cfg.Repo, refs[1])
			if err != nil {
				return err
			}
		}
		if prior.Format != 0 && current.Format == 0 {
			return unsafeHistory(refs[0], "manifest.json")
		}
		if err := verifyManifestArtifacts(ctx, cfg.Repo, refs[0], current); err != nil {
			return err
		}
		names, err := ownedArtifactPaths(prior, current)
		if err != nil {
			return err
		}
		owned := map[string]bool{}
		for _, name := range names {
			owned[name] = true
		}
		changed, err := scopeGit(ctx, cfg.Repo, "diff-tree", "--root", "--no-commit-id", "--no-renames", "-r", "--name-only", "-z", refs[0])
		if err != nil {
			return err
		}
		for _, name := range strings.Split(string(changed), "\x00") {
			if name == "" {
				continue
			}
			if !owned[name] {
				return unsafeHistory(refs[0], name)
			}
			entry, err := scopeGit(ctx, cfg.Repo, "ls-tree", "-z", refs[0], "--", ":(literal)"+name)
			if err != nil {
				return err
			}
			if len(entry) == 0 {
				continue
			}
			if !bytes.HasPrefix(entry, []byte("100644 blob ")) {
				return unsafeHistory(refs[0], name)
			}
			switch name {
			case "manifest.json":
				if current.Format != formatVersion || !current.Encrypted {
					return unsafeHistory(refs[0], name)
				}
			case "README.md":
				body, err := scopeGit(ctx, cfg.Repo, "show", refs[0]+":"+name)
				if err != nil || !ownedBackupReadme(body) {
					return unsafeHistory(refs[0], name)
				}
			default:
				if err := verifyGitCiphertext(ctx, cfg.Repo, refs[0], name, current); err != nil {
					return unsafeHistory(refs[0], name)
				}
			}
		}
	}
	return nil
}

func verifyManifestArtifacts(ctx context.Context, repo, ref string, manifest ckbackup.Manifest) error {
	names, err := ownedArtifactPaths(manifest)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "README.md" || name == "manifest.json" {
			continue
		}
		entry, err := scopeGit(ctx, repo, "ls-tree", "-z", ref, "--", ":(literal)"+name)
		if err != nil || !bytes.HasPrefix(entry, []byte("100644 blob ")) {
			return unsafeHistory(ref, name)
		}
		if err := verifyGitCiphertext(ctx, repo, ref, name, manifest); err != nil {
			return unsafeHistory(ref, name)
		}
	}
	return nil
}

func verifyPushDestination(ctx context.Context, repo string) error {
	fetch, err := scopeGit(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	push, err := scopeGit(ctx, repo, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(push)) != strings.TrimSpace(string(fetch)) {
		return errors.New("backup requires one push destination matching the inspected remote; review origin push URLs before retrying")
	}
	return nil
}

func publicationManifest(ctx context.Context, repo, ref string) (ckbackup.Manifest, error) {
	var manifest ckbackup.Manifest
	entry, err := scopeGit(ctx, repo, "ls-tree", "-z", ref, "--", "manifest.json")
	if err != nil || len(entry) == 0 {
		return manifest, err
	}
	if !bytes.HasPrefix(entry, []byte("100644 blob ")) {
		return manifest, errors.New("unpublished manifest is not a regular file")
	}
	data, err := scopeGit(ctx, repo, "show", ref+":manifest.json")
	if err != nil {
		return manifest, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Public FileEntry has only shard and bytes; logical file metadata belongs
	// in the encrypted index, so unknown public fields must remain rejected.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, errors.New("unpublished backup manifest is invalid")
	}
	if err := uniqueManifestKeys(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return manifest, err
	}
	if manifest.Format != formatVersion || !manifest.Encrypted {
		return manifest, errors.New("unpublished backup manifest is not a supported encrypted snapshot")
	}
	if err := validatePublicManifest(manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func validatePublicManifest(manifest ckbackup.Manifest) error {
	if manifest.Exported.IsZero() || len(manifest.Recipients) == 0 {
		return errors.New("unpublished manifest lacks snapshot metadata")
	}
	for _, recipient := range manifest.Recipients {
		parsed, err := age.ParseX25519Recipient(recipient)
		if err != nil || parsed.String() != recipient {
			return errors.New("unpublished manifest contains an invalid public recipient")
		}
	}
	for name, count := range manifest.Counts {
		switch name {
		case "contacts", "chats", "groups", "participants", "group_participants", "messages", "message_revisions", "archive_identity", "media_files":
		default:
			return errors.New("unpublished manifest contains an unknown count")
		}
		if count < 0 {
			return errors.New("unpublished manifest contains an invalid count")
		}
	}
	for _, shard := range manifest.Shards {
		switch shard.Table {
		case "contacts", "chats", "groups", "group_participants", "messages", "message_revisions", "archive_identity", "_backup_files":
		default:
			return errors.New("unpublished manifest contains an unknown table")
		}
		digest, err := hex.DecodeString(shard.SHA256)
		if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != shard.SHA256 || shard.Rows < 0 || shard.Bytes <= 0 {
			return errors.New("unpublished manifest contains invalid shard metadata")
		}
	}
	_, err := ownedArtifactPaths(manifest)
	return err
}

func uniqueManifestKeys(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("unpublished manifest nesting is invalid")
	}
	token, err := decoder.Token()
	if err != nil {
		return errors.New("unpublished manifest is invalid")
	}
	if delimiter, ok := token.(json.Delim); ok {
		keys := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return errors.New("unpublished manifest key is invalid")
				}
				name, ok := key.(string)
				name = strings.ToLower(name)
				if !ok || keys[name] {
					return errors.New("unpublished manifest contains duplicate keys")
				}
				keys[name] = true
			}
			if err := uniqueManifestKeys(decoder, depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return errors.New("unpublished manifest is invalid")
		}
	}
	if depth == 0 {
		if _, err := decoder.Token(); err != io.EOF {
			return errors.New("unpublished manifest contains trailing data")
		}
	}
	return nil
}

func verifyGitCiphertext(ctx context.Context, repo, ref, name string, manifest ckbackup.Manifest) error {
	expected := int64(-1)
	for _, entry := range manifest.Shards {
		if entry.Path == name {
			expected = entry.Bytes
		}
	}
	for _, entry := range manifest.Files {
		if entry.Shard == name {
			expected = entry.Bytes
		}
	}
	size, err := scopeGit(ctx, repo, "cat-file", "-s", ref+":"+name)
	if err != nil {
		return err
	}
	actual, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
	if err != nil || expected < 1 || actual != expected {
		return errors.New("unpublished ciphertext size differs from its manifest")
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", ref+":"+name) // #nosec G204 -- Git-derived ref and validated owned path, checked for regular blob mode and manifest size; no shell.
	cmd.Dir = repo
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		return err
	}
	_, headerErr := age.ExtractHeader(io.LimitReader(out, 1<<20))
	// Closing after the bounded header read may cause Git to exit on SIGPIPE.
	_ = out.Close()
	_ = cmd.Wait()
	return headerErr
}

func unsafeHistory(ref, name string) error {
	if len(name) > 160 {
		name = name[:160] + "..."
	}
	return fmt.Errorf("refusing unverified unpublished backup commit %.12s path %q; history requires independent review", ref, name)
}
