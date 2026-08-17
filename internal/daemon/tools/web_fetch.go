package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	webFetchTimeout     = 15 * time.Second
	webFetchMaxBytes    = 200_000
	webFetchMaxRespRead = webFetchMaxBytes + 1 // +1 so we can detect truncation
)

var htmlTagRe = regexp.MustCompile(`(?is)<script.*?</script>|<style.*?</style>|<[^>]+>`)

// webFetch does a GET request and returns the body as text — a crude
// HTML-tag strip when the response looks like HTML, otherwise the raw
// body. This is not a real HTML-to-Markdown pipeline, just enough to make
// fetched docs/readmes usable as tool-call context.
type webFetch struct {
	client *http.Client
}

func newWebFetch() *webFetch {
	return &webFetch{client: &http.Client{Timeout: webFetchTimeout}}
}

func (t *webFetch) Name() string { return "web_fetch" }
func (t *webFetch) Description() string {
	return "Fetch a URL over HTTP(S) and return its content as text (HTML tags are stripped). Useful for reading documentation or a README from a link."
}

func (t *webFetch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "The http:// or https:// URL to fetch."}
		},
		"required": ["url"]
	}`)
}

type webFetchArgs struct {
	URL string `json:"url"`
}

func (t *webFetch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args webFetchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return "error: url must start with http:// or https://", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return fmt.Sprintf("error: building request: %v", err), nil
	}
	req.Header.Set("User-Agent", "kram-agent/0")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Sprintf("error: fetching %s: %v", args.URL, err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxRespRead))
	if err != nil {
		return fmt.Sprintf("error: reading response from %s: %v", args.URL, err), nil
	}

	truncated := len(body) > webFetchMaxBytes
	if truncated {
		body = body[:webFetchMaxBytes]
	}

	text := string(body)
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") {
		text = htmlTagRe.ReplaceAllString(text, " ")
		text = strings.Join(strings.Fields(text), " ")
	}

	result := fmt.Sprintf("[%d %s]\n\n%s", resp.StatusCode, args.URL, text)
	if truncated {
		result += "\n\n[truncated]"
	}
	return result, nil
}
