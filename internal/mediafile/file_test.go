package mediafile

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestConfinedMediaRead(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "nested", "image")
	if err := os.Mkdir(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("exact media bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := Stat(root, name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 17 {
		t.Fatalf("stat: %v, %v", info, err)
	}
	file, err := Open(root, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "exact media bytes" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if !Within(root, name) || !Within(root, root) || Within(root, root+"-neighbor/image") || Within(root, filepath.Dir(root)) || Within(root, "relative") {
		t.Fatal("incorrect selected-root membership")
	}
}

func TestResolvePreservesOSPathMeaning(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "child"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "actual"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{
		{"", base},
		{"target/actual", filepath.Join(target, "actual")},
		{"alias/../actual", filepath.Join(target, "actual")},
		{"alias/../new/leaf", filepath.Join(target, "new", "leaf")},
		{filepath.Join(target, "new", "leaf"), filepath.Join(target, "new", "leaf")},
	} {
		got, err := Resolve(tc.name)
		if err != nil || got != tc.want {
			t.Fatalf("resolve %q: %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	// Compare the raw OS traversal with the resolved, confined read.
	raw, err := os.ReadFile("alias/../actual")
	if err != nil || string(raw) != "target" {
		t.Fatalf("OS traversal: %q, %v", raw, err)
	}
	if err := os.Symlink("missing", "broken"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing/../leaf", "broken", "target/actual/leaf"} {
		if _, err := Resolve(name); err == nil {
			t.Fatalf("resolved invalid traversal %q", name)
		}
	}
}

func TestMediaRejectsAliasesAndSpecialFiles(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "leaf")
	parent := filepath.Join(root, "parent")
	rootAlias := filepath.Join(t.TempDir(), "root")
	for link, target := range map[string]string{leaf: sentinel, parent: outside, rootAlias: root} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "directory")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A Unix socket exercises nonregular refusal without a blocking read.
	socket := filepath.Join(root, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct{ root, name string }{
		{root, sentinel},
		{root, root},
		{root, root + "/directory/../regular"},
		{root, leaf},
		{root, filepath.Join(parent, "sentinel")},
		{rootAlias, filepath.Join(rootAlias, "regular")},
		{regular, filepath.Join(regular, "leaf")},
		{root, dir},
		{root, socket},
		{root, filepath.Join(regular, "leaf")},
		{root, filepath.Join(root, "missing")},
		{filepath.Join(root, "missing"), filepath.Join(root, "missing", "leaf")},
	} {
		if _, err := Stat(tc.root, tc.name); err == nil {
			t.Errorf("stat accepted %q under %q", tc.name, tc.root)
		}
		file, err := Open(tc.root, tc.name)
		if err == nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Errorf("open accepted %q under %q", tc.name, tc.root)
		}
	}
	data, err := os.ReadFile(sentinel) // #nosec G304 -- separate temp sentinel created above; this checks preservation, not absence of reads.
	if err != nil || string(data) != "outside bytes" {
		t.Fatal("outside sentinel changed")
	}
}
