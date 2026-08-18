// Package snapshot gives Kram a way to capture and restore the workspace's
// file state without ever touching the user's own git repository — no
// `git reset --hard`, no checkout, no change to their index, staging area,
// current branch, or commits. If an agent breaks something, a snapshot is
// the way back, independent of whatever the user is doing with git
// themselves.
//
// The storage engine is git itself, but a second, entirely separate
// repository, isolated under <workspace>/.kram/snapshots/.git
// (--git-dir), operating against the real workspace directory as its
// --work-tree. Every operation this package performs — add, commit, diff,
// reset --hard — targets that isolated --git-dir. None of it is a
// "dangerous" command in that context: it's our own private, hidden
// repository nobody else looks at, used purely as inexpensive, correct
// content-addressed storage for whole-tree snapshots. It shares a
// filesystem with the user's repo but nothing else — different index,
// different HEAD, different history, different config, different
// identity.
//
// The user's real .git and Kram's own .kram/ are never captured — see
// ensureRepo's exclude file. Everything else present in the workspace at
// snapshot time is, respecting whatever the user's own .gitignore already
// says: files git itself would call untracked-and-ignored (node_modules,
// build output, .env, ...) are skipped, which is deliberate — see
// DECISIONS.md.
package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable is returned by any Store operation when git isn't on
// PATH. The feature degrades gracefully: callers (the snapshot_* tools)
// turn this into a plain "unavailable" text result, never a crash.
var ErrUnavailable = errors.New("snapshot: git is not available on PATH")

const (
	gitTimeout   = 30 * time.Second
	maxDiffBytes = 30_000
)

// Snapshot is one point-in-time capture of the workspace — a commit in
// the isolated snapshot repository, never in the user's own.
type Snapshot struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// ShortID is the 12-character prefix used in tool-facing output — the
// full 40-character hash still works anywhere an id is accepted.
func (s Snapshot) ShortID() string {
	if len(s.ID) > 12 {
		return s.ID[:12]
	}
	return s.ID
}

// FileChange describes what restoring a given snapshot would do to one
// path, relative to the workspace's current on-disk state.
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "will be overwritten" | "will be restored" | "will be removed"
}

// RestoreResult reports exactly what a Restore call changed — restoring
// is a mutating, potentially destructive action, so this is never silent
// about its effect (see DECISIONS.md, "Restore over stale state").
type RestoreResult struct {
	SnapshotID string       `json:"snapshot_id"`
	Changes    []FileChange `json:"changes"`
}

// Store owns one workspace's isolated snapshot repository.
type Store struct {
	workspace string
	gitDir    string

	// mu serializes this Store's own git invocations within one process.
	// It does not protect against a second Kram process touching the same
	// workspace concurrently — git's own index.lock already turns that
	// case into a returned error rather than corruption, which is enough
	// for this feature's scope (see DECISIONS.md).
	mu sync.Mutex
}

// NewStore returns a Store rooted at workspace's .kram/snapshots
// directory. Like artifact.Open, it touches no disk until an operation
// actually needs it — no directory is created just by calling this.
func NewStore(workspace string) *Store {
	return &Store{
		workspace: workspace,
		gitDir:    filepath.Join(workspace, ".kram", "snapshots", ".git"),
	}
}

// Available reports whether the snapshot feature can be used at all in
// this environment. Every exported Store method also checks this itself
// and returns ErrUnavailable, so callers that only care about the error
// path don't need to call this separately — it exists mainly so a tool
// can decide once, up front, whether to attempt anything.
func Available() error {
	if _, err := exec.LookPath("git"); err != nil {
		return ErrUnavailable
	}
	return nil
}

// hashPattern is checked before any id is ever interpolated into a git
// command — same discipline as artifact.Store.paths, and for the same
// reason: an id string that starts with "-" could otherwise be
// misinterpreted by git as a flag instead of a revision.
var hashPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// run invokes git against the isolated snapshot repository, with both
// --git-dir and --work-tree set — the form every content-touching
// operation (add, commit, diff, reset) uses.
func (s *Store) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--git-dir=" + s.gitDir, "--work-tree=" + s.workspace}, args...)
	return s.exec(ctx, full)
}

// runRaw invokes git against the isolated repository with only --git-dir
// set — for init/config calls that must not require or touch a work
// tree.
func (s *Store) runRaw(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--git-dir=" + s.gitDir}, args...)
	return s.exec(ctx, full)
}

func (s *Store) exec(ctx context.Context, args []string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Dir = s.workspace
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

// ensureRepo lazily creates and configures the isolated snapshot
// repository the first time it's needed. Idempotent: a second call is a
// cheap no-op once HEAD exists.
func (s *Store) ensureRepo(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.gitDir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.gitDir, 0o755); err != nil {
		return fmt.Errorf("creating snapshot storage: %w", err)
	}
	if _, err := s.runRaw(ctx, "init", "--quiet"); err != nil {
		return fmt.Errorf("initializing snapshot storage: %w", err)
	}
	// Isolated identity, entirely local to this hidden repo's own config
	// file — never the user's global ~/.gitconfig, never their project
	// .git/config. commit.gpgsign=false so a snapshot commit can't fail
	// just because the user's global git config expects commits to be
	// signed; there is no user-facing signature to make here.
	for _, kv := range [][2]string{
		{"user.email", "kram-snapshot@local"},
		{"user.name", "Kram Snapshot"},
		{"commit.gpgsign", "false"},
	} {
		if _, err := s.runRaw(ctx, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("configuring snapshot storage: %w", err)
		}
	}
	// info/exclude (not .gitignore — this repo has no work-tree files of
	// its own to put one in) unconditionally hides the user's real .git
	// and Kram's own .kram, regardless of what the user's .gitignore
	// says. The user's actual .gitignore files, elsewhere in the work
	// tree, are picked up automatically by git's normal ignore-pattern
	// walk since --work-tree points at the same directory they live in
	// — see DECISIONS.md for why respecting them is the chosen behavior.
	excludePath := filepath.Join(s.gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("configuring snapshot exclusions: %w", err)
	}
	if err := os.WriteFile(excludePath, []byte("/.git\n/.kram\n"), 0o644); err != nil {
		return fmt.Errorf("configuring snapshot exclusions: %w", err)
	}
	return nil
}

// Create captures the workspace's current file state as a new snapshot:
// every file present, minus the user's own .gitignore matches, minus
// .git and .kram. message becomes the commit message; an empty message
// gets a timestamp default. Never touches the user's real .git.
func (s *Store) Create(ctx context.Context, message string) (Snapshot, error) {
	if err := Available(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureRepo(ctx); err != nil {
		return Snapshot{}, err
	}
	if _, err := s.run(ctx, "add", "-A", "--"); err != nil {
		return Snapshot{}, fmt.Errorf("staging workspace for snapshot: %w", err)
	}
	if strings.TrimSpace(message) == "" {
		message = "snapshot " + time.Now().UTC().Format(time.RFC3339)
	}
	// --allow-empty: a snapshot taken when nothing changed since the last
	// one is still a valid, useful marker (e.g. "known-good, right before
	// a risky operation") — refusing it would surprise a caller who
	// asked for exactly that.
	if _, err := s.run(ctx, "commit", "--quiet", "--allow-empty", "-m", message); err != nil {
		return Snapshot{}, fmt.Errorf("committing snapshot: %w", err)
	}
	return s.head(ctx)
}

func (s *Store) head(ctx context.Context) (Snapshot, error) {
	out, err := s.run(ctx, "log", "-1", "--format=%H%x1f%cI%x1f%s")
	if err != nil {
		return Snapshot{}, fmt.Errorf("reading snapshot: %w", err)
	}
	return parseLogLine(strings.TrimSpace(out))
}

func parseLogLine(line string) (Snapshot, error) {
	parts := strings.SplitN(line, "\x1f", 3)
	if len(parts) != 3 {
		return Snapshot{}, fmt.Errorf("malformed snapshot log entry")
	}
	createdAt, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		createdAt = time.Time{}
	}
	return Snapshot{ID: parts[0], CreatedAt: createdAt, Message: parts[2]}, nil
}

// List returns every snapshot ever taken for this workspace, newest
// first (the isolated repo's own log order — there is exactly one linear
// history, since nothing ever branches or checks out a different commit
// within it). Returns an empty slice, not an error, if no snapshot has
// ever been created.
func (s *Store) List(ctx context.Context) ([]Snapshot, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(filepath.Join(s.gitDir, "HEAD")); err != nil {
		return nil, nil
	}
	out, err := s.run(ctx, "log", "--format=%H%x1f%cI%x1f%s")
	if err != nil {
		if strings.Contains(err.Error(), "does not have any commits yet") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	var snaps []Snapshot
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		snap, err := parseLogLine(line)
		if err != nil {
			continue
		}
		snaps = append(snaps, snap)
	}
	return snaps, nil
}

// resolve validates id's shape and confirms it names a real commit in
// the isolated repository, returning its full hash.
func (s *Store) resolve(ctx context.Context, id string) (string, error) {
	if !hashPattern.MatchString(id) {
		return "", fmt.Errorf("invalid snapshot id %q", id)
	}
	out, err := s.run(ctx, "rev-parse", "--verify", id+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("no such snapshot %q", id)
	}
	return strings.TrimSpace(out), nil
}

// Diff shows what restoring id would change, as a unified diff against
// the workspace's current on-disk state — without applying anything.
// Calling this is always safe.
func (s *Store) Diff(ctx context.Context, id string) (string, error) {
	if err := Available(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	hash, err := s.resolve(ctx, id)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, "diff", hash, "--")
	if err != nil {
		return "", fmt.Errorf("diffing snapshot: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "(no differences — workspace already matches this snapshot)", nil
	}
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes] + "\n\n[diff truncated]"
	}
	return out, nil
}

// changesFor computes, before any mutation happens, exactly which paths
// a restore to hash will touch and how — the basis for RestoreResult, so
// Restore is never silent about its own effect.
func (s *Store) changesFor(ctx context.Context, hash string) ([]FileChange, error) {
	out, err := s.run(ctx, "diff", "--name-status", hash, "--")
	if err != nil {
		return nil, fmt.Errorf("computing restore preview: %w", err)
	}
	var changes []FileChange
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || fields[0] == "" {
			continue
		}
		var status string
		switch fields[0][0] {
		case 'A':
			// Exists now, absent from the snapshot: restoring removes it.
			status = "will be removed"
		case 'D':
			// In the snapshot, absent now: restoring recreates it.
			status = "will be restored"
		case 'M':
			status = "will be overwritten"
		default:
			status = "will change"
		}
		changes = append(changes, FileChange{Path: fields[1], Status: status})
	}
	return changes, nil
}

// Restore brings the workspace's files back to exactly the state
// captured by snapshot id. It never touches the user's real .git, index,
// branch, or commits — only the isolated repository's own hidden HEAD
// (meaningless outside this package) and the content of files on disk.
//
// Chosen behavior for a stale snapshot (the workspace has changed again
// since id was taken): overwrite and report, never silently and never
// refuse. Restore always returns the full list of paths it changed —
// see RestoreResult — so the caller (and, through the snapshot_restore
// tool, the model and the user) can see exactly what happened, even
// though nothing blocks the restore itself. See DECISIONS.md for why
// this was chosen over refusing on staleness.
//
// A file the snapshot system has no record of at all — created after
// the most recent snapshot and never captured by any Create call — is
// left untouched. Restore only ever undoes what it once knew about.
func (s *Store) Restore(ctx context.Context, id string) (RestoreResult, error) {
	if err := Available(); err != nil {
		return RestoreResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	hash, err := s.resolve(ctx, id)
	if err != nil {
		return RestoreResult{}, err
	}
	changes, err := s.changesFor(ctx, hash)
	if err != nil {
		return RestoreResult{}, err
	}
	if _, err := s.run(ctx, "reset", "--quiet", "--hard", hash); err != nil {
		return RestoreResult{}, fmt.Errorf("restoring snapshot: %w", err)
	}
	return RestoreResult{SnapshotID: hash, Changes: changes}, nil
}
