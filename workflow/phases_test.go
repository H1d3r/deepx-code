package workflow

import (
	"reflect"
	"testing"
)

func TestParsePhaseTitles(t *testing.T) {
	src := `export const meta = {
  name: "review-changes",
  description: "多视角审查",
  phases: [
    { title: "Collect", detail: "收集变更信息" },
    { title: "Review",  detail: "正确性 / 安全 / 简洁性 并行审查" },
    { title: "Synthesize", detail: "去重合并，输出最终报告" },
  ],
};
export default async function main(){}`
	got := parsePhaseTitles(src)
	want := []string{"Collect", "Review", "Synthesize"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParsePhaseTitles_None(t *testing.T) {
	src := `export const meta = { name: "x", description: "d" }; export default async function main(){}`
	if got := parsePhaseTitles(src); got != nil {
		t.Fatalf("无 phases 应返回 nil,得到 %v", got)
	}
}

func TestParseExpectedSteps(t *testing.T) {
	src := `export const meta = { name:"r", phases:[{title:"Collect"},{title:"Review"},{title:"Report"}] };
export default async function main(){
  phase("Collect");
  const c = await agent("收集", { label: "collector", model: "flash" });
  phase("Review");
  const [a,b] = await parallel([
    () => agent("正确性", { label: "correctness", model: "flash" }),
    () => agent("安全",   { label: "security", model: "flash" }),
  ]);
  phase("Report");
  return await agent("汇总", { label: "synthesizer", model: "pro" });
}`
	got := parseExpectedSteps(src)
	want := []ExpectedStep{
		{Phase: "Collect", Label: "collector", Model: "flash"},
		{Phase: "Review", Label: "correctness", Model: "flash"},
		{Phase: "Review", Label: "security", Model: "flash"},
		{Phase: "Report", Label: "synthesizer", Model: "pro"},
	}
	if len(got) != len(want) {
		t.Fatalf("步骤数 got %d want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("步骤[%d] got %+v want %+v", i, got[i], want[i])
		}
	}
}
