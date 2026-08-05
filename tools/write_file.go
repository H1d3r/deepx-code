package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// writeLimit 是单次 Write 的 content 字节上限,由 tui 启动 / 换模型时按上下文窗口算好注入
// (agent.WriteContentLimitBytes → SetWriteContentLimit)。0 = 未注入,回落 defaultWriteLimit。
// 原子存取:TUI 改,工具执行读。
var writeLimit atomic.Int64

// defaultWriteLimit 是未注入时的兜底(窗口未知,取保守值)。
const defaultWriteLimit = 16 * 1024

// SetWriteContentLimit 注入单次 Write 的 content 字节上限。启动时调,同 SetCodeGraphRoot;
// 换模型(窗口变了)时也要重调。n <= 0 视为未知,回落默认值。
func SetWriteContentLimit(n int) { writeLimit.Store(int64(n)) }

// WriteContentLimit 返回当前生效的上限。
func WriteContentLimit() int {
	if n := writeLimit.Load(); n > 0 {
		return int(n)
	}
	return defaultWriteLimit
}

// 为什么上限设在工具层、而不是"入历史时改写参数":被系统改写过的参数形态会出现在历史里,
// 模型会把它当成 Write 的标准写法模仿 —— 缺 content 的伪调用、把折叠标记当 content 写回、
// 反复 Read 验证刚写的文件,根子都在这里。而拒绝是一条普通的 tool error,模型见过、
// 会正确处理,不构成可模仿的伪形态。

// writeTooLargeMsg 生成超限拒绝的提示。
// 必须可执行 —— 只说"太大了"模型会原样重试,白烧一遍输出 token;
// 且绝不回显 content —— 错误信息会进模型上下文,回显等于把超大内容再喂回去一遍。
// 也不引导用 Bash heredoc:那只是把同样体量挪进 Bash 的参数里,历史照样吃下。
func writeTooLargeMsg(n, limit int) string {
	return fmt.Sprintf(
		"Write 拒绝: content %d 字节,超过单次上限 %d 字节(该上限随模型上下文窗口自适应)。\n"+
			"请分批写入:先用 Write 写入开头一部分,再用 Update 把后续内容逐段追加"+
			"(old_string 取当前文件末尾的若干行,new_string 为这几行加上新增内容)。\n"+
			"不要重试同样大小的写入。",
		n, limit)
}

// WriteFile 写入（覆盖）文本文件。
// 参数:
//
//	path    (string) 文件路径
//	content (string) 写入的内容
func WriteFile(args map[string]any) ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return ToolResult{Output: "错误: path 参数为空", Success: false}
	}
	content, _ := args["content"].(string)
	// 超限提前拒绝:放在任何文件系统操作之前,不为一个注定失败的写入去建父目录。
	if limit := WriteContentLimit(); len(content) > limit {
		return ToolResult{Output: writeTooLargeMsg(len(content), limit), Success: false}
	}

	absPath, err := confineToWorkspace(path)
	if err != nil {
		return ToolResult{Output: err.Error(), Success: false}
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return ToolResult{Output: fmt.Sprintf("创建父目录失败: %v", err), Success: false}
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return ToolResult{Output: fmt.Sprintf("写入失败: %v", err), Success: false}
	}
	CodeGraphInvalidate() // 文件变了,代码图谱缓存失效,下次查询重建
	return ToolResult{
		Output:  fmt.Sprintf("已写入 %s (%d bytes)", absPath, len(content)),
		Success: true,
	}
}
