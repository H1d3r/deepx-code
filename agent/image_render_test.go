package agent

import "testing"

// priorOCRResult 用于「同一图片路径已 OCR 过就别重复」的短路判定(issue #146)。
func TestPriorOCRResult(t *testing.T) {
	convo := []ChatMessage{
		{Role: "user", Content: "图在 /tmp/a.png,别识别了"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Function: ToolCallFunc{Name: "OCR", Arguments: `{"path":"/tmp/a.png"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "OCR", Content: "(图片中未识别到文字)"},
	}

	t.Run("同一路径命中并回上次结果", func(t *testing.T) {
		prev, ok := priorOCRResult(`{"path":"/tmp/a.png"}`, convo)
		if !ok || prev != "(图片中未识别到文字)" {
			t.Fatalf("应命中并返回上次结果,got %q ok=%v", prev, ok)
		}
	})

	t.Run("等价路径经Clean后仍命中", func(t *testing.T) {
		if _, ok := priorOCRResult(`{"path":"/tmp/./a.png"}`, convo); !ok {
			t.Fatalf("等价路径应命中")
		}
	})

	t.Run("不同路径不命中", func(t *testing.T) {
		if _, ok := priorOCRResult(`{"path":"/tmp/b.png"}`, convo); ok {
			t.Fatalf("不同路径不应命中")
		}
	})

	t.Run("无历史OCR不命中", func(t *testing.T) {
		fresh := []ChatMessage{{Role: "user", Content: "hi"}}
		if _, ok := priorOCRResult(`{"path":"/tmp/a.png"}`, fresh); ok {
			t.Fatalf("无先前 OCR 不应命中")
		}
	})

	t.Run("坏参数不panic不命中", func(t *testing.T) {
		if _, ok := priorOCRResult(`not json`, convo); ok {
			t.Fatalf("坏参数不应命中")
		}
	})
}
