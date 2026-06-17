package workflow

import (
	"context"
	"testing"
)

func TestRun_NestedWorkflow(t *testing.T) {
	src := `
export const meta = { name: "parent" };
export default async function main() {
  const r = await workflow("child", { x: 1 });
  return "parent got: " + r;
}`
	var calls []string
	got, err := Run(context.Background(), &Script{Name: "parent", Source: src}, RunOptions{
		Executor: &mockExec{},
		RunChild: func(ctx context.Context, name string, args any) (string, error) {
			calls = append(calls, name)
			return "child-result", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "parent got: child-result" {
		t.Fatalf("got %q", got)
	}
	if len(calls) != 1 || calls[0] != "child" {
		t.Fatalf("RunChild calls = %v", calls)
	}
}

func TestRun_NestedWorkflow_RejectedWhenUnsupported(t *testing.T) {
	src := `
export const meta = { name: "p" };
export default async function main() {
  try { await workflow("c"); return "no-error"; } catch (e) { return "rejected"; }
}`
	// RunChild 为 nil → 嵌套应被拒绝
	got, err := Run(context.Background(), &Script{Name: "p", Source: src}, RunOptions{Executor: &mockExec{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "rejected" {
		t.Fatalf("got %q, want rejected", got)
	}
}
