// Package mcp 是 deepx 的 Model Context Protocol 客户端:把外部 MCP server 暴露的工具接入
// deepx,转发给 LLM 调用。手写 JSON-RPC,不引 SDK。
//
// 支持两种传输:
//   - stdio:server 作为子进程,经 stdin/stdout 行分隔 JSON 通信(本地 server,npx/python -m)
//   - http :Streamable HTTP,POST JSON-RPC 到一个 URL,响应可能是 application/json 或
//     text/event-stream(SSE)。覆盖远程 / 独立运行的 MCP server。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const protocolVersion = "2024-11-05"
const requestTimeout = 30 * time.Second

// ToolDef 是 MCP server 通过 tools/list 返回的一个工具定义。
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// transport 抽象 stdio / http 两种传输:都提供同步请求 + 通知 + 关闭。
type transport interface {
	call(method string, params any) (json.RawMessage, error)
	notify(method string, params any) error
	close()
}

// Client 是一个 MCP server 连接(传输无关)。
type Client struct {
	t transport
}

// Connect 按配置选传输(URL 非空走 http,否则 stdio),建立连接并完成 MCP 握手。
func Connect(cfg ServerConfig) (*Client, error) {
	var t transport
	var err error
	if cfg.URL != "" {
		t = newHTTPTransport(cfg.URL, cfg.Headers)
	} else {
		t, err = newStdioTransport(cfg.Command, cfg.Args, cfg.Env)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{t: t}
	initParams := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "deepx", "version": "1"},
	}
	if _, err := t.call("initialize", initParams); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP 握手失败: %w", err)
	}
	if err := t.notify("notifications/initialized", nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ListTools 拉取 server 暴露的工具清单。
func (c *Client) ListTools() ([]ToolDef, error) {
	raw, err := c.t.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool 调用 server 上的某个工具,返回拼接后的文本结果。
func (c *Client) CallTool(tool string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.t.call("tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	var sb []byte
	for _, part := range out.Content {
		if part.Text != "" {
			if len(sb) > 0 {
				sb = append(sb, '\n')
			}
			sb = append(sb, part.Text...)
		}
	}
	text := string(sb)
	if out.IsError {
		return text, fmt.Errorf("MCP 工具返回错误")
	}
	return text, nil
}

// Close 关闭连接。
func (c *Client) Close() {
	if c.t != nil {
		c.t.close()
	}
}

// ============ stdio transport ============

// writeReq 让 sendPayload 把"写一次 JSON"的工作交给专用 writer goroutine。
// 用 channel 模式而非 mutex 是因为:mutex 一旦被卡死 enc.Encode 持有,所有等待者全堵在 Lock 上;
// 用 channel 则可以在提交端用 select+timeout 跳出来,把"无法写入"转成可观察的错误。
type writeReq struct {
	payload any
	done    chan error
}

type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	enc   *json.Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse
	closed  bool

	// writeCh:所有写入串行化经由此 channel,由 writerLoop 唯一消费者执行 enc.Encode。
	// 这样持锁的不是写 stdin 的同一个 goroutine,提交方可超时跳出,避免全员阻塞。
	writeCh chan writeReq
	// stopCh:close() 触发时关闭,通知 writerLoop 退出、唤醒所有阻塞在 sendPayload 上的调用方。
	stopCh chan struct{}
}

func newStdioTransport(command string, args []string, env map[string]string) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = append([]string(nil), os.Environ()...)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP server 失败: %w", err)
	}
	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		enc:     json.NewEncoder(stdin),
		pending: map[int64]chan rpcResponse{},
		writeCh: make(chan writeReq, 32),
		stopCh:  make(chan struct{}),
	}
	go t.writerLoop()
	go t.readLoop(stdout)
	return t, nil
}

// writeTimeout 是 stdin 写入(包括"提交到写队列"和"实际 enc.Encode 完成")两段各自的预算。
// 5s 留得宽,但远低于 requestTimeout(30s)—— 写阻塞通常意味着对端进程死锁,30s 太久;
// 5s 就把死锁断开,避免后续所有 MCP 调用全卡同一个 transport。
//
// var 而非 const:测试时可调小到 100ms 量级,避免单测真等 5 秒。
var writeTimeout = 5 * time.Second

// writerLoop 是 stdio transport 唯一执行 enc.Encode 的 goroutine。
// 串行化写入(json.Encoder 并发不安全),同时让"卡在 enc.Encode 上"只阻塞自己,
// 调用方走 sendPayload 的 select+timeout 跳出,不会被锁住。
func (t *stdioTransport) writerLoop() {
	for {
		select {
		case <-t.stopCh:
			return
		case req := <-t.writeCh:
			req.done <- t.enc.Encode(req.payload)
		}
	}
}

// sendPayload 把"写一次 JSON"提交给 writerLoop,双段超时保护:
//  1. 入队超时(writeCh 满 = writer 在跑 encode 没空 + 队列堆满)→ server 大概率死锁,强制 close 连接
//  2. 写完超时(writerLoop 在 enc.Encode 卡了)→ 同上,强制 close
//
// close 后所有 pending 调用通过 stopCh 同步返回错误,后续新调用也立刻拿"连接已关闭"。
// 这就把"一个工具死锁让全 MCP 卡死"的故障收敛为"那一次返错,后续路径仍清晰"。
func (t *stdioTransport) sendPayload(payload any) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("MCP 连接已关闭")
	}
	t.mu.Unlock()

	done := make(chan error, 1)
	req := writeReq{payload: payload, done: done}

	// 段 1:入队
	select {
	case t.writeCh <- req:
	case <-t.stopCh:
		return fmt.Errorf("MCP 连接已关闭")
	case <-time.After(writeTimeout):
		go t.close()
		return fmt.Errorf("MCP 写入队列阻塞超时(%s),server 可能死锁,连接已断开", writeTimeout)
	}

	// 段 2:等 enc.Encode 完成
	select {
	case err := <-done:
		return err
	case <-t.stopCh:
		return fmt.Errorf("MCP 连接已关闭")
	case <-time.After(writeTimeout):
		// writerLoop 卡在 enc.Encode → stdin pipe buffer 满 → 对端死锁不读。
		// 异步触发 close(里面会 stdin.Close,释放卡住的 enc.Encode)
		go t.close()
		return fmt.Errorf("MCP 写入 stdin 超时(%s),server 死锁,连接已断开", writeTimeout)
	}
}

func (t *stdioTransport) call(method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("MCP 连接已关闭")
	}
	t.nextID++
	id := t.nextID
	ch := make(chan rpcResponse, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	if err := t.sendPayload(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		t.mu.Lock()
		if t.pending != nil {
			delete(t.pending, id)
		}
		t.mu.Unlock()
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("MCP 连接中断")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		t.mu.Lock()
		if t.pending != nil {
			delete(t.pending, id)
		}
		t.mu.Unlock()
		return nil, fmt.Errorf("MCP 请求超时(%s)", method)
	case <-t.stopCh:
		return nil, fmt.Errorf("MCP 连接已关闭")
	}
}

func (t *stdioTransport) notify(method string, params any) error {
	return t.sendPayload(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *stdioTransport) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	for _, ch := range t.pending {
		close(ch)
	}
	t.pending = nil
	t.mu.Unlock()
	close(t.stopCh)     // 通知 writerLoop 退出 + 唤醒所有阻塞在 sendPayload 上的调用方
	_ = t.stdin.Close() // 关 stdin 让 writerLoop 卡在的 enc.Encode 返回 EPIPE,goroutine 不漏
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.cmd.Wait()
}

func (t *stdioTransport) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil || resp.ID == nil {
			continue
		}
		t.mu.Lock()
		ch := t.pending[*resp.ID]
		if ch != nil {
			delete(t.pending, *resp.ID)
		}
		t.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	t.close()
}

// ============ http transport(Streamable HTTP)============

type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu      sync.Mutex
	nextID  int64
	session string // initialize 返回的 Mcp-Session-Id,后续请求带上
}

func newHTTPTransport(url string, headers map[string]string) *httpTransport {
	return &httpTransport{url: url, headers: headers, client: &http.Client{Timeout: requestTimeout}}
}

func (h *httpTransport) post(body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", h.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.mu.Lock()
	session := h.session
	h.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.client.Do(req)
}

func (h *httpTransport) call(method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.mu.Unlock()
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	resp, err := h.post(body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.session = sid
		h.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSEResponse(resp.Body, id)
	}
	var r rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

func (h *httpTransport) notify(method string, params any) error {
	body, _ := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	resp, err := h.post(body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (h *httpTransport) close() {}

// readSSEResponse 从 SSE 流里找出 id 匹配的 JSON-RPC 响应(data: 行)。
func readSSEResponse(r io.Reader, id int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		var resp rpcResponse
		if json.Unmarshal([]byte(data), &resp) != nil || resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
	return nil, fmt.Errorf("SSE 流未返回 id=%d 的响应", id)
}
