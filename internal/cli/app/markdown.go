package app

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// kramGlamourStyle is a compact custom Glamour style matching Kram's own
// palette (see styles.go) — not Glamour's default "dark" style, which adds
// a document margin and a boxed h1 that don't fit the chat's flat,
// unbordered layout.
const kramGlamourStyle = `{
	"document": {"margin": 0},
	"block_quote": {"indent": 1, "indent_token": "│ ", "color": "240"},
	"paragraph": {},
	"list": {"level_indent": 2},
	"heading": {"block_suffix": "\n", "color": "111", "bold": true},
	"h1": {"color": "111", "bold": true},
	"h2": {"color": "111", "bold": true},
	"h3": {"color": "111", "bold": true},
	"h4": {"color": "111", "bold": true},
	"h5": {"color": "111", "bold": true},
	"h6": {"color": "240"},
	"text": {},
	"strikethrough": {"crossed_out": true},
	"emph": {"italic": true},
	"strong": {"bold": true, "color": "252"},
	"hr": {"color": "237", "format": "\n───\n"},
	"item": {"block_prefix": "• "},
	"enumeration": {"block_prefix": ". "},
	"task": {"ticked": "[x] ", "unticked": "[ ] "},
	"link": {"color": "111", "underline": true},
	"link_text": {"color": "111", "bold": true},
	"image": {"color": "179", "underline": true},
	"image_text": {"color": "240", "format": "[image: {{.text}}]"},
	"code": {"prefix": "", "suffix": "", "color": "173", "background_color": "235"},
	"code_block": {"color": "252", "margin": 1},
	"table": {},
	"definition_list": {},
	"definition_term": {},
	"definition_description": {},
	"html_block": {},
	"html_span": {}
}`

// newMarkdownRenderer builds a Glamour renderer word-wrapped to width. A
// construction failure (malformed style, unsupported terminal) returns nil
// rather than an error — callers fall back to plain text, since a
// rendering bug must never crash the CLI.
func newMarkdownRenderer(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes([]byte(kramGlamourStyle)),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil
	}
	return r
}

// renderMarkdown renders raw through r, falling back to the raw text
// untouched on any error or panic — markdown formatting is a nice-to-have,
// never a reason to lose or crash on an otherwise-good response.
func renderMarkdown(r *glamour.TermRenderer, raw string) (out string) {
	if r == nil || raw == "" {
		return raw
	}
	defer func() {
		if rec := recover(); rec != nil {
			out = raw
		}
	}()
	rendered, err := r.Render(raw)
	if err != nil {
		return raw
	}
	return strings.TrimRight(rendered, "\n")
}
