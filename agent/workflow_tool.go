package agent

import (
	"context"
	"deepx/tools"
	"deepx/workflow"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// workflowToolArgs 是 Workflow 工具的参数(对齐 tools.go 里声明的 schema)。
type workflowToolArgs struct {
	Action string `json:"action"` // create | run | list
	Name   string `json:"name"`   // run:要跑的 workflow 名
	SaveAs string `json:"saveAs"` // create:保存名(kebab-case)
	Script string `json:"script"` // create:脚本源码
	Args   string `json:"args"`   // run:可选参数(JSON 或 k=v)
}

// isWorkflowRun 判断一个工具调用是不是 Workflow(action=run)——用于强制「跑前确认」。
func isWorkflowRun(tc ToolCall) bool {
	if tc.Function.Name != "Workflow" {
		return false
	}
	var in workflowToolArgs
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &in)
	return strings.EqualFold(strings.TrimSpace(in.Action), "run")
}

// workflowLoaderFor 按 workspace + home 构造与 TUI 一致的发现器。
func workflowLoaderFor(workspace string) *workflow.Loader {
	home, _ := os.UserHomeDir()
	proj, glob := workflow.DefaultDirs(workspace, home)
	return workflow.New(proj, glob)
}

// handleWorkflowTool 处理 Workflow 工具调用(在 llm.go 工具循环里被拦截)。
// list/create 是纯文件操作;run 在本回合内联执行,进度经 ch 流式输出,结果回给模型续写总结。
func handleWorkflowTool(
	ctx context.Context,
	tc ToolCall,
	models ModelConfig,
	mode AgentMode,
	workspace, skillCatalog string,
	ch chan<- tea.Msg,
) tools.ToolResult {
	var in workflowToolArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &in); err != nil {
		return tools.ToolResult{Output: "Workflow: 参数解析失败: " + err.Error(), Success: false}
	}
	loader := workflowLoaderFor(workspace)

	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "list":
		metas := loader.List()
		if len(metas) == 0 {
			return tools.ToolResult{Output: "当前没有 workflow。可用 action=create 创建一个。", Success: true}
		}
		var sb strings.Builder
		for _, m := range metas {
			fmt.Fprintf(&sb, "- %s (%s):%s\n", m.Name, m.Scope, m.Description)
		}
		return tools.ToolResult{Output: strings.TrimRight(sb.String(), "\n"), Success: true}

	case "create":
		if strings.TrimSpace(in.SaveAs) == "" || strings.TrimSpace(in.Script) == "" {
			return tools.ToolResult{Output: "Workflow create 需要 saveAs(kebab-case 名字)和 script(完整脚本源码)", Success: false}
		}
		dir := filepath.Join(workspace, ".deepx", "workflows")
		// overwrite=false:同名已存在则报错,不静默覆盖旧脚本(要换内容请先删或换名)。
		p, err := workflow.Save(dir, in.SaveAs, in.Script, false)
		if err != nil {
			return tools.ToolResult{Output: "保存失败: " + err.Error(), Success: false}
		}
		return tools.ToolResult{
			Output: fmt.Sprintf("已保存 workflow %q → %s\n可以用 `/workflow %s` 运行,或调用 Workflow(action:\"run\", name:\"%s\")(运行前会请用户确认)。",
				in.SaveAs, p, in.SaveAs, in.SaveAs),
			Success: true,
		}

	case "run":
		if strings.TrimSpace(in.Name) == "" {
			return tools.ToolResult{Output: "Workflow run 需要 name(要运行的 workflow 名)", Success: false}
		}
		script, err := loader.Load(in.Name)
		if err != nil {
			return tools.ToolResult{Output: "加载失败: " + err.Error(), Success: false}
		}
		emitText := func(s string) { ch <- TokenMsg(s) } // channel 发送本身并发安全
		emitMsg := func(m tea.Msg) { ch <- m }
		args := parseWorkflowToolArgs(in.Args)
		result, rerr := runWorkflowWithProgress(ctx, models, mode, workspace, skillCatalog, script, args, emitText, emitMsg)
		if rerr != nil {
			return tools.ToolResult{Output: "运行失败: " + rerr.Error() + "(已保存进度,下次运行可 resume)", Success: false}
		}
		emitText(fmt.Sprintf("\n\n**✓ workflow %s 完成**\n", script.Name))
		return tools.ToolResult{
			Output:  "workflow 运行完成,最终结果:\n" + result + "\n\n请基于以上结果给用户写一段简洁总结。",
			Success: true,
		}

	default:
		return tools.ToolResult{Output: "Workflow action 必须是 create / run / list 之一", Success: false}
	}
}

// parseWorkflowToolArgs 解析 run 的 args:空→nil;合法 JSON→解析值;含 = →map;否则→原字符串。
func parseWorkflowToolArgs(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	if strings.Contains(s, "=") {
		out := map[string]string{}
		for _, t := range strings.Fields(s) {
			if i := strings.IndexByte(t, '='); i > 0 {
				out[t[:i]] = t[i+1:]
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return s
}
