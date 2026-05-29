package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestHubApply 验证 hub reducer:user→token 开 assistant 气泡,tool_call/result 配对,plan/usage 更新。
func TestHubApply(t *testing.T) {
	h := NewHub("flash-x", "pro-y", "/tmp/ws", "zh")

	h.Broadcast(Event{Kind: "user_message", Text: "hi"})
	h.Broadcast(Event{Kind: "token", Text: "hel"})
	h.Broadcast(Event{Kind: "token", Text: "lo"})
	h.Broadcast(Event{Kind: "tool_call", Name: "Bash", Args: "ls"})
	if got := h.SnapshotCopy(); len(got.ToolCalls) != 1 || got.ToolCalls[0].ID == "" {
		t.Fatalf("tool_call should append a running tool with ID, got %+v", got.ToolCalls)
	}
	yes := true
	h.Broadcast(Event{Kind: "tool_result", Name: "Bash", Output: "a\nb", Success: &yes})
	h.Broadcast(Event{Kind: "usage", Usage: &Usage{PromptTokens: 100, CompletionTokens: 20, CacheHit: 80, CacheMiss: 20}})
	h.Broadcast(Event{Kind: "done"})

	s := h.SnapshotCopy()
	if len(s.Messages) != 2 || s.Messages[0].Role != "user" || s.Messages[1].Role != "assistant" {
		t.Fatalf("messages wrong: %+v", s.Messages)
	}
	if s.Messages[1].Content != "hello" {
		t.Fatalf("assistant content = %q, want hello", s.Messages[1].Content)
	}
	if len(s.ToolCalls) != 1 || s.ToolCalls[0].Status != "done" || s.ToolCalls[0].Output != "a\nb" {
		t.Fatalf("toolcall wrong: %+v", s.ToolCalls)
	}
	if s.Usage == nil || s.Usage.PromptTokens != 100 {
		t.Fatalf("usage wrong: %+v", s.Usage)
	}
	if s.Streaming {
		t.Fatalf("streaming should be false after done")
	}
}

// TestServerAuthAndCallbacks 验证 token 鉴权 + input/review 回调 + SSE 快照。
func TestServerAuthAndCallbacks(t *testing.T) {
	h := NewHub("flash", "pro", "/tmp/ws", "en")
	srv := NewServer(h)
	gotInput := make(chan string, 1)
	gotReview := make(chan bool, 1)
	srv.OnInput = func(s string) { gotInput <- s }
	srv.OnReview = func(b bool) { gotReview <- b }

	rawURL, err := srv.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	u, _ := url.Parse(rawURL)
	base := "http://" + u.Host
	token := u.Query().Get("t")
	if token == "" {
		t.Fatalf("URL missing token: %s", rawURL)
	}

	// 无 token → 403
	resp, err := http.Get(base + "/api/state")
	if err != nil {
		t.Fatalf("get state no-token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-token state want 403, got %d", resp.StatusCode)
	}

	// 带 token → 200
	resp, err = http.Get(base + "/api/state?t=" + token)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	var snap Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if resp.StatusCode != 200 || snap.Models.Flash != "flash" {
		t.Fatalf("state want 200+flash, got %d %+v", resp.StatusCode, snap.Models)
	}

	// POST /api/input → 回调拿到文本
	postJSON(t, base+"/api/input?t="+token, map[string]any{"text": "你好"})
	select {
	case got := <-gotInput:
		if got != "你好" {
			t.Fatalf("OnInput got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnInput not called")
	}

	// POST /api/review → 回调拿到 approve
	postJSON(t, base+"/api/review?t="+token, map[string]any{"approve": true})
	select {
	case got := <-gotReview:
		if !got {
			t.Fatal("OnReview got false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("OnReview not called")
	}

	// GET / 应返回内嵌的 index.html(验证 go:embed 链路)
	resp, err = http.Get(base + "/?t=" + token)
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	idxBody, _ := readAllString(resp)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(idxBody, "deepx web") {
		t.Fatalf("index want 200 + embedded html, got %d (len=%d)", resp.StatusCode, len(idxBody))
	}

	// SSE /api/events 首帧应为 snapshot
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events?t="+token, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(line, "event: snapshot") {
		t.Fatalf("first SSE line = %q, want 'event: snapshot'", line)
	}
}

// TestHandleFiles 验证 /api/files 鉴权 + OnListFiles 回调结果以 JSON 数组返回。
func TestHandleFiles(t *testing.T) {
	h := NewHub("flash", "pro", "/tmp/ws", "en")
	srv := NewServer(h)
	srv.OnListFiles = func() []string { return []string{"tui/model.go", "README.md"} }

	rawURL, err := srv.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	u, _ := url.Parse(rawURL)
	base := "http://" + u.Host
	token := u.Query().Get("t")

	// 无 token → 403
	resp, err := http.Get(base + "/api/files")
	if err != nil {
		t.Fatalf("get files no-token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-token files want 403, got %d", resp.StatusCode)
	}

	// 带 token → 回调列表
	resp, err = http.Get(base + "/api/files?t=" + token)
	if err != nil {
		t.Fatalf("get files: %v", err)
	}
	var files []string
	_ = json.NewDecoder(resp.Body).Decode(&files)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(files) != 2 || files[0] != "tui/model.go" {
		t.Fatalf("files want 200 + 2 entries, got %d %v", resp.StatusCode, files)
	}
}

func readAllString(resp *http.Response) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(resp.Body)
	return b.String(), err
}

func postJSON(t *testing.T, url string, body map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("post %s status %d", url, resp.StatusCode)
	}
}
