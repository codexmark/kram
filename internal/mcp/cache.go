// Schema cache: a best-effort, on-disk record of what tools/list (plus
// serverInfo) returned for a given server the last time Kram actually
// connected to it. Keyed by server name + a fingerprint of the connection
// config that produced it, so a config edit invalidates the old entry
// automatically rather than serving stale tools under a changed identity.
//
// Single file (kramhome/mcp-cache.json), not one file per server: with the
// number of MCP servers a real workspace configures (single digits, almost
// always), one small JSON file is simpler to read, write, and reason about
// atomically than a directory of them — no directory-listing, no per-file
// locking, no orphaned files left behind when a server is removed from
// mcp.json. The whole file is read, mutated in memory, and rewritten on
// every save, which is fine at this scale.
//
// What this delivers today: after every real tools/list (initial connect,
// a successful reconnect, or a tools/list_changed-triggered refresh), the
// result is written here. That makes the cache useful for diagnostics (an
// operator or a future tool can answer "what did this server last
// advertise, and when" without starting it) and gives a cheap fingerprint
// check that a config change actually altered the connection identity.
//
// What this does NOT do yet: nothing in Connect/ConnectAll consults the
// cache to decide whether it's safe to skip starting a stdio process for
// pure discovery — ConnectAll's contract (always dial every enabled
// server at startup, logging and skipping failures) is left exactly as it
// was. cachedTools below is the seam a future "trust the cache, connect
// lazily" mode would call; it's unused by production code today except by
// its own tests, which is deliberate infrastructure-ahead-of-need rather
// than a half-wired feature.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/codexmark/kram/internal/kramhome"
)

// cacheTTL bounds how long a cached schema is trusted even if the
// fingerprint still matches — a server can change its own tool set
// without Kram's config changing at all (a version bump, a feature flag),
// and 24h keeps a long-running install from trusting a schema that's
// meaningfully out of date.
const cacheTTL = 24 * time.Hour

// cacheMu serializes read-modify-write access to the cache file across
// goroutines in this process (reconnects for different servers can finish
// concurrently and both want to save).
var cacheMu sync.Mutex

type cacheEntry struct {
	Fingerprint string    `json:"fingerprint"`
	SavedAt     time.Time `json:"savedAt"`
	ServerInfo  string    `json:"serverInfo"`
	Tools       []Tool    `json:"tools"`
}

type cacheFile struct {
	Servers map[string]cacheEntry `json:"servers"`
}

func cachePath() (string, error) {
	return kramhome.Path("mcp-cache.json")
}

// fingerprint hashes every connection-identity field of cfg — the fields
// that determine what process gets started or what endpoint gets dialed.
// Enabled is deliberately excluded: toggling a server off and back on
// doesn't change what it would serve, so it shouldn't invalidate the
// cache. Map fields are sorted before hashing so the same config produces
// the same fingerprint regardless of Go's randomized map iteration order.
func fingerprint(cfg ServerConfig) string {
	h := sha256.New()
	fmt.Fprintf(h, "kind=%d\n", cfg.kind())
	fmt.Fprintf(h, "command=%s\n", cfg.Command)
	for _, a := range cfg.Args {
		fmt.Fprintf(h, "arg=%s\n", a)
	}
	for _, k := range sortedKeys(cfg.Env) {
		fmt.Fprintf(h, "env=%s=%s\n", k, cfg.Env[k])
	}
	fmt.Fprintf(h, "url=%s\n", cfg.URL)
	for _, k := range sortedKeys(cfg.Headers) {
		fmt.Fprintf(h, "header=%s=%s\n", k, cfg.Headers[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadCacheFile reads the cache, tolerating everything short of a panic:
// a missing file (the common case — nothing's been cached yet), a
// malformed one (corrupted by a crash mid-write, or hand-edited), or
// anything else unreadable all come back as an empty cache. A schema
// cache is an optimization; it must never be the thing that breaks Kram.
func loadCacheFile() cacheFile {
	empty := cacheFile{Servers: map[string]cacheEntry{}}
	path, err := cachePath()
	if err != nil {
		return empty
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return empty
	}
	if cf.Servers == nil {
		cf.Servers = map[string]cacheEntry{}
	}
	return cf
}

// saveCacheEntry records the schema a real tools/list just returned for
// name. Best-effort by design (same as every other local-state writer in
// this codebase — credentials, toolsettings): a failed write here costs
// nothing but the optimization itself, never a reason to fail the caller
// that just successfully talked to a live MCP server.
func saveCacheEntry(name string, cfg ServerConfig, serverInfo string, tools []Tool) {
	path, err := cachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}

	cacheMu.Lock()
	defer cacheMu.Unlock()

	cf := loadCacheFile()
	cf.Servers[name] = cacheEntry{
		Fingerprint: fingerprint(cfg),
		SavedAt:     time.Now(),
		ServerInfo:  serverInfo,
		Tools:       tools,
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// cachedTools returns a cache hit for name+cfg — the tools and serverInfo
// recorded the last time this exact config was connected, within the TTL.
// A miss (nothing cached, fingerprint mismatch because the config
// changed, TTL expired, or a corrupted file) always returns ok=false
// rather than an error: the only correct response to "the cache doesn't
// help here" is to fall back to a real connection, never to fail.
//
// Unused by ConnectAll today — see the package doc above for why that's
// deliberate. Exercised directly by cache_test.go and available for a
// future lazy-discovery path.
func cachedTools(name string, cfg ServerConfig) ([]Tool, string, bool) {
	cacheMu.Lock()
	cf := loadCacheFile()
	cacheMu.Unlock()

	entry, ok := cf.Servers[name]
	if !ok {
		return nil, "", false
	}
	if entry.Fingerprint != fingerprint(cfg) {
		return nil, "", false
	}
	if time.Since(entry.SavedAt) > cacheTTL {
		return nil, "", false
	}
	return entry.Tools, entry.ServerInfo, true
}
