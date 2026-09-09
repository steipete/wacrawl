package mediafile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve preserves OS symlink/.. semantics before cleaning missing suffixes.
func Resolve(name string) (string, error) {
	name = filepath.FromSlash(name)
	if !filepath.IsAbs(name) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		name = cwd + string(filepath.Separator) + name
	}
	var suffix []string
	for {
		if _, err := os.Lstat(name); err == nil {
			resolved, err := filepath.EvalSymlinks(name)
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
		name = strings.TrimRight(name, string(filepath.Separator))
		i := strings.LastIndexByte(name, byte(filepath.Separator))
		if i < 0 || name[i+1:] == ".." || name[i+1:] == "." {
			return "", errors.New("cannot resolve media root")
		}
		suffix = append(suffix, name[i+1:])
		name = name[:i+1]
	}
}

func Within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func Stat(root, name string) (os.FileInfo, error) {
	r, rel, err := openRoot(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return stat(r, rel)
}

func Open(root, name string) (*os.File, error) {
	r, rel, err := openRoot(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	before, err := stat(r, rel)
	if err != nil {
		return nil, err
	}
	f, err := r.OpenFile(rel, readFlags, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, errors.New("media file changed or is not regular")
	}
	return f, nil
}

func openRoot(root, name string) (*os.Root, string, error) {
	if filepath.Clean(name) != name || !Within(root, name) || name == root {
		return nil, "", errors.New("media path is outside its selected root")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("media root is not a regular directory")
	}
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(root)
	return r, rel, err
}

func stat(root *os.Root, rel string) (os.FileInfo, error) {
	var info os.FileInfo
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		name := filepath.Join(parts[:i+1]...)
		var err error
		info, err = root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("media path is not a confined regular file: %q", rel)
		}
	}
	return info, nil
}
