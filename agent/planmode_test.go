package agent

import (
	"strings"
	"testing"
)

// issue #108:plan 模式应在执行层硬拦 Write/Update/Bash,而不仅靠 system prompt 让 LLM 自觉。
func TestExecuteTool_PlanModeBlocksWrites(t *testing.T) {
	// 这些工具在 plan 模式下必须被拦下(返回失败、不真正执行)。
	for _, name := range []string{"Write", "Update", "Bash"} {
		tc := ToolCall{Function: ToolCallFunc{Name: name, Arguments: "{}"}}
		res := executeTool(tc, AgentMode_Plan, nil)
		if res.Success {
			t.Errorf("plan 模式下 %s 应被拦截 (Success=false),却成功了", name)
		}
		if !strings.Contains(res.Output, "plan 模式") {
			t.Errorf("plan 模式下 %s 的拦截提示应说明原因,得到:%q", name, res.Output)
		}
	}
}

func TestExecuteTool_PlanModeAllowsReads(t *testing.T) {
	// Read 是只读工具,plan 模式下不应被 plan 拦截逻辑挡住。
	// (用一个不存在的路径,Executor 会因找不到文件失败,但失败原因不能是"plan 模式"。)
	tc := ToolCall{Function: ToolCallFunc{Name: "Read", Arguments: `{"path":"/nonexistent/__deepx_plan_test__"}`}}
	res := executeTool(tc, AgentMode_Plan, nil)
	if strings.Contains(res.Output, "plan 模式") {
		t.Errorf("Read 在 plan 模式下不应被 plan 拦截,得到:%q", res.Output)
	}
}

func TestBlockedInPlan(t *testing.T) {
	blocked := map[string]bool{"Write": true, "Update": true, "Bash": true}
	for _, name := range []string{"Write", "Update", "Bash", "Read", "Grep", "List", "OCR", "Glob"} {
		if got := blockedInPlan(name); got != blocked[name] {
			t.Errorf("blockedInPlan(%q) = %v, 期望 %v", name, got, blocked[name])
		}
	}
}
