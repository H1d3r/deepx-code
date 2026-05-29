package tui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFileMentionContext(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		row, col   int
		wantActive bool
		wantQuery  string
		wantStart  int
	}{
		{"句尾 @ 起手", "@mod", 0, 4, true, "mod", 0},
		{"句中 @", "look at @src/m", 0, 14, true, "src/m", 8},
		{"空 query 刚打 @", "see @", 0, 5, true, "", 4},
		{"邮箱不触发", "mail me@host", 0, 12, false, "", 0},
		{"@ 后有空格不触发", "@foo bar", 0, 8, false, "", 0},
		{"空串", "", 0, 0, false, "", 0},
		{"多行第二行 @", "a\n@mod", 1, 4, true, "mod", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, _, query, active := fileMentionContext(tc.value, tc.row, tc.col)
			if active != tc.wantActive || query != tc.wantQuery || (active && start != tc.wantStart) {
				t.Fatalf("fileMentionContext(%q,%d,%d) = (start=%d,query=%q,active=%v), want (start=%d,query=%q,active=%v)",
					tc.value, tc.row, tc.col, start, query, active, tc.wantStart, tc.wantQuery, tc.wantActive)
			}
		})
	}
}

func TestFileMentionContext_ReplaceSpan(t *testing.T) {
	// 验证 [start,end) 正好框住 "@query",可被整段替换
	val := "look at @src/mo more"
	start, end, query, active := fileMentionContext(val, 0, 15) // 光标在 "@src/mo" 之后
	if !active || query != "src/mo" {
		t.Fatalf("query=%q active=%v", query, active)
	}
	got := val[:start] + "@CHOSEN " + val[end:]
	if want := "look at @CHOSEN  more"; got != want {
		t.Fatalf("替换后 = %q, want %q", got, want)
	}
}

func TestFilterWorkspaceFiles(t *testing.T) {
	files := []string{"tui/model.go", "tui/view.go", "agent/llm.go", "README.md"}
	// 前缀(basename)优先
	got := filterWorkspaceFiles("model", files, 10)
	if len(got) == 0 || got[0] != "tui/model.go" {
		t.Fatalf("query=model got=%v", got)
	}
	// 子串回退
	got = filterWorkspaceFiles("llm", files, 10)
	if !slices.Contains(got, "agent/llm.go") {
		t.Fatalf("query=llm 应命中 agent/llm.go, got=%v", got)
	}
	// 空 query 返回全部(截到 limit)
	if got = filterWorkspaceFiles("", files, 2); len(got) != 2 {
		t.Fatalf("空 query + limit=2 应返回 2 条, got=%v", got)
	}
}

func TestResolveFileMentions(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "real.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in, want string
	}{
		{"看下 @real.go 这个", "看下 `real.go` 这个"},    // 真实文件 → 反引号路径
		{"列下 @pkg/ 目录", "列下 `pkg/` 目录"},          // 真实目录(带尾随 /)→ 反引号路径
		{"@nope.go 不存在", "@nope.go 不存在"},         // 不存在 → 原样
		{"mail me@host.com", "mail me@host.com"}, // 邮箱(非文件)→ 原样
	}
	for _, tc := range cases {
		if got := resolveFileMentions(tc.in, ws); got != tc.want {
			t.Errorf("resolveFileMentions(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestListWorkspaceFiles(t *testing.T) {
	ws := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("foo.go")
	mk("sub/bar.txt")
	mk(".git/config")
	mk("node_modules/dep/index.js")
	mk(".hidden")

	got := listWorkspaceFiles(ws)
	if !slices.Contains(got, "foo.go") || !slices.Contains(got, "sub/bar.txt") {
		t.Fatalf("应包含 foo.go 和 sub/bar.txt, got=%v", got)
	}
	if !slices.Contains(got, "sub/") {
		t.Errorf("应包含目录 sub/(带尾随 /), got=%v", got)
	}
	for _, bad := range []string{".git/config", "node_modules/dep/index.js", ".hidden",
		".git/", "node_modules/", ".git"} {
		if slices.Contains(got, bad) {
			t.Errorf("不应包含被忽略项 %q, got=%v", bad, got)
		}
	}
}
