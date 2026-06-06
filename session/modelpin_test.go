package session

import (
	"os"
	"path/filepath"
	"testing"
)

// /model 锁定按**子会话**(convDir)持久化:不同对话各记各的,缺失返回空串(上层归一 auto)。
func TestModelPinPerConversation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	ws := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}

	// 默认对话:缺失 → 空串
	if got := m.LoadModelPin(); got != "" {
		t.Errorf("初始应为空串,得到 %q", got)
	}
	// 默认对话存 flash
	m.SaveModelPin("flash")
	if got := m.LoadModelPin(); got != "flash" {
		t.Errorf("默认对话应读回 flash,得到 %q", got)
	}

	// 新建对话(/new):各记各的,新对话不继承 flash
	if _, err := m.NewConversation(); err != nil {
		t.Fatal(err)
	}
	if got := m.LoadModelPin(); got != "" {
		t.Errorf("新对话应为空串(不继承),得到 %q", got)
	}
	m.SaveModelPin("pro")
	if got := m.LoadModelPin(); got != "pro" {
		t.Errorf("新对话应读回 pro,得到 %q", got)
	}

	// 写 model_pin 不应破坏同文件里的 working_mode(共用 state.json)
	m.SaveWorkingMode("openspec")
	m.SaveModelPin("flash")
	if got := m.LoadWorkingMode(); got != "openspec" {
		t.Errorf("写 model_pin 不应覆盖 working_mode,得到 %q", got)
	}
	if got := m.LoadModelPin(); got != "flash" {
		t.Errorf("model_pin 应为 flash,得到 %q", got)
	}
}
