// Package lsp is a from-scratch Language Server Protocol client — the way
// Kram gets real semantic navigation (diagnostics, go-to-definition,
// find-references) instead of only grep/glob's text matching. Like
// internal/mcp, this is hand-rolled JSON-RPC 2.0 rather than an SDK
// dependency, but the wire framing is different: LSP servers speak
// `Content-Length: N\r\n\r\n<json>` over stdio, not MCP's newline-delimited
// JSON, so it needs its own transport rather than reusing internal/mcp's.
//
// This implements one vertical slice, not the whole protocol: initialize,
// textDocument/didOpen (+didClose, to reopen cleanly on every query),
// textDocument/publishDiagnostics, textDocument/definition and
// textDocument/references. No code actions, no rename, no call hierarchy,
// hover is not implemented.
package lsp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// headerContentLength is the only header this client looks for.
// Content-Type may also appear on the wire; it's read past and ignored,
// since every LSP message body is JSON regardless of what it says.
const headerContentLength = "content-length:"

// writeFrame writes one Content-Length-framed JSON-RPC message. Framing is
// two header lines (well, one) then a blank line then the raw body — no
// trailing newline after the body itself.
func writeFrame(w io.Writer, payload []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readFrame reads exactly one Content-Length-framed message from r.
//
// Because r is a *bufio.Reader, this handles both fragmentation cases
// correctly without any extra bookkeeping here: ReadString and io.ReadFull
// each keep pulling from the underlying reader until they have a full
// line/full body, so a message arriving in several small reads is
// reassembled transparently. And when two messages arrive concatenated in
// a single underlying read, bufio.Reader buffers the remainder internally,
// so the next call to readFrame picks up exactly where the last one left
// off rather than the caller needing to track any leftover bytes itself.
func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line: end of headers
		}
		if idx := strings.IndexByte(line, ':'); idx >= 0 {
			name := strings.ToLower(strings.TrimSpace(line[:idx]))
			if name+":" == headerContentLength {
				n, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
				if err != nil {
					return nil, fmt.Errorf("lsp: bad Content-Length header %q: %w", line, err)
				}
				contentLength = n
			}
			// any other header (Content-Type, ...) is read past and ignored
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("lsp: message had no Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("lsp: reading %d-byte body: %w", contentLength, err)
	}
	return body, nil
}
