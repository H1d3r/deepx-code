package agent

import (
	"strings"
	"testing"
)

func TestBuildProgressItems_DetailVsAggregate(t *testing.T) {
	plan := []PlanItem{
		{ID: "wf-0", Title: "[Review] a", Model: "pro", Status: PlanStatusDone},
		{ID: "wf-1", Title: "[Review] b", Model: "flash", Status: PlanStatusRunning},
	}
	phases := []*wfPhaseAgg{{name: "Review", total: 2, running: 1, done: 1}}

	// 详细模式:逐 agent,保留模型。
	detail := buildProgressItems(false, plan, phases)
	if len(detail) != 2 || detail[0].Model != "pro" {
		t.Fatalf("detail mode should return per-agent items with model: %+v", detail)
	}

	// 聚合模式:每 phase 一行,无模型,标题含 done/total。
	agg := buildProgressItems(true, plan, phases)
	if len(agg) != 1 {
		t.Fatalf("aggregate mode should return 1 item per phase, got %d", len(agg))
	}
	if !strings.Contains(agg[0].Title, "Review") || !strings.Contains(agg[0].Title, "1/2") {
		t.Fatalf("aggregate title bad: %q", agg[0].Title)
	}
	if agg[0].Status != PlanStatusRunning {
		t.Fatalf("phase with a running agent should be Running, got %v", agg[0].Status)
	}
}

func TestWfPhaseStatus(t *testing.T) {
	cases := []struct {
		a    wfPhaseAgg
		want PlanStatus
	}{
		{wfPhaseAgg{total: 3, running: 1, done: 2}, PlanStatusRunning},
		{wfPhaseAgg{total: 3, done: 3}, PlanStatusDone},
		{wfPhaseAgg{total: 3, done: 2, failed: 1}, PlanStatusFailed},
		{wfPhaseAgg{total: 3}, PlanStatusPending},
	}
	for _, c := range cases {
		if got := wfPhaseStatus(&c.a); got != c.want {
			t.Errorf("status(%+v) = %v, want %v", c.a, got, c.want)
		}
	}
}

func TestRenderPhaseSnapshot(t *testing.T) {
	phases := []*wfPhaseAgg{
		{name: "Review", total: 100, done: 98, failed: 2},
		{name: "Synthesize", total: 1, done: 1},
	}
	out := renderPhaseSnapshot(phases)
	if !strings.Contains(out, "Review") || !strings.Contains(out, "100/100") || !strings.Contains(out, "✗2") {
		t.Fatalf("phase snapshot missing parts: %q", out)
	}
	if !strings.Contains(out, "✓ Synthesize  1/1") {
		t.Fatalf("done phase line bad: %q", out)
	}
	// 每个 phase 一行,大规模也只有少数行。
	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 1 {
		t.Fatalf("expected 2 lines (2 phases), got %d newlines", n)
	}
}
