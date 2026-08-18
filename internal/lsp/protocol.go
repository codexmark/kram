package lsp

import "encoding/json"

const jsonRPCVersion = "2.0"

// rpcRequest is a message this client sends that expects a reply.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcNotification is a message this client sends that expects no reply
// (didOpen, didClose, initialized).
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is a reply this client sends to a server-initiated request
// (e.g. workspace/configuration) — see Client.handleServerRequest.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// envelope is the inbound shape covering every message an LSP server can
// send: a response to our request (ID set, Method empty), a request from
// the server to us (ID and Method both set), or a notification (ID empty,
// Method set). One struct for all three because they arrive interleaved
// on the same stream and the reader can't know which it has until parsed
// — same reasoning internal/mcp's `message` type documents.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// --- initialize ---

type initializeParams struct {
	ProcessID    int            `json:"processId"`
	RootURI      string         `json:"rootUri"`
	RootPath     string         `json:"rootPath,omitempty"`
	Capabilities map[string]any `json:"capabilities"`
	ClientInfo   clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	Capabilities json.RawMessage `json:"capabilities"`
	ServerInfo   *struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	} `json:"serverInfo,omitempty"`
}

// --- text document sync ---

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

// --- positions ---

// Position is 0-indexed on both axes, the native LSP convention: line 0 is
// the file's first line, character 0 is the first column. Kram's LSP tools
// intentionally keep this convention rather than translating to 1-indexed,
// and document it in each tool's description instead — see
// internal/daemon/tools/lsp.go.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range within one file, identified by URI.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type referenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

type referenceParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      referenceContext       `json:"context"`
}

// --- diagnostics ---

// Diagnostic severities, per the LSP spec (textDocument/publishDiagnostics).
const (
	SeverityError       = 1
	SeverityWarning     = 2
	SeverityInformation = 3
	SeverityHint        = 4
)

// Diagnostic is one server-reported problem with a file.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     any    `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// SeverityLabel renders a diagnostic's severity as short lowercase text
// ("error", "warning", "info", "hint") for display in tool output.
func (d Diagnostic) SeverityLabel() string {
	switch d.Severity {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "info"
	case SeverityHint:
		return "hint"
	default:
		return "unknown"
	}
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     int          `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// --- definition / declaration ---
//
// textDocument/definition's result is a notorious union type: Location |
// Location[] | LocationLink[] | null. locationOrLink covers every field
// any of those shapes can carry so one json.Unmarshal handles all of them;
// resolve() below picks the right fields out.
type locationOrLink struct {
	// Location / Location[]
	URI   string `json:"uri"`
	Range *Range `json:"range"`
	// LocationLink[]
	TargetURI            string `json:"targetUri"`
	TargetRange          *Range `json:"targetRange"`
	TargetSelectionRange *Range `json:"targetSelectionRange"`
}

func (l locationOrLink) resolve() Location {
	if l.TargetURI != "" {
		r := l.TargetSelectionRange
		if r == nil {
			r = l.TargetRange
		}
		if r == nil {
			r = &Range{}
		}
		return Location{URI: l.TargetURI, Range: *r}
	}
	r := l.Range
	if r == nil {
		r = &Range{}
	}
	return Location{URI: l.URI, Range: *r}
}

// parseLocations decodes a textDocument/definition or
// textDocument/references result, which may be a bare object, an array,
// or JSON null (no result found).
func parseLocations(raw json.RawMessage) ([]Location, error) {
	trimmed := trimSpaceBytes(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var items []locationOrLink
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		out := make([]Location, len(items))
		for i, it := range items {
			out[i] = it.resolve()
		}
		return out, nil
	}
	var single locationOrLink
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, err
	}
	return []Location{single.resolve()}, nil
}

func trimSpaceBytes(b []byte) []byte {
	start := 0
	for start < len(b) && isJSONSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isJSONSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
