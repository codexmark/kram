package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/mcp"
)

func runTool(t *testing.T, tool Tool, raw string) string {
	t.Helper()
	got, err := tool.Execute(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCoreFileToolsBehavior(t *testing.T) {
	root := t.TempDir()
	write := newWriteFile(root)
	if got := runTool(t, write, `{"path":"dir/a.txt","content":"old old"}`); !strings.Contains(got, "wrote") {
		t.Fatal(got)
	}
	if got := runTool(t, newReadFile(root), `{"path":"dir/a.txt"}`); got != "old old" {
		t.Fatalf("read = %q", got)
	}
	if got := runTool(t, newEditFile(root), `{"path":"dir/a.txt","old_string":"old","new_string":"new"}`); !strings.Contains(got, "appears 2 times") {
		t.Fatal(got)
	}
	if got := runTool(t, newEditFile(root), `{"path":"dir/a.txt","old_string":"old","new_string":"new","replace_all":true}`); !strings.Contains(got, "2 occurrence") {
		t.Fatal(got)
	}
	if got := runTool(t, newEditFile(root), `{"path":"dir/a.txt","old_string":"missing","new_string":"x"}`); !strings.Contains(got, "not found") {
		t.Fatal(got)
	}
	if got := runTool(t, newEditFile(root), `{"path":"dir/a.txt","old_string":"","new_string":"x"}`); !strings.Contains(got, "must not be empty") {
		t.Fatal(got)
	}
	if got := runTool(t, newEditFile(root), `{"path":"dir/a.txt","old_string":"x","new_string":"x"}`); !strings.Contains(got, "identical") {
		t.Fatal(got)
	}

	if got := runTool(t, newMoveFile(root), `{"old_path":"dir/a.txt","new_path":"other/b.txt"}`); !strings.Contains(got, "moved") {
		t.Fatal(got)
	}
	if got := runTool(t, newMoveFile(root), `{"old_path":"missing","new_path":"x"}`); !strings.Contains(got, "error:") {
		t.Fatal(got)
	}
	if got := runTool(t, newDeleteFile(root), `{"path":"other"}`); !strings.Contains(got, "directory") {
		t.Fatal(got)
	}
	if got := runTool(t, newDeleteFile(root), `{"path":"other/b.txt"}`); !strings.Contains(got, "deleted") {
		t.Fatal(got)
	}
	if got := runTool(t, newDeleteFile(root), `{"path":"missing"}`); !strings.Contains(got, "error:") {
		t.Fatal(got)
	}

	for _, tool := range []Tool{write, newReadFile(root), newEditFile(root), newMoveFile(root), newDeleteFile(root)} {
		if got := runTool(t, tool, `{`); !strings.Contains(got, "invalid arguments") {
			t.Errorf("%s malformed = %q", tool.Name(), got)
		}
	}
	for _, tool := range []Tool{write, newReadFile(root), newEditFile(root), newMoveFile(root), newDeleteFile(root)} {
		var raw string
		switch tool.Name() {
		case "move_file":
			raw = `{"old_path":"../x","new_path":"x"}`
		default:
			raw = `{"path":"../x","old_string":"a","new_string":"b"}`
		}
		if got := runTool(t, tool, raw); !strings.Contains(got, "escapes") {
			t.Errorf("%s escape = %q", tool.Name(), got)
		}
	}
}

func TestReadTruncationAndDirectoryListings(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "huge"), []byte(strings.Repeat("a", maxReadBytes+1)), 0644); err != nil {
		t.Fatal(err)
	}
	if got := runTool(t, newReadFile(root), `{"path":"huge"}`); !strings.Contains(got, "truncated") {
		t.Fatal("large read not truncated")
	}
	if got := runTool(t, newReadFile(root), `{"path":"missing"}`); !strings.Contains(got, "error:") {
		t.Fatal(got)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "z.txt"), []byte("z"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := runTool(t, newListDir(root), `{}`); !strings.Contains(got, "sub/") || !strings.Contains(got, "z.txt") {
		t.Fatal(got)
	}
	if got := runTool(t, newListDir(root), `{"path":"sub"}`); got != "(empty directory)" {
		t.Fatal(got)
	}
	if got := runTool(t, newListDir(root), `{`); !strings.Contains(got, "invalid") {
		t.Fatal(got)
	}
	if got := runTool(t, newListDir(root), `{"path":"missing"}`); !strings.Contains(got, "error:") {
		t.Fatal(got)
	}
}

func TestGlobBehaviorAndPatterns(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"root.go", "a/x.go", "a/b/y.txt"} {
		if err := os.WriteFile(filepath.Join(root, p), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	g := newGlob(root)
	if got := runTool(t, g, `{"pattern":"**/*.go"}`); !strings.Contains(got, "root.go") || !strings.Contains(got, "a/x.go") {
		t.Fatal(got)
	}
	if got := runTool(t, g, `{"pattern":"*.none"}`); got != "(no matches)" {
		t.Fatal(got)
	}
	for _, raw := range []string{`{`, `{"pattern":""}`, `{"pattern":"*","path":"../"}`} {
		if got := runTool(t, g, raw); !strings.Contains(got, "error:") {
			t.Errorf("glob(%s) = %q", raw, got)
		}
	}
	for _, p := range []string{"**/x", "a/**", "a/*.go", "x?.[ch]", "a+b"} {
		if _, err := regexp.Compile(globToRegex(p)); err != nil {
			t.Errorf("globToRegex(%q) invalid: %v", p, err)
		}
	}
}

type askerStub struct {
	answer string
	err    error
}

func (a askerStub) Ask(context.Context, string, []string) (string, error) { return a.answer, a.err }

func TestAskAndDepthContexts(t *testing.T) {
	tool := newAskQuestion()
	if got := runTool(t, tool, `{"question":"q"}`); !strings.Contains(got, "isn't available") {
		t.Fatal(got)
	}
	ctx := WithAsker(context.Background(), askerStub{answer: "yes"})
	got, _ := tool.Execute(ctx, json.RawMessage(`{"question":"q","options":["yes"]}`))
	if got != "yes" {
		t.Fatal(got)
	}
	ctx = WithAsker(context.Background(), askerStub{err: errors.New("closed")})
	got, _ = tool.Execute(ctx, json.RawMessage(`{"question":"q"}`))
	if !strings.Contains(got, "closed") {
		t.Fatal(got)
	}
	for _, raw := range []string{`{`, `{"question":""}`} {
		got, _ = tool.Execute(WithAsker(context.Background(), askerStub{}), json.RawMessage(raw))
		if !strings.Contains(got, "error:") {
			t.Fatal(got)
		}
	}
	if depthFromContext(WithDepth(context.Background(), 3)) != 3 || depthFromContext(context.Background()) != 0 {
		t.Fatal("depth context mismatch")
	}
}

func TestTodoRoundTrip(t *testing.T) {
	s := newTodoStore(t.TempDir())
	if got := runTool(t, newTodoRead(s), `{}`); got != "(no todos yet)" {
		t.Fatal(got)
	}
	if got := runTool(t, newTodoWrite(s), `{"todos":[{"content":"a"},{"content":"b","status":"in_progress"},{"content":"c","status":"completed"}]}`); !strings.Contains(got, "saved 3") {
		t.Fatal(got)
	}
	got := runTool(t, newTodoRead(s), `{}`)
	if !strings.Contains(got, "[ ] a") || !strings.Contains(got, "[~] b") || !strings.Contains(got, "[x] c") {
		t.Fatal(got)
	}
	if got := runTool(t, newTodoWrite(s), `{`); !strings.Contains(got, "invalid") {
		t.Fatal(got)
	}
	loaded := newTodoStore(filepath.Dir(filepath.Dir(s.path)))
	if len(loaded.read()) != 3 {
		t.Fatalf("loaded = %+v", loaded.read())
	}
}

func TestWebFetchTextHTMLAndValidation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/html" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<style>x</style><b>Hello</b> world"))
			return
		}
		_, _ = w.Write([]byte("plain"))
	}))
	defer s.Close()
	tf := newWebFetch()
	if got := runTool(t, tf, `{"url":"`+s.URL+`/html"}`); strings.Contains(got, "<b>") || !strings.Contains(got, "Hello world") {
		t.Fatal(got)
	}
	if got := runTool(t, tf, `{"url":"`+s.URL+`/text"}`); !strings.Contains(got, "plain") {
		t.Fatal(got)
	}
	for _, raw := range []string{`{`, `{"url":"ftp://x"}`} {
		if got := runTool(t, tf, raw); !strings.Contains(got, "error:") {
			t.Fatal(got)
		}
	}
}

type delegatorStub struct{}

func (delegatorStub) RunTask(_ context.Context, goal, taskContext, model string, depth int) (string, error) {
	if goal == "fail" {
		return "", errors.New("boom")
	}
	return fmt.Sprintf("%s:%s:%s:%d", goal, taskContext, model, depth), nil
}

func TestDelegateTaskValidationDepthAndResults(t *testing.T) {
	r := NewRegistry(t.TempDir(), nil, nil)
	task := newDelegateTask(r)
	if got := runTool(t, task, `{}`); !strings.Contains(got, "not available") {
		t.Fatal(got)
	}
	r.SetDelegator(delegatorStub{})
	for _, tc := range []struct {
		ctx       context.Context
		raw, want string
	}{
		{WithDepth(context.Background(), 1), `{"tasks":[{"goal":"x"}]}`, "max subagent"},
		{context.Background(), `{`, "invalid arguments"},
		{context.Background(), `{"tasks":[]}`, "at least one"},
		{context.Background(), `{"tasks":[{"goal":"one","context":"ctx","model":"m"}]}`, "one:ctx:m:1"},
		{context.Background(), `{"tasks":[{"goal":"one"},{"goal":"fail"}]}`, "subagent 2/2"},
	} {
		got, err := task.Execute(tc.ctx, json.RawMessage(tc.raw))
		if err != nil || !strings.Contains(got, tc.want) {
			t.Errorf("delegate = %q, %v, want %q", got, err, tc.want)
		}
	}
}

func TestMCPEmptyManagerToolsAndMetadata(t *testing.T) {
	m := mcp.ConnectAll(context.Background(), map[string]mcp.ServerConfig{}, nil)
	defer m.Close()
	r := NewRegistry(t.TempDir(), nil, nil)
	before := len(r.AllTools())
	r.RegisterMCP(m)
	if len(r.AllTools()) != before {
		t.Fatal("empty MCP manager registered tools")
	}
	if _, err := resolveMCPServer(m, "missing"); err == nil {
		t.Fatal("resolve missing server succeeded")
	}
	for _, tool := range []Tool{newMCPResourceList(m), newMCPResourceRead(m), newMCPPromptList(m), newMCPPromptGet(m)} {
		_ = tool.Name()
		_ = tool.Description()
		_ = tool.Schema()
		for _, raw := range []string{`{`, `{}`} {
			got := runTool(t, tool, raw)
			if !strings.Contains(got, "error:") && !strings.Contains(got, "no resources") && !strings.Contains(got, "no prompts") {
				t.Errorf("%s = %q", tool.Name(), got)
			}
		}
	}
	mt := newMCPTool(nil, "server", mcp.Tool{Name: "remote"})
	if mt.Name() != "mcp__server__remote" || !strings.Contains(mt.Description(), "remote") || len(mt.Schema()) == 0 {
		t.Fatalf("mcp tool metadata = %+v", mt)
	}
}

func TestRegistryMutationAndLifecycle(t *testing.T) {
	r := NewRegistry(t.TempDir(), nil, nil)
	r.StopLSPServers()
	r.ReplaceDisabled([]string{"read_file", "", "read_file"})
	if got := r.AllTools(); len(got) == 0 {
		t.Fatal("no tools")
	}
	if got, _ := r.Execute(context.Background(), "read_file", json.RawMessage(`{}`)); !strings.Contains(got, "disabled") {
		t.Fatal(got)
	}
	r.ReplaceDisabled(nil)
	_ = r.Skills()
}

func TestGitToolsInTemporaryRepository(t *testing.T) {
	root := t.TempDir()
	if out, err := runGit(context.Background(), root, "init"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := runTool(t, newGitStatus(root), `{}`); !strings.Contains(got, "new.txt") {
		t.Fatal(got)
	}
	if got := runTool(t, newGitDiff(root), `{}`); got == "" {
		t.Fatal("empty diff result")
	}
	if got := runTool(t, newGitDiff(root), `{"staged":true}`); got == "" {
		t.Fatal("empty staged diff result")
	}
	if out, err := runGit(context.Background(), root, "not-a-command"); err == nil || out == "" {
		t.Fatalf("invalid git = %q, %v", out, err)
	}
}

func TestSkillInstallHelpers(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "collection", "demo")
	if err := os.MkdirAll(filepath.Join(root, ".git", "ignored"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skill, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "ref.txt"), []byte("reference"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("\nMIT License\nrest"), 0644); err != nil {
		t.Fatal(err)
	}
	found, err := findSkillDirs(root)
	if err != nil || found["demo"] != skill {
		t.Fatalf("found = %+v, %v", found, err)
	}
	dst := filepath.Join(t.TempDir(), "installed")
	if err := installSkill(skill, dst, "https://example.test/repo"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "SOURCE")); err != nil || !strings.Contains(string(data), "example.test") {
		t.Fatalf("source = %q, %v", data, err)
	}
	if got := detectLicense(root); got != "MIT License" {
		t.Fatalf("license = %q", got)
	}
	if got := detectLicense(t.TempDir()); got != "" {
		t.Fatalf("missing license = %q", got)
	}
	if err := installSkill(filepath.Join(root, "missing"), filepath.Join(t.TempDir(), "x"), "repo"); err == nil {
		t.Fatal("install missing source succeeded")
	}
	if _, err := cloneSkillRepository(context.Background(), "http://127.0.0.1:1/missing.git", filepath.Join(t.TempDir(), "clone")); err == nil {
		t.Fatal("cloneSkillRepository unexpectedly succeeded")
	}

	tool := newSkillInstall()
	for _, raw := range []string{`{`, `{"repo":""}`, `{"repo":"git@example.test:x"}`} {
		if got := runTool(t, tool, raw); !strings.Contains(got, "error:") {
			t.Errorf("skill_install(%s)=%q", raw, got)
		}
	}
}

func TestStoreBackedToolMetadataViaRegistry(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "tools.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := NewRegistry(root, st, nil)
	defs := r.Definitions()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Function.Name] = true
	}
	for _, name := range []string{"memory_write", "memory_search", "session_search"} {
		if !names[name] {
			t.Errorf("missing definition %s", name)
		}
	}
}

func TestSkillListLoadAndDisabled(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".kram", "skills", "demo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\ndescription: useful\n---\nDo the thing."), 0644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(root, nil, nil)
	if got := runTool(t, newSkillList(r), `{}`); !strings.Contains(got, "demo [project]: useful") {
		t.Fatal(got)
	}
	if got := runTool(t, newSkillLoad(r), `{"name":"demo"}`); got != "Do the thing." {
		t.Fatalf("body=%q", got)
	}
	if got := runTool(t, newSkillLoad(r), `{`); !strings.Contains(got, "invalid") {
		t.Fatal(got)
	}
	if got := runTool(t, newSkillLoad(r), `{"name":"missing"}`); !strings.Contains(got, "no skill") {
		t.Fatal(got)
	}
	r.ReplaceDisabled([]string{"demo"})
	if got := runTool(t, newSkillList(r), `{}`); strings.Contains(got, "demo [project]") {
		t.Fatal(got)
	}
	if got := runTool(t, newSkillLoad(r), `{"name":"demo"}`); !strings.Contains(got, "disabled") {
		t.Fatal(got)
	}
}

func TestMemoryAndSessionSearchValidationFailures(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	mw := newMemoryWrite(st, root)
	ms := newMemorySearch(st, root)
	ss := newSessionSearch(st)
	for _, tc := range []struct {
		tool      Tool
		raw, want string
	}{
		{mw, `{`, "invalid"}, {mw, `{}`, "content must"}, {mw, `{"operation":"remove"}`, "needs an id"}, {mw, `{"operation":"replace"}`, "needs an id"}, {mw, `{"operation":"replace","id":1}`, "needs content"},
		{ms, `{`, "invalid"}, {ms, `{}`, "query must"}, {ss, `{`, "invalid"}, {ss, `{}`, "query must"},
	} {
		got := runTool(t, tc.tool, tc.raw)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s=%q want %q", tc.tool.Name(), got, tc.want)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool Tool
		raw  string
	}{{mw, `{"content":"x"}`}, {mw, `{"operation":"remove","id":1}`}, {mw, `{"operation":"replace","id":1,"content":"x"}`}, {ms, `{"query":"x"}`}, {ss, `{"query":"x"}`}} {
		if got := runTool(t, tc.tool, tc.raw); !strings.Contains(got, "error:") {
			t.Errorf("%s closed=%q", tc.tool.Name(), got)
		}
	}
}

func TestMCPRegistryEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo tool", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "called"}}}
		case "resources/list":
			result = map[string]any{"resources": []any{map[string]any{"uri": "doc://one", "name": "One", "description": "doc"}}}
		case "resources/read":
			result = map[string]any{"contents": []any{map[string]any{"uri": "doc://one", "text": "resource text"}}}
		case "prompts/list":
			result = map[string]any{"prompts": []any{map[string]any{"name": "greet", "description": "Greeting"}}}
		case "prompts/get":
			result = map[string]any{"description": "Greeting", "messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "hello prompt"}}}}
		default:
			http.Error(w, "unknown "+req.Method, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	mgr := mcp.ConnectAll(context.Background(), map[string]mcp.ServerConfig{"fake": {Type: "http", URL: srv.URL}}, nil)
	defer mgr.Close()
	r := NewRegistry(t.TempDir(), nil, nil)
	r.RegisterMCP(mgr)
	for _, tc := range []struct{ name, raw, want string }{
		{"mcp__fake__echo", `{"value":"x"}`, "called"},
		{"mcp_resource_list", `{}`, "doc://one"}, {"mcp_resource_list", `{"server":"fake"}`, "doc://one"},
		{"mcp_resource_read", `{"server":"fake","uri":"doc://one"}`, "resource text"},
		{"mcp_prompt_list", `{}`, "greet"}, {"mcp_prompt_list", `{"server":"fake"}`, "greet"},
		{"mcp_prompt_get", `{"server":"fake","name":"greet"}`, "hello prompt"},
	} {
		got, err := r.Execute(context.Background(), tc.name, json.RawMessage(tc.raw))
		if err != nil || !strings.Contains(got, tc.want) {
			t.Errorf("%s=%q,%v want %q", tc.name, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ name, raw string }{{"mcp_resource_list", `{"server":"missing"}`}, {"mcp_resource_read", `{"server":"missing"}`}, {"mcp_prompt_list", `{"server":"missing"}`}, {"mcp_prompt_get", `{"server":"missing"}`}, {"mcp_resource_read", `{`}, {"mcp_prompt_get", `{`}} {
		got, _ := r.Execute(context.Background(), tc.name, json.RawMessage(tc.raw))
		if !strings.Contains(got, "error:") {
			t.Errorf("%s bad=%q", tc.name, got)
		}
	}
}

func TestSkillInstallExecuteAgainstDumbHTTPGit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	work := t.TempDir()
	skillDir := filepath.Join(work, "skills", "demo")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("demo instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "LICENSE"), []byte("MIT License\n"), 0644); err != nil {
		t.Fatal(err)
	}
	failClone := false
	clone := func(_ context.Context, _ string, dst string) ([]byte, error) {
		if failClone {
			return []byte("simulated clone failure"), errors.New("clone failed")
		}
		err := filepath.WalkDir(work, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(work, path)
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0755)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0644)
		})
		return nil, err
	}
	tool := newSkillInstall()
	tool.clone = clone
	url := "https://example.test/repo.git"
	if got := runTool(t, tool, `{"repo":"`+url+`"}`); !strings.Contains(got, "demo") || !strings.Contains(got, "MIT License") {
		t.Fatalf("list=%q", got)
	}
	if got := runTool(t, tool, `{"repo":"`+url+`","skills":["demo","missing"]}`); !strings.Contains(got, "installed 1") || !strings.Contains(got, "not found") {
		t.Fatalf("install=%q", got)
	}

	failClone = true
	if got := runTool(t, tool, `{"repo":"`+url+`"}`); !strings.Contains(got, "cloning") {
		t.Fatalf("clone failure=%q", got)
	}
}
