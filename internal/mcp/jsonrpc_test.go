package mcp

import (
	"encoding/json"
	"testing"
)

func TestRequestMarshalsWithID(t *testing.T) {
	req := request{JSONRPC: jsonRPCVersion, ID: 7, Method: "tools/list", Params: map[string]any{"cursor": "x"}}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	if round["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", round["jsonrpc"])
	}
	if round["id"] != float64(7) {
		t.Errorf("id = %v, want 7", round["id"])
	}
	if round["method"] != "tools/list" {
		t.Errorf("method = %v, want tools/list", round["method"])
	}
}

func TestNotificationHasNoID(t *testing.T) {
	n := notification{JSONRPC: jsonRPCVersion, Method: "notifications/initialized"}
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	if _, hasID := round["id"]; hasID {
		t.Error("a notification must never carry an id field — that's what distinguishes it from a request expecting a response")
	}
}

func TestMessageDistinguishesResponseFromNotification(t *testing.T) {
	respData := []byte(`{"jsonrpc":"2.0","id":3,"result":{"ok":true}}`)
	var resp message
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID == nil || *resp.ID != 3 {
		t.Fatalf("expected id=3, got %v", resp.ID)
	}

	notifData := []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
	var notif message
	if err := json.Unmarshal(notifData, &notif); err != nil {
		t.Fatal(err)
	}
	if notif.ID != nil {
		t.Errorf("a notification should decode with a nil ID, got %v", notif.ID)
	}
	if notif.Method != "notifications/progress" {
		t.Errorf("method = %q", notif.Method)
	}
}

func TestMessageParsesRPCError(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Error == nil {
		t.Fatal("expected a non-nil Error")
	}
	if msg.Error.Code != -32601 {
		t.Errorf("code = %d, want -32601", msg.Error.Code)
	}
	if msg.Error.Error() != "method not found" {
		t.Errorf("Error() = %q", msg.Error.Error())
	}
}

func TestCallToolResultContentBlocks(t *testing.T) {
	data := []byte(`{
		"content": [
			{"type": "text", "text": "hello"},
			{"type": "resource", "resource": {"uri": "file:///x", "text": "contents"}},
			{"type": "resource_link", "uri": "file:///y"},
			{"type": "image", "mimeType": "image/png", "data": "base64stuff"}
		],
		"isError": false
	}`)
	var res callToolResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 4 {
		t.Fatalf("got %d content blocks, want 4", len(res.Content))
	}
	if res.Content[0].Type != "text" || res.Content[0].Text != "hello" {
		t.Errorf("block 0 = %+v", res.Content[0])
	}
	if res.Content[1].Resource == nil || res.Content[1].Resource.Text != "contents" {
		t.Errorf("block 1 resource not parsed: %+v", res.Content[1])
	}
	if res.Content[2].URI != "file:///y" {
		t.Errorf("block 2 uri = %q", res.Content[2].URI)
	}
}
