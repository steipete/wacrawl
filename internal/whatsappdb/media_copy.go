package whatsappdb

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openclaw/wacrawl/internal/mediafile"
)

var (
	copyMediaBytes = io.Copy
	syncMediaCopy  = (*os.File).Sync
	closeMediaCopy = (*os.File).Close
)

func sourceMediaRoot(sourceRoot, src string) (string, error) {
	if filepath.Clean(src) != src {
		return "", errors.New("invalid media source path")
	}
	for _, root := range []string{filepath.Join(sourceRoot, "Message", "Media"), filepath.Join(sourceRoot, "Media")} {
		if src != root && mediafile.Within(root, src) {
			// Message itself must not redirect the selected media root.
			relative, _ := filepath.Rel(sourceRoot, root)
			for current := root; current != sourceRoot; current = filepath.Dir(current) {
				info, err := os.Lstat(current)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return "", err
				}
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return "", fmt.Errorf("invalid selected media root %q", relative)
				}
			}
			return root, nil
		}
	}
	return "", errors.New("media source is outside Message/Media and Media")
}

func prepareMediaRoot(sourceRoot, mediaRoot string) (string, error) {
	source, err := mediafile.Resolve(sourceRoot)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(mediaRoot); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", errors.New("archive media root is not a regular directory")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	root, err := mediafile.Resolve(mediaRoot)
	if err != nil {
		return "", err
	}
	if containsMediaPath(source, root) || containsMediaPath(root, source) {
		return "", errors.New("archive media root overlaps the selected source")
	}
	return root, nil
}

func containsMediaPath(root, name string) bool {
	if mediafile.Within(root, name) {
		return true
	}
	info, err := os.Stat(root)
	if err != nil {
		return false
	}
	// Existing ancestors expose case-equivalent names and other filesystem
	// aliases before a new output directory is created.
	for current := name; ; current = filepath.Dir(current) {
		if candidate, err := os.Stat(current); err == nil && os.SameFile(info, candidate) {
			return true
		}
		if filepath.Dir(current) == current {
			return false
		}
	}
}

func copyMediaFile(sourceRoot, src, mediaRoot string) (string, error) {
	info, err := mediafile.Stat(sourceRoot, src)
	if err != nil {
		return "", normalizeMediaReadError(src, err)
	}
	if !mediaMaterialized(info) {
		return "", fmt.Errorf("%w: %s", errMediaNotDownloaded, src)
	}
	in, err := openMediaFileForCopy(sourceRoot, src)
	if err != nil {
		return "", normalizeMediaReadError(src, err)
	}
	defer func() { _ = in.Close() }()
	opened, err := in.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", errors.New("media source changed before copy")
	}
	if !mediaMaterialized(opened) {
		return "", errMediaNotDownloaded
	}
	if err := os.MkdirAll(filepath.Dir(mediaRoot), 0o700); err != nil {
		return "", err
	}
	parent, err := os.OpenRoot(filepath.Dir(mediaRoot))
	if err != nil {
		return "", err
	}
	defer func() { _ = parent.Close() }()
	// The backup collector includes hidden files under media, so stage beside it.
	stage, err := os.MkdirTemp(filepath.Dir(mediaRoot), ".wacrawl-media-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return "", err
	}
	defer func() { _ = stageRoot.Close() }()
	out, err := stageRoot.OpenFile("object", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	size, copyErr := copyMediaBytes(io.MultiWriter(out, hash), in)
	syncErr := syncMediaCopy(out)
	closeErr := closeMediaCopy(out)
	if copyErr != nil && syncErr == nil && closeErr == nil {
		return "", normalizeMediaReadError(src, copyErr)
	}
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return "", err
	}
	after, err := in.Stat()
	if err != nil || size != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return "", errors.New("media source changed during copy")
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if err := os.MkdirAll(mediaRoot, 0o700); err != nil {
		return "", err
	}
	rootInfo, err := os.Lstat(mediaRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("archive media root changed")
	}
	root, err := os.OpenRoot(mediaRoot)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(digest[:2], 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	dir, err := root.Lstat(digest[:2])
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("archive object directory is not regular")
	}
	dest := filepath.Join(mediaRoot, digest[:2], digest)
	// Link never overwrites an existing object. Only a complete stage is visible.
	if err := parent.Link(filepath.Join(filepath.Base(stage), "object"), filepath.Join(filepath.Base(mediaRoot), digest[:2], digest)); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	existing, err := mediafile.Open(mediaRoot, dest)
	if err != nil {
		return "", err
	}
	defer func() { _ = existing.Close() }()
	destInfo, err := existing.Stat()
	if err != nil || os.SameFile(opened, destInfo) {
		return "", errors.New("archive object aliases the source")
	}
	check := sha256.New()
	n, err := io.Copy(check, existing)
	if err != nil || n != size || !strings.EqualFold(fmt.Sprintf("%x", check.Sum(nil)), digest) {
		return "", errors.New("existing archive object does not match its content address")
	}
	return dest, nil
}
