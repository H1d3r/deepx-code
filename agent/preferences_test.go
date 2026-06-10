package agent

import (
	"os"
	"strings"
	"testing"
)

func TestSaveAndLoadProjectMemory(t *testing.T) {
	ws := t.TempDir()
	p, err := saveMemory("project", "用 pnpm 不用 npm", ws)
	if err != nil {
		t.Fatalf("saveMemory: %v", err)
	}
	// 去重:再写一次同内容,不应重复
	_, _ = saveMemory("project", "用 pnpm 不用 npm", ws)
	_, _ = saveMemory("project", "测试放 __tests__", ws)

	data, _ := os.ReadFile(p)
	got := string(data)
	if strings.Count(got, "用 pnpm 不用 npm") != 1 {
		t.Errorf("应去重,只保留一条 pnpm,实际:\n%s", got)
	}
	if !strings.Contains(got, "测试放 __tests__") {
		t.Errorf("第二条偏好丢失:\n%s", got)
	}

	// loadPreferences 应注入「本项目约定」段 + 内容
	prefs := loadPreferences(ws)
	if !strings.Contains(prefs, "本项目约定") || !strings.Contains(prefs, "用 pnpm") {
		t.Errorf("loadPreferences 未正确注入项目偏好:\n%s", prefs)
	}
}

func TestParseRememberArgs(t *testing.T) {
	s, c, err := parseRememberArgs(`{"scope":"global","content":"中文回复"}`)
	if err != nil || s != "global" || c != "中文回复" {
		t.Fatalf("解析失败: scope=%q content=%q err=%v", s, c, err)
	}
	// 非法 scope → 兜底 project
	if s, _, _ := parseRememberArgs(`{"scope":"xx","content":"a"}`); s != "project" {
		t.Errorf("非法 scope 应兜底 project,got %q", s)
	}
	// 空 content → 报错
	if _, _, e := parseRememberArgs(`{"scope":"global","content":"  "}`); e == nil {
		t.Errorf("空 content 应报错")
	}
}
