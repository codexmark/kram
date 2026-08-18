package store

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

// searchSchema indexes real conversation history — what the user and the
// model actually said — for the session_search tool. This is a distinct
// concern from memory_entries/memory_fts (agent-curated notes, see
// memory.go): session_search answers "where did we actually discuss X",
// even for things nobody ever called memory_write about. Kept in its own
// file/constant for the same reason memorySchema is split out of schema.
//
// Only role IN ('user', 'assistant') with non-empty content is indexed:
//   - "tool" rows are raw tool output (bash/grep/etc results), not
//     something anyone said — indexing them would bury real conversation
//     under command output noise.
//   - "system" rows are machine-authored framing (the system prompt,
//     AGENTS.md injection, and compaction summaries — see
//     internal/daemon/compaction, CompactionMarkerName). Excluding the
//     entire role rather than special-casing the compaction marker name
//     is deliberate: it's a blanket, structural guarantee that a
//     generated summary can never surface as if it were an original
//     message (acceptance criterion), not something that depends on
//     remembering to filter one specific Name value. A compaction
//     summary can still appear inside a match's context window (as a
//     neighboring message, clearly tagged Role: "system"), which is
//     useful context and not a false claim about who said it.
//   - Assistant turns that are pure tool-calls have empty Content; those
//     are skipped too since an empty string can never match a query.
const searchSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	content, content='messages', content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages
WHEN new.role IN ('user', 'assistant') AND new.content <> ''
BEGIN
	INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages
WHEN old.role IN ('user', 'assistant') AND old.content <> ''
BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.id, old.content);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages
WHEN (old.role IN ('user', 'assistant') AND old.content <> '')
  OR (new.role IN ('user', 'assistant') AND new.content <> '')
BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.id, old.content);
	INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
`

// backfillMessagesFTS populates messages_fts for a database that had
// messages before this feature existed — the triggers above only fire on
// writes from this point forward, so a pre-existing messages table would
// otherwise be silently unsearchable. Guarded to run only when the index
// is completely empty (and skipped entirely once anything has been
// indexed), so it costs one cheap existence check on every normal startup
// and a single one-time scan the first time a database upgrades into
// having search.
func backfillMessagesFTS(db *sql.DB) error {
	var alreadyIndexed int
	if err := db.QueryRow(`SELECT count(*) FROM messages_fts`).Scan(&alreadyIndexed); err != nil {
		return fmt.Errorf("checking message search index: %w", err)
	}
	if alreadyIndexed > 0 {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO messages_fts(rowid, content)
		SELECT id, content FROM messages
		WHERE role IN ('user', 'assistant') AND content <> ''`)
	if err != nil {
		return fmt.Errorf("indexing existing messages: %w", err)
	}
	return nil
}

// SearchScope controls whether subagent-run sessions (title prefixed
// "subagent: ", see internal/daemon/agent RunTask) are included in
// session_search results. Subagent sessions are internal execution noise
// from delegate_task, not conversations the user had, so they're excluded
// by default rather than merely ranked lower — a noisy subagent
// transcript that happens to repeat the user's search term would
// otherwise crowd out the real conversation it was talking about.
const (
	SearchScopeUser = "user" // default: exclude subagent-run sessions
	SearchScopeAll  = "all"  // include subagent-run sessions too
)

const subagentTitlePrefix = "subagent: "

// defaultWindowBefore/After sizes the context window returned around each
// match: enough surrounding conversation to make sense of the hit without
// dumping the whole session.
const (
	defaultWindowBefore = 3
	defaultWindowAfter  = 3
)

// SearchResult is one match from SearchMessages: the message that matched
// the query, which session it's from, and a chronological window of
// surrounding messages from that same session for context.
type SearchResult struct {
	Message      Message   `json:"message"`
	SessionTitle string    `json:"session_title"`
	IsSubagent   bool      `json:"is_subagent"`
	Window       []Message `json:"window"`
	// Score is the raw FTS5 bm25() value for this match (negative,
	// more-negative-is-better) — exported mainly so ranking behavior is
	// directly testable/inspectable, not something callers need to
	// interpret themselves.
	Score float64 `json:"-"`
}

// relevanceTieTolerance is how close two matches' bm25 scores have to be,
// as a fraction of their average magnitude, to be treated as "the same
// relevance" for tie-breaking purposes. bm25's absolute scale depends on
// corpus statistics (term rarity, average document length) and varies
// wildly between a small workspace and a long-running one, so the
// tolerance has to be relative, not a fixed constant — an earlier version
// of this rounded the raw score by a fixed multiplier, which collapsed
// every real score in a small test corpus (~1e-6 magnitude) into the same
// bucket and destroyed the relevance signal entirely. 20% is a
// deliberately loose band: it's meant to catch "basically the same
// relevance, so let recency decide" rather than to second-guess bm25 on
// genuinely different scores.
const relevanceTieTolerance = 0.20

// SearchMessages does a full-text search (SQLite FTS5, BM25 ranking) over
// real conversation history — user and assistant messages actually
// exchanged, never tool output or system/compaction framing (see
// searchSchema). Ranking is BM25 first; matches whose scores are within
// relevanceTieTolerance of each other are then reordered by recency
// (newest first), since "how relevant" and "how long ago" both matter and
// a near-tie on relevance shouldn't be decided by incidental row order.
// scope controls whether subagent-run sessions are included
// (SearchScopeUser excludes them, SearchScopeAll includes them); anything
// else is treated as SearchScopeUser.
func (s *Store) SearchMessages(query string, limit int, scope string) ([]SearchResult, error) {
	ftsQuery := sanitizeFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	q := `
		SELECT m.id, m.session_id, m.role, m.content, m.provider, m.tool_calls, m.tool_call_id, m.name, m.images, m.created_at,
		       s.title, bm25(messages_fts) AS score
		FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE f.content MATCH ?`
	args := []any{ftsQuery}
	if scope != SearchScopeAll {
		q += ` AND s.title NOT LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(subagentTitlePrefix)+"%")
	}
	// Primary sort is the untouched bm25 score (ascending: more negative,
	// i.e. more relevant, first); m.created_at DESC is only a
	// deterministic fallback for exact ties before the real recency
	// tie-break pass below reorders near-ties.
	q += `
		ORDER BY score ASC, m.created_at DESC
		LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("searching messages: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var m Message
		var toolCallsJSON, imagesJSON, title string
		var score float64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Provider, &toolCallsJSON, &m.ToolCallID, &m.Name, &imagesJSON, &m.CreatedAt, &title, &score); err != nil {
			return nil, fmt.Errorf("scanning search result: %w", err)
		}
		if err := decodeMessageJSON(&m, toolCallsJSON, imagesJSON); err != nil {
			return nil, err
		}
		out = append(out, SearchResult{Message: m, SessionTitle: title, IsSubagent: isSubagentTitle(title), Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	applyRecencyTieBreak(out)

	for i := range out {
		window, err := s.contextWindow(out[i].Message.SessionID, out[i].Message.ID, defaultWindowBefore, defaultWindowAfter)
		if err != nil {
			return nil, err
		}
		out[i].Window = window
	}
	return out, nil
}

// applyRecencyTieBreak reorders results in place: within each run of
// consecutive matches whose bm25 score is within relevanceTieTolerance of
// the run's best (first, most-relevant) score, it sorts by CreatedAt
// descending instead of leaving SQL's tie-break order. Comparing every
// candidate in a run against the run's anchor score (not its immediate
// neighbor) keeps a long gradual drift in scores from chaining into one
// enormous "tied" group.
func applyRecencyTieBreak(results []SearchResult) {
	i := 0
	for i < len(results) {
		j := i + 1
		for j < len(results) && closeScores(results[i].Score, results[j].Score) {
			j++
		}
		if j-i > 1 {
			group := results[i:j]
			sort.SliceStable(group, func(a, b int) bool {
				return group[a].Message.CreatedAt > group[b].Message.CreatedAt
			})
		}
		i = j
	}
}

func closeScores(a, b float64) bool {
	if a == b {
		return true
	}
	denom := (math.Abs(a) + math.Abs(b)) / 2
	if denom == 0 {
		return true
	}
	return math.Abs(a-b)/denom <= relevanceTieTolerance
}

// contextWindow returns up to `before` messages preceding messageID and
// up to `after` messages following it, all from the same session, plus
// the message itself, in chronological order. Scoped strictly to
// sessionID so a match near the start/end of one session's history never
// pulls in another session's messages, even though message ids are a
// single global sequence shared across every session.
func (s *Store) contextWindow(sessionID string, messageID int64, before, after int) ([]Message, error) {
	const cols = `id, session_id, role, content, provider, tool_calls, tool_call_id, name, images, created_at`

	beforeRows, err := s.db.Query(
		`SELECT `+cols+` FROM messages WHERE session_id = ? AND id < ? ORDER BY id DESC LIMIT ?`,
		sessionID, messageID, before,
	)
	if err != nil {
		return nil, fmt.Errorf("loading preceding context: %w", err)
	}
	var pre []Message
	for beforeRows.Next() {
		m, err := scanMessage(beforeRows)
		if err != nil {
			beforeRows.Close()
			return nil, err
		}
		pre = append(pre, m)
	}
	if err := beforeRows.Err(); err != nil {
		beforeRows.Close()
		return nil, err
	}
	beforeRows.Close()
	// pre was fetched newest-first (to LIMIT the closest N), so reverse it
	// back into chronological order.
	for i, j := 0, len(pre)-1; i < j; i, j = i+1, j-1 {
		pre[i], pre[j] = pre[j], pre[i]
	}

	row := s.db.QueryRow(`SELECT `+cols+` FROM messages WHERE session_id = ? AND id = ?`, sessionID, messageID)
	mid, err := scanMessage(row)
	if err != nil {
		return nil, fmt.Errorf("loading matched message: %w", err)
	}

	afterRows, err := s.db.Query(
		`SELECT `+cols+` FROM messages WHERE session_id = ? AND id > ? ORDER BY id ASC LIMIT ?`,
		sessionID, messageID, after,
	)
	if err != nil {
		return nil, fmt.Errorf("loading following context: %w", err)
	}
	defer afterRows.Close()
	var post []Message
	for afterRows.Next() {
		m, err := scanMessage(afterRows)
		if err != nil {
			return nil, err
		}
		post = append(post, m)
	}
	if err := afterRows.Err(); err != nil {
		return nil, err
	}

	out := make([]Message, 0, len(pre)+1+len(post))
	out = append(out, pre...)
	out = append(out, mid)
	out = append(out, post...)
	return out, nil
}

// sanitizeFTSQuery turns free-text search input into a safe FTS5 MATCH
// expression: every run of letters/digits (Unicode-aware, so accents and
// non-Latin scripts like Japanese or Cyrillic count) becomes its own
// double-quoted term, and terms are implicitly ANDed by FTS5 — a plain,
// predictable "all these words" search.
//
// This exists because FTS5's query-syntax parser treats characters like
// '-', ':', '(', ')', '"', and '*' as operators (NOT, column-filter,
// grouping, phrase, prefix), not literal text. A raw, unquoted user query
// such as "unique-search-term" is not "the phrase unique-search-term" —
// it's parsed as an expression and (found empirically) fails outright
// with "no such column: search", since fts5 reads the text after the
// hyphen as a column-filter operand. Every ordinary word becomes its own
// quoted literal specifically so hyphens, punctuation, and any other
// FTS5 operator character in the user's text can never be interpreted as
// query syntax — the search is plain-word matching, not a query
// language exposed to whatever text happens to be in a conversation.
func sanitizeFTSQuery(query string) string {
	var b strings.Builder
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('"')
		// FTS5 quoted-string syntax escapes an embedded double-quote by
		// doubling it — moot today since cur only ever contains
		// letters/digits, but kept for correctness if that ever changes.
		b.WriteString(strings.ReplaceAll(cur.String(), `"`, `""`))
		b.WriteByte('"')
		cur.Reset()
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return b.String()
}

func isSubagentTitle(title string) bool {
	return strings.HasPrefix(title, subagentTitlePrefix)
}

// escapeLike escapes SQLite LIKE metacharacters (%, _, and the escape
// character itself) in a string that's going to be embedded as a LIKE
// pattern fragment — subagentTitlePrefix happens to contain none of them
// today, but the escape is here so that stays true even if the prefix
// text ever changes.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
