package tools

import (
	"os"
	"strings"
	"testing"
)

// withTempWorkspace 把工作区切到临时目录,测试结束还原 —— Write 受 confineToWorkspace 约束。
func withTempWorkspace(t *testing.T) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// withLimit 临时设置上限,测试结束还原(writeLimit 是包级状态)。
func withLimit(t *testing.T, n int) {
	t.Helper()
	prev := writeLimit.Load()
	SetWriteContentLimit(n)
	t.Cleanup(func() { writeLimit.Store(prev) })
}

// TestWriteContentLimit_DefaultWhenUnset 未注入时回落保守默认,不是 0(0 会把一切写入都拒掉)。
func TestWriteContentLimit_DefaultWhenUnset(t *testing.T) {
	withLimit(t, 0)
	if got := WriteContentLimit(); got != defaultWriteLimit {
		t.Fatalf("未注入时应回落 %d, got %d", defaultWriteLimit, got)
	}
	withLimit(t, -1)
	if got := WriteContentLimit(); got != defaultWriteLimit {
		t.Fatalf("负值应回落 %d, got %d", defaultWriteLimit, got)
	}
}

// TestWriteFile_RejectsOversizedContent 超限直接拒绝,且错误信息不回显 content ——
// 错误会进模型上下文,回显等于把超大内容再喂一遍。
func TestWriteFile_RejectsOversizedContent(t *testing.T) {
	withTempWorkspace(t)
	withLimit(t, 1024)

	content := strings.Repeat("x", 1025)
	res := WriteFile(map[string]any{"path": "big.txt", "content": content})

	if res.Success {
		t.Fatal("超限写入应被拒绝")
	}
	if strings.Contains(res.Output, content) {
		t.Fatal("错误信息不应回显 content")
	}
	// 提示必须可执行,否则模型会原样重试、白烧一遍输出 token
	for _, want := range []string{"分批", "Update", "1024"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("错误信息应含 %q(可执行的替代方案 + 当前上限): %s", want, res.Output)
		}
	}
	if _, err := os.Stat("big.txt"); !os.IsNotExist(err) {
		t.Error("被拒绝的写入不应落盘")
	}
}

// TestWriteFile_RejectBeforeSideEffects 拒绝发生在任何文件系统操作之前:父目录也不该被创建。
func TestWriteFile_RejectBeforeSideEffects(t *testing.T) {
	withTempWorkspace(t)
	withLimit(t, 1024)

	res := WriteFile(map[string]any{"path": "newdir/sub/big.txt", "content": strings.Repeat("x", 2048)})
	if res.Success {
		t.Fatal("超限写入应被拒绝")
	}
	if _, err := os.Stat("newdir"); !os.IsNotExist(err) {
		t.Error("不应为一个注定失败的写入创建父目录")
	}
}

// TestWriteFile_SizeBoundary 边界:恰好等于上限放行,+1 拒绝。
func TestWriteFile_SizeBoundary(t *testing.T) {
	withTempWorkspace(t)
	withLimit(t, 1024)

	if res := WriteFile(map[string]any{"path": "exact.txt", "content": strings.Repeat("x", 1024)}); !res.Success {
		t.Fatalf("恰好等于上限应放行, got %s", res.Output)
	}
	if res := WriteFile(map[string]any{"path": "over.txt", "content": strings.Repeat("x", 1025)}); res.Success {
		t.Fatal("超过上限 1 字节应拒绝")
	}
}

// TestWriteFile_LimitIsAdaptive 上限是可注入的,不是编译期常量。
func TestWriteFile_LimitIsAdaptive(t *testing.T) {
	withTempWorkspace(t)
	content := strings.Repeat("x", 4096)

	withLimit(t, 2048)
	if res := WriteFile(map[string]any{"path": "a.txt", "content": content}); res.Success {
		t.Fatal("上限 2048 时 4096 字节应被拒绝")
	}
	SetWriteContentLimit(8192) // 换了个大窗口的模型
	if res := WriteFile(map[string]any{"path": "b.txt", "content": content}); !res.Success {
		t.Fatalf("上限提到 8192 后同样内容应放行, got %s", res.Output)
	}
}

// TestWriteFile_NormalWriteUnaffected 正常写入不受影响,内容逐字节一致。
func TestWriteFile_NormalWriteUnaffected(t *testing.T) {
	withTempWorkspace(t)
	withLimit(t, 64*1024)

	const body = "package main\n\nfunc main() {}\n"
	if res := WriteFile(map[string]any{"path": "main.go", "content": body}); !res.Success {
		t.Fatalf("正常写入应成功, got %s", res.Output)
	}
	got, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(got) != body {
		t.Fatalf("内容应逐字节一致\n want=%q\n got =%q", body, string(got))
	}
}

// TestWriteToolSpec_RendersActualLimit 工具描述里的上限占位符必须渲染成当前实际值 ——
// 模型只有知道确切字节数才能事先把大文件拆好,写死数字或含糊措辞都会骗到它。
func TestWriteToolSpec_RendersActualLimit(t *testing.T) {
	withLimit(t, 19660)

	var spec *OpenAIToolSpec
	for _, tl := range Tools {
		if tl.Name == "Write" {
			s := tl.ToOpenAISpec()
			spec = &s
		}
	}
	if spec == nil {
		t.Fatal("找不到 Write 工具")
	}
	desc := spec.Function.Description
	if strings.Contains(desc, descWriteLimitToken) {
		t.Errorf("占位符未被渲染: %s", desc)
	}
	if !strings.Contains(desc, "19660") {
		t.Errorf("描述里应含当前上限 19660, got: %s", desc)
	}

	// 换个窗口 → 描述跟着变
	SetWriteContentLimit(9828)
	for _, tl := range Tools {
		if tl.Name == "Write" {
			if d := tl.ToOpenAISpec().Function.Description; !strings.Contains(d, "9828") {
				t.Errorf("换上限后描述应跟着变, got: %s", d)
			}
		}
	}
}

// TestToolSpec_StableForPrefixCache 同一上限下反复渲染必须逐字节一致 ——
// 工具定义 JSON 是缓存前缀的一部分,抖一下就击穿缓存。
func TestToolSpec_StableForPrefixCache(t *testing.T) {
	withLimit(t, 19660)
	var first string
	for i := range 3 {
		var sb strings.Builder
		for _, tl := range Tools {
			sb.WriteString(tl.ToOpenAISpec().Function.Description)
		}
		if i == 0 {
			first = sb.String()
			continue
		}
		if sb.String() != first {
			t.Fatal("同一上限下工具描述不稳定,会击穿前缀缓存")
		}
	}
}
