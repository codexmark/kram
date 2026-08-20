package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCallToolAllBlockShapesAndEmpty(t *testing.T) {
	resource := struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
	}{URI: "file:///r", Text: "embedded"}
	c, _ := newTestClient(func(string, *int64, json.RawMessage) (any, bool) {
		return callToolResult{Content: []contentBlock{{Type: "resource", Resource: &resource}, {Type: "resource", Resource: &struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType,omitempty"`
			Text     string `json:"text,omitempty"`
		}{URI: "file:///empty"}}, {Type: "resource_link", URI: "file:///link"}, {Type: "image", MimeType: "image/png"}}}, false
	})
	defer c.Close()
	out, err := c.CallTool(context.Background(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"embedded", "resource: file:///empty", "resource link: file:///link", "image content, image/png"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	empty, _ := newTestClient(func(string, *int64, json.RawMessage) (any, bool) { return callToolResult{}, false })
	defer empty.Close()
	if out, err := empty.CallTool(context.Background(), "x", nil); err != nil || out != "(empty result)" {
		t.Fatalf("empty=(%q,%v)", out, err)
	}
}

func TestResourcePromptAndLoadErrors(t *testing.T) {
	c, _ := newTestClient(func(string, *int64, json.RawMessage) (any, bool) { return nil, true })
	defer c.Close()
	if _, err := c.ListResources(context.Background()); err == nil {
		t.Fatal("resources error")
	}
	if _, err := c.ReadResource(context.Background(), "x"); err == nil {
		t.Fatal("read error")
	}
	if _, err := c.ListPrompts(context.Background()); err == nil {
		t.Fatal("prompts error")
	}
	if _, err := c.GetPrompt(context.Background(), "x", nil); err == nil {
		t.Fatal("prompt error")
	}
	if err := c.loadTools(context.Background()); err == nil {
		t.Fatal("tools error")
	}
	empty, _ := newTestClient(func(string, *int64, json.RawMessage) (any, bool) { return readResourceResult{}, false })
	defer empty.Close()
	if out, err := empty.ReadResource(context.Background(), "x"); err != nil || out != "(empty resource)" {
		t.Fatalf("empty=(%q,%v)", out, err)
	}
}

type sendErrorTransport struct{ ch chan message }

func (s *sendErrorTransport) Send(context.Context, []byte) error { return errors.New("send failed") }
func (s *sendErrorTransport) Recv() <-chan message               { return s.ch }
func (s *sendErrorTransport) Close() error                       { close(s.ch); return nil }

func TestCallAndNotifySendErrorsAndUnknownNotification(t *testing.T) {
	tr := &sendErrorTransport{ch: make(chan message)}
	c := &Client{name: "x", transport: tr, pending: map[int64]chan message{}, done: make(chan struct{})}
	go c.dispatch()
	if err := c.call(context.Background(), "x", nil, nil); err == nil || !strings.Contains(err.Error(), "sending x") {
		t.Fatalf("err=%v", err)
	}
	if err := c.notify(context.Background(), "x", nil); err == nil {
		t.Fatal("notify send error")
	}
	c.handleNotification(message{Method: "unrelated"})
	_ = c.Close()
}
