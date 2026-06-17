package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func wfTool(args string) ToolCall {
	return ToolCall{Function: ToolCallFunc{Name: "Workflow", Arguments: args}}
}

func TestHandleWorkflowTool_CreateThenList(t *testing.T) {
	ws := t.TempDir()
	ch := make(chan tea.Msg, 16)

	script := `export const meta = { name: \"demo-flow\", description: \"a demo\" };\nexport default async function main(){ return \"ok\"; }`
	create := handleWorkflowTool(context.Background(),
		wfTool(`{"action":"create","saveAs":"demo-flow","script":"`+script+`"}`),
		ModelConfig{}, AgentMode_Auto, ws, "", ch)
	if !create.Success {
		t.Fatalf("create failed: %s", create.Output)
	}
	// 文件应落在 ws/.deepx/workflows/demo-flow.mjs(对齐 Claude Code 的 .mjs)
	p := filepath.Join(ws, ".deepx", "workflows", "demo-flow.mjs")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected file at %s: %v", p, err)
	}

	list := handleWorkflowTool(context.Background(),
		wfTool(`{"action":"list"}`),
		ModelConfig{}, AgentMode_Auto, ws, "", ch)
	if !list.Success || !strings.Contains(list.Output, "demo-flow") {
		t.Fatalf("list should contain demo-flow, got: %q", list.Output)
	}
}

func TestHandleWorkflowTool_CreateValidation(t *testing.T) {
	ws := t.TempDir()
	ch := make(chan tea.Msg, 4)
	// 缺 script
	r := handleWorkflowTool(context.Background(),
		wfTool(`{"action":"create","saveAs":"x"}`),
		ModelConfig{}, AgentMode_Auto, ws, "", ch)
	if r.Success {
		t.Fatal("expected failure when script missing")
	}
	// 非法名
	r = handleWorkflowTool(context.Background(),
		wfTool(`{"action":"create","saveAs":"../evil","script":"x"}`),
		ModelConfig{}, AgentMode_Auto, ws, "", ch)
	if r.Success {
		t.Fatal("expected failure for unsafe name")
	}
}

func TestHandleWorkflowTool_UnknownAction(t *testing.T) {
	r := handleWorkflowTool(context.Background(), wfTool(`{"action":"frobnicate"}`),
		ModelConfig{}, AgentMode_Auto, t.TempDir(), "", make(chan tea.Msg, 1))
	if r.Success {
		t.Fatal("expected failure for unknown action")
	}
}

func TestIsWorkflowRun(t *testing.T) {
	if !isWorkflowRun(wfTool(`{"action":"run","name":"x"}`)) {
		t.Fatal("run should be detected")
	}
	if isWorkflowRun(wfTool(`{"action":"create"}`)) {
		t.Fatal("create should not be a run")
	}
	if isWorkflowRun(ToolCall{Function: ToolCallFunc{Name: "Bash"}}) {
		t.Fatal("non-Workflow tool should not be a run")
	}
}

func TestParseWorkflowToolArgs(t *testing.T) {
	if v := parseWorkflowToolArgs(""); v != nil {
		t.Fatalf("empty → nil, got %v", v)
	}
	if v := parseWorkflowToolArgs(`{"version":"1.2"}`); v == nil {
		t.Fatal("json object should parse")
	}
	m, ok := parseWorkflowToolArgs("version=1.2 env=prod").(map[string]string)
	if !ok || m["version"] != "1.2" || m["env"] != "prod" {
		t.Fatalf("k=v parse failed: %#v", parseWorkflowToolArgs("version=1.2 env=prod"))
	}
	if v := parseWorkflowToolArgs("just a topic"); v != "just a topic" {
		t.Fatalf("plain string fallback failed: %v", v)
	}
}
