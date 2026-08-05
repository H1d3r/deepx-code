package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件钉的是 rewriteToolCallArgsForHistory 的核心不变量:
// **只修 arguments 的 JSON 合法性,不改任何工具的参数语义。**
//
// 这里曾经把 Write 的大 content 折叠成 "[已写入 …]" 的引用描述。移除的原因见
// rewriteToolCallArgsForHistory 的注释:被系统改写过的参数形态会出现在历史里,
// 模型会把它当成该工具的标准写法模仿。防上下文膨胀改由工具层上限
// (tools.SetWriteContentLimit,随窗口自适应)+ 压缩兜底承担。

// mkTC 构造一个工具调用。
func mkTC(name, argsJSON string) ToolCall {
	return ToolCall{ID: "id1", Type: "function", Function: ToolCallFunc{Name: name, Arguments: argsJSON}}
}

// argsMap 把 arguments JSON 解回 map,断言仍是合法 JSON。
func argsMap(t *testing.T, argsJSON string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v\n%s", err, argsJSON)
	}
	return m
}

// TestWriteArgs_LargeContentKeptVerbatim 是这次改动的核心断言:
// 大 content 原样入历史,不折叠、不改写、不截断。
func TestWriteArgs_LargeContentKeptVerbatim(t *testing.T) {
	content := strings.Repeat("这是一段中文内容。", 200) // 远超旧阈值 512 字节
	raw := `{"path":"a/b/中文.go","content":` + jsonStr(content) + `}`

	got := rewriteToolCallArgsForHistory([]ToolCall{mkTC("Write", raw)})[0].Function.Arguments

	if got != raw {
		t.Fatalf("大 content 的 Write 应原样保留\n want=%s\n got =%s", raw, got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("结果含非法 UTF-8: %q", got)
	}
	m := argsMap(t, got)
	if c, _ := m["content"].(string); c != content {
		t.Fatalf("content 应逐字节一致(len want=%d got=%d)", len(content), len(c))
	}
	if p, _ := m["path"].(string); p != "a/b/中文.go" {
		t.Fatalf("path 应保持不变, got=%q", p)
	}
	// 折叠时代的痕迹一个都不该出现
	for _, marker := range []string{"已写入", "content_omitted", "需要内容用 Read 查看"} {
		if strings.Contains(got, marker) {
			t.Fatalf("历史里不应出现系统改写的痕迹 %q", marker)
		}
	}
}

func TestWriteArgs_SmallContentKeptVerbatim(t *testing.T) {
	in := mkTC("Write", `{"path":"x.txt","content":"小内容"}`)
	out := rewriteToolCallArgsForHistory([]ToolCall{in})
	if out[0].Function.Arguments != in.Function.Arguments {
		t.Fatalf("小 content 应原样保留\n want=%s\n got =%s", in.Function.Arguments, out[0].Function.Arguments)
	}
}

// TestWriteArgs_NoHTMLEscape 没有重新编码就不会有 HTML 转义问题。
// 旧实现要靠 json.Encoder + SetEscapeHTML(false) 兜住,现在从源头消失了。
func TestWriteArgs_NoHTMLEscape(t *testing.T) {
	raw := `{"path":"a<b>&c.go","content":` + jsonStr(strings.Repeat("x", 600)) + `}`
	got := rewriteToolCallArgsForHistory([]ToolCall{mkTC("Write", raw)})[0].Function.Arguments
	if !strings.Contains(got, "a<b>&c.go") {
		t.Fatalf("< > & 应保持字面量,不被 HTML 转义, got=%s", got)
	}
	if p, _ := argsMap(t, got)["path"].(string); p != "a<b>&c.go" {
		t.Fatalf("path 应原样保留 < > &, got=%q", p)
	}
}

func TestUpdate_NeverTruncated(t *testing.T) {
	// Update 的 old_string/new_string 承载 diff 语义,Read 补不回来,一律原样。
	old := strings.Repeat("旧", 500)
	nw := strings.Repeat("新", 500)
	raw := `{"path":"f.go","old_string":` + jsonStr(old) + `,"new_string":` + jsonStr(nw) + `}`
	out := rewriteToolCallArgsForHistory([]ToolCall{mkTC("Update", raw)})
	if out[0].Function.Arguments != raw {
		t.Fatalf("Update 应原样保留,不裁剪\n want=%s\n got =%s", raw, out[0].Function.Arguments)
	}
}

func TestOtherTools_Untouched(t *testing.T) {
	in := mkTC("Bash", `{"command":"`+strings.Repeat("echo ", 300)+`"}`)
	out := rewriteToolCallArgsForHistory([]ToolCall{in})
	if out[0].Function.Arguments != in.Function.Arguments {
		t.Fatalf("非 Write 工具不应被改动")
	}
}

// TestInvalidJSON_StillRepaired 非法 JSON 仍要被修成合法的 —— 这是 issue #201 的修复,
// 与折叠是两件事,删折叠不能把它一起删掉。
func TestInvalidJSON_StillRepaired(t *testing.T) {
	broken := "{not json" + strings.Repeat("x", 600)
	got := rewriteToolCallArgsForHistory([]ToolCall{mkTC("Write", broken)})[0].Function.Arguments
	if !json.Valid([]byte(got)) {
		t.Fatalf("坏 arguments 必须被修成合法 JSON(否则严格后端会对后续所有请求 400), got=%q", got)
	}
}

func TestRewrite_DoesNotMutateOriginal(t *testing.T) {
	// 执行仍用原始 toolCalls:确认原始未被改动。
	raw := `{"path":"z.go","content":` + jsonStr(strings.Repeat("y", 600)) + `}`
	orig := []ToolCall{mkTC("Write", raw)}
	_ = rewriteToolCallArgsForHistory(orig)
	if orig[0].Function.Arguments != raw {
		t.Fatalf("原始 toolCalls 不应被改动(执行用的是它)")
	}
}

func TestRewrite_MixedBatch(t *testing.T) {
	bigWrite := `{"path":"big.go","content":` + jsonStr(strings.Repeat("a", 600)) + `}`
	tcs := []ToolCall{
		mkTC("Write", bigWrite),
		mkTC("Update", `{"path":"u.go","old_string":"a","new_string":"b"}`),
		mkTC("Read", `{"path":"r.go"}`),
	}
	out := rewriteToolCallArgsForHistory(tcs)
	for i := range tcs {
		if out[i].Function.Arguments != tcs[i].Function.Arguments {
			t.Fatalf("批次中第 %d 条(%s)应原样保留\n want=%s\n got =%s",
				i, tcs[i].Function.Name, tcs[i].Function.Arguments, out[i].Function.Arguments)
		}
	}
}

// jsonStr 把字符串编成合法 JSON 字面量(带引号、转义),供拼接测试用例。
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
