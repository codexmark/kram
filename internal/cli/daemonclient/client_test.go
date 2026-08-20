package daemonclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateSessionPostsAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions" {
			t.Errorf("request = %s %s, want POST /sessions", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "my title" {
			t.Errorf("posted title = %q, want %q", body["title"], "my title")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Session{ID: "ses_1", Title: "my title"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	sess, err := c.CreateSession(context.Background(), "my title")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "ses_1" || sess.Title != "my title" {
		t.Errorf("sess = %+v, want ID=ses_1 Title=%q", sess, "my title")
	}
}

func TestListSessionsGetsAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/sessions" {
			t.Errorf("request = %s %s, want GET /sessions", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Session{{ID: "ses_1"}, {ID: "ses_2"}})
	}))
	defer srv.Close()

	c := New(srv.URL)
	list, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListSessions() = %+v, want 2 entries", list)
	}
}

func TestGetSessionReturnsSessionAndMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/ses_1" {
			t.Errorf("path = %q, want /sessions/ses_1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session":  Session{ID: "ses_1"},
			"messages": []Message{{ID: 1, Content: "hi"}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	sess, msgs, err := c.GetSession(context.Background(), "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "ses_1" || len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Errorf("sess=%+v msgs=%+v, want ID=ses_1 and one message \"hi\"", sess, msgs)
	}
}

func TestListToolsDecodesToolsAndSkills(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools" {
			t.Errorf("path = %q, want /tools", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tools":  []ToolInfo{{Name: "bash", Disabled: false}},
			"skills": []Skill{{Name: "code-review", Scope: "project"}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	tools, skills, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "bash" {
		t.Errorf("tools = %+v, want one entry named bash", tools)
	}
	if len(skills) != 1 || skills[0].Name != "code-review" {
		t.Errorf("skills = %+v, want one entry named code-review", skills)
	}
}

func TestUpdateToolSettingsPutsDisabledSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/tools/settings" {
			t.Errorf("request = %s %s, want PUT /tools/settings", r.Method, r.URL.Path)
		}
		var body struct {
			Disabled []string `json:"disabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Disabled) != 2 || body.Disabled[0] != "bash" || body.Disabled[1] != "write_file" {
			t.Errorf("disabled = %v", body.Disabled)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	if err := New(srv.URL).UpdateToolSettings(context.Background(), []string{"bash", "write_file"}); err != nil {
		t.Fatal(err)
	}
}

func TestGetContextDecodesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/ses_1/context" {
			t.Errorf("path = %q, want /sessions/ses_1/context", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ContextUsage{Budget: 100, Used: 40, Free: 60})
	}))
	defer srv.Close()

	c := New(srv.URL)
	usage, err := c.GetContext(context.Background(), "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Budget != 100 || usage.Used != 40 || usage.Free != 60 {
		t.Errorf("usage = %+v, want Budget=100 Used=40 Free=60", usage)
	}
}

func TestAnswerQuestionPostsExpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/ses_1/answer" {
			t.Errorf("path = %q, want /sessions/ses_1/answer", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["question_id"] != "q1" || body["answer"] != "yes" {
			t.Errorf("body = %+v, want question_id=q1 answer=yes", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if err := c.AnswerQuestion(context.Background(), "ses_1", "q1", "yes"); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerApprovalPostsExpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/ses_1/approve" {
			t.Errorf("path = %q, want /sessions/ses_1/approve", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["approval_id"] != "a1" || body["decision"] != "once" {
			t.Errorf("body = %+v, want approval_id=a1 decision=once", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if err := c.AnswerApproval(context.Background(), "ses_1", "a1", "once"); err != nil {
		t.Fatal(err)
	}
}

func TestDoJSONReturnsDaemonErrorMessageOnFailureBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "session not found"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, _, err := c.GetSession(context.Background(), "ses_missing")
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Errorf("err = %v, want it to mention the daemon's error message", err)
	}
}

func TestDoJSONReturnsPlainErrorWhenNoJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ListSessions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want it to mention the raw status", err)
	}
}

func writeSSE(w http.ResponseWriter, lines ...string) {
	for _, l := range lines {
		fmt.Fprintf(w, "data: %s\n\n", l)
	}
}

func TestSendMessageStreamParsesDeltasAndDoneEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/ses_1/messages" {
			t.Errorf("path = %q, want /sessions/ses_1/messages", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["content"] != "hello" {
			t.Errorf("posted content = %v, want %q", body["content"], "hello")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeSSE(w, `{"type":"delta","content":"Hi"}`, `{"type":"done","message":{"id":1,"content":"Hi"}}`)
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL)
	stream, err := c.SendMessageStream(context.Background(), "ses_1", "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var events []StreamEvent
	for {
		evt, done, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			if evt.Type != "" {
				events = append(events, evt)
			}
			break
		}
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2 (delta, done)", events)
	}
	if events[0].Type != "delta" || events[0].Content != "Hi" {
		t.Errorf("events[0] = %+v, want type=delta content=Hi", events[0])
	}
	if events[1].Type != "done" || events[1].Message.Content != "Hi" {
		t.Errorf("events[1] = %+v, want type=done message.content=Hi", events[1])
	}
}

func TestSendMessageStreamReturnsErrorOnNon2xxBeforeStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "content must not be empty"})
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.SendMessageStream(context.Background(), "ses_1", "", nil)
	if err == nil || !strings.Contains(err.Error(), "content must not be empty") {
		t.Errorf("err = %v, want it to mention the daemon's rejection message", err)
	}
}

func TestMessageStreamNextReportsDoneOnEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// No [DONE] marker, no events — connection just closes.
	}))
	defer srv.Close()

	c := New(srv.URL)
	stream, err := c.SendMessageStream(context.Background(), "ses_1", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	_, done, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("expected Next() to report done on a stream that closed with no [DONE] marker")
	}
}

func TestMessageStreamNextSkipsMalformedEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{not valid json`, `{"type":"delta","content":"ok"}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL)
	stream, err := c.SendMessageStream(context.Background(), "ses_1", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	evt, done, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if done || evt.Type != "delta" || evt.Content != "ok" {
		t.Errorf("first well-formed event = %+v (done=%v), want the malformed one skipped and \"ok\" delivered", evt, done)
	}
}

func TestSendMessageStreamIncludesImagesWhenPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		imgs, _ := body["images"].([]any)
		if len(imgs) != 1 || imgs[0] != "data:image/png;base64,abc" {
			t.Errorf("posted images = %v, want one data URL", body["images"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL)
	stream, err := c.SendMessageStream(context.Background(), "ses_1", "hi", []string{"data:image/png;base64,abc"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
}
