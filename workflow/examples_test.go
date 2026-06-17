package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExample_ReviewChanges 跑随包样例,确保它语法正确、流程能走通(用 mock executor)。
func TestExample_ReviewChanges(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("examples", "review-changes.js"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var phases []string
	ex := &mockExec{reply: func(c AgentCall) (string, error) { return "[" + c.Label + "] ok", nil }}
	got, err := Run(context.Background(), &Script{Name: "review-changes", Source: string(data)}, RunOptions{
		Executor: ex,
		OnPhase:  func(p string) { phases = append(phases, p) },
	})
	if err != nil {
		t.Fatalf("Run example: %v", err)
	}
	// 4 次 agent:correctness / security / maintainability / synthesizer
	if len(ex.calls) != 4 {
		t.Fatalf("expected 4 agent calls, got %d", len(ex.calls))
	}
	// 阶段顺序
	if strings.Join(phases, ",") != "Review,Synthesize" {
		t.Fatalf("phases = %v", phases)
	}
	// synthesizer 的输入应包含三份子审查结果
	last := ex.calls[3]
	for _, want := range []string{"correctness", "security", "maintainability"} {
		if !strings.Contains(last.Prompt, want) {
			t.Fatalf("synthesizer prompt missing %q: %s", want, last.Prompt)
		}
	}
	if !strings.Contains(got, "synthesizer") {
		t.Fatalf("result should be synthesizer output, got %q", got)
	}
}
