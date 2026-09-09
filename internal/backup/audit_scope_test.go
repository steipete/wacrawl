package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ckbackup "github.com/openclaw/crawlkit/backup"
)

func TestAuditBackupAllowsSiblingRepository(t *testing.T) {
	ctx := context.Background()
	st := openFixtureStore(t, "archive.db")
	parent := filepath.Dir(st.Path())
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		ArchivePath: st.Path(), Repo: filepath.Join(parent, "backup"), Remote: remote,
		Identity: filepath.Join(parent, "age.key"), ConfigPath: filepath.Join(parent, "backup.json"),
	}
	if _, _, err := Init(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, st, opts); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{st.Path(), st.Path() + "-wal", st.Path() + "-shm", st.Path() + "-journal", filepath.Join(parent, "media")} {
		bad := opts
		bad.Repo = name
		if err := ValidateWriteOptions(bad); err == nil {
			t.Fatalf("accepted protected archive path %q", name)
		}
	}
}

func TestAuditBackupRejectsSymlinkParentTraversalBeforeWrites(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(source, "nested"), alias); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		SourcePath: source, Repo: alias + string(filepath.Separator) + "../backup", Remote: remote,
		Identity: filepath.Join(t.TempDir(), "age.key"), ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	if _, _, err := Init(context.Background(), opts); err == nil {
		t.Fatal("accepted an OS path resolving inside the selected source")
	}
	for _, name := range []string{filepath.Join(source, "backup"), opts.ConfigPath, opts.Identity} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("rejected layout wrote %q: %v", name, err)
		}
	}
}

func TestAuditBackupRejectsFileSourceHardlink(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	sentinel := []byte("synthetic source sentinel")
	if err := os.WriteFile(source, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "backup.json")
	if err := os.Link(source, config); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Repo: filepath.Join(t.TempDir(), "repo"), Identity: filepath.Join(t.TempDir(), "age.key")}
	if err := validateWriteLayout(cfg, Options{SourcePath: source, ConfigPath: config}); err == nil {
		t.Fatal("accepted a config hardlink to the selected source file")
	}
	got, err := os.ReadFile(source) // #nosec G304 -- fixed source.db sentinel created in t.TempDir; verifies hardlink refusal preserved its bytes.
	if err != nil || string(got) != string(sentinel) {
		t.Fatalf("source changed: %q %v", got, err)
	}
}

func TestAuditInitPreservesIdentityOnCaseInsensitiveFilesystem(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "PROBE")); os.IsNotExist(err) {
		t.Skip("requires a case-insensitive filesystem")
	}
	opts := Options{Repo: filepath.Join(t.TempDir(), "repo"), Identity: filepath.Join(parent, "age.key"), ConfigPath: filepath.Join(parent, "AGE.KEY")}
	if _, _, err := Init(context.Background(), opts); err == nil {
		t.Fatal("accepted case-equivalent config and identity")
	}
	if _, err := RecipientFromIdentity(opts.Identity); err != nil {
		t.Fatalf("generated identity was overwritten: %v", err)
	}
	if _, err := os.Stat(opts.Repo); !os.IsNotExist(err) {
		t.Fatal("initialized publication repository after alias detection")
	}
}

func TestAuditInitChecksConfigCreatedCaseAlias(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "PROBE")); os.IsNotExist(err) {
		t.Skip("requires a case-insensitive filesystem")
	} else if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		Repo: filepath.Join(parent, "backup"), Remote: remote,
		ConfigPath: filepath.Join(parent, "BACKUP", "backup.json"), Identity: filepath.Join(t.TempDir(), "age.key"),
	}
	if _, _, err := Init(context.Background(), opts); err == nil {
		t.Fatal("accepted configuration inside a case-equivalent repository")
	}
	if _, err := os.Stat(filepath.Join(opts.Repo, ".git")); !os.IsNotExist(err) {
		t.Fatalf("initialized Git around the configuration: %v", err)
	}
}

func TestAuditPushChecksManifestTypeBeforeReading(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
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
			manifest := filepath.Join(cfg.Repo, "manifest.json")
			if kind == "directory" {
				err = os.Mkdir(manifest, 0o700)
			} else {
				err = os.Symlink(t.TempDir(), manifest)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Push(ctx, openFixtureStore(t, "archive.db"), opts); err == nil || !strings.Contains(err.Error(), "not a regular confined file") {
				t.Fatalf("expected type validation before the manifest reader, got %v", err)
			}
		})
	}
}

func TestAuditBackupCommitOwnsLiteralCurrentAndPriorPaths(t *testing.T) {
	testAuditBackupCommitOwnsLiteralCurrentAndPriorPaths(t, false)
}

func TestAuditBackupCommitIncludesStagedOwnedDeletion(t *testing.T) {
	testAuditBackupCommitOwnsLiteralCurrentAndPriorPaths(t, true)
}

func testAuditBackupCommitOwnsLiteralCurrentAndPriorPaths(t *testing.T, stagedDeletion bool) {
	t.Helper()
	ctx := context.Background()
	cfg := Config{Repo: t.TempDir()}
	recipient, err := EnsureIdentity(filepath.Join(t.TempDir(), "age.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		full := filepath.Join(cfg.Repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		content := []byte("synthetic fixture\n")
		if name == "README.md" {
			content = []byte(backupReadme)
		}
		if strings.HasSuffix(name, ".age") {
			content, _, err = ckbackup.EncryptShard(content, []string{recipient})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := ckbackup.Manifest{Shards: []ckbackup.ShardEntry{{Path: "data/prior.age"}}}
	for _, name := range []string{"README.md", "manifest.json", "data/prior.age"} {
		write(name)
	}
	if changed, err := commitAndPush(ctx, cfg, "test: prior", false, old); err != nil || !changed {
		t.Fatalf("prior commit: %v, %v", changed, err)
	}
	for _, name := range []string{"notes.txt", "archive.db", "data/private.txt", "data/object1.age", "data/object[1].age", "data/files/generation-object.age"} {
		write(name)
	}
	runGit(t, cfg.Repo, "add", "--", "notes.txt", "archive.db", "data/private.txt")
	before, err := scopeGit(ctx, cfg.Repo, "diff", "--cached", "--binary")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.Repo, "data", "prior.age")); err != nil {
		t.Fatal(err)
	}
	if stagedDeletion {
		runGit(t, cfg.Repo, "add", "--", "data/prior.age")
	}
	old.Shards = append(old.Shards, ckbackup.ShardEntry{Path: "data/never-tracked.age"})
	current := ckbackup.Manifest{
		Shards: []ckbackup.ShardEntry{{Path: "data/object[1].age"}},
		Files:  []ckbackup.FileEntry{{Shard: "data/files/generation-object.age"}},
	}
	if changed, err := commitAndPush(ctx, cfg, "test: current", false, old, current); err != nil || !changed {
		t.Fatalf("current commit: %v, %v", changed, err)
	}
	after, err := scopeGit(ctx, cfg.Repo, "diff", "--cached", "--binary")
	if err != nil || string(after) != string(before) {
		t.Fatalf("unrelated index changed: %v\n%s", err, after)
	}
	tree, err := scopeGit(ctx, cfg.Repo, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"notes.txt", "archive.db", "data/private.txt", "data/object1.age", "data/prior.age"} {
		if strings.Contains("\n"+string(tree), "\n"+unwanted+"\n") {
			t.Fatalf("unowned or deleted path committed: %s", unwanted)
		}
	}
	for _, wanted := range []string{"data/object[1].age", "data/files/generation-object.age"} {
		if !strings.Contains("\n"+string(tree), "\n"+wanted+"\n") {
			t.Fatalf("owned path missing: %s", wanted)
		}
	}
}

func TestAuditInitRejectsIdentityOverlapBeforeWrites(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	opts := Options{
		Repo: repo, Identity: filepath.Join(repo, "age.key"),
		ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	if _, _, err := Init(context.Background(), opts); err == nil {
		t.Fatal("identity inside backup accepted")
	}
	for _, name := range []string{repo, opts.ConfigPath} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("rejection wrote %s: %v", filepath.Base(name), err)
		}
	}
}

func TestAuditBackupRejectsAliasesAndUnownedManifestPaths(t *testing.T) {
	for _, name := range []string{"../key.age", "data/../key.age", "data/.git/key.age", "data/key.txt", `data\key.age`} {
		if _, err := ownedArtifactPaths(ckbackup.Manifest{Shards: []ckbackup.ShardEntry{{Path: name}}}); err == nil {
			t.Fatalf("accepted invalid artifact %q", name)
		}
	}
	cfg := Config{Repo: t.TempDir(), Identity: filepath.Join(t.TempDir(), "age.key")}
	if err := os.WriteFile(cfg.Identity, []byte("synthetic identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(cfg.Identity, filepath.Join(cfg.Repo, "README.md")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if err := validateOwnedFiles(cfg, []string{"README.md"}); err == nil {
		t.Fatal("identity hardlink accepted")
	}
}

func TestAuditBackupRejectsPlaintextCiphertextRole(t *testing.T) {
	cfg := Config{Repo: t.TempDir()}
	if err := ensureRepo(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Repo, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Repo, "data", "not-encrypted.age"), []byte("synthetic private content"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := ckbackup.Manifest{Shards: []ckbackup.ShardEntry{{Path: "data/not-encrypted.age"}}}
	if _, err := commitAndPush(context.Background(), cfg, "test", false, manifest); err == nil {
		t.Fatal("plaintext .age file was eligible for commit")
	}
	index, err := scopeGit(context.Background(), cfg.Repo, "ls-files")
	if err != nil || len(index) != 0 {
		t.Fatalf("rejected plaintext changed index: %s, %v", index, err)
	}
}

func TestAuditBackupPreservesUnownedCiphertext(t *testing.T) {
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	opts := Options{
		Repo: t.TempDir(), Remote: remote, Identity: filepath.Join(t.TempDir(), "age.key"),
		ConfigPath: filepath.Join(t.TempDir(), "backup.json"),
	}
	if _, _, err := Init(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(opts.Repo, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(opts.Repo, "data", "unowned.age")
	content := []byte("synthetic unowned file")
	if err := os.WriteFile(unowned, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, openFixtureStore(t, "archive.db"), opts); err == nil || !strings.Contains(err.Error(), "unowned ciphertext") {
		t.Fatalf("unowned cleanup was not blocked: %v", err)
	}
	got, err := os.ReadFile(unowned) // #nosec G304 -- fixed temp data/unowned.age sentinel verifies the refused push left its bytes unchanged.
	if err != nil || string(got) != string(content) {
		t.Fatalf("unowned file changed: %q, %v", got, err)
	}
}

func TestAuditBackupRejectsOutputInputAliases(t *testing.T) {
	for _, kind := range []string{"config-identity", "config-archive", "identity-archive", "identity-source"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{Repo: filepath.Join(t.TempDir(), "repo"), Identity: filepath.Join(dir, "age.key")}
			opts := Options{ConfigPath: filepath.Join(dir, "backup.json"), ArchivePath: filepath.Join(dir, "archive.db"), SourcePath: t.TempDir()}
			switch kind {
			case "config-identity":
				opts.ConfigPath = cfg.Identity
			case "config-archive":
				opts.ConfigPath = opts.ArchivePath
			case "identity-archive":
				cfg.Identity = opts.ArchivePath
			case "identity-source":
				cfg.Identity = filepath.Join(opts.SourcePath, "age.key")
			}
			if err := validateWriteLayout(cfg, opts); err == nil {
				t.Fatal("accepted output/input alias")
			}
		})
	}
}
