package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codexmark/kram-gateway/internal/daemon/compaction"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

// sessionSearchDefaultLimit caps how many matches session_search returns
// when the model doesn't specify one — enough to survey a topic without
// flooding a single tool result.
const sessionSearchDefaultLimit = 8

// sessionSearchMaxContentChars truncates an over-long message inside a
// result so one huge pasted file doesn't dominate the tool result at the
// expense of every other match/context line.
const sessionSearchMaxContentChars = 400

// sessionSearch is the deterministic (no-LLM) counterpart to memory_search:
// it searches what was actually said across every session in this
// workspace's database, not the agent's curated memory_entries. See
// store.SearchMessages and searchSchema in internal/daemon/store/search.go
// for the indexing rules (user/assistant only, BM25 ranking).
type sessionSearch struct {
	store *store.Store
}

func newSessionSearch(st *store.Store) *sessionSearch {
	return &sessionSearch{store: st}
}

func (t *sessionSearch) Name() string { return "session_search" }
func (t *sessionSearch) Description() string {
	return "Full-text search over the REAL history of past conversations in this workspace — what was actually said, not curated memory (use memory_search for that instead). Finds discussions even if nobody ever wrote them down with memory_write. Each match includes its session id, timestamp, and a few surrounding messages for context — never the whole session. Subagent-run sessions (internal delegate_task noise) are excluded by default; pass scope \"all\" to include them. Generated compaction summaries are never returned as if they were something someone said."
}

func (t *sessionSearch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search terms (SQLite FTS5 syntax — plain words are fine, and non-ASCII text works)."},
			"limit": {"type": "integer", "description": "Max number of matches to return. Defaults to 8."},
			"scope": {"type": "string", "enum": ["user", "all"], "description": "\"user\" (default) excludes subagent-run sessions. \"all\" also searches subagent transcripts."}
		},
		"required": ["query"]
	}`)
}

type sessionSearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
	Scope string `json:"scope"`
}

func (t *sessionSearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args sessionSearchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(args.Query) == "" {
		return "error: query must not be empty", nil
	}

	limit := args.Limit
	if limit <= 0 {
		limit = sessionSearchDefaultLimit
	}
	scope := store.SearchScopeUser
	if args.Scope == store.SearchScopeAll {
		scope = store.SearchScopeAll
	}

	results, err := t.store.SearchMessages(args.Query, limit, scope)
	if err != nil {
		return fmt.Sprintf("error: searching history: %v", err), nil
	}
	if len(results) == 0 {
		return "no matches found", nil
	}

	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&b, "session %s (%q) at %s\n", r.Message.SessionID, r.SessionTitle, formatUnixTime(r.Message.CreatedAt))
		if r.IsSubagent {
			b.WriteString("(subagent-run session)\n")
		}
		b.WriteString("context:\n")
		for _, m := range r.Window {
			marker := "  "
			if m.ID == r.Message.ID {
				marker = "> " // the message that actually matched the query
			}
			fmt.Fprintf(&b, "%s[%s]%s %s\n", marker, m.Role, compactionNote(m), truncateForSearch(m.Content))
		}
	}
	return b.String(), nil
}

// compactionNote flags a window entry that's a machine-generated
// compaction summary rather than a real message, so it can never be read
// as something the user or model actually said even when it shows up as
// surrounding context for a match. SearchMessages itself never returns one
// of these as the matched message — only role IN ('user','assistant') is
// indexed — but a compaction marker can legitimately appear as a
// neighboring message in a window.
func compactionNote(m store.Message) string {
	if m.Role == "system" && m.Name == compaction.CompactionMarkerName {
		return " (GENERATED compaction summary, not an original message)"
	}
	return ""
}

func formatUnixTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

func truncateForSearch(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	r := []rune(s)
	if len(r) > sessionSearchMaxContentChars {
		return string(r[:sessionSearchMaxContentChars]) + "…"
	}
	return s
}
