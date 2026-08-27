// Package localstore centralizes the one thing every small on-disk store
// in Kram (credentials, tool settings, onboarding, custom providers,
// config, permissions) needs and several of them got wrong: writing a
// file such that a reader — or a crash mid-write — never observes a
// half-written or truncated file.
//
// Before this, config and permissions wrote atomically (temp file +
// rename) while credentials, toolsettings, onboarding and customprovider
// used a raw os.WriteFile that truncates the target in place. So the file
// whose corruption hurts most — credentials.json, holding API keys and
// OAuth tokens — was among the least protected. AtomicWrite makes the
// safe path the only path, by construction rather than by each store
// remembering to reimplement it.
package localstore

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWrite writes data to path so a reader never sees a partial file:
// it writes a sibling temp file, fsyncs it, then renames it into place (an
// atomic operation on POSIX). Parent directories are created as needed.
//
// The fsync before rename matters for power loss specifically: a plain
// write-then-rename protects against a reader (or a process crash)
// observing a half-written file, but without the fsync the rename can
// become visible before the data blocks reach disk, so a power cut can
// leave a file that exists but is empty or truncated. fsync forces the
// bytes down first, so the rename only ever exposes fully-written data.
//
// On Windows os.Rename fails if the destination already exists, so the
// existing file is removed first — leaving a brief window where neither
// the old nor the new file exists. That's still strictly better than a
// reader observing a half-written file: a missing file is the normal
// first-run state every store here already handles, whereas a truncated
// one is a parse error that can brick the store.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := writeAndSync(tmp, data, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return fmt.Errorf("removing existing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s to %s: %w", tmp, path, err)
	}
	return nil
}

// writeAndSync writes data to tmp and fsyncs it to disk before returning,
// so the bytes are durable before the caller renames the file into place.
func writeAndSync(tmp string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp, err)
	}
	return nil
}
