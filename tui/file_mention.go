package tui

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// @ 文件提及功能:输入框里打 "@" 触发文件选择器,选中后插入 "@相对路径",
// 提交时 buildUserMessage 把它解析成可被 Read 工具读取的路径(设计 B —— 不内联文件内容,
// 交给模型按需调 Read,跟图片占位符 → 路径 → img_ocr 的范式一致)。

const (
	fileMentionCacheCap = 5000 // 缓存的最大文件数,防止超大仓库把内存/遍历拖垮
	fileMentionMaxRows  = 10   // 选择器一次最多展示几行候选
)

var errStopWalk = errors.New("stop walk")

// listWorkspaceFiles 遍历 workspace,返回工作区相对路径(/ 分隔),按修改时间新→旧排序。
// 目录带尾随 "/"(便于在选择器里区分、选中后继续下钻),文件不带。
// 跳过 .git / node_modules / vendor 及隐藏目录,跳过隐藏文件;复用 glob_file.go 的忽略口径。
// 累计到 fileMentionCacheCap 即停,避免病态大仓库遍历过久。
func listWorkspaceFiles(root string) []string {
	type fent struct {
		rel string
		mod int64
	}
	var ents []fent
	add := func(rel string, mod int64) bool {
		ents = append(ents, fent{rel, mod})
		return len(ents) < fileMentionCacheCap
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := info.Name()
		if info.IsDir() {
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				(p != root && strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if p == root {
				return nil // 不收根目录自身
			}
			rel, e := filepath.Rel(root, p)
			if e != nil {
				return nil
			}
			if !add(filepath.ToSlash(rel)+"/", info.ModTime().Unix()) {
				return errStopWalk
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return nil
		}
		if !add(filepath.ToSlash(rel), info.ModTime().Unix()) {
			return errStopWalk
		}
		return nil
	})
	sort.Slice(ents, func(i, j int) bool { return ents[i].mod > ents[j].mod })
	out := make([]string, len(ents))
	for i, e := range ents {
		out[i] = e.rel
	}
	return out
}

// fileMentionContext 从光标位置往回扫,定位当前正在输入的 "@提及"。
// row/col 是 textarea 的光标行列(textarea.Line()/Column())。
// 返回:@ 的 rune 下标 start、光标 rune 偏移 end、@ 与光标之间的 query、是否处于提及态。
// 触发条件:@ 必须位于行首或紧跟空白(避免把 user@host 这类邮箱误判),且 @ 到光标间无空白。
func fileMentionContext(value string, row, col int) (start, end int, query string, active bool) {
	lines := strings.Split(value, "\n")
	if row < 0 || row >= len(lines) {
		return 0, 0, "", false
	}
	off := 0
	for i := range row {
		off += len([]rune(lines[i])) + 1 // +1 为换行符
	}
	off += col
	runes := []rune(value)
	if off > len(runes) {
		off = len(runes)
	}
	for i := off - 1; i >= 0; i-- {
		r := runes[i]
		if r == '@' {
			if i == 0 || isWhitespaceLike(runes[i-1]) {
				return i, off, string(runes[i+1 : off]), true
			}
			return 0, 0, "", false
		}
		if isWhitespaceLike(r) {
			return 0, 0, "", false
		}
	}
	return 0, 0, "", false
}

// filterWorkspaceFiles 按 query 过滤候选:basename / 路径前缀优先,其次子串。组内保持原(按时间)序。
func filterWorkspaceFiles(query string, files []string, limit int) []string {
	q := strings.ToLower(query)
	var pref, sub []string
	for _, f := range files {
		lf := strings.ToLower(f)
		if q == "" || strings.HasPrefix(lf, q) || strings.HasPrefix(strings.ToLower(filepath.Base(f)), q) {
			pref = append(pref, f)
		} else if strings.Contains(lf, q) {
			sub = append(sub, f)
		}
	}
	out := append(pref, sub...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// === 选择器渲染(对齐 palette.go 的 renderCommandPalette 视觉)===

// renderFileMentionPalette 把候选路径渲染成多行字符串,每行精确 width 列。
func renderFileMentionPalette(matches []string, selIdx, width int) string {
	if len(matches) == 0 || width <= 0 {
		return ""
	}
	var sb strings.Builder
	for i, p := range matches {
		marker := "  "
		if i == selIdx {
			marker = "▸ "
		}
		raw := marker + p
		if cur := ansi.StringWidth(raw); cur < width {
			raw += strings.Repeat(" ", width-cur)
		} else if cur > width {
			raw = ansi.Cut(raw, 0, width)
		}
		var styled string
		if i == selIdx {
			styled = paletteSelStyle.Render(raw)
		} else {
			markSeg := paletteDescStyle.Render(marker)
			pathSeg := paletteNameStyle.Render(p)
			styled = markSeg + pathSeg
			if cur := ansi.StringWidth(styled); cur < width {
				styled += strings.Repeat(" ", width-cur)
			} else if cur > width {
				styled = ansi.Cut(styled, 0, width)
			}
		}
		sb.WriteString(styled)
		if i < len(matches)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// === 提交时解析 ===

// fileMentionRe 匹配 "@路径" token(路径含字母数字及 / . - _ ~)。
var fileMentionRe = regexp.MustCompile(`@([A-Za-z0-9_./~\-]+)`)

// resolveFileMentions 把文本里指向真实存在路径(文件或目录)的 "@相对路径" 替换成反引号包裹的
// 相对路径,让模型识别为引用并按需调 Read(文件)/ List(目录)。指向不存在路径的 @token
// 原样保留(可能是邮箱、@here 等)。
func resolveFileMentions(text, workspace string) string {
	return fileMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		rel := strings.TrimPrefix(match, "@")
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workspace, rel)
		}
		if _, err := os.Stat(abs); err != nil {
			return match
		}
		return "`" + rel + "`"
	})
}
