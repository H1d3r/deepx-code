package tui

import (
	"strings"
	"testing"

	"deepx/agent"
)

// askRecord 把作答折叠成对话流档案段(issue #134)。覆盖单选/多选/未选/跳过四种形态。
func TestAskRecord(t *testing.T) {
	m := model{
		askQuestions: []agent.AskQuestion{
			{Question: "用哪种认证方式?", Options: []agent.AskOption{{Label: "OAuth 2.0"}, {Label: "API Key"}}},
			{Question: "启用哪些特性?", Multiple: true, Options: []agent.AskOption{{Label: "缓存"}, {Label: "压缩"}, {Label: "重试"}}},
		},
		askSelected: [][]bool{
			{true, false},       // 单选:OAuth 2.0
			{true, false, true}, // 多选:缓存 + 重试
		},
	}

	got := m.askRecord(false)
	if !strings.Contains(got, "❓ 用哪种认证方式? → **OAuth 2.0**") {
		t.Errorf("single-select line missing, got:\n%s", got)
	}
	if !strings.Contains(got, "❓ 启用哪些特性? → **缓存、重试**") {
		t.Errorf("multi-select line missing, got:\n%s", got)
	}

	// 跳过:每题都标注「已跳过」,不暴露任何选项。
	skip := m.askRecord(true)
	if strings.Count(skip, "（已跳过）") != 2 {
		t.Errorf("skipped record should mark every question, got:\n%s", skip)
	}
	if strings.Contains(skip, "OAuth") {
		t.Errorf("skipped record must not leak selections, got:\n%s", skip)
	}

	// 一题都没选时落到「（未选）」兜底,不会 panic / 空答案。
	none := model{
		askQuestions: []agent.AskQuestion{{Question: "随便选", Options: []agent.AskOption{{Label: "A"}}}},
		askSelected:  [][]bool{{false}},
	}
	if r := none.askRecord(false); !strings.Contains(r, "（未选）") {
		t.Errorf("no-selection fallback missing, got:\n%s", r)
	}
}
