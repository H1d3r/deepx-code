package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadFile 读取文本文件。
// 参数:
//
//	path   (string)         文件路径
//	offset (int, 可选)      起始行号（从 1 开始）
//	limit  (int, 可选)      最多读取行数，默认 2000
func ReadFile(args map[string]any) ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return ToolResult{Output: "错误: path 参数为空", Success: false}
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("路径错误: %v", err), Success: false}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("文件不存在: %v", err), Success: false}
	}
	if info.IsDir() {
		return ToolResult{Output: "目标是目录,请使用 List 工具", Success: false}
	}
	if info.Size() > 10*1024*1024 {
		return ToolResult{Output: "文件过大（>10MB）", Success: false}
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("读取失败: %v", err), Success: false}
	}

	offset := toInt(args["offset"], 1)
	limit := toInt(args["limit"], 2000)
	if offset < 1 {
		offset = 1
	}

	lines := strings.Split(string(data), "\n")
	if offset > len(lines) {
		return ToolResult{Output: "(空范围)", Success: true}
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}

	var sb strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "... (共 %d 行，已显示 %d-%d)\n", len(lines), offset, end)
	}
	return ToolResult{Output: sb.String(), Success: true}
}

func toInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var n int
		_, err := fmt.Sscanf(x, "%d", &n)
		if err == nil {
			return n
		}
	}
	return def
}
