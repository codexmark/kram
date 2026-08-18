package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/snapshot"
)

// snapshotErrorText turns a snapshot package error into the plain-text
// error result every tool in this package returns instead of a Go
// error — same convention as every other tool here: the model gets to
// see and react to a failure, rather than the agent loop treating it as
// a hard stop. snapshot.ErrUnavailable (no git on PATH) gets a
// specifically worded message since it's the one case a caller might
// want to detect and stop retrying.
func snapshotErrorText(err error) (string, error) {
	if errors.Is(err, snapshot.ErrUnavailable) {
		return "error: workspace snapshots are unavailable in this environment — git is not installed or not on PATH", nil
	}
	return fmt.Sprintf("error: %v", err), nil
}

// snapshotCreate captures the workspace's current file state. See
// internal/snapshot for exactly what "current state" means (respects
// the user's .gitignore, always excludes .git and .kram) and why this
// never touches the user's real git repository.
type snapshotCreate struct {
	store *snapshot.Store
}

func newSnapshotCreate(store *snapshot.Store) *snapshotCreate { return &snapshotCreate{store: store} }

func (t *snapshotCreate) Name() string { return "snapshot_create" }
func (t *snapshotCreate) Description() string {
	return "Capture the current state of every file in the workspace (respecting .gitignore) as a named, restorable snapshot. Does not touch the user's own git repository, index, staging area, branch, or commits — this is a separate, hidden mechanism. Use before a risky multi-file change so there is a way back with snapshot_restore."
}

func (t *snapshotCreate) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"message": {"type": "string", "description": "Optional short label for this snapshot, e.g. why it was taken. Defaults to a timestamp."}
		}
	}`)
}

func (t *snapshotCreate) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Message string `json:"message"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &args)
	}
	snap, err := t.store.Create(ctx, args.Message)
	if err != nil {
		return snapshotErrorText(err)
	}
	return fmt.Sprintf("snapshot created: %s — %q (%s)", snap.ShortID(), snap.Message, snap.CreatedAt.Format(time.RFC3339)), nil
}

// snapshotList lists every snapshot taken so far, newest first.
type snapshotList struct {
	store *snapshot.Store
}

func newSnapshotList(store *snapshot.Store) *snapshotList { return &snapshotList{store: store} }

func (t *snapshotList) Name() string { return "snapshot_list" }
func (t *snapshotList) Description() string {
	return "List every workspace snapshot taken so far, newest first, with its id, message, and creation time. Use this to find the id snapshot_diff or snapshot_restore need."
}

func (t *snapshotList) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *snapshotList) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	snaps, err := t.store.List(ctx)
	if err != nil {
		return snapshotErrorText(err)
	}
	if len(snaps) == 0 {
		return "no snapshots yet — use snapshot_create to take one", nil
	}
	var b strings.Builder
	for _, s := range snaps {
		fmt.Fprintf(&b, "%s  %s  %s\n", s.ShortID(), s.CreatedAt.Format(time.RFC3339), s.Message)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// snapshotDiff shows what a restore would change, without applying
// anything — always safe to call.
type snapshotDiff struct {
	store *snapshot.Store
}

func newSnapshotDiff(store *snapshot.Store) *snapshotDiff { return &snapshotDiff{store: store} }

func (t *snapshotDiff) Name() string { return "snapshot_diff" }
func (t *snapshotDiff) Description() string {
	return "Show what restoring a given snapshot would change in the workspace, as a unified diff against the current file state. Read-only — applies nothing. Call this before snapshot_restore to see the effect first."
}

func (t *snapshotDiff) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Snapshot id, from snapshot_create or snapshot_list."}
		},
		"required": ["id"]
	}`)
}

func (t *snapshotDiff) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	out, err := t.store.Diff(ctx, args.ID)
	if err != nil {
		return snapshotErrorText(err)
	}
	return out, nil
}

// snapshotRestore is the one mutating tool in this file — it overwrites
// workspace file content to match a snapshot. It never touches the
// user's real git repository (see internal/snapshot), but it is a
// destructive action against the workspace's *files*, which the
// description below says plainly, and its result always names exactly
// what changed rather than reporting success silently.
type snapshotRestore struct {
	store *snapshot.Store
}

func newSnapshotRestore(store *snapshot.Store) *snapshotRestore {
	return &snapshotRestore{store: store}
}

func (t *snapshotRestore) Name() string { return "snapshot_restore" }
func (t *snapshotRestore) Description() string {
	return "DESTRUCTIVE: overwrites workspace files to match a previously taken snapshot, discarding any changes made to those files since — including changes the caller may not know about, if the workspace has moved on further since that snapshot was taken. Requires an explicit snapshot id from snapshot_list; never guesses 'the latest'. Reports every file it changed. Never touches the user's own git repository, index, staging area, branch, or commits. Consider calling snapshot_diff first to preview the effect."
}

func (t *snapshotRestore) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Snapshot id to restore, from snapshot_create or snapshot_list. Required — there is no default or 'latest' shortcut."}
		},
		"required": ["id"]
	}`)
}

func (t *snapshotRestore) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(args.ID) == "" {
		return "error: id is required — call snapshot_list to find one", nil
	}

	result, err := t.store.Restore(ctx, args.ID)
	if err != nil {
		return snapshotErrorText(err)
	}
	if len(result.Changes) == 0 {
		return fmt.Sprintf("restored %s — workspace already matched this snapshot, nothing changed", shortHash(result.SnapshotID)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "restored %s — %d file(s) changed:\n", shortHash(result.SnapshotID), len(result.Changes))
	for _, c := range result.Changes {
		fmt.Fprintf(&b, "  %s: %s\n", c.Status, c.Path)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func shortHash(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
