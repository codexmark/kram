package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheHitReturnsSavedTools(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := ServerConfig{Command: "mcp-fs", Args: []string{"--root", "."}}
	tools := []Tool{{Name: "read", Description: "reads a file"}}
	saveCacheEntry("fs", cfg, "fs-server 1.0", tools)

	got, info, ok := cachedTools("fs", cfg)
	if !ok {
		t.Fatal("expected a cache hit for the exact config just saved")
	}
	if info != "fs-server 1.0" {
		t.Errorf("serverInfo = %q, want %q", info, "fs-server 1.0")
	}
	if len(got) != 1 || got[0].Name != "read" {
		t.Errorf("tools = %+v, want one tool named read", got)
	}
}

func TestCacheMissWhenNothingSaved(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, _, ok := cachedTools("never-connected", ServerConfig{Command: "x"})
	if ok {
		t.Error("expected a miss when nothing has ever been cached for this server")
	}
}

func TestCacheInvalidatedByFingerprintChange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	original := ServerConfig{Command: "mcp-fs", Args: []string{"--root", "."}}
	saveCacheEntry("fs", original, "fs-server", []Tool{{Name: "read"}})

	changed := ServerConfig{Command: "mcp-fs", Args: []string{"--root", "/other"}}
	if _, _, ok := cachedTools("fs", changed); ok {
		t.Error("a config change (different args) should invalidate the cached entry")
	}

	// The original config must still hit — only the changed one is a miss.
	if _, _, ok := cachedTools("fs", original); !ok {
		t.Error("the original, unchanged config should still hit")
	}
}

func TestCacheInvalidatedByEnvChange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := ServerConfig{Command: "mcp-fs", Env: map[string]string{"TOKEN": "abc"}}
	saveCacheEntry("fs", cfg, "fs-server", []Tool{{Name: "read"}})

	changed := ServerConfig{Command: "mcp-fs", Env: map[string]string{"TOKEN": "xyz"}}
	if _, _, ok := cachedTools("fs", changed); ok {
		t.Error("a changed env value should invalidate the cached entry")
	}
}

func TestCacheExpiredByTTL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg := ServerConfig{Command: "mcp-fs"}
	saveCacheEntry("fs", cfg, "fs-server", []Tool{{Name: "read"}})

	// Rewrite the saved entry with a SavedAt far enough in the past to be
	// past cacheTTL, bypassing the wall-clock wait a real TTL test would
	// otherwise need.
	path, err := cachePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		t.Fatal(err)
	}
	entry := cf.Servers["fs"]
	entry.SavedAt = time.Now().Add(-cacheTTL - time.Hour)
	cf.Servers["fs"] = entry
	rewritten, err := json.Marshal(cf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := cachedTools("fs", cfg); ok {
		t.Error("an entry older than cacheTTL should be treated as a miss")
	}
}

func TestCacheCorruptedFileIsTreatedAsMiss(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path := filepath.Join(dir, "kram-gateway", "mcp-cache.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not valid json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Must not panic or error — a corrupted cache file must never be able
	// to break Kram, just make the cache a no-op.
	if _, _, ok := cachedTools("fs", ServerConfig{Command: "x"}); ok {
		t.Error("a corrupted cache file should be treated as a miss, not parsed as a hit")
	}

	// And saving afterward should still work, overwriting the garbage.
	saveCacheEntry("fs", ServerConfig{Command: "x"}, "info", []Tool{{Name: "t"}})
	if _, _, ok := cachedTools("fs", ServerConfig{Command: "x"}); !ok {
		t.Error("saving after a corrupted read should still succeed and be retrievable")
	}
}

func TestFingerprintStableAcrossMapOrdering(t *testing.T) {
	a := ServerConfig{Command: "x", Env: map[string]string{"A": "1", "B": "2"}}
	b := ServerConfig{Command: "x", Env: map[string]string{"B": "2", "A": "1"}}
	if fingerprint(a) != fingerprint(b) {
		t.Error("fingerprint should be independent of map iteration order")
	}
}

func TestFingerprintIgnoresEnabledField(t *testing.T) {
	on := true
	off := false
	a := ServerConfig{Command: "x", Enabled: &on}
	b := ServerConfig{Command: "x", Enabled: &off}
	if fingerprint(a) != fingerprint(b) {
		t.Error("toggling Enabled should not change the fingerprint — it doesn't change what the server would serve")
	}
}
