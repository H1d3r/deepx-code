package tools

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// editLineCache:编辑时(文件还是改前)记下「(path, old_string) → 命中段精确起始行」,
// 供 UI 渲染 diff 行号时直接取——这样不论改前/改后、内容多常见,都有准确行号,
// 不再依赖渲染时回读当前文件 grep(改后 old_string 没了 / 短串歧义都会丢行号)。
var editLineCache sync.Map // key: path\x00old_string  → int(1-indexed)

func editLineKey(path, old string) string { return path + "\x00" + old }

// RecordedEditLine 取出某次编辑在文件里的真实起始行(编辑时记的);没有返回 (0,false)。
func RecordedEditLine(path, old string) (int, bool) {
	if v, ok := editLineCache.Load(editLineKey(path, old)); ok {
		return v.(int), true
	}
	return 0, false
}

// EditFile 字符串模式替换:
//
//	old_string  (string) 要替换的内容
//	new_string  (string) 替换为
//	replace_all (bool)   是否替换所有匹配，默认 false
//
// 匹配采用多级回退(对齐 codex/aider 等业界做法),把模型常见的空白漂移吸收掉,
// 大幅降低「old_string 找不到」:
//  1. 精确匹配;
//  2. 控制符 unescape 回退(模型把 \n/\t 当字面量发过来);
//  3. 行对齐 + 每行空白容差(先容行尾空白、再容首尾缩进),命中文件里【真实那段】文本后替换。
//
// 关键:始终靠【内容】定位、替换文件真实文本(不靠行号),所以不会错改无关行;命中必须唯一,
// 否则报错要求补上下文。CRLF 文件先归一为 LF 处理、写回时还原。
func EditFile(args map[string]any) ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return ToolResult{Output: "错误: path 参数为空", Success: false}
	}
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	if oldStr == "" {
		return ToolResult{Output: "错误: old_string 不能为空", Success: false}
	}
	if oldStr == newStr {
		return ToolResult{Output: "错误: new_string 必须与 old_string 不同", Success: false}
	}
	replaceAll, _ := args["replace_all"].(bool)

	absPath, err := confineToWorkspace(path)
	if err != nil {
		return ToolResult{Output: err.Error(), Success: false}
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("读取失败: %v", err), Success: false}
	}

	// CRLF 文件归一为 LF 处理,写回时还原;search/replace 同样归一,口径一致。
	content, crlf := toLF(string(data))
	search := strings.ReplaceAll(oldStr, "\r\n", "\n")
	replace := strings.ReplaceAll(newStr, "\r\n", "\n")

	actual, count, note := resolveEditTarget(content, search)
	if count == 0 {
		return ToolResult{Output: "错误: 在文件中未找到 old_string" + editDivergenceHint(content, search), Success: false}
	}
	if count > 1 && !replaceAll {
		return ToolResult{
			Output:  fmt.Sprintf("错误: old_string 出现 %d 次,请提供更长上下文或设置 replace_all=true", count),
			Success: false,
		}
	}

	// 编辑时(content 还是改前)记下命中段的精确起始行,供 UI 渲染 diff 行号直接取。
	if idx := strings.Index(content, actual); idx >= 0 {
		editLineCache.Store(editLineKey(path, oldStr), strings.Count(content[:idx], "\n")+1)
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(content, actual, replace)
	} else {
		updated = strings.Replace(content, actual, replace, 1)
	}
	out := updated
	if crlf {
		out = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	if err := os.WriteFile(absPath, []byte(out), 0o644); err != nil {
		return ToolResult{Output: fmt.Sprintf("写入失败: %v", err), Success: false}
	}
	CodeGraphInvalidate() // 文件变了,代码图谱缓存失效,下次查询重建
	return ToolResult{
		Output:  fmt.Sprintf("已替换 %d 处%s -> %s", count, note, absPath),
		Success: true,
	}
}

// LocateEditTargetLine 返回 search 在 content 里(经与 EditFile 同一套多级容差匹配)命中段的
// 首行行号(1-indexed);找不到 / 多处歧义返回 0。供 UI 预览渲染 diff 行号,口径与 EditFile 一致
// ——容差匹配成功时也能给出行号,不再因空白/缩进/CRLF 漂移而退化成无行号。
func LocateEditTargetLine(content, search string) int {
	lf, _ := toLF(content)
	s := strings.ReplaceAll(search, "\r\n", "\n")
	actual, count, _ := resolveEditTarget(lf, s)
	if count == 0 || actual == "" { // 0=没找到;actual==""=容差档下多处歧义,不给行号
		return 0
	}
	idx := strings.Index(lf, actual)
	if idx < 0 {
		return 0
	}
	return strings.Count(lf[:idx], "\n") + 1
}

// resolveEditTarget 多级回退定位:返回文件里【真实匹配到的文本】、匹配次数、以及一段说明(用了哪级回退)。
// count==0 表示没找到;count>1 表示多处(行对齐回退只在唯一时返回真实文本,多处时只回计数)。
func resolveEditTarget(content, search string) (actual string, count int, note string) {
	// 1. 精确匹配
	if c := strings.Count(content, search); c > 0 {
		return search, c, ""
	}
	// 2. 控制符 unescape 回退:模型把 \n / \t 当字面量(双反斜杠)发过来时还原再试
	if u := unescapeLiteralControls(search); u != search {
		if c := strings.Count(content, u); c > 0 {
			return u, c, "(已还原字面量转义)"
		}
	}
	// 3. 行对齐 + 空白容差:先只容行尾空白,再容首尾缩进。命中唯一才用(避免误改)。
	for _, m := range []struct {
		eq   func(a, b string) bool
		note string
	}{
		{eqLineRStrip, "(行尾空白容差匹配)"},
		{eqLineTrim, "(空白/缩进容差匹配)"},
	} {
		if a, c := locateLineAligned(content, search, m.eq); c == 1 {
			return a, 1, m.note
		} else if c > 1 {
			// 该容差级别下多处命中:不返回真实文本(replace 无法区分),交由上层按"多处"处理。
			return "", c, ""
		}
	}
	return "", 0, ""
}

// locateLineAligned 在 content 里按行查找与 search 各行(在 eq 容差下)逐行匹配的连续窗口。
// 唯一命中时返回文件里真实那段文本(原样,保留真实缩进);否则返回("", 命中数)。
func locateLineAligned(content, search string, eq func(a, b string) bool) (string, int) {
	sLines := strings.Split(strings.TrimSuffix(search, "\n"), "\n")
	cLines := strings.Split(content, "\n")
	m := len(sLines)
	if m == 0 {
		return "", 0
	}
	var hits []string
	for i := 0; i+m <= len(cLines); i++ {
		ok := true
		for k := 0; k < m; k++ {
			if !eq(cLines[i+k], sLines[k]) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, strings.Join(cLines[i:i+m], "\n"))
			i += m - 1 // 跳过本窗口,避免重叠重复计数
		}
	}
	if len(hits) == 1 {
		return hits[0], 1
	}
	return "", len(hits)
}

func eqLineRStrip(a, b string) bool {
	return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
}
func eqLineTrim(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// unescapeLiteralControls 把字面量的 \n \t \r(反斜杠+字母)还原成真实控制符。仅作回退用。
func unescapeLiteralControls(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r").Replace(s)
}

// toLF 把内容归一为 LF;返回是否原本是 CRLF(用于写回还原)。
func toLF(s string) (string, bool) {
	if strings.Contains(s, "\r\n") {
		return strings.ReplaceAll(s, "\r\n", "\n"), true
	}
	return s, false
}

// editDivergenceHint 找不到时给一句有用的提示:若 search 的某些行能在文件里单独找到,
// 多半是整体空白/缩进/相邻行对不上 —— 提示重新 Read 后逐字复制。
func editDivergenceHint(content, search string) string {
	sLines := strings.Split(strings.TrimSuffix(search, "\n"), "\n")
	found := 0
	for _, ln := range sLines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.Contains(content, t) {
			found++
		}
	}
	if found > 0 {
		return "(部分行能单独找到,可能是缩进/空白/相邻行对不上 —— 请重新 Read 该文件并逐字复制 old_string,注意保留原始缩进)"
	}
	return "(该文本不在文件中 —— 请先 Read 确认文件当前内容)"
}
