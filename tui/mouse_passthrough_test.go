package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// 右键粘贴(issue #211)必须与终端 PasteMsg 走同一条 insertPastedText 管线。
// 这两条是当初直接 InsertString 会踩的坑,单独钉住。

// CRLF 必须归一:不归一的话 textarea 的 Sanitizer 会把 \r\n 拆成两个 \n,
// 行数翻倍、输入框和历史一起错乱 —— 而右键粘贴的主要用户就在 Windows / WSL2,剪贴板全是 CRLF。
func TestInsertPastedText_NormalizesCRLF(t *testing.T) {
	m := initModel()
	if cmd := m.insertPastedText("第一行\r\n第二行"); cmd != nil {
		_ = cmd()
	}
	got := m.input.Value()
	if strings.Contains(got, "\r") {
		t.Fatalf("粘贴内容不该残留 \\r: %q", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Fatalf("两行文本应只有 1 个换行(CRLF 被拆成两个 \\n 就会变 2),got %d: %q", n, got)
	}
}

// 超阈值文本走占位符:直插会被 textarea 的 CharLimit(4000)静默截断,还撑爆输入区。
func TestInsertPastedText_LongTextUsesPlaceholder(t *testing.T) {
	m := initModel()
	long := strings.Repeat("x", pasteTextThreshold+1)
	if cmd := m.insertPastedText(long); cmd != nil {
		_ = cmd()
	}
	got := m.input.Value()
	if strings.Contains(got, long) {
		t.Fatal("超长文本应换成占位符,而不是整段插进输入框")
	}
	if !strings.Contains(got, "Pasted text") {
		t.Fatalf("输入框里应是占位符, got %q", got)
	}
	if len(m.pastedTexts) != 1 {
		t.Fatalf("全文应存进 pastedTexts 待提交时展开, got %d 条", len(m.pastedTexts))
	}
	for _, v := range m.pastedTexts {
		if v != long {
			t.Error("存下的应是完整原文")
		}
	}

	// 多行同理(即使很短):超过 pasteLineCap 也走占位符
	m2 := initModel()
	if cmd := m2.insertPastedText("a\nb\nc\nd"); cmd != nil {
		_ = cmd()
	}
	if !strings.Contains(m2.input.Value(), "Pasted text") {
		t.Errorf("超过 pasteLineCap 行数应走占位符, got %q", m2.input.Value())
	}
}

// 短文本正常进输入框,不该被占位符逻辑误伤。
func TestInsertPastedText_ShortTextGoesInline(t *testing.T) {
	m := initModel()
	if cmd := m.insertPastedText("hello"); cmd != nil {
		_ = cmd()
	}
	if got := m.input.Value(); !strings.Contains(got, "hello") {
		t.Fatalf("短文本应直接进输入框, got %q", got)
	}
	if len(m.pastedTexts) != 0 {
		t.Error("短文本不该占用 pastedTexts")
	}
}

// 输入区命中判断:左键(进入编辑)与右键(粘贴)共用,别再各写一份。
func TestInInputArea(t *testing.T) {
	m := initModel() // width=80 height=30
	_, vpH := m.layout()
	leftW, _ := m.layout()

	cases := []struct {
		name string
		x, y int
		want bool
	}{
		{"输入区左上角", 0, vpH, true},
		{"输入区内部", 5, vpH + 1, true},
		{"chat 区(输入区上方)", 5, vpH - 1, false},
		{"右侧状态栏同一行", leftW, vpH, false},
		{"屏幕下沿之外", 5, m.height, false},
		{"负坐标", -1, vpH, false},
	}
	for _, c := range cases {
		if got := m.inInputArea(c.x, c.y); got != c.want {
			t.Errorf("%s: inInputArea(%d,%d) = %v, want %v", c.name, c.x, c.y, got, c.want)
		}
	}
}

// F2 开启穿透后,View 必须把鼠标捕获关掉,终端才能接管选择与右键粘贴。
func TestWrapView_MouseModeFollowsPassthrough(t *testing.T) {
	m := initModel()
	if got := m.wrapView("x").MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("默认应捕获鼠标, got %v", got)
	}
	m.mousePassthrough = true
	if got := m.wrapView("x").MouseMode; got != tea.MouseModeNone {
		t.Fatalf("穿透开启时应关掉鼠标捕获, got %v", got)
	}
}

// 切换穿透时必须清掉选区/拖拽态:穿透打开后收不到任何鼠标事件,
// 若此刻正拖着选区,MouseReleaseMsg 永远不会来,高亮会一直卡在屏幕上。
func TestToggleMousePassthrough_ClearsDragState(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // metaUpdate 会落盘,重定向 HOME 免得动到真实 ~/.deepx/meta.json
	m := initModel()
	m.selecting = true
	m.inputSelecting = true
	m.inputDragging = true
	m.scrollbarDragging = true

	m.toggleMousePassthrough()

	if !m.mousePassthrough {
		t.Fatal("应切到穿透开启")
	}
	if m.selecting || m.inputSelecting || m.inputDragging || m.scrollbarDragging {
		t.Error("切换时应清掉所有选区/拖拽态,否则高亮会卡住")
	}
	if !strings.Contains(m.chatContent.String(), T("mouse.passthrough.on")) {
		t.Error("应给出开启提示")
	}

	m.toggleMousePassthrough()
	if m.mousePassthrough {
		t.Fatal("再按一次应关闭")
	}
	if !strings.Contains(m.chatContent.String(), T("mouse.passthrough.off")) {
		t.Error("应给出关闭提示")
	}
}
