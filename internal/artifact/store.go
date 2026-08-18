// Package artifact persists oversized tool output to disk instead of
// forcing it either into RAM/context unbounded or truncating it away —
// the two bad options a tool result otherwise has. A large result is
// saved once, given a short id, and made available for the model to read
// back in bounded slices (artifact_read) instead of ever appearing in the
// conversation transcript in full.
package artifact

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// defaultReadLimit bounds one artifact_read call — generous enough to be
// useful, small enough that reading an artifact can't itself recreate the
// exact unbounded-context problem artifacts exist to avoid.
const defaultReadLimit = 8000

// Artifact is one persisted tool result's metadata — the content itself
// lives in a sibling file, not in this struct, so listing/inspecting
// metadata never requires reading a potentially huge payload.
type Artifact struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name"`
	CreatedAt  int64  `json:"created_at"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
}

// Store persists artifacts under <workspace>/.kram/artifacts — project-
// scoped like everything else in .kram, and daemon-lifetime rather than
// session-lifetime (an artifact created by one session should stay
// readable if referenced from another, same reasoning as background.go's
// processManager).
type Store struct {
	dir string
}

// Open returns a Store rooted at workspace's .kram/artifacts directory.
// It doesn't touch disk until Save/Read actually needs to.
func Open(workspace string) *Store {
	return &Store{dir: filepath.Join(workspace, ".kram", "artifacts")}
}

var idPattern = regexp.MustCompile(`^art_[0-9a-f]{16}$`)

func newID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "art_" + hex.EncodeToString(buf)
}

// paths resolves id to its two on-disk files, rejecting anything that
// isn't a well-formed generated id before ever joining it into a
// filesystem path — the artifact_read tool takes an id from the model, and
// this is what stops "id" from ever becoming an arbitrary-path reader
// (there is deliberately no way to pass a real filesystem path in here).
func (s *Store) paths(id string) (content, meta string, err error) {
	if !idPattern.MatchString(id) {
		return "", "", fmt.Errorf("invalid artifact id %q", id)
	}
	return filepath.Join(s.dir, id+".bin"), filepath.Join(s.dir, id+".json"), nil
}

// Save persists r's content as a new artifact and returns its metadata.
// The sha256 is computed while streaming, not as a second pass.
func (s *Store) Save(sessionID, toolCallID, toolName string, r io.Reader) (Artifact, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Artifact{}, err
	}
	id := newID()
	contentPath, metaPath, err := s.paths(id)
	if err != nil {
		return Artifact{}, err // unreachable: newID always matches idPattern
	}

	f, err := os.OpenFile(contentPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return Artifact{}, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(f, io.TeeReader(r, h))
	if err != nil {
		os.Remove(contentPath)
		return Artifact{}, err
	}

	a := Artifact{
		ID: id, SessionID: sessionID, ToolCallID: toolCallID, ToolName: toolName,
		CreatedAt: time.Now().Unix(), Size: n, SHA256: hex.EncodeToString(h.Sum(nil)),
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		os.Remove(contentPath)
		return Artifact{}, err
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		os.Remove(contentPath)
		return Artifact{}, err
	}
	return a, nil
}

// SaveFile persists the content of an existing file — typically a
// SpillWriter's temp file — as a new artifact, and removes the source file
// on success: the source is consumed, not left behind for the caller to
// also clean up.
func (s *Store) SaveFile(sessionID, toolCallID, toolName, srcPath string) (Artifact, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return Artifact{}, err
	}
	a, err := s.Save(sessionID, toolCallID, toolName, f)
	f.Close()
	if err != nil {
		return Artifact{}, err
	}
	os.Remove(srcPath)
	return a, nil
}

// Read returns a slice of one artifact's content: up to limit bytes
// starting at offset (both clamped to sane values — offset < 0 becomes 0,
// limit <= 0 or too large becomes defaultReadLimit), plus its metadata.
func (s *Store) Read(id string, offset, limit int) (string, Artifact, error) {
	contentPath, metaPath, err := s.paths(id)
	if err != nil {
		return "", Artifact{}, err
	}

	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return "", Artifact{}, fmt.Errorf("no such artifact %q", id)
	}
	var a Artifact
	if err := json.Unmarshal(metaData, &a); err != nil {
		return "", Artifact{}, fmt.Errorf("corrupt metadata for artifact %q: %w", id, err)
	}

	f, err := os.Open(contentPath)
	if err != nil {
		return "", a, fmt.Errorf("no such artifact %q", id)
	}
	defer f.Close()

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > defaultReadLimit {
		limit = defaultReadLimit
	}
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return "", a, err
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", a, err
	}
	return string(buf[:n]), a, nil
}

// Preview returns the first n bytes of an artifact's content — the "too
// large, here's a taste" text shown right after a spill.
func (s *Store) Preview(id string, n int) (string, error) {
	text, _, err := s.Read(id, 0, n)
	return text, err
}

// GC deletes artifacts whose metadata is older than maxAge — best-effort
// disk hygiene, never correctness-critical: a garbage-collected artifact
// simply becomes an unresolvable id if referenced later, the same outcome
// as if it had never been created. Called once at Registry construction
// (daemon startup), not on a timer — a long-lived daemon process restarts
// often enough in practice (every workspace re-open) that this is enough
// to keep .kram/artifacts from growing forever.
func (s *Store) GC(maxAge time.Duration) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		contentPath, metaPath, err := s.paths(id)
		if err != nil {
			continue
		}
		os.Remove(contentPath)
		os.Remove(metaPath)
	}
}
