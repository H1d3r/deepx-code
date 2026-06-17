package tools

import (
	"os"
	"strings"
	"testing"
)

func TestResolveEditTarget(t *testing.T) {
	content := "func f() {\n\tx := 1\n\treturn x\n}\n"

	cases := []struct {
		name      string
		search    string
		wantCount int
		wantText  string // 命中文件里的真实文本(唯一时)
	}{
		{"精确", "\tx := 1", 1, "\tx := 1"},
		{"行尾多空格", "\tx := 1   ", 1, "\tx := 1"},       // 模型尾部多敲了空格
		{"缩进漂移(空格代Tab)", "    x := 1", 1, "\tx := 1"}, // 模型用空格、文件是 Tab
		{"多行+缩进漂移", "    x := 1\n    return x", 1, "\tx := 1\n\treturn x"},
		{"不存在", "y := 2", 0, ""},
	}
	for _, c := range cases {
		got, n, _ := resolveEditTarget(content, c.search)
		if n != c.wantCount {
			t.Errorf("[%s] count=%d want %d", c.name, n, c.wantCount)
			continue
		}
		if c.wantCount == 1 && got != c.wantText {
			t.Errorf("[%s] actual=%q want %q", c.name, got, c.wantText)
		}
	}
}

func TestResolveEditTarget_Ambiguous(t *testing.T) {
	content := "a := 1\nb := 2\na := 1\n"
	// 精确出现 2 次 → count=2(上层据此要求 replace_all 或补上下文)
	if _, n, _ := resolveEditTarget(content, "a := 1"); n != 2 {
		t.Fatalf("精确多处应 count=2, got %d", n)
	}
	// 空白容差下也多处 → count>1,不返回真实文本
	got, n, _ := resolveEditTarget(content, "a := 1  ")
	if n <= 1 || got != "" {
		t.Fatalf("容差多处应 count>1 且不返回文本, got count=%d text=%q", n, got)
	}
}

func TestUnescapeLiteralControls(t *testing.T) {
	if got := unescapeLiteralControls(`a\nb\tc`); got != "a\nb\tc" {
		t.Fatalf("got %q", got)
	}
}

func TestToLF(t *testing.T) {
	if lf, crlf := toLF("a\r\nb\r\n"); !crlf || lf != "a\nb\n" {
		t.Fatalf("toLF crlf=%v lf=%q", crlf, lf)
	}
	if _, crlf := toLF("a\nb\n"); crlf {
		t.Fatal("纯 LF 不应判 crlf")
	}
}

func TestLocateEditTargetLine(t *testing.T) {
	content := "package x\n\nfunc f() {\n\tx := 1\n\treturn x\n}\n"
	// 精确:第 4 行
	if got := LocateEditTargetLine(content, "\tx := 1"); got != 4 {
		t.Fatalf("精确 行号 got %d want 4", got)
	}
	// 缩进漂移(空格代 Tab):仍应定位到第 4 行
	if got := LocateEditTargetLine(content, "    x := 1"); got != 4 {
		t.Fatalf("容差 行号 got %d want 4", got)
	}
	// CRLF 文件 + LF needle:仍应定位
	crlf := strings.ReplaceAll(content, "\n", "\r\n")
	if got := LocateEditTargetLine(crlf, "\tx := 1"); got != 4 {
		t.Fatalf("CRLF 行号 got %d want 4", got)
	}
	// 找不到 → 0
	if got := LocateEditTargetLine(content, "nope"); got != 0 {
		t.Fatalf("找不到应 0, got %d", got)
	}
}

func TestEditFile_RecordsLine(t *testing.T) {
	dir := t.TempDir()
	// workspace 未注入时 confineToWorkspace 放行,临时绝对路径直接可写。
	p := dir + "/f.txt"
	if err := os.WriteFile(p, []byte("l1\nl2\ntarget\nl4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := EditFile(map[string]any{"path": p, "old_string": "target", "new_string": "TARGET"})
	if !res.Success {
		t.Fatalf("edit 失败: %s", res.Output)
	}
	// 编辑后再取:行号应是命中段的真实起始行(第 3 行),与"改后文件已无 target"无关
	if ln, ok := RecordedEditLine(p, "target"); !ok || ln != 3 {
		t.Fatalf("RecordedEditLine got (%d,%v) want (3,true)", ln, ok)
	}
}
